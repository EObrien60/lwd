package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lwd/internal/api"
	"lwd/internal/spec"
	"lwd/internal/store"
)

func testServer(fd *fakeDaemon) (*Server, *Authenticator) {
	auth := NewAuthenticator([]byte("test-signing-key"), "test-password")
	return NewServer(fd, auth), auth
}

// authedRequest builds a request carrying a valid signed session cookie for
// auth, so it passes the Middleware's auth check.
func authedRequest(t *testing.T, auth *Authenticator, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: signSession(auth.key, time.Now().Add(time.Hour)),
	})
	return req
}

func do(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestApiApps(t *testing.T) {
	fd := newFakeDaemon()
	fd.apps = []api.AppStatus{
		{Name: "foo", Image: "img:1", Status: store.StatusRunning, Domain: "foo.example.com"},
	}
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/apps", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got []api.AppStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body)
	}
	if len(got) != 1 || got[0].Name != "foo" {
		t.Fatalf("got %+v", got)
	}
}

func TestApiAppDetail(t *testing.T) {
	fd := newFakeDaemon()
	fd.apps = []api.AppStatus{
		{Name: "foo", Status: store.StatusRunning},
		{Name: "bar", Status: store.StatusRunning},
	}
	fd.history["foo"] = []store.Deployment{
		{ID: 2, App: "foo", Image: "img:2"},
		{ID: 1, App: "foo", Image: "img:1"},
	}
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/apps/foo", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got appDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body)
	}
	if got.Status == nil || got.Status.Name != "foo" {
		t.Fatalf("status = %+v", got.Status)
	}
	if len(got.History) != 2 || got.History[0].Image != "img:2" {
		t.Fatalf("history = %+v", got.History)
	}
}

func TestApiAppDetailUnknownApp(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	rec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/apps/ghost", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got appDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body)
	}
	if got.Status != nil {
		t.Fatalf("status = %+v, want nil", got.Status)
	}
	if len(got.History) != 0 {
		t.Fatalf("history = %+v, want empty", got.History)
	}
}

const validToml = `
name = "myapp"
image = "nginx:latest"
port = 80
domain = "myapp.example.com"
`

func TestApiApplyParsesToml(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	req := authedRequest(t, auth, http.MethodPost, "/api/apply", strings.NewReader(validToml))
	rec := do(srv, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(fd.applied) != 1 {
		t.Fatalf("applied count = %d, want 1", len(fd.applied))
	}
	got := fd.applied[0]
	if got.Name != "myapp" || got.Image != "nginx:latest" || got.Port != 80 {
		t.Fatalf("applied = %+v", got)
	}
}

func TestApiApplyRejectsBadToml(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	req := authedRequest(t, auth, http.MethodPost, "/api/apply", strings.NewReader("not [ valid toml {{ ==="))
	rec := do(srv, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(fd.applied) != 0 {
		t.Fatalf("Apply should not have been called, got %+v", fd.applied)
	}
}

func TestApiApplyRejectsBadSpec(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	// Valid TOML, but missing image/port: fails spec.Validate.
	req := authedRequest(t, auth, http.MethodPost, "/api/apply", strings.NewReader(`name = "myapp"`))
	rec := do(srv, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(fd.applied) != 0 {
		t.Fatalf("Apply should not have been called, got %+v", fd.applied)
	}
}

func TestApiRollback(t *testing.T) {
	fd := newFakeDaemon()
	fd.rollbackResult = &store.Deployment{App: "foo", Image: "img:1", Status: store.StatusRunning}
	srv, auth := testServer(fd)

	req := authedRequest(t, auth, http.MethodPost, "/api/apps/foo/rollback", nil)
	rec := do(srv, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var dep store.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &dep); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body)
	}
	if dep.Image != "img:1" {
		t.Fatalf("dep = %+v", dep)
	}
}

func TestApiRollbackError(t *testing.T) {
	fd := newFakeDaemon()
	fd.rollbackErr = errString("no previous deployment")
	srv, auth := testServer(fd)

	req := authedRequest(t, auth, http.MethodPost, "/api/apps/foo/rollback", nil)
	rec := do(srv, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestApiRedeploy(t *testing.T) {
	appJSON, err := json.Marshal(&spec.App{Name: "foo", Image: "img:2", Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	fd := newFakeDaemon()
	fd.history["foo"] = []store.Deployment{
		{ID: 2, App: "foo", Image: "img:2", Spec: string(appJSON)},
		{ID: 1, App: "foo", Image: "img:1"},
	}
	srv, auth := testServer(fd)

	req := authedRequest(t, auth, http.MethodPost, "/api/apps/foo/redeploy", nil)
	rec := do(srv, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(fd.applied) != 1 {
		t.Fatalf("applied count = %d, want 1", len(fd.applied))
	}
	if fd.applied[0].Image != "img:2" {
		t.Fatalf("applied = %+v, want newest (img:2)", fd.applied[0])
	}
}

func TestApiRedeployEmptyHistory(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	req := authedRequest(t, auth, http.MethodPost, "/api/apps/foo/redeploy", nil)
	rec := do(srv, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(fd.applied) != 0 {
		t.Fatalf("Apply should not have been called, got %+v", fd.applied)
	}
}

func TestApiDelete(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	req := authedRequest(t, auth, http.MethodDelete, "/api/apps/foo", nil)
	rec := do(srv, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(fd.removed) != 1 || fd.removed[0] != "foo" {
		t.Fatalf("removed = %+v", fd.removed)
	}
}

func TestApiSecrets(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	setReq := authedRequest(t, auth, http.MethodPost, "/api/apps/foo/secrets",
		strings.NewReader("key=API_KEY&value=super-secret-value"))
	setReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setRec := do(srv, setReq)
	if setRec.Code != http.StatusNoContent {
		t.Fatalf("set status = %d, body = %s", setRec.Code, setRec.Body)
	}

	listRec := do(srv, authedRequest(t, auth, http.MethodGet, "/api/apps/foo/secrets", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRec.Code, listRec.Body)
	}
	if strings.Contains(listRec.Body.String(), "super-secret-value") {
		t.Fatalf("response leaked secret value: %s", listRec.Body)
	}
	var names []string
	if err := json.Unmarshal(listRec.Body.Bytes(), &names); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, listRec.Body)
	}
	if len(names) != 1 || names[0] != "API_KEY" {
		t.Fatalf("names = %+v", names)
	}

	delRec := do(srv, authedRequest(t, auth, http.MethodDelete, "/api/apps/foo/secrets/API_KEY", nil))
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", delRec.Code, delRec.Body)
	}

	listRec2 := do(srv, authedRequest(t, auth, http.MethodGet, "/api/apps/foo/secrets", nil))
	var names2 []string
	if err := json.Unmarshal(listRec2.Body.Bytes(), &names2); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, listRec2.Body)
	}
	if len(names2) != 0 {
		t.Fatalf("names2 = %+v, want empty after delete", names2)
	}
}

func TestApiSecretsRejectsEmptyKey(t *testing.T) {
	fd := newFakeDaemon()
	srv, auth := testServer(fd)

	req := authedRequest(t, auth, http.MethodPost, "/api/apps/foo/secrets", strings.NewReader("key=&value=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := do(srv, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestApiRequiresAuth(t *testing.T) {
	fd := newFakeDaemon()
	srv, _ := testServer(fd)

	req := httptest.NewRequest(http.MethodGet, "/api/apps", nil)
	rec := do(srv, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

// errString is a trivial error type for tests.
type errString string

func (e errString) Error() string { return string(e) }
