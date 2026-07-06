# Task 3 report: wire CLI/lwd-mcp/lwd-web to `client.FromEnv`; drop `web.Config.SocketPath`; lwd-web public-bind warning

## Summary

Routed all three daemon-client construction sites — `internal/cli.newClient()`,
`cmd/lwd-mcp/main.go`, `cmd/lwd-web/main.go` — through `client.FromEnv()`
(built in Task 2), removing their direct `client.New(config.SocketPath())` /
`client.New(cfg.SocketPath)` calls. Removed `web.Config.SocketPath` entirely
— that resolution now lives solely in `client.FromEnv`. Added a log-only
public-bind warning to `lwd-web` when its listen address is non-loopback.

With `LWD_DAEMON` unset (the default), all three still resolve to the local
unix socket exactly as before — `client.FromEnv` falls back to
`LWD_SOCKET`/`config.SocketPath()`, same defaulting `web.Config.SocketPath`
used to do inline.

## Files changed

- `internal/cli/cli.go` — `newClient()` body: `client.New(config.SocketPath())` → `client.FromEnv()`. (`internal/config` import still used elsewhere in the file — untouched.)
- `cmd/lwd-mcp/main.go` — `main()` builds `c := client.FromEnv()` instead of resolving `config.SocketPath()` itself; dropped the now-unused `lwd/internal/config` import; log line no longer names a socket path (`"lwd-mcp: serving MCP over stdio"`); updated the package doc comment to mention `LWD_DAEMON`/`LWD_API_TOKEN` as the remote path.
- `cmd/lwd-web/main.go` — builds `c := client.FromEnv()` instead of `client.New(cfg.SocketPath)`; log line is now `"lwd-web: listening on %s"` (dropped the socket-path clause); added `hostIsLoopback(addr string) bool` (inlined, mirrors `internal/cli/apiauth.go`'s `isLoopbackAddr` — a few lines, no new cross-package coupling) and a startup check that logs the public-bind warning (does not block startup); updated the package doc comment.
- `cmd/lwd-web/main_test.go` (new) — `TestHostIsLoopback` table test for the new helper (loopback host/port forms true; `0.0.0.0`, bare `:port`, routable IP, hostname, and a malformed addr all false).
- `internal/web/config.go` — removed `SocketPath` field from `Config`; removed its `LWD_SOCKET`/`config.SocketPath()` resolution from `LoadConfig`; dropped the now-unused `lwd/internal/config` import; updated the `LoadConfig` doc comment to state the daemon target is resolved by `client.FromEnv` (not this package).
- `internal/web/auth_test.go` — added `TestLoadConfigReturnsAddrPasswordKey`, which loads a valid config and uses `reflect` to assert `Config` has exactly 3 fields and no `SocketPath` field (a compile-proof-by-reflection pin, since no existing test constructed `Config{SocketPath: ...}` literally — nothing else needed updating for compile).

No other file in the repo referenced `web.Config.SocketPath` or built a
`web.Config{...}` literal with that field (checked via grep across
`internal/web/*_test.go` and the whole tree) — the two pre-existing
`TestLoadConfigRequiresPassword`/`TestLoadConfigRejectsShortSecret` tests
only set `LWD_SOCKET` as an env var to zero out cross-test leakage; that's
harmless now that `LoadConfig` doesn't read it, and was left as-is (removing
those two lines is optional cleanup, not required for correctness).

## TDD: RED → GREEN

**RED** (added `cmd/lwd-web/main_test.go` referencing the not-yet-existing
`hostIsLoopback`, and `TestLoadConfigReturnsAddrPasswordKey` against the
not-yet-changed `Config` struct, before touching any implementation):

```
$ go test ./internal/web/... ./cmd/lwd-web/...
# lwd/cmd/lwd-web [lwd/cmd/lwd-web.test]
cmd/lwd-web/main_test.go:20:13: undefined: hostIsLoopback
--- FAIL: TestLoadConfigReturnsAddrPasswordKey (0.00s)
    auth_test.go:113: expected Config to have exactly 3 fields (Addr, Password, SigningKey), got 4: {Addr:127.0.0.1:8079 Password:hunter2 SocketPath:/var/lib/lwd/lwd.sock SigningKey:[...]}
FAIL
FAIL	lwd/internal/web	0.469s
FAIL	lwd/cmd/lwd-web [build failed]
FAIL
```

**GREEN** (after implementing `web.Config` field removal, the FromEnv wiring
in cli/mcp/web, and the `hostIsLoopback` helper + warning):

```
$ go build ./...
(clean)

$ go test ./...
ok  	lwd/cmd/lwd-web	0.447s
ok  	lwd/internal/cli	0.724s
ok  	lwd/internal/web	1.010s
... (all other packages ok/cached, no failures)
```

## Verification

```
$ go vet ./...            # clean
$ gofmt -l .              # clean (no output)
$ CGO_ENABLED=0 go build ./...                          # clean
$ CGO_ENABLED=0 go build -o /tmp/lwd-bin/ ./cmd/...      # lwd, lwd-agent, lwd-mcp, lwd-web all built
```

Manual smoke test of the warning (built binary, no Docker/daemon needed since
`lwd-web` doesn't dial the daemon until a request comes in):

```
$ LWD_WEB_PASSWORD=x LWD_WEB_ADDR=127.0.0.1:0 ./lwd-web
2026/07/06 11:41:17 lwd-web: listening on 127.0.0.1:0
                                                              # no warning — loopback

$ LWD_WEB_PASSWORD=x LWD_WEB_ADDR=0.0.0.0:0 ./lwd-web
2026/07/06 11:41:17 lwd-web: WARNING: bound to a non-loopback address (0.0.0.0:0) over plain HTTP — put it behind TLS (e.g. Caddy) or an SSH tunnel; the session cookie is marked Secure only behind an X-Forwarded-Proto: https proxy
2026/07/06 11:41:17 lwd-web: listening on 0.0.0.0:0
                                                              # warning logged, then still starts listening (log-only, non-blocking)
```

## Loopback helper location

Inlined as `hostIsLoopback` directly in `cmd/lwd-web/main.go` (not exported
from `internal/cli`, which lwd-web — a separate binary/module-internal
package — has no other reason to import). It's a near-verbatim copy of
`internal/cli/apiauth.go`'s `isLoopbackAddr`: `net.SplitHostPort`, then a
switch on `127.0.0.1`/`::1`/`localhost`. Duplication here is intentional per
the brief ("Keep it local") — the two copies are small, stable, and unlikely
to drift, and keeping them separate avoids adding a new internal/cli →
cmd/lwd-web (or a new shared package) dependency for a 10-line predicate.

## Public-bind warning

`cmd/lwd-web/main.go`, right after `web.LoadConfig()` succeeds and before
`client.FromEnv()`/`web.NewServer` are constructed:

```go
if !hostIsLoopback(cfg.Addr) {
    log.Printf("lwd-web: WARNING: bound to a non-loopback address (%s) over plain HTTP — put it behind TLS (e.g. Caddy) or an SSH tunnel; the session cookie is marked Secure only behind an X-Forwarded-Proto: https proxy", cfg.Addr)
}
```

Log-only — never returns an error, never calls `os.Exit`. `lwd-web` still
starts and serves on any address, matching the brief's "Do NOT block
startup."

## Self-review

- Confirmed `LWD_DAEMON` unset is the zero-regression path: `client.FromEnv`
  (Task 2, untouched here) falls back to `LWD_SOCKET`/`config.SocketPath()`
  for all three call sites, identical to each site's prior direct
  `config.SocketPath()` resolution.
- Confirmed no remaining compile-time or runtime reference to
  `web.Config.SocketPath` anywhere in the tree (`grep -rn "SocketPath"` over
  `internal/web`, `cmd/lwd-web`, `cmd/lwd-mcp` shows only the doc-comment
  prose and the reflection-based negative-assertion test).
- Confirmed `cmd/lwd-mcp/main.go`'s `lwd/internal/config` import was in fact
  now-unused after removing the `config.SocketPath()` call (build fails
  loudly on an unused import, so this was caught by `go build`, not missed).
- Verified the warning fires for `0.0.0.0:0`, is silent for `127.0.0.1:0`,
  and does not prevent `ListenAndServe` from proceeding, via a live binary
  run (see Verification above) — not just a unit test of the pure predicate.
- README/docs updates for the new `LWD_DAEMON`/`LWD_API_TOKEN` env vars and a
  "Remote access & running lwd-web as a service" section are explicitly
  out of scope for this task — confirmed via
  `docs/superpowers/specs/2026-07-06-lwd-remote-daemon-access.md`, which
  scopes that README section to a later task in this plan (referred to as
  "T5 docs" in the task brief's Interfaces section).

## Concerns

None blocking. Two minor, non-blocking notes:

1. `internal/web/auth_test.go`'s two pre-existing `LoadConfig` tests
   (`TestLoadConfigRequiresPassword`, `TestLoadConfigRejectsShortSecret`)
   still `t.Setenv("LWD_SOCKET", "")`, which is now a no-op for `LoadConfig`
   (it never reads that var). Left in place — harmless, and removing it is
   pure cleanup with no behavior change; happy to strip it in a follow-up if
   preferred.
2. `hostIsLoopback` and `internal/cli`'s `isLoopbackAddr` are now two
   independent copies of the same ~10-line predicate. This was an explicit
   choice per the brief to avoid new cross-package coupling; flagging it in
   case a future task wants to consolidate into a small shared package
   (e.g. `internal/netutil`) once a third consumer appears.
