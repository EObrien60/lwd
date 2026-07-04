# lwd — lightweight deploy

A suckless, self-hosted deployment engine for Docker apps. Point it at an app,
deploy with one command, get automatic HTTPS and zero-downtime rollouts for
free. Single static Go binary that is both the daemon and the CLI, plus an
optional second binary, [`lwd-web`](#web-ui-lwd-web), for a browser
dashboard.

> This is the **router + blue-green + secrets + compose apps + web UI + git
> deploy + backing services** milestone (Phases 1–6). Pinned surfaces (outside
> compose) arrive in a later milestone.

## Build

```bash
CGO_ENABLED=0 go build -o lwd ./cmd/lwd
```

## Run the daemon

```bash
sudo LWD_DATA_DIR=/var/lib/lwd ./lwd daemon
```

The daemon listens on a unix socket at `$LWD_DATA_DIR/lwd.sock` (default
`/var/lib/lwd/lwd.sock`) and talks to the local Docker daemon. On startup (and
before the first deploy) it brings up a private `lwd` Docker network and a
managed `lwd-caddy` container that fronts every app on ports 80/443 — this is
the only container that ever publishes host ports.

## Define an app

Create `lwd.toml` in a directory:

```toml
name = "blog"
image = "ghcr.io/me/blog:latest"
domain = "blog.example.com"
port = 8080

[health]
path = "/healthz"
timeout = "30s"
```

`domain` is live: it's how Caddy routes requests to this app. Use a public
FQDN for automatic ACME certificates, or a `.localhost`/bare hostname (e.g.
`blog.localhost`, `blog`) for local development — those get an internally
self-signed cert instead of hitting a public CA.

## Deploy, inspect, and roll back

```bash
lwd apply ./myapp     # deploy ./myapp/lwd.toml (blue-green for image and git-built apps, delegated to `docker compose` for compose apps)
lwd ls                # list apps and status
lwd logs blog -f      # stream logs
lwd history blog      # show past deployments for an app
lwd rollback blog     # redeploy the previous version
lwd rm blog           # stop and deregister
```

## How deploys work

This section describes single-service (`image`) apps; a git-built app (see
[Deploy from Git](#deploy-from-git)) uses this exact same blue-green path once
its image has been built — the only difference is *what* image gets deployed.
See [Compose apps](#compose-apps) for the multi-container `compose` model,
which does not use blue-green.

Every `apply` of a single-service or git-built app is a blue-green swap, never
an in-place recreate:

1. A new "surface" container is started alongside whatever is currently
   running, attached only to the private `lwd` network — it never publishes a
   host port.
2. It's staged behind a throwaway internal hostname on the shared Caddy router
   and health-checked **through Caddy** (never by talking to the container
   directly), using a layered policy:
   - if `health.path` is set, poll it for a 2xx through Caddy;
   - otherwise, if the image declares a Docker `HEALTHCHECK`, honor it;
   - otherwise, fall back to a liveness check (container still running + Caddy
     can reach it at all).
3. Only once the new container passes health does the real domain flip to it.
   The previous container keeps serving every request until that instant, so
   there is no downtime window.
4. The old surface is then retired and removed.

A failed candidate never touches the live route: if health checks don't pass,
the new container and its staging route are torn down and the failure is
recorded, while whatever was already running keeps serving traffic untouched.

Every deployment's resolved spec is snapshotted, so `lwd rollback` restores
the exact previous image/config from that snapshot — not a re-resolution of
whatever `lwd.toml` currently says — via the same zero-downtime blue-green
path used for every other deploy. (For a compose app, `lwd rollback` instead
restores the snapshotted compose file content — see
[Compose apps](#compose-apps).)

## Compose apps

An app can be a multi-container [Docker Compose](https://docs.docker.com/compose/)
stack instead of a single image — declare `compose` + `service` instead of `image`:

```toml
name    = "webapp"
compose = "docker-compose.yml"   # relative to the app dir (or an absolute path)
service = "web"                  # the compose service Caddy fronts
domain  = "webapp.example.com"
port    = 8080                   # container port of `service`
env     = { LOG_LEVEL = "info" } # passed to `docker compose` as environment
secrets = ["DATABASE_URL"]       # resolved, then passed the same way

[health]
path = "/healthz"
```

This requires the **`docker compose` CLI plugin** on the host running the daemon
(Docker Desktop and modern Docker Engine both ship it — single-service `image` apps
don't need it). `lwd.toml` validation rejects mixing `compose` with `image` or
`[build]`; `surfaces` is not supported for either shape.

### How compose deploys work (in-place recreate, not blue-green)

lwd delegates orchestration entirely to `docker compose` rather than running its own
blue-green swap:

1. `docker compose -p lwd-<app> -f <compose file> up -d --remove-orphans`. Compose only
   recreates services whose image or config changed since the last `up` — **an
   unchanged backing service (a database, a cache) is left running untouched.** This is
   the core guarantee of the compose model: redeploying to ship a new web-service image
   does not restart the database.
2. lwd resolves `service`'s running container, joins it to the shared `lwd` network, and
   points `domain` at it through Caddy.
3. It's health-checked through the route using the same layered policy as single-service
   apps (`health.path` if set, otherwise a liveness fallback) — but through the **live**
   route, not a staging one.

**Honest tradeoff:** this is **not zero-downtime**. Because compose — not lwd's surface
machinery — owns the container lifecycle, there is no old container left to keep
serving while the new one is health-checked; the web service takes a brief in-place
restart on every redeploy, and the route flips to it before health-gating runs. If the
health check then fails, lwd does not tear anything down: the (possibly broken) new
stack is left live and the deployment is recorded as failed. Run `lwd rollback <app>`
to restore the previous compose content and recover.

### Env, secrets, and rollback for compose apps

`env` and declared `secrets` are resolved exactly like single-service apps — fail-closed
on any unset secret, aborting before `docker compose` ever runs — and passed as
**environment variables to the `docker compose` process**, so the compose file's
`${VAR}` interpolation and any service's own `environment:` entries can reference them.
Secret values never touch disk as part of a project file.

Every deploy snapshots the compose file's content at that moment, so `lwd rollback` (see
below) restores the exact prior stack — re-applying the stored content with secrets
re-resolved against their current values — rather than whatever the compose file on disk
currently says. `lwd rm <app>` runs `docker compose down` against the stored content,
removing the project's containers and its own default network. `lwd logs` and `lwd ls`
work the same as for single-service apps, against `service`'s container.

## Deploy from Git

Instead of a pre-built `image`, an app can be built straight from a git repo —
declare `[git]` + `[build]` instead of `image`:

```toml
name   = "myapp"
domain = "myapp.example.com"
port   = 8080

[git]
url = "https://github.com/me/myapp"
ref = "main"   # branch, tag, or commit sha; a branch tracks its latest commit on every deploy
# path = "."   # subdir within the repo to use as the build context root

[build]
dockerfile = "Dockerfile"   # relative to git path
```

This composes the box's own tools exactly like lwd already composes `docker`,
`docker compose`, and `caddy` — there is **no GitHub/GitLab API, OAuth, or
webhook receiver** involved. `lwd apply` shells out to the host's `git clone`
and then `docker build`; private repos work if the box's own git is already
authenticated (an SSH key or a credential helper) — lwd itself manages no git
credentials and never sees one.

### Build → blue-green flow

1. lwd clones `[git].ref` into a throwaway temp directory (removed after the
   build) with the box's `git`, resolving it to a commit sha.
2. It runs `docker build` against the checked-out tree (`[build].dockerfile`,
   relative to `[git].path`), tagging the result `lwd-build/<app>:<shortsha>`.
   If that exact tag already exists locally (redeploying the same commit),
   the build is skipped — idempotent redeploys don't rebuild.
3. The built image is then deployed exactly like an `image` app: a fresh
   zero-downtime blue-green surface (see [How deploys work](#how-deploys-work)) —
   staged, health-checked through Caddy, then cut over. No registry is
   involved; the image only ever needs to exist on the local Docker daemon.
4. The deployment record stores the resolved commit sha and the built image
   tag, alongside the usual spec snapshot.

### Rollback (tag-by-sha)

Every build is tagged and kept by commit sha, so `lwd rollback <app>`
redeploys the **previous deployment's already-built image tag** directly —
no re-clone, no rebuild, just another blue-green swap onto
`lwd-build/<app>:<oldsha>`, which is still sitting on the local Docker daemon.
This costs some local disk for old image layers over time (nothing prunes
old `lwd-build/*` tags automatically yet).

### What git deploy supports

Git deploy only supports the git-repo-has-a-Dockerfile → single-service
blue-green shape. `lwd.toml` remains the source of truth for the app's name,
domain, port, env, secrets, and backing services — lwd never reads a
`lwd.toml` committed inside the cloned repo itself (a "the repo configures
itself" model is a possible future addition, not built). Deploying a
repo's *own* `docker-compose.yml` in place (as opposed to the Phase 4
user-provided-compose model, which is unaffected) is also not built — see
[Known limitations](#known-limitations-this-milestone). Validation rejects
mixing `[git]` with `image` or `compose`, and requires `[build]` (a
Dockerfile), `domain`, and `port`.

`lwd-web`'s Deploy modal has a matching **From Git** tab (URL/ref/subdir,
Dockerfile, name/domain/port, env, secrets, backing services) that builds the
`lwd.toml` for you and applies it — see [Web UI](#web-ui-lwd-web).

## Backing services

Any single-service app — whether `image`-based or git-built — can declare
**pinned backing services** (a database, a cache, an object store…) that lwd
runs alongside it, even though the app itself is just one container:

```toml
name   = "myapp"
image  = "ghcr.io/me/myapp:latest"   # or [git] + [build] — backing works with either
domain = "myapp.example.com"
port   = 8080
env    = { DATABASE_URL = "postgres://app:app@db:5432/app" }

[[services]]
name   = "db"
image  = "postgres:16"
env    = { POSTGRES_USER = "app", POSTGRES_PASSWORD = "app", POSTGRES_DB = "app" }
volume = "db-data:/var/lib/postgresql/data"

[[services]]
name    = "minio"
image   = "minio/minio"
command = "server /data"
env     = { MINIO_ROOT_USER = "admin" }
secrets = ["MINIO_ROOT_PASSWORD"]   # resolved and injected into the backing service too
volume  = "minio-data:/data"
```

Each `[[services]]` entry accepts `name`, `image`, `command` (optional),
`env`, `secrets` (declared names, resolved and injected the same fail-closed
way as the app's own `secrets`), and `volume` (`name:path` for a named,
persistent volume, or a bind-mount path).

- **Pinned, never blue-greened.** Backing services are rendered into a
  generated `docker-compose` project (`lwd-<app>`) and brought up with
  `docker compose up -d`, on a dedicated per-app network. They are **not**
  torn down or recreated on every `apply`/`rollback` of the app itself —
  compose's own "only recreate what changed" semantics mean an unchanged
  backing service (and its data) survives redeploy after redeploy.
- **Named volumes persist.** A `volume = "name:path"` declares a top-level
  named Docker volume; it is not removed by a normal redeploy, rollback, or
  even `lwd rm` (which runs `compose down` without `-v` — data is never
  auto-destroyed).
- **Reachable by name.** The app's own surface container is connected to both
  the shared `lwd` network (so Caddy can reach it) and the per-app backing
  network (so it can reach `db`, `minio`, etc. by container name, exactly as
  written in `env`/`secrets` above). Backing services publish no host ports;
  they're internal-only.
- **Works with `image` or `[git]` apps, not Phase-4 `compose` apps.**
  `lwd.toml` validation rejects `[[services]]` on a `compose` app — those
  already define their whole stack (including their own database/cache
  services) directly in the compose file; see [Compose apps](#compose-apps).
- **Removal:** `lwd rm <app>` removes the surface container(s) **and** runs
  `compose down` on the backing project — this does remove the backing
  containers, but named volumes are left in place unless pruned separately.

`lwd-web`'s **From Git** and **Builder** tabs both have a "Backing services"
section that builds `[[services]]` entries into the generated `lwd.toml` for
you — see [Web UI](#web-ui-lwd-web).

## Secrets

Apps can declare secret names in `lwd.toml`:

```toml
secrets = ["DATABASE_URL", "API_KEY"]
```

Only the **names** are committed to `lwd.toml`; the values live in the
daemon's own store, encrypted at rest, and are set out-of-band:

```bash
lwd secret set blog DATABASE_URL   # reads the value from stdin
lwd secret ls blog                 # lists names only — never values
lwd secret rm blog DATABASE_URL
```

At deploy time, the reconciler resolves every name in `secrets` and injects
it into the surface container's environment (a secret wins over a same-named
key in `env`). Resolution is **fail-closed**: if any declared secret has no
value set, `apply` aborts before starting anything — the new container is
never created and whatever was already running (if anything) keeps serving
traffic untouched.

Values are encrypted with AES-256-GCM using a key generated on first use and
stored at `<data_dir>/secret.key` with `0600` permissions. Once a value is
set, it is **never read back out of the daemon** — the API and CLI only
expose `set`, `ls` (names only), and `rm`; there is no `get`.

## Authoring lwd.toml (Claude skill)

`skills/lwd-toml/` is a [Claude Code](https://claude.com/claude-code) skill
that writes an `lwd.toml` for you: point it at a project directory and it
inspects for a `Dockerfile`/`docker-compose.yml`, detects the
language/framework and listening port, spots backing-service dependencies
(a postgres/redis/mysql/minio client, or a db service in a compose file),
picks the right app shape (`[git]`+`[build]`, `image =`, or
`compose =`/`service =`), and writes a valid `lwd.toml` — asking only for
what it can't infer (the `domain`, and confirmation of the port/backing
services). It never invents secret values, only names.

To use it, copy or symlink the skill directory into Claude Code's personal
skills folder:

```bash
cp -r skills/lwd-toml ~/.claude/skills/lwd-toml
# or: ln -s "$(pwd)/skills/lwd-toml" ~/.claude/skills/lwd-toml
```

Then ask Claude Code to "make an lwd.toml for this project" or "deploy this
with lwd" from within the target project. The skill's worked examples
(`skills/lwd-toml/references/examples/*.toml`) are round-trip validated
against the real `internal/spec` parser/validator by
`internal/spec/examples_test.go`, so they stay in sync with the schema.

## Web UI (lwd-web)

`lwd-web` is a **separate dashboard binary** — a "self-hosted Vercel" front
end for lwd. It is just another client of the daemon's existing unix-socket
API (the same API the `lwd` CLI uses): it makes **zero changes to the
daemon**, reconciler, router, or store, and can do nothing the daemon API
doesn't already permit.

### Build

```bash
CGO_ENABLED=0 go build -o lwd-web ./cmd/lwd-web
```

### Run

```bash
LWD_WEB_PASSWORD=changeme ./lwd-web
```

- `LWD_WEB_PASSWORD` (required) — the dashboard's admin password; `lwd-web`
  refuses to start without it.
- `LWD_WEB_ADDR` (default `127.0.0.1:8079`) — listen address.
- `LWD_WEB_SECRET` (optional) — cookie-signing key; if unset, a random key is
  generated at startup (sessions reset on restart).
- The daemon's unix socket is located the same way the CLI locates it —
  `LWD_SOCKET`, or `LWD_DATA_DIR` (default `/var/lib/lwd`) + `lwd.sock` — so
  run `lwd-web` on the same host as the daemon, with the same `LWD_DATA_DIR`
  if you've customized it.

### Auth

A single shared admin password (`LWD_WEB_PASSWORD`) gates the whole
dashboard. `POST /login` checks the password with a constant-time compare
and sets an `HttpOnly`, `SameSite=Lax` signed session cookie (`Secure` when
served over TLS); the session expires after 24 hours. There's no
multi-user/role model — this is a single-operator tool, same as the daemon
itself.

### Exposing it safely

`lwd-web` binds `127.0.0.1:8079` by default and speaks plain HTTP with no
built-in TLS, so don't expose it directly to the internet. Instead:

- **SSH tunnel** (simplest): `ssh -L 8079:localhost:8079 you@host`, then browse
  `http://localhost:8079` locally.
- **Front it with lwd's own Caddy**: point a `domain` at `lwd-web`'s address
  the same way you'd front any other app, so you get automatic HTTPS. (Since
  `lwd-web` isn't itself deployed as an lwd app in this milestone, this means
  adding it to the Caddy config manually or deploying it as a plain container
  that proxies to the host; dogfooding `lwd-web` as an lwd-managed app is a
  later enhancement.)

### Features

- **Overview** — every app's name, domain, status, image, and health at a
  glance, with a **Deploy** action to create a new app.
- **Deploy modal — From Git / Builder / Paste** — three ways to author a new
  app's `lwd.toml`, each with a live preview of the generated document:
  - **From Git** builds a [git-deployed](#deploy-from-git) app: URL, ref,
    subdir, Dockerfile, plus name/domain/port/env/secrets and any backing
    services.
  - **Builder** builds an `image`-based app the same way, minus the git
    fields.
  - **Paste** takes a raw `lwd.toml` document directly (also used for
    **Edit & apply** on an existing app's config).
  Both From Git and Builder have an "Add backing service" control that emits
  `[[services]]` entries (name, image, command, env, secrets, volume) into
  the generated document — see [Backing services](#backing-services).
- **Live logs** — a per-app log stream over SSE, with a follow toggle.
- **History + rollback** — past deployments for an app, with a one-click
  **Roll back** to any prior deployment (for a git-built app, this redeploys
  the prior built image tag, same as `lwd rollback`).
- **Secrets** — list secret names (never values), set, and delete.
- **Redeploy** — re-apply an app's current deployment spec snapshot (e.g.
  after fixing something on the daemon host, or just to restart it).
- **Config edit** — view and edit an app's `lwd.toml` and re-apply it.

As with the CLI, compose apps deployed or edited through the UI still need
their compose file present on the daemon host, and a git-built app's repo
must still be reachable (and, for a private repo, the daemon host's git
already authenticated) at deploy time; pasting/generating a full `lwd.toml`
for a single-service, git-built, or backing-service app works fully from the
UI end to end.

## Networking model

- lwd creates and manages one private Docker network, `lwd`. Every app
  container and the `lwd-caddy` container join it.
- App containers publish **no host ports** — Caddy reaches them by container
  name and port on the `lwd` network. This is why `lwd.toml`'s `port` is just
  the app's container port (e.g. `80` for `traefik/whoami`), not a host port
  to reserve.
- Only `lwd-caddy` binds host ports: 80 and 443 for traffic, and 2019
  (loopback-only) for its admin API, which lwd uses to push routing config.

## Scope of this milestone

- Single host.
- `domain` (routing + TLS) is fully live, for single-service, git-built, and
  compose apps.
- `secrets` (declare names in `lwd.toml`, set values via `lwd secret set`)
  is fully live: values are encrypted at rest and injected into the
  container/compose environment at deploy time, fail-closed on any unset
  name — including for backing-service `secrets`.
- `compose` (multi-container apps, delegated to the `docker compose` CLI
  plugin — see [Compose apps](#compose-apps)) is fully live.
- `[git]` + `[build]` (build-from-source: box-native `git clone` + `docker
  build` → zero-downtime blue-green, no registry — see
  [Deploy from Git](#deploy-from-git)) is fully live.
- `[[services]]` (pinned backing services alongside an `image` or `[git]` app
  — see [Backing services](#backing-services)) is fully live.
- `surfaces` in `lwd.toml` is parsed but rejected with a clear error for all
  shapes; the surfaces-outside-compose blue-green model discussed for the web
  tier of a compose app is deliberately not built (YAGNI for now).
- [`lwd-web`](#web-ui-lwd-web) (a separate dashboard binary) is fully live:
  overview, live logs, history/rollback, secrets, redeploy, config edit, and
  a Deploy modal with **From Git**, **Builder**, and **Paste** tabs (both
  From Git and Builder support declaring backing services), all as a thin
  client of the same daemon API the CLI uses. Deploying `lwd-web` itself as
  an lwd-managed app and multi-user auth are not built yet.

### Known limitations (this milestone)

- Compose deploys are **not zero-downtime** (see
  [Compose apps](#compose-apps)): the routed service gets a brief in-place
  restart on every redeploy, and a failed health check leaves the
  (possibly broken) new stack live rather than rolling back automatically —
  run `lwd rollback` to recover. Single-service and git-built apps remain
  zero-downtime blue-green.
- Git deploy is repo-has-a-Dockerfile → single-service only. lwd never reads
  a `lwd.toml` committed inside the cloned repo (config stays entirely in the
  `lwd.toml` alongside the app, not the repo itself), and deploying a repo's
  *own* `docker-compose.yml` in place (git + repo-compose) is deferred — the
  Phase 4 user-provided-compose model is unaffected. Auto-redeploy on push
  (a git hook or poller) and pushing built images to a registry are also not
  built; lwd stays manual-trigger and single-host by design.
- Backing-service (`[[services]]`) and top-level `secrets` **names** must be
  valid environment-variable identifiers (`[A-Za-z_][A-Za-z0-9_]*`) — they're
  injected as container env vars and, for backing services, spliced into an
  unescapable `${NAME}` compose-interpolation reference, so `lwd.toml`
  validation rejects any other form up front.
- Mutable image tags (e.g. `:latest`) are re-pulled on every `apply` when the
  registry is reachable; if the pull fails but the image exists locally, the
  local copy is used. (Git-built images are never pulled — see
  [Deploy from Git](#deploy-from-git).)
- Public ACME certificates require the daemon's host to be reachable on
  80/443 from the internet for the domains being issued; purely local/internal
  domains (`.localhost`, bare hostnames) always use Caddy's self-signed
  internal CA instead and work fully offline.
- Building lwd requires **Go 1.25+** (a transitive dependency of the Docker
  SDK raises the floor above the 1.22 language baseline).
- Every domain is served over both HTTP and HTTPS with no automatic
  HTTP→HTTPS redirect. Public domains still get Let's Encrypt certificates,
  but plaintext HTTP is not upgraded — forced-HTTPS for public domains is a
  later enhancement.
- All app containers share the `lwd` Docker network with the Caddy container,
  whose admin API is reachable on that network. lwd assumes all deployed apps
  are trusted (single-operator use); isolating the router admin API from app
  containers is a later hardening step.
- Secret values are encrypted at rest with AES-256-GCM, which protects the
  SQLite database file and its backups (e.g. a leaked backup or disk image is
  useless without the key). It does **not** protect against an attacker with
  root (or the daemon's own user) on the host: the key file lives alongside
  the database in `<data_dir>/secret.key`, so anyone who can read the data
  directory can decrypt every secret. This is a data-at-rest control, not a
  substitute for host security.

## Testing

```bash
go test ./...                              # unit tests (e2e SKIPs without Docker)
LWD_DOCKER_TEST=1 go test ./test/ -v       # + real end-to-end test against Docker
```

`internal/web`'s `TestIntegrationWebClientDaemon` runs as part of the plain
`go test ./...` (no Docker, no build tag): it starts a real daemon
`api.Server` on a temp unix socket backed by the fake node/router/compose
stack, drives `lwd-web`'s HTTP handler over real HTTP through a real
`internal/client`, and exercises login → `/api/apps` → `/api/apply` →
`/api/apps` again, proving the browser → `lwd-web` → daemon chain end to end.

The end-to-end suite drives the full stack — a real Docker daemon, a real
`lwd-caddy` container, and real deployments (`traefik/whoami`, and for the
compose test also `redis`) — against real Docker:

- `TestEndToEndBlueGreenRollback` runs two blue-green deploys and a rollback,
  asserting zero downtime across the swap.
- `TestEndToEndSecretInjection` wires a real `secrets.Store` into the
  reconciler, sets a secret, deploys an app declaring it, and asserts (via
  `docker inspect`, not the app's HTTP response) that the value reached the
  container's environment — then asserts a deploy declaring an unset secret
  fails closed with no container left running.
- `TestEndToEndComposeApp` deploys a real two-service compose stack (a `web`
  service Caddy fronts, plus a `cache` backing service standing in for a
  database) via a real `compose.CLI`, asserts the web service is reachable
  through Caddy, then redeploys and asserts the `cache` container's ID is
  **unchanged** — proving compose does not recreate an unchanged backing
  service across a redeploy. It additionally `t.Skip`s (with a clear message)
  if the `docker compose` CLI plugin is not available.
- `TestEndToEndGitDeploy` creates a throwaway local git repo (a one-line
  `FROM traefik/whoami` Dockerfile) and deploys it as a git-built app
  declaring a `cache` (`redis`) backing service, via real `source.CLI` (git)
  and `build.CLI` (`docker build`) — proving build-from-source end to end: the
  cloned repo is actually built into a locally-tagged `lwd-build/e2e-git:<sha>`
  image (checked with `docker image inspect`) and deployed through Caddy;
  redeploying the same ref reuses the built tag while still starting a fresh
  blue-green surface, and the pinned `cache` container's ID is **unchanged**
  across both the redeploy and a `lwd rollback`. It `t.Skip`s if `git` or the
  `docker compose` CLI plugin (needed for the backing service) is not
  available.

All four tests clean up every container, network, and (for the git test)
built image they create, and will `t.Skip` (rather than fail) if ports 80/443
are already in use on the host.
