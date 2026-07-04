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
| `node` | string | all | defaults to `"local"` |
| `env` | map[string]string | all | non-secret config, passed as container/compose env |
| `secrets` | []string | app + each `[[services]]` entry | **names only, never values**; must match `^[A-Za-z_][A-Za-z0-9_]*$` |
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
