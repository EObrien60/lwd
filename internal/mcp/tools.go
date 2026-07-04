package mcp

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lwd/internal/api"
	"lwd/internal/spec"
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

// lwdDeploymentOutput is the structured result shared by every tool that
// triggers or reports a deployment (lwd_apply, lwd_deploy_git,
// lwd_rollback): the essentials a caller needs to know the outcome, without
// the internal Spec/Compose snapshots store.Deployment carries for rollback.
type lwdDeploymentOutput struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	Status      string `json:"status"`
	ContainerID string `json:"container_id"`
}

func deploymentOutput(d *store.Deployment) lwdDeploymentOutput {
	return lwdDeploymentOutput{
		Name:        d.App,
		Image:       d.Image,
		Status:      d.Status,
		ContainerID: d.ContainerID,
	}
}

// lwdApplyInput is the input of lwd_apply. Exactly one of Dir or Toml must
// be set.
type lwdApplyInput struct {
	Dir  string `json:"dir,omitempty" jsonschema:"local directory containing an lwd.toml to deploy (mutually exclusive with toml)"`
	Toml string `json:"toml,omitempty" jsonschema:"inline lwd.toml document to deploy (mutually exclusive with dir)"`
}

// registerLwdApply adds the lwd_apply tool: deploy an app from either a
// local directory's lwd.toml or an inline toml document. This mutates live
// state (it calls the daemon's Apply), so the MCP host should confirm with
// the user before invoking it.
func (s *Server) registerLwdApply(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_apply",
		Description: "Deploy an app from an lwd.toml, either read from a local directory (dir) or supplied inline (toml). Exactly one of dir or toml is required.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdApplyInput) (*sdk.CallToolResult, lwdDeploymentOutput, error) {
		haveDir := in.Dir != ""
		haveToml := in.Toml != ""
		if haveDir == haveToml {
			return nil, lwdDeploymentOutput{}, fmt.Errorf("exactly one of dir or toml is required")
		}

		var (
			app *spec.App
			err error
		)
		if haveToml {
			app, err = spec.Parse([]byte(in.Toml))
		} else {
			app, err = spec.Load(in.Dir)
		}
		if err != nil {
			return nil, lwdDeploymentOutput{}, err
		}
		if err := app.Validate(); err != nil {
			return nil, lwdDeploymentOutput{}, err
		}

		dep, err := s.client.Apply(ctx, app)
		if err != nil {
			return nil, lwdDeploymentOutput{}, err
		}
		return nil, deploymentOutput(dep), nil
	})
}

// lwdDeployGitServiceInput mirrors spec.Service for the lwd_deploy_git tool's
// services argument.
type lwdDeployGitServiceInput struct {
	Name    string            `json:"name" jsonschema:"the service name"`
	Image   string            `json:"image" jsonschema:"the service's container image"`
	Command string            `json:"command,omitempty" jsonschema:"override command for the service's container"`
	Env     map[string]string `json:"env,omitempty" jsonschema:"plain (non-secret) environment variables for the service"`
	Secrets []string          `json:"secrets,omitempty" jsonschema:"names of previously-set secrets to inject as environment variables"`
	Volume  string            `json:"volume,omitempty" jsonschema:"named volume to mount for persistent data"`
}

// lwdDeployGitInput is the input of lwd_deploy_git.
type lwdDeployGitInput struct {
	URL        string                     `json:"url" jsonschema:"the git remote URL to build from"`
	Ref        string                     `json:"ref,omitempty" jsonschema:"the git ref (branch, tag, or commit) to build; defaults to main"`
	Dockerfile string                     `json:"dockerfile,omitempty" jsonschema:"path to the Dockerfile within the repo; defaults to Dockerfile"`
	Name       string                     `json:"name" jsonschema:"the app name"`
	Domain     string                     `json:"domain" jsonschema:"the domain to route to this app"`
	Port       int                        `json:"port" jsonschema:"the container port the app listens on"`
	Services   []lwdDeployGitServiceInput `json:"services,omitempty" jsonschema:"optional backing services (e.g. a database) to deploy alongside the app"`
}

const (
	defaultGitRef        = "main"
	defaultGitDockerfile = "Dockerfile"
)

// registerLwdDeployGit adds the lwd_deploy_git tool: build a spec.App for a
// git-sourced deploy from discrete fields (rather than requiring the caller
// to hand-author an lwd.toml), validate it, and apply it. This mutates live
// state (it calls the daemon's Apply), so the MCP host should confirm with
// the user before invoking it.
func (s *Server) registerLwdDeployGit(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_deploy_git",
		Description: "Deploy an app built from a git repository: clone url at ref, build dockerfile, and route domain:port. Optionally deploy backing services alongside it.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdDeployGitInput) (*sdk.CallToolResult, lwdDeploymentOutput, error) {
		ref := in.Ref
		if ref == "" {
			ref = defaultGitRef
		}
		dockerfile := in.Dockerfile
		if dockerfile == "" {
			dockerfile = defaultGitDockerfile
		}

		var services []spec.Service
		for _, svc := range in.Services {
			services = append(services, spec.Service{
				Name:    svc.Name,
				Image:   svc.Image,
				Command: svc.Command,
				Env:     svc.Env,
				Secrets: svc.Secrets,
				Volume:  svc.Volume,
			})
		}

		app := &spec.App{
			Name:   in.Name,
			Domain: in.Domain,
			Port:   in.Port,
			Git: &spec.Git{
				URL: in.URL,
				Ref: ref,
			},
			Build: &spec.Build{
				Dockerfile: dockerfile,
			},
			Services: services,
		}
		if err := app.Validate(); err != nil {
			return nil, lwdDeploymentOutput{}, err
		}

		dep, err := s.client.Apply(ctx, app)
		if err != nil {
			return nil, lwdDeploymentOutput{}, err
		}
		return nil, deploymentOutput(dep), nil
	})
}

// lwdRollbackInput is the input of lwd_rollback.
type lwdRollbackInput struct {
	Name string `json:"name" jsonschema:"the app name"`
}

// registerLwdRollback adds the lwd_rollback tool: revert an app to its
// previous deployment. This mutates live state (it calls the daemon's
// Rollback), so the MCP host should confirm with the user before invoking
// it.
func (s *Server) registerLwdRollback(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_rollback",
		Description: "Roll back a lwd-managed app to its previous deployment.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdRollbackInput) (*sdk.CallToolResult, lwdDeploymentOutput, error) {
		dep, err := s.client.Rollback(ctx, in.Name)
		if err != nil {
			return nil, lwdDeploymentOutput{}, err
		}
		return nil, deploymentOutput(dep), nil
	})
}

// lwdRemoveInput is the input of lwd_remove.
type lwdRemoveInput struct {
	Name string `json:"name" jsonschema:"the app name"`
}

// lwdRemoveOutput is the structured result of lwd_remove.
type lwdRemoveOutput struct {
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
}

// registerLwdRemove adds the lwd_remove tool: permanently stop and remove a
// lwd-managed app. Annotated destructiveHint so the MCP host prompts for
// confirmation before calling it; unlike the read tools, it is NOT annotated
// readOnlyHint.
func (s *Server) registerLwdRemove(srv *sdk.Server) {
	destructive := true
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_remove",
		Description: "Permanently stop and remove a lwd-managed app. This cannot be undone.",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdRemoveInput) (*sdk.CallToolResult, lwdRemoveOutput, error) {
		if err := s.client.Remove(ctx, in.Name); err != nil {
			return nil, lwdRemoveOutput{}, err
		}
		return nil, lwdRemoveOutput{Name: in.Name, Removed: true}, nil
	})
}
