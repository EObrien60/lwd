package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lwd/internal/api"
)

// lwdListOutput is the structured result of lwd_list.
type lwdListOutput struct {
	Apps []api.AppStatus `json:"apps"`
}

// registerLwdList adds the lwd_list tool: a read-only overview of every app
// lwd manages (name, image, status, domain). This is the first real tool,
// proving the ClientIface -> go-sdk wiring end to end; more read tools
// (lwd_status, lwd_logs, lwd_history) and mutating tools follow in later
// tasks.
func (s *Server) registerLwdList(srv *sdk.Server) {
	readOnly := true
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_list",
		Description: "List all lwd-managed apps with their current status, image, and domain.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: readOnly},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ any) (*sdk.CallToolResult, lwdListOutput, error) {
		apps, err := s.client.Apps(ctx)
		if err != nil {
			return nil, lwdListOutput{}, err
		}
		return nil, lwdListOutput{Apps: apps}, nil
	})
}
