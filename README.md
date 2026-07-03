# lwd — lightweight deploy

A suckless, self-hosted deployment engine for Docker apps. Point it at an app,
deploy with one command, get automatic HTTPS and zero-downtime rollouts for
free. Single static Go binary that is both the daemon and the CLI.

> This is the **router + blue-green + secrets** milestone. Compose apps,
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
- `secrets` (declare names in `lwd.toml`, set values via `lwd secret set`)
  is fully live: values are encrypted at rest and injected into the
  container environment at deploy time, fail-closed on any unset name.
- `compose`, `[build]`, and `surfaces` in `lwd.toml` are parsed but rejected
  with a clear error until their milestones land.

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

The end-to-end suite drives the full stack — a real Docker daemon, a real
`lwd-caddy` container, and real `traefik/whoami` deployments — against real
Docker:

- `TestEndToEndBlueGreenRollback` runs two blue-green deploys and a rollback,
  asserting zero downtime across the swap.
- `TestEndToEndSecretInjection` wires a real `secrets.Store` into the
  reconciler, sets a secret, deploys an app declaring it, and asserts (via
  `docker inspect`, not the app's HTTP response) that the value reached the
  container's environment — then asserts a deploy declaring an unset secret
  fails closed with no container left running.

Both tests clean up every container and network they create, and will
`t.Skip` (rather than fail) if ports 80/443 are already in use on the host.
