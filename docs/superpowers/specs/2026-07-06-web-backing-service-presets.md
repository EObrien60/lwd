# lwd-web — backing-service preset picker

**Status:** Design (user-requested). Focused web-UI feature on merged main.
**Date:** 2026-07-06

## Problem

Adding a backing service in the deploy modal today means clicking "+ Add backing
service" → a BLANK row → hand-typing the image, env, volume, secrets. Error-prone
and slow for the common cases (Postgres, Redis, MinIO…). The user wants to SELECT
from a preseeded manifest of common services that prefills a correct config, with
a Custom option retained.

(Git "autosense" — auto-detecting repo build settings — is explicitly DEFERRED.)

## What exists

Both the **From Git** and **Builder** deploy tabs have a "Backing services"
section: `deploy.<tab>.services[]`, each row from `newServiceRow()` =
`{name, image, command, volume, env:[], secrets:[]}`, rendered as an editable
"service-card", added via a single "+ Add backing service" button, and emitted as
`[[services]]` blocks by `appendServiceTables(lines, services)`. The daemon's
`spec.Service` is `{Name, Image, Command, Env map, Secrets []string, Volume}` —
**no port** (backing is reached by service name on the per-app Docker network,
using the image's default port). Secret env values come from the app's secrets
(`Secrets []string` names → resolved + injected into the backing compose at
up-time), not plaintext.

## Design

### Preset catalog (client-side, buildless)
A `SERVICE_PRESETS` array in `app.js` — curated, editable-after-pick defaults.
Each preset: `{ key, label, name, image, command?, volume, env:{...}, secrets:[...], note? }`.
`secrets` are env KEYS that must be provided as app secrets (NOT plaintext env);
the preset puts them in the row's `secrets[]`, never in `env`.

Preseed (sensible current images; user can edit any field after picking):

| key | label | image | volume | env (defaults) | secrets |
|---|---|---|---|---|---|
| postgres | PostgreSQL | `postgres:16` | `pgdata:/var/lib/postgresql/data` | `POSTGRES_DB=app`, `POSTGRES_USER=app` | `POSTGRES_PASSWORD` |
| mariadb | MySQL / MariaDB | `mariadb:11` | `mysqldata:/var/lib/mysql` | `MARIADB_DATABASE=app`, `MARIADB_USER=app` | `MARIADB_PASSWORD`, `MARIADB_ROOT_PASSWORD` |
| redis | Redis | `redis:7` | `redisdata:/data` | — | — |
| valkey | Valkey | `valkey/valkey:8` | `valkeydata:/data` | — | — |
| minio | MinIO | `minio/minio` | `miniodata:/data` | `MINIO_ROOT_USER=lwd` (command: `server /data --console-address :9001`) | `MINIO_ROOT_PASSWORD` |
| mongo | MongoDB | `mongo:7` | `mongodata:/data/db` | `MONGO_INITDB_ROOT_USERNAME=lwd` | `MONGO_INITDB_ROOT_PASSWORD` |
| custom | Custom… | — | — | — | — |

(MinIO also gets `command = "server /data --console-address :9001"`.)

### Picker UX
- Replace the single "+ Add backing service" button (in BOTH the git and builder
  tabs) with a small **preset picker**: a labeled `<select>` (or a menu of quick
  buttons) listing the preset labels + "Custom…", plus an Add action. Choosing a
  preset and adding pushes a row prefilled from the preset (spread its
  name/image/command/volume + a deep copy of env→[{key,value}] rows and
  secrets→[names]); "Custom…" pushes a blank `newServiceRow()` (today's behavior).
- The pushed row is the SAME editable `service-card` as today — every field
  (name/image/command/volume/env/secrets) remains editable; the user can tweak
  or delete. Multiple services (mixed presets + custom) can be added.
- Per service, when it has secret keys, show a clear inline note:
  "Needs secrets: `POSTGRES_PASSWORD` — set them in the **Secrets** tab (the
  deploy fails closed until every referenced secret exists)." Link/anchor to the
  app's secrets area if easy.
- Keep the design system (existing `.service-card`, `.subhead`, `.btn`,
  `.add-row-btn`, `.hint` classes; buildless — no npm/CDN/external fetch). Invoke
  the `frontend-design` skill for the picker's look so it matches the dashboard.

### Generation
No change to `appendServiceTables` semantics — a prefilled row emits the same
`[[services]]` (name/image/command/volume/env/secrets) it would if typed by hand.
VERIFY `appendServiceTables` already emits `env` and `secrets` (the row has them);
if it currently omits either, extend it so a preset's env + secrets round-trip
into the generated `lwd.toml`. Secret env keys emit as `secrets = ["KEY", ...]`
(NOT plaintext env).

## Non-goals

- Git autosense (deferred).
- A server-side/daemon service catalog (client-side JS is enough; no daemon
  change). No new backing-service runtime behavior — this is purely a
  faster/guided way to fill the existing `[[services]]`.
- Auto-generating secret VALUES (the user sets them in the Secrets tab; presets
  only declare the KEYS).

## Testing

- app.js: a small test isn't practical for browser Alpine JS; rely on
  frontend-design + code review + (if tooling permits) a Playwright screenshot;
  otherwise validate the generated toml logic. If any Go/web test harness touches
  the assets, keep it green.
- Confirm (by reading `appendServiceTables`) a preset row → correct `[[services]]`
  with env + secrets; add a note in the report showing an example generated block
  for a postgres preset (name/image/volume/env + `secrets = ["POSTGRES_PASSWORD"]`).
- Buildless: grep the diff for any external resource (must be none).
- Zero regression: the manual/Custom path is unchanged; existing deploy flows
  (image/git/builder/compose/replicas/pool) untouched; `go test ./...` green.

## Docs

README: in the Web UI section, note the deploy modal's backing-service picker
(preseeded Postgres/MySQL/Redis/Valkey/MinIO/MongoDB + Custom) and that
password-type env are added as secrets to set in the Secrets tab.
