# lwd Phase 7 — `lwd.toml` authoring skill

**Status:** Design (decisions resolved with the user)
**Date:** 2026-07-04
**Builds on:** Phases 1–6 (all merged).
**Deliverable:** a Claude skill (SKILL.md + references), NOT Go code. Built via the
writing-skills skill; committed to the lwd repo and copied into `~/.claude/skills`.

## Goal

A skill that, when the user asks to "make an `lwd.toml`" / "deploy this project with
lwd," inspects the project, infers the right app shape, and writes a valid `lwd.toml`
that will pass `lwd apply` on the first try.

## Decisions (resolved)

1. **Location:** `skills/lwd-toml/` in the lwd repo (versioned with the schema it
   targets), and also copied into `~/.claude/skills/lwd-toml/` so it's active now.
2. **Depth:** full inspector — detect Dockerfile / `docker-compose.yml` / language /
   port / backing deps, infer the shape + a health path, suggest `[[services]]`, and
   only ask for what it can't know (the `domain`; confirm the port).

## What the skill contains

- **`SKILL.md`** — name `lwd-toml`; description triggers on authoring/generating an
  `lwd.toml` or "deploy with lwd." The procedure:
  1. **Inspect** the target dir: is there a `Dockerfile`? a `docker-compose.yml`? what
     language/framework (package.json, go.mod, requirements.txt/pyproject, Gemfile,
     Cargo.toml…)? what port does the app listen on (Dockerfile `EXPOSE`, framework
     default, env)? any backing deps (a postgres/redis/mysql/minio/s3 client in the
     manifest, or a db service in a compose file)?
  2. **Choose the shape** (decision table in a reference file):
     - Dockerfile in a git repo → `[git]` (url/ref) + `[build]` (dockerfile) — the
       build-from-source path.
     - Prebuilt image the user pushes → `image = "..."`.
     - The repo's *own* `docker-compose.yml` you want run as-is → a Phase-4 compose app
       (`compose = "docker-compose.yml"`, `service = "..."`). (Note: distinct from
       `[[services]]` backing.)
  3. **Infer** `port` + a `[health]` path (e.g. `/`, `/health`, `/healthz` — confirm
     with the user), non-secret `env`, and `secrets = [...]` names for anything
     sensitive (never values).
  4. **Suggest `[[services]]` backing** (pinned db/cache/object-store) when a client
     dep or compose db service is detected — with sane images (postgres:16, redis:7,
     minio/minio), a named `volume`, and matching `env`/`secrets`.
  5. **Ask only the unknowables** — the `domain` (required; can't infer), confirm the
     port, confirm suggested backing services.
  6. **Write** `./lwd.toml`, then tell the user to `lwd apply .` (or use the web UI),
     and mention `lwd secret set <app> <KEY>` for each declared secret.
- **`references/schema.md`** — the authoritative `lwd.toml` schema + validation rules,
  so generated files always validate:
  - fields: `name, image, domain, port, node, env, secrets, [health]{path,timeout},
    [git]{url,ref,path}, [build]{context,dockerfile}, compose, service, [[services]]{name,image,command,env,secrets,volume}`.
  - rules (from the code): `[[services]]` is plural (compose apps use singular
    `service`); a git app needs `url`+`[build]`, no `image`/`compose`; `[[services]]`
    not allowed on compose apps; **secret names must match `^[A-Za-z_][A-Za-z0-9_]*$`**;
    git url scheme must be http/https/git/ssh/file/scp-style (no `ext::`); git `ref`
    `^[A-Za-z0-9][A-Za-z0-9._/-]*$`; no `..`/absolute in `git.path`/`build.*`; backing
    service `name` matches `^[a-z0-9][a-z0-9-]*$`; port required; name required + DNS-safe.
  - a couple of complete worked examples (a Node app from git with a Postgres backing
    service; a prebuilt-image app; a compose app).
- **`references/detect.md`** — the detection heuristics (files → language → default
  port/health; deps → suggested backing services) as a lookup table.

## Non-goals

- Deploying (that's Phase 8's MCP; the skill only authors the file).
- Modifying the project's code/Dockerfile.
- Inventing a domain or secret values.

## Verification

- The skill's worked examples must parse+validate against the real `internal/spec`
  (round-trip each example through `spec.Parse`+`Validate` during the build; a failing
  example is a skill bug).
- Dogfood: run the skill against 2–3 sample project layouts (a Go+Dockerfile repo, a
  Node app with a `pg` dep, a repo with a `docker-compose.yml`) and confirm the
  generated `lwd.toml` validates and matches the intended shape.
- Follow the writing-skills skill's own checklist (frontmatter, trigger clarity,
  no placeholders, token-lean SKILL.md with details in references).

## README

Add a short "Authoring lwd.toml (Claude skill)" note pointing at `skills/lwd-toml/`
and how to install it (copy/symlink into `~/.claude/skills`).
