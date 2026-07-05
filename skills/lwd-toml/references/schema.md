# lwd.toml schema and validation rules

Authoritative source: `internal/spec/spec.go` (`App`, `Git`, `Build`,
`Service` structs and `(*App).Validate()`) in the `lwd` repo. This file
mirrors that code; if they ever disagree, the Go source wins.

## Fields

| Field | Type | Applies to | Notes |
|---|---|---|---|
| `name` | string | all | required; DNS-safe: `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$` |
| `image` | string | image apps | prebuilt image ref; mutually exclusive with `[git]`/`compose` |
| `domain` | string | image, git, compose apps | required for those three shapes; live routing + TLS via Caddy |
| `port` | int | image, git, compose apps | required for those three shapes; the **container** port, not a host port |
| `node` | string | all | **unset** (omit the field, or `""`) means "let lwd schedule it" — the daemon picks the node with the most free capacity in `pool` at deploy time; `"local"` pins it to the controller; any other value pins it to that registered node name (see `lwd node ls`). Unset does **not** default to `"local"` |
| `pool` | string | all | the node pool to schedule into when `node` is unset; defaults to `"default"` (the pool the implicit `local` node and every node registered without `--pool` live in). Ignored (but still validated) when `node` is pinned. Must match `^[A-Za-z0-9][A-Za-z0-9_-]*$` |
| `env` | map[string]string | all | non-secret config, passed as container/compose env |
| `secrets` | []string | app + each `[[services]]` entry | **names only, never values**; must match `^[A-Za-z_][A-Za-z0-9_]*$` |
| `[requirements]` | table | all | resource needs the scheduler uses when `node` is unset: `cpu` (float, cores, e.g. `0.5`) and `memory` (size string, e.g. `"512M"`, `"2G"` — binary units, K/M/G/T or Ki/Mi/Gi/Ti). Either or both may be set; omit the whole table for no requirements. A node whose live capacity can't be measured is optimistically assumed to fit |
| `[health]` | table | all | `path` (string) and `timeout` (Go duration string, e.g. `"30s"`, default `30s`) |
| `[git]` | table | git apps | `url` (required), `ref` (default `"main"`), `path` (subdir, optional) |
| `[build]` | table | git apps only | `context`, `dockerfile` — **required** alongside `[git]`; rejected on every other shape |
| `compose` | string | compose apps | path to a `docker-compose.yml`, relative to the app dir or absolute |
| `service` | string | compose apps | the compose service Caddy fronts; required when `compose` is set |
| `[[services]]` | array of tables | image or git apps only | pinned backing services: `name`, `image`, `command` (optional), `env`, `secrets`, `volume` (`name:path`) |
| `surfaces` | []string | — | **parsed but always rejected** ("surfaces are not supported yet") — never emit this field |

## Shape rules (mutually exclusive)

Exactly one of these three shapes per app:

1. **Git app** — `[git]` set:
   - `git.url` required, non-empty.
   - `[build]` required (git apps always build from a Dockerfile).
   - `image` and `compose` must be unset (mixing either is rejected).
   - `domain` and `port` required.
2. **Compose app** — `compose` set (non-empty):
   - `service` required.
   - `domain` and `port` required.
   - `image` and `[build]` must be unset.
   - `[[services]]` is **rejected** on compose apps (the compose file already
     defines its own full stack).
3. **Image (single-service) app** — neither of the above:
   - `image` required, non-empty.
   - `port` required.
   - `[build]` is **rejected** ("build-from-source is not supported yet" —
     only reachable together with `[git]`).

## Validation rules (exact regexes / constraints from `spec.go`)

- `name`: required; `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`.
- `secrets` (top-level and per-`[[services]]`): each name must match
  `^[A-Za-z_][A-Za-z0-9_]*$` (valid env-var identifier — secrets are injected
  as container env vars, and for backing services also spliced into an
  unescapable `${NAME}` compose-interpolation reference).
- `git.url`:
  - must not start with `-` (option-injection guard).
  - if it has a `scheme://` prefix, the scheme must be one of `http`,
    `https`, `git`, `ssh`, `file` (case-insensitive) — `ext::`, `fd::`, and
    other command-executing transports are rejected outright.
  - otherwise, must be scp-like ssh syntax (`user@host:path`, e.g.
    `git@github.com:me/app.git`).
  - bare local filesystem paths are rejected (use `file://` instead).
- `git.ref`: `^[A-Za-z0-9][A-Za-z0-9._/-]*$` (no leading `-` or `.`, no
  whitespace); empty is allowed and defaults to `"main"`.
- `git.path`, `build.context`, `build.dockerfile`: each must be a relative
  path (not absolute) with no `..` path segment (no escaping the clone
  root); empty is allowed.
- `[[services]].name`: required; `^[a-z0-9][a-z0-9-]*$`; must be unique
  within the app.
- `[[services]].image`: required, non-empty.
- `surfaces`: any non-empty value is rejected for every shape — never emit
  this field.
- `pool`: `^[A-Za-z0-9][A-Za-z0-9_-]*$` when set; empty (unset) is allowed
  and means `"default"`.
- `requirements.cpu`: must not be negative; `0` (or omitting `cpu`) means no
  CPU requirement.
- `requirements.memory`: must parse as a size (see the `pool`/`[requirements]`
  row above); empty (or omitting `memory`) means no memory requirement.

## Worked examples

Each example below is a **complete, valid** `lwd.toml`. All three are
round-trip tested against the real `spec.Parse` + `Validate` by
`internal/spec/examples_test.go`, which reads the identical files from
`references/examples/` in this skill directory — so if the schema ever
changes underneath this document, that test fails and flags the drift.

### (a) Node app built from git, with a Postgres backing service

`references/examples/git-postgres.toml`:

```toml
name    = "api"
domain  = "api.example.com"
port    = 3000
env     = { NODE_ENV = "production" }
secrets = ["DATABASE_URL", "SESSION_SECRET"]

[git]
url = "https://github.com/example/api"
ref = "main"

[build]
dockerfile = "Dockerfile"

[health]
path    = "/healthz"
timeout = "30s"

[[services]]
name    = "db"
image   = "postgres:16"
env     = { POSTGRES_USER = "app", POSTGRES_DB = "app" }
secrets = ["POSTGRES_PASSWORD"]
volume  = "db-data:/var/lib/postgresql/data"
```

Notes: `DATABASE_URL` and `SESSION_SECRET` are the app's own secrets (set via
`lwd secret set api DATABASE_URL` etc. — the app builds its own connection
string from the secret value). `POSTGRES_PASSWORD` is the backing service's
own secret, matching the exact env var the official `postgres` image reads;
set it separately (`lwd secret set api POSTGRES_PASSWORD`) with the same
password embedded in the app's `DATABASE_URL`. `db-data` is a named volume,
so the database survives every redeploy of `api`.

### (b) Prebuilt-image app

`references/examples/image-app.toml`:

```toml
name    = "blog"
image   = "ghcr.io/me/blog:latest"
domain  = "blog.example.com"
port    = 8080
env     = { LOG_LEVEL = "info" }
secrets = ["API_KEY"]

[health]
path    = "/healthz"
timeout = "30s"
```

### (c) Compose app (the repo's own docker-compose.yml, run as-is)

`references/examples/compose-app.toml`:

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

Notes: `compose` is resolved relative to the app directory if not absolute.
`service` must name the compose service that serves HTTP on `port` — Caddy
fronts that one service. Any other services in the compose file (a database,
a worker) are brought up by `docker compose` itself and are **not** declared
as `[[services]]` (which is rejected on compose apps).

### (d) Scheduled image app (no pinned node, pool + resource requirements)

`references/examples/scheduled-app.toml`:

```toml
name    = "worker"
image   = "ghcr.io/me/worker:latest"
domain  = "worker.example.com"
port    = 8080

pool = "web"

[requirements]
cpu    = 1
memory = "1G"
```

Notes: `node` is **not set** — this is the key difference from every example
above (all of which either omit `node` entirely on a single-node deploy, or
pin it explicitly). On a fleet with more than one node registered in the
`web` pool (see `lwd node add ... --pool web`), the daemon places this app on
whichever node in that pool currently has the most free memory (then CPU) and
at least 1 CPU core and 1G of available memory free; on a single-node
install, or a pool with just `local` in it, it simply lands on `local` — the
same as every other example. Never write `node = "local"` and expect
scheduling to still apply: an explicit `node` (including `"local"`) always
bypasses the scheduler.
