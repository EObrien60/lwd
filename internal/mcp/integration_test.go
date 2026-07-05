package mcp

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lwd/internal/api"
	"lwd/internal/build"
	"lwd/internal/client"
	"lwd/internal/compose"
	"lwd/internal/node"
	"lwd/internal/reconciler"
	"lwd/internal/router"
	"lwd/internal/secrets"
	"lwd/internal/source"
	"lwd/internal/store"
)

// startFakeDaemon runs a real daemon api.Server on a temp unix socket, backed
// by the fake node/router/compose stack (no Docker) plus a real temp SQLite
// store and a real secrets.Store. This mirrors
// internal/web/integration_test.go's helper of the same name so this test can
// exercise the full mcp -> client -> daemon path without Docker.
func startFakeDaemon(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "lwd.sock")

	f := node.NewFake()
	s, err := store.Open(filepath.Join(dir, "lwd.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	rt := router.NewFakeRouter()
	cipher, err := secrets.NewCipher(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("secrets.NewCipher: %v", err)
	}
	secStore := secrets.NewStore(cipher, s)
	// "" is mapped alongside "local" because spec.Parse (Phase 11a) no
	// longer normalizes an unset lwd.toml `node` to "local" — it preserves
	// "" so the (future) scheduler can tell "unset" from "pinned local".
	// FakeResolver, unlike the production RegistryResolver, does not
	// special-case "" on its own, so the apply-without-node path exercised
	// here needs it mapped explicitly.
	daemon := api.New(reconciler.New(node.FakeResolver{"": f, "local": f}, rt, s, secStore, compose.NewFake(), source.NewFake(), build.NewFake()), s, f, rt, secStore, nil)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	httpSrv := &http.Server{Handler: daemon.Handler()}
	go httpSrv.Serve(ln)
	t.Cleanup(func() {
		httpSrv.Close()
		s.Close()
		os.Remove(sock)
	})
	return sock
}

// expectedToolNames is every tool lwd-mcp registers; the design spec and
// README both enumerate this same list.
var expectedToolNames = []string{
	"lwd_list",
	"lwd_status",
	"lwd_logs",
	"lwd_history",
	"lwd_apply",
	"lwd_deploy_git",
	"lwd_rollback",
	"lwd_remove",
	"lwd_secret_set",
	"lwd_secret_list",
	"lwd_secret_delete",
	"lwd_node_list",
	"lwd_node_add",
	"lwd_node_remove",
}

// TestIntegrationMCPClientDaemon drives lwd-mcp's real MCP server (Server,
// backed by a real *client.Client) against a real daemon api.Server on a
// temp unix socket. No Docker is involved: the daemon uses the fake
// node/router/compose stack. This proves the full
// MCP tool call -> internal/client -> daemon chain works end to end.
func TestIntegrationMCPClientDaemon(t *testing.T) {
	sock := startFakeDaemon(t)
	c := client.New(sock)
	srv := NewServer(c).MCP()

	serverTransport, clientTransport := sdk.NewInMemoryTransports()

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, serverTransport) }()

	mcpClient := sdk.NewClient(&sdk.Implementation{Name: "integration-test-client", Version: "0.0.0"}, nil)
	cs, err := mcpClient.Connect(ctx, clientTransport, nil)
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

	// tools/list: every tool from the design spec must be registered.
	lr, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := make(map[string]bool, len(lr.Tools))
	for _, tool := range lr.Tools {
		got[tool.Name] = true
	}
	for _, name := range expectedToolNames {
		if !got[name] {
			t.Errorf("expected tool %q registered, got tools: %v", name, toolNames(lr.Tools))
		}
	}

	// lwd_list against a fresh daemon: empty is fine.
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lwd_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool(lwd_list): %v", err)
	}
	if res.IsError {
		t.Fatalf("lwd_list returned tool error: %+v", res.Content)
	}
	var listOut lwdListOutput
	decodeStructured(t, res, &listOut)
	if len(listOut.Apps) != 0 {
		t.Fatalf("lwd_list before apply = %+v, want empty", listOut.Apps)
	}

	// lwd_apply with a single-service toml deploys through the real daemon.
	const tomlDoc = `
name = "blog"
image = "ghcr.io/example/blog:latest"
domain = "blog.localhost"
port = 8080
`
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lwd_apply", Arguments: map[string]any{"toml": tomlDoc}})
	if err != nil {
		t.Fatalf("CallTool(lwd_apply): %v", err)
	}
	if res.IsError {
		t.Fatalf("lwd_apply returned tool error: %+v", res.Content)
	}
	var applyOut lwdDeploymentOutput
	decodeStructured(t, res, &applyOut)
	if applyOut.Name != "blog" || applyOut.Status != store.StatusRunning {
		t.Fatalf("lwd_apply output = %+v", applyOut)
	}

	// lwd_list now shows the deployed app.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lwd_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool(lwd_list) after apply: %v", err)
	}
	if res.IsError {
		t.Fatalf("lwd_list after apply returned tool error: %+v", res.Content)
	}
	decodeStructured(t, res, &listOut)
	if len(listOut.Apps) != 1 || listOut.Apps[0].Name != "blog" {
		t.Fatalf("lwd_list after apply = %+v, want [blog]", listOut.Apps)
	}
	if listOut.Apps[0].Domain != "blog.localhost" {
		t.Errorf("lwd_list app domain = %q, want %q", listOut.Apps[0].Domain, "blog.localhost")
	}
}

func toolNames(tools []*sdk.Tool) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return names
}
