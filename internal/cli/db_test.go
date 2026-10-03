package cli

import (
	"strings"
	"testing"
)

const backupJSON = `{"id":7,"app":"hello","env":"staging","host":"m1","database":"hello_staging","file":"20261003T030000.000Z.dump","bytes":2048,"sha256":"ab","kind":"manual","status":"succeeded","started_at":"2026-10-03T03:00:00Z","finished_at":"2026-10-03T03:00:02Z"}`
const failedBackupJSON = `{"id":8,"app":"hello","env":"prod","host":"m1","database":"hello_prod","bytes":0,"kind":"scheduled","status":"failed","error":"pg_dump: connection refused","started_at":"2026-10-03T03:00:00Z","finished_at":"2026-10-03T03:00:01Z"}`

func TestHelpListsDBCommands(t *testing.T) {
	f := newFakeAPI(t)
	r := f.run(t, "", "help")
	for _, c := range []string{"db status", "db backup", "db backups", "db restore", "--yes", "backup status"} {
		if !strings.Contains(r.stdout, c) {
			t.Errorf("usage missing %q", c)
		}
	}
}

func TestDBStatus(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["GET /v1/apps/hello/envs/staging/resources"] = `[{"app":"hello","env":"staging","kind":"bucket","host":"m1","name":"hello-staging","created_at":"2026-10-03T01:00:00Z","updated_at":"2026-10-03T01:00:00Z"},{"app":"hello","env":"staging","kind":"database","host":"m1","name":"hello_staging","created_at":"2026-10-03T01:00:00Z","updated_at":"2026-10-03T01:00:00Z"}]`
	f.responses["GET /v1/apps/hello/envs/staging/db/backups"] = `[` + backupJSON + `]`
	r := f.run(t, "", "db", "status", "hello", "staging")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"database", "hello_staging", "bucket", "hello-staging", "m1", "last backup", "7", "20261003T030000.000Z.dump"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, r.stdout)
		}
	}
	if r := f.run(t, "", "db", "status", "hello", "staging", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"resources"`) {
		t.Errorf("json: %+v", r)
	}
}

func TestDBBackupAndList(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["POST /v1/apps/hello/envs/staging/db/backup"] = backupJSON
	r := f.run(t, "", "db", "backup", "hello", "staging")
	if r.code != 0 || !strings.Contains(r.stdout, "backup 7") || !strings.Contains(r.stdout, "2048") {
		t.Fatalf("%+v", r)
	}
	f.responses["GET /v1/apps/hello/envs/staging/db/backups"] = `[` + failedBackupJSON + `,` + backupJSON + `]`
	r = f.run(t, "", "db", "backups", "hello", "staging")
	if r.code != 0 || !strings.Contains(r.stdout, "ID") || !strings.Contains(r.stdout, "failed") || !strings.Contains(r.stdout, "connection refused") {
		t.Fatalf("%+v", r)
	}
}

func TestDBRestoreRequiresYes(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["POST /v1/apps/hello/envs/staging/db/restore"] = `{"backup":` + backupJSON + `,"file":"20261003T030000.000Z.dump","stopped":true,"stop_ms":100,"restore_ms":2000,"start_ms":300,"total_ms":2400}`
	r := f.run(t, "", "db", "restore", "hello", "staging", "7")
	if r.code != 2 || !strings.Contains(r.stderr, "--yes") || f.count() != 0 {
		t.Fatalf("without --yes: %+v (requests %d)", r, f.count())
	}
	if r := f.run(t, "", "db", "restore", "hello", "staging", "x", "--yes"); r.code != 2 {
		t.Errorf("bad id: %+v", r)
	}
	r = f.run(t, "", "db", "restore", "hello", "staging", "7", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "restored backup 7") || !strings.Contains(r.stdout, "2.4s") {
		t.Fatalf("%+v", r)
	}
	if body := f.last(t).Body; body != `{"backup_id":7}` {
		t.Errorf("body = %s", body)
	}
}

func TestBackupStatus(t *testing.T) {
	f := newFakeAPI(t)
	f.responses["GET /v1/backups"] = `{"databases":[{"app":"hello","env":"prod","host":"m1","database":"hello_prod","last_failure":` + failedBackupJSON + `},{"app":"hello","env":"staging","host":"m1","database":"hello_staging","latest":` + backupJSON + `}],"failures":[` + failedBackupJSON + `]}`
	r := f.run(t, "", "backup", "status")
	if r.code != 1 {
		t.Errorf("a database with no successful backup should exit 1: %+v", r)
	}
	for _, want := range []string{"hello/prod", "never", "hello/staging", "20261003T030000.000Z.dump", "FAILURES", "connection refused"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, r.stdout)
		}
	}
	f.responses["GET /v1/backups"] = `{"databases":[{"app":"hello","env":"staging","host":"m1","database":"hello_staging","latest":` + backupJSON + `}],"failures":[]}`
	if r := f.run(t, "", "backup", "status"); r.code != 0 || strings.Contains(r.stdout, "FAILURES") {
		t.Errorf("healthy: %+v", r)
	}
}
