package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lwd/internal/bundle"
)

const testPassword = "Pw_abcdefghijklmnopqrstuvwxyz012345"

// fakePlatform simulates the docker CLI's view of the platform: whether the
// network exists, the Garage layout, buckets and keys, and pg_dump output.
type fakePlatform struct {
	mu        sync.Mutex
	network   bool
	assigned  bool
	staged    bool
	buckets   map[string]bool
	keys      map[string]string // id -> name
	nextKey   int
	dump      string
	failVerbs map[string]error // "pg_restore", "stop", ... -> error
}

func newFakePlatform(h *harness) *fakePlatform {
	fp := &fakePlatform{buckets: map[string]bool{}, keys: map[string]string{}, dump: "PGDMP-fake-dump", failVerbs: map[string]error{}}
	h.docker.respond = fp.respond
	return fp
}

func keyID(i int) string { return fmt.Sprintf("GK%024x", i) }

func (fp *fakePlatform) respond(c call) (string, error, bool) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if err := fp.failVerbs[c.Verb]; err != nil {
		return "", err, true
	}
	switch c.Verb {
	case "network":
		switch c.Rest[0] {
		case "inspect":
			if !fp.network {
				return "", fmt.Errorf("docker network: exit status 1: network lwd-platform not found"), true
			}
			return "lwd-platform\n", nil, true
		case "create":
			fp.network = true
			return "abc\n", nil, true
		}
	case "exec":
		args := c.Rest
		if args[0] == "-i" {
			args = args[1:]
		}
		container, cmd := args[0], args[1:]
		if err := fp.failVerbs[cmd[0]]; err != nil {
			return "", err, true
		}
		switch {
		case container == s3Container:
			return fp.garage(cmd[1:])
		case cmd[0] == "pg_dump":
			return fp.dump, nil, true
		case cmd[0] == "psql", cmd[0] == "pg_restore":
			return "", nil, true
		}
	}
	return "", nil, false
}

func (fp *fakePlatform) garage(a []string) (string, error, bool) {
	j := strings.Join(a, " ")
	switch {
	case j == "layout show":
		switch {
		case fp.staged:
			return "staged...\n    garage layout apply --version 1\n", nil, true
		case fp.assigned:
			return "Current cluster layout version: 1\n", nil, true
		}
		return "No nodes currently have a role in the cluster.\nCurrent cluster layout version: 0\n", nil, true
	case j == "node id -q":
		return "be3f584f1e737f1de718e5814df83b5e0c6aa1371eedecdfb6a38ecc7da8b61e@127.0.0.1:3901\n", nil, true
	case strings.HasPrefix(j, "layout assign -z dc1 -c "+garageCapacity+" be3f584f1e737f1de718e5814df83b5e0c6aa1371eedecdfb6a38ecc7da8b61e"):
		fp.staged = true
		return "staged\n", nil, true
	case j == "layout apply --version 1":
		fp.staged, fp.assigned = false, true
		return "applied\n", nil, true
	case a[0] == "bucket" && a[1] == "info":
		if !fp.buckets[a[2]] {
			return "", fmt.Errorf("docker exec: exit status 1: Error: GetBucketInfo returned NoSuchBucket (404): Bucket not found: %s", a[2]), true
		}
		return "==== BUCKET INFORMATION ====\n", nil, true
	case a[0] == "bucket" && a[1] == "create":
		fp.buckets[a[2]] = true
		return "==== BUCKET INFORMATION ====\n", nil, true
	case a[0] == "bucket" && a[1] == "allow":
		return "RWO\n", nil, true
	case j == "key list":
		out := "ID                          Created     Name                 Expiration\n"
		for id, name := range fp.keys {
			out += id + "  2026-10-03  " + name + "  never\n"
		}
		return out, nil, true
	case a[0] == "key" && a[1] == "create":
		fp.nextKey++
		id := keyID(fp.nextKey)
		fp.keys[id] = a[2]
		return keyInfo(id, a[2]), nil, true
	case a[0] == "key" && a[1] == "info":
		name, ok := fp.keys[a[2]]
		if !ok {
			return "", fmt.Errorf("docker exec: exit status 1: Error: GetKeyInfo returned NoSuchAccessKey (404): Access key not found: %s", a[2]), true
		}
		out := keyInfo(a[2], name)
		if len(a) < 4 || a[3] != "--show-secret" {
			out = strings.Replace(out, secretFor(a[2]), "(redacted)", 1)
		}
		return out, nil, true
	}
	return "", fmt.Errorf("unexpected garage command %q", j), true
}

func secretFor(id string) string { return strings.Repeat(id[len(id)-1:], 64) }

func keyInfo(id, name string) string {
	return "==== ACCESS KEY INFORMATION ====\nKey ID:              " + id + "\nKey name:            " + name +
		"\nSecret key:          " + secretFor(id) + "\nCreated:             2026-10-03\n"
}

func (h *harness) execCalls(prog string) []call {
	return h.docker.callsMatching(func(c call) bool {
		if c.Verb != "exec" {
			return false
		}
		for _, a := range c.Rest {
			if a == prog {
				return true
			}
		}
		return false
	})
}

func TestProvisionDatabaseStartsPlatform(t *testing.T) {
	h := newHarness(t)
	fp := newFakePlatform(h)
	resp, body := h.do("PUT", "/v1/platform/databases/hello_staging", "secret", bundle.DatabaseRequest{Password: testPassword})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if !fp.network {
		t.Error("platform network not created")
	}
	if !h.docker.has(platformProject, "up") {
		t.Errorf("platform not started: %q", h.docker.recorded())
	}
	ups := h.docker.callsMatching(func(c call) bool { return c.Project == platformProject && c.Verb == "up" })
	if strings.Join(ups[0].Rest, " ") != "-d --wait --wait-timeout 180" {
		t.Errorf("up args = %q", ups[0].Rest)
	}
	if !fp.assigned {
		t.Error("garage layout not assigned and applied")
	}
	for _, f := range []string{"platform/compose.yaml", "platform/postgres.password", "platform/garage.toml"} {
		info, err := os.Stat(filepath.Join(h.dir, f))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, err, info)
		}
	}
	info, _ := os.Stat(filepath.Join(h.dir, "platform"))
	if info.Mode().Perm() != 0o700 {
		t.Errorf("platform dir mode %v", info.Mode())
	}
	pw, _ := os.ReadFile(filepath.Join(h.dir, "platform/postgres.password"))
	if len(strings.TrimSpace(string(pw))) != 64 {
		t.Errorf("superuser password length %d", len(strings.TrimSpace(string(pw))))
	}
	compose, _ := os.ReadFile(filepath.Join(h.dir, "platform/compose.yaml"))
	if !strings.Contains(string(compose), strings.TrimSpace(string(pw))) {
		t.Error("compose does not use the generated superuser password")
	}

	psql := h.execCalls("psql")
	if len(psql) != 2 {
		t.Fatalf("psql calls = %+v", psql)
	}
	for _, c := range psql {
		joined := strings.Join(c.Rest, " ")
		if !strings.Contains(joined, "ON_ERROR_STOP=1") || strings.Contains(joined, testPassword) {
			t.Errorf("psql args %q: want ON_ERROR_STOP and no password on the command line", joined)
		}
	}
	if !strings.Contains(psql[0].Stdin, "REVOKE CONNECT, TEMPORARY ON DATABASE postgres, template1 FROM PUBLIC") {
		t.Errorf("hardening sql = %q", psql[0].Stdin)
	}
	sql := psql[1].Stdin
	for _, want := range []string{
		`CREATE ROLE "hello_staging" LOGIN`,
		`ALTER ROLE "hello_staging" WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '` + testPassword + `'`,
		`CREATE DATABASE "hello_staging" OWNER "hello_staging"`,
		`ALTER DATABASE "hello_staging" OWNER TO "hello_staging"`,
		`REVOKE CONNECT, TEMPORARY ON DATABASE "hello_staging" FROM PUBLIC`,
		`\connect "hello_staging"`,
		`ALTER SCHEMA public OWNER TO "hello_staging"`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("sql missing %q:\n%s", want, sql)
		}
	}

	// Idempotent: a second call keeps generated secrets and the layout.
	garageToml, _ := os.ReadFile(filepath.Join(h.dir, "platform/garage.toml"))
	resp, body = h.do("PUT", "/v1/platform/databases/hello_staging", "secret", bundle.DatabaseRequest{Password: testPassword})
	if resp.StatusCode != 200 {
		t.Fatalf("second: %d %s", resp.StatusCode, body)
	}
	pw2, _ := os.ReadFile(filepath.Join(h.dir, "platform/postgres.password"))
	garageToml2, _ := os.ReadFile(filepath.Join(h.dir, "platform/garage.toml"))
	if string(pw2) != string(pw) || string(garageToml2) != string(garageToml) {
		t.Error("platform secrets regenerated")
	}
	assigns := h.docker.callsMatching(func(c call) bool { return strings.Contains(strings.Join(c.Rest, " "), "layout assign") })
	if len(assigns) != 1 {
		t.Errorf("layout assigned %d times", len(assigns))
	}
	creates := h.docker.callsMatching(func(c call) bool { return c.Verb == "network" && c.Rest[0] == "create" })
	if len(creates) != 1 {
		t.Errorf("network created %d times", len(creates))
	}
}

func TestGarageLayoutCompletesStagedChange(t *testing.T) {
	h := newHarness(t)
	fp := newFakePlatform(h)
	fp.staged = true
	if err := h.n.ensureGarageLayout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fp.assigned {
		t.Error("staged layout not applied")
	}
	if n := len(h.docker.callsMatching(func(c call) bool { return strings.Contains(strings.Join(c.Rest, " "), "layout assign") })); n != 0 {
		t.Errorf("assigned again (%d)", n)
	}
}

func TestPlatformRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	newFakePlatform(h)
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/v1/platform/databases/Hello", bundle.DatabaseRequest{Password: testPassword}},
		{"PUT", "/v1/platform/databases/a-b", bundle.DatabaseRequest{Password: testPassword}},
		{"PUT", "/v1/platform/databases/" + strings.Repeat("a", 64), bundle.DatabaseRequest{Password: testPassword}},
		{"PUT", "/v1/platform/databases/ok_name", bundle.DatabaseRequest{Password: "short"}},
		{"PUT", "/v1/platform/databases/ok_name", bundle.DatabaseRequest{Password: "abcdefghijklmnopqrstuvwxyz'; DROP ROLE x; --"}},
		{"PUT", "/v1/platform/databases/ok_name", "{bad json"},
		{"PUT", "/v1/platform/buckets/ab", bundle.BucketRequest{}},
		{"PUT", "/v1/platform/buckets/-abc", bundle.BucketRequest{}},
		{"PUT", "/v1/platform/buckets/a_bc", bundle.BucketRequest{}},
		{"PUT", "/v1/platform/buckets/abc", bundle.BucketRequest{AccessKeyID: "GK; rm -rf"}},
		{"POST", "/v1/platform/databases/Bad/backup", nil},
		{"GET", "/v1/platform/databases/Bad/backups", nil},
		{"POST", "/v1/platform/databases/ok_name/restore", bundle.RestoreRequest{File: "../../etc/passwd", App: "hello", Env: "staging"}},
		{"POST", "/v1/platform/databases/ok_name/restore", bundle.RestoreRequest{File: "20261003T110700.000Z.dump", App: "Hello", Env: "staging"}},
	} {
		resp, body := h.do(tc.method, tc.path, "secret", tc.body)
		if resp.StatusCode != http.StatusBadRequest || errCode(t, body) != "invalid" {
			t.Errorf("%s %s %v: %d %s", tc.method, tc.path, tc.body, resp.StatusCode, body)
		}
	}
	if calls := h.docker.recorded(); len(calls) != 0 {
		t.Errorf("invalid input reached docker: %q", calls)
	}
	for _, path := range []string{"/v1/platform/databases/x", "/v1/platform/buckets/xyz"} {
		if resp, _ := h.do("PUT", path, "", bundle.DatabaseRequest{}); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without token: %d", path, resp.StatusCode)
		}
	}
}

func TestProvisionBucket(t *testing.T) {
	h := newHarness(t)
	fp := newFakePlatform(h)
	resp, body := h.do("PUT", "/v1/platform/buckets/hello-staging", "secret", bundle.BucketRequest{})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var creds bundle.BucketCredentials
	json.Unmarshal(body, &creds)
	if creds.AccessKeyID != keyID(1) || creds.SecretAccessKey != secretFor(keyID(1)) {
		t.Fatalf("creds = %+v", creds)
	}
	if !fp.buckets["hello-staging"] || fp.keys[keyID(1)] != "lwd-hello-staging" {
		t.Errorf("garage state: buckets %v keys %v", fp.buckets, fp.keys)
	}
	allow := h.docker.callsMatching(func(c call) bool { return strings.Contains(strings.Join(c.Rest, " "), "bucket allow") })
	if len(allow) != 1 || !strings.HasSuffix(strings.Join(allow[0].Rest, " "), "bucket allow --read --write --owner hello-staging --key "+keyID(1)) {
		t.Errorf("allow = %+v", allow)
	}

	// Same key id again: kept, nothing created.
	resp, body = h.do("PUT", "/v1/platform/buckets/hello-staging", "secret", bundle.BucketRequest{AccessKeyID: keyID(1)})
	var again bundle.BucketCredentials
	json.Unmarshal(body, &again)
	if resp.StatusCode != 200 || again != creds || fp.nextKey != 1 {
		t.Errorf("second: %d %+v nextKey=%d", resp.StatusCode, again, fp.nextKey)
	}
	// No id (controller lost the answer): the key is found by name.
	resp, body = h.do("PUT", "/v1/platform/buckets/hello-staging", "secret", bundle.BucketRequest{})
	json.Unmarshal(body, &again)
	if resp.StatusCode != 200 || again != creds || fp.nextKey != 1 {
		t.Errorf("by name: %d %+v nextKey=%d", resp.StatusCode, again, fp.nextKey)
	}
	// Host rebuilt: the stored key is gone, a new one is made.
	delete(fp.keys, keyID(1))
	resp, body = h.do("PUT", "/v1/platform/buckets/hello-staging", "secret", bundle.BucketRequest{AccessKeyID: keyID(1)})
	json.Unmarshal(body, &again)
	if resp.StatusCode != 200 || again.AccessKeyID != keyID(2) {
		t.Errorf("rebuilt: %d %+v", resp.StatusCode, again)
	}
	creates := h.docker.callsMatching(func(c call) bool { return strings.Contains(strings.Join(c.Rest, " "), "bucket create") })
	if len(creates) != 1 {
		t.Errorf("bucket created %d times", len(creates))
	}
}

func TestBackupDatabase(t *testing.T) {
	h := newHarness(t)
	fp := newFakePlatform(h)
	dir := filepath.Join(h.dir, "backups", "postgres", "hello_staging")
	os.MkdirAll(dir, 0o700)
	for i := 0; i < 15; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("202501%02dT000000.000Z.dump", i+1)), []byte("old"), 0o600)
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)

	resp, body := h.do("POST", "/v1/platform/databases/hello_staging/backup", "secret", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var bf bundle.BackupFile
	json.Unmarshal(body, &bf)
	sum := sha256.Sum256([]byte(fp.dump))
	if !bundle.BackupFileRE.MatchString(bf.File) || bf.Bytes != int64(len(fp.dump)) || bf.SHA256 != hex.EncodeToString(sum[:]) || bf.CreatedAt == "" {
		t.Fatalf("backup = %+v", bf)
	}
	data, err := os.ReadFile(filepath.Join(dir, bf.File))
	if err != nil || string(data) != fp.dump {
		t.Fatalf("dump file: %v %q", err, data)
	}
	info, _ := os.Stat(filepath.Join(dir, bf.File))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("dump mode %v", info.Mode())
	}
	dumps := h.execCalls("pg_dump")
	if len(dumps) != 1 || strings.Join(dumps[0].Rest, " ") != "lwd-postgres pg_dump -U postgres -Fc -d hello_staging" {
		t.Errorf("pg_dump calls = %+v", dumps)
	}

	resp, body = h.do("GET", "/v1/platform/databases/hello_staging/backups", "secret", nil)
	var list []bundle.BackupFile
	json.Unmarshal(body, &list)
	if resp.StatusCode != 200 || len(list) != keepBackups || list[0].File != bf.File {
		t.Fatalf("list after prune: %d %d %+v", resp.StatusCode, len(list), list)
	}
	if list[len(list)-1].File != "20250103T000000.000Z.dump" {
		t.Errorf("oldest kept = %s (the two oldest should be pruned)", list[len(list)-1].File)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Error("prune removed an unrelated file")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".partial") {
			t.Errorf("leftover %s", e.Name())
		}
	}

	// A failing dump leaves no file behind.
	fp.failVerbs["pg_dump"] = errFake
	resp, _ = h.do("POST", "/v1/platform/databases/hello_staging/backup", "secret", nil)
	if resp.StatusCode != 500 {
		t.Errorf("failed dump status %d", resp.StatusCode)
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != keepBackups+1 {
		t.Errorf("%d entries after failed dump", len(entries))
	}

	resp, body = h.do("GET", "/v1/platform/databases/nothing_here/backups", "secret", nil)
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("empty list: %d %s", resp.StatusCode, body)
	}
}

func TestRestoreDatabase(t *testing.T) {
	h := newHarness(t)
	h.deploy(bundleN(1))
	fp := newFakePlatform(h)
	dir := filepath.Join(h.dir, "backups", "postgres", "hello_staging")
	os.MkdirAll(dir, 0o700)
	const file = "20261003T110700.123Z.dump"
	os.WriteFile(filepath.Join(dir, file), []byte("DUMP-CONTENT"), 0o600)
	before := len(h.docker.recorded())

	req := bundle.RestoreRequest{File: file, App: "hello", Env: "staging"}
	resp, body := h.do("POST", "/v1/platform/databases/hello_staging/restore", "secret", req)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var res bundle.RestoreResult
	json.Unmarshal(body, &res)
	if !res.Stopped || res.File != file {
		t.Errorf("result = %+v", res)
	}
	calls := h.docker.callsMatching(func(call) bool { return true })[before:]
	var seq []string
	for _, c := range calls {
		switch {
		case c.Project == "lwd-hello-staging-d1":
			seq = append(seq, c.Verb)
		case c.Verb == "exec":
			seq = append(seq, c.Rest[2])
		}
	}
	if strings.Join(seq, ",") != "stop,psql,pg_restore,start" {
		t.Fatalf("sequence = %v", seq)
	}
	psql := h.execCalls("psql")
	sql := psql[len(psql)-1].Stdin
	for _, want := range []string{
		`DROP DATABASE IF EXISTS "hello_staging" WITH (FORCE)`,
		`CREATE DATABASE "hello_staging" OWNER "hello_staging"`,
		`REVOKE CONNECT, TEMPORARY ON DATABASE "hello_staging" FROM PUBLIC`,
		`ALTER SCHEMA public OWNER TO "hello_staging"`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("restore sql missing %q", want)
		}
	}
	restore := h.execCalls("pg_restore")[0]
	if restore.Stdin != "DUMP-CONTENT" || !strings.Contains(strings.Join(restore.Rest, " "), "pg_restore -U postgres --no-owner --role=hello_staging --exit-on-error -d hello_staging") {
		t.Errorf("pg_restore = %+v", restore)
	}

	// A failing restore still starts the app again and reports the error.
	fp.failVerbs["pg_restore"] = errFake
	before = len(h.docker.recorded())
	resp, body = h.do("POST", "/v1/platform/databases/hello_staging/restore", "secret", req)
	if resp.StatusCode != 500 || !strings.Contains(string(body), "pg_restore") {
		t.Errorf("failed restore: %d %s", resp.StatusCode, body)
	}
	last := h.docker.recorded()[before:]
	if last[len(last)-1] != "lwd-hello-staging-d1 start " {
		t.Errorf("app not started after failed restore: %q", last)
	}
	delete(fp.failVerbs, "pg_restore")

	// Missing backup: 404, nothing stopped.
	before = len(h.docker.recorded())
	resp, body = h.do("POST", "/v1/platform/databases/hello_staging/restore", "secret", bundle.RestoreRequest{File: "20200101T000000.000Z.dump", App: "hello", Env: "staging"})
	if resp.StatusCode != 404 || errCode(t, body) != "not_found" || len(h.docker.recorded()) != before {
		t.Errorf("missing backup: %d %s", resp.StatusCode, body)
	}

	// Busy app-env: 409.
	h.n.tryLock("hello-staging")
	resp, body = h.do("POST", "/v1/platform/databases/hello_staging/restore", "secret", req)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("busy: %d %s", resp.StatusCode, body)
	}
	h.n.unlock("hello-staging")

	// No live deployment: restore without stopping anything.
	resp, body = h.do("POST", "/v1/platform/databases/hello_staging/restore", "secret", bundle.RestoreRequest{File: file, App: "other", Env: "prod"})
	json.Unmarshal(body, &res)
	if resp.StatusCode != 200 || res.Stopped {
		t.Errorf("no live: %d %s", resp.StatusCode, body)
	}
}

func TestStatusReportsPlatform(t *testing.T) {
	h := newHarness(t)
	st := h.n.status(context.Background())
	if st.Platform.Provisioned || st.Platform.Services == nil {
		t.Errorf("fresh node platform = %+v", st.Platform)
	}
	newFakePlatform(h)
	if err := h.n.ProvisionDatabase(context.Background(), "a_b", bundle.DatabaseRequest{Password: testPassword}); err != nil {
		t.Fatal(err)
	}
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Project == platformProject && c.Verb == "ps" {
			return `{"Name":"lwd-postgres","Service":"postgres","State":"running","Health":"healthy"}` + "\n" + `{"Name":"lwd-s3","Service":"s3","State":"running","Health":"healthy"}`, nil, true
		}
		return "", nil, false
	}
	st = h.n.status(context.Background())
	if !st.Platform.Provisioned || len(st.Platform.Services) != 2 || st.Platform.Services[0].Name != "lwd-postgres" {
		t.Errorf("platform = %+v", st.Platform)
	}
}

func TestStartReupsExistingPlatformOnly(t *testing.T) {
	h := newHarness(t)
	newFakePlatform(h)
	if err := h.n.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.docker.has(platformProject, "up") {
		t.Error("platform started on a node that never provisioned a resource")
	}
	if err := h.n.ProvisionDatabase(context.Background(), "a_b", bundle.DatabaseRequest{Password: testPassword}); err != nil {
		t.Fatal(err)
	}
	h.docker.calls = nil
	if err := h.n.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.docker.has(platformProject, "up") {
		t.Error("existing platform not re-upped on start")
	}
	// A platform failure does not stop the node from starting.
	h.docker.respond = func(c call) (string, error, bool) {
		if c.Project == platformProject && c.Verb == "up" {
			return "", errFake, true
		}
		return "", nil, false
	}
	if err := h.n.Start(context.Background()); err != nil {
		t.Errorf("start failed because of the platform: %v", err)
	}
}

func TestDeployPlatformBundleEnsuresNetwork(t *testing.T) {
	h := newHarness(t)
	fp := newFakePlatform(h)
	b := bundleN(1)
	b.Platform = true
	res := h.deploy(b)
	if res.Status != bundle.StatusSucceeded {
		t.Fatalf("%+v", res)
	}
	if !fp.network {
		t.Error("deploy did not create the platform network")
	}
	compose, _ := os.ReadFile(filepath.Join(h.dir, "apps", "hello-staging", "d1", composeFile))
	if !strings.Contains(string(compose), `"external": true`) {
		t.Errorf("compose lacks the external platform network:\n%s", compose)
	}
}
