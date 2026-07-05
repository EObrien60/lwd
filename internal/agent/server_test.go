package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lwd/internal/node"
)

const testToken = "s3cr3t-token"

func newTestServer(fake *node.Fake) http.Handler {
	return NewServer(fake, testToken).Handler()
}

func doReq(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRun_ValidToken_CallsFakeAndReturnsContainer(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathRun, testToken, RunRequest{
		Spec: node.RunSpec{Name: "web1", Image: "nginx:latest"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp RunResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Container.Name != "web1" || resp.Container.Image != "nginx:latest" {
		t.Fatalf("unexpected container: %+v", resp.Container)
	}
	found := false
	for _, c := range fake.Calls {
		if strings.HasPrefix(c, "RunContainer:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("fake.Calls did not record RunContainer; got %v", fake.Calls)
	}
}

func TestRun_MissingToken_Returns401(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathRun, "", RunRequest{Spec: node.RunSpec{Name: "web1"}})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var errResp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error == "" {
		t.Fatalf("expected non-empty error message")
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("fake should not have been called; got %v", fake.Calls)
	}
}

func TestRun_WrongToken_Returns401(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathRun, "wrong-token", RunRequest{Spec: node.RunSpec{Name: "web1"}})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("fake should not have been called; got %v", fake.Calls)
	}
}

func TestHealthz_NoAuthRequired_HealthyReturns200(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodGet, PathHealthz, "", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHealthz_NoAuthRequired_UnhealthyReturns503(t *testing.T) {
	fake := node.NewFake()
	fake.PingErr = errors.New("docker unreachable")
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodGet, PathHealthz, "", nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

func TestEnsureNetwork_Delegates(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathEnsureNetwork, testToken, EnsureNetworkRequest{Name: "lwd-net"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, c := range fake.Calls {
		if c == "EnsureNetwork:lwd-net" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fake.Calls did not record EnsureNetwork:lwd-net; got %v", fake.Calls)
	}
}

func TestImagePresent_Delegates(t *testing.T) {
	fake := node.NewFake()
	fake.Images = map[string]bool{"nginx:latest": true}
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathImagePresent, testToken, ImagePresentRequest{Ref: "nginx:latest"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp ImagePresentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Present {
		t.Fatalf("expected Present=true")
	}
}

func TestRemove_Delegates(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathRemove, testToken, RemoveRequest{ID: "abc123"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, c := range fake.Calls {
		if c == "RemoveContainer:abc123" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fake.Calls did not record RemoveContainer:abc123; got %v", fake.Calls)
	}
}

func TestContainerHealth_Delegates(t *testing.T) {
	fake := node.NewFake()
	fake.HealthState = "running"
	fake.DockerHealth = "healthy"
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathContainerHealth, testToken, ContainerHealthRequest{ID: "abc123"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp ContainerHealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.State != "running" || resp.DockerHealth != "healthy" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestRun_NodeError_Returns500WithErrorResponse(t *testing.T) {
	fake := node.NewFake()
	fake.RunErr = errors.New("boom")
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathRun, testToken, RunRequest{Spec: node.RunSpec{Name: "web1"}})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(errResp.Error, "boom") {
		t.Fatalf("expected error to mention 'boom'; got %q", errResp.Error)
	}
}

func TestRun_BadJSON_Returns400(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	req := httptest.NewRequest(http.MethodPost, PathRun, strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestLogs_StreamsBody(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	req := httptest.NewRequest(http.MethodGet, PathLogs+"?id=abc123&follow=false", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fake log line") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
	found := false
	for _, c := range fake.Calls {
		if c == "ContainerLogs:abc123" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fake.Calls did not record ContainerLogs:abc123; got %v", fake.Calls)
	}
}

func TestSave_StreamsBody(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	req := httptest.NewRequest(http.MethodGet, PathSave+"?ref=nginx:latest", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("expected non-empty tar stream body")
	}
}

func TestLoad_ReadsBodyAsTar(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	req := httptest.NewRequest(http.MethodPost, PathLoad, strings.NewReader("fake-tar-bytes"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !fake.Loaded {
		t.Fatalf("expected fake.Loaded=true")
	}
	if string(fake.LastLoaded) != "fake-tar-bytes" {
		t.Fatalf("unexpected loaded bytes: %q", fake.LastLoaded)
	}
}

func TestList_Delegates(t *testing.T) {
	fake := node.NewFake()
	fake.RunErr = nil
	h := newTestServer(fake)

	// seed one container via /run
	doReq(t, h, http.MethodPost, PathRun, testToken, RunRequest{Spec: node.RunSpec{Name: "web1", Labels: map[string]string{"app": "web"}}})

	rec := doReq(t, h, http.MethodPost, PathList, testToken, ListRequest{Labels: map[string]string{"app": "web"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp ListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Containers) != 1 || resp.Containers[0].Name != "web1" {
		t.Fatalf("unexpected containers: %+v", resp.Containers)
	}
}

func TestConnectNetwork_Delegates(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathConnectNetwork, testToken, ConnectNetworkRequest{ContainerID: "abc123", Network: "lwd-net"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, c := range fake.Calls {
		if c == "ConnectContainerToNetwork:abc123:lwd-net" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fake.Calls did not record ConnectContainerToNetwork; got %v", fake.Calls)
	}
}

func TestEnsureImage_Delegates(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathEnsureImage, testToken, EnsureImageRequest{Ref: "nginx:latest"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, c := range fake.Calls {
		if c == "EnsureImage:nginx:latest" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fake.Calls did not record EnsureImage:nginx:latest; got %v", fake.Calls)
	}
}

func TestHealth_Delegates(t *testing.T) {
	fake := node.NewFake()
	h := newTestServer(fake)

	rec := doReq(t, h, http.MethodPost, PathHealth, testToken, HealthCheckRequest{
		Container: node.Container{ID: "abc123"},
		Health:    node.HealthSpec{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, c := range fake.Calls {
		if c == "Health:abc123" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fake.Calls did not record Health:abc123; got %v", fake.Calls)
	}
}
