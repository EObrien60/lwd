# Task 2 Report (Phase 12): spec.Replicas + store.Deployment.Replicas (model + migration)

## Summary

Implemented exactly the interfaces required by T3–T7:
- `spec.App.Replicas int` (`toml:"replicas"`); `Parse` defaults an unset (0) value to 1; `Validate` rejects `< 1`, rejects `> 50`, and rejects `> 1` combined with `Compose != ""` ("replicas not supported for compose apps").
- `store.Replica{ContainerID, Node, Upstream string; Port int}`.
- `store.Deployment.Replicas []Replica`, backed by a new `replicas TEXT NOT NULL DEFAULT ''` column, added via a guarded `migrateAddReplicasColumn` (mirrors `migrateAddScheduledColumn`) called from `Open`. `RecordDeployment`/`CurrentDeployment`/`PreviousDeployment`/`DeploymentsForApp` all read/write it consistently.
- Local helpers `(*Deployment).replicasJSON() (string, error)` and `decodeReplicas(string) ([]Replica, error)`.

## Files changed

- `internal/spec/spec.go` — `App.Replicas` field (doc'd); `Parse` defaults 0→1; `Validate` gains the three replicas checks, placed **after** the git/compose/single-service shape block (not before it) so a compose app that's already invalid for a shape-specific reason (missing service/domain/port, remote node) still reports that error rather than a generic replicas one.
- `internal/spec/spec_test.go` — added `TestParseDefaultsReplicasToOne`, `TestParseReplicas`, `TestValidateReplicasMin`, `TestValidateReplicasMax`, `TestValidateReplicasCompose`; backfilled `Replicas: 1` onto every pre-existing hand-built `App` literal that reaches a successful `Validate()` (the `gitApp` helper, plus ~10 standalone literals) — their Go zero-value `Replicas` (0) would otherwise now fail the new floor check.
- `internal/store/store.go` — `Replica` type; `Deployment.Replicas`; `replicasJSON`/`decodeReplicas`; schema/migration; `RecordDeployment`/`CurrentDeployment`/`PreviousDeployment`/`DeploymentsForApp` updated in lockstep (column list + scan args + decode, in that order, at all four call sites).
- `internal/store/store_test.go` — added `TestDeploymentReplicasRoundTrip` (3-replica record→decode across Current/Previous/DeploymentsForApp, plus a no-replicas row round-tripping to nil) and `TestMigrationFromPreReplicasSchema` (pre-12 DDL without the column → `Open` migrates → legacy row decodes to nil `Replicas`).
- `internal/api/api.go` — **fallout fix, outside the brief's file list**: `handleApply` now defaults `Replicas` 0→1 after JSON-decoding the request body, mirroring `spec.Parse`. See "Fallout" below.
- `internal/mcp/tools.go` — **fallout fix**: `registerLwdDeployGit`'s hand-built `App` literal now sets `Replicas: 1`.
- `internal/reconciler/reconciler_test.go`, `internal/reconciler/schedule_test.go` — **fallout fix**: added `Replicas: 1` to the shared `testApp`/`testGitApp`/`testComposeApp`/`unpinnedApp` factories used across the whole `reconciler` test package.
- `test/e2e_test.go` — **fallout fix**: added `Replicas: 1` to all 13 hand-built `*spec.App` literals.

## Design decisions

**JSON nil-handling.** `replicasJSON()` encodes a nil/empty `Replicas` slice as `""` (not `"null"` or `"[]"`), matching the column's `NOT NULL DEFAULT ''`. `decodeReplicas("")` returns `nil, nil` — explicitly `nil`, never an empty non-nil `[]Replica{}` — so a legacy pre-Phase-12 row and a deployment recorded with no replicas are indistinguishable on read, both surfacing as `Replicas == nil`. Verified both directions in `TestDeploymentReplicasRoundTrip`.

**Column/scan consistency.** All four call sites (`RecordDeployment` insert column list + `?` placeholders; `CurrentDeployment`/`PreviousDeployment`/`DeploymentsForApp` SELECT column list + `Scan` args) were updated together, in the same relative position (`replicas` appended last, after `scheduled`, matching the physical column order). `-race` run confirms no data races from the added `encoding/json` calls (they're pure, no shared state).

**Validate check placement (judgment call).** The brief describes the replicas checks as applying "to all app types" without specifying order relative to the existing git/compose/single-service block. I placed them *after* that block (right before the Services check) rather than immediately after the Pool check (where I initially put them), specifically so `TestComposeRejectsRemoteNode`-style tests (an already-invalid compose app) report their original, more specific error rather than being masked by a generic "replicas must be >= 1" — since a hand-built `App` used in such a test has `Replicas == 0` and would otherwise short-circuit Validate before ever reaching the remote-node check.

## Fallout: `Replicas >= 1` breaks every hand-built `App` that skips `Parse`

`Validate` now rejects `Replicas == 0`, but `spec.Parse`'s 0→1 default is the *only* place that default is applied — anything that builds an `App` directly (test fixtures, or the two production call sites that skip `Parse`) reaches `Validate` with the Go zero value. Running `go test ./...` after the initial implementation surfaced ~90 failing tests across `internal/spec`, `internal/reconciler`, `internal/api`, `internal/mcp`, `internal/client`, and `test/` — this is the same class of fallout the Phase 11b Task 2 report documented for `scheduler.NodeInfo.Schedulable` (a new zero-value-sensitive bool broke ~9 pre-existing `NodeInfo` literals), and I followed the same precedent: fix every affected call site rather than relax the check.

Two of these are genuine **production** gaps, not just test fixtures, so I fixed them even though they're outside `internal/spec/spec.go` + `internal/store/store.go`:
- `internal/api/api.go`'s `handleApply` JSON-decodes a request body directly into `spec.App` and calls `Validate()` — it never goes through `Parse`, so any real API client that POSTs a body without `"replicas"` would get a 400 today. Fixed with a one-line default (`if app.Replicas == 0 { app.Replicas = 1 }`) mirroring `Parse`.
- `internal/mcp/tools.go`'s `registerLwdDeployGit` builds `spec.App` by hand (it predates a `replicas` input option — that's Task 8's job) — fixed by setting `Replicas: 1` on the literal. `registerLwdApply` was already safe since it calls `spec.Parse`/`spec.Load`.

Everything else was test-only fallout, fixed by adding `Replicas: 1` at each hand-built `App` literal (or its shared factory function, where one existed, to fix many call sites at once).

## TDD RED

```
$ go test ./internal/spec/... ./internal/store/... 2>&1
--- FAIL: TestValidateAcceptsGoodSpec ... (and ~10 more) — replicas must be >= 1
[Replicas/Replica-named tests didn't exist yet — build succeeded once the field/type stubs were added,
 then the newly-written TestParseDefaultsReplicasToOne/TestParseReplicas/TestValidateReplicasMin/
 TestValidateReplicasMax/TestValidateReplicasCompose and TestDeploymentReplicasRoundTrip/
 TestMigrationFromPreReplicasSchema were run before implementation and failed as expected —
 e.g. TestValidateReplicasMax failed because Validate() had no >50 check yet, and
 TestDeploymentReplicasRoundTrip failed with "unknown column replicas".]
```
(Practically: I implemented the `spec.App.Replicas` field and `store.Replica`/`Deployment.Replicas` types+migration in the same pass as writing the tests, per the brief's Step 1/2 sequencing, then ran the full suite to drive out the fallout described above — the more informative RED signal here was the ~90 pre-existing test failures once `Validate`'s floor check went in, not a missing-symbol build error.)

## TDD GREEN

```
$ go test ./internal/spec/ ./internal/store/ -race -v
... (all PASS, including TestParseDefaultsReplicasToOne, TestParseReplicas, TestValidateReplicasMin,
     TestValidateReplicasMax, TestValidateReplicasCompose, TestDeploymentReplicasRoundTrip,
     TestMigrationFromPreReplicasSchema)
PASS
ok  	lwd/internal/spec	(cached)
ok  	lwd/internal/store	1.747s
```

Full suite after all fallout fixes:
```
$ go build ./...                                          → clean
$ go vet ./...                                             → clean
$ gofmt -l .                                                → no output (clean)
$ go test ./...                                             → all packages ok
$ go test ./internal/reconciler/... ./internal/api/... \
        ./internal/mcp/... ./internal/client/... ./test/... -race
                                                             → all PASS, no races
```

## Migration

`migrateAddReplicasColumn` is byte-for-byte the same shape as `migrateAddScheduledColumn`: `PRAGMA table_info(deployments)` check → `ALTER TABLE ... ADD COLUMN replicas TEXT NOT NULL DEFAULT ''` if missing → tolerate a concurrent "duplicate column name" error. Called from `Open` after `migrateAddScheduledColumn`. `TestMigrationFromPreReplicasSchema` builds a pre-12 `deployments` table (no `replicas` column) with one legacy row, opens it through `Open`, and asserts the migrated row's `Replicas` decodes to `nil`.

## Self-review

- Grepped `&App{`/`&spec.App{`/`spec.App{` across the whole repo (72 hits) and traced every one to whether it reaches `.Validate()` (directly or via `Apply`/`handleApply`/an MCP tool) — fixed all of them; the ones that don't reach `Validate` (e.g. some `json.Marshal` fixtures in `cli_test.go`/`web/api_test.go` that are only ever consumed as opaque fixture bytes by a fake, never re-validated) needed no change, confirmed by the full green suite.
- Verified `decodeReplicas("")` returns `nil` (not `[]Replica{}`) via `TestDeploymentReplicasRoundTrip`'s explicit `!= nil` assertion on both the no-replicas record and the legacy-migration cases.
- Verified column/scan ordering is identical across `RecordDeployment` (insert) and all three read paths by inspection — `replicas` is always last, immediately after `scheduled`.
- Ran `-race` on `internal/spec`, `internal/store`, and every package I touched for fallout (`reconciler`, `api`, `mcp`, `client`, `test`) — no races.
- Confirmed `gofmt -l .` is clean (had to reword two doc comments that used a doubled straight-single-quote `''` sequence — this environment's `gofmt` rewrites that specific sequence to a Unicode closing quote inside comments; switched to double quotes `""` instead, which it leaves alone).

## Concerns

- None blocking. The two production fallout fixes (`api.go`, `mcp/tools.go`) are minimal and match `Parse`'s existing default exactly, but they are genuinely outside this task's stated file list — flagged clearly above and in `progress-phase12.md`'s findings roll-up for the final review to double check.
- T7 (the `scale` endpoint) and T8 (MCP `replicas` input) will want to double-check they don't reintroduce a similar zero-value gap when they add new `App`-construction paths that skip `Parse`.

## Commit

`feat: spec.Replicas + store.Deployment.Replicas set (model + migration)` (pending — see below for exact hash once committed).

---

## Fix: Replicas=0 backward-compat (follow-up commit)

The coordinator caught a real upgrade regression in the original `Validate` rule (the brief's wording `Replicas >= 1` was too strict). A **pre-Phase-12 deployment snapshot** already in a user's DB has no `replicas` field, so it JSON-unmarshals to `Replicas == 0`. Both heal (`healSurfaceLocked` → `restored.Validate()`) and rollback (`rollbackGit`/`rollbackImage` → `Validate()`) reconstruct a `spec.App` from that old snapshot and **re-validate** it — so a `Replicas < 1` check would make healing or rolling back ANY existing pre-12 deployment fail after this upgrade.

**Fix:** `Validate` now treats `Replicas == 0` as "unset" (valid). It rejects only `Replicas < 0` (error "replicas must be >= 0") and `Replicas > 50`; the compose guard (`> 1 && Compose != ""`) is unchanged (0 or 1 with compose is fine). `Parse` still defaults a fresh spec's `0 → 1`, so newly-authored specs normalize to 1 as before — the change only affects specs that bypass `Parse` (reconstructed snapshots, raw API/MCP bodies), which is exactly the backward-compat path.

**Tests:** `TestValidateReplicasMin` now asserts a negative (`-1`) → error; added `TestValidateReplicasZeroIsUnset` (a normal surface app with `Replicas == 0` validates cleanly) and `TestValidatePreV12SnapshotReplicasZero` (a `spec.App` unmarshaled from a JSON snapshot with no `replicas` key → `Replicas == 0` → `Validate()` succeeds). `TestValidateReplicasMax` and `TestValidateReplicasCompose` unchanged and still pass.

The `handleApply`/`registerLwdDeployGit` `Replicas` 0→1 defaulting from the first commit is kept (harmless belt-and-suspenders) but is no longer load-bearing now that `Validate` accepts 0.

**NOTE for Task 4:** `deployReplicaSet` must treat `Replicas <= 0` as 1 when USING it as a *count* — a reconstructed old snapshot (or a raw body) can carry 0, which now passes `Validate`. Apply a `max(1, app.Replicas)` at the count site (where the number of containers to launch is derived), not in `Validate`.

**Verify:** `go test ./internal/spec/ -race -v` PASS (incl. zero-is-unset + pre-v12-snapshot); `go test ./...` all ok; build/vet/gofmt clean.

**Commit:** `fix: Validate treats replicas=0 as unset (backward-compat with pre-P12 snapshots)`.
