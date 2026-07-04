package mcp

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lwd/internal/api"
	"lwd/internal/store"
)

// lwdListOutput is the structured result of lwd_list.
type lwdListOutput struct {
	Apps []api.AppStatus `json:"apps"`
}

// deploymentSummary is a lean, non-sensitive view of a store.Deployment: it
// deliberately omits Spec/Compose, which are internal snapshots used for
// rollback, not something a tool caller needs.
type deploymentSummary struct {
	ID        int64     `json:"id"`
	Image     string    `json:"image"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func summarizeDeployments(deps []store.Deployment) []deploymentSummary {
	out := make([]deploymentSummary, 0, len(deps))
	for _, d := range deps {
		out = append(out, deploymentSummary{
			ID:        d.ID,
			Image:     d.Image,
			Status:    d.Status,
			CreatedAt: d.CreatedAt,
		})
	}
	return out
}

// lwdStatusInput is the input of lwd_status.
type lwdStatusInput struct {
	Name string `json:"name" jsonschema:"the app name"`
}

// lwdStatusOutput is the structured result of lwd_status.
type lwdStatusOutput struct {
	Status  *api.AppStatus      `json:"status"`
	History []deploymentSummary `json:"history"`
}

// registerLwdStatus adds the lwd_status tool: the current status of one app
// plus its deployment history, newest first.
func (s *Server) registerLwdStatus(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_status",
		Description: "Get the current status and deployment history of a single lwd-managed app.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdStatusInput) (*sdk.CallToolResult, lwdStatusOutput, error) {
		apps, err := s.client.Apps(ctx)
		if err != nil {
			return nil, lwdStatusOutput{}, err
		}
		var found *api.AppStatus
		for i := range apps {
			if apps[i].Name == in.Name {
				found = &apps[i]
				break
			}
		}
		if found == nil {
			return nil, lwdStatusOutput{}, fmt.Errorf("app %q not found", in.Name)
		}
		history, err := s.client.History(ctx, in.Name)
		if err != nil {
			return nil, lwdStatusOutput{}, err
		}
		return nil, lwdStatusOutput{Status: found, History: summarizeDeployments(history)}, nil
	})
}

// lwdLogsInput is the input of lwd_logs.
type lwdLogsInput struct {
	Name string `json:"name" jsonschema:"the app name"`
	Tail int    `json:"tail,omitempty" jsonschema:"maximum number of trailing lines to return (default 200)"`
}

// lwdLogsOutput is the structured result of lwd_logs.
type lwdLogsOutput struct {
	Logs string `json:"logs"`
}

const defaultLogsTail = 200

// registerLwdLogs adds the lwd_logs tool: the app's captured (non-following)
// logs, trimmed to the last `tail` lines (default defaultLogsTail).
func (s *Server) registerLwdLogs(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_logs",
		Description: "Get the most recent logs for a lwd-managed app.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdLogsInput) (*sdk.CallToolResult, lwdLogsOutput, error) {
		tail := in.Tail
		if tail <= 0 {
			tail = defaultLogsTail
		}
		var buf bytes.Buffer
		if err := s.client.Logs(ctx, in.Name, false, &buf); err != nil {
			return nil, lwdLogsOutput{}, err
		}
		return nil, lwdLogsOutput{Logs: lastLines(buf.String(), tail)}, nil
	})
}

// lastLines returns the last n lines of text, preserving a trailing newline
// when the input had one. If text has n or fewer lines, it is returned
// unchanged.
func lastLines(text string, n int) string {
	trimmed := strings.TrimSuffix(text, "\n")
	hadTrailingNewline := trimmed != text
	lines := strings.Split(trimmed, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if hadTrailingNewline {
		out += "\n"
	}
	return out
}

// lwdHistoryInput is the input of lwd_history.
type lwdHistoryInput struct {
	Name string `json:"name" jsonschema:"the app name"`
}

// lwdHistoryOutput is the structured result of lwd_history.
type lwdHistoryOutput struct {
	Deployments []deploymentSummary `json:"deployments"`
}

// registerLwdHistory adds the lwd_history tool: the recorded deployments for
// an app (id, image, status, created time), newest first.
func (s *Server) registerLwdHistory(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_history",
		Description: "List recorded deployments for a lwd-managed app.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdHistoryInput) (*sdk.CallToolResult, lwdHistoryOutput, error) {
		history, err := s.client.History(ctx, in.Name)
		if err != nil {
			return nil, lwdHistoryOutput{}, err
		}
		return nil, lwdHistoryOutput{Deployments: summarizeDeployments(history)}, nil
	})
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
