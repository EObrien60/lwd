package controller

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"lwd/internal/bundle"
	"lwd/internal/client"
	"lwd/internal/store"
)

var resourceManifest = strings.Replace(helloManifest, `name = "hello"`, "name = \"hello\"\ndatabase = true\nstorage = true", 1)

// readyResources is ready() with database and storage declared.
func (h *harness) readyResources() store.Release {
	h.t.Helper()
	h.ready()
	if _, err := h.c.ApplyApp(h.ctx, "hello", []byte(resourceManifest)); err != nil {
		h.t.Fatal(err)
	}
	rel, err := h.c.CreateRelease(h.ctx, "hello", client.ReleaseRequest{Tag: "v1"})
	if err != nil {
		h.t.Fatal(err)
	}
	return rel
}

func (h *harness) deployOK(app, env string) store.Deployment {
	h.t.Helper()
	d, err := h.c.Deploy(h.ctx, app, env, client.DeployRequest{})
	if err != nil || d.Status != store.StatusSucceeded {
		h.t.Fatalf("deploy %s/%s = %+v, %v", app, env, d, err)
	}
	return d
}

func TestResourceNames(t *testing.T) {
	if got := databaseName("my-app", "pre-prod"); got != "my_app_pre_prod" {
		t.Errorf("databaseName = %s", got)
	}
	if got := bucketName("my-app", "pre-prod"); got != "my-app-pre-prod" {
		t.Errorf("bucketName = %s", got)
	}
	long := strings.Repeat("a", 31)
	if !bundle.DatabaseNameRE.MatchString(databaseName(long, long)) || !bundle.BucketNameRE.MatchString(bucketName(long, long)) {
		t.Error("longest app/env names do not make valid resource names")
	}
	if !bundle.BucketNameRE.MatchString(bucketName("a", "b")) {
		t.Error("shortest names do not make a valid bucket")
	}
	if pw := newPassword(); !bundle.PasswordRE.MatchString(pw) || pw == newPassword() {
		t.Errorf("password %q", pw)
	}
}

func TestDeployProvisionsAndInjectsResources(t *testing.T) {
	h := setup(t)
	h.readyResources()
	h.deployOK("hello", "staging")

	pw := h.node.dbs["hello_staging"]
	if pw == "" {
		t.Fatalf("database not provisioned: %v", h.node.dbs)
	}
	creds := h.node.buckets["hello-staging"]
	b := h.node.lastBundle()
	if !b.Platform {
		t.Error("bundle.Platform not set")
	}
	wantURL := "postgres://hello_staging:" + url.QueryEscape(pw) + "@lwd-postgres:5432/hello_staging?sslmode=disable"
	want := map[string]string{
		"DATABASE_URL":         wantURL,
		"S3_ENDPOINT":          "http://lwd-s3:3900",
		"S3_REGION":            "garage",
		"S3_BUCKET":            "hello-staging",
		"S3_ACCESS_KEY_ID":     creds.AccessKeyID,
		"S3_SECRET_ACCESS_KEY": creds.SecretAccessKey,
	}
	for k, v := range want {
		if b.Vars[k] != v {
			t.Errorf("%s = %q, want %q", k, b.Vars[k], v)
		}
	}
	if b.Vars["SESSION_SECRET"] != secretVal || b.Vars["LOG_LEVEL"] != "debug" {
		t.Errorf("ordinary vars lost: %v", b.Vars)
	}

	// Redeploy: same password, existing key passed back and kept.
	h.deployOK("hello", "staging")
	if h.node.dbs["hello_staging"] != pw {
		t.Error("database password changed on redeploy")
	}
	last := h.node.bucketReqs[len(h.node.bucketReqs)-1]
	if last.AccessKeyID != creds.AccessKeyID || h.node.keySeq != 1 {
		t.Errorf("bucket request %+v keySeq %d", last, h.node.keySeq)
	}
	if h.node.lastBundle().Vars["DATABASE_URL"] != wantURL {
		t.Error("DATABASE_URL changed on redeploy")
	}

	// Host rebuilt: the node lost the key and makes a new one; the
	// controller stores and injects it.
	delete(h.node.buckets, "hello-staging")
	h.deployOK("hello", "staging")
	nb := h.node.lastBundle()
	if nb.Vars["S3_ACCESS_KEY_ID"] == creds.AccessKeyID || nb.Vars["S3_ACCESS_KEY_ID"] != h.node.buckets["hello-staging"].AccessKeyID {
		t.Errorf("new key not injected: %s", nb.Vars["S3_ACCESS_KEY_ID"])
	}
	h.deployOK("hello", "staging")
	if got := h.node.bucketReqs[len(h.node.bucketReqs)-1].AccessKeyID; got != nb.Vars["S3_ACCESS_KEY_ID"] {
		t.Errorf("stored key not updated: sent %s", got)
	}

	rs, err := h.c.Resources(h.ctx, "hello", "staging")
	if err != nil || len(rs) != 2 || rs[0].Kind != "bucket" || rs[0].Name != "hello-staging" || rs[1].Name != "hello_staging" || rs[1].Host != "m1" {
		t.Fatalf("resources = %+v %v", rs, err)
	}

	// Credentials appear nowhere: API bodies, events, deployment results, the DB rows.
	es, _ := h.c.Events(h.ctx, "", "", 1000)
	ds, _ := h.c.Deployments(h.ctx, "hello", "staging")
	h.bodies.WriteString(mustJSON(es) + mustJSON(ds))
	all := h.bodies.String()
	for _, s := range []string{pw, creds.SecretAccessKey, nb.Vars["S3_SECRET_ACCESS_KEY"]} {
		if strings.Contains(all, s) {
			t.Errorf("a response leaked resource credential %q", s)
		}
	}
	for _, r := range []string{store.ResourceDatabase, store.ResourceBucket} {
		row, _ := h.store.GetResource(h.ctx, "hello", "staging", r)
		if strings.Contains(string(row.Credentials), pw) || strings.Contains(string(row.Credentials), "secret") {
			t.Errorf("%s credentials stored in clear", r)
		}
	}
	h.assertNoLeaks()
	if !hasEvent(es, "resource.provisioned") {
		t.Error("no resource.provisioned event")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func hasEvent(es []store.Event, kind string) bool {
	for _, e := range es {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestDeployWithoutResourcesIsUnchanged(t *testing.T) {
	h := setup(t)
	h.ready()
	h.deployOK("hello", "staging")
	if h.node.lastBundle().Platform || len(h.node.dbs) != 0 || len(h.node.buckets) != 0 {
		t.Error("resources provisioned for an app without any")
	}
	if _, ok := h.node.lastBundle().Vars["DATABASE_URL"]; ok {
		t.Error("DATABASE_URL injected without database = true")
	}
}

func TestDeployRejectsSecretCollidingWithPlatformVar(t *testing.T) {
	h := setup(t)
	h.readyResources()
	if _, err := h.c.SetSecret(h.ctx, "hello", "staging", "S3_BUCKET", []byte("mine")); err != nil {
		t.Fatal(err)
	}
	_, err := h.c.Deploy(h.ctx, "hello", "staging", client.DeployRequest{})
	wantCode(t, err, client.CodeInvalid)
	if !strings.Contains(err.Error(), "S3_BUCKET") || !strings.Contains(err.Error(), "storage = true") {
		t.Errorf("err = %v", err)
	}
	if len(h.node.bundles) != 0 {
		t.Error("bundle sent despite collision")
	}
	if ds, _ := h.c.Deployments(h.ctx, "hello", "staging"); len(ds) != 0 {
		t.Errorf("deployment recorded: %+v", ds)
	}
}

func TestDeployFailsWhenProvisioningFails(t *testing.T) {
	h := setup(t)
	h.readyResources()
	h.node.set(func(n *fakeNode) { n.failDB = true })
	_, err := h.c.Deploy(h.ctx, "hello", "staging", client.DeployRequest{})
	if err == nil || !strings.Contains(err.Error(), "hello_staging") {
		t.Fatalf("err = %v", err)
	}
	if len(h.node.bundles) != 0 {
		t.Error("bundle sent without a database")
	}
}

func TestResourceNameClashBetweenApps(t *testing.T) {
	h := setup(t)
	h.ready()
	app := func(name, env string) {
		m := strings.NewReplacer(`name = "hello"`, "name = \""+name+"\"\ndatabase = true", "[env.staging]", "[env."+env+"]", `domain = "hello.test"`, `domain = "`+name+`.test"`,
			"[env.prod]\nhost   = \"m1\"\ndomain = \"hello.example.com\"\n", "").Replace(helloManifest)
		if _, err := h.c.ApplyApp(h.ctx, name, []byte(m)); err != nil {
			t.Fatal(err)
		}
		if _, err := h.c.SetSecret(h.ctx, name, env, "SESSION_SECRET", []byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := h.c.CreateRelease(h.ctx, name, client.ReleaseRequest{Tag: "v1"}); err != nil {
			t.Fatal(err)
		}
	}
	app("a-b", "c")
	app("a", "b-c")
	h.deployOK("a-b", "c")
	_, err := h.c.Deploy(h.ctx, "a", "b-c", client.DeployRequest{})
	wantCode(t, err, client.CodeInvalid)
	if !strings.Contains(err.Error(), "a_b_c") {
		t.Errorf("err = %v", err)
	}
}

func TestDatabaseBackupAndRestore(t *testing.T) {
	h := setup(t)
	h.readyResources()

	_, err := h.c.BackupDB(h.ctx, "hello", "staging")
	wantCode(t, err, client.CodeNotFound) // not provisioned yet

	h.deployOK("hello", "staging")
	h.deployOK("hello", "prod")
	b, err := h.c.BackupDB(h.ctx, "hello", "staging")
	if err != nil || b.Status != store.BackupSucceeded || b.Kind != store.BackupManual || b.File == "" || b.Bytes != 1234 || b.SHA256 != "cafe" || b.Database != "hello_staging" || b.Host != "m1" || b.FinishedAt == nil {
		t.Fatalf("backup = %+v %v", b, err)
	}
	h.node.set(func(n *fakeNode) { n.failBackup = true })
	_, err = h.c.BackupDB(h.ctx, "hello", "staging")
	if err == nil {
		t.Fatal("failed backup reported success")
	}
	h.node.set(func(n *fakeNode) { n.failBackup = false })
	list, err := h.c.DBBackups(h.ctx, "hello", "staging")
	if err != nil || len(list) != 2 || list[0].Status != store.BackupFailed || list[0].Error == "" || list[1].ID != b.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
	es, _ := h.c.Events(h.ctx, "hello", "staging", 100)
	if !hasEvent(es, "backup.succeeded") || !hasEvent(es, "backup.failed") {
		t.Error("backup events missing")
	}

	// Restore checks the backup belongs to this app-env and succeeded.
	prodB, _ := h.c.BackupDB(h.ctx, "hello", "prod")
	_, err = h.c.RestoreDB(h.ctx, "hello", "staging", prodB.ID)
	wantCode(t, err, client.CodeInvalid)
	_, err = h.c.RestoreDB(h.ctx, "hello", "staging", list[0].ID)
	wantCode(t, err, client.CodeInvalid)
	_, err = h.c.RestoreDB(h.ctx, "hello", "staging", 99999)
	wantCode(t, err, client.CodeNotFound)
	if len(h.node.restores) != 0 {
		t.Fatalf("node restored: %+v", h.node.restores)
	}

	res, err := h.c.RestoreDB(h.ctx, "hello", "staging", b.ID)
	if err != nil || res.Backup.ID != b.ID || !res.Stopped || res.TotalMS != 6 {
		t.Fatalf("restore = %+v %v", res, err)
	}
	if got := h.node.restores; len(got) != 1 || got[0] != (bundle.RestoreRequest{File: b.File, App: "hello", Env: "staging"}) {
		t.Errorf("node restores = %+v", got)
	}
	es, _ = h.c.Events(h.ctx, "hello", "staging", 100)
	if !hasEvent(es, "db.restored") {
		t.Error("no db.restored event")
	}

	// Restore takes the app-env deploy lock.
	err = h.store.WithLock(h.ctx, "hello", "staging", func(context.Context) error {
		_, err := h.c.RestoreDB(h.ctx, "hello", "staging", b.ID)
		wantCode(t, err, client.CodeConflict)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A backup taken on another host cannot be restored after a move.
	row, _ := h.store.GetResource(h.ctx, "hello", "staging", store.ResourceDatabase)
	h.store.PutHost(h.ctx, store.Host{Name: "m2", Addr: h.node.addr(), TokenEnc: []byte("x")})
	row.Host = "m2"
	h.store.PutResource(h.ctx, row)
	_, err = h.c.RestoreDB(h.ctx, "hello", "staging", b.ID)
	wantCode(t, err, client.CodeInvalid)
}

func TestBackupStatus(t *testing.T) {
	h := setup(t)
	h.readyResources()
	h.deployOK("hello", "staging")
	h.deployOK("hello", "prod")
	ok, _ := h.c.BackupDB(h.ctx, "hello", "staging")
	h.node.set(func(n *fakeNode) { n.failBackup = true })
	h.c.BackupDB(h.ctx, "hello", "prod")

	st, err := h.c.BackupStatus(h.ctx)
	if err != nil || len(st.Databases) != 2 {
		t.Fatalf("status = %+v %v", st, err)
	}
	prod, staging := st.Databases[0], st.Databases[1]
	if prod.Env != "prod" || prod.Latest != nil || prod.LastFailure == nil || prod.Database != "hello_prod" {
		t.Errorf("prod = %+v", prod)
	}
	if staging.Latest == nil || staging.Latest.ID != ok.ID || staging.LastFailure != nil {
		t.Errorf("staging = %+v", staging)
	}
	if len(st.Failures) != 1 || st.Failures[0].Env != "prod" {
		t.Errorf("failures = %+v", st.Failures)
	}
}

func TestScheduledBackups(t *testing.T) {
	h := setup(t)
	h.readyResources()
	h.deployOK("hello", "staging")
	h.deployOK("hello", "prod")
	ctx := context.Background()
	day := time.Date(2026, 10, 3, 0, 0, 0, 0, time.Local)

	h.ctrl.RunScheduledBackups(ctx, day.Add(2*time.Hour+59*time.Minute), 3)
	if h.node.backups != 0 {
		t.Fatalf("backed up before the hour: %d", h.node.backups)
	}
	// Rows are compared with started_at, which is the real clock; use now-relative times.
	now := time.Now()
	hour := now.Hour()
	h.ctrl.RunScheduledBackups(ctx, now, hour)
	if h.node.backups != 2 {
		t.Fatalf("backups = %d, want one per database", h.node.backups)
	}
	h.ctrl.RunScheduledBackups(ctx, now.Add(time.Minute), hour)
	if h.node.backups != 2 {
		t.Errorf("second run the same day backed up again (%d)", h.node.backups)
	}
	list, _ := h.c.DBBackups(h.ctx, "hello", "prod")
	if len(list) != 1 || list[0].Kind != store.BackupScheduled || list[0].Status != store.BackupSucceeded {
		t.Errorf("prod backups = %+v", list)
	}

	// Failures are recorded as failed rows with a backup.failed event.
	h2 := setup(t)
	h2.readyResources()
	h2.deployOK("hello", "staging")
	h2.node.set(func(n *fakeNode) { n.failBackup = true })
	h2.ctrl.RunScheduledBackups(ctx, now, hour)
	list, _ = h2.c.DBBackups(h2.ctx, "hello", "staging")
	if len(list) != 1 || list[0].Status != store.BackupFailed || list[0].Kind != store.BackupScheduled {
		t.Errorf("failed scheduled backup = %+v", list)
	}
	es, _ := h2.c.Events(h2.ctx, "hello", "staging", 100)
	if !hasEvent(es, "backup.failed") {
		t.Error("no backup.failed event")
	}
}
