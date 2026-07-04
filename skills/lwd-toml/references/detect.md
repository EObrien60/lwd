# Detection heuristics

Lookup tables for inferring language, port, health path, and backing
services from a project directory. All inferred values are **suggestions to
confirm with the user**, not final answers — never write them to `lwd.toml`
silently.

## Shape signal (check in this order)

1. `Dockerfile` present → candidate for `[git]` + `[build]` (build-from-source).
2. `docker-compose.yml` / `compose.yml` present, no separate deploy image
   intended → candidate for `compose` + `service`.
3. Neither, or the user says they'll build/push an image themselves →
   `image = "..."`.

If both a `Dockerfile` and a `docker-compose.yml` exist, ask which one the
user wants lwd to drive — a compose app ignores its own `Dockerfile` (compose
itself would build it), while a `[git]`+`[build]` app ignores the compose
file entirely.

## Port and health path, by manifest

Dockerfile `EXPOSE <port>` always wins over the table below when present.

| Manifest file | Language/framework signal | Default port | Default health path |
|---|---|---|---|
| `package.json` — generic Node | (no framework match below) | `3000` | `/` |
| `package.json` — `"next"` dep | Next.js | `3000` | `/` |
| `package.json` — `"express"` dep | Express | `3000` | `/` |
| `package.json` — `"fastify"` dep | Fastify | `3000` | `/health` |
| `go.mod` | Go | `8080` | `/health` or `/healthz` (check `main.go`/router for an actual route) |
| `requirements.txt` / `pyproject.toml` — `flask` | Flask | `5000` | `/` |
| `requirements.txt` / `pyproject.toml` — `django` | Django | `8000` | `/` |
| `requirements.txt` / `pyproject.toml` — `fastapi` + `uvicorn` | FastAPI | `8000` | `/docs` or `/health` |
| `Gemfile` — `rails` | Ruby on Rails | `3000` | `/up` (Rails 7.1+ default) or `/` |
| `Cargo.toml` | Rust (framework-dependent: actix-web, axum, rocket) | `8080` | `/` |

When no manifest matches, or the port can't be determined confidently, ask
the user directly rather than guessing.

## Backing-service signal, by dependency or compose service

| Signal | Suggested `[[services]]` |
|---|---|
| `pg`, `postgres` (npm); `psycopg2`, `asyncpg` (pip); `django.db.backends.postgresql`; `pg` gem; Go `lib/pq` or `jackc/pgx` | `image = "postgres:16"`, named volume `db-data:/var/lib/postgresql/data`, `env.POSTGRES_USER`/`POSTGRES_DB`, `secrets = ["POSTGRES_PASSWORD"]` |
| `mysql`, `mysql2` (npm); `PyMySQL`, `mysqlclient` (pip); `mysql2` gem; Go `go-sql-driver/mysql` | `image = "mysql:8"`, named volume `db-data:/var/lib/mysql`, `env.MYSQL_DATABASE`, `secrets = ["MYSQL_ROOT_PASSWORD"]` |
| `redis`, `ioredis` (npm); `redis` (pip); `redis` gem; Go `redis/go-redis` | `image = "redis:7"`, no volume needed unless persistence is requested |
| AWS SDK S3 client, `minio` client library, `boto3` used against an S3-compatible endpoint | `image = "minio/minio"`, `command = "server /data"`, named volume `minio-data:/data`, `env.MINIO_ROOT_USER`, `secrets = ["MINIO_ROOT_PASSWORD"]` |
| A `docker-compose.yml` in the project already defines a `postgres`/`mysql`/`redis`/`minio`/`mongo` service | Mirror that service's image/env/volume shape as a matching `[[services]]` entry (only relevant when the app itself is `[git]`/`image`-based, not when deploying that same compose file as-is via `compose =`) |

Always confirm the suggested image tag/version and whether the user wants
the service at all before adding it — don't assume every detected dependency
should become a pinned container (e.g. it may point at an external managed
database instead).
