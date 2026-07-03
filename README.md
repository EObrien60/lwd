# lwd — lightweight deploy

A suckless, self-hosted deployment engine for Docker apps. Point it at an app,
deploy with one command, list and inspect running apps. Single static Go binary
that is both the daemon and the CLI.

> This is the **core deploy** milestone. Routing/TLS, blue-green, rollback,
> secrets, compose apps, and the web UI arrive in later milestones.

## Build

```bash
CGO_ENABLED=0 go build -o lwd ./cmd/lwd
```

## Run the daemon

```bash
sudo LWD_DATA_DIR=/var/lib/lwd ./lwd daemon
```

The daemon listens on a unix socket at `$LWD_DATA_DIR/lwd.sock` (default
`/var/lib/lwd/lwd.sock`) and talks to the local Docker daemon.

## Define an app

Create `lwd.toml` in a directory:

```toml
name = "blog"
image = "ghcr.io/me/blog:latest"
port = 8080

[health]
path = "/healthz"
timeout = "30s"
```

## Deploy and inspect

```bash
lwd apply ./myapp     # deploy the app in ./myapp/lwd.toml
lwd ls                # list apps and status
lwd logs blog -f      # stream logs
lwd rm blog           # stop and deregister
```

## Scope of this milestone

- Single host, pre-built images only.
- Deploys are recreate (brief downtime); zero-downtime blue-green comes with the
  router milestone.
- `compose`, `[build]`, and `surfaces` in `lwd.toml` are parsed but rejected with
  a clear error until their milestones land.

### Known limitations (this milestone)

- Mutable image tags (e.g. `:latest`) are re-pulled on every `apply` when the
  registry is reachable; if the pull fails but the image exists locally, the
  local copy is used.
- The `domain` and `secrets` fields in `lwd.toml` are **parsed but not yet
  applied** in this milestone (routing/TLS and secret injection arrive in
  later milestones). Do not rely on `secrets` being injected yet.
- Building lwd requires **Go 1.25+** (a transitive dependency of the Docker
  SDK raises the floor above the 1.22 language baseline).

## Testing

```bash
go test ./...                              # unit tests
LWD_DOCKER_TEST=1 go test ./... -v         # + Docker integration/e2e tests
```
