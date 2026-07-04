package mcp

import (
	"context"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

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
