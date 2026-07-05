package mcp

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lwd/internal/api"
	"lwd/internal/client"
	"lwd/internal/node"
	"lwd/internal/reconciler"
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

// lwdRequirementsInput mirrors spec.Requirements for the lwd_apply and
// lwd_deploy_git tools' optional requirements argument, used by the
// scheduler (Phase 11a) to pick a node when node/pool are left unset.
type lwdRequirementsInput struct {
	CPU    float64 `json:"cpu,omitempty" jsonschema:"CPU cores required (e.g. 0.5); omitted or 0 means no CPU requirement"`
	Memory string  `json:"memory,omitempty" jsonschema:"memory required, as a size string like \"512M\" or \"2G\"; omitted means no memory requirement"`
}

// lwdApplyInput is the input of lwd_apply. Exactly one of Dir or Toml must
// be set.
type lwdApplyInput struct {
	Dir  string `json:"dir,omitempty" jsonschema:"local directory containing an lwd.toml to deploy (mutually exclusive with toml)"`
	Toml string `json:"toml,omitempty" jsonschema:"inline lwd.toml document to deploy (mutually exclusive with dir)"`
	Node string `json:"node,omitempty" jsonschema:"name of a registered node to place this app on (overrides the toml's own node field); omitted lets lwd schedule it (or pins to \"local\" for the controller)"`
	Pool string `json:"pool,omitempty" jsonschema:"name of a node pool to schedule into when node is omitted; omitted means the \"default\" pool"`
	// Requirements is a pointer so an entirely-omitted argument (vs. an
	// explicit zero-valued {}) is distinguishable: only a non-nil
	// Requirements sets app.Requirements at all.
	Requirements *lwdRequirementsInput `json:"requirements,omitempty" jsonschema:"optional resource requirements (cpu, memory) the scheduler uses when node is omitted"`
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
		if in.Node != "" {
			app.Node = in.Node
		}
		if in.Pool != "" {
			app.Pool = in.Pool
		}
		if in.Requirements != nil {
			app.Requirements = &spec.Requirements{CPU: in.Requirements.CPU, Memory: in.Requirements.Memory}
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
	URL          string                     `json:"url" jsonschema:"the git remote URL to build from"`
	Ref          string                     `json:"ref,omitempty" jsonschema:"the git ref (branch, tag, or commit) to build; defaults to main"`
	Dockerfile   string                     `json:"dockerfile,omitempty" jsonschema:"path to the Dockerfile within the repo; defaults to Dockerfile"`
	Name         string                     `json:"name" jsonschema:"the app name"`
	Domain       string                     `json:"domain" jsonschema:"the domain to route to this app"`
	Port         int                        `json:"port" jsonschema:"the container port the app listens on"`
	Services     []lwdDeployGitServiceInput `json:"services,omitempty" jsonschema:"optional backing services (e.g. a database) to deploy alongside the app"`
	Node         string                     `json:"node,omitempty" jsonschema:"name of a registered node to place this app on; omitted lets lwd schedule it (or pins to \"local\" for the controller)"`
	Pool         string                     `json:"pool,omitempty" jsonschema:"name of a node pool to schedule into when node is omitted; omitted means the \"default\" pool"`
	Requirements *lwdRequirementsInput      `json:"requirements,omitempty" jsonschema:"optional resource requirements (cpu, memory) the scheduler uses when node is omitted"`
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
			Name:     in.Name,
			Domain:   in.Domain,
			Port:     in.Port,
			Replicas: 1,
			Git: &spec.Git{
				URL: in.URL,
				Ref: ref,
			},
			Build: &spec.Build{
				Dockerfile: dockerfile,
			},
			Services: services,
		}
		if in.Node != "" {
			app.Node = in.Node
		}
		if in.Pool != "" {
			app.Pool = in.Pool
		}
		if in.Requirements != nil {
			app.Requirements = &spec.Requirements{CPU: in.Requirements.CPU, Memory: in.Requirements.Memory}
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

// lwdSecretSetInput is the input of lwd_secret_set.
type lwdSecretSetInput struct {
	App   string `json:"app" jsonschema:"the app name"`
	Key   string `json:"key" jsonschema:"the secret's name"`
	Value string `json:"value" jsonschema:"the secret's value"`
}

// lwdSecretSetOutput is the structured result of lwd_secret_set. It
// deliberately omits Value: a secret value must never appear in a tool
// response.
type lwdSecretSetOutput struct {
	OK  bool   `json:"ok"`
	App string `json:"app"`
	Key string `json:"key"`
}

// registerLwdSecretSet adds the lwd_secret_set tool: set (or overwrite) a
// secret value for an app. This mutates live state (it calls the daemon's
// SetSecret), so the MCP host should confirm with the user before invoking
// it. The confirmation response never echoes the value.
func (s *Server) registerLwdSecretSet(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_secret_set",
		Description: "Set (or overwrite) a secret value for a lwd-managed app. The secret value is never returned in the response.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdSecretSetInput) (*sdk.CallToolResult, lwdSecretSetOutput, error) {
		if err := s.client.SetSecret(ctx, in.App, in.Key, in.Value); err != nil {
			return nil, lwdSecretSetOutput{}, err
		}
		return nil, lwdSecretSetOutput{OK: true, App: in.App, Key: in.Key}, nil
	})
}

// lwdSecretListInput is the input of lwd_secret_list.
type lwdSecretListInput struct {
	App string `json:"app" jsonschema:"the app name"`
}

// lwdSecretListOutput is the structured result of lwd_secret_list: secret
// NAMES only, never values.
type lwdSecretListOutput struct {
	Names []string `json:"names"`
}

// registerLwdSecretList adds the lwd_secret_list tool: the names of secrets
// set for an app. It never returns secret values, so it is safe to annotate
// readOnlyHint.
func (s *Server) registerLwdSecretList(srv *sdk.Server) {
	readOnly := true
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_secret_list",
		Description: "List the names of secrets set for a lwd-managed app. Values are never returned.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: readOnly},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdSecretListInput) (*sdk.CallToolResult, lwdSecretListOutput, error) {
		names, err := s.client.ListSecrets(ctx, in.App)
		if err != nil {
			return nil, lwdSecretListOutput{}, err
		}
		return nil, lwdSecretListOutput{Names: names}, nil
	})
}

// lwdSecretDeleteInput is the input of lwd_secret_delete.
type lwdSecretDeleteInput struct {
	App string `json:"app" jsonschema:"the app name"`
	Key string `json:"key" jsonschema:"the secret's name"`
}

// lwdSecretDeleteOutput is the structured result of lwd_secret_delete.
type lwdSecretDeleteOutput struct {
	OK  bool   `json:"ok"`
	App string `json:"app"`
	Key string `json:"key"`
}

// registerLwdSecretDelete adds the lwd_secret_delete tool: remove a secret
// from an app. Annotated destructiveHint, consistent with lwd_remove, so the
// MCP host prompts for confirmation before calling it.
func (s *Server) registerLwdSecretDelete(srv *sdk.Server) {
	destructive := true
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_secret_delete",
		Description: "Delete a secret from a lwd-managed app. This cannot be undone.",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdSecretDeleteInput) (*sdk.CallToolResult, lwdSecretDeleteOutput, error) {
		if err := s.client.DeleteSecret(ctx, in.App, in.Key); err != nil {
			return nil, lwdSecretDeleteOutput{}, err
		}
		return nil, lwdSecretDeleteOutput{OK: true, App: in.App, Key: in.Key}, nil
	})
}

// lwdNodeInfo is one registered node's registry row + live transport/
// reachability (client.NodeStatus, as returned by lwd_node_list before
// Phase 11a Task 8) plus its live Capacity snapshot, sourced from the
// daemon's health snapshot (client.Health) rather than client.Nodes — see
// reconciler.NodeHealth.Capacity. Capacity is zero-valued
// (Capacity.Known == false) if no health snapshot is available at all, or if
// this node has no entry in it yet (e.g. no reconcile pass has run) — this
// never fails the tool call, since node/pool info is still useful without
// capacity.
type lwdNodeInfo struct {
	client.NodeStatus
	Capacity node.Capacity `json:"capacity"`
}

// lwdNodeListOutput is the structured result of lwd_node_list.
type lwdNodeListOutput struct {
	Nodes []lwdNodeInfo `json:"nodes"`
}

// registerLwdNodeList adds the lwd_node_list tool: every registered node
// (ssh host, mesh address, agent URL if any, pool) plus its live transport,
// reachability, and best-effort capacity, as reported by the daemon.
func (s *Server) registerLwdNodeList(srv *sdk.Server) {
	readOnly := true
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_node_list",
		Description: "List every registered node, its ssh host/mesh address/agent URL/pool, and its live transport (agent/ssh), reachability, and capacity (CPU/memory/disk).",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: readOnly},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ any) (*sdk.CallToolResult, lwdNodeListOutput, error) {
		nodes, err := s.client.Nodes(ctx)
		if err != nil {
			return nil, lwdNodeListOutput{}, err
		}
		// Capacity lives in the health snapshot, not the /nodes response (see
		// client.NodeStatus vs reconciler.NodeHealth). A failed fetch is
		// tolerated: every node is still returned, just with Capacity.Known
		// == false, rather than failing the whole tool call over a
		// best-effort enrichment.
		capByName := map[string]node.Capacity{}
		if h, herr := s.client.Health(ctx); herr == nil {
			for _, nh := range h.Nodes {
				capByName[nh.Name] = nh.Capacity
			}
		}
		out := make([]lwdNodeInfo, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, lwdNodeInfo{NodeStatus: n, Capacity: capByName[n.Name]})
		}
		return nil, lwdNodeListOutput{Nodes: out}, nil
	})
}

// lwdNodeAddInput is the input of lwd_node_add.
type lwdNodeAddInput struct {
	Name     string `json:"name" jsonschema:"the node's name, used as the node= value in lwd.toml"`
	SSHHost  string `json:"ssh_host" jsonschema:"anything ssh accepts for this node (user@host, or a ~/.ssh/config Host alias)"`
	MeshAddr string `json:"mesh_addr" jsonschema:"the WireGuard mesh address the controller reaches this node's app traffic at"`
	AgentURL string `json:"agent_url,omitempty" jsonschema:"base URL of this node's lwd-agent (e.g. http://<mesh-addr>:8078); omit if the node has no agent registered"`
	Pool     string `json:"pool,omitempty" jsonschema:"the node pool to register this node into; omit for the \"default\" pool"`
}

// lwdNodeAddOutput is the structured result of lwd_node_add.
type lwdNodeAddOutput struct {
	OK   bool   `json:"ok"`
	Name string `json:"name"`
}

// registerLwdNodeAdd adds the lwd_node_add tool: register (or update) a node
// in the daemon's registry. This mutates live state, so the MCP host should
// confirm with the user before invoking it.
func (s *Server) registerLwdNodeAdd(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_node_add",
		Description: "Register (or update) a node lwd can place apps on: name, ssh host, mesh address, an optional lwd-agent URL, and an optional pool.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdNodeAddInput) (*sdk.CallToolResult, lwdNodeAddOutput, error) {
		if err := s.client.AddNode(ctx, in.Name, in.SSHHost, in.MeshAddr, in.AgentURL, in.Pool); err != nil {
			return nil, lwdNodeAddOutput{}, err
		}
		return nil, lwdNodeAddOutput{OK: true, Name: in.Name}, nil
	})
}

// lwdNodeRemoveInput is the input of lwd_node_remove.
type lwdNodeRemoveInput struct {
	Name string `json:"name" jsonschema:"the node's name"`
}

// lwdNodeRemoveOutput is the structured result of lwd_node_remove.
type lwdNodeRemoveOutput struct {
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
}

// registerLwdNodeRemove adds the lwd_node_remove tool: deregister a node.
// Annotated destructiveHint, consistent with lwd_remove/lwd_secret_delete, so
// the MCP host prompts for confirmation before calling it.
func (s *Server) registerLwdNodeRemove(srv *sdk.Server) {
	destructive := true
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_node_remove",
		Description: "Deregister a node from lwd. Apps already placed on it are not moved or removed.",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdNodeRemoveInput) (*sdk.CallToolResult, lwdNodeRemoveOutput, error) {
		if err := s.client.RemoveNode(ctx, in.Name); err != nil {
			return nil, lwdNodeRemoveOutput{}, err
		}
		return nil, lwdNodeRemoveOutput{Name: in.Name, Removed: true}, nil
	})
}

// lwdNodeTargetInput is the input shared by lwd_node_drain, lwd_node_evacuate,
// and lwd_node_uncordon: just the target node's name.
type lwdNodeTargetInput struct {
	Name string `json:"name" jsonschema:"the node's name"`
}

// registerLwdNodeDrain adds the lwd_node_drain tool: cordon a node (exclude
// it from future scheduler placement) THEN evacuate every scheduler-placed
// surface currently on it onto some other fitting node — the two-step "take
// this node out of service" operation. Pinned surfaces and compose/backing
// stacks are left running untouched (see reconciler.EvacuateNode).
// Annotated destructiveHint, since surfaces actually move (and their old
// containers are torn down) — the MCP host should confirm with the user
// before invoking it.
func (s *Server) registerLwdNodeDrain(srv *sdk.Server) {
	destructive := true
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_node_drain",
		Description: "Cordon a node (exclude it from future scheduler placement) THEN move every scheduler-placed surface off it onto another fitting node. Pinned surfaces and compose apps are left untouched.",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdNodeTargetInput) (*sdk.CallToolResult, reconciler.EvacuateResult, error) {
		res, err := s.client.Drain(ctx, in.Name)
		if err != nil {
			return nil, reconciler.EvacuateResult{}, err
		}
		return nil, res, nil
	})
}

// registerLwdNodeEvacuate adds the lwd_node_evacuate tool: move every
// scheduler-placed surface off a node onto some other fitting node, WITHOUT
// cordoning it first — unlike lwd_node_drain, new placements can still land
// on it afterward. Annotated destructiveHint for the same reason as drain:
// surfaces actually move.
func (s *Server) registerLwdNodeEvacuate(srv *sdk.Server) {
	destructive := true
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_node_evacuate",
		Description: "Move every scheduler-placed surface off a node onto another fitting node, without cordoning it (new placements may still land on it afterward). Pinned surfaces and compose apps are left untouched.",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdNodeTargetInput) (*sdk.CallToolResult, reconciler.EvacuateResult, error) {
		res, err := s.client.Evacuate(ctx, in.Name)
		if err != nil {
			return nil, reconciler.EvacuateResult{}, err
		}
		return nil, res, nil
	})
}

// lwdNodeUncordonOutput is the structured result of lwd_node_uncordon.
type lwdNodeUncordonOutput struct {
	OK   bool   `json:"ok"`
	Name string `json:"name"`
}

// registerLwdNodeUncordon adds the lwd_node_uncordon tool: clear a node's
// cordon, making it eligible for scheduler placement again. This is
// deliberately NOT annotated destructiveHint — unlike drain/evacuate, it
// never moves or touches anything already deployed, only lifts a placement
// restriction.
func (s *Server) registerLwdNodeUncordon(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lwd_node_uncordon",
		Description: "Clear a node's cordon, making it eligible for scheduler placement again. Never moves or touches anything already deployed on it.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in lwdNodeTargetInput) (*sdk.CallToolResult, lwdNodeUncordonOutput, error) {
		if err := s.client.Uncordon(ctx, in.Name); err != nil {
			return nil, lwdNodeUncordonOutput{}, err
		}
		return nil, lwdNodeUncordonOutput{OK: true, Name: in.Name}, nil
	})
}
