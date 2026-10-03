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
  - platform resources (M2): see "M2 — database and storage resources"
- Uses the `docker` / `docker compose` CLIs only (no Docker SDK).
- Disk layout (`LWD_NODE_DIR`, default `/srv/lwd`):
  ```
  system/compose.yaml           lwd-system project: caddy, network_mode host
  caddy/etc/Caddyfile           persisted, mounted dir; caddy boots from it
  caddy/data, caddy/config      cert + ACME state survive container recreation
  apps/<app>-<env>/state.json   live deployment, ports, last 20 attempts
  apps/<app>-<env>/d<N>/        bundle.json, compose.yaml, .env (0600), result.json, failure.log
  platform/                     lwd-platform project (0700): compose.yaml, postgres.password, garage.toml (0600)
  postgres/  garage/{meta,data}  platform service data
  backups/postgres/<db>/        <UTC ts>.dump, newest 14 kept
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
- `bundle.Platform` (set by the controller when the app declares a resource)
  attaches every service and the migrate job to the external docker network
  `lwd-platform` in addition to the project's default network; the node
  creates that network if it is missing. Nothing else in the bundle changes.

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
  `LWD_INSECURE_REGISTRIES` (comma list, dev only), `LWD_BACKUP_HOUR` (local
  hour 0-23 of the daily database backups, default 3).
- Migrations: embedded `internal/store/migrations/NNNN_name.sql`, applied in
  order at start, tracked in `schema_migrations`.
- Tables: `hosts`, `apps` (manifest text), `environments`, `releases`
  (commit, manifest snapshot, service→digest map), `deployments` (release, env,
  host, kind deploy|rollback, status running|succeeded|failed|reverted, actor,
  reason, timings, result jsonb), `secrets` (app, env, key, version, encrypted
  value), `events` (append-only); M2: `resources` (app, env, kind, host, name,
  encrypted credentials; name unique per kind) and `backups`.
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
GET    /v1/apps/{app}/envs/{env}/resources       kind, name, host only (never credentials)
POST   /v1/apps/{app}/envs/{env}/db/backup       manual backup -> backup row
GET    /v1/apps/{app}/envs/{env}/db/backups      recorded backups, newest first
POST   /v1/apps/{app}/envs/{env}/db/restore      {backup_id}  destructive; deploy lock held
GET    /v1/backups                               per database: latest ok + last failure; failures of 7 days
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

Resources (M2): top-level `database = true` and/or `storage = true`. An app
may not define (as a variable or secret) any name these inject.

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
lwd db status APP ENV | db backup APP ENV | db backups APP ENV
lwd db restore APP ENV BACKUP_ID --yes
lwd backup status
lwd events [APP]
```
Client config: `LWD_URL` (default `http://127.0.0.1:7470`), `LWD_TOKEN` or
`~/.config/lwd/token`.

## M2 — database and storage resources (qMechanic staging)

Status: implemented ("M2-lite": shared class, local backups). Offsite copy and
PITR are still to come.

Manifest flags: `database = true`, `storage = true`. Applications only see env:

| Flag | Injected |
|---|---|
| `database` | `DATABASE_URL=postgres://<app>_<env>:<pw>@lwd-postgres:5432/<app>_<env>?sslmode=disable` (`-` → `_`) |
| `storage` | `S3_ENDPOINT=http://lwd-s3:3900`, `S3_REGION=garage`, `S3_BUCKET=<app>-<env>`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` |

The S3 endpoint is internal to `lwd-platform`; apps that serve files proxy them.
A user variable or secret with an injected name fails the deploy (and `app
apply`, for names in the manifest).

Resource class is `shared` (one instance per host). `dedicated`/`managed` are
reserved names only.

- **Platform services** are a node-owned compose project `lwd-platform`
  (`/srv/lwd/platform/compose.yaml`) on the docker network `lwd-platform`,
  which the node creates (compose treats it as external): `lwd-postgres`
  (`postgres:17-alpine`, data `/srv/lwd/postgres`, superuser password
  generated once into `platform/postgres.password`) and `lwd-s3` (Garage
  `dxflrs/garage:v2.4.1`, digest-pinned; single-node layout assigned once;
  data `/srv/lwd/garage`). Neither publishes a host port; both have
  healthchecks and `restart: unless-stopped`. They start lazily the first
  time a host is asked to provision a resource (`up -d --wait`) and are
  re-upped on every node start once they exist. App services of a bundle
  with `platform: true` join `lwd-platform` in addition to their project
  network. `/v1/status` reports `platform.services`.
- **Why Garage, not MinIO**: maintained open-source S3 server with published
  release images, small single binary, per-key bucket permissions through
  its CLI. MinIO's community edition no longer ships maintained images.
- **Database posture** (identical to qMechanic's `local/db-init.sql`): role
  `<app>_<env>` `LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE`, database
  `<app>_<env>` `OWNER` that role, `public` schema owned by it, `CONNECT`
  revoked from `PUBLIC`. Row-level security with FORCE therefore binds the app.
  `CONNECT`/`TEMPORARY` are also revoked from `PUBLIC` on `postgres` and
  `template1`, so an app role can connect to its own database only.
- **Bucket posture**: one Garage access key `lwd-<bucket>` per app-env with
  read/write/owner on its bucket only (new keys cannot create buckets).
- **Provisioning**: the controller owns names and credentials, stored
  encrypted in `resources` and never returned by the API, events or
  deployment results. Every deploy calls the node for each declared resource
  before building the bundle (idempotent, so a rebuilt host heals). The node
  executes via `docker exec` (SQL to `psql` on stdin with `ON_ERROR_STOP`;
  names validated: db `^[a-z][a-z0-9_]{0,62}$`, bucket `^[a-z0-9][a-z0-9-]{2,62}$`):
  - `PUT  /v1/platform/databases/{name}` `{password}` → create-or-update role + database
  - `PUT  /v1/platform/buckets/{name}` `{access_key_id?}` → `{access_key_id, secret_access_key}`;
    the node creates the key (Garage ids are `GK…`) and keeps the one the
    controller passes while it exists
  - `POST /v1/platform/databases/{name}/backup` → `{file, bytes, sha256, created_at}`
    (`pg_dump -Fc` to `/srv/lwd/backups/postgres/<name>/<UTC ts>.dump`, newest 14 kept)
  - `GET  /v1/platform/databases/{name}/backups` → files, newest first
  - `POST /v1/platform/databases/{name}/restore` `{file, app, env}` → `compose stop`
    of the app-env's live project, drop (`WITH (FORCE)`) and recreate with the
    same posture, `pg_restore --no-owner --role=<name> --exit-on-error`,
    `compose start` (also after a failed restore); returns step timings.
- **Backups**: the controller records every attempt in `backups` (manual or
  scheduled, succeeded or failed with the error) with events
  `backup.succeeded|failed`. A controller goroutine backs up every database
  once a day after `LWD_BACKUP_HOUR`; it catches up after downtime and does
  not repeat a day's attempt. Restores require a succeeded backup of the
  same app-env, host and database, hold the deploy lock, and are recorded as
  `db.restored`/`db.restore_failed`. CLI: `lwd db status|backup|backups|restore
  --yes`, `lwd backup status` (exit 1 if a database has no good backup).
  Offsite copy and PITR come later without changing these commands.
- Known limitation: services on `lwd-platform` can reach each other at the
  network level; the database role and bucket policy are the isolation
  boundary. Acceptable while every workload is OBH's own.
