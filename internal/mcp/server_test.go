package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lwd/internal/api"
	"lwd/internal/client"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/store"
)

// connectTestServer wires a fakeClient-backed Server to a real go-sdk client
// over an in-memory transport pair, and returns the connected client session
// plus a cleanup func that closes the session and waits for srv.Run to
// return.
func connectTestServer(t *testing.T, fc *fakeClient) *sdk.ClientSession {
	t.Helper()
	s := NewServer(fc)
	srv := s.MCP()

	serverTransport, clientTransport := sdk.NewInMemoryTransports()

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, serverTransport) }()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}

	t.Cleanup(func() {
		if err := cs.Close(); err != nil {
			t.Fatalf("cs.Close: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("srv.Run: %v", err)
		}
	})

	return cs
}

// findTool returns the named tool from a tools/list result, or nil.
func findTool(tools []*sdk.Tool, name string) *sdk.Tool {
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	return nil
}

// callTool invokes name with args and fails the test on a protocol-level
// error. It returns the result even when the tool itself reported an error
// (IsError), so callers can assert on that.
func callTool(t *testing.T, cs *sdk.ClientSession, name string, args any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

// decodeStructured unmarshals a tool result's structured content into out.
func decodeStructured(t *testing.T, res *sdk.CallToolResult, out any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal StructuredContent: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal StructuredContent: %v (raw: %s)", err, b)
	}
}

func TestToolStatus(t *testing.T) {
	fc := newFakeClient()
	fc.apps = []api.AppStatus{
		{Name: "web", Image: "nginx:latest", Status: "running", Domain: "web.example.com"},
	}
	created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fc.history["web"] = []store.Deployment{
		{ID: 2, App: "web", Image: "nginx:latest", Status: "running", CreatedAt: created},
		{ID: 1, App: "web", Image: "nginx:1.24", Status: "rolled_back", CreatedAt: created.Add(-time.Hour)},
	}

	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_status")
	if tool == nil {
		t.Fatalf("lwd_status tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Errorf("lwd_status should be annotated readOnlyHint=true, got %+v", tool.Annotations)
	}

	res := callTool(t, cs, "lwd_status", map[string]any{"name": "web"})
	if res.IsError {
		t.Fatalf("lwd_status(web) returned tool error: %+v", res.Content)
	}
	var out lwdStatusOutput
	decodeStructured(t, res, &out)
	if out.Status == nil || out.Status.Name != "web" || out.Status.Image != "nginx:latest" {
		t.Errorf("unexpected status: %+v", out.Status)
	}
	if len(out.History) != 2 || out.History[0].Image != "nginx:latest" {
		t.Errorf("unexpected history: %+v", out.History)
	}

	// Unknown app -> tool error.
	res = callTool(t, cs, "lwd_status", map[string]any{"name": "missing"})
	if !res.IsError {
		t.Errorf("lwd_status(missing) should be a tool error, got %+v", res)
	}
}

func TestToolLogs(t *testing.T) {
	fc := newFakeClient()
	var lines []string
	for i := 1; i <= 5; i++ {
		lines = append(lines, "line "+string(rune('0'+i)))
	}
	fc.logsData = strings.Join(lines, "\n") + "\n"

	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_logs")
	if tool == nil {
		t.Fatalf("lwd_logs tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Errorf("lwd_logs should be annotated readOnlyHint=true, got %+v", tool.Annotations)
	}

	// No tail specified: full captured output.
	res := callTool(t, cs, "lwd_logs", map[string]any{"name": "web"})
	if res.IsError {
		t.Fatalf("lwd_logs(web) returned tool error: %+v", res.Content)
	}
	var out lwdLogsOutput
	decodeStructured(t, res, &out)
	for _, l := range lines {
		if !strings.Contains(out.Logs, l) {
			t.Errorf("expected logs to contain %q, got %q", l, out.Logs)
		}
	}

	// tail=2 trims to the last two lines only.
	res = callTool(t, cs, "lwd_logs", map[string]any{"name": "web", "tail": 2})
	if res.IsError {
		t.Fatalf("lwd_logs(web, tail=2) returned tool error: %+v", res.Content)
	}
	decodeStructured(t, res, &out)
	got := strings.Split(strings.TrimRight(out.Logs, "\n"), "\n")
	if len(got) != 2 {
		t.Fatalf("expected 2 lines with tail=2, got %d: %q", len(got), out.Logs)
	}
	if got[0] != lines[3] || got[1] != lines[4] {
		t.Errorf("expected last 2 lines %v, got %v", lines[3:], got)
	}
}

func TestToolHistory(t *testing.T) {
	fc := newFakeClient()
	created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fc.history["api"] = []store.Deployment{
		{ID: 3, App: "api", Image: "api:v3", Status: "running", CreatedAt: created, Spec: "{secret stuff}", Compose: "services: ..."},
	}

	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_history")
	if tool == nil {
		t.Fatalf("lwd_history tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Errorf("lwd_history should be annotated readOnlyHint=true, got %+v", tool.Annotations)
	}

	res := callTool(t, cs, "lwd_history", map[string]any{"name": "api"})
	if res.IsError {
		t.Fatalf("lwd_history(api) returned tool error: %+v", res.Content)
	}
	var out lwdHistoryOutput
	decodeStructured(t, res, &out)
	if len(out.Deployments) != 1 {
		t.Fatalf("expected 1 deployment, got %d: %+v", len(out.Deployments), out.Deployments)
	}
	d := out.Deployments[0]
	if d.ID != 3 || d.Image != "api:v3" || d.Status != "running" || !d.CreatedAt.Equal(created) {
		t.Errorf("unexpected deployment summary: %+v", d)
	}

	// Concise output: no raw Spec/Compose blobs leaked.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret stuff") || strings.Contains(string(raw), "services: ...") {
		t.Errorf("lwd_history leaked Spec/Compose content: %s", raw)
	}
}

// TestServerConstructs verifies NewServer(&fakeClient{}).MCP() builds a
// working go-sdk server with the expected tools registered. It connects a
// real go-sdk client over an in-memory transport pair and calls tools/list,
// which is the only way to introspect a *sdk.Server's registered tools from
// outside the package.
//
// Note: the compile-time assertion `var _ ClientIface = (*client.Client)(nil)`
// in server.go is itself a test — if internal/client drifts from ClientIface,
// this package (and therefore this test) fails to build.
func TestServerConstructs(t *testing.T) {
	s := NewServer(newFakeClient())
	srv := s.MCP()
	if srv == nil {
		t.Fatal("NewServer(...).MCP() returned a nil server")
	}

	serverTransport, clientTransport := sdk.NewInMemoryTransports()

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, serverTransport) }()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	var lwdList *sdk.Tool
	for _, tool := range res.Tools {
		if tool.Name == "lwd_list" {
			lwdList = tool
			break
		}
	}
	if lwdList == nil {
		t.Fatalf("lwd_list tool not registered; got %d tools: %+v", len(res.Tools), res.Tools)
	}
	if lwdList.Annotations == nil || !lwdList.Annotations.ReadOnlyHint {
		t.Errorf("lwd_list should be annotated readOnlyHint=true, got %+v", lwdList.Annotations)
	}

	if err := cs.Close(); err != nil {
		t.Fatalf("cs.Close: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("srv.Run: %v", err)
	}
}

const validSingleServiceToml = `
name = "web"
image = "nginx:latest"
domain = "web.example.com"
port = 8080
`

func TestToolApplyToml(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	// Valid single-service toml: Apply is called with the parsed app.
	res := callTool(t, cs, "lwd_apply", map[string]any{"toml": validSingleServiceToml})
	if res.IsError {
		t.Fatalf("lwd_apply(valid toml) returned tool error: %+v", res.Content)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("expected Apply called once, got %d", len(fc.applied))
	}
	got := fc.applied[0]
	if got.Name != "web" || got.Image != "nginx:latest" || got.Domain != "web.example.com" || got.Port != 8080 {
		t.Errorf("unexpected applied app: %+v", got)
	}
	var out lwdDeploymentOutput
	decodeStructured(t, res, &out)
	if out.Name != "web" || out.Image != "nginx:latest" || out.Status != store.StatusRunning {
		t.Errorf("unexpected apply output: %+v", out)
	}

	// Invalid (malformed) toml -> tool error, Apply not called again.
	res = callTool(t, cs, "lwd_apply", map[string]any{"toml": "this is not [ valid toml"})
	if !res.IsError {
		t.Errorf("lwd_apply(malformed toml) should be a tool error, got %+v", res)
	}
	if len(fc.applied) != 1 {
		t.Errorf("Apply should not have been called for malformed toml, got %d calls", len(fc.applied))
	}

	// Valid toml syntax but missing image+port -> Validate error, tool error.
	res = callTool(t, cs, "lwd_apply", map[string]any{"toml": `name = "web"`})
	if !res.IsError {
		t.Errorf("lwd_apply(missing image/port) should be a tool error, got %+v", res)
	}
	if len(fc.applied) != 1 {
		t.Errorf("Apply should not have been called for an invalid app, got %d calls", len(fc.applied))
	}

	// Both dir and toml -> tool error.
	res = callTool(t, cs, "lwd_apply", map[string]any{"toml": validSingleServiceToml, "dir": "/tmp/whatever"})
	if !res.IsError {
		t.Errorf("lwd_apply(dir+toml) should be a tool error, got %+v", res)
	}

	// Neither dir nor toml -> tool error.
	res = callTool(t, cs, "lwd_apply", map[string]any{})
	if !res.IsError {
		t.Errorf("lwd_apply(neither) should be a tool error, got %+v", res)
	}

	if len(fc.applied) != 1 {
		t.Errorf("Apply should have been called exactly once overall, got %d", len(fc.applied))
	}
}

func TestToolApplyDir(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lwd.toml"), []byte(validSingleServiceToml), 0o644); err != nil {
		t.Fatalf("write lwd.toml: %v", err)
	}

	res := callTool(t, cs, "lwd_apply", map[string]any{"dir": dir})
	if res.IsError {
		t.Fatalf("lwd_apply(dir) returned tool error: %+v", res.Content)
	}
	if len(fc.applied) != 1 || fc.applied[0].Name != "web" {
		t.Fatalf("expected Apply called once with the loaded app, got %+v", fc.applied)
	}

	// A directory with no lwd.toml -> tool error.
	res = callTool(t, cs, "lwd_apply", map[string]any{"dir": t.TempDir()})
	if !res.IsError {
		t.Errorf("lwd_apply(dir without lwd.toml) should be a tool error, got %+v", res)
	}
}

func TestToolDeployGit(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if tool := findTool(lr.Tools, "lwd_deploy_git"); tool == nil {
		t.Fatalf("lwd_deploy_git tool not registered; got %+v", lr.Tools)
	}

	res := callTool(t, cs, "lwd_deploy_git", map[string]any{
		"url":    "https://github.com/example/app.git",
		"name":   "app",
		"domain": "app.example.com",
		"port":   3000,
		"services": []map[string]any{
			{
				"name":  "redis",
				"image": "redis:7",
				"env":   map[string]any{"FOO": "bar"},
			},
		},
	})
	if res.IsError {
		t.Fatalf("lwd_deploy_git(valid) returned tool error: %+v", res.Content)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("expected Apply called once, got %d", len(fc.applied))
	}
	got := fc.applied[0]
	if got.Git == nil || got.Git.URL != "https://github.com/example/app.git" {
		t.Errorf("expected Git.URL set, got %+v", got.Git)
	}
	if got.Git.Ref != "main" {
		t.Errorf("expected Git.Ref to default to \"main\", got %q", got.Git.Ref)
	}
	if got.Build == nil || got.Build.Dockerfile != "Dockerfile" {
		t.Errorf("expected Build.Dockerfile to default to \"Dockerfile\", got %+v", got.Build)
	}
	if got.Domain != "app.example.com" || got.Port != 3000 || got.Name != "app" {
		t.Errorf("unexpected app fields: %+v", got)
	}
	if len(got.Services) != 1 || got.Services[0].Name != "redis" || got.Services[0].Image != "redis:7" || got.Services[0].Env["FOO"] != "bar" {
		t.Errorf("unexpected services: %+v", got.Services)
	}

	// Invalid: command-executing git transport -> Validate rejects before Apply.
	res = callTool(t, cs, "lwd_deploy_git", map[string]any{
		"url":    "ext::sh -c 'touch pwned'",
		"name":   "evil",
		"domain": "evil.example.com",
		"port":   80,
	})
	if !res.IsError {
		t.Errorf("lwd_deploy_git(ext:: url) should be a tool error, got %+v", res)
	}
	if len(fc.applied) != 1 {
		t.Errorf("Apply should not have been called for an invalid git spec, got %d calls", len(fc.applied))
	}

	// Invalid: missing domain -> Validate rejects before Apply.
	res = callTool(t, cs, "lwd_deploy_git", map[string]any{
		"url":  "https://github.com/example/app.git",
		"name": "nodomain",
		"port": 80,
	})
	if !res.IsError {
		t.Errorf("lwd_deploy_git(missing domain) should be a tool error, got %+v", res)
	}
	if len(fc.applied) != 1 {
		t.Errorf("Apply should not have been called for a missing domain, got %d calls", len(fc.applied))
	}
}

func TestToolDeployGitCustomRefAndDockerfile(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_deploy_git", map[string]any{
		"url":        "https://github.com/example/app.git",
		"ref":        "release/v2",
		"dockerfile": "docker/Dockerfile.prod",
		"name":       "app",
		"domain":     "app.example.com",
		"port":       3000,
	})
	if res.IsError {
		t.Fatalf("lwd_deploy_git(custom ref/dockerfile) returned tool error: %+v", res.Content)
	}
	got := fc.applied[0]
	if got.Git.Ref != "release/v2" {
		t.Errorf("expected Git.Ref %q, got %q", "release/v2", got.Git.Ref)
	}
	if got.Build.Dockerfile != "docker/Dockerfile.prod" {
		t.Errorf("expected Build.Dockerfile %q, got %q", "docker/Dockerfile.prod", got.Build.Dockerfile)
	}
}

func TestToolRollback(t *testing.T) {
	fc := newFakeClient()
	fc.rollbackResult = &store.Deployment{App: "web", Image: "nginx:1.24", Status: store.StatusRunning, ContainerID: "abc123"}
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_rollback", map[string]any{"name": "web"})
	if res.IsError {
		t.Fatalf("lwd_rollback(web) returned tool error: %+v", res.Content)
	}
	var out lwdDeploymentOutput
	decodeStructured(t, res, &out)
	if out.Name != "web" || out.Image != "nginx:1.24" || out.Status != store.StatusRunning || out.ContainerID != "abc123" {
		t.Errorf("unexpected rollback output: %+v", out)
	}

	fc.rollbackErr = fmt.Errorf("rollback failed")
	res = callTool(t, cs, "lwd_rollback", map[string]any{"name": "web"})
	if !res.IsError {
		t.Errorf("lwd_rollback should surface a daemon error as a tool error, got %+v", res)
	}
}

// TestToolScale covers Phase 12 Task 8's lwd_scale tool: {app, replicas} ->
// client.Scale, annotated destructiveHint (it tears down and recreates
// containers), and NOT readOnlyHint.
func TestToolScale(t *testing.T) {
	fc := newFakeClient()
	fc.scaleResult = &store.Deployment{
		App: "web", Image: "nginx:1.24", Status: store.StatusRunning,
		ContainerID: "abc123", Replicas: []store.Replica{{}, {}, {}},
	}
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_scale")
	if tool == nil {
		t.Fatalf("lwd_scale tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
		t.Errorf("lwd_scale should be annotated destructiveHint=true, got %+v", tool.Annotations)
	}
	if tool.Annotations.ReadOnlyHint {
		t.Errorf("lwd_scale must not be annotated readOnlyHint=true, got %+v", tool.Annotations)
	}

	res := callTool(t, cs, "lwd_scale", map[string]any{"app": "web", "replicas": 3})
	if res.IsError {
		t.Fatalf("lwd_scale(web, 3) returned tool error: %+v", res.Content)
	}
	var out lwdScaleOutput
	decodeStructured(t, res, &out)
	if out.Name != "web" || out.Status != store.StatusRunning || out.Replicas != 3 {
		t.Errorf("unexpected scale output: %+v", out)
	}
	if len(fc.scaleCalls) != 1 || fc.scaleCalls[0].Name != "web" || fc.scaleCalls[0].Replicas != 3 {
		t.Fatalf("scaleCalls = %+v, want one call for web with replicas=3", fc.scaleCalls)
	}

	fc.scaleErr = fmt.Errorf("scale failed")
	res = callTool(t, cs, "lwd_scale", map[string]any{"app": "web", "replicas": 1})
	if !res.IsError {
		t.Errorf("lwd_scale should surface a daemon error as a tool error, got %+v", res)
	}
}

// TestToolApplyWithReplicas covers Phase 12 Task 8: lwd_apply's optional
// replicas argument sets app.Replicas before Validate/Apply, alongside
// node/pool/requirements.
func TestToolApplyWithReplicas(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_apply", map[string]any{"toml": validSingleServiceToml, "replicas": 3})
	if res.IsError {
		t.Fatalf("lwd_apply(replicas) returned tool error: %+v", res.Content)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("expected Apply called once, got %d", len(fc.applied))
	}
	if fc.applied[0].Replicas != 3 {
		t.Errorf("expected applied App.Replicas = 3, got %d", fc.applied[0].Replicas)
	}

	// Omitting replicas leaves the toml's own value (defaulted to 1 by
	// spec.Parse) untouched.
	res = callTool(t, cs, "lwd_apply", map[string]any{"toml": validSingleServiceToml})
	if res.IsError {
		t.Fatalf("lwd_apply(no replicas) returned tool error: %+v", res.Content)
	}
	if fc.applied[1].Replicas != 1 {
		t.Errorf("expected applied App.Replicas = 1 (toml default) when omitted, got %d", fc.applied[1].Replicas)
	}
}

// TestToolDeployGitWithReplicas covers Phase 12 Task 8: lwd_deploy_git's
// optional replicas argument sets app.Replicas before Validate/Apply.
func TestToolDeployGitWithReplicas(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_deploy_git", map[string]any{
		"url": "https://github.com/example/app.git", "name": "app", "domain": "app.example.com", "port": 3000, "replicas": 3,
	})
	if res.IsError {
		t.Fatalf("lwd_deploy_git(replicas) returned tool error: %+v", res.Content)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("expected Apply called once, got %d", len(fc.applied))
	}
	if fc.applied[0].Replicas != 3 {
		t.Errorf("expected applied App.Replicas = 3, got %d", fc.applied[0].Replicas)
	}
}

// TestToolStatusAndListIncludeReplicas covers Phase 12 Task 8: lwd_status
// and lwd_list surface api.AppStatus.Replicas untouched — no mcp-layer
// transformation needed, since it already rides through client.Apps/Status.
func TestToolStatusAndListIncludeReplicas(t *testing.T) {
	fc := newFakeClient()
	fc.apps = []api.AppStatus{
		{Name: "web", Image: "nginx:latest", Status: "running", Domain: "web.example.com", Replicas: 3},
	}
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_list", map[string]any{})
	if res.IsError {
		t.Fatalf("lwd_list returned tool error: %+v", res.Content)
	}
	var listOut lwdListOutput
	decodeStructured(t, res, &listOut)
	if len(listOut.Apps) != 1 || listOut.Apps[0].Replicas != 3 {
		t.Fatalf("lwd_list Apps = %+v, want one app with Replicas=3", listOut.Apps)
	}

	res = callTool(t, cs, "lwd_status", map[string]any{"name": "web"})
	if res.IsError {
		t.Fatalf("lwd_status returned tool error: %+v", res.Content)
	}
	var statusOut lwdStatusOutput
	decodeStructured(t, res, &statusOut)
	if statusOut.Status == nil || statusOut.Status.Replicas != 3 {
		t.Fatalf("lwd_status Status = %+v, want Replicas=3", statusOut.Status)
	}
}

func TestToolRemove(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_remove")
	if tool == nil {
		t.Fatalf("lwd_remove tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
		t.Errorf("lwd_remove should be annotated destructiveHint=true, got %+v", tool.Annotations)
	}
	if tool.Annotations.ReadOnlyHint {
		t.Errorf("lwd_remove must not be annotated readOnlyHint=true, got %+v", tool.Annotations)
	}

	res := callTool(t, cs, "lwd_remove", map[string]any{"name": "web"})
	if res.IsError {
		t.Fatalf("lwd_remove(web) returned tool error: %+v", res.Content)
	}
	if len(fc.removed) != 1 || fc.removed[0] != "web" {
		t.Fatalf("expected Remove called with \"web\", got %+v", fc.removed)
	}

	fc.removeErr = fmt.Errorf("remove failed")
	res = callTool(t, cs, "lwd_remove", map[string]any{"name": "missing"})
	if !res.IsError {
		t.Errorf("lwd_remove should surface a daemon error as a tool error, got %+v", res)
	}
}

// resultText concatenates a tool result's structured content and unstructured
// text content into a single string, for asserting a secret value never
// appears anywhere in the response.
func resultText(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal StructuredContent: %v", err)
		}
		sb.Write(b)
	}
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestToolSecretSetAndList(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if tool := findTool(lr.Tools, "lwd_secret_set"); tool == nil {
		t.Fatalf("lwd_secret_set tool not registered; got %+v", lr.Tools)
	} else if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
		t.Errorf("lwd_secret_set must not be annotated readOnlyHint=true, got %+v", tool.Annotations)
	}
	listTool := findTool(lr.Tools, "lwd_secret_list")
	if listTool == nil {
		t.Fatalf("lwd_secret_list tool not registered; got %+v", lr.Tools)
	}
	if listTool.Annotations == nil || !listTool.Annotations.ReadOnlyHint {
		t.Errorf("lwd_secret_list should be annotated readOnlyHint=true, got %+v", listTool.Annotations)
	}

	const secretValue = "sup3r-s3cr3t-p4ssw0rd"

	setRes := callTool(t, cs, "lwd_secret_set", map[string]any{"app": "web", "key": "DB_PASSWORD", "value": secretValue})
	if setRes.IsError {
		t.Fatalf("lwd_secret_set returned tool error: %+v", setRes.Content)
	}
	var setOut lwdSecretSetOutput
	decodeStructured(t, setRes, &setOut)
	if !setOut.OK || setOut.App != "web" || setOut.Key != "DB_PASSWORD" {
		t.Errorf("unexpected lwd_secret_set output: %+v", setOut)
	}
	if strings.Contains(resultText(t, setRes), secretValue) {
		t.Errorf("lwd_secret_set response leaked the secret value: %s", resultText(t, setRes))
	}

	listRes := callTool(t, cs, "lwd_secret_list", map[string]any{"app": "web"})
	if listRes.IsError {
		t.Fatalf("lwd_secret_list returned tool error: %+v", listRes.Content)
	}
	var listOut lwdSecretListOutput
	decodeStructured(t, listRes, &listOut)
	if len(listOut.Names) != 1 || listOut.Names[0] != "DB_PASSWORD" {
		t.Errorf("expected lwd_secret_list to show [DB_PASSWORD], got %+v", listOut.Names)
	}
	if strings.Contains(resultText(t, listRes), secretValue) {
		t.Errorf("lwd_secret_list response leaked the secret value: %s", resultText(t, listRes))
	}
}

func TestToolSecretDelete(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_secret_delete")
	if tool == nil {
		t.Fatalf("lwd_secret_delete tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
		t.Errorf("lwd_secret_delete should be annotated destructiveHint=true, got %+v", tool.Annotations)
	}

	callTool(t, cs, "lwd_secret_set", map[string]any{"app": "web", "key": "DB_PASSWORD", "value": "hunter2"})

	listRes := callTool(t, cs, "lwd_secret_list", map[string]any{"app": "web"})
	var listOut lwdSecretListOutput
	decodeStructured(t, listRes, &listOut)
	if len(listOut.Names) != 1 {
		t.Fatalf("expected secret to be set before delete, got %+v", listOut.Names)
	}

	delRes := callTool(t, cs, "lwd_secret_delete", map[string]any{"app": "web", "key": "DB_PASSWORD"})
	if delRes.IsError {
		t.Fatalf("lwd_secret_delete returned tool error: %+v", delRes.Content)
	}
	var delOut lwdSecretDeleteOutput
	decodeStructured(t, delRes, &delOut)
	if !delOut.OK || delOut.App != "web" || delOut.Key != "DB_PASSWORD" {
		t.Errorf("unexpected lwd_secret_delete output: %+v", delOut)
	}

	listRes = callTool(t, cs, "lwd_secret_list", map[string]any{"app": "web"})
	decodeStructured(t, listRes, &listOut)
	if len(listOut.Names) != 0 {
		t.Errorf("expected no secrets after delete, got %+v", listOut.Names)
	}

	fc.deleteSecretErr = fmt.Errorf("delete failed")
	delRes = callTool(t, cs, "lwd_secret_delete", map[string]any{"app": "web", "key": "DB_PASSWORD"})
	if !delRes.IsError {
		t.Errorf("lwd_secret_delete should surface a daemon error as a tool error, got %+v", delRes)
	}
}

func TestToolNodeList(t *testing.T) {
	fc := newFakeClient()
	fc.nodes = []client.NodeStatus{
		{Node: store.Node{Name: "web1", SSHHost: "deploy@web1.example.com", MeshAddr: "100.64.0.2", AgentURL: "http://100.64.0.2:8078"}, Transport: "agent", Reachable: true},
		{Node: store.Node{Name: "web2", SSHHost: "deploy@web2.example.com", MeshAddr: "100.64.0.3"}, Transport: "ssh", Reachable: false},
	}
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_node_list")
	if tool == nil {
		t.Fatalf("lwd_node_list tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Errorf("lwd_node_list should be annotated readOnlyHint=true, got %+v", tool.Annotations)
	}

	res := callTool(t, cs, "lwd_node_list", map[string]any{})
	if res.IsError {
		t.Fatalf("lwd_node_list returned tool error: %+v", res.Content)
	}
	var out lwdNodeListOutput
	decodeStructured(t, res, &out)
	if len(out.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d: %+v", len(out.Nodes), out.Nodes)
	}
	if out.Nodes[0].Name != "web1" || out.Nodes[0].Transport != "agent" || !out.Nodes[0].Reachable {
		t.Errorf("unexpected node[0]: %+v", out.Nodes[0])
	}
	if out.Nodes[1].Name != "web2" || out.Nodes[1].Transport != "ssh" || out.Nodes[1].Reachable {
		t.Errorf("unexpected node[1]: %+v", out.Nodes[1])
	}
}

func TestToolNodeAdd(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if tool := findTool(lr.Tools, "lwd_node_add"); tool == nil {
		t.Fatalf("lwd_node_add tool not registered; got %+v", lr.Tools)
	}

	res := callTool(t, cs, "lwd_node_add", map[string]any{
		"name": "web1", "ssh_host": "deploy@web1.example.com", "mesh_addr": "100.64.0.2", "agent_url": "http://100.64.0.2:8078",
	})
	if res.IsError {
		t.Fatalf("lwd_node_add returned tool error: %+v", res.Content)
	}
	if len(fc.addedNodes) != 1 {
		t.Fatalf("expected AddNode called once, got %d", len(fc.addedNodes))
	}
	got := fc.addedNodes[0]
	if got.Name != "web1" || got.SSHHost != "deploy@web1.example.com" || got.MeshAddr != "100.64.0.2" || got.AgentURL != "http://100.64.0.2:8078" {
		t.Errorf("unexpected AddNode call: %+v", got)
	}

	// agent_url is optional.
	res = callTool(t, cs, "lwd_node_add", map[string]any{"name": "web2", "ssh_host": "deploy@web2.example.com", "mesh_addr": "100.64.0.3"})
	if res.IsError {
		t.Fatalf("lwd_node_add (no agent_url) returned tool error: %+v", res.Content)
	}
	if len(fc.addedNodes) != 2 || fc.addedNodes[1].AgentURL != "" {
		t.Fatalf("expected second AddNode with empty agent_url, got %+v", fc.addedNodes)
	}

	fc.addNodeErr = fmt.Errorf("add failed")
	res = callTool(t, cs, "lwd_node_add", map[string]any{"name": "web3", "ssh_host": "h", "mesh_addr": "m"})
	if !res.IsError {
		t.Errorf("lwd_node_add should surface a daemon error as a tool error, got %+v", res)
	}
}

func TestToolNodeRemove(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_node_remove")
	if tool == nil {
		t.Fatalf("lwd_node_remove tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
		t.Errorf("lwd_node_remove should be annotated destructiveHint=true, got %+v", tool.Annotations)
	}

	res := callTool(t, cs, "lwd_node_remove", map[string]any{"name": "web1"})
	if res.IsError {
		t.Fatalf("lwd_node_remove returned tool error: %+v", res.Content)
	}
	if len(fc.removedNodes) != 1 || fc.removedNodes[0] != "web1" {
		t.Fatalf("expected RemoveNode called with \"web1\", got %+v", fc.removedNodes)
	}

	fc.removeNodeErr = fmt.Errorf("remove failed")
	res = callTool(t, cs, "lwd_node_remove", map[string]any{"name": "web1"})
	if !res.IsError {
		t.Errorf("lwd_node_remove should surface a daemon error as a tool error, got %+v", res)
	}
}

// TestToolNodeDrain covers lwd_node_drain: it must be annotated
// destructiveHint=true (it cordons AND evacuates — surfaces actually move),
// call client.Drain with the given name, and return the resulting
// reconciler.EvacuateResult (moved/skipped/failed) as structured output.
func TestToolNodeDrain(t *testing.T) {
	fc := newFakeClient()
	fc.drainResult = reconciler.EvacuateResult{
		Moved:   []string{"blog"},
		Skipped: []string{"pinned"},
		Failed:  []reconciler.EvacFailure{{App: "shop", Err: "no capacity"}},
	}
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_node_drain")
	if tool == nil {
		t.Fatalf("lwd_node_drain tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
		t.Errorf("lwd_node_drain should be annotated destructiveHint=true, got %+v", tool.Annotations)
	}

	res := callTool(t, cs, "lwd_node_drain", map[string]any{"name": "web1"})
	if res.IsError {
		t.Fatalf("lwd_node_drain returned tool error: %+v", res.Content)
	}
	if len(fc.drainCalls) != 1 || fc.drainCalls[0] != "web1" {
		t.Fatalf("expected Drain called with \"web1\", got %+v", fc.drainCalls)
	}
	var out reconciler.EvacuateResult
	decodeStructured(t, res, &out)
	if len(out.Moved) != 1 || out.Moved[0] != "blog" {
		t.Errorf("Moved = %v", out.Moved)
	}
	if len(out.Skipped) != 1 || out.Skipped[0] != "pinned" {
		t.Errorf("Skipped = %v", out.Skipped)
	}
	if len(out.Failed) != 1 || out.Failed[0].App != "shop" {
		t.Errorf("Failed = %v", out.Failed)
	}

	fc.drainErr = fmt.Errorf("drain failed")
	res = callTool(t, cs, "lwd_node_drain", map[string]any{"name": "web1"})
	if !res.IsError {
		t.Errorf("lwd_node_drain should surface a daemon error as a tool error, got %+v", res)
	}
}

// TestToolNodeEvacuate covers lwd_node_evacuate: destructiveHint=true (it
// moves surfaces, just without cordoning first), calls client.Evacuate, and
// returns the resulting reconciler.EvacuateResult.
func TestToolNodeEvacuate(t *testing.T) {
	fc := newFakeClient()
	fc.evacuateResult = reconciler.EvacuateResult{Moved: []string{"blog"}}
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_node_evacuate")
	if tool == nil {
		t.Fatalf("lwd_node_evacuate tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
		t.Errorf("lwd_node_evacuate should be annotated destructiveHint=true, got %+v", tool.Annotations)
	}

	res := callTool(t, cs, "lwd_node_evacuate", map[string]any{"name": "web1"})
	if res.IsError {
		t.Fatalf("lwd_node_evacuate returned tool error: %+v", res.Content)
	}
	if len(fc.evacuateCalls) != 1 || fc.evacuateCalls[0] != "web1" {
		t.Fatalf("expected Evacuate called with \"web1\", got %+v", fc.evacuateCalls)
	}
	var out reconciler.EvacuateResult
	decodeStructured(t, res, &out)
	if len(out.Moved) != 1 || out.Moved[0] != "blog" {
		t.Errorf("Moved = %v", out.Moved)
	}

	fc.evacuateErr = fmt.Errorf("evacuate failed")
	res = callTool(t, cs, "lwd_node_evacuate", map[string]any{"name": "web1"})
	if !res.IsError {
		t.Errorf("lwd_node_evacuate should surface a daemon error as a tool error, got %+v", res)
	}
}

// TestToolNodeUncordon covers lwd_node_uncordon: low-risk (no
// destructiveHint — it only clears a cordon, touching nothing already
// deployed), calls client.Uncordon, and confirms via a simple ok/name
// output.
func TestToolNodeUncordon(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tool := findTool(lr.Tools, "lwd_node_uncordon")
	if tool == nil {
		t.Fatalf("lwd_node_uncordon tool not registered; got %+v", lr.Tools)
	}
	if tool.Annotations != nil && tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint {
		t.Errorf("lwd_node_uncordon should NOT be annotated destructiveHint=true (it's low-risk), got %+v", tool.Annotations)
	}

	res := callTool(t, cs, "lwd_node_uncordon", map[string]any{"name": "web1"})
	if res.IsError {
		t.Fatalf("lwd_node_uncordon returned tool error: %+v", res.Content)
	}
	if len(fc.uncordonCalls) != 1 || fc.uncordonCalls[0] != "web1" {
		t.Fatalf("expected Uncordon called with \"web1\", got %+v", fc.uncordonCalls)
	}
	var out lwdNodeUncordonOutput
	decodeStructured(t, res, &out)
	if !out.OK || out.Name != "web1" {
		t.Errorf("unexpected output: %+v", out)
	}

	fc.uncordonErr = fmt.Errorf("uncordon failed")
	res = callTool(t, cs, "lwd_node_uncordon", map[string]any{"name": "web1"})
	if !res.IsError {
		t.Errorf("lwd_node_uncordon should surface a daemon error as a tool error, got %+v", res)
	}
}

// TestToolNodeListIncludesSchedulable covers that lwd_node_list's structured
// output round-trips each node's cordon state (store.Node.Schedulable,
// embedded via client.NodeStatus) — the field a caller checks before
// deciding whether to place something on it, or as a signal that a node
// needs lwd_node_uncordon.
func TestToolNodeListIncludesSchedulable(t *testing.T) {
	fc := newFakeClient()
	fc.nodes = []client.NodeStatus{
		{Node: store.Node{Name: "web1", Schedulable: true}, Transport: "agent", Reachable: true},
		{Node: store.Node{Name: "web2", Schedulable: false}, Transport: "ssh", Reachable: true},
	}
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_node_list", map[string]any{})
	if res.IsError {
		t.Fatalf("lwd_node_list returned tool error: %+v", res.Content)
	}
	var out lwdNodeListOutput
	decodeStructured(t, res, &out)
	if len(out.Nodes) != 2 || !out.Nodes[0].Schedulable || out.Nodes[1].Schedulable {
		t.Fatalf("unexpected schedulable states: %+v", out.Nodes)
	}
}

func TestToolApplyWithNode(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_apply", map[string]any{"toml": validSingleServiceToml, "node": "web1"})
	if res.IsError {
		t.Fatalf("lwd_apply(node) returned tool error: %+v", res.Content)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("expected Apply called once, got %d", len(fc.applied))
	}
	if fc.applied[0].Node != "web1" {
		t.Errorf("expected applied App.Node = %q, got %q", "web1", fc.applied[0].Node)
	}
}

func TestToolDeployGitWithNode(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_deploy_git", map[string]any{
		"url": "https://github.com/example/app.git", "name": "app", "domain": "app.example.com", "port": 3000, "node": "web1",
	})
	if res.IsError {
		t.Fatalf("lwd_deploy_git(node) returned tool error: %+v", res.Content)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("expected Apply called once, got %d", len(fc.applied))
	}
	if fc.applied[0].Node != "web1" {
		t.Errorf("expected applied App.Node = %q, got %q", "web1", fc.applied[0].Node)
	}
}

// TestToolNodeAddWithPool covers Phase 11a Task 8: lwd_node_add's pool
// argument reaches client.AddNode (Task 4 left it always passing "").
func TestToolNodeAddWithPool(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_node_add", map[string]any{
		"name": "web1", "ssh_host": "deploy@web1.example.com", "mesh_addr": "100.64.0.2", "pool": "web",
	})
	if res.IsError {
		t.Fatalf("lwd_node_add(pool) returned tool error: %+v", res.Content)
	}
	if len(fc.addedNodes) != 1 || fc.addedNodes[0].Pool != "web" {
		t.Fatalf("expected AddNode called once with pool %q, got %+v", "web", fc.addedNodes)
	}

	// pool is optional, same as agent_url.
	res = callTool(t, cs, "lwd_node_add", map[string]any{"name": "web2", "ssh_host": "h", "mesh_addr": "m"})
	if res.IsError {
		t.Fatalf("lwd_node_add(no pool) returned tool error: %+v", res.Content)
	}
	if len(fc.addedNodes) != 2 || fc.addedNodes[1].Pool != "" {
		t.Fatalf("expected second AddNode with empty pool, got %+v", fc.addedNodes)
	}
}

// TestToolApplyWithPoolAndRequirements covers Phase 11a Task 8: lwd_apply's
// optional pool and requirements arguments set app.Pool/app.Requirements
// before Validate/Apply.
func TestToolApplyWithPoolAndRequirements(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_apply", map[string]any{
		"toml":         validSingleServiceToml,
		"pool":         "web",
		"requirements": map[string]any{"cpu": 1.5, "memory": "512M"},
	})
	if res.IsError {
		t.Fatalf("lwd_apply(pool, requirements) returned tool error: %+v", res.Content)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("expected Apply called once, got %d", len(fc.applied))
	}
	got := fc.applied[0]
	if got.Pool != "web" {
		t.Errorf("expected applied App.Pool = %q, got %q", "web", got.Pool)
	}
	if got.Requirements == nil || got.Requirements.CPU != 1.5 || got.Requirements.Memory != "512M" {
		t.Errorf("unexpected applied App.Requirements: %+v", got.Requirements)
	}

	// Omitting requirements entirely leaves app.Requirements nil (not a
	// zero-valued &spec.Requirements{}) — Validate treats nil as "no
	// requirements declared" (see spec.go), distinct from an explicit {}.
	res = callTool(t, cs, "lwd_apply", map[string]any{"toml": validSingleServiceToml})
	if res.IsError {
		t.Fatalf("lwd_apply(no pool/requirements) returned tool error: %+v", res.Content)
	}
	if fc.applied[1].Requirements != nil {
		t.Errorf("expected nil Requirements when omitted, got %+v", fc.applied[1].Requirements)
	}
}

// TestToolDeployGitWithPoolAndRequirements covers Phase 11a Task 8:
// lwd_deploy_git's optional pool and requirements arguments set
// app.Pool/app.Requirements before Validate/Apply.
func TestToolDeployGitWithPoolAndRequirements(t *testing.T) {
	fc := newFakeClient()
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_deploy_git", map[string]any{
		"url": "https://github.com/example/app.git", "name": "app", "domain": "app.example.com", "port": 3000,
		"pool":         "web",
		"requirements": map[string]any{"cpu": 0.5, "memory": "1G"},
	})
	if res.IsError {
		t.Fatalf("lwd_deploy_git(pool, requirements) returned tool error: %+v", res.Content)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("expected Apply called once, got %d", len(fc.applied))
	}
	got := fc.applied[0]
	if got.Pool != "web" {
		t.Errorf("expected applied App.Pool = %q, got %q", "web", got.Pool)
	}
	if got.Requirements == nil || got.Requirements.CPU != 0.5 || got.Requirements.Memory != "1G" {
		t.Errorf("unexpected applied App.Requirements: %+v", got.Requirements)
	}
}

// TestToolNodeListIncludesPoolAndCapacity covers Phase 11a Task 8:
// lwd_node_list's output carries each node's pool (from client.Nodes,
// unchanged) and its live capacity, merged in from client.Health by node
// name.
func TestToolNodeListIncludesPoolAndCapacity(t *testing.T) {
	fc := newFakeClient()
	fc.nodes = []client.NodeStatus{
		{Node: store.Node{Name: "web1", Pool: "web"}, Transport: "agent", Reachable: true},
	}
	fc.health = reconciler.Health{
		Nodes: []reconciler.NodeHealth{
			{Name: "web1", Transport: "agent", Reachable: true, Capacity: node.Capacity{
				CPUCores: 4, CPUUsed: 0.5, MemTotal: 8 << 30, MemAvailable: 6 << 30, Known: true,
			}},
		},
	}
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_node_list", map[string]any{})
	if res.IsError {
		t.Fatalf("lwd_node_list returned tool error: %+v", res.Content)
	}
	var out lwdNodeListOutput
	decodeStructured(t, res, &out)
	if len(out.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d: %+v", len(out.Nodes), out.Nodes)
	}
	got := out.Nodes[0]
	if got.Pool != "web" {
		t.Errorf("Pool = %q, want %q", got.Pool, "web")
	}
	if !got.Capacity.Known || got.Capacity.CPUCores != 4 || got.Capacity.MemAvailable != 6<<30 {
		t.Errorf("unexpected Capacity: %+v", got.Capacity)
	}
}

// TestToolNodeListCapacityUnknownOnHealthError covers that a failed
// client.Health call (e.g. a daemon that hasn't run a reconcile pass, or a
// transient error) doesn't fail lwd_node_list — every node is still
// returned, just with Capacity.Known == false.
func TestToolNodeListCapacityUnknownOnHealthError(t *testing.T) {
	fc := newFakeClient()
	fc.nodes = []client.NodeStatus{{Node: store.Node{Name: "web1", Pool: "default"}}}
	fc.healthErr = fmt.Errorf("health unavailable")
	cs := connectTestServer(t, fc)

	res := callTool(t, cs, "lwd_node_list", map[string]any{})
	if res.IsError {
		t.Fatalf("lwd_node_list should tolerate a health fetch error, got tool error: %+v", res.Content)
	}
	var out lwdNodeListOutput
	decodeStructured(t, res, &out)
	if len(out.Nodes) != 1 || out.Nodes[0].Capacity.Known {
		t.Errorf("unexpected Nodes: %+v", out.Nodes)
	}
}
