# lwd node — implementation notes

`DESIGN.md` is the contract. This file records how `internal/node` meets it
and the few places where it refines it.

## Configuration

| Variable | Default | |
|---|---|---|
| `LWD_NODE_TOKEN` | — | required; the node refuses to start without it |
| `LWD_NODE_ADDR` | `0.0.0.0:7480` | |
| `LWD_NODE_DIR` | `/srv/lwd` | must be absolute |
| `LWD_CADDY_IMAGE` | `caddy:2` | |
| `LWD_ACME_EMAIL` | — | optional; Caddyfile global `email` |

## Startup (idempotent)

1. Create `system/`, `caddy/{etc,data,config}/`, `apps/` (0700: it holds secrets).
2. Write `system/compose.yaml` (project `lwd-system`, Caddy, host networking).
3. Rebuild the Caddyfile from every `apps/*/state.json` live deployment and
   write it into `caddy/etc/`.
4. `docker compose -p lwd-system ... up -d`, wait for the admin API on
   `127.0.0.1:2019`, then `/load` the Caddyfile (an already-running Caddy keeps
   its in-memory config across `up -d`).

If Caddy does not come up the node exits non-zero; systemd restarts it.

## Deployment directory

`apps/<app>-<env>/d<N>/`:

| File | Mode | |
|---|---|---|
| `bundle.json` | 0600 | the bundle as received (contains resolved secrets) |
| `compose.yaml` | 0644 | structure only, no vars |
| `environment.json` | 0600 | per-service `environment:` mapping |
| `result.json` | 0600 | the `bundle.Result` returned |
| `failure.log` | 0600 | on failure/revert: `docker compose logs --tail 300` (+ migrate output) |

Every compose invocation for a deployment is
`docker compose -p lwd-<app>-<env>-d<N> --project-directory d<N> -f compose.yaml -f environment.json ...`.

## Refinements to DESIGN.md

- **No `.env`/`env_file`.** Compose's dotenv dialect cannot safely quote a
  value containing both quote kinds plus newlines, and a file called `.env` in
  the project dir is also read for interpolation. Vars go in
  `environment.json` (merged as a second compose file, 0600). Both compose
  files are emitted as JSON (valid YAML, quoted by `encoding/json`), and every
  `$` in a bundle value is written as `$$` so compose interpolation never
  rewrites it.
- **Migrate job** is the compose service `lwd-migrate` under profile
  `migrate`, `restart: "no"`, run with
  `docker compose --profile migrate run --rm -T lwd-migrate`. A bundle may not
  name its own service `lwd-migrate`.
- **Extra validation (400 `invalid`)** beyond `bundle.Validate`: domains must
  be lower-case DNS names (they are written into the Caddyfile unquoted), a
  domain may not be routed by another app-env on the node, ready paths must be
  absolute URL paths, var names may not contain `=` or NUL, and redeploying the
  deployment id that is already live is refused.
- **Phase `prepare`.** Writing `d<N>/` and allocating ports is reported as
  phase `prepare`; a failure there (e.g. no free port) is `failed`/`prepare`.
- **Readiness** uses one deadline of `ReadyTimeoutSeconds` (default 60) for
  all services, polled every 500 ms; workers must show `running` in
  `docker compose ps --all --format json` within the same deadline.
- **Smoke** runs `SmokeSeconds` (default 10) rounds one second apart. A
  routed service is probed at `https://<domain><ready>` for every domain
  (dialing 127.0.0.1:443, SNI = domain, verification off); a service without
  domains on its loopback port.
- **Crash safety at `done`:** `state.json` is updated to the new live
  deployment *before* the previous project is downed.
- **Pruning** runs after every attempt (not only successes) so failing retries
  cannot fill the disk; the 5 newest `d<N>` dirs and the live one are kept.
- **Deploys survive the controller disconnecting**: the request context's
  cancellation is ignored once a deploy starts; the outcome is in
  `result.json` / `state.json`.
- **Delete** removes routes first, then downs the live project, then removes
  the state dir; if `down` fails the state stays and the delete can be retried.
- **Logs** take `tail` 1–10000 (default 100) and do not take the app-env lock;
  restart and delete do (409 while a deploy runs).

## Status

`GET /v1/status` returns hostname, version, process uptime, load averages
(`/proc/loadavg`), memory total/available (`/proc/meminfo`), root filesystem
size/free (`statfs("/")`), the Caddy container state, and per app-env the live
deployment, the last attempt and `docker compose ps` rows (both the JSON-array
and JSON-lines output formats are accepted). `platform` reports whether the
platform project exists and its `docker compose ps` rows.

## Platform services (M2)

- `ensurePlatform` (serialised by one mutex, run by every provisioning call
  and at node start if `platform/compose.yaml` exists): create dirs (0700),
  generate `platform/postgres.password` and `platform/garage.toml` (rpc
  secret, admin token) once, write `platform/compose.yaml` (0600: it carries
  `POSTGRES_PASSWORD`), `docker network create lwd-platform` if `inspect`
  fails, `compose up -d --wait --wait-timeout 180`, give the Garage node a
  role if `layout show` says none has one (and apply any staged layout
  version), then revoke `CONNECT, TEMPORARY` from `PUBLIC` on `postgres` and
  `template1`. A platform failure at node start is logged, not fatal.
- Postgres healthcheck is `pg_isready -h 127.0.0.1` (TCP): initdb's
  temporary server listens on the socket only.
- The docker runner gained `Stream(stdin, stdout)`: SQL goes to `psql -X -q
  -v ON_ERROR_STOP=1` on stdin; dumps stream to a temp file in the backup dir
  (sha256 computed while writing, fsync, rename), never through memory.
- Garage is driven through its CLI (`docker exec lwd-s3 /garage ...`):
  `bucket info`/`create`, `key info --show-secret`, `key list` (to find
  `lwd-<bucket>` when the controller has no id), `key create`, `bucket allow
  --read --write --owner`. "Not found" is recognised from the error text.
- Backup and restore of one database are serialised (lock `db:<name>`);
  restore also takes the app-env lock, so it is refused (409) during a
  deploy. Backup files are named `YYYYMMDDTHHMMSS.mmmZ.dump`.
