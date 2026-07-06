# lwd — lightweight deploy

> Buy a machine. Install Linux. Install lwd. Deploy software with the UX of
> Vercel, the resilience of Fargate, and the operational simplicity of Docker
> Compose.

lwd is a suckless, self-hosted deployment engine for Docker apps: `lwd apply`
gets you automatic HTTPS and a zero-downtime rollout for free, on your own
hardware, with no control-plane babysitting. It is built in the Unix
tradition — it **composes** Docker, Caddy, Git, and (for a fleet) WireGuard,
rather than replacing any of them. One static Go binary is both the daemon
and the CLI; three optional binaries add a web dashboard, an MCP server for
coding agents, and a per-node worker for multi-machine fleets.

## Table of contents

- [Status](#status)
- [Quickstart](#quickstart)
- [Troubleshooting: cannot reach the daemon / socket not found](#troubleshooting-cannot-reach-the-daemon--socket-not-found)
- [Concepts](#concepts)
- [The four binaries](#the-four-binaries)
- [lwd.toml reference](#lwdtoml-reference)
- [CLI command reference](#cli-command-reference)
- [How deploys work](#how-deploys-work)
- [App shapes](#app-shapes)
  - [Image apps](#image-apps)
  - [Compose apps](#compose-apps)
  - [Deploy from Git](#deploy-from-git)
  - [Backing services](#backing-services)
- [Secrets](#secrets)
- [Multi-node federation](#multi-node-federation)
- [Scheduling, capacity, and pools](#scheduling-capacity-and-pools)
- [Replicas & load balancing](#replicas--load-balancing)
- [Node maintenance & failover](#node-maintenance--failover)
- [Self-healing & health](#self-healing--health)
- [Web UI (lwd-web)](#web-ui-lwd-web)
- [Remote access & running lwd-web as a service](#remote-access--running-lwd-web-as-a-service)
- [Agent access (lwd-mcp)](#agent-access-lwd-mcp)
- [Authoring lwd.toml (Claude skill)](#authoring-lwdtoml-claude-skill)
- [Architecture & networking model](#architecture--networking-model)
- [Security model](#security-model)
- [Configuration reference](#configuration-reference)
- [Roadmap](#roadmap)
- [Development & testing](#development--testing)
- [License](#license)

## Status

lwd is under active, phased development (P1–P12 merged; see
[Roadmap](#roadmap)). Be precise about what "works" means here:

- **Verified against real Docker in this environment:** single-node deploy
  (`lwd apply` → running container → routed through Caddy with a real HTTP
  response), blue-green redeploys, `lwd scale` with round-robin load
  balancing across replicas, `lwd health`/`lwd status`, and `lwd rm`.
- **Implemented and covered by unit + integration tests (fakes, no Docker
  required), plus Docker-gated end-to-end tests that pass locally but are
  NOT exercised on a real multi-machine cluster in this environment:**
  multi-node federation (the ssh and `lwd-agent` transports, `docker
  save|load` image movement), cross-node scheduling/placement, automatic
  node-loss failover, and drain/evacuate. The Docker-gated multi-node e2e
  test (`TestEndToEndRemoteNode`) SKIPs without a real second machine (or
  passwordless ssh to `localhost` with a reachable Docker daemon) — treat
  multi-node as implemented-and-tested-against-fakes, not
  battle-tested-in-production.
- **Not yet built:** P13 (multiple Caddy edges + DNS round-robin), P14
  (first-class postgres/valkey/minio resource drivers), P15 (resource HA via
  Patroni + backups). See [Roadmap](#roadmap).

Repository: [github.com/EObrien60/lwd](https://github.com/EObrien60/lwd).

## Quickstart

This gets a real app running on one box, routed through HTTPS-capable Caddy,
in under five minutes. It mirrors a deploy that has been run and verified
end-to-end against real Docker.

**Prerequisites:** Go 1.25+, Docker, and (for compose apps only) the `docker
compose` CLI plugin. No cgo is required — lwd uses a pure-Go SQLite driver.

### Scripted install (Debian / RHEL family)

`install.sh` fetches build dependencies, builds all four binaries, and installs
them to `/usr/local/bin`. It auto-detects the package manager (**apt** on
Debian/Ubuntu, **dnf/yum** on Fedora/RHEL/CentOS/Rocky/AlmaLinux) and, if the
system Go is older than 1.25 (or absent), downloads a private Go toolchain to
`/usr/local/go` — so it works even on distros whose packaged Go is behind.

```bash
git clone https://github.com/EObrien60/lwd && cd lwd
sudo ./install.sh                # build + install the 4 binaries
# or, in one line (clones for you):
curl -fsSL https://raw.githubusercontent.com/EObrien60/lwd/main/install.sh | sudo bash

# useful flags:
sudo ./install.sh --docker       # also install Docker Engine + compose plugin if missing
sudo ./install.sh --systemd      # also install & enable an lwd.service (daemon under systemd)
sudo ./install.sh --docker --systemd
sudo ./install.sh --prefix /opt/bin --go-version 1.25.4
```

It never uses cgo (pure-Go SQLite), so no C toolchain is installed — only Go,
git, curl, and tar. Run `./install.sh --help` for all options. Docker itself is
a **runtime** dependency (lwd drives a Docker daemon); pass `--docker` or install
it yourself. WireGuard is only needed for multi-node fleets.

> **One-command public setup.** Don't want to touch any config? `sudo
> ./install.sh --public` installs Docker (if missing), builds the binaries,
> and stands up the daemon **and** the web dashboard as systemd services —
> zero interaction. It auto-generates a strong dashboard password and session
> secret, binds the dashboard on `0.0.0.0:8079`, starts both services, and
> prints the dashboard URL and password at the end. Pass `--web-password
> <yours>` to set your own instead of generating one. The dashboard is served
> over **plain HTTP** — fine for a quick look or behind a VPN/SSH tunnel, but
> put a real TLS terminator (or restrict `:8079` to a tunnel) in front of it
> before treating it as internet-facing.

**Updating an existing install.** From a git checkout, pull and re-run with
`--update`:

```bash
cd lwd && git pull && sudo ./install.sh --update
```

`--update` also works standalone — from inside a checkout it fast-forward
pulls the latest commit first (non-fatal if that fails, e.g. local changes;
it just builds the checkout as-is), then rebuilds and reinstalls the four
binaries over the existing ones, then restarts whichever of
`lwd`/`lwd-web`/`lwd-agent` are currently running under systemd so the new
binaries take effect. It never touches `/etc/lwd/*.env` or any other config.
Services that are installed but not running are left stopped (start them
yourself). Compose it with other flags, e.g. `sudo ./install.sh --update --web`
updates the binaries/restarts running services *and* installs the `lwd-web`
unit if it isn't already present.

### Uninstall

```bash
sudo ./install.sh --destroy          # (alias: --remove) uninstall, asks about config/apps
sudo ./install.sh --destroy-all      # uninstall + wipe config, state, and all deployed apps
sudo ./install.sh --destroy --no-interactive   # uninstall, keep config/apps, no prompt
```

`--destroy` (alias `--remove`) never builds or installs anything — it always
stops/disables/removes the `lwd`/`lwd-web`/`lwd-agent` systemd units (if
present) and removes the four binaries from `$PREFIX`. It then asks,
interactively, whether to also remove `/etc/lwd` (config), `/var/lib/lwd`
(daemon state — the encrypted secret key and deployment DB), and **all**
deployed apps + the `lwd-caddy` ingress container + the private `lwd` Docker
network. The prompt defaults to **No** — press Enter or answer anything but
`y`/`Y` to keep them.

- `--destroy-all` skips the prompt and removes everything above
  unconditionally (config, daemon state, deployed apps, `lwd-caddy`, the
  `lwd` network) — use this for a full teardown.
- `--no-interactive` (with `--destroy`, not `--destroy-all`) skips the prompt
  and defaults to **keeping** config/apps — an install-only uninstall, safe
  for scripts.
- Piped installs (`curl ... | bash -s -- --destroy`) have no controlling
  terminal to prompt against, so they also default to keeping config/apps
  rather than hanging on a read — re-run with `--destroy-all` if you want
  those gone too.

Every step is best-effort and idempotent: a missing unit, binary, container,
or directory is not an error. **Named Docker volumes (e.g. postgres/minio
data) are never auto-deleted**, even by `--destroy-all` — list them with
`docker volume ls` and remove the ones you actually want gone with `docker
volume rm <name>`.

### Or build manually

```bash
# 1. Build the daemon + CLI (one binary does both)
CGO_ENABLED=0 go build -o lwd ./cmd/lwd

# 2. Start the daemon (creates /var/lib/lwd, a private `lwd` Docker
#    network, and a managed `lwd-caddy` container fronting ports 80/443)
sudo LWD_DATA_DIR=/var/lib/lwd ./lwd daemon &

# 3. Define an app
mkdir myapp && cat > myapp/lwd.toml <<'EOF'
name   = "app"
image  = "traefik/whoami:latest"
domain = "app.localhost"
port   = 80
EOF

# 4. Deploy it
./lwd apply ./myapp
# deployed app (traefik/whoami:latest) container <id>

# 5. Hit it through Caddy
curl -H "Host: app.localhost" http://127.0.0.1/
# Hostname: ...
# (response headers include `Via: 1.1 Caddy`)
```

`app.localhost` gets an internally self-signed certificate automatically
(no public CA involved) because it's a `.localhost`/bare hostname — use a
real public FQDN in `domain` to get a real Let's Encrypt certificate instead.

From here:

```bash
./lwd scale app 3      # 3 load-balanced replicas — round-robin verified
./lwd status app       # replica count + per-replica node
./lwd health           # nodes / edge / self-heal state
./lwd ls                # every app's status
./lwd logs app -f       # stream logs
./lwd rm app            # stop and deregister
```

## Troubleshooting: cannot reach the daemon / socket not found

If `lwd ls` (or any other client command) fails with something like `cannot
reach the lwd daemon at unix:///var/lib/lwd/lwd.sock — is it running?`, the
daemon either isn't running or never finished starting:

- **The daemon needs root** — it creates `/var/lib/lwd`, a Docker network,
  and a managed `lwd-caddy` container, and needs to talk to the Docker
  socket. Run it with `sudo lwd daemon`, or point it at a directory your
  user already owns via `LWD_DATA_DIR=<dir> lwd daemon` (no sudo needed, but
  Docker access is still required — e.g. your user must be in the `docker`
  group).
- **A backgrounded daemon may have failed to start.** `lwd daemon &` prints
  its error and exits silently in the background if, say, `mkdir
  /var/lib/lwd` or the Docker connection fails — easy to miss. Check its
  output, or if it's running as a systemd service (see `install.sh
  --systemd`), check `journalctl -u lwd`.
- **The CLI and daemon must agree on the socket path.** Both derive it from
  `LWD_DATA_DIR` (default `/var/lib/lwd`, socket at `<data-dir>/lwd.sock`),
  or you can pin it directly with `LWD_SOCKET` on the client side. If you
  started the daemon with a custom `LWD_DATA_DIR` (or `LWD_SOCKET`), set the
  same variable when running CLI commands, or the client will look for a
  socket that isn't there.
- **For a remote daemon**, set `LWD_DAEMON=host:port` (and `LWD_API_TOKEN`
  if the daemon requires auth) instead of relying on the local socket — see
  [Remote access & running lwd-web as a service](#remote-access--running-lwd-web-as-a-service).

The friendly error message above is meant to point at these fixes directly;
if it's still unclear, the three options in order of preference are `sudo
lwd daemon`, `LWD_DATA_DIR=<writable-dir> lwd daemon`, or `sudo ./install.sh
--systemd` to run it as a managed service.

### Troubleshooting: `:80`/`:443` already in use

lwd's own managed Caddy (`lwd-caddy`) is the single thing that owns host
ports 80 and 443 — it's the edge that routes every deployed app's `domain`.
If something else on the box is already bound to 80 or 443 (another
webserver, a stray Caddy/nginx, a previous `lwd-caddy` container from a
different Docker context), the daemon will fail to start the edge:

- If a **stopped or orphaned `lwd-caddy` container** already exists, the
  daemon adopts it if it's healthy and running; otherwise it removes the
  stale container and recreates it. If that recreation still fails (because
  something *else* holds the port), you'll get a clear error naming the port
  and the container it couldn't bind, rather than a silent hang.
- **Free the ports or stop the conflicting service** — e.g. `sudo systemctl
  stop nginx` / `apache2`, or `docker ps` to find and stop whatever else is
  publishing `0.0.0.0:80`/`:443`, then retry `lwd daemon` (or restart the
  `lwd` systemd unit).
- This is independent of `lwd-web`'s own port (`8079` by default) — the
  dashboard doesn't touch 80/443 at all.

## Concepts

lwd's mental model has three kinds of things, plus the machinery that keeps
them converged:

- **Surfaces** — stateless, disposable application containers: sites, APIs,
  workers. Surfaces scale horizontally (see [Replicas](#replicas--load-balancing)),
  move between machines (see [Node maintenance & failover](#node-maintenance--failover)),
  and are always deployed blue-green — never patched in place.
- **Resources** — stateful things with identity and their own storage: a
  Postgres database, a cache, an object store. lwd runs them today as
  [backing services](#backing-services) pinned to one node's disk — they are
  **never** blue-greened and **never** auto-migrated. (A dedicated
  resource-driver model with HA is planned; see [Roadmap](#roadmap), P14/P15.)
- **Nodes** — dumb compute: a machine's CPU, RAM, disk, and network. Nodes
  don't know about applications; applications are scheduled onto them. The
  implicit `local` node is the machine `lwd daemon` runs on; more can be
  [registered](#multi-node-federation).
- **Deployments & generations** — every `lwd apply`/`rollback`/`scale`
  produces a new deployment row with a snapshotted, fully-resolved spec
  (image, env, secrets already substituted where relevant, replica set). A
  rollback restores that exact snapshot, not a re-resolution of whatever
  `lwd.toml` says today.
- **Pools** — a named grouping of nodes the [scheduler](#scheduling-capacity-and-pools)
  picks within, so you can dedicate a subset of your fleet to a class of app.
- **The edge** — the single shared Caddy instance (`lwd-caddy`) that
  terminates TLS and reverse-proxies every app's `domain` to its current
  surface(s). It is the only container that ever binds host ports 80/443.
- **The reconciler** — a continuous control loop, off the request path, that
  self-heals dead surfaces, observes node/edge reachability, and (past a
  grace period) fails scheduled surfaces off a node that's gone. See
  [Self-healing & health](#self-healing--health).

## The four binaries

| Binary | What it is | Build |
| --- | --- | --- |
| `lwd` | The daemon (`lwd daemon`) **and** the CLI client, in one static binary. The daemon owns Docker, the SQLite store, the router, secrets, and the reconciler; the CLI is just a client of its unix socket. | `CGO_ENABLED=0 go build -o lwd ./cmd/lwd` |
| `lwd-web` | An optional browser dashboard — a thin HTTP client of the same daemon socket the CLI uses. See [Web UI](#web-ui-lwd-web). | `CGO_ENABLED=0 go build -o lwd-web ./cmd/lwd-web` |
| `lwd-mcp` | An optional local [MCP](https://modelcontextprotocol.io) server (stdio only) that lets a coding agent drive lwd. See [Agent access](#agent-access-lwd-mcp). | `CGO_ENABLED=0 go build -o lwd-mcp ./cmd/lwd-mcp` |
| `lwd-agent` | An optional, dumb per-node worker that replaces docker-over-ssh as the transport to a registered node. See [Multi-node federation](#multi-node-federation). | `CGO_ENABLED=0 go build -o lwd-agent ./cmd/lwd-agent` |

Build all four at once:

```bash
CGO_ENABLED=0 go build ./cmd/lwd ./cmd/lwd-web ./cmd/lwd-mcp ./cmd/lwd-agent
```

Every binary supports a `version` subcommand (`lwd version`, `lwd-web
version`, ...) that prints its name and build version.

## lwd.toml reference

An app is one `lwd.toml` file in a directory (`lwd apply <dir>` reads
`<dir>/lwd.toml`). Every field below is drawn directly from
`internal/spec/spec.go`'s `App` struct and `Validate()`.

### Core

| Field | Type | Default | Required? | Meaning |
| --- | --- | --- | --- | --- |
| `name` | string | — | yes | App identity. Must match `[a-zA-Z0-9][a-zA-Z0-9_.-]*`. |
| `image` | string | — | yes, for an [image app](#image-apps) (mutually exclusive with `git`/`compose`) | The OCI image to run. |
| `domain` | string | — | yes for [git](#deploy-from-git) and [compose](#compose-apps) apps; **not enforced** for a plain image app (one with no `domain` simply gets no Caddy route) | The FQDN Caddy routes to this app. A public FQDN gets a real ACME cert; a `.localhost`/bare hostname gets an internally self-signed one. |
| `port` | int | — | yes, for image/git/compose apps | The app's **container** port (not a host port — lwd never publishes app ports directly). |
| `env` | map[string]string | `{}` | no | Plain (non-secret) environment variables passed to the container (or, for compose, to the `docker compose` process). |
| `secrets` | []string | `[]` | no | **Names** only — see [Secrets](#secrets). Each must match `[A-Za-z_][A-Za-z0-9_]*`. A secret wins over a same-named `env` key. |

### Placement

| Field | Type | Default | Required? | Meaning |
| --- | --- | --- | --- | --- |
| `node` | string | `""` (unset) | no | `""` lets the [scheduler](#scheduling-capacity-and-pools) place the app; `"local"` pins it to the controller; any other value pins it to a [registered node](#multi-node-federation) name. A `compose` app may only be `""`/`"local"` — remote compose is rejected. |
| `pool` | string | `"default"` | no | Narrows which pool an *unpinned* app schedules into. Must match `[A-Za-z0-9][A-Za-z0-9_-]*`. Ignored (but still validated) if `node` is pinned. |
| `replicas` | int | `1` | no | Number of load-balanced surface replicas, 1–50. Not supported with `compose` or `[[services]]`. See [Replicas & load balancing](#replicas--load-balancing). |

### `[health]`

| Field | Type | Default | Required? | Meaning |
| --- | --- | --- | --- | --- |
| `health.path` | string | `""` | no | An HTTP path polled for a 2xx through Caddy during blue-green staging. If unset, falls back to the image's own Docker `HEALTHCHECK`, then to a plain liveness check. See [How deploys work](#how-deploys-work). |
| `health.timeout` | duration string (e.g. `"30s"`) | `30s` | no | How long the layered health check is given before the candidate is considered failed. |

### `[requirements]`

| Field | Type | Default | Required? | Meaning |
| --- | --- | --- | --- | --- |
| `requirements.cpu` | float | unset | no | Minimum free CPU cores a candidate node must have (e.g. `0.5`, `2`). Must be `>= 0`. |
| `requirements.memory` | size string | unset | no | Minimum free memory (e.g. `"512M"`, `"2G"`, `"1Ki"`). Binary units throughout (K/Ki, M/Mi, G/Gi, T/Ti all base-1024) — parsed by `spec.ParseSize`. |

Omit `[requirements]` entirely for "runs anywhere with room." See
[Scheduling, capacity, and pools](#scheduling-capacity-and-pools).

### `[git]` + `[build]` (build-from-source)

| Field | Type | Default | Required? | Meaning |
| --- | --- | --- | --- | --- |
| `git.url` | string | — | yes, if `[git]` is present | Clone URL. Allowed forms: `http(s)://`, `git://`, `ssh://`, `file://`, or scp-like `user@host:path`. Command-executing transports (`ext::`, `fd::`, a bare local path) are rejected. |
| `git.ref` | string | `"main"` | no | Branch, tag, or commit SHA. A branch tracks its latest commit on every deploy. |
| `git.path` | string | `""` (repo root) | no | Subdirectory within the repo used as the **build context** — see note below. No `..` segments allowed. |
| `build.dockerfile` | string | `"Dockerfile"` semantics via `docker build -f` | no | Dockerfile path, relative to `git.path`. No `..` segments allowed. |
| `build.context` | string | unset | no | Parsed and path-validated (no `..` segments) but **not currently used** by the build: the effective build context is always `git.path`. Documented here for accuracy — leave it unset. |

`[git]` requires `[build]`, `domain`, and `port`, and cannot be combined with
`image` or `compose`. See [Deploy from Git](#deploy-from-git).

### Compose apps

| Field | Type | Default | Required? | Meaning |
| --- | --- | --- | --- | --- |
| `compose` | string (path) | — | yes, to select the compose shape | Path to a `docker-compose.yml`, relative to the app directory (or absolute). |
| `service` | string | — | yes, if `compose` is set | The compose service Caddy fronts. |

Cannot combine with `image`, `git`, `[build]`, `[[services]]`, or
`replicas > 1`; cannot be placed on a non-local `node`. See
[Compose apps](#compose-apps).

### `[[services]]` (backing services)

| Field | Type | Default | Required? | Meaning |
| --- | --- | --- | --- | --- |
| `services[].name` | string | — | yes | Must match `[a-z0-9][a-z0-9-]*`, unique within the app. |
| `services[].image` | string | — | yes | Image for the backing container. |
| `services[].command` | string | `""` | no | Overrides the image's default command. |
| `services[].env` | map[string]string | `{}` | no | Plain env vars for the backing container. |
| `services[].secrets` | []string | `[]` | no | Secret names, resolved and injected the same fail-closed way as the app's own `secrets`. |
| `services[].volume` | string | `""` | no | `name:path` for a persistent named Docker volume, or a bind-mount path. |

Only valid on `image`/`[git]` apps (not `compose`), and not combined with
`replicas > 1`. See [Backing services](#backing-services).

### Not supported

`surfaces` is parsed (as `[]string`) but **always rejected** by `Validate()`
— it exists so a document that sets it fails with a clear error rather than
being silently ignored.

### Example: minimal image app

```toml
name   = "blog"
image  = "ghcr.io/me/blog:latest"
domain = "blog.example.com"
port   = 8080

[health]
path    = "/healthz"
timeout = "30s"
```

### Example: build from Git

```toml
name   = "myapp"
domain = "myapp.example.com"
port   = 8080

[git]
url = "https://github.com/me/myapp"
ref = "main"

[build]
dockerfile = "Dockerfile"
```

### Example: replicas + pool + requirements

```toml
name     = "worker"
image    = "ghcr.io/me/worker:latest"
domain   = "worker.example.com"
port     = 8080
pool     = "web"
replicas = 3

[requirements]
cpu    = 1
memory = "1G"
```

### Example: app with a backing Postgres

```toml
name   = "myapp"
image  = "ghcr.io/me/myapp:latest"
domain = "myapp.example.com"
port   = 8080
env    = { DATABASE_URL = "postgres://app:app@db:5432/app" }

[[services]]
name   = "db"
image  = "postgres:16"
env    = { POSTGRES_USER = "app", POSTGRES_PASSWORD = "app", POSTGRES_DB = "app" }
volume = "db-data:/var/lib/postgresql/data"
```

### Example: compose app

```toml
name    = "webapp"
compose = "docker-compose.yml"
service = "web"
domain  = "webapp.example.com"
port    = 8080
env     = { LOG_LEVEL = "info" }
secrets = ["DATABASE_URL"]

[health]
path = "/healthz"
```

## CLI command reference

Every command below is dispatched from `internal/cli/cli.go`'s `Run`. All
client subcommands talk to the daemon over its unix socket.

### Apps

| Command | Flags | What it does |
| --- | --- | --- |
| `lwd apply [dir]` | — | Deploys `<dir>/lwd.toml` (default `.`). Blue-green for image/git apps, delegated to `docker compose` for compose apps. |
| `lwd ls` | — | Lists every app: name, status, domain, replica count, image. |
| `lwd status <app>` | — | One app's status plus (when available) per-replica node/container detail. |
| `lwd logs <app>` | `-f`, `--follow` | Prints (or streams) the app's container logs. |
| `lwd history <app>` | — | Lists past deployments: ID, status, image, created time. |
| `lwd rollback <app>` | — | Redeploys the previous deployment's exact snapshotted spec. |
| `lwd scale <app> <replicas>` | — | Redeploys the app's current spec at a new replica count (`replicas` must be a positive integer). |
| `lwd rm <app>` | — | Stops and deregisters the app. |

```
$ lwd ls
APP                  STATUS     DOMAIN                         REPLICAS  IMAGE
blog                 running    blog.example.com               1         ghcr.io/me/blog:latest
```

```
$ lwd apply ./myapp
deployed app (traefik/whoami:latest) container 3f2a9c1b7e4d
```

### Secrets

| Command | What it does |
| --- | --- |
| `lwd secret set <app> <KEY>` | Sets a secret's value, read from **stdin**. |
| `lwd secret ls <app>` | Lists secret names for the app (never values). |
| `lwd secret rm <app> <KEY>` | Deletes a secret. |

```bash
echo -n 'postgres://...' | lwd secret set blog DATABASE_URL
# secret DATABASE_URL set for blog; redeploy to apply
```

### Nodes

| Command | Flags | What it does |
| --- | --- | --- |
| `lwd node add <name> <ssh-host> <mesh-addr>` | `--agent <url>`, `--pool <name>` | Registers a node. `<ssh-host>` is anything `ssh` accepts; `<mesh-addr>` is the address the controller reaches it at for app traffic (meant to be a WireGuard mesh address). |
| `lwd node ls` | — | Lists every registered node: ssh host, mesh addr, agent URL, pool, live transport, schedulable state, reachability. |
| `lwd node rm <name>` | — | Deregisters a node. Apps already placed on it are not moved. |
| `lwd node capacity` | — | Lists every node's live CPU/mem/disk (used/total or available/total) and whether it was measured (`KNOWN`). |
| `lwd node inspect <name>` | — | One node's pool, transport, reachability, capacity, and the surfaces currently placed on it. |
| `lwd node drain <name>` | — | Cordons the node, then moves every scheduler-placed surface off it. |
| `lwd node evacuate <name>` | — | Moves every scheduler-placed surface off the node, **without** cordoning it. |
| `lwd node uncordon <name>` | — | Clears a cordon; never moves anything already running. |

```
$ lwd node add web1 deploy@web1.example.com 100.64.0.2 --agent http://100.64.0.2:8078 --pool web
added node web1 (ssh deploy@web1.example.com, mesh 100.64.0.2, agent http://100.64.0.2:8078, pool web)
```

```
$ lwd node capacity
NODE             POOL       SCHEDULABLE  CPU            MEM                  DISK                 KNOWN
local            default    yes          1.20/8         6.1G/16.0G           140.0G/200.0G        yes
web1             web        yes          3.60/4         1.0G/8.0G            8.0G/100.0G          yes
web2             web        cordoned     —              —                    —                    no
```

```
$ lwd node drain web1
Moved: blog
Skipped (pinned): payments-db-proxy
```

### Pools

| Command | What it does |
| --- | --- |
| `lwd pool ls` | Lists every pool and its node count. |

```
$ lwd pool ls
POOL                 NODES
default              1
web                  2
```

### Fleet health & daemon

| Command | What it does |
| --- | --- |
| `lwd health` | Prints the reconciler's live snapshot: node reachability, edge (Caddy) reachability, and per-app self-heal state (`healthy`/`degraded`/`healing`/`failed`) with heal-attempt counts. |
| `lwd daemon` | Runs the daemon in the foreground: brings up the `lwd` Docker network + `lwd-caddy`, starts the reconciler loop, and listens on the unix socket. |

```
$ lwd health
NODES
NAME                 TRANSPORT  REACHABLE
local                local      yes

EDGE
caddy reachable: yes

APPS
APP                  STATE      HEAL ATTEMPTS  LAST ERROR
blog                 healthy    0
```

## How deploys work

This describes the path for **image** and **git-built** apps (identical
once the image is built); [compose apps](#compose-apps) use a different,
explicitly non-zero-downtime path.

Every `apply` is a blue-green swap, never an in-place recreate:

1. A new surface container is started alongside whatever is currently
   running, attached only to the private `lwd` network — it never publishes
   a host port itself.
2. It's staged behind a throwaway internal hostname on the shared Caddy
   router and health-checked **through Caddy** (never by talking to the
   container directly), using a layered policy:
   - if `health.path` is set, poll it for a 2xx through Caddy;
   - otherwise, if the image declares a Docker `HEALTHCHECK`, honor it;
   - otherwise, fall back to a liveness check (container still running and
     reachable through Caddy at all).
3. Only once the candidate passes health does the real domain flip to it.
   The previous container keeps serving every request until that instant —
   there is no downtime window.
4. The old surface is retired and removed.

A failed candidate never touches the live route: the new container and its
staging route are torn down, the failure is recorded, and whatever was
already running keeps serving traffic untouched.

Every deployment's resolved spec is snapshotted, so `lwd rollback` restores
the exact previous image/config from that snapshot — not a re-resolution of
whatever `lwd.toml` currently says — via the same blue-green path. A
git-built app's rollback redeploys the previous deployment's already-built
image tag directly (no re-clone, no rebuild).

## App shapes

### Image apps

The default shape: declare `image` + `domain` + `port` and deploy. See
[the minimal example](#example-minimal-image-app) above.

### Compose apps

Declare `compose` + `service` instead of `image` to run a multi-container
[Docker Compose](https://docs.docker.com/compose/) stack. This requires the
**`docker compose` CLI plugin** on the daemon's host (single-service `image`
apps don't need it). `lwd.toml` validation rejects mixing `compose` with
`image` or `[build]`; `surfaces` and `[[services]]` are not supported on a
compose app, and it can only run on `local`.

**How it deploys (in-place recreate, not blue-green):**

1. `docker compose -p lwd-<app> -f <compose file> up -d --remove-orphans`.
   Compose only recreates services whose image or config changed — an
   unchanged backing service (a database, a cache) is left running
   untouched. This is the model's core guarantee: redeploying to ship a new
   web-service image does not restart the database.
2. lwd resolves `service`'s running container, joins it to the shared `lwd`
   network, and points `domain` at it through Caddy.
3. It's health-checked through the **live** route using the same layered
   policy as an image app.

**Honest tradeoff: this is not zero-downtime.** Because `docker compose`,
not lwd's blue-green surface machinery, owns the container lifecycle, the
web service takes a brief in-place restart on every redeploy, and the route
flips to it before health-gating runs. If the health check then fails, lwd
does **not** tear anything down — the possibly-broken new stack is left
live and the deployment is recorded as failed. Run `lwd rollback <app>` to
restore the previous compose content and recover.

`env` and declared `secrets` are resolved fail-closed (aborting before
`docker compose` ever runs on any unset secret) and passed as environment
variables to the `docker compose` process, so the compose file's `${VAR}`
interpolation can reference them. Every deploy snapshots the compose file's
content, so `lwd rollback` restores the exact prior stack. `lwd rm <app>`
runs `docker compose down` against the stored content (named volumes are
left in place).

### Deploy from Git

Declare `[git]` + `[build]` instead of `image` to build straight from a repo
— no GitHub/GitLab API, OAuth, or webhook receiver involved; `lwd apply`
shells out to the host's own `git clone` and then `docker build`. Private
repos work if the box's own git is already authenticated (an SSH key or
credential helper) — lwd manages no git credentials itself.

**Build → blue-green flow:**

1. lwd clones `git.ref` into a throwaway temp directory (removed after the
   build), resolving it to a commit SHA.
2. It runs `docker build` against the checked-out tree, tagging the result
   `lwd-build/<app>:<shortsha>`. If that exact tag already exists locally
   (redeploying the same commit), the build is skipped.
3. The built image is deployed exactly like an `image` app: a fresh
   zero-downtime blue-green surface. No registry is involved — the image
   only needs to exist on the local Docker daemon.
4. The deployment record stores the resolved commit SHA and the built image
   tag.

Every build is tagged and kept by commit SHA, so `lwd rollback <app>`
redeploys the previous deployment's already-built image tag directly —
no re-clone, no rebuild. Old `lwd-build/*` tags are not pruned automatically
yet, which costs local disk over time.

Git deploy only supports the git-repo-has-a-Dockerfile → single-service
shape: `lwd.toml` remains the source of truth for name/domain/port/env/
secrets/backing services, and lwd never reads an `lwd.toml` committed inside
the cloned repo. `lwd-web`'s Deploy modal has a matching **From Git** tab.

### Backing services

Any `image`- or `[git]`-based app can declare pinned backing services (a
database, a cache, an object store) via `[[services]]` — see the
[reference](#services-backing-services) and [example](#example-app-with-a-backing-postgres)
above.

- **Pinned, never blue-greened.** Backing services run in a generated
  `docker-compose` project (`lwd-<app>`) on a dedicated per-app network, and
  are not torn down or recreated on every `apply`/`rollback` of the app
  itself — an unchanged backing service (and its data) survives redeploy
  after redeploy.
- **Named volumes persist** across redeploy, rollback, and `lwd rm` (which
  runs `compose down` without `-v`).
- **Reachable by name.** The app's surface container joins both the shared
  `lwd` network (so Caddy can reach it) and the per-app backing network (so
  it can reach `db`, `minio`, etc. by container name). Backing services
  publish no host ports.
- **Not for compose apps.** A `compose` app already defines its whole stack
  directly; validation rejects `[[services]]` there.
- **Removal:** `lwd rm <app>` removes the surface **and** runs `compose
  down` on the backing project — this removes the backing containers, but
  named volumes are left in place.

## Secrets

Apps declare secret **names** only:

```toml
secrets = ["DATABASE_URL", "API_KEY"]
```

Values live in the daemon's own store, encrypted at rest, set out-of-band:

```bash
lwd secret set blog DATABASE_URL   # reads the value from stdin
lwd secret ls blog                 # lists names only — never values
lwd secret rm blog DATABASE_URL
```

At deploy time the reconciler resolves every declared name and injects it
into the surface container's environment (a secret wins over a same-named
`env` key). Resolution is **fail-closed**: if any declared secret has no
value set, `apply` aborts before starting anything — the new container is
never created and whatever was already running keeps serving traffic
untouched.

Values are encrypted with **AES-256-GCM** using a key generated on first use
and stored at `<data_dir>/secret.key` with **`0600`** permissions. Once set,
a value is **never read back out of the daemon** — the API and CLI only
expose `set`, `ls` (names only), and `rm`; there is no `get`.

## Multi-node federation

> Implemented and covered by fake-backed tests; the Docker-gated multi-node
> end-to-end test is not exercised on a real cluster in this environment —
> see [Status](#status).

By default every app deploys to `local`, the machine `lwd daemon` runs on.
lwd can additionally deploy to another registered machine over
**docker-over-ssh** (or the [`lwd-agent` transport](#the-lwd-agent-transport)),
moving whatever image it needs there with `docker save | ssh | load` — no
image registry required. lwd manages no ssh credentials of its own: the
daemon's own `ssh` (agent, keys, `~/.ssh/config`) must already reach the
target non-interactively.

### Register a node

```bash
lwd node add web1 deploy@web1.example.com 100.64.0.2
lwd node add web1 deploy@web1.example.com 100.64.0.2 --agent http://100.64.0.2:8078
lwd node ls
lwd node rm web1
```

- `<ssh-host>` is anything `ssh` itself accepts (`user@host`, or a `Host`
  alias from `~/.ssh/config`).
- `<mesh-addr>` is meant to be a **WireGuard mesh** address: the central
  Caddy talks plain HTTP to a remote surface across it, so it should be a
  private, trusted network, not a public IP. Standing up the mesh itself is
  outside lwd's scope, same as ssh — bring your own.
- `--agent <url>` is optional: the base URL of a running `lwd-agent` on that
  node. If set, lwd prefers it over docker-over-ssh whenever reachable.
- `local` is implicit and never appears in `lwd node ls`.

### Place an app on a node

```toml
name   = "worker"
image  = "ghcr.io/me/worker:latest"
domain = "worker.example.com"
port   = 8080
node   = "web1"
```

`node = "local"` pins to the controller; omitting `node` hands placement to
the [scheduler](#scheduling-capacity-and-pools). A single-node install (no
other nodes registered, `node` unset) is completely unaffected — there's
only ever one place to run.

### How a remote deploy works

1. **The build always stays on the controller.** A git app's `docker build`
   never runs on the target node; only the resulting image is shipped.
2. Before starting the container, lwd tries an ordinary registry pull on
   the target itself first; if that doesn't produce the image (e.g. a
   locally-built tag never pushed anywhere), lwd transfers it directly:
   `docker save` on the controller, piped over the transport into `docker
   load` on the target.
3. The surface runs on the target, reached over whichever
   [transport](#the-lwd-agent-transport) is currently selected — a
   registered `lwd-agent` if reachable, otherwise the Docker SDK's ssh
   connection helper. A **local** surface publishes no host port; a
   **remote** surface publishes an ephemeral host port bound to the node's
   mesh address, and Caddy's upstream becomes `<mesh-addr>:<published-port>`.
   Blue-green staging, health-checking, and cutover work exactly as on
   `local`.
4. A remote app's `[[services]]` are brought up with `docker compose`
   **targeting the node's own daemon** (`DOCKER_HOST=ssh://<ssh-host>`),
   pinned alongside the surface on that node.

**What's remote-capable today:** `image` and `[git]`+`[build]` apps, with or
without `[[services]]`. **A `compose` app cannot be placed on a remote node**
— validation rejects it outright, because `applyCompose` doesn't yet thread
a resolved node's `DOCKER_HOST` through.

### The lwd-agent transport

`lwd-agent` is a dumb per-node worker that replaces raw docker-over-ssh. It
performs **no orchestration of its own** — every request is delegated
straight to a local `node.Node` (the same abstraction the controller uses
for its own Docker daemon); all placement, scheduling, and blue-green logic
stays entirely on the controller.

```bash
CGO_ENABLED=0 go build -o lwd-agent ./cmd/lwd-agent
LWD_AGENT_TOKEN=changeme ./lwd-agent   # binds :8078 by default
```

- `LWD_AGENT_TOKEN` (required) — a shared bearer token; `lwd-agent` refuses
  to start without one. The controller sends its own `LWD_AGENT_TOKEN` as
  `Authorization: Bearer <token>` on every request except `/healthz`
  (unauthenticated liveness probe).
- `LWD_AGENT_ADDR` (default `:8078`) — listen address. Bind this to the
  node's WireGuard mesh interface, not a public one — `lwd-agent` has no TLS
  of its own and relies entirely on the mesh for transport privacy.

Once registered with `--agent <url>`, every deploy targeting that node
resolves its transport fresh: the daemon pings the agent's authenticated
`/ready` endpoint (200 only when the token is valid **and** the agent's own
Docker is reachable). If it succeeds, the whole deploy goes over the
agent's HTTP API instead of ssh; if it fails for any reason, lwd **falls
back to docker-over-ssh automatically**, re-evaluated on every resolve (not
cached), so a node's agent coming back up is picked up on the next deploy.

**Trust boundary:** a registered `lwd-agent`'s bearer token is effectively
**root on that node** — its HTTP API exposes raw Docker primitives
(run/remove any container, load arbitrary image tarballs, read any
container's logs) with no per-app restriction. Bind it only to the mesh
interface, and treat the token like an ssh private key.

**Capacity precision also depends on transport:** agent-connected nodes
report precise, live usage (reading `/proc/meminfo`, `/proc/loadavg`, and
`statfs` on their own host); ssh-only nodes report best-effort totals from
`docker info` (no live usage figures, but still `Known: true`); a probe that
fails entirely reports `Known: false` (optimistically included as a
placement candidate rather than excluded — see
[Scheduling, capacity, and pools](#scheduling-capacity-and-pools)).

## Scheduling, capacity, and pools

Every registered node (plus the implicit `local`) belongs to a **pool** —
nodes registered without one, and `local`, live in `"default"`.

```bash
lwd node add web1 deploy@web1.example.com 100.64.0.2 --pool web
lwd node add web2 deploy@web2.example.com 100.64.0.3 --pool web
lwd pool ls
```

**Placing an app:**

- **`node` unset** — the daemon schedules it: at apply time, it gathers
  every reachable node in the app's `pool` (`"default"` if unset — always
  includes `local`) along with each one's live capacity, picks the node
  with the most free memory (ties broken by free CPU, then name), and
  deploys there. This is a **one-time placement decision** made at `apply`
  — redeploying the same app schedules it again from scratch, and may land
  somewhere different if capacity shifted. Once placed, a scheduled app's
  surface only moves later via drain/evacuate or automatic failover, never
  a background rebalancer.
- **`node = "local"` or `node = "<name>"`** — pins the app; the scheduler is
  never consulted.
- **`pool = "<name>"`** narrows which pool an unpinned app schedules into.
- **`[requirements]`** excludes any candidate node without that much free
  `cpu`/`memory`. A node whose live capacity couldn't be measured is
  **optimistically assumed to fit** rather than excluded, so a flaky probe
  never turns into a failed deploy.

**Single-node stays exactly as before:** with no other nodes registered,
every pool but `default` is empty, `local` is the only candidate, and an
unpinned app always lands there.

Inspect placement and capacity with `lwd node capacity` and `lwd node
inspect <name>` — see the [CLI reference](#nodes) for sample output. `node
inspect` derives "what's running here" from every app's most recently
recorded deployment spec — there is no separate placement table.

## Replicas & load balancing

An `image` or `[git]`-built app can run as **N load-balanced replicas**
instead of one container:

```toml
replicas = 3
```

or scale a running app live, without touching `lwd.toml`:

```bash
lwd scale blog 3     # scale up
lwd scale blog 1     # scale back down
```

`lwd scale` (and the equivalent `POST /apps/{name}/scale`, `lwd-web`'s scale
control, and `lwd-mcp`'s `lwd_scale` tool) reuses the app's current recorded
spec snapshot — same as `lwd rollback` — and only changes `Replicas`, so
scaling never accidentally picks up an unrelated `lwd.toml` edit sitting on
disk.

- **Round-robin + passive health.** Caddy load-balances across every
  replica with round-robin, passively ejecting one that starts failing
  (`fail_duration`) until it recovers — no separate polling loop on the
  router's side. `replicas = 1` (the default) generates a **byte-identical**
  Caddyfile to every earlier phase — it is not internally a "1-wide pool."
- **Spread placement.** An unpinned multi-replica app is scheduled one
  replica per node, ranked by the same most-free-capacity logic as a
  single-replica app — falling back to sharing nodes if there aren't enough
  distinct ones. A **pinned** multi-replica app puts every replica on that
  one node.
- **Set-based blue-green.** A fresh set of N replicas is started and
  health-gated (same layered policy) before the live route flips to it; if
  even one new replica fails health, the whole generation is rolled back —
  nothing new goes live. Only once every replica in the new set is healthy
  are the old set's replicas retired, each removed on its own node.
- **Per-replica self-heal and failover.** The reconciler heals a dead
  replica individually, in place, on its own node — siblings are untouched.
  The same granularity applies to automatic node-loss failover and manual
  drain/evacuate: only the replicas on the affected node move.
- **Not supported with `compose` apps or `[[services]]`** (backing services
  run pinned on a single node's network, so a multi-node replica set
  couldn't reach it from every node).
- **Human-scaling only** — there is no autoscaler, no target-CPU/RPS policy,
  and no continuous rebalancing of a healthy multi-replica set.

## Node maintenance & failover

**Only scheduler-placed ("scheduled") surfaces are ever moved.** A surface
is "scheduled" if it landed on its node because `node` was left unset — its
concrete node is recorded in the deployment's spec snapshot
(`store.Deployment.Scheduled`). Everything else is left exactly where it is:
an explicitly `node = "..."`-pinned app, a `compose` app, and any backing
service.

### Operator-initiated: drain, evacuate, uncordon

```bash
lwd node drain web1      # cordon web1, then move its scheduled surfaces off it
lwd node evacuate web1   # move its scheduled surfaces off it, WITHOUT cordoning
lwd node uncordon web1   # clear the cordon
```

- **`drain`** = cordon (excludes the node from future scheduler placement)
  **then** evacuate: every scheduled surface currently on the node is moved,
  via the same blue-green path a self-heal uses, onto whichever other
  reachable, schedulable, in-pool node ranks highest.
- **`evacuate`** does the move without cordoning — the node stays eligible
  for new placements.
- **`uncordon`** clears the cordon; it never touches anything already
  running.

Every one of these returns an `EvacuateResult`: which apps **moved**, which
were **skipped** (pinned), and which **failed** to move (with why — usually
"no other node has room"). A failed move leaves the original surface running
untouched.

### Automatic failover on node loss

The continuous reconciler tracks how long each registered node has been
continuously unreachable. Once that streak exceeds `LWD_FAILOVER_GRACE`
(default `60s`), the next reconcile pass automatically evacuates every
scheduled surface on that node — exactly as a manual drain would, minus the
cordon (a node that comes back is immediately eligible again; nothing
re-populates it automatically).

- A **pinned** surface on a lost node is left running as-is (reported
  `degraded`) and is **never** moved by automatic failover.
- The old container is removed from the dead node only if it's still
  reachable enough to ask; for an actually-gone node, cleanup is skipped and
  the old deployment row is simply marked retired.
- **Single-node installs have nothing to fail over** — behavior is
  unchanged from every earlier phase.
- This is loss-triggered reschedule, not a background rebalancer: a healthy
  fleet is never touched, and a node that comes back stays empty until you
  place something on it again.
- **Controller-partition case:** if the controller itself loses mesh
  connectivity to its nodes, every registered node reads unreachable after
  the grace period, and each one's scheduled `default`-pool surfaces are
  evacuated onto the local node, bounded by local capacity (surfaces that
  don't fit are left reported-degraded). When the partition heals, the
  original remote containers are orphaned; a future reconcile pass or `lwd
  node rm` cleans up their now-retired rows. Multi-edge routing (P13) is the
  long-term fix for a single controller/edge being the partition point.

## Self-healing & health

The daemon runs a **continuous reconciler loop**, entirely off the request
path: `lwd apply`/`rollback` return as soon as their own deploy finishes,
and the loop runs on its own schedule — one pass immediately at daemon
startup, then one per `LWD_RECONCILE_INTERVAL` tick (default `15s`), plus a
non-blocking nudge right after every successful `apply`/`rollback`. Each
pass recovers its own panics — a bad reconcile never takes down the daemon.

**What gets healed:** single-service surfaces (`image`/`[git]` apps, with or
without `[[services]]`) that have crashed, exited, or otherwise gone
missing. The reconciler recreates them through the exact blue-green flow
`apply` itself uses (a git app's already-built image tag is reused, never
rebuilt). Repeated failures back off exponentially (15s, 30s, 1m, ...) and
give up after `LWD_HEAL_MAX_ATTEMPTS` (default `5`) attempts, at which point
the app is reported `failed` and left alone.

A container that is **running but Docker `HEALTHCHECK`-unhealthy is NOT
detected or healed** by this loop — it's reported `healthy`, same as an
actually-healthy container.

**What does not get healed:** `compose` apps (their lifecycle belongs to
`docker compose`, not lwd's surface model — no entry appears for them in
the health snapshot); the edge (Caddy reachability is observed and
reported, never acted on — there's only one edge to route through today).
**Node reachability is observed and, past a grace period, acted on** — see
[Node maintenance & failover](#node-maintenance--failover).

```bash
lwd health
```

reads the same snapshot the daemon exposes at `GET /health` (no secret
value ever appears in it), which `lwd-web`'s **Health** panel also renders
live. `state` is one of `healthy`, `degraded` (observed unhealthy, not yet
healing/backing off), `healing` (a heal attempt in flight), or `failed`
(gave up after `LWD_HEAL_MAX_ATTEMPTS`).

## Web UI (lwd-web)

`lwd-web` is a separate dashboard binary — a "self-hosted Vercel" front end
for lwd. It is just another client of the daemon's existing unix-socket API
(the same one the CLI uses): it makes **zero changes to the daemon** and can
do nothing the daemon API doesn't already permit.

```bash
CGO_ENABLED=0 go build -o lwd-web ./cmd/lwd-web
LWD_WEB_PASSWORD=changeme ./lwd-web
```

- `LWD_WEB_PASSWORD` (required) — the dashboard's admin password; `lwd-web`
  refuses to start without it.
- `LWD_WEB_ADDR` (default `127.0.0.1:8079`) — listen address.
- `LWD_WEB_SECRET` (optional) — the cookie-signing key; **must be at least
  16 bytes** if set (`lwd-web` refuses to start with a shorter one). If
  unset, a random 32-byte key is generated at startup and sessions reset on
  restart.
- `LWD_SOCKET` (optional) — overrides the daemon unix-socket path lwd-web
  connects to; defaults to `LWD_DATA_DIR` (default `/var/lib/lwd`) +
  `lwd.sock`, same resolution the daemon itself uses. (This is now shared
  client behavior via `client.FromEnv`: the `lwd` CLI and `lwd-mcp` honor
  `LWD_SOCKET` too. It is consulted only when `LWD_DAEMON` is unset — a TCP
  daemon takes precedence.)

**Auth:** a single shared admin password gates the whole dashboard.
`POST /login` checks it with a constant-time compare and sets an
`HttpOnly`, `SameSite=Lax` signed session cookie — `Secure` when served over
TLS directly, or behind a TLS-terminating proxy that sets
`X-Forwarded-Proto: https`. Sessions expire after 24 hours. There is no
multi-user/role model — this is a single-operator tool, same as the daemon.

**Exposing it safely:** `lwd-web` binds `127.0.0.1:8079` by default with no
built-in TLS — don't expose it directly to the internet. Either SSH-tunnel
to it (`ssh -L 8079:localhost:8079 you@host`) or front it with lwd's own
Caddy, the same way you'd front any other app.

**Features:** an **Overview** of every app (status, image, health, replica
badge) with a **Deploy** action; a Deploy modal with **From Git** / **Builder**
/ **Paste** tabs (each with a live `lwd.toml` preview, backing-service
builder, and Node/Pool/CPU/Memory/Replicas fields); a **Replicas** tab per
app (live count, scale control, per-replica node/container/upstream table);
**live logs** over SSE; **history + rollback**; **secrets** (set/list/delete,
never values); **redeploy** and **config edit**; a **Nodes** view
(list/add/remove/drain/evacuate/uncordon, live transport + reachability +
pool + schedulable badge); and a **Health** view (the reconciler's live
snapshot, auto-refreshing).

The From Git/Builder tabs' backing-service builder includes a **preset
picker**: pick PostgreSQL, MySQL/MariaDB, Redis, Valkey, MinIO, MongoDB, or
Custom to prefill a still-editable service row with a sensible image,
volume, and env. Any password-type env a preset needs is added to that
row's secrets list rather than as plaintext env — set its actual value in
the **Secrets** tab.

## Remote access & running lwd-web as a service

Everything above assumes `lwd-web` and `lwd daemon` on the same box, talking
over the local unix socket. This section covers exposing `lwd-web` beyond
`127.0.0.1`, giving the daemon its own TCP listener for a remote/tunneled
client, and running both as systemd services.

**Public `lwd-web`.** Setting `LWD_WEB_ADDR=0.0.0.0:8079` (or any
non-loopback address) makes the dashboard reachable from other machines —
but `lwd-web` serves **plain HTTP** with no built-in TLS, so put it behind a
TLS-terminating proxy (e.g. Caddy) or an SSH tunnel before exposing it
publicly. The session cookie is only marked `Secure` when the proxy sets
`X-Forwarded-Proto: https`; over a bare non-loopback HTTP bind it isn't.
`lwd-web` logs a startup warning whenever it's bound to a non-loopback
address, as a reminder.

**The daemon's TCP endpoint.** By default `lwd daemon` listens **only** on
its local unix socket (`0600`, filesystem-permission-gated, no auth needed).
Setting `LWD_ADDR` (e.g. `127.0.0.1:8077`, or an address on a private/
WireGuard-mesh interface) makes it *additionally* listen on TCP. Because the
daemon's API is full control (deploy, secrets, node management — everything),
this is fail-closed: a **non-loopback** `LWD_ADDR` requires `LWD_API_TOKEN`
to be set, or the daemon refuses to start at all
(`LWD_ADDR "..." binds a non-loopback interface but LWD_API_TOKEN is
unset — refusing to expose an unauthenticated control plane`). A
**loopback** `LWD_ADDR` (`127.0.0.1`, `::1`, or `localhost`) may run
token-less, since it's no more exposed than the unix socket — useful for an
SSH tunnel. Requests to the TCP listener authenticate with a bearer token
(`Authorization: Bearer <LWD_API_TOKEN>`), checked in constant time; the
unix socket itself is never token-gated. Recommended topology: bind
`LWD_ADDR` to loopback or a private mesh address only, and reach it through
a password-protected `lwd-web` behind TLS or through a tunnel — never bind
the daemon's TCP listener to a public interface, token or not.

**Connecting a remote or tunneled client.** `lwd-web`, the `lwd` CLI, and
`lwd-mcp` all resolve their daemon connection the same way
(`client.FromEnv`): set `LWD_DAEMON=<host:port>` (a bare `host:port`, or a
full `http://`/`https://` URL) plus `LWD_API_TOKEN` matching the daemon's,
and they'll dial that TCP endpoint instead of the local socket. Leave
`LWD_DAEMON` unset and nothing changes — the local unix socket is still the
default for all three.

**SSH-tunnel example.** If `lwd daemon` and `lwd-web` both run on the
server and you just want the dashboard on your laptop:

```bash
ssh -L 8079:127.0.0.1:8079 you@server
# then open http://127.0.0.1:8079 in a local browser
```

`lwd-web` on the server still talks to the daemon over the local unix
socket — no `LWD_DAEMON` needed on either end. If instead you want to run
`lwd-web` itself on your laptop against a daemon on a remote server, tunnel
the daemon's TCP port instead and point `lwd-web` at it:

```bash
ssh -L 8077:127.0.0.1:8077 you@server   # server: LWD_ADDR=127.0.0.1:8077
LWD_DAEMON=127.0.0.1:8077 LWD_API_TOKEN=... LWD_WEB_PASSWORD=... ./lwd-web
```

**Running as systemd services.** `sudo ./install.sh --web` installs and
enables `lwd-web.service`, reading its environment from `/etc/lwd/web.env`
(`0600`, root-owned). Set `LWD_WEB_PASSWORD` there — the installer writes
a `CHANGE_ME` placeholder and deliberately does **not** start the service
while that placeholder is still in place. `sudo ./install.sh --agent` does
the same for `lwd-agent.service` + `/etc/lwd/agent.env`
(`LWD_AGENT_TOKEN`). Add `LWD_ADDR`/`LWD_API_TOKEN`/`LWD_DAEMON` to the
relevant env file the same way if you want a service using this remote-access
setup.

**Dogfooding note.** It's possible to deploy `lwd-web` itself as an app
under `lwd` (as a surface pointed at the daemon via `LWD_DAEMON`), but
running it under systemd instead is recommended — deploying the dashboard
through the very daemon it depends on creates a restart/bootstrap ordering
paradox (redeploying `lwd-web` can race with, or depend on, the daemon
being up).

## Agent access (lwd-mcp)

`lwd-mcp` is a local [Model Context Protocol](https://modelcontextprotocol.io)
server that lets a coding agent (Claude Code, or any other MCP host) drive
lwd directly. Like `lwd-web`, it's just another client of the daemon's
unix-socket API — zero daemon changes, nothing beyond what the daemon API
already permits. It speaks MCP over **stdio only**: no network listener, no
auth of its own (the daemon socket is `0600`), and it requires `lwd daemon`
to already be running.

```bash
CGO_ENABLED=0 go build -o lwd-mcp ./cmd/lwd-mcp
```

Register it with an MCP host — a Claude Code-style `.mcp.json` entry:

```json
{
  "mcpServers": {
    "lwd": {
      "command": "/path/to/lwd-mcp",
      "args": [],
      "env": { "LWD_DATA_DIR": "/var/lib/lwd" }
    }
  }
}
```

### Tools

All 18 tools are stable, plain JSON in and out — no secret value is ever
returned by any tool:

| Tool | Description |
| --- | --- |
| `lwd_list` | List all apps with current status, image, domain, and replica count. |
| `lwd_status` | Status + deployment history for one app. |
| `lwd_logs` | Recent logs (`tail`-limited, default 200 lines). |
| `lwd_history` | Recorded deployments (image, status, time). |
| `lwd_apply` | Deploy from an `lwd.toml`, given inline (`toml`) or a local directory (`dir`); optional `node`/`pool`/`requirements`/`replicas` override the toml. |
| `lwd_deploy_git` | Deploy from a git repo, from discrete fields (url/ref/dockerfile/name/domain/port/services), without hand-authoring an `lwd.toml`. |
| `lwd_rollback` | Roll back to the previous deployment. |
| `lwd_scale` | Change replica count. |
| `lwd_remove` | Permanently stop and remove an app. |
| `lwd_secret_set` | Set (or overwrite) a secret. Never echoed back. |
| `lwd_secret_list` | List secret names (never values). |
| `lwd_secret_delete` | Delete a secret. |
| `lwd_node_list` | List registered nodes: ssh host, mesh address, agent URL, pool, schedulable, transport, reachability, capacity. |
| `lwd_node_add` | Register (or update) a node. |
| `lwd_node_remove` | Deregister a node. |
| `lwd_node_drain` | Cordon a node, then move its scheduler-placed surfaces off it. |
| `lwd_node_evacuate` | Move a node's scheduler-placed surfaces off it, without cordoning. |
| `lwd_node_uncordon` | Clear a node's cordon. |

`lwd_list`, `lwd_status`, `lwd_logs`, `lwd_history`, `lwd_secret_list`, and
`lwd_node_list` are annotated `readOnlyHint: true`; `lwd_remove`,
`lwd_secret_delete`, `lwd_node_remove`, `lwd_node_drain`, `lwd_node_evacuate`,
and `lwd_scale` are annotated `destructiveHint: true`. `lwd_node_uncordon` is
deliberately **not** destructive — it only lifts a placement restriction.
`lwd-mcp` asks nothing before calling the daemon; it relies entirely on the
MCP host's own per-call approval UI to gate destructive tools.

## Authoring lwd.toml (Claude skill)

`skills/lwd-toml/` is a [Claude Code](https://claude.com/claude-code) skill
that writes an `lwd.toml` for you: point it at a project directory and it
inspects for a `Dockerfile`/`docker-compose.yml`, detects the
language/framework and listening port, spots backing-service dependencies,
picks the right app shape, and writes a valid `lwd.toml` — asking only for
what it can't infer (the `domain`, and confirmation of port/backing
services). It never invents secret values, only names.

```bash
cp -r skills/lwd-toml ~/.claude/skills/lwd-toml
# or: ln -s "$(pwd)/skills/lwd-toml" ~/.claude/skills/lwd-toml
```

Then ask Claude Code to "make an lwd.toml for this project" from within the
target project. The skill's worked examples
(`skills/lwd-toml/references/schema.md`, `detect.md`) are kept in sync with
`internal/spec`'s actual validation rules.

## Architecture & networking model

```
Linux → Docker → WireGuard mesh → Caddy (edge) → lwd-agent → lwd Controller → Applications
```

- The **controller** (the `lwd daemon` process) owns desired state — the
  SQLite store, the reconciler, the router manager, secrets — and is **not
  on the request path**: if it crashes, already-deployed apps keep serving;
  new deploys and reconcile passes just pause.
- lwd creates and manages one private Docker network, `lwd`, **on every
  node it deploys to**. Every local app container, every remote app
  container (on its own node), and the `lwd-caddy` container (on the
  controller) join `lwd` on their respective host.
- A **local** app container publishes **no host ports** — Caddy reaches it
  by container name and port on the `lwd` network. This is why `port` in
  `lwd.toml` is just the container port, not something you reserve on the
  host.
- A **remote** app container publishes an ephemeral host port bound to its
  node's mesh address, and the controller's Caddy upstream becomes
  `<mesh-addr>:<published-port>`.
- Only `lwd-caddy`, on the controller, binds host ports 80 and 443 for
  traffic, plus 2019 (loopback-only) for its admin API, which lwd uses to
  push routing config. A registered node runs no lwd-managed container of
  its own besides the app surfaces (and their backing services) placed on
  it.

The daemon exposes a small HTTP API over its unix socket (`internal/api`),
which every client — the CLI, `lwd-web`, `lwd-mcp` — talks to identically:

| Method & path | Purpose |
| --- | --- |
| `POST /apply` | Deploy an app from a resolved spec. |
| `GET /apps` | List apps + status. |
| `GET /apps/{name}/logs` | Fetch/stream logs. |
| `GET /apps/{name}/history` | Deployment history. |
| `POST /apps/{name}/rollback` | Roll back. |
| `POST /apps/{name}/scale` | Change replica count. |
| `DELETE /apps/{name}` | Remove an app. |
| `POST /apps/{name}/secrets` | Set a secret. |
| `GET /apps/{name}/secrets` | List secret names. |
| `DELETE /apps/{name}/secrets/{key}` | Delete a secret. |
| `POST /nodes` | Register a node. |
| `GET /nodes` | List nodes. |
| `DELETE /nodes/{name}` | Deregister a node. |
| `POST /nodes/{name}/drain` | Drain a node. |
| `POST /nodes/{name}/evacuate` | Evacuate a node. |
| `POST /nodes/{name}/uncordon` | Uncordon a node. |
| `GET /pools` | List pools. |
| `GET /health` | The reconciler's live health snapshot. |

## Security model

- The daemon's unix socket is created with **`0600`** permissions — only the
  user that started `lwd daemon` (or root) can talk to it. Every client
  (CLI, `lwd-web`, `lwd-mcp`) inherits its access purely from filesystem
  permission on that socket.
- Secrets are encrypted at rest with **AES-256-GCM**, key at
  `<data_dir>/secret.key` (**`0600`**). This is a data-at-rest control: it
  protects a leaked backup or disk image, **not** against an attacker with
  root (or the daemon's own user) on the live host — anyone who can read the
  data directory can decrypt every secret.
- A registered `lwd-agent`'s bearer token is **effectively root on that
  node** — see [The lwd-agent transport](#the-lwd-agent-transport). Bind it
  to the mesh interface only, never a public one.
- A remote surface's mesh-address traffic between the controller's Caddy and
  a node (and the controller's traffic to a registered `lwd-agent`) is
  **plain HTTP** — lwd relies entirely on the mesh (e.g. WireGuard) for
  transport encryption; it adds none of its own on top.
- **Known limitation:** all app containers share the `lwd` Docker network
  with the Caddy container, whose admin API is reachable on that network.
  lwd assumes all deployed apps are trusted (single-operator use);
  isolating the router admin API from app containers is a later hardening
  step.
- **Known limitation:** every domain is served over both HTTP and HTTPS
  with **no automatic HTTP→HTTPS redirect**. Public domains still get
  Let's Encrypt certificates, but plaintext HTTP is not upgraded.
- `lwd-web`'s session cookie is `HttpOnly`/`SameSite=Lax`, and `Secure` when
  served over TLS (directly, or behind a proxy setting
  `X-Forwarded-Proto: https`) — see [Web UI](#web-ui-lwd-web).

## Configuration reference

All env vars are read directly by the process they affect — most have a
default and are optional.

| Variable | Default | Read by | Meaning |
| --- | --- | --- | --- |
| `LWD_DATA_DIR` | `/var/lib/lwd` | `lwd` (daemon + CLI), `lwd-mcp` | Root directory for the unix socket (`lwd.sock`), SQLite DB (`lwd.db`), generated Caddyfile, and the secret-encryption key (`secret.key`). |
| `LWD_ADDR` | — (unix socket only) | `lwd daemon` | Additional TCP listen address for the daemon's API (e.g. `127.0.0.1:8077`, or a private/mesh IP). A **non-loopback** value requires `LWD_API_TOKEN` — the daemon refuses to start otherwise (fail-closed); a loopback value may run token-less. See [Remote access](#remote-access--running-lwd-web-as-a-service). |
| `LWD_API_TOKEN` | — | `lwd daemon` (bearer required by the `LWD_ADDR` TCP listener), any client (`lwd` CLI, `lwd-web`, `lwd-mcp`) connecting via `LWD_DAEMON` | Bearer token for the daemon's TCP listener, checked in constant time. Never consulted for the unix socket. |
| `LWD_DAEMON` | — (unix socket) | `lwd` (CLI), `lwd-web`, `lwd-mcp` (via `client.FromEnv`) | Remote daemon connect target (`host:port` or `http(s)://host:port`); pairs with `LWD_API_TOKEN`. Unset means dial the local unix socket, unchanged from before this feature. |
| `LWD_AGENT_TOKEN` | — | `lwd daemon` (as the client credential), `lwd-agent` (required to start) | Shared bearer token authenticating controller ↔ `lwd-agent` traffic. |
| `LWD_AGENT_ADDR` | `:8078` | `lwd-agent` | Listen address for the agent's HTTP API — bind to the mesh interface. |
| `LWD_WEB_PASSWORD` | — | `lwd-web` (required to start) | The dashboard's single admin password. |
| `LWD_WEB_ADDR` | `127.0.0.1:8079` | `lwd-web` | Listen address for the dashboard. A non-loopback value exposes it to the network over **plain HTTP** — put it behind TLS or a tunnel; see [Remote access](#remote-access--running-lwd-web-as-a-service). |
| `LWD_WEB_SECRET` | random (regenerated per process) | `lwd-web` | Cookie-signing key; must be at least 16 bytes if set. |
| `LWD_SOCKET` | (falls back to `LWD_DATA_DIR`/`lwd.sock`) | `lwd` CLI, `lwd-mcp`, `lwd-web` | Overrides the daemon unix-socket path these clients dial. Consulted only when `LWD_DAEMON` is unset (a TCP daemon takes precedence); default behavior is unchanged when both are unset. |
| `LWD_RECONCILE_INTERVAL` | `15s` | `lwd daemon` | Delay between continuous-reconciler passes (`time.ParseDuration`; a zero/negative/unparseable value falls back to the default). |
| `LWD_HEAL_MAX_ATTEMPTS` | `5` | `lwd daemon` | Consecutive self-heal attempts on a dead surface before it's reported `failed`. |
| `LWD_FAILOVER_GRACE` | `60s` | `lwd daemon` | How long a registered node must be continuously unreachable before its scheduled surfaces are automatically evacuated. |

## Roadmap

- **P1–P8 — done:** core deploy; HTTPS/blue-green/rollback; secrets; compose
  apps; web UI; git deploy + build-from-source + backing services; the
  lwd.toml authoring skill; local MCP.
- **P9a — done:** node registry + docker-over-ssh transport + `docker
  save|load` image movement + explicit `node =` placement + WireGuard-mesh
  routing.
- **P9b — done:** the dumb `lwd-agent` binary as a preferred transport over
  docker-over-ssh, with automatic fallback; node UX in the CLI/web/MCP.
- **P10 — done:** the continuous reconciler loop; self-heals dead surfaces;
  observes (but doesn't yet act on) node/edge reachability.
- **P11a — done:** capacity-aware scheduler, node pools, `[requirements]`.
- **P11b — done:** node drain/evacuate/uncordon; automatic node-loss
  failover.
- **P12 — done:** surface replicas + Caddy round-robin load balancing;
  `lwd scale`.
- **P13 — planned:** multi-edge routing — N Caddy edges, each fed identical
  controller-pushed route config, with DNS round-robin across edge IPs.
- **P14 — planned:** first-class resource drivers (postgres/valkey/minio)
  in single-mode (one node, local disk, backups), evolving today's
  generated-compose backing services.
- **P15 — planned:** resource HA — Patroni-based Postgres failover, plus
  scheduled backups and restore.

Single-node stays a first-class, zero-mesh path throughout — the one-box
experience must never regress as federation lands. See `docs/VISION.md` for
the full philosophy and design principles behind this ordering.

## Development & testing

```bash
go test ./...                          # unit + integration tests — no Docker needed
LWD_DOCKER_TEST=1 go test ./test/ -v   # + real end-to-end tests against Docker
```

The plain `go test ./...` run (no build tag, no Docker) already covers a lot
end-to-end against fakes: `internal/web`'s `TestIntegrationWebClientDaemon`
drives the browser → `lwd-web` → daemon chain over real HTTP; `internal/mcp`'s
`TestIntegrationMCPClientDaemon` drives the same for `lwd-mcp` over MCP's
in-memory transport; and dedicated fake-backed tests exercise agent
transport selection, self-heal, scheduling, node drain/failover, and
replicas — all without a Docker daemon.

The Docker-gated suite (`LWD_DOCKER_TEST=1 go test ./test/ -v`) drives the
full stack against real Docker: blue-green + rollback, secret injection,
a real compose app, a real git build, a remote node over `ssh://localhost`,
and a real `lwd-agent` deploy. Each test `t.Skip`s (rather than fails) with
a clear message when its prerequisite isn't available in the environment
(no `docker compose` plugin, no `git`, no passwordless ssh to `localhost`,
ports 80/443 already in use) — it never fakes a pass.

This codebase was built with a subagent-driven, phase-by-phase development
process (see `docs/VISION.md` for the phase roadmap this README documents);
each phase's implementation, tests, and documentation update landed together.

## License

No license file is currently published in this repository (TBD).
