package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"lwd/internal/store"
	"lwd/internal/store/storetest"
)

func TestResources(t *testing.T) {
	s := storetest.New(t)
	seed(t, s)
	if _, err := s.GetResource(ctx, "hello", "staging", store.ResourceDatabase); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	r := store.Resource{App: "hello", Env: "staging", Kind: store.ResourceDatabase, Host: "h1", Name: "hello_staging", Credentials: []byte("enc1")}
	if err := s.PutResource(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetResource(ctx, "hello", "staging", store.ResourceDatabase)
	if err != nil || got.Name != "hello_staging" || got.Host != "h1" || string(got.Credentials) != "enc1" || got.CreatedAt.IsZero() {
		t.Fatalf("get = %+v %v", got, err)
	}
	// Upsert keeps created_at and replaces host/credentials.
	time.Sleep(5 * time.Millisecond)
	r.Credentials = []byte("enc2")
	if err := s.PutResource(ctx, r); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetResource(ctx, "hello", "staging", store.ResourceDatabase)
	if string(got2.Credentials) != "enc2" || !got2.CreatedAt.Equal(got.CreatedAt) || !got2.UpdatedAt.After(got.UpdatedAt) {
		t.Errorf("upsert = %+v", got2)
	}
	// Another app-env may not take the same name.
	clash := store.Resource{App: "hello", Env: "prod", Kind: store.ResourceDatabase, Host: "h1", Name: "hello_staging", Credentials: []byte("x")}
	if err := s.PutResource(ctx, clash); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("duplicate name = %v", err)
	}
	if err := s.PutResource(ctx, store.Resource{App: "hello", Env: "staging", Kind: "queue", Host: "h1", Name: "q", Credentials: []byte("x")}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("bad kind = %v", err)
	}
	b := store.Resource{App: "hello", Env: "staging", Kind: store.ResourceBucket, Host: "h1", Name: "hello-staging", Credentials: []byte("x")}
	if err := s.PutResource(ctx, b); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListResources(ctx, "hello", "staging")
	if err != nil || len(list) != 2 || list[0].Kind != store.ResourceBucket || list[1].Kind != store.ResourceDatabase {
		t.Fatalf("list = %+v %v", list, err)
	}
	all, err := s.ListResources(ctx, "", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %+v %v", all, err)
	}
	// Credentials never serialise.
	data, _ := json.Marshal(list)
	if strings.Contains(string(data), "enc2") || strings.Contains(string(data), "credentials") {
		t.Errorf("credentials in JSON: %s", data)
	}
}

func TestBackups(t *testing.T) {
	s := storetest.New(t)
	seed(t, s)
	t0 := time.Now().Add(-time.Hour)
	add := func(env, kind, status string, at time.Time) store.Backup {
		t.Helper()
		fin := at.Add(time.Second)
		b := store.Backup{App: "hello", Env: env, Host: "h1", Database: "hello_" + env, Kind: kind, Status: status, StartedAt: at, FinishedAt: &fin}
		if status == store.BackupSucceeded {
			b.File, b.Bytes, b.SHA256 = at.UTC().Format("20060102T150405.000Z")+".dump", 10, "ab"
		} else {
			b.Error = "boom"
		}
		if err := s.AddBackup(ctx, &b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	b1 := add("staging", store.BackupManual, store.BackupSucceeded, t0)
	b2 := add("staging", store.BackupScheduled, store.BackupFailed, t0.Add(time.Minute))
	b3 := add("staging", store.BackupScheduled, store.BackupSucceeded, t0.Add(2*time.Minute))
	add("prod", store.BackupScheduled, store.BackupSucceeded, t0)
	if b1.ID == 0 || b2.ID <= b1.ID {
		t.Fatalf("ids %d %d", b1.ID, b2.ID)
	}
	got, err := s.GetBackup(ctx, b1.ID)
	if err != nil || got.File != b1.File || got.Kind != store.BackupManual || got.FinishedAt == nil {
		t.Fatalf("get = %+v %v", got, err)
	}
	list, err := s.ListBackups(ctx, "hello", "staging", 10)
	if err != nil || len(list) != 3 || list[0].ID != b3.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
	latest, err := s.LatestBackup(ctx, "hello", "staging", store.BackupSucceeded)
	if err != nil || latest.ID != b3.ID {
		t.Errorf("latest ok = %+v %v", latest, err)
	}
	lf, err := s.LatestBackup(ctx, "hello", "staging", store.BackupFailed)
	if err != nil || lf.ID != b2.ID || lf.Error != "boom" {
		t.Errorf("latest failed = %+v %v", lf, err)
	}
	if _, err := s.LatestBackup(ctx, "hello", "prod", store.BackupFailed); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no failure = %v", err)
	}
	since, err := s.ScheduledBackupSince(ctx, "hello", "staging", t0.Add(90*time.Second))
	if err != nil || !since {
		t.Errorf("scheduled since = %v %v", since, err)
	}
	since, _ = s.ScheduledBackupSince(ctx, "hello", "staging", t0.Add(3*time.Minute))
	if since {
		t.Error("no scheduled backup after t0+3m")
	}
	fails, err := s.FailedBackupsSince(ctx, t0.Add(-time.Minute))
	if err != nil || len(fails) != 1 || fails[0].ID != b2.ID {
		t.Errorf("failures = %+v %v", fails, err)
	}
	if err := s.AddBackup(ctx, &store.Backup{App: "hello", Env: "staging", Host: "h1", Database: "d", Kind: "weekly", Status: store.BackupFailed, StartedAt: t0}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("bad kind = %v", err)
	}
}
