package nodeclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"lwd/internal/bundle"
)

func TestPlatformCalls(t *testing.T) {
	var seen []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path)
		bodies = append(bodies, string(b))
		switch r.Method + " " + r.URL.Path {
		case "PUT /v1/platform/databases/a_b":
			io.WriteString(w, `{"ok":true}`)
		case "PUT /v1/platform/buckets/a-b":
			json.NewEncoder(w).Encode(bundle.BucketCredentials{AccessKeyID: "GK1", SecretAccessKey: "s"})
		case "POST /v1/platform/databases/a_b/backup":
			json.NewEncoder(w).Encode(bundle.BackupFile{File: "f.dump", Bytes: 3, SHA256: "abc"})
		case "GET /v1/platform/databases/a_b/backups":
			json.NewEncoder(w).Encode([]bundle.BackupFile{{File: "f.dump"}})
		case "POST /v1/platform/databases/a_b/restore":
			json.NewEncoder(w).Encode(bundle.RestoreResult{File: "f.dump", Stopped: true, TotalMS: 5})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	ctx := context.Background()

	if err := c.ProvisionDatabase(ctx, "a_b", bundle.DatabaseRequest{Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	creds, err := c.ProvisionBucket(ctx, "a-b", bundle.BucketRequest{AccessKeyID: "GK0"})
	if err != nil || creds.AccessKeyID != "GK1" || creds.SecretAccessKey != "s" {
		t.Fatalf("bucket: %+v %v", creds, err)
	}
	bf, err := c.BackupDatabase(ctx, "a_b")
	if err != nil || bf.File != "f.dump" || bf.SHA256 != "abc" {
		t.Fatalf("backup: %+v %v", bf, err)
	}
	list, err := c.ListBackups(ctx, "a_b")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	res, err := c.RestoreDatabase(ctx, "a_b", bundle.RestoreRequest{File: "f.dump", App: "a", Env: "b"})
	if err != nil || !res.Stopped || res.TotalMS != 5 {
		t.Fatalf("restore: %+v %v", res, err)
	}
	if bodies[0] != `{"password":"pw"}` || bodies[1] != `{"access_key_id":"GK0"}` || bodies[4] != `{"file":"f.dump","app":"a","env":"b"}` {
		t.Errorf("bodies = %q", bodies)
	}
	if len(seen) != 5 {
		t.Errorf("seen = %q", seen)
	}
}
