# lwd — lightweight deploy

A suckless, self-hosted deployment engine for Docker apps. Point it at an app,
deploy with one command, get automatic HTTPS and zero-downtime rollouts for
free. Single static Go binary that is both the daemon and the CLI, plus three
optional client/worker binaries: [`lwd-web`](#web-ui-lwd-web) (a browser
dashboard), [`lwd-mcp`](#agent-access-lwd-mcp) (an MCP server for coding
agents), and [`lwd-agent`](#the-lwd-agent-transport) (a dumb per-node worker
that replaces raw docker-over-ssh for a registered node).

> This is the **router + blue-green + secrets + compose apps + web UI + git
> deploy + backing services + multi-node federation + dumb node agent + node
> UX + continuous reconciler/self-heal + capacity-aware scheduler + node
> pools + node drain/evacuate + automatic node-loss failover** milestone
> (Phases 1–11b — **Phase 11 is now complete**). Surface replicas +
> load-balancing (P12) are next (see `docs/VISION.md`).

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

## Multi-node (federation)

By default every app deploys to the same machine `lwd daemon` runs on. lwd
can additionally deploy an app to another registered machine over
**docker-over-ssh**, moving whatever image it needs there with `docker save |
ssh | load` — no image registry required, and lwd manages no ssh credentials
of its own (the daemon's own `ssh` — agent, keys, `~/.ssh/config` — must
already reach the target non-interactively, exactly like [Deploy from
Git](#deploy-from-git)'s use of the box's own git).

**`node = "local"` pins an app to the controller; leaving `node` unset lets
lwd pick where it runs** — see [Scheduling & pools](#scheduling--pools) for
the placement rules. On a single-node install (no other nodes registered)
these two are equivalent in practice: there's only ever one place to run.

### Register a node

```bash
lwd node add web1 deploy@web1.example.com 100.64.0.2
lwd node add web1 deploy@web1.example.com 100.64.0.2 --agent http://100.64.0.2:8078
lwd node ls
lwd node rm web1
```

- `<ssh-host>` is anything `ssh` itself accepts (`user@host`, or a `Host`
  alias from `~/.ssh/config`).
- `<mesh-addr>` is the address the controller reaches that node at for app
  traffic. This is meant to be a **WireGuard mesh** address: the central
  Caddy talks plain HTTP to a remote surface across it, so it should be a
  private, trusted network, not a public IP. Standing up the mesh itself
  (WireGuard keys/peers) is outside lwd's scope, same as ssh — bring your
  own.
- `--agent <url>` is optional: the base URL of a running
  [`lwd-agent`](#the-lwd-agent-transport) on that node (typically
  `http://<mesh-addr>:8078`). If set, lwd prefers talking to that agent over
  docker-over-ssh whenever it's reachable — see
  [The lwd-agent transport](#the-lwd-agent-transport).
- `local` is implicit and never appears in `lwd node ls`; registering a node
  named `local` is not meaningful and not supported.
- `lwd node ls`, `lwd-web`'s **Nodes** view, and `lwd-mcp`'s `lwd_node_list`
  tool all show each node's live **transport** (`agent`, `ssh`, or `local`)
  and **reachability**, probed fresh on every call — see
  [Web UI](#web-ui-lwd-web) and [Agent access](#agent-access-lwd-mcp).

### Place an app on a node

Add `node = "<name>"` to `lwd.toml` to pin it to a specific node:

```toml
name   = "worker"
image  = "ghcr.io/me/worker:latest"
domain = "worker.example.com"
port   = 8080
node   = "web1"
```

Setting `node = "local"` pins it to the controller. **Omitting `node`
entirely** hands placement to the scheduler instead of pinning it anywhere —
see [Scheduling & pools](#scheduling--pools). Every existing single-node
`lwd.toml` (no `node` line, no other nodes registered) is unaffected: with
nothing else to choose from, the scheduler places it on `local`, same as
before. `lwd-web`'s Deploy modal (both **From Git** and **Builder** tabs) has
a **Node** dropdown — `Auto` (unset), `local`, or a registered node — that
adds (or omits) the `node = "..."` line for you, plus optional **Pool**/
**CPU**/**Memory** fields; `lwd-mcp`'s `lwd_apply` and `lwd_deploy_git` tools
take optional `node`, `pool`, and `requirements` arguments that do the
same — see [Web UI](#web-ui-lwd-web) and [Agent access](#agent-access-lwd-mcp).

### How a remote deploy works

1. **The build always stays on the controller.** A git-built app's `docker
   build` (see [Deploy from Git](#deploy-from-git)) never runs on the target
   node; only the resulting image is shipped there.
2. Before starting the container, lwd makes the image available on the
   target: it first tries an ordinary registry pull **on the target
   itself** (covers any image the target can fetch on its own, with no data
   flowing through the controller); if that doesn't produce the image (e.g.
   a locally-built `lwd-build/<app>:<sha>` tag that was never pushed
   anywhere), lwd transfers it directly — `docker save` on the controller,
   piped over the transport (agent or ssh) into `docker load` on the target.
3. The surface container runs on the target node, reached over whichever
   [transport](#the-lwd-agent-transport) is currently selected for it — a
   registered `lwd-agent` if one is reachable, otherwise the Docker SDK's ssh
   connection helper (the same mechanism `docker -H ssh://<ssh-host>` uses).
   Either way the behavior is identical from here: a **local** surface
   publishes no host port at all — Caddy reaches it by container name on the
   shared `lwd` network, unchanged from single-node. A **remote** surface
   instead publishes an ephemeral host port bound to the node's mesh address,
   and the central Caddy's upstream for that route becomes
   `<mesh-addr>:<published-port>`. Blue-green staging, health-checking
   (through Caddy), and cutover all work exactly as described in [How
   deploys work](#how-deploys-work) — only the upstream address differs.
4. A remote app's declared [`[[services]]`](#backing-services) are brought
   up with `docker compose` **targeting the node's own daemon**
   (`DOCKER_HOST=ssh://<ssh-host>` in the process env passed to the compose
   CLI — backing services always go over ssh/`DOCKER_HOST`, independent of
   which transport runs the surface container itself) — pinned alongside the
   surface on that node, not the controller.

### What's remote-capable today

- `image` apps and `[git]`+`[build]` apps — with or without
  `[[services]]` — can be placed on a registered node.
- **A `compose =` app cannot be placed on a remote node yet.**
  `lwd.toml` validation rejects `compose` combined with a non-local `node`
  outright (`compose apps on remote nodes are not supported yet`): unlike
  the image/git backing-services path above, `applyCompose`'s `docker
  compose up` and routing don't yet thread a resolved node's `DOCKER_HOST`
  through, so silently allowing it would run the stack on the controller
  instead of the node the user asked for. Run a compose app on `local`, or
  reshape it as an `image`/`[git]` app with `[[services]]` if it needs to
  run on a remote node.
- Node reachability is now continuously observed (see [Self-healing &
  health](#self-healing--health)), and a dead surface is healed in place on
  its existing node. An unpinned app is placed once, at apply time, by the
  capacity-aware scheduler (see [Scheduling & pools](#scheduling--pools)).
  **Cross-node reschedule when a node goes away entirely** — both
  operator-initiated (`lwd node drain`/`evacuate`) and fully automatic on
  node loss — is now live; see [Node maintenance &
  failover](#node-maintenance--failover).

## Scheduling & pools

Every registered node (plus the implicit `local` node) belongs to a **pool**
— a group the scheduler picks within. Nodes registered without an explicit
pool, and `local`, live in `"default"`.

```bash
lwd node add web1 deploy@web1.example.com 100.64.0.2 --pool web
lwd node add web2 deploy@web2.example.com 100.64.0.3 --pool web
lwd pool ls
```

```
POOL                 NODES
default              1
web                  2
```

### Placing an app

- **`node` unset** (no `node` line in `lwd.toml`) — the daemon schedules it:
  at apply time, it gathers every reachable node in the app's `pool`
  (`"default"` if `pool` is also unset — always includes `local`) along with
  each one's live [capacity](#the-lwd-agent-transport), picks the node with
  the most free memory (ties broken by free CPU, then name), and deploys
  there. This is a one-time placement decision made at `apply`, not a
  continuously rebalanced one — redeploying the same app schedules it again
  from scratch (it may land somewhere different if capacity shifted). Once
  placed, a scheduled app's surface can still move later — but only via
  drain/evacuate or automatic failover, never a background rebalancer; see
  [Node maintenance & failover](#node-maintenance--failover).
- **`node = "local"`** or **`node = "<registered-name>"`** — pins the app
  exactly as before Phase 11a; the scheduler is never consulted.
- **`pool = "<name>"`** narrows which pool an unpinned app schedules into.
  Ignored (but still validated) if `node` is pinned.
- **`[requirements]`** declares what the app needs — `cpu` (cores, e.g.
  `0.5`) and/or `memory` (a size string, e.g. `"512M"`, `"2G"`) — and the
  scheduler excludes any candidate node that doesn't have that much free.
  Neither field is required; omit `[requirements]` entirely for "runs
  anywhere with room."

```toml
name    = "worker"
image   = "ghcr.io/me/worker:latest"
domain  = "worker.example.com"
port    = 8080
pool    = "web"

[requirements]
cpu    = 1
memory = "1G"
```

A node whose live capacity couldn't be measured (e.g. a briefly-unreachable
remote, or an ssh-only node that only ever reports totals, never live usage)
is **optimistically assumed to fit** any requirement rather than being
excluded outright — this keeps a flaky capacity probe from turning into a
failed deploy. See [The lwd-agent transport](#the-lwd-agent-transport) for
which nodes report precise, live-usage capacity (agent-connected) versus
best-effort totals (ssh-only).

**Single-node stays exactly as before:** with no other nodes registered,
every pool but `default` is empty, `local` is the only candidate, and an
unpinned app always lands there — scheduling changes nothing observable for
a single-machine install.

### Inspecting capacity and placement

```bash
lwd node capacity
```

```
NODE             POOL       CPU            MEM                  DISK                 KNOWN
local            default    1.20/8         6.1G/16.0G           140.0G/200.0G        yes
web1             web        3.60/4         1.0G/8.0G            8.0G/100.0G          yes
web2             web        —              —                    —                    no
```

`CPU` is *used*/*cores*, `MEM` and `DISK` are *available*/*total* — the same
numbers the scheduler ranks candidates by. `KNOWN = no` means this node's
capacity couldn't be measured on the last probe (see the agent-vs-ssh note
above); it's still a placement candidate, just an optimistic one.

```bash
lwd node inspect web1
```

```
NODE:      web1
POOL:      web
TRANSPORT: agent
REACHABLE: true
CPU:       3.60/4 cores
MEM:       1.0G/8.0G
DISK:      8.0G/100.0G

SURFACES
APP                  STATUS     DOMAIN                         IMAGE
worker               running    worker.example.com             ghcr.io/me/worker:latest
```

`node inspect` derives "what's running here" from every app's most recently
recorded deployment spec (there's no separate placement table to query) —
the same data `lwd-web`'s Nodes/Health views and `lwd-mcp`'s `lwd_node_list`
tool render as pool badges and CPU/memory/disk bars/fields.

## The lwd-agent transport

`lwd-agent` is a **fourth, optional binary**: a dumb per-node worker that
replaces raw docker-over-ssh as the transport a registered node's containers
run over. It performs **no orchestration of its own** — every request it gets
is delegated straight through to a local `node.Node` (the same abstraction
the controller uses for its own Docker daemon); all placement, scheduling,
and blue-green logic still lives entirely on the controller.

### Build and run

```bash
CGO_ENABLED=0 go build -o lwd-agent ./cmd/lwd-agent
LWD_AGENT_TOKEN=changeme ./lwd-agent   # binds :8078 by default
```

- `LWD_AGENT_TOKEN` (required) — a shared bearer token; `lwd-agent` refuses to
  start without one. The controller sends this same token (its own
  `LWD_AGENT_TOKEN` env var, wired into `node.NewRegistryResolver`) as
  `Authorization: Bearer <token>` on every request except `/healthz`, which is
  unauthenticated (used only for an external liveness probe). The controller's
  `LWD_AGENT_TOKEN` must match the agent's: if it is missing or wrong, the
  agent's authenticated `/ready` readiness probe (used for transport
  selection, see below) fails, the agent transport is treated as unavailable,
  and lwd falls back to docker-over-ssh. `/ready` returns 200 only when the
  request is authenticated **and** the agent's own Docker daemon is reachable,
  so a node whose agent is up and authorized but whose Docker is down is also
  treated as unavailable and likewise falls back to ssh.
- `LWD_AGENT_ADDR` (default `:8078`) — listen address. Bind this to the
  node's **WireGuard mesh interface** (e.g. `100.64.0.2:8078`), not a public
  one — `lwd-agent` has no TLS of its own; it relies entirely on the mesh for
  transport privacy, same as the plain-HTTP mesh traffic described in
  [Networking model](#networking-model).

### Registering and using it

```bash
lwd node add web1 deploy@web1.example.com 100.64.0.2 --agent http://100.64.0.2:8078
```

Once registered, every deploy targeting `node = "web1"` resolves its
transport fresh: the daemon builds an agent client for the registered
`agent_url` and pings its authenticated `/ready` endpoint with the
controller's `LWD_AGENT_TOKEN`; that endpoint returns 200 only when the agent
is up, the token is valid, **and** the agent's own Docker daemon is reachable.
If it succeeds, the entire deploy (
image presence, network setup, run/remove/health/logs, and the
`docker save|load` image-transfer fallback) goes over the agent's HTTP API
instead of ssh. If the agent doesn't answer — not registered, temporarily
down, answering with a missing/wrong token, or up but with an unreachable
Docker daemon — lwd **falls back to docker-over-ssh automatically** — the exact P9a behavior — so registering a
node without `--agent` (or with a currently unreachable one, or a
misconfigured token) keeps working exactly as before. This fallback is
re-evaluated on every resolve, not cached permanently, so a node's agent
coming back up (or its token being fixed) is picked up on the next deploy
without restarting the daemon.

### Trust boundary

**A registered `lwd-agent`'s bearer token is effectively root on that node.**
Its HTTP API exposes raw Docker primitives (run/remove any container, load
arbitrary image tarballs, read any container's logs) with no per-app or
per-namespace restriction — anyone who can reach `LWD_AGENT_ADDR` with a
valid token can run anything on that machine, exactly as anyone with access
to `ssh://<ssh-host>`'s Docker socket could in the P9a ssh transport. This is
why `lwd-agent` must bind only the mesh interface (never a public one) and
why the shared token must be treated like an ssh private key: keep it out of
version control, rotate it if it leaks, and don't reuse it as any other
credential.

`lwd-web`'s **Nodes** view and `lwd-mcp`'s `lwd_node_list`/`lwd_node_add`/
`lwd_node_remove` tools manage the node registry (including `agent_url`) the
same way the CLI's `lwd node` subcommands do — see
[Web UI](#web-ui-lwd-web) and [Agent access](#agent-access-lwd-mcp).

### Node capacity: agent vs ssh

The transport a node is reached over also decides how precise its reported
[capacity](#scheduling--pools) is:

- **Agent-connected nodes report precise, live usage** — `lwd-agent` reads
  `/proc/meminfo` and `/proc/loadavg` and `statfs`s the root filesystem on
  its own host directly, the same way the controller measures its own
  (`local`) capacity. CPU-used, memory-available, and disk-free all reflect
  what's happening on that machine right now.
- **ssh-only nodes report best-effort totals from `docker info`** — without
  an agent running there to read `/proc` locally, lwd falls back to what the
  remote Docker daemon itself reports (CPU core count, total memory), which
  has no live *usage* figures. `CPUUsed` and `MemAvailable` are left at their
  zero value in this case (still `Known: true` — the totals themselves are
  real, just not usage-aware), so the scheduler's free-memory ranking treats
  such a node as though nothing is using it yet.
- **A probe that fails entirely** (unreachable node, agent down, ssh
  timeout) reports `Known: false` — see [Scheduling &
  pools](#scheduling--pools) for how that's handled (optimistic inclusion,
  never a hard exclusion).

This is the same reason `lwd-agent` is worth running on a node you plan to
schedule onto with `[requirements]`: it's the difference between the
scheduler seeing real headroom versus just "how big is this box."

## Self-healing & health

The daemon runs a **continuous reconciler loop** entirely off the request
path: `lwd apply`/`lwd rollback` still return as soon as *their own* deploy
finishes, and the loop runs in the background on its own schedule —
`cli.runDaemon` starts it in a goroutine at daemon startup, with one pass
immediately (so a freshly started daemon doesn't wait a full interval before
its first check), then one pass on every tick of `LWD_RECONCILE_INTERVAL`
(default `15s`), plus one extra, non-blocking pass nudged right after every
successful `apply`/`rollback` (so a bad deploy gets a fast first look rather
than waiting out the interval). Each pass recovers its own panics and logs
its own errors — a bad reconcile never takes down the daemon.

**What actually gets healed: single-service surfaces that have CRASHED,
EXITED, or otherwise gone missing** (`image` or `[git]`-built apps, with or
without `[[services]]`) — the same population `lwd rollback` operates on.
Each pass checks the current deployment's container; if it's not `running`
at all (gone/exited/crashed), the reconciler heals it by running the exact
blue-green flow `apply` itself uses (recreate — a git app's already-built
image tag is reused, never rebuilt), then re-points the route and records a
fresh deployment row, exactly as if you'd run `lwd rollback` or `redeploy` by
hand. Repeated failures back off exponentially (15s, 30s, 1m, ...) and give
up after `LWD_HEAL_MAX_ATTEMPTS` consecutive attempts (default `5`), at which
point the app is reported `failed` and left alone rather than retried
forever.

A container that is **running but reports Docker `HEALTHCHECK`-unhealthy is
NOT detected or healed by this loop** — it's reported `healthy` in the
snapshot, same as an actually-healthy container. Layered self-healing based
on a running container's `HEALTHCHECK` status is out of scope for this
milestone; today it only recreates surfaces that Docker itself reports as
crashed, exited, or gone.

**What does *not* get healed:**

- **`compose =` apps** — their container lifecycle belongs to `docker
  compose`, not lwd's blue-green surface model; the reconciler doesn't
  observe or touch them at all (no entry appears for them in the health
  snapshot). Recover a broken compose stack with `lwd rollback`, same as
  today.
- **Edge reachability is observed, not acted on** — every pass probes the
  shared edge (Caddy)'s admin API and records what it saw, but there is only
  one edge, so there is nothing to reschedule it onto.
- **Node reachability is observed AND, past a grace period, acted on**: a
  registered node that stays unreachable is automatically failed over — see
  [Node maintenance & failover](#node-maintenance--failover) for the full
  mechanics (grace period, what moves vs. what doesn't, single-node
  behavior).

**Checking on it:**

```bash
lwd health
```

```
NODES
NAME                 TRANSPORT  REACHABLE
local                local      yes

EDGE
caddy reachable: yes

APPS
APP                  STATE      HEAL ATTEMPTS  LAST ERROR
blog                 healthy    0
```

This reads the same snapshot the daemon exposes at `GET /health` (via
`client.Health`, returning `nodes`/`edge`/`apps` — no secret values ever
appear in it), which `lwd-web`'s **Health** panel (see [Web
UI](#web-ui-lwd-web)) also renders live in the browser. `state` is one of
`healthy`, `degraded` (observed unhealthy, not yet healing or waiting on
backoff), `healing` (a heal attempt is in flight), or `failed` (gave up after
`LWD_HEAL_MAX_ATTEMPTS`).

## Node maintenance & failover

Phase 10 heals a dead surface **in place**, on the node it's already on.
Phase 11a schedules an unpinned surface **once**, at `apply` time. Neither
moves an already-running surface **off a node that later disappears** — that
gap is what this section closes: cross-node reschedule, both
operator-initiated (planned maintenance) and fully automatic (unplanned node
loss).

**Only scheduler-placed ("scheduled") surfaces are ever moved.** A surface is
"scheduled" if it landed on its node because `node` was left unset in
`lwd.toml` (see [Scheduling & pools](#scheduling--pools)) — its concrete node
is recorded in the deployment's spec snapshot as
`store.Deployment.Scheduled`. Everything else is left exactly where it is,
no matter what:

- An app with `node = "..."` **pinned** explicitly — you asked for that node
  by name, so lwd never second-guesses it.
- A `compose =` app — its lifecycle belongs to `docker compose`, not lwd's
  blue-green surface model (same carve-out as [self-healing](#self-healing--health)).
- Any backing service (`[[services]]`) — always pinned alongside its
  surface's node.

### Operator-initiated: drain, evacuate, uncordon

```bash
lwd node drain web1      # cordon web1, then move its scheduled surfaces off it
lwd node evacuate web1   # move its scheduled surfaces off it, WITHOUT cordoning
lwd node uncordon web1   # clear the cordon — web1 is eligible for placement again
```

- **`drain`** = cordon (`store.Node.Schedulable = false`, excluding the node
  from future scheduler placement — see [Placing an app](#placing-an-app))
  **then** evacuate: every scheduled surface currently on the node is moved,
  via the same blue-green redeploy path a self-heal uses, onto whichever
  other reachable, schedulable, in-pool node the scheduler ranks highest.
  Use this ahead of planned maintenance (a reboot, a decommission) — nothing
  new will land on the node afterward, and everything already there is gone.
- **`evacuate`** does the move WITHOUT cordoning first: the node stays
  eligible for new placements. Useful for a one-off rebalance without taking
  the node out of rotation.
- **`uncordon`** clears the cordon. It never touches anything already
  running — a cordoned node's existing surfaces (if you evacuated instead of
  draining, or added the node cordoned) keep running untouched.

Every one of these prints (or, via the daemon API/`lwd-web`/`lwd-mcp`,
returns as JSON) an **`EvacuateResult`**: which apps' surfaces actually
**moved**, which were **skipped** (pinned — see above), and which **failed**
to move (with why — typically "no other node has room"). A failed move
leaves the original surface running untouched; nothing is torn down until
its replacement is confirmed live, same blue-green guarantee as every other
redeploy path in lwd.

```
$ lwd node drain web1
Moved: blog
Skipped (pinned): payments-db-proxy
Failed: (none)
```

`lwd node ls` and `lwd node capacity`'s `SCHEDULABLE` column (see
[Inspecting capacity and placement](#inspecting-capacity-and-placement))
show `yes` or `cordoned` for every registered node.

### Automatic failover on node loss

The [continuous reconciler](#self-healing--health) tracks how long each
registered node has been continuously unreachable. Once that streak exceeds
`LWD_FAILOVER_GRACE` (a duration string, e.g. `2m`; **default `60s`**), the
next reconcile pass automatically evacuates every scheduled surface on that
node — exactly as a manual `lwd node drain` would, minus the cordon (a node
that comes back is immediately eligible again; nothing re-populates it
automatically — see [Known limitations](#known-limitations-this-milestone)).
The grace period exists so a brief network blip doesn't trigger a
reschedule; a genuinely dead node is moved off within one grace window of
going dark.

- A **pinned** surface on the lost node is left running (it can't be —
  reachability permitting — it's simply reported `degraded` in the health
  snapshot, same as any surface on an unreachable node) and is **never**
  moved by automatic failover, same as a manual evacuate.
- The old container is removed from the dead node **only if the node is
  still reachable enough to ask** (e.g. an evacuate triggered manually while
  it's flapping); for an actually-gone node, there's nothing to ask, so
  cleanup is skipped and the old deployment row is still marked retired —
  its container simply dies along with the node.
- **Single-node installs have nothing to fail over**: with no other node
  registered, there's no failure to detect (nothing but `local` is ever
  probed for loss) and nowhere to move a surface even if there were —
  behavior is completely unchanged from every earlier phase.
- This is loss-triggered reschedule, not a background rebalancer: a
  healthy fleet is never touched, and a node that comes back after a
  failover stays empty (of that surface) until you place something on it
  again — see the one-shot/no-rebalancing note in [Scheduling &
  pools](#scheduling--pools).

`lwd-web`'s **Nodes** view exposes drain/evacuate/uncordon as buttons (per
node, rendering the returned `EvacuateResult`) and a schedulable/cordoned
badge; its **Health** panel also flags a cordoned node. `lwd-mcp` exposes the
same three operations as `lwd_node_drain`/`lwd_node_evacuate`/
`lwd_node_uncordon` tools — see [Web UI](#web-ui-lwd-web) and [Agent
access](#agent-access-lwd-mcp).

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
  the generated document — see [Backing services](#backing-services). Both
  also have a **Node** dropdown (`Auto`/`local`/a registered node), an
  optional **Pool** dropdown, and optional **CPU**/**Memory** requirement
  fields that emit `node`/`pool`/`[requirements]` — see
  [Scheduling & pools](#scheduling--pools).
- **Live logs** — a per-app log stream over SSE, with a follow toggle.
- **History + rollback** — past deployments for an app, with a one-click
  **Roll back** to any prior deployment (for a git-built app, this redeploys
  the prior built image tag, same as `lwd rollback`).
- **Secrets** — list secret names (never values), set, and delete.
- **Redeploy** — re-apply an app's current deployment spec snapshot (e.g.
  after fixing something on the daemon host, or just to restart it).
- **Config edit** — view and edit an app's `lwd.toml` and re-apply it.
- **Nodes** — a dedicated view (reachable from the header, alongside the
  Fleet overview) listing every registered [node](#multi-node-federation):
  **pool**, ssh host, mesh address, agent URL, live **transport**
  (`agent`/`ssh`), a **reachable/unreachable** indicator probed fresh on
  every load, and a **schedulable/cordoned** badge; an inline form to
  register a new node (name, ssh host, mesh address, optional agent URL,
  optional pool). Each node row has **Drain**, **Evacuate**, and (once
  cordoned) **Uncordon** actions — see [Node maintenance &
  failover](#node-maintenance--failover) — plus a **Remove** action; a
  drain/evacuate renders the returned moved/skipped/failed result inline.
  The Deploy modal's **From Git**/**Builder** tabs also have a **Node**
  dropdown (populated from this same list, plus `Auto` and `local`) and a
  **Pool** dropdown (from `GET /api/pools`) — see
  [Scheduling & pools](#scheduling--pools).
- **Health** — a dedicated view (reachable from the header, alongside Fleet
  and Nodes) rendering the [continuous reconciler's](#self-healing--health)
  live snapshot from `GET /api/health`: every node's pool, transport/
  reachability, a **cordoned** badge (see [Node maintenance &
  failover](#node-maintenance--failover)), and live **CPU/memory/disk** (as
  small meter bars, or "unknown" if the last probe couldn't measure it — see
  [Node capacity: agent vs ssh](#node-capacity-agent-vs-ssh)); the shared
  edge (Caddy) reachability; and every self-healed app's state
  (`healthy`/`degraded`/`healing`/`failed`) with its heal-attempt count and
  last error, auto-refreshing every few seconds. Read-only, and carries no
  secret values.

As with the CLI, compose apps deployed or edited through the UI still need
their compose file present on the daemon host, and a git-built app's repo
must still be reachable (and, for a private repo, the daemon host's git
already authenticated) at deploy time; pasting/generating a full `lwd.toml`
for a single-service, git-built, or backing-service app works fully from the
UI end to end.

## Agent access (lwd-mcp)

`lwd-mcp` is a **third, optional binary** (the fourth being
[`lwd-agent`](#the-lwd-agent-transport), a node worker rather than a daemon
client): a local
[Model Context Protocol](https://modelcontextprotocol.io) server that lets a
coding agent (Claude Code, or any other MCP host) drive lwd directly. Like
`lwd-web`, it is just another client of the daemon's existing unix-socket
API — it makes **zero changes to the daemon** and can do nothing the daemon
API doesn't already permit. It speaks MCP over **stdio only**: no network
listener, no auth of its own (the daemon socket is `0600` and reachable only
by whoever can already run `lwd`/`lwd-mcp` on the box), and it requires
`lwd daemon` to already be running.

### Build

```bash
CGO_ENABLED=0 go build -o lwd-mcp ./cmd/lwd-mcp
```

### Register with an MCP host

`lwd-mcp` locates the daemon socket the same way the CLI does — `LWD_DATA_DIR`
(default `/var/lib/lwd`) + `lwd.sock` — so point it at the same
`LWD_DATA_DIR` the daemon uses if you've customized it. A Claude
Code-style `.mcp.json` entry:

```json
{
  "mcpServers": {
    "lwd": {
      "command": "/path/to/lwd-mcp",
      "args": [],
      "env": {
        "LWD_DATA_DIR": "/var/lib/lwd"
      }
    }
  }
}
```

### Tools

All tool names, inputs, and outputs are stable, plain JSON (no secret value is
ever returned by any tool):

| Tool | Description |
| --- | --- |
| `lwd_list` | List all lwd-managed apps with their current status, image, and domain. |
| `lwd_status` | Get the current status and deployment history of a single app. |
| `lwd_logs` | Get the most recent logs (`tail`-limited, default 200 lines) for an app. |
| `lwd_history` | List recorded deployments (image, status, time) for an app. |
| `lwd_apply` | Deploy an app from an `lwd.toml`, given inline (`toml`) or a local directory (`dir`); optional `node`/`pool`/`requirements` (`cpu`, `memory`) arguments set the same-named `lwd.toml` fields before validating, overriding whatever the toml itself says — see [Scheduling & pools](#scheduling--pools). |
| `lwd_deploy_git` | Deploy an app built from a git repo, from discrete fields (url/ref/dockerfile/name/domain/port/services), without hand-authoring an `lwd.toml`; also takes optional `node`/`pool`/`requirements` arguments. |
| `lwd_rollback` | Roll back an app to its previous deployment. |
| `lwd_remove` | Permanently stop and remove an app. |
| `lwd_secret_set` | Set (or overwrite) a secret value for an app. The value is never echoed back. |
| `lwd_secret_list` | List the names of secrets set for an app — names only, never values. |
| `lwd_secret_delete` | Delete a secret from an app. |
| `lwd_node_list` | List every registered [node](#multi-node-federation): ssh host, mesh address, agent URL, pool, `schedulable` (cordon state), and its live transport (`agent`/`ssh`), reachability, and [capacity](#scheduling--pools) (CPU/memory/disk). |
| `lwd_node_add` | Register (or update) a node: `name`, `ssh_host`, `mesh_addr`, and optional `agent_url`/`pool`. |
| `lwd_node_remove` | Deregister a node. Apps already placed on it are not moved or removed. |
| `lwd_node_drain` | Cordon a node then move every scheduler-placed surface off it onto another fitting node — see [Node maintenance & failover](#node-maintenance--failover). Pinned surfaces are left untouched. |
| `lwd_node_evacuate` | Move every scheduler-placed surface off a node onto another fitting node, without cordoning it. |
| `lwd_node_uncordon` | Clear a node's cordon, making it eligible for scheduler placement again. Never moves or touches anything already deployed. |

`lwd_list`, `lwd_status`, `lwd_logs`, `lwd_history`, `lwd_secret_list`, and
`lwd_node_list` are annotated `readOnlyHint: true`; `lwd_remove`,
`lwd_secret_delete`, `lwd_node_remove`, `lwd_node_drain`, and
`lwd_node_evacuate` are annotated `destructiveHint: true` (drain/evacuate
actually move — and tear down — running surfaces); `lwd_node_uncordon` is
deliberately NOT annotated destructive — it only lifts a placement
restriction and never touches anything already running. lwd-mcp itself asks
nothing before calling the daemon — it relies entirely on **the MCP host's
own per-call approval UI** (e.g. Claude Code's tool-permission prompt) to
gate destructive and state-changing tools before they run; there is no
additional confirmation argument to pass.

## Networking model

- lwd creates and manages one private Docker network, `lwd`, **on every node
  it deploys to**. Every local app container, every remote app container (on
  its own node), and the `lwd-caddy` container (on the controller) join the
  `lwd` network on their respective host.
- A **local** app container publishes **no host ports** — Caddy reaches it by
  container name and port on the `lwd` network. This is why `lwd.toml`'s
  `port` is just the app's container port (e.g. `80` for `traefik/whoami`),
  not a host port to reserve.
- A **remote** app container (placed via [`node =
  "..."`](#multi-node-federation)) publishes an ephemeral host port bound to
  its node's mesh address, since the controller's Caddy has no other way to
  reach across nodes; `lwd.toml`'s `port` is still just the container port —
  the host port is chosen automatically.
- Only `lwd-caddy`, on the controller, binds host ports 80 and 443 for
  traffic, and 2019 (loopback-only) for its admin API, which lwd uses to push
  routing config. A registered node runs no lwd-managed container of its
  own besides the app surfaces (and their backing services) placed on it.

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
- **Multi-node (federation)** — `lwd node add/ls/rm`, `node = "..."`
  placement, a **dumb `lwd-agent` transport** preferred over docker-over-ssh
  when reachable (automatic fallback to ssh otherwise), and `docker
  save|load` image movement (see [Multi-node](#multi-node-federation) and
  [The lwd-agent transport](#the-lwd-agent-transport)) — is live for `image`
  and `[git]`+`[build]` apps (with or without `[[services]]`). A `compose =`
  app can only be placed on `local` for now (validation rejects the
  combination). The single-node path (`node` unset, no other nodes
  registered) is fully unchanged.
- **Capacity-aware scheduling and node pools** (see
  [Scheduling & pools](#scheduling--pools)) are fully live: an app with
  `node` unset is placed, once, at apply time, on the most-free node in its
  `pool` (`[requirements]` optionally gates candidates by free CPU/memory);
  `lwd node capacity`/`lwd node inspect`/`lwd pool ls`, `lwd-web`'s Nodes and
  Health views, and `lwd-mcp`'s `lwd_node_list` all surface pool + live
  capacity. Initial placement is one-shot, not a continuous rebalancer — see
  the next bullet for what moves a surface *after* it's placed.
- **Node drain/evacuate/uncordon and automatic node-loss failover** (see
  [Node maintenance & failover](#node-maintenance--failover)) are fully
  live: `lwd node drain`/`evacuate`/`uncordon` (+ the daemon API, `lwd-web`
  buttons, and `lwd-mcp` tools) for operator-initiated migration off a node,
  and a continuous-reconciler pass that automatically evacuates a node's
  scheduled surfaces once it's been unreachable past `LWD_FAILOVER_GRACE`
  (default `60s`). Only scheduler-placed surfaces ever move — pinned apps,
  compose apps, and backing services never do. Single-node installs are
  unaffected (nothing to fail over with no other node registered).
- [`lwd-web`](#web-ui-lwd-web) (a separate dashboard binary) is fully live:
  overview, live logs, history/rollback, secrets, redeploy, config edit, a
  **Nodes** view (list/add/remove/drain/evacuate/uncordon, live transport +
  reachability + pool + schedulable state), a **Health** view (the
  continuous reconciler's live snapshot, including per-node CPU/memory/disk
  and cordon state), and a Deploy modal with **From Git**, **Builder**, and
  **Paste** tabs (From Git and Builder support declaring backing services,
  picking a node/pool, and setting resource requirements), all as a thin
  client of the same daemon API the CLI uses. Deploying `lwd-web` itself as
  an lwd-managed app and multi-user auth are not built yet.
- [`lwd-mcp`](#agent-access-lwd-mcp) (a separate stdio MCP server binary) is
  fully live: all seventeen tools (list/status/logs/history/apply/deploy_git/
  rollback/remove/secret set-list-delete/node list-add-remove-drain-evacuate-
  uncordon), including pool/requirements/capacity and node
  drain/evacuate/uncordon (see [Node maintenance &
  failover](#node-maintenance--failover)), as a thin client of the same
  daemon API the CLI and `lwd-web` use — no daemon changes, no network
  listener, no secret value ever returned. Networked MCP transport is not
  built yet.
- [Self-healing & health](#self-healing--health) — a continuous reconciler
  loop, off the request path, self-heals dead single-service surfaces
  (`image`/`[git]` apps), observes edge (Caddy) reachability, and
  automatically fails scheduled surfaces over off a node that's gone
  unreachable past grace (see the bullet above) — is fully live. `compose=`
  apps are not self-healed (see [Known
  limitations](#known-limitations-this-milestone)).

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
- **A `compose =` app cannot be placed on a remote node** — see [What's
  remote-capable today](#whats-remote-capable-today), so it can never be
  drained/evacuated/failed-over either (it's always on `local`, and `local`
  can't be cordoned/evacuated).
- **No fail-back and no rebalancing**: once [automatic
  failover](#node-maintenance--failover) moves a surface off a lost node, it
  stays on its new node even after the old one comes back — nothing
  re-populates a recovered node automatically, and a healthy fleet is never
  rebalanced in the background. Surface **replicas** and load-balancing
  across them (so losing one instance is a non-event, and load can be
  spread deliberately) are a later milestone (P12, see `docs/VISION.md`).
- A remote surface's mesh-address traffic between the controller's Caddy and
  the node is **plain HTTP** — lwd relies on the mesh itself (e.g.
  WireGuard) for transport encryption between nodes; it adds none of its
  own on top. This is unchanged by the `lwd-agent` transport: its own
  controller↔agent HTTP traffic is likewise unencrypted at the application
  layer and relies entirely on the mesh — see
  [The lwd-agent transport](#the-lwd-agent-transport)'s trust-boundary note.

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

`internal/mcp`'s `TestIntegrationMCPClientDaemon` does the same for
`lwd-mcp`, also as part of the plain `go test ./...` (no Docker, no build
tag): it starts the same kind of fake-backed daemon on a temp unix socket,
builds a real `internal/client`, wires it into `mcp.NewServer`, and drives
the real go-sdk MCP server over an in-memory transport — `tools/list`
(asserting every one of the seventeen tools is registered), then `lwd_list`
(empty), `lwd_apply` with an inline `lwd.toml`, and `lwd_list` again to
confirm the app appears — proving the agent tool call → `lwd-mcp` →
`internal/client` → daemon chain end to end.

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
- `TestEndToEndRemoteNode` exercises [multi-node
  federation](#multi-node-federation) end to end: it registers `ssh://localhost`
  as a stand-in "remote" node against a real `node.RegistryResolver`, deploys
  an app pinned to it, and asserts (via `docker -H ssh://localhost ...`, not
  just the local daemon view) that the image is present and the surface
  container is running on that node, reachable through Caddy at the node's
  mesh address — then deploys a second, `node = "local"` app in the same run
  and asserts it still works unchanged. It requires key-based ssh to
  `localhost` with a reachable Docker daemon there (`docker -H
  ssh://localhost version` must succeed); it probes for that first and
  `t.Skip`s with a clear message — it never fakes a pass — if unusable, which
  is the common case in a sandboxed/CI environment with no sshd running.
- `TestEndToEndAgentNodeDeploy` exercises the [`lwd-agent`
  transport](#the-lwd-agent-transport) end to end: it starts a real
  `internal/agent.Server` (backed by a real `node.NewLocal()`) on loopback,
  registers a node pointing at it, asserts the resolver actually selects the
  `agent` transport (not ssh — the registered ssh host is deliberately left
  undialable, so a silent fallback would fail the test rather than pass by
  coincidence), and deploys `traefik/whoami` through it, asserting it's
  reachable through Caddy. Because it stands in for a second machine with a
  loopback address, it first probes whether a *sibling* container can reach a
  host-published `127.0.0.1` port at all — some Docker setups (notably Docker
  Desktop's Linux VM) restrict that to the host itself, a limitation a real
  WireGuard mesh address would not have — and `t.Skip`s with a clear message,
  rather than failing, if that probe itself fails.

The **agent transport-selection path** doesn't need any of the above: `go
test ./...` (no Docker, no build tag) always runs
`TestEndToEndAgentTransportSelection`, which starts a real
`internal/agent.Server` (backed by a fake `node.Node`, so no Docker daemon is
needed) on loopback via `httptest.NewServer`, registers it in a real
`store.Store`, and asserts a real `node.RegistryResolver` — the exact type
`cli.runDaemon` wires up — dials its authenticated `/ready` endpoint over
real HTTP and selects `"agent"` (via both `Reachable` and `ResolveMeta`)
rather than falling back to ssh.

Neither does the **[self-heal](#self-healing--health) path**:
`TestEndToEndSelfHeal` also always runs as part of `go test ./...`, entirely
against fakes (`node.Fake`, `router.FakeRouter`, a real `store.Store`) — no
Docker daemon anywhere. It builds the reconciler exactly as `cli.runDaemon`
does, deploys an `image` app, kills its surface the way a real container
death would be observed (removing it from the fake node so a
`ContainerHealth` check reports it gone), calls `Reconcile` once, and asserts
the same things an operator (or the web Health panel, or `lwd health`) would
see: a brand-new running surface, the live route re-pointed at it, a fresh
`running` deployment row, and the health snapshot reporting the app healthy
again.

Nor does the **[scheduler](#scheduling--pools) path**: `TestEndToEndScheduling`
also always runs as part of `go test ./...`, entirely against fakes — a real
reconciler/store/scheduler, two registered `node.Fake`s in pool `"default"`
with deliberately different `MemAvailable`, plus the implicit `local` node
(left with unmeasured, `Known: false` capacity so it can never win the
ranking). It deploys an app with `node` left **unset**, and asserts the
surface actually ran on the higher-capacity node (checking which fake
received the `RunContainer` call, not just the recorded spec) and that the
deployment row's spec snapshot records that concrete node — proving
`resolvePlacement`'s full candidate-gathering (local + every registered node
in the app's pool) and `scheduler.Place`'s most-free ranking end to end, with
no Docker involved.

Nor does the **[node maintenance & failover](#node-maintenance--failover)
path**, also always part of `go test ./...`, entirely against fakes:
`TestEndToEndNodeDrain` deploys an unpinned app across two registered fake
nodes (it lands on the more-free one), then cordons and `EvacuateNode`s that
node — exactly what `POST /nodes/{name}/drain` (and `lwd node drain`) do —
and asserts the surface is redeployed on the other node, the old container
removed, and the old deployment row retired.
`TestEndToEndNodeLossFailover` deploys a scheduled surface plus a surface
explicitly pinned to the same node, marks that node unreachable, seeds
`unreachableSince` past `config.FailoverGrace()` (via the exported
`Reconciler.SeedUnreachableSince`, so the test doesn't sleep), and asserts a
plain `Reconcile` pass moves only the scheduled surface — the pinned one is
left running untouched, reported `degraded`.

All six Docker-gated tests clean up every container, network, node
registration, and (for the git test) built image they create, and will
`t.Skip` (rather than fail) if ports 80/443 are already in use on the host.
