# lwd — lightweight deploy

A suckless, self-hosted deployment engine for Docker apps. Point it at an app,
deploy with one command, get automatic HTTPS and zero-downtime rollouts for
free. Single static Go binary that is both the daemon and the CLI.

> This is the **router + blue-green** milestone. Secrets, compose apps,
> pinned surfaces, and the web UI arrive in later milestones.

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
lwd apply ./myapp     # zero-downtime blue-green deploy of ./myapp/lwd.toml
lwd ls                # list apps and status
lwd logs blog -f      # stream logs
lwd history blog      # show past deployments for an app
lwd rollback blog     # redeploy the previous version, itself zero-downtime
lwd rm blog           # stop and deregister
```

## How deploys work

Every `apply` is a blue-green swap, never an in-place recreate:

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
path used for every other deploy.

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

- Single host, pre-built images only.
- `domain` (routing + TLS) is fully live.
- `compose`, `[build]`, and `surfaces` in `lwd.toml` are parsed but rejected
  with a clear error until their milestones land.
- `secrets` in `lwd.toml` is parsed but **not yet injected** — do not rely on
  it yet.

### Known limitations (this milestone)

- Mutable image tags (e.g. `:latest`) are re-pulled on every `apply` when the
  registry is reachable; if the pull fails but the image exists locally, the
  local copy is used.
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

## Testing

```bash
go test ./...                              # unit tests (e2e SKIPs without Docker)
LWD_DOCKER_TEST=1 go test ./test/ -v       # + real end-to-end test against Docker
```

The end-to-end test drives the full stack — a real Docker daemon, a real
`lwd-caddy` container, and a real `traefik/whoami` deployment — through two
blue-green deploys and a rollback, asserting zero downtime across the swap. It
cleans up every container and network it creates, and will `t.Skip` (rather
than fail) if ports 80/443 are already in use on the host.
