// Package node_test exercises AgentNode against a REAL agent.Server wrapping
// a node.Fake, so the HTTP client and server are tested together against the
// shared wire contract. This file lives in an external test package
// (node_test) rather than node: internal/agent imports internal/node, so an
// in-package node test file that also imports internal/agent would create an
// import cycle.
package node_test

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lwd/internal/agent"
	"lwd/internal/node"
)

const testToken = "tok-abc123"

func newTestServer(t *testing.T, fake *node.Fake) (*node.AgentNode, *node.Fake) {
	t.Helper()
	srv := httptest.NewServer(agent.NewServer(fake, testToken).Handler())
	t.Cleanup(srv.Close)
	return node.NewAgentNode(srv.URL, testToken), fake
}

func TestAgentNode_Ping(t *testing.T) {
	fake := node.NewFake()
	an, _ := newTestServer(t, fake)

	if err := an.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	fake.PingErr = context.DeadlineExceeded
	if err := an.Ping(context.Background()); err == nil {
		t.Fatal("expected error when fake.PingErr set, got nil")
	}
}

func TestAgentNode_RunContainer_RoundTrip(t *testing.T) {
	fake := node.NewFake()
	an, fake := newTestServer(t, fake)

	spec := node.RunSpec{
		Name:  "web-1",
		Image: "nginx:latest",
		Env:   map[string]string{"FOO": "bar"},
		Port:  80,
	}
	c, err := an.RunContainer(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if c.Name != "web-1" || c.Image != "nginx:latest" || c.State != "running" {
		t.Fatalf("unexpected container: %+v", c)
	}
	if fake.LastRunSpec.Name != "web-1" || fake.LastRunSpec.Env["FOO"] != "bar" {
		t.Fatalf("fake did not record spec correctly: %+v", fake.LastRunSpec)
	}
	found := false
	for _, call := range fake.Calls {
		if call == "RunContainer:web-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fake.Calls missing RunContainer: %v", fake.Calls)
	}
}

func TestAgentNode_ImagePresent(t *testing.T) {
	fake := node.NewFake()
	fake.Images = map[string]bool{"present:latest": true}
	an, _ := newTestServer(t, fake)

	present, err := an.ImagePresent(context.Background(), "present:latest")
	if err != nil {
		t.Fatalf("ImagePresent: %v", err)
	}
	if !present {
		t.Fatal("expected present:latest to be present")
	}

	absent, err := an.ImagePresent(context.Background(), "absent:latest")
	if err != nil {
		t.Fatalf("ImagePresent: %v", err)
	}
	if absent {
		t.Fatal("expected absent:latest to be absent")
	}
}

func TestAgentNode_EnsureImageAndNetwork(t *testing.T) {
	fake := node.NewFake()
	an, fake := newTestServer(t, fake)

	if err := an.EnsureImage(context.Background(), "nginx:latest"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	if err := an.EnsureNetwork(context.Background(), "lwd-net"); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	wantCalls := map[string]bool{"EnsureImage:nginx:latest": false, "EnsureNetwork:lwd-net": false}
	for _, c := range fake.Calls {
		if _, ok := wantCalls[c]; ok {
			wantCalls[c] = true
		}
	}
	for c, ok := range wantCalls {
		if !ok {
			t.Fatalf("expected call %q not recorded: %v", c, fake.Calls)
		}
	}
}

func TestAgentNode_ConnectContainerToNetwork(t *testing.T) {
	fake := node.NewFake()
	an, fake := newTestServer(t, fake)

	if err := an.ConnectContainerToNetwork(context.Background(), "c1", "lwd-net"); err != nil {
		t.Fatalf("ConnectContainerToNetwork: %v", err)
	}
	found := false
	for _, c := range fake.Calls {
		if c == "ConnectContainerToNetwork:c1:lwd-net" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fake.Calls missing ConnectContainerToNetwork: %v", fake.Calls)
	}
}

func TestAgentNode_ContainerHealth(t *testing.T) {
	fake := node.NewFake()
	fake.HealthState = "running"
	fake.DockerHealth = "healthy"
	an, _ := newTestServer(t, fake)

	state, dockerHealth, err := an.ContainerHealth(context.Background(), "c1")
	if err != nil {
		t.Fatalf("ContainerHealth: %v", err)
	}
	if state != "running" || dockerHealth != "healthy" {
		t.Fatalf("unexpected result: state=%q dockerHealth=%q", state, dockerHealth)
	}
}

func TestAgentNode_RemoveContainer_ListContainers(t *testing.T) {
	fake := node.NewFake()
	an, fake := newTestServer(t, fake)

	c, err := an.RunContainer(context.Background(), node.RunSpec{Name: "web-2", Image: "nginx", Labels: map[string]string{"app": "x"}})
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}

	list, err := an.ListContainers(context.Background(), map[string]string{"app": "x"})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("unexpected list: %+v", list)
	}

	if err := an.RemoveContainer(context.Background(), c.ID); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	_ = fake
}

func TestAgentNode_Health(t *testing.T) {
	fake := node.NewFake()
	an, _ := newTestServer(t, fake)

	c := node.Container{ID: "c1"}
	h := node.HealthSpec{Timeout: 2 * time.Second}
	if err := an.Health(context.Background(), c, h); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestAgentNode_LoadImage(t *testing.T) {
	fake := node.NewFake()
	an, fake := newTestServer(t, fake)

	payload := []byte("fake-tar-bytes-1234")
	if err := an.LoadImage(context.Background(), bytes.NewReader(payload)); err != nil {
		t.Fatalf("LoadImage: %v", err)
	}
	if !fake.Loaded {
		t.Fatal("expected fake.Loaded = true")
	}
	if !bytes.Equal(fake.LastLoaded, payload) {
		t.Fatalf("fake.LastLoaded = %q, want %q", fake.LastLoaded, payload)
	}
}

func TestAgentNode_SaveImage(t *testing.T) {
	fake := node.NewFake()
	an, _ := newTestServer(t, fake)

	rc, err := an.SaveImage(context.Background(), "myimage:latest")
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading SaveImage stream: %v", err)
	}
	if !strings.Contains(string(b), "myimage:latest") {
		t.Fatalf("expected stream to reference ref, got %q", b)
	}
}

func TestAgentNode_ContainerLogs(t *testing.T) {
	fake := node.NewFake()
	an, _ := newTestServer(t, fake)

	rc, err := an.ContainerLogs(context.Background(), "c1", false)
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading logs stream: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("expected non-empty log stream")
	}
}

func TestAgentNode_WrongToken(t *testing.T) {
	fake := node.NewFake()
	srv := httptest.NewServer(agent.NewServer(fake, testToken).Handler())
	t.Cleanup(srv.Close)

	an := node.NewAgentNode(srv.URL, "wrong-token")
	if err := an.EnsureImage(context.Background(), "nginx:latest"); err == nil {
		t.Fatal("expected error with wrong token, got nil")
	}
	if _, err := an.ImagePresent(context.Background(), "nginx:latest"); err == nil {
		t.Fatal("expected error with wrong token, got nil")
	}
}

func TestAgentNode_TrimsTrailingSlash(t *testing.T) {
	fake := node.NewFake()
	srv := httptest.NewServer(agent.NewServer(fake, testToken).Handler())
	t.Cleanup(srv.Close)

	an := node.NewAgentNode(srv.URL+"/", testToken)
	if err := an.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with trailing-slash baseURL: %v", err)
	}
}

var _ node.Node = (*node.AgentNode)(nil)
