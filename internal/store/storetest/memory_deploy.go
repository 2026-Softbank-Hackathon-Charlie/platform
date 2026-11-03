package storetest

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// deployMemory holds apps, deployments, their events and webhook deliveries.
// Its zero value is ready. The store's mutex guards it, and it numbers events
// with its own sequence, like the separate bigserial in Postgres.
type deployMemory struct {
	apps        map[string]store.App
	deployments map[string]store.Deployment
	events      []store.DeploymentEvent // in Seq order
	seq         int64
	deliveries  map[string]bool
}

func copyApp(a store.App) store.App {
	a.Environments = append([]string{}, a.Environments...)
	a.Spec = bytes.Clone(a.Spec)
	return a
}

func copyDeployment(d store.Deployment) store.Deployment {
	d.Images = append([]store.DeploymentImage{}, d.Images...)
	d.Environments = append([]store.DeploymentEnvironment{}, d.Environments...)
	d.StartedAt, d.FinishedAt = pgTimePtr(d.StartedAt), pgTimePtr(d.FinishedAt)
	return d
}

func (m *memory) CreateApp(_ context.Context, a store.App) error {
	a, err := a.Clean()
	if err != nil {
		return err
	}
	if err := checkJSON("app spec", a.Spec); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.deploy.apps[a.Name]; ok {
		return fmt.Errorf("%w: app %s exists", store.ErrConflict, a.Name)
	}
	if m.deploy.apps == nil {
		m.deploy.apps = map[string]store.App{}
	}
	a = copyApp(a)
	a.CreatedAt = pgTime(a.CreatedAt)
	m.deploy.apps[a.Name] = a
	return nil
}

func (m *memory) GetApp(_ context.Context, name string) (store.App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.deploy.apps[name]
	if !ok {
		return store.App{}, fmt.Errorf("%w: app %s", store.ErrNotFound, name)
	}
	return copyApp(a), nil
}

func (m *memory) ListApps(_ context.Context) ([]store.App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.App, 0, len(m.deploy.apps))
	for _, a := range m.deploy.apps {
		out = append(out, copyApp(a))
	}
	slices.SortFunc(out, func(a, b store.App) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (m *memory) UpdateApp(_ context.Context, a store.App) error {
	if !store.Clean(a.Name) {
		return fmt.Errorf("%w: app %q", store.ErrNotFound, a.Name)
	}
	a, err := a.Clean()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.deploy.apps[a.Name]
	if !ok {
		return fmt.Errorf("%w: app %s", store.ErrNotFound, a.Name)
	}
	cur.Branch, cur.Environments, cur.AutoDeploy = a.Branch, append([]string{}, a.Environments...), a.AutoDeploy
	cur.Spec, cur.SpecCommit = bytes.Clone(a.Spec), a.SpecCommit
	m.deploy.apps[a.Name] = cur
	return nil
}

func (m *memory) DeleteApp(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.deploy.apps[name]; !ok {
		return fmt.Errorf("%w: app %s", store.ErrNotFound, name)
	}
	delete(m.deploy.apps, name)
	gone := map[string]bool{}
	for id, d := range m.deploy.deployments {
		if d.App == name {
			gone[id] = true
			delete(m.deploy.deployments, id)
		}
	}
	m.deploy.events = slices.DeleteFunc(m.deploy.events, func(e store.DeploymentEvent) bool { return gone[e.DeploymentID] })
	return nil
}

func (m *memory) CreateDeployment(_ context.Context, d store.Deployment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.deploy.apps[d.App]; !ok {
		return fmt.Errorf("%w: app %s", store.ErrNotFound, d.App)
	}
	d = d.Clean()
	if _, ok := m.deploy.deployments[d.ID]; ok {
		return fmt.Errorf("%w: deployment %s exists", store.ErrConflict, d.ID)
	}
	if m.deploy.deployments == nil {
		m.deploy.deployments = map[string]store.Deployment{}
	}
	d = copyDeployment(d)
	d.CreatedAt = pgTime(d.CreatedAt)
	m.deploy.deployments[d.ID] = d
	return nil
}

func (m *memory) GetDeployment(_ context.Context, id string) (store.Deployment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deploy.deployments[id]
	if !ok {
		return store.Deployment{}, fmt.Errorf("%w: deployment %s", store.ErrNotFound, id)
	}
	return copyDeployment(d), nil
}

func (m *memory) ListDeployments(_ context.Context, app string, limit int) ([]store.Deployment, error) {
	out := m.listDeployments(func(d store.Deployment) bool { return d.App == app })
	// Same order as Postgres: created_at DESC, id DESC.
	slices.SortFunc(out, func(a, b store.Deployment) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(b.ID, a.ID))
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memory) ListUnfinishedDeployments(_ context.Context) ([]store.Deployment, error) {
	out := m.listDeployments(func(d store.Deployment) bool { return !store.DeploymentFinished(d.Status) })
	// Same order as Postgres: created_at, id.
	slices.SortFunc(out, func(a, b store.Deployment) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func (m *memory) listDeployments(keep func(store.Deployment) bool) []store.Deployment {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.Deployment{}
	for _, d := range m.deploy.deployments {
		if keep(d) {
			out = append(out, copyDeployment(d))
		}
	}
	return out
}

func (m *memory) UpdateDeployment(_ context.Context, d store.Deployment) (store.Deployment, error) {
	if !store.Clean(d.ID) {
		return store.Deployment{}, fmt.Errorf("%w: deployment %q", store.ErrNotFound, d.ID)
	}
	d = d.Clean()
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.deploy.deployments[d.ID]
	if !ok {
		return store.Deployment{}, fmt.Errorf("%w: deployment %s", store.ErrNotFound, d.ID)
	}
	if store.DeploymentFinished(cur.Status) {
		return store.Deployment{}, fmt.Errorf("%w: deployment is %s", store.ErrConflict, cur.Status)
	}
	cur.Status, cur.Images, cur.Environments = d.Status, d.Images, d.Environments
	cur.Error, cur.BuildID, cur.StartedAt, cur.FinishedAt = d.Error, d.BuildID, d.StartedAt, d.FinishedAt
	cur = copyDeployment(cur)
	m.deploy.deployments[d.ID] = cur
	return copyDeployment(cur), nil
}

func (m *memory) AppendDeploymentEvent(_ context.Context, e store.DeploymentEvent) (store.DeploymentEvent, error) {
	e = e.Clean()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.deploy.deployments[e.DeploymentID]; !ok {
		return store.DeploymentEvent{}, fmt.Errorf("%w: deployment %s", store.ErrNotFound, e.DeploymentID)
	}
	m.deploy.seq++
	e.Seq, e.Time = m.deploy.seq, pgTime(e.Time)
	m.deploy.events = append(m.deploy.events, e)
	return e, nil
}

func (m *memory) ListDeploymentEvents(_ context.Context, deploymentID string, afterSeq int64, limit int) ([]store.DeploymentEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.DeploymentEvent{}
	for _, e := range m.deploy.events {
		if limit > 0 && len(out) == limit {
			break
		}
		if e.DeploymentID == deploymentID && e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memory) RecordWebhookDelivery(_ context.Context, id string, _ time.Time) (bool, error) {
	id = store.CleanText(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deploy.deliveries[id] {
		return false, nil
	}
	if m.deploy.deliveries == nil {
		m.deploy.deliveries = map[string]bool{}
	}
	m.deploy.deliveries[id] = true
	return true, nil
}
