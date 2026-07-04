package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lwd/internal/api"
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
