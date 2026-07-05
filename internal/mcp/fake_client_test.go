package mcp

import (
	"context"
	"io"

	"lwd/internal/api"
	"lwd/internal/client"
	"lwd/internal/reconciler"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// fakeClient implements ClientIface with in-memory, settable data and error
// knobs, for testing lwd-mcp's tool handlers without a real daemon. Mirrors
// internal/web's fakeDaemon.
type fakeClient struct {
	apps    []api.AppStatus
	appsErr error

	history    map[string][]store.Deployment
	historyErr error

	logsData string
	logsErr  error

	applied     []*spec.App
	applyResult *store.Deployment
	applyErr    error

	rollbackResult *store.Deployment
	rollbackErr    error

	removed   []string
	removeErr error

	// secrets maps app -> key -> value.
	secrets map[string]map[string]string

	setSecretErr    error
	listSecretsErr  error
	deleteSecretErr error

	nodes    []client.NodeStatus
	nodesErr error

	addedNodes []nodeAddCall
	addNodeErr error

	removedNodes  []string
	removeNodeErr error

	drainResult reconciler.EvacuateResult
	drainErr    error
	drainCalls  []string

	evacuateResult reconciler.EvacuateResult
	evacuateErr    error
	evacuateCalls  []string

	uncordonErr   error
	uncordonCalls []string

	health    reconciler.Health
	healthErr error
}

// nodeAddCall captures the arguments of one AddNode call, so tests can assert
// on them (including agent_url) without a real daemon.
type nodeAddCall struct {
	Name, SSHHost, MeshAddr, AgentURL, Pool string
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		history: make(map[string][]store.Deployment),
		secrets: make(map[string]map[string]string),
	}
}

var _ ClientIface = (*fakeClient)(nil)

func (f *fakeClient) Apps(ctx context.Context) ([]api.AppStatus, error) {
	if f.appsErr != nil {
		return nil, f.appsErr
	}
	return f.apps, nil
}

func (f *fakeClient) History(ctx context.Context, name string) ([]store.Deployment, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return f.history[name], nil
}

func (f *fakeClient) Logs(ctx context.Context, name string, follow bool, w io.Writer) error {
	if f.logsErr != nil {
		return f.logsErr
	}
	_, err := io.WriteString(w, f.logsData)
	return err
}

func (f *fakeClient) Apply(ctx context.Context, app *spec.App) (*store.Deployment, error) {
	f.applied = append(f.applied, app)
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	if f.applyResult != nil {
		return f.applyResult, nil
	}
	return &store.Deployment{App: app.Name, Image: app.Image, Status: store.StatusRunning}, nil
}

func (f *fakeClient) Rollback(ctx context.Context, name string) (*store.Deployment, error) {
	if f.rollbackErr != nil {
		return nil, f.rollbackErr
	}
	if f.rollbackResult != nil {
		return f.rollbackResult, nil
	}
	return &store.Deployment{App: name, Status: store.StatusRunning}, nil
}

func (f *fakeClient) Remove(ctx context.Context, name string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeClient) SetSecret(ctx context.Context, app, key, value string) error {
	if f.setSecretErr != nil {
		return f.setSecretErr
	}
	if f.secrets[app] == nil {
		f.secrets[app] = make(map[string]string)
	}
	f.secrets[app][key] = value
	return nil
}

func (f *fakeClient) ListSecrets(ctx context.Context, app string) ([]string, error) {
	if f.listSecretsErr != nil {
		return nil, f.listSecretsErr
	}
	var names []string
	for k := range f.secrets[app] {
		names = append(names, k)
	}
	return names, nil
}

func (f *fakeClient) DeleteSecret(ctx context.Context, app, key string) error {
	if f.deleteSecretErr != nil {
		return f.deleteSecretErr
	}
	if f.secrets[app] != nil {
		delete(f.secrets[app], key)
	}
	return nil
}

func (f *fakeClient) Nodes(ctx context.Context) ([]client.NodeStatus, error) {
	if f.nodesErr != nil {
		return nil, f.nodesErr
	}
	return f.nodes, nil
}

func (f *fakeClient) AddNode(ctx context.Context, name, sshHost, meshAddr, agentURL, pool string) error {
	if f.addNodeErr != nil {
		return f.addNodeErr
	}
	f.addedNodes = append(f.addedNodes, nodeAddCall{Name: name, SSHHost: sshHost, MeshAddr: meshAddr, AgentURL: agentURL, Pool: pool})
	return nil
}

func (f *fakeClient) RemoveNode(ctx context.Context, name string) error {
	if f.removeNodeErr != nil {
		return f.removeNodeErr
	}
	f.removedNodes = append(f.removedNodes, name)
	return nil
}

func (f *fakeClient) Drain(ctx context.Context, name string) (reconciler.EvacuateResult, error) {
	f.drainCalls = append(f.drainCalls, name)
	if f.drainErr != nil {
		return reconciler.EvacuateResult{}, f.drainErr
	}
	return f.drainResult, nil
}

func (f *fakeClient) Evacuate(ctx context.Context, name string) (reconciler.EvacuateResult, error) {
	f.evacuateCalls = append(f.evacuateCalls, name)
	if f.evacuateErr != nil {
		return reconciler.EvacuateResult{}, f.evacuateErr
	}
	return f.evacuateResult, nil
}

func (f *fakeClient) Uncordon(ctx context.Context, name string) error {
	f.uncordonCalls = append(f.uncordonCalls, name)
	if f.uncordonErr != nil {
		return f.uncordonErr
	}
	return nil
}

func (f *fakeClient) Health(ctx context.Context) (reconciler.Health, error) {
	if f.healthErr != nil {
		return reconciler.Health{}, f.healthErr
	}
	return f.health, nil
}
