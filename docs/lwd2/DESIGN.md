# LWD 2.0 — design contract

LWD is the boring, deterministic application platform for OBH/QH: given an
application, environment, immutable release and target host, make the declared
application exist, remain healthy and be recoverable.

Not Kubernetes, not a scheduler, not a cloud provider. Ansible manages hosts.
LWD manages workloads and application resources. agentd (later) operates LWD.

v1 lives at tag `v0.1-legacy`. Carried forward: the stage → readiness → cutover
→ retire sequence, Caddyfile generation + admin `/load`, the secret cipher,
snapshot-per-release. Deleted: scheduler, pools, replicas, drain/evacuate/
failover, ssh/agent transports, git builds, web UI, MCP (returns in M4 over /v1).

## Components (one binary)

| Mode | Runs on | Owns |
|---|---|---|
| `lwd controller` | control-01, as an unprivileged user | Postgres state, `/v1` API, releases, secrets, deploy/rollback orchestration |
| `lwd node` | every workload host, root, installed by Ansible | local Docker Compose projects, local Caddy, readiness/smoke, on-disk last-known state |
| `lwd <cmd>` | anywhere | CLI client of the controller |

The controller never runs Docker against another host. It sends a self-contained
bundle (`internal/bundle`) to the node. The node never reads the controller DB.
Apps keep serving when the controller is down; a node reboot restores
everything from local state (restart policies + persisted Caddyfile).

## Node

- API: HTTP, `LWD_NODE_ADDR` (default `0.0.0.0:7480`), bearer `LWD_NODE_TOKEN`
  (constant-time). Firewall restricts it to the controller.
  - `GET  /v1/health` → `{"ok":true,"version":"..."}`
  - `GET  /v1/status` → host facts + every app-env's live deployment + container states
  - `POST /v1/deploy` (body `bundle.Bundle`) → `bundle.Result`; synchronous; 409 if that app-env is busy
  - `POST /v1/apps/{app}/{env}/restart?service=` → restarts live containers
  - `GET  /v1/apps/{app}/{env}/logs?service=&tail=` → text
  - `DELETE /v1/apps/{app}/{env}` → removes live project, routes and state
- Uses the `docker` / `docker compose` CLIs only (no Docker SDK).
- Disk layout (`LWD_NODE_DIR`, default `/srv/lwd`):
  ```
  system/compose.yaml           lwd-system project: caddy, network_mode host
  caddy/etc/Caddyfile           persisted, mounted dir; caddy boots from it
  caddy/data, caddy/config      cert + ACME state survive container recreation
  apps/<app>-<env>/state.json   live deployment, ports, last 20 attempts
  apps/<app>-<env>/d<N>/        bundle.json, compose.yaml, .env (0600), result.json, failure.log
  ```
  Keep the 5 newest `d<N>` dirs.
- Caddy: `caddy:2` with `network_mode: host`, admin `127.0.0.1:2019` (never
  reachable from app containers), automatic HTTP→HTTPS redirect, one site block
  per domain → `127.0.0.1:<hostport>`. `tls internal` when bundle TLS is internal.
- HTTP services publish `127.0.0.1:<hostport>:<port>`; host ports allocated by
  the node from 20000–29999, persisted per deployment.
- Each deployment is its own compose project `lwd-<app>-<env>-d<deployment>`:
  `restart: unless-stopped`, `env_file: .env`, json-file logs (10m × 3), labels
  `lwd.app/env/release/deployment/service`.

### Deploy algorithm (per app-env, serialized)

1. validate bundle; write `d<N>/` (bundle, compose, .env)
2. `pull` — `docker compose pull` (digest-pinned images only)
3. `migrate` — `docker compose run --rm <migrate>` if declared; failure → failed
4. `start` — `docker compose up -d` for the candidate project
5. `ready` — each HTTP service: GET `http://127.0.0.1:<hostport><ready>` until
   2xx/3xx within ReadyTimeoutSeconds; workers must be running. Failure →
   capture logs, `down` candidate, status `failed`; live deployment untouched.
6. `cutover` — rewrite routes to the candidate's ports, write Caddyfile, `/load`
7. `smoke` — for SmokeSeconds, every second GET `https://<domain><ready>` via
   127.0.0.1:443 with SNI (verification skipped: this checks routing, not
   trust). Any failure → routes back to previous, `down` candidate, status
   `reverted`, logs retained.
8. `done` — `down` the previous project, update state, prune old dirs.

Workers of old and new deployments overlap for the smoke window. Migrations must
be backwards compatible (expand/contract); rollback never reverts schema.

## Controller

- `LWD_DATABASE_URL` (Postgres), `LWD_LISTEN` (default `127.0.0.1:7470`),
  `LWD_API_TOKEN` (bearer, single admin token for M1), `LWD_SECRET_KEY_FILE`,
  `LWD_INSECURE_REGISTRIES` (comma list, dev only).
- Migrations: embedded `internal/store/migrations/NNNN_name.sql`, applied in
  order at start, tracked in `schema_migrations`.
- Tables: `hosts`, `apps` (manifest text), `environments`, `releases`
  (commit, manifest snapshot, service→digest map), `deployments` (release, env,
  host, kind deploy|rollback, status running|succeeded|failed|reverted, actor,
  reason, timings, result jsonb), `secrets` (app, env, key, version, encrypted
  value), `events` (append-only).
- Deploy: per app-env Postgres advisory lock → build bundle from release +
  environment + latest secret versions → insert deployment(running) → node
  `/v1/deploy` → record result + events.
- Rollback: default target is the release of the newest `succeeded` deployment
  whose release differs from the currently live one; `--to <release>` overrides.
  Same path as deploy, kind `rollback`. Uses current secrets.
- Releases are immutable: tags are resolved to digests once, at release creation.
  `latest` is refused.

### API `/v1` (bearer)

```
POST   /v1/hosts                         {name, addr, token}
GET    /v1/hosts            GET /v1/hosts/{name}  (includes live node status)
PUT    /v1/apps/{app}                    body: lwd.toml text
GET    /v1/apps             GET /v1/apps/{app}
POST   /v1/apps/{app}/releases           {commit, tag, images?{svc:ref}}
GET    /v1/apps/{app}/releases
POST   /v1/apps/{app}/envs/{env}/deploy  {release?}  (default newest release)
POST   /v1/apps/{app}/envs/{env}/rollback {to?}
POST   /v1/apps/{app}/envs/{env}/restart {service?}
GET    /v1/apps/{app}/envs/{env}/status
GET    /v1/apps/{app}/envs/{env}/deployments
GET    /v1/apps/{app}/envs/{env}/logs?service=&tail=
PUT    /v1/apps/{app}/envs/{env}/secrets/{key}   body: raw value
GET    /v1/apps/{app}/envs/{env}/secrets         names + versions only
DELETE /v1/apps/{app}/envs/{env}/secrets/{key}
GET    /v1/events?app=&env=&limit=
```
Errors: `{"error":{"code":"conflict|not_found|invalid|node_unreachable|internal","message":"..."}}`.

## Manifest (`lwd.toml`)

```toml
name = "hello"

env     = { LOG_LEVEL = "info" }   # plain vars for every service
secrets = ["SESSION_SECRET"]       # must be set per environment before deploy

[services.web]
image  = "ghcr.io/obhsoftware/lwd-hello-web"   # repository only: no tag, no digest
port   = 8080
domain = "@"                                   # "@" = env domain; "x" = x.<env domain>
ready  = "/"

[services.api]
image  = "ghcr.io/obhsoftware/lwd-hello-api"
port   = 3000
domain = "api"
ready  = "/ready"

[services.worker]
image   = "ghcr.io/obhsoftware/lwd-hello-api"
command = ["node", "dist/worker.js"]

[migrate]
service = "api"
command = ["node", "dist/migrate.js"]

[env.staging]
host   = "m1"
domain = "hello.m1.lwd.internal"
tls    = "internal"                 # default "acme"
env    = { LOG_LEVEL = "debug" }    # per-environment overrides
```

Reserved for M2 (rejected until implemented): `database`, `storage`.

## CLI

```
lwd controller | lwd node | lwd version
lwd host add NAME ADDR --token-file F | host list | host status NAME
lwd app apply [DIR] | app list | app status APP [ENV]
lwd release create APP --tag T [--commit C] [--image svc=ref] | release list APP
lwd deploy APP ENV [--release N]
lwd rollback APP ENV [--to N]
lwd restart APP ENV [--service S]
lwd logs APP ENV [--service S] [--tail N]
lwd history APP ENV
lwd secret set APP ENV KEY (value on stdin) | secret list APP ENV | secret rm APP ENV KEY
lwd events [APP]
```
Client config: `LWD_URL` (default `http://127.0.0.1:7470`), `LWD_TOKEN` or
`~/.config/lwd/token`.

## M2 — database and storage resources (qMechanic staging)

Manifest flags: `database = true`, `storage = true`. Applications only see env:

| Flag | Injected |
|---|---|
| `database` | `DATABASE_URL=postgres://<app>_<env>:<pw>@lwd-postgres:5432/<app>_<env>` |
| `storage` | `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET=<app>-<env>`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` |

Resource class is `shared` (one instance per host). `dedicated`/`managed` are
reserved names only.

- **Platform services** are a node-owned compose project `lwd-platform` on the
  docker network `lwd-platform`: `lwd-postgres` (postgres:17, data
  `/srv/lwd/postgres`) and `lwd-minio` (data `/srv/lwd/minio`). Neither
  publishes a host port. They start lazily the first time a host is asked to
  provision a resource. App services that need a resource join
  `lwd-platform` in addition to their project network.
- **Database posture** (identical to qMechanic's `local/db-init.sql`): role
  `<app>_<env>` `LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE`, database
  `<app>_<env>` `OWNER` that role, `public` schema owned by it, `CONNECT`
  revoked from `PUBLIC`. Row-level security with FORCE therefore binds the app.
- **Bucket posture**: one MinIO user per app-env with a policy limited to its
  bucket.
- **Provisioning**: the controller owns names and generated credentials
  (stored as LWD-owned secrets, never shown); the node executes idempotent
  operations via `docker exec` in the platform containers:
  - `PUT  /v1/platform/databases/{name}` `{password}`
  - `PUT  /v1/platform/buckets/{name}` `{access_key, secret_key}`
  - `POST /v1/platform/databases/{name}/backup` → `{file, bytes, sha256}`
    (`pg_dump -Fc` to `/srv/lwd/backups/postgres/<name>/<UTC ts>.dump`)
  - `POST /v1/platform/databases/{name}/restore` `{file}` → stops the
    app-env's live project, recreates the database, `pg_restore`, restarts.
- **Backups**: the controller records every backup in `backups` and runs one
  per database daily; `lwd db backup|restore|list`, `lwd backup status`.
  Offsite copy and PITR come later without changing these commands.
- Known limitation: services on `lwd-platform` can reach each other at the
  network level; the database role and bucket policy are the isolation
  boundary. Acceptable while every workload is OBH's own.
