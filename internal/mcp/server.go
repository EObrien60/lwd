// Package mcp implements lwd-mcp: a local Model Context Protocol server that
// exposes lwd's daemon operations as MCP tools over stdio. It is a client of
// the daemon's unix socket (internal/client), the same trust boundary as the
// CLI — no new network surface, no daemon changes.
package mcp

import (
	"context"
	"io"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lwd/internal/api"
	"lwd/internal/client"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// ClientIface is the subset of *client.Client that lwd-mcp's tools use. Its
// method signatures must match *client.Client exactly so that value satisfies
// this interface; a fake implements it for handler tests (see
// fake_client_test.go).
type ClientIface interface {
	Apps(ctx context.Context) ([]api.AppStatus, error)
	History(ctx context.Context, name string) ([]store.Deployment, error)
	Logs(ctx context.Context, name string, follow bool, w io.Writer) error
	Apply(ctx context.Context, app *spec.App) (*store.Deployment, error)
	Rollback(ctx context.Context, name string) (*store.Deployment, error)
	Remove(ctx context.Context, name string) error
	SetSecret(ctx context.Context, app, key, value string) error
	ListSecrets(ctx context.Context, app string) ([]string, error)
	DeleteSecret(ctx context.Context, app, key string) error
}

// The real daemon client must satisfy ClientIface. This assertion fails the
// build if internal/client drifts from the interface lwd-mcp depends on.
var _ ClientIface = (*client.Client)(nil)

// Server wires lwd's daemon client to MCP tools served over stdio.
type Server struct {
	client ClientIface
}

// NewServer returns a Server backed by c.
func NewServer(c ClientIface) *Server {
	return &Server{client: c}
}

// MCP builds the go-sdk MCP server with all lwd tools registered.
func (s *Server) MCP() *sdk.Server {
	impl := &sdk.Implementation{
		Name:    "lwd-mcp",
		Title:   "lwd",
		Version: "0.1.0",
	}
	srv := sdk.NewServer(impl, nil)
	s.registerTools(srv)
	return srv
}

// registerTools adds every lwd tool to srv. Read tools (Task 1/2) and
// mutating tools (Task 3/4) are added here as they're implemented; Task 1
// wires the plumbing and one real read tool (lwd_list) to prove it.
func (s *Server) registerTools(srv *sdk.Server) {
	s.registerLwdList(srv)
	s.registerLwdStatus(srv)
	s.registerLwdLogs(srv)
	s.registerLwdHistory(srv)
	s.registerLwdApply(srv)
	s.registerLwdDeployGit(srv)
	s.registerLwdRollback(srv)
	s.registerLwdRemove(srv)
}

// Serve runs the MCP server over stdio until the client disconnects or ctx
// is cancelled. Callers must not write to stdout: it is the MCP protocol
// channel.
func (s *Server) Serve(ctx context.Context) error {
	return s.MCP().Run(ctx, &sdk.StdioTransport{})
}
