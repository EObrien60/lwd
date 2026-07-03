// Package reconciler makes the running state match a desired app spec.
// It is written entirely against node.Node and store.Store so it can be tested
// with no Docker daemon.
package reconciler

import (
	"context"
	"fmt"
	"time"

	"lwd/internal/node"
	"lwd/internal/spec"
	"lwd/internal/store"
)

// Reconciler applies desired app specs against a node, recording history.
type Reconciler struct {
	node  node.Node
	store *store.Store
}

// New returns a Reconciler bound to a node and store.
func New(n node.Node, s *store.Store) *Reconciler {
	return &Reconciler{node: n, store: s}
}

func containerName(app *spec.App) string { return "lwd-" + app.Name }

// Apply reconciles one app: ensure image, recreate the container, health-check,
// and record the deployment. On health failure the new container is removed and
// the app is left with no running deployment (MVP recreate semantics; blue-green
// arrives with the router in a later plan).
func (r *Reconciler) Apply(ctx context.Context, app *spec.App) (*store.Deployment, error) {
	if err := app.Validate(); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}

	if err := r.node.EnsureImage(ctx, app.Image); err != nil {
		return nil, fmt.Errorf("ensure image: %w", err)
	}

	label := map[string]string{"lwd.app": app.Name}

	// Remove any existing containers for this app (recreate).
	existing, err := r.node.ListContainers(ctx, label)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	for _, c := range existing {
		if err := r.node.RemoveContainer(ctx, c.ID); err != nil {
			return nil, fmt.Errorf("remove old container %s: %w", c.ID, err)
		}
	}

	// Start the new container.
	c, err := r.node.RunContainer(ctx, node.RunSpec{
		Name:   containerName(app),
		Image:  app.Image,
		Env:    app.Env,
		Labels: label,
		Port:   app.Port,
	})
	if err != nil {
		return nil, fmt.Errorf("run container: %w", err)
	}

	// Health check the new container.
	hErr := r.node.Health(ctx, c, node.HealthSpec{Path: app.Health.Path, Timeout: app.Health.Timeout})
	if hErr != nil {
		_ = r.node.RemoveContainer(ctx, c.ID)
		if prev, err := r.store.CurrentDeployment(app.Name); err == nil && prev != nil {
			_ = r.store.SetStatus(prev.ID, store.StatusRetired)
		}
		_, _ = r.store.RecordDeployment(store.Deployment{
			App: app.Name, Image: app.Image, ContainerID: c.ID,
			Status: store.StatusFailed, CreatedAt: time.Now(),
		})
		return nil, fmt.Errorf("health check failed: %w", hErr)
	}

	// Retire the previous running deployment, if any.
	if prev, err := r.store.CurrentDeployment(app.Name); err == nil && prev != nil {
		_ = r.store.SetStatus(prev.ID, store.StatusRetired)
	}

	// Record the new running deployment.
	dep := store.Deployment{
		App: app.Name, Image: app.Image, ContainerID: c.ID,
		Status: store.StatusRunning, CreatedAt: time.Now(),
	}
	id, err := r.store.RecordDeployment(dep)
	if err != nil {
		return nil, fmt.Errorf("record deployment: %w", err)
	}
	dep.ID = id
	return &dep, nil
}
