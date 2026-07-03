# lwd Phase 3 Implementation Plan — secrets

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** `lwd.toml` declares secret names; values are set via `lwd secret set` (stdin), stored encrypted at rest, and injected as env at container start. Never in the repo, never read back out.

**Architecture:** A `secrets.Cipher` (AES-256-GCM, stdlib, key file `0600`) encrypts values; the `store` persists encrypted blobs in a `secrets` table; a `secrets.Store` combines them and exposes `Resolve(app, names)` (fail-closed). The reconciler merges resolved secrets into container env at deploy. API/CLI: `secret set|ls|rm` — set takes stdin, ls returns names only, no get-value endpoint.

**Tech Stack:** Go 1.25+, stdlib `crypto/aes`+`crypto/cipher`+`crypto/rand` (no new dep, no cgo), `modernc.org/sqlite`.

## Global Constraints

- Go floor **1.25**; **no cgo** (CGO_ENABLED=0). Module `lwd`; imports `lwd/internal/<pkg>`.
- Design spec governs: `docs/superpowers/specs/2026-07-03-lwd-phase3-secrets-design.md`.
- **Encryption:** AES-256-GCM via Go stdlib. 32-byte key at `<data_dir>/secret.key`, generated with `crypto/rand` and written `0600` if absent. Random 12-byte nonce prepended to ciphertext. NO external crypto dependency.
- **Values never leave the daemon:** no get-value API/CLI; `ls` returns names only; `set` reads the value from stdin.
- **Fail-closed:** a declared secret (`secrets=[...]`) with no stored value aborts the deploy (pre-start), leaving any running version untouched.
- **Per-app** secrets only, keyed by `(app, key)`. Setting an existing key overwrites (upsert).
- Tests use a temp data dir; Docker-dependent tests guarded by `LWD_DOCKER_TEST`.

---

### Task 1: secrets.Cipher — AES-256-GCM with a key file

**Files:**
- Create: `internal/secrets/cipher.go`
- Test: `internal/secrets/cipher_test.go`

**Interfaces:**
- Produces:
  - `type Cipher struct { ... }`
  - `func NewCipher(keyPath string) (*Cipher, error)` — loads the 32-byte key at keyPath; if the file is absent, generates 32 random bytes and writes them `0600` (creating parent dir if needed); errors if present but not exactly 32 bytes.
  - `func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error)` — AES-256-GCM; output = nonce(12) || ciphertext.
  - `func (c *Cipher) Decrypt(blob []byte) ([]byte, error)` — splits nonce, decrypts; errors on short/tampered input.

- [ ] **Step 1: Write the failing tests**

Create `internal/secrets/cipher_test.go`:
```go
package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCipherRoundTrip(t *testing.T) {
	kp := filepath.Join(t.TempDir(), "secret.key")
	c, err := NewCipher(kp)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := c.Encrypt([]byte("hunter2"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != "hunter2" {
		t.Errorf("got %q, want hunter2", got)
	}
}

func TestKeyFileGeneratedWith0600(t *testing.T) {
	kp := filepath.Join(t.TempDir(), "secret.key")
	if _, err := NewCipher(kp); err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	fi, err := os.Stat(kp)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file perm = %v, want 0600", fi.Mode().Perm())
	}
	if fi.Size() != 32 {
		t.Errorf("key size = %d, want 32", fi.Size())
	}
}

func TestKeyFilePersistsAcrossOpens(t *testing.T) {
	kp := filepath.Join(t.TempDir(), "secret.key")
	c1, _ := NewCipher(kp)
	blob, _ := c1.Encrypt([]byte("x"))
	c2, err := NewCipher(kp) // same key file
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := c2.Decrypt(blob)
	if err != nil || string(got) != "x" {
		t.Fatalf("cross-open decrypt failed: %v %q", err, got)
	}
}

func TestNonceRandomized(t *testing.T) {
	c, _ := NewCipher(filepath.Join(t.TempDir(), "k"))
	a, _ := c.Encrypt([]byte("same"))
	b, _ := c.Encrypt([]byte("same"))
	if bytes.Equal(a, b) {
		t.Error("expected different ciphertexts (random nonce)")
	}
}

func TestDecryptRejectsTampered(t *testing.T) {
	c, _ := NewCipher(filepath.Join(t.TempDir(), "k"))
	blob, _ := c.Encrypt([]byte("data"))
	blob[len(blob)-1] ^= 0xff // flip a byte
	if _, err := c.Decrypt(blob); err == nil {
		t.Error("expected error decrypting tampered blob")
	}
	if _, err := c.Decrypt([]byte("short")); err == nil {
		t.Error("expected error decrypting too-short blob")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/secrets/ -v`
Expected: FAIL — `undefined: NewCipher`.

- [ ] **Step 3: Implement**

Create `internal/secrets/cipher.go`:
```go
// Package secrets stores per-app secret values encrypted at rest and resolves
// them at deploy time. Values enter the daemon once and are never read back out.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const keySize = 32 // AES-256

// Cipher encrypts and decrypts secret values with AES-256-GCM using a key loaded
// from (or generated at) a key file.
type Cipher struct {
	gcm cipher.AEAD
}

// NewCipher loads the 32-byte key at keyPath, generating and writing it (0600) if
// absent. It errors if the file exists but is not exactly 32 bytes.
func NewCipher(keyPath string) (*Cipher, error) {
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Cipher{gcm: gcm}, nil
}

func loadOrCreateKey(keyPath string) ([]byte, error) {
	data, err := os.ReadFile(keyPath)
	if err == nil {
		if len(data) != keySize {
			return nil, fmt.Errorf("key file %s is %d bytes, want %d", keyPath, len(data), keySize)
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir key dir: %w", err)
	}
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}
	return key, nil
}

// Encrypt returns nonce||ciphertext for the plaintext.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return c.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt.
func (c *Cipher) Decrypt(blob []byte) ([]byte, error) {
	ns := c.gcm.NonceSize()
	if len(blob) < ns {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	pt, err := c.gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return pt, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/secrets/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/secrets/cipher.go internal/secrets/cipher_test.go
git commit -m "feat: secrets cipher (AES-256-GCM with 0600 key file)"
```

---

### Task 2: store — encrypted secrets table

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

**Interfaces:**
- Produces (store never sees plaintext — blobs are pre-encrypted by the caller):
  - `func (s *Store) SetSecret(app, key string, enc []byte) error` — upsert.
  - `func (s *Store) GetSecret(app, key string) ([]byte, error)` — `(nil, nil)` when absent.
  - `func (s *Store) ListSecretKeys(app string) ([]string, error)` — sorted ascending.
  - `func (s *Store) DeleteSecret(app, key string) error`.
- Schema: add to the base `schema` string
  `CREATE TABLE IF NOT EXISTS secrets (app TEXT NOT NULL, key TEXT NOT NULL, value BLOB NOT NULL, PRIMARY KEY(app,key));` (naturally idempotent; safe on existing DBs — no ALTER needed).

- [ ] **Step 1: Write failing tests**

Add to `internal/store/store_test.go` (use `t.TempDir()`):
```go
func TestSecretSetGetDelete(t *testing.T) {
	s := openTemp(t)
	if err := s.SetSecret("blog", "DB", []byte{1, 2, 3}); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	got, err := s.GetSecret("blog", "DB")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(got) != string([]byte{1, 2, 3}) {
		t.Errorf("got %v", got)
	}
	// upsert
	if err := s.SetSecret("blog", "DB", []byte{9}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = s.GetSecret("blog", "DB")
	if len(got) != 1 || got[0] != 9 {
		t.Errorf("upsert failed: %v", got)
	}
	// delete
	if err := s.DeleteSecret("blog", "DB"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	got, _ = s.GetSecret("blog", "DB")
	if got != nil {
		t.Errorf("want nil after delete, got %v", got)
	}
}

func TestGetSecretAbsentReturnsNil(t *testing.T) {
	s := openTemp(t)
	got, err := s.GetSecret("nope", "X")
	if err != nil || got != nil {
		t.Fatalf("want (nil,nil), got (%v,%v)", got, err)
	}
}

func TestListSecretKeysSortedAndScoped(t *testing.T) {
	s := openTemp(t)
	s.SetSecret("blog", "B", []byte{1})
	s.SetSecret("blog", "A", []byte{1})
	s.SetSecret("api", "Z", []byte{1})
	keys, err := s.ListSecretKeys("blog")
	if err != nil {
		t.Fatalf("ListSecretKeys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "A" || keys[1] != "B" {
		t.Fatalf("keys = %v, want [A B]", keys)
	}
}
```
(`openTemp` already exists in the test file from Phase 1.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ -run Secret -v` → FAIL.

- [ ] **Step 3: Implement**

Add the `secrets` table to the `schema` const, and implement the four methods with parameterized SQL. `SetSecret` uses `INSERT ... ON CONFLICT(app,key) DO UPDATE SET value=excluded.value`. `GetSecret` returns `(nil, nil)` on `sql.ErrNoRows`. `ListSecretKeys` does `SELECT key ... WHERE app=? ORDER BY key ASC`.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/store/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat: store encrypted per-app secrets table"
```

---

### Task 3: secrets.Store — cipher + persistence + Resolve (fail-closed)

**Files:**
- Create: `internal/secrets/store.go`
- Create: `internal/secrets/store_test.go`

**Interfaces:**
- Consumes: `secrets.Cipher`, `store.Store` (SetSecret/GetSecret/ListSecretKeys/DeleteSecret).
- Produces:
  - `func NewStore(c *Cipher, db *store.Store) *Store`
  - `func (s *Store) Set(app, key, value string) error` — encrypt then persist.
  - `func (s *Store) Get(app, key string) (value string, ok bool, err error)`
  - `func (s *Store) List(app string) ([]string, error)`
  - `func (s *Store) Delete(app, key string) error`
  - `func (s *Store) Resolve(app string, names []string) (map[string]string, error)` — returns all values, or an error naming the first missing/undecryptable secret (**fail-closed**).

- [ ] **Step 1: Write failing tests**

Create `internal/secrets/store_test.go`:
```go
package secrets

import (
	"path/filepath"
	"strings"
	"testing"

	"lwd/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	c, err := NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	db, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(c, db)
}

func TestSetGetList(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("blog", "DB", "postgres://x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok, err := s.Get("blog", "DB")
	if err != nil || !ok || v != "postgres://x" {
		t.Fatalf("Get = %q,%v,%v", v, ok, err)
	}
	names, _ := s.List("blog")
	if len(names) != 1 || names[0] != "DB" {
		t.Fatalf("List = %v", names)
	}
}

func TestResolveAllPresent(t *testing.T) {
	s := newTestStore(t)
	s.Set("blog", "A", "1")
	s.Set("blog", "B", "2")
	got, err := s.Resolve("blog", []string{"A", "B"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["A"] != "1" || got["B"] != "2" {
		t.Fatalf("Resolve = %v", got)
	}
}

func TestResolveFailsClosedOnMissing(t *testing.T) {
	s := newTestStore(t)
	s.Set("blog", "A", "1")
	_, err := s.Resolve("blog", []string{"A", "MISSING"})
	if err == nil {
		t.Fatal("want error for missing secret")
	}
	if !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("error should name the missing secret, got %v", err)
	}
}

func TestResolveEmptyNames(t *testing.T) {
	s := newTestStore(t)
	got, err := s.Resolve("blog", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Resolve(nil) = %v, %v", got, err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/secrets/ -run 'SetGetList|Resolve' -v` → FAIL.

- [ ] **Step 3: Implement**

Create `internal/secrets/store.go`:
```go
package secrets

import (
	"fmt"

	"lwd/internal/store"
)

// Store persists per-app secret values encrypted via Cipher and resolves them.
type Store struct {
	cipher *Cipher
	db     *store.Store
}

// NewStore combines a Cipher with the persistence store.
func NewStore(c *Cipher, db *store.Store) *Store {
	return &Store{cipher: c, db: db}
}

// Set encrypts value and upserts it for (app, key).
func (s *Store) Set(app, key, value string) error {
	enc, err := s.cipher.Encrypt([]byte(value))
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	return s.db.SetSecret(app, key, enc)
}

// Get returns the decrypted value and whether it exists.
func (s *Store) Get(app, key string) (string, bool, error) {
	enc, err := s.db.GetSecret(app, key)
	if err != nil {
		return "", false, err
	}
	if enc == nil {
		return "", false, nil
	}
	pt, err := s.cipher.Decrypt(enc)
	if err != nil {
		return "", false, fmt.Errorf("decrypt %s/%s: %w", app, key, err)
	}
	return string(pt), true, nil
}

// List returns the secret names for an app (never values), sorted.
func (s *Store) List(app string) ([]string, error) {
	return s.db.ListSecretKeys(app)
}

// Delete removes a secret.
func (s *Store) Delete(app, key string) error {
	return s.db.DeleteSecret(app, key)
}

// Resolve returns values for all names, or an error naming the first that is
// missing or undecryptable (fail-closed — used at deploy time).
func (s *Store) Resolve(app string, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, name := range names {
		v, ok, err := s.Get(app, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("secret %q is not set for app %q", name, app)
		}
		out[name] = v
	}
	return out, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/secrets/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/secrets/
git commit -m "feat: secrets.Store with fail-closed Resolve"
```

---

### Task 4: reconciler — inject resolved secrets into container env

**Files:**
- Modify: `internal/reconciler/reconciler.go`
- Modify: `internal/reconciler/reconciler_test.go`

**Interfaces:**
- Produces:
  - `type SecretResolver interface { Resolve(app string, names []string) (map[string]string, error) }`
  - `New` gains a `SecretResolver` param: `func New(n node.Node, r router.Router, s *store.Store, sec SecretResolver) *Reconciler`.
- Behavior: in `Apply`, build the container env as `merge(app.Env, resolve(app.Secrets))`, secrets overriding on key collision. If `Resolve` errors, abort the deploy **before starting the new container** (record `StatusFailed`, return the error) — the running version is untouched. `secrets.Store` satisfies `SecretResolver`.

- [ ] **Step 1: Write failing tests**

Add a fake resolver to `internal/reconciler/reconciler_test.go` and tests. The test helper `newTestReconciler` must be updated to pass a resolver.
```go
type fakeResolver struct {
	vals map[string]string
	err  error
}

func (f *fakeResolver) Resolve(app string, names []string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]string{}
	for _, n := range names {
		out[n] = f.vals[n]
	}
	return out, nil
}
```
Tests:
- `TestApplyInjectsSecrets`: app with `Env{"A":"1"}`, `Secrets:["DB"]`, resolver returns `{"DB":"secret"}`, ProbeStatus 200 → the fake node's created container `RunSpec.Env` contains both `A=1` and `DB=secret`. (Add capture of the last `RunSpec` to `node.Fake` if not present — check fake.go; if it records containers with Env, assert via that.)
- `TestSecretOverridesEnv`: `Env{"K":"plain"}`, `Secrets:["K"]`, resolver `{"K":"secret"}` → container env has `K=secret`.
- `TestApplyFailsClosedOnResolveError`: resolver returns an error → Apply errors, NO new container started (node.Calls has no `RunContainer`), running version (if any) untouched.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/reconciler/ -v` → FAIL (New signature; behavior).

- [ ] **Step 3: Implement**

Update `New` to store the resolver. In `Apply`, before `RunContainer`, compute:
```go
secretVals, err := r.secrets.Resolve(app.Name, app.Secrets)
if err != nil {
	// fail closed, before starting anything; record a failed attempt with the spec snapshot
	// (no container to remove; old version untouched)
	specJSON, _ := json.Marshal(app)
	_, _ = r.store.RecordDeployment(store.Deployment{App: app.Name, Image: app.Image, Status: store.StatusFailed, Spec: string(specJSON), CreatedAt: time.Now()})
	return nil, fmt.Errorf("resolve secrets: %w", err)
}
env := map[string]string{}
for k, v := range app.Env { env[k] = v }
for k, v := range secretVals { env[k] = v } // secrets win
```
Place this after `EnsureImage` (or after validate/EnsureUp) but **before** `RunContainer`, and pass `env` (not `app.Env`) into the `node.RunSpec`. Keep everything else in the blue-green flow identical.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/reconciler/ -v` and `go test ./...` → PASS. `CGO_ENABLED=0 go build ./...`, `go vet ./...` clean.

Note: `New`'s signature change ripples to `internal/cli` (daemon), `internal/api` tests, `internal/client` tests, `test/e2e_test.go`. Update those call sites minimally to pass a resolver so the build stays green: real daemon passes the real `secrets.Store` (wired fully in Task 5/6); tests pass a `&fakeResolver{}` or the real store. Note precisely what you touched outside `internal/reconciler` in your report.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "feat: inject resolved secrets into container env (fail-closed)"
```

---

### Task 5: API + client + CLI — secret set/ls/rm; wire secrets.Store into the daemon

**Files:**
- Modify: `internal/config/config.go` (add `KeyPath()`)
- Modify: `internal/api/api.go`, `internal/api/api_test.go`
- Modify: `internal/client/client.go`
- Modify: `internal/cli/cli.go`

**Interfaces:**
- `config.KeyPath() string` = `filepath.Join(DataDir(), "secret.key")`.
- API (`Server` gains a `secrets *secrets.Store` field; `api.New` gains the param — update all call sites):
  - `POST /apps/{name}/secrets` body `{"key":"..","value":".."}` → `secrets.Set` → 204. 400 on missing key.
  - `GET /apps/{name}/secrets` → 200 JSON `[]string` (names via `secrets.List`). Never values.
  - `DELETE /apps/{name}/secrets/{key}` → `secrets.Delete` → 204.
- Client: `SetSecret(ctx, app, key, value string) error`, `ListSecrets(ctx, app string) ([]string, error)`, `DeleteSecret(ctx, app, key string) error`.
- CLI (`Run` dispatch gains `secret`):
  - `lwd secret set <app> <KEY>` — reads the value from **stdin** (all of stdin, trimming a single trailing newline), calls `SetSecret`, prints `secret <KEY> set for <app>; redeploy to apply`.
  - `lwd secret ls <app>` — prints names, one per line.
  - `lwd secret rm <app> <KEY>` — deletes, prints confirmation.
- Daemon (`runDaemon`): build `cipher, err := secrets.NewCipher(config.KeyPath())`; `secStore := secrets.NewStore(cipher, store)`; pass `secStore` to both `reconciler.New(...)` (as the resolver) and `api.New(...)`.

- [ ] **Step 1: Write failing api tests**

In `internal/api/api_test.go`, extend the test server to construct a real `secrets.Store` (temp cipher + the test's store) OR a fake implementing the same methods the Server uses. Prefer a real `secrets.Store` (temp dir key). Tests:
- `TestSecretSetAndList`: POST a secret, then GET `/apps/blog/secrets` returns `["DB"]` (name only). The value must NOT appear anywhere in the list response.
- `TestSecretDelete`: after set, DELETE `/apps/blog/secrets/DB` → 204; GET returns empty.
- `TestSecretSetMissingKey`: POST with empty key → 400.

- [ ] **Step 2: Run to verify failure** → FAIL.

- [ ] **Step 3: Implement** config.KeyPath, the API routes + `Server.secrets` field + `api.New` param (update all call sites incl. cli.go daemon, client_test, e2e), client methods, CLI `secret` subcommands, and the daemon wiring (cipher + secrets.Store passed to reconciler.New and api.New).

- [ ] **Step 4: Verify**

Run: `CGO_ENABLED=0 go build -o /tmp/lwd ./cmd/lwd && go test ./... && go vet ./... && /tmp/lwd version`
Expected: all pass; version prints `lwd 0.1.0-dev`.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "feat: secret set/ls/rm API+CLI; wire secrets.Store into daemon"
```

---

### Task 6: e2e + README

**Files:**
- Modify: `test/e2e_test.go`
- Modify: `README.md`

**Interfaces:** consumes the full stack.

- [ ] **Step 1: Extend the e2e (guarded by `LWD_DOCKER_TEST`)**

Add a subtest (or extend the existing e2e) that: constructs the real stack including `secrets.NewStore(NewCipher(tempKey), store)` wired into the reconciler; sets a secret (`secStore.Set(app, "LWD_TEST_SECRET", "s3cr3t")`); deploys `traefik/whoami` declaring `Secrets:["LWD_TEST_SECRET"]` and `port=80` at a `.localhost` domain; then asserts the secret reached the container's environment — e.g. `whoami`'s `/api` or `/` output echoes request env, OR inspect the container's env via the docker client and assert `LWD_TEST_SECRET=s3cr3t` is present. Also assert a deploy declaring an UNSET secret fails closed (Apply returns an error and the app is not started). Clean up containers + network as the existing e2e does.

- [ ] **Step 2: Run unit suite** — `go test ./...` (e2e SKIPs) → PASS.

- [ ] **Step 3: Run the real e2e** — `LWD_DOCKER_TEST=1 go test ./test/ -v` → MUST pass against real Docker; confirm no strays.

- [ ] **Step 4: README** — add a "Secrets" section: `secrets = [...]` in lwd.toml (names only, committed); `lwd secret set <app> <KEY>` reads from stdin; values encrypted at rest with a `0600` key file; fail-closed on missing secret; values never read back (`ls` shows names only). Note the threat model (protects DB/backups, not root-on-host) and add it to Known limitations if apt. Move `secrets` from the "deferred" list to "supported."

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "test: e2e secret injection + fail-closed; docs: secrets section"
```

---

## Self-Review

**Spec coverage:**
- AES-256-GCM encryption with 0600 key file (stdlib) → Task 1. ✓
- Encrypted persistence (secrets table, blobs) → Task 2. ✓
- secrets.Store + fail-closed Resolve → Task 3. ✓
- Reconciler env injection (secrets override env), fail-closed abort before start → Task 4. ✓
- API (set/list-names/delete, no get-value) + client + CLI (stdin set, ls names, rm) + daemon wiring → Task 5. ✓
- e2e (injection + fail-closed) + README → Task 6. ✓

**Deferred (by design):** global/shared secrets, rotation, external managers; compose/surfaces (Phase 4), web UI (Phase 5).

**Placeholder scan:** pure/logic units (cipher, store, secrets.Store, reconciler merge) have complete code + tests; API/CLI/e2e follow established Phase 1/2 patterns with concrete contracts. No TBD.

**Type consistency:** `SecretResolver.Resolve(app string, names []string)(map[string]string,error)` matches `secrets.Store.Resolve`. Store blob methods (`SetSecret/GetSecret/ListSecretKeys/DeleteSecret`) consistent across store/secrets.Store. `api.New` + `reconciler.New` signature changes noted with all call sites (cli daemon, api/client/e2e tests). `config.KeyPath()` used by the daemon to build the cipher.

**Cross-task note:** Task 4 changes `reconciler.New`; Task 5 changes `api.New`. Both ripple to cli/api-tests/client-tests/e2e — each task updates its call sites to keep `go test ./...` green, with the real `secrets.Store` wired only in Task 5's daemon path.
