package node

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// roundTrip marshals v to JSON, unmarshals into a fresh zero value of the
// same type, and returns the decoded value for comparison.
func roundTrip[T any](t *testing.T, v T) T {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v (data=%s)", err, data)
	}
	return out
}

func TestRunRequestRoundTrip(t *testing.T) {
	req := RunRequest{
		Spec: RunSpec{
			Name:    "x",
			Image:   "i",
			Port:    80,
			Network: "lwd",
			Env:     map[string]string{"A": "b"},
			Labels:  map[string]string{"lwd.app": "x"},
			Publish: []PortMapping{
				{HostIP: "10.0.0.1", HostPort: 0, ContainerPort: 80},
			},
			Cmd: []string{"echo", "hi"},
		},
	}
	got := roundTrip(t, req)
	if !reflect.DeepEqual(req, got) {
		t.Fatalf("RunRequest round-trip mismatch:\n original=%+v\n decoded =%+v", req, got)
	}
}

func TestRunResponseRoundTrip(t *testing.T) {
	resp := RunResponse{
		Container: Container{
			ID:       "abc123",
			Name:     "x",
			Image:    "i",
			State:    "running",
			Labels:   map[string]string{"lwd.app": "x"},
			HostPort: 8080,
			IP:       "172.17.0.2",
		},
	}
	got := roundTrip(t, resp)
	if !reflect.DeepEqual(resp, got) {
		t.Fatalf("RunResponse round-trip mismatch:\n original=%+v\n decoded =%+v", resp, got)
	}
}

func TestImagePresentResponseRoundTrip(t *testing.T) {
	resp := ImagePresentResponse{Present: true}
	got := roundTrip(t, resp)
	if !reflect.DeepEqual(resp, got) {
		t.Fatalf("ImagePresentResponse round-trip mismatch:\n original=%+v\n decoded =%+v", resp, got)
	}
}

func TestHealthCheckRequestRoundTrip(t *testing.T) {
	req := HealthCheckRequest{
		Container: Container{
			ID:       "abc123",
			Name:     "x",
			Image:    "i",
			State:    "running",
			Labels:   map[string]string{"lwd.app": "x"},
			HostPort: 8080,
			IP:       "172.17.0.2",
		},
		Health: HealthSpec{
			Path:    "/healthz",
			Timeout: 5 * time.Second,
		},
	}
	got := roundTrip(t, req)
	if !reflect.DeepEqual(req, got) {
		t.Fatalf("HealthCheckRequest round-trip mismatch:\n original=%+v\n decoded =%+v", req, got)
	}
	if got.Health.Timeout != 5*time.Second {
		t.Fatalf("Timeout not preserved: got %v", got.Health.Timeout)
	}
}

func TestRemainingTypesRoundTrip(t *testing.T) {
	if got := roundTrip(t, RemoveRequest{ID: "abc"}); !reflect.DeepEqual(RemoveRequest{ID: "abc"}, got) {
		t.Fatalf("RemoveRequest round-trip mismatch: %+v", got)
	}
	listReq := ListRequest{Labels: map[string]string{"lwd.app": "x"}}
	if got := roundTrip(t, listReq); !reflect.DeepEqual(listReq, got) {
		t.Fatalf("ListRequest round-trip mismatch: %+v", got)
	}
	listResp := ListResponse{Containers: []Container{
		{ID: "1", Name: "a", Image: "i", State: "running", Labels: map[string]string{"k": "v"}, HostPort: 1, IP: "1.2.3.4"},
	}}
	if got := roundTrip(t, listResp); !reflect.DeepEqual(listResp, got) {
		t.Fatalf("ListResponse round-trip mismatch: %+v", got)
	}
	if got := roundTrip(t, EnsureImageRequest{Ref: "img"}); !reflect.DeepEqual(EnsureImageRequest{Ref: "img"}, got) {
		t.Fatalf("EnsureImageRequest round-trip mismatch: %+v", got)
	}
	if got := roundTrip(t, ImagePresentRequest{Ref: "img"}); !reflect.DeepEqual(ImagePresentRequest{Ref: "img"}, got) {
		t.Fatalf("ImagePresentRequest round-trip mismatch: %+v", got)
	}
	if got := roundTrip(t, EnsureNetworkRequest{Name: "net"}); !reflect.DeepEqual(EnsureNetworkRequest{Name: "net"}, got) {
		t.Fatalf("EnsureNetworkRequest round-trip mismatch: %+v", got)
	}
	connReq := ConnectNetworkRequest{ContainerID: "c1", Network: "net"}
	if got := roundTrip(t, connReq); !reflect.DeepEqual(connReq, got) {
		t.Fatalf("ConnectNetworkRequest round-trip mismatch: %+v", got)
	}
	if got := roundTrip(t, ContainerHealthRequest{ID: "c1"}); !reflect.DeepEqual(ContainerHealthRequest{ID: "c1"}, got) {
		t.Fatalf("ContainerHealthRequest round-trip mismatch: %+v", got)
	}
	chResp := ContainerHealthResponse{State: "running", DockerHealth: "healthy"}
	if got := roundTrip(t, chResp); !reflect.DeepEqual(chResp, got) {
		t.Fatalf("ContainerHealthResponse round-trip mismatch: %+v", got)
	}
	errResp := ErrorResponse{Error: "boom"}
	if got := roundTrip(t, errResp); !reflect.DeepEqual(errResp, got) {
		t.Fatalf("ErrorResponse round-trip mismatch: %+v", got)
	}
}

func TestPathConstants(t *testing.T) {
	paths := map[string]string{
		PathHealthz:         "/healthz",
		PathReady:           "/ready",
		PathRun:             "/run",
		PathRemove:          "/remove",
		PathList:            "/list",
		PathEnsureImage:     "/ensure-image",
		PathImagePresent:    "/image-present",
		PathLoad:            "/load",
		PathSave:            "/save",
		PathLogs:            "/logs",
		PathEnsureNetwork:   "/ensure-network",
		PathConnectNetwork:  "/connect-network",
		PathContainerHealth: "/container-health",
		PathHealth:          "/health",
	}
	for got, want := range paths {
		if got != want {
			t.Errorf("path constant mismatch: got %q want %q", got, want)
		}
	}
}
