---
name: lwd-toml
description: Use when authoring or generating an lwd.toml file, or when asked to deploy a project with lwd, containerize a repo for lwd, or set up lwd for an app. Triggers on requests like "make an lwd.toml", "write an lwd config", "deploy this with lwd", or "set up lwd for this project".
---

# lwd-toml

Authors a valid `lwd.toml` for a project by inspecting it, choosing the right
app shape, and asking only for what can't be inferred. The generated file
must satisfy `internal/spec`'s `Validate()` in the `lwd` repo — see
`references/schema.md` for the authoritative field list and every validation
rule, with three complete worked examples.

## Procedure

1. **Inspect the target directory.**
   - `Dockerfile` present? `docker-compose.yml` present?
   - Language/framework: `package.json`, `go.mod`, `requirements.txt` /
     `pyproject.toml`, `Gemfile`, `Cargo.toml`.
   - Listening port: a Dockerfile `EXPOSE` line wins; otherwise the
     framework's conventional default.
   - Backing-service hints: a postgres/mysql/redis/minio/S3 client library in
     the manifest, or a db/cache service in `docker-compose.yml`.
   - See `references/detect.md` for the file→language→port/health lookup
     table and the dependency→backing-service table.

2. **Choose the app shape:**

   | Situation | Shape |
   |---|---|
   | Project has a `Dockerfile`, lives in (or will be pushed to) a git repo | `[git]` (`url`, `ref`) + `[build]` (`dockerfile`) |
   | You'll build/push a pre-built image yourself | `image = "..."` |
   | Project has its own `docker-compose.yml` you want run as-is | `compose = "docker-compose.yml"` + `service = "<web service name>"` |

   These are mutually exclusive — never combine `image`, `[git]`, or
   `compose` on the same app.

3. **Infer and confirm:**
   - `port`: from `EXPOSE` or the framework default (see `detect.md`) —
     confirm with the user, don't guess silently.
   - `[health].path`: a plausible endpoint (`/`, `/health`, `/healthz`) —
     confirm.
   - `env`: non-secret config only (log level, feature flags, non-sensitive
     hostnames/ports).
   - `secrets`: names only, for anything sensitive (API keys, DB passwords,
     tokens) — never put a real value in `lwd.toml`.

4. **Suggest `[[services]]` backing** when a matching client dependency or
   compose db service is found: `postgres:16`, `redis:7`, or `minio/minio`,
   each with a named `volume` and matching `env`/`secrets` (see
   `references/detect.md` and the worked example in `references/schema.md`).

5. **Ask only the unknowables**, in one batch:
   - `domain` (required — cannot be inferred).
   - Confirm the inferred `port` and `[health].path`.
   - Confirm any suggested backing services (image/version, whether to
     include them at all).

6. **Write `./lwd.toml`.** Then tell the user:
   - Run `lwd apply .` (or use the `lwd-web` dashboard's Deploy modal).
   - For each declared secret, run `lwd secret set <app> <KEY>` before the
     first deploy — deploys fail closed on any unset secret.

## Validation is non-negotiable

Every field and constraint in `references/schema.md` comes directly from
`internal/spec/spec.go`'s `Validate()`. Do not invent fields, and do not
relax a rule (e.g. secret name charset, git URL scheme, no `..` in paths) —
an invalid `lwd.toml` fails `lwd apply` outright. When unsure whether a shape
is valid, check the decision table and rule list in `references/schema.md`
before writing the file.
