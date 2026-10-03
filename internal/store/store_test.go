package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"lwd/internal/store"
	"lwd/internal/store/storetest"
)

var ctx = context.Background()

// seed creates host h1, app "hello" with environments staging and prod, and
// one release; it returns the release.
func seed(t *testing.T, s *store.Store) store.Release {
	t.Helper()
	if err := s.PutHost(ctx, store.Host{Name: "h1", Addr: "10.0.0.1:7480", TokenEnc: []byte("enc")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutApp(ctx, store.App{Name: "hello", Manifest: "m1"}, []store.Environment{
		{Name: "staging", Host: "h1", Domain: "s.example.com", TLS: "internal"},
		{Name: "prod", Host: "h1", Domain: "example.com", TLS: "acme"},
	}); err != nil {
		t.Fatal(err)
	}
	r := store.Release{App: "hello", Commit: "abc", Tag: "v1", Manifest: "m1", Images: map[string]string{"web": "r/web@sha256:1"}, Actor: "me"}
	if err := s.CreateRelease(ctx, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := storetest.New(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil || v < 1 {
		t.Fatalf("version = %d, %v", v, err)
	}
}

func TestHosts(t *testing.T) {
	s := storetest.New(t)
	if _, err := s.GetHost(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetHost missing = %v", err)
	}
	if err := s.PutHost(ctx, store.Host{Name: "h1", Addr: "a:1", TokenEnc: []byte("t1")}); err != nil {
		t.Fatal(err)
	}
	// Re-adding a host rotates its address and token.
	if err := s.PutHost(ctx, store.Host{Name: "h1", Addr: "a:2", TokenEnc: []byte("t2")}); err != nil {
		t.Fatal(err)
	}
	h, err := s.GetHost(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if h.Addr != "a:2" || string(h.TokenEnc) != "t2" || h.CreatedAt.IsZero() {
		t.Errorf("host = %+v", h)
	}
	if err := s.PutHost(ctx, store.Host{Name: "h0", Addr: "b:1", TokenEnc: []byte("t")}); err != nil {
		t.Fatal(err)
	}
	hs, err := s.ListHosts(ctx)
	if err != nil || len(hs) != 2 || hs[0].Name != "h0" {
		t.Fatalf("ListHosts = %+v, %v", hs, err)
	}
	// The encrypted token must never be serialised.
	b, _ := json.Marshal(h)
	if strings.Contains(string(b), "dDI") || strings.Contains(strings.ToLower(string(b)), "token") {
		t.Errorf("token leaked in JSON: %s", b)
	}
}

func TestAppsAndEnvironmentSync(t *testing.T) {
	s := storetest.New(t)
	rel := seed(t, s)

	envs, err := s.ListEnvironments(ctx, "hello")
	if err != nil || len(envs) != 2 || envs[0].Name != "prod" {
		t.Fatalf("envs = %+v, %v", envs, err)
	}

	// A deployment references staging, so dropping both environments from the
	// manifest only removes prod.
	d := store.Deployment{App: "hello", Env: "staging", Release: rel.ID, Host: "h1", Kind: "deploy", Actor: "me"}
	if err := s.CreateDeployment(ctx, &d); err != nil {
		t.Fatal(err)
	}
	kept, err := s.PutApp(ctx, store.App{Name: "hello", Manifest: "m2"}, []store.Environment{
		{Name: "dev", Host: "h1", Domain: "d.example.com", TLS: "acme"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0] != "staging" {
		t.Errorf("kept = %v", kept)
	}
	envs, _ = s.ListEnvironments(ctx, "hello")
	var names []string
	for _, e := range envs {
		names = append(names, e.Name)
	}
	if len(names) != 2 || names[0] != "dev" || names[1] != "staging" {
		t.Errorf("env names = %v", names)
	}

	a, err := s.GetApp(ctx, "hello")
	if err != nil || a.Manifest != "m2" || !a.UpdatedAt.After(a.CreatedAt) && !a.UpdatedAt.Equal(a.CreatedAt) {
		t.Fatalf("app = %+v, %v", a, err)
	}
	if _, err := s.GetApp(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp missing = %v", err)
	}
	apps, err := s.ListApps(ctx)
	if err != nil || len(apps) != 1 {
		t.Errorf("apps = %+v, %v", apps, err)
	}
	if _, err := s.GetEnvironment(ctx, "hello", "prod"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("prod should be gone: %v", err)
	}
	e, err := s.GetEnvironment(ctx, "hello", "dev")
	if err != nil || e.Domain != "d.example.com" {
		t.Errorf("dev = %+v, %v", e, err)
	}
}

func TestPutAppUnknownHost(t *testing.T) {
	s := storetest.New(t)
	_, err := s.PutApp(ctx, store.App{Name: "a", Manifest: "m"}, []store.Environment{{Name: "p", Host: "ghost", Domain: "x.y", TLS: "acme"}})
	if !errors.Is(err, store.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestReleases(t *testing.T) {
	s := storetest.New(t)
	r1 := seed(t, s)
	if r1.ID == 0 || r1.CreatedAt.IsZero() {
		t.Fatalf("release = %+v", r1)
	}
	r2 := store.Release{App: "hello", Tag: "v2", Manifest: "m1", Images: map[string]string{"web": "r/web@sha256:2"}}
	if err := s.CreateRelease(ctx, &r2); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestRelease(ctx, "hello")
	if err != nil || latest.ID != r2.ID || latest.Images["web"] != "r/web@sha256:2" {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	got, err := s.GetRelease(ctx, "hello", r1.ID)
	if err != nil || got.Commit != "abc" || got.Manifest != "m1" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := s.GetRelease(ctx, "other", r1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("release of another app must be not found, got %v", err)
	}
	list, err := s.ListReleases(ctx, "hello", 10)
	if err != nil || len(list) != 2 || list[0].ID != r2.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := s.LatestRelease(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("latest missing = %v", err)
	}
}

func TestDeploymentsLiveAndRollbackTarget(t *testing.T) {
	s := storetest.New(t)
	r1 := seed(t, s)
	r2 := store.Release{App: "hello", Tag: "v2", Manifest: "m1", Images: map[string]string{}}
	if err := s.CreateRelease(ctx, &r2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LiveDeployment(ctx, "hello", "prod"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("live before any deploy = %v", err)
	}

	deploy := func(rel int64, status string) store.Deployment {
		t.Helper()
		d := store.Deployment{App: "hello", Env: "prod", Release: rel, Host: "h1", Kind: "deploy", Actor: "me"}
		if err := s.CreateDeployment(ctx, &d); err != nil {
			t.Fatal(err)
		}
		if d.Status != "running" {
			t.Fatalf("new deployment status = %q", d.Status)
		}
		if err := s.FinishDeployment(ctx, d.ID, status, "done", "msg", json.RawMessage(`{"status":"`+status+`"}`)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	deploy(r1.ID, "succeeded")
	deploy(r2.ID, "succeeded")
	deploy(r2.ID, "succeeded")
	last := deploy(r1.ID, "reverted")

	live, err := s.LiveDeployment(ctx, "hello", "prod")
	if err != nil || live.Release != r2.ID {
		t.Fatalf("live = %+v, %v", live, err)
	}
	target, err := s.RollbackTarget(ctx, "hello", "prod", live.Release)
	if err != nil || target.Release != r1.ID {
		t.Fatalf("target = %+v, %v", target, err)
	}
	if _, err := s.RollbackTarget(ctx, "hello", "staging", 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("staging target = %v", err)
	}

	got, err := s.GetDeployment(ctx, last.ID)
	if err != nil || got.Status != "reverted" || got.FinishedAt == nil || string(got.Result) != `{"status": "reverted"}` && string(got.Result) != `{"status":"reverted"}` {
		t.Fatalf("got = %+v, %v (result %s)", got, err, got.Result)
	}
	list, err := s.ListDeployments(ctx, "hello", "prod", 3)
	if err != nil || len(list) != 3 || list[0].ID != last.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	ld, err := s.LastDeployment(ctx, "hello", "prod")
	if err != nil || ld.ID != last.ID {
		t.Fatalf("last = %+v, %v", ld, err)
	}
}

func TestFailInterrupted(t *testing.T) {
	s := storetest.New(t)
	r := seed(t, s)
	d := store.Deployment{App: "hello", Env: "prod", Release: r.ID, Host: "h1", Kind: "deploy"}
	if err := s.CreateDeployment(ctx, &d); err != nil {
		t.Fatal(err)
	}
	n, err := s.FailInterrupted(ctx)
	if err != nil || n != 1 {
		t.Fatalf("n = %d, %v", n, err)
	}
	got, _ := s.GetDeployment(ctx, d.ID)
	if got.Status != "failed" || got.FinishedAt == nil {
		t.Errorf("got = %+v", got)
	}
}

func TestLockIsExclusivePerAppEnv(t *testing.T) {
	s := storetest.New(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error)
	go func() {
		done <- s.WithLock(ctx, "hello", "prod", func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	if err := s.WithLock(ctx, "hello", "prod", func(context.Context) error { return nil }); !errors.Is(err, store.ErrBusy) {
		t.Errorf("second lock = %v, want ErrBusy", err)
	}
	// A different environment is independent.
	if err := s.WithLock(ctx, "hello", "staging", func(context.Context) error { return nil }); err != nil {
		t.Errorf("other env lock = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Released: can take it again, and fn's error is returned.
	sentinel := errors.New("boom")
	if err := s.WithLock(ctx, "hello", "prod", func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("relock = %v", err)
	}
}

func TestSecrets(t *testing.T) {
	s := storetest.New(t)
	seed(t, s)
	v, err := s.SetSecret(ctx, "hello", "prod", "A", []byte("a1"))
	if err != nil || v != 1 {
		t.Fatalf("set = %d, %v", v, err)
	}
	if v, _ = s.SetSecret(ctx, "hello", "prod", "A", []byte("a2")); v != 2 {
		t.Fatalf("second set version = %d", v)
	}
	if _, err := s.SetSecret(ctx, "hello", "prod", "B", []byte("b1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetSecret(ctx, "hello", "staging", "C", []byte("c1")); err != nil {
		t.Fatal(err)
	}
	vals, err := s.SecretValues(ctx, "hello", "prod")
	if err != nil || len(vals) != 2 || string(vals["A"]) != "a2" || string(vals["B"]) != "b1" {
		t.Fatalf("values = %q, %v", vals, err)
	}
	if err := s.DeleteSecret(ctx, "hello", "prod", "B"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecret(ctx, "hello", "prod", "B"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("double delete = %v", err)
	}
	list, err := s.ListSecrets(ctx, "hello", "prod")
	if err != nil || len(list) != 1 || list[0].Key != "A" || list[0].Version != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// Setting again after delete continues the version sequence.
	if v, _ = s.SetSecret(ctx, "hello", "prod", "B", []byte("b2")); v != 3 {
		t.Errorf("version after delete = %d, want 3", v)
	}
}

func TestEvents(t *testing.T) {
	s := storetest.New(t)
	seed(t, s)
	for i, k := range []string{"a", "b", "c"} {
		env := "prod"
		if i == 2 {
			env = "staging"
		}
		if err := s.AddEvent(ctx, store.Event{App: "hello", Env: env, Kind: k, Message: k, Data: json.RawMessage(`{"i":1}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddEvent(ctx, store.Event{Kind: "host.added"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListEvents(ctx, "", "", 10)
	if err != nil || len(all) != 4 || all[0].Kind != "host.added" {
		t.Fatalf("all = %+v, %v", all, err)
	}
	prod, _ := s.ListEvents(ctx, "hello", "prod", 10)
	if len(prod) != 2 || prod[0].Kind != "b" {
		t.Fatalf("prod = %+v", prod)
	}
	app, _ := s.ListEvents(ctx, "hello", "", 1)
	if len(app) != 1 || app[0].Kind != "c" || app[0].At.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("app = %+v", app)
	}
}
