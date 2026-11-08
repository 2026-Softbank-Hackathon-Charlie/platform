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

// deployMemory holds apps, their added branches, deployments, their events
// and webhook deliveries. Its zero value is ready. The store's mutex guards
// it, and it numbers events with its own sequence, like the separate
// bigserial in Postgres.
type deployMemory struct {
	apps        map[string]store.App
	branches    map[string]map[string]store.AppBranch // by app, then branch
	deployments map[string]store.Deployment
	events      []store.DeploymentEvent // in Seq order
	seq         int64
	deliveries  map[string]bool
	databases   map[string]store.AppDatabase // by app (ADR 0030)
}

func copyApp(a store.App) store.App {
	a.Environments = append([]string{}, a.Environments...)
	a.Spec = bytes.Clone(a.Spec)
	return a
}

func copyAppBranch(b store.AppBranch) store.AppBranch {
	b.Spec = bytes.Clone(b.Spec)
	return b
}

func copyDeployment(d store.Deployment) store.Deployment {
	d.Spec = bytes.Clone(d.Spec)
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
	return m.createApp(a)
}

// createApp stores a cleaned and checked app. The caller holds the mutex.
func (m *memory) createApp(a store.App) error {
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

func (m *memory) CreateAppWithDatabase(_ context.Context, a store.App, d store.AppDatabase) error {
	a, err := a.Clean()
	if err != nil {
		return err
	}
	if err := checkJSON("app spec", a.Spec); err != nil {
		return err
	}
	d = d.Clean()
	if d.PrimaryEnv == "" || d.PeerEnv == "" || d.PrimaryEnv == d.PeerEnv {
		return fmt.Errorf("storetest: the database pair of app %s needs two different environments", a.Name)
	}
	if d.SourceKind != store.DBSourcePeer && d.SourceKind != store.DBSourceExternal {
		return fmt.Errorf("storetest: the database pair of app %s has source kind %q", a.Name, d.SourceKind)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.deploy.databases[a.Name]; ok {
		return fmt.Errorf("%w: app %s already has a database pair", store.ErrConflict, a.Name)
	}
	if err := m.createApp(a); err != nil {
		return err
	}
	if m.deploy.databases == nil {
		m.deploy.databases = map[string]store.AppDatabase{}
	}
	d.App, d.CreatedAt = a.Name, pgTime(d.CreatedAt)
	m.deploy.databases[d.App] = d
	return nil
}

func (m *memory) GetAppDatabase(_ context.Context, app string) (store.AppDatabase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deploy.databases[app]
	if !ok {
		return store.AppDatabase{}, fmt.Errorf("%w: database pair of app %s", store.ErrNotFound, app)
	}
	return d.Clean(), nil // Clean copies the byte slices
}

func (m *memory) NextPeerGeneration(_ context.Context, app string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deploy.databases[app]
	if !ok {
		return 0, fmt.Errorf("%w: database pair of app %s", store.ErrNotFound, app)
	}
	d.PeerGeneration++
	m.deploy.databases[app] = d
	return d.PeerGeneration, nil
}

func (m *memory) SetAppDatabaseSourceKind(_ context.Context, app, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deploy.databases[app]
	if !ok {
		return fmt.Errorf("%w: database pair of app %s", store.ErrNotFound, app)
	}
	if d.SourceKind != from {
		return fmt.Errorf("%w: database pair of app %s is %s, not %s", store.ErrConflict, app, d.SourceKind, from)
	}
	d.SourceKind = to
	m.deploy.databases[app] = d
	return nil
}

func (m *memory) SetAppDatabaseReplicationPassword(_ context.Context, app string, nonce, ciphertext []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deploy.databases[app]
	if !ok {
		return fmt.Errorf("%w: database pair of app %s", store.ErrNotFound, app)
	}
	d.ReplicationPasswordNonce, d.ReplicationPasswordCiphertext = bytes.Clone(nonce), bytes.Clone(ciphertext)
	m.deploy.databases[app] = d
	return nil
}

func (m *memory) ListAppDatabases(_ context.Context) ([]store.AppDatabase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.AppDatabase, 0, len(m.deploy.databases))
	for _, d := range m.deploy.databases {
		out = append(out, d.Clean())
	}
	slices.SortFunc(out, func(a, b store.AppDatabase) int { return cmp.Compare(a.App, b.App) })
	return out, nil
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
	if _, ok := m.deploy.branches[a.Name][a.Branch]; ok {
		return fmt.Errorf("%w: %s is an added branch of app %s", store.ErrBranchExists, a.Branch, a.Name)
	}
	cur.Branch, cur.AutoDeploy = a.Branch, a.AutoDeploy
	cur.Spec, cur.SpecCommit = bytes.Clone(a.Spec), a.SpecCommit
	m.deploy.apps[a.Name] = cur
	return nil
}

func (m *memory) SetAppEnvironments(_ context.Context, name string, from, to []string) error {
	if !store.Clean(name) {
		return fmt.Errorf("%w: app %q", store.ErrNotFound, name)
	}
	if len(to) == 0 {
		return fmt.Errorf("storetest: app %s keeps at least one environment", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.deploy.apps[name]
	if !ok {
		return fmt.Errorf("%w: app %s", store.ErrNotFound, name)
	}
	clean := func(list []string) []string {
		out := make([]string, len(list))
		for i, e := range list {
			out[i] = store.CleanText(e)
		}
		return out
	}
	if !slices.Equal(cur.Environments, clean(from)) {
		return fmt.Errorf("%w: app %s is in %v, not %v", store.ErrConflict, name, cur.Environments, from)
	}
	cur.Environments = clean(to)
	m.deploy.apps[name] = cur
	return nil
}

func (m *memory) DeleteApp(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.deploy.apps[name]; !ok {
		return fmt.Errorf("%w: app %s", store.ErrNotFound, name)
	}
	delete(m.deploy.apps, name)
	delete(m.deploy.branches, name)
	delete(m.deploy.databases, name)
	m.secrets.deleteSecrets(func(k secretKey) bool { return k.app == name })
	m.deleteDeployments(func(d store.Deployment) bool { return d.App == name })
	return nil
}

// deleteDeployments deletes the deployments gone reports, and their events.
// The caller holds the mutex.
func (m *memory) deleteDeployments(gone func(store.Deployment) bool) {
	ids := map[string]bool{}
	for id, d := range m.deploy.deployments {
		if gone(d) {
			ids[id] = true
			delete(m.deploy.deployments, id)
		}
	}
	m.deploy.events = slices.DeleteFunc(m.deploy.events, func(e store.DeploymentEvent) bool { return ids[e.DeploymentID] })
}

func (m *memory) CreateAppBranch(_ context.Context, b store.AppBranch, maxBranches int) error {
	if !store.Clean(b.App) {
		return fmt.Errorf("%w: app %q", store.ErrNotFound, b.App)
	}
	b, err := b.Clean()
	if err != nil {
		return err
	}
	if err := checkJSON("app branch spec", b.Spec); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.deploy.apps[b.App]
	if !ok {
		return fmt.Errorf("%w: app %s", store.ErrNotFound, b.App)
	}
	if _, added := m.deploy.branches[b.App][b.Branch]; added || b.Branch == a.Branch {
		return fmt.Errorf("%w: app %s has branch %s", store.ErrBranchExists, b.App, b.Branch)
	}
	_, taken := m.deploy.apps[b.Instance]
	for _, branches := range m.deploy.branches {
		for _, other := range branches {
			taken = taken || other.Instance == b.Instance
		}
	}
	if taken {
		return fmt.Errorf("%w: instance name %s is taken", store.ErrInstanceExists, b.Instance)
	}
	if n := len(m.deploy.branches[b.App]); n >= maxBranches {
		return fmt.Errorf("%w: app %s has %d added branches", store.ErrInstanceLimit, b.App, n)
	}
	if m.deploy.branches == nil {
		m.deploy.branches = map[string]map[string]store.AppBranch{}
	}
	if m.deploy.branches[b.App] == nil {
		m.deploy.branches[b.App] = map[string]store.AppBranch{}
	}
	b = copyAppBranch(b)
	b.CreatedAt = pgTime(b.CreatedAt)
	m.deploy.branches[b.App][b.Branch] = b
	return nil
}

func (m *memory) ListAppBranches(_ context.Context, app string) ([]store.AppBranch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.AppBranch, 0, len(m.deploy.branches[app]))
	for _, b := range m.deploy.branches[app] {
		out = append(out, copyAppBranch(b))
	}
	slices.SortFunc(out, func(a, b store.AppBranch) int { return cmp.Compare(a.Branch, b.Branch) })
	return out, nil
}

func (m *memory) UpdateAppBranch(_ context.Context, b store.AppBranch) error {
	if !store.Clean(b.App) || !store.Clean(b.Branch) {
		return fmt.Errorf("%w: app %q branch %q", store.ErrNotFound, b.App, b.Branch)
	}
	b, err := b.Clean()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.deploy.branches[b.App][b.Branch]
	if !ok {
		return fmt.Errorf("%w: app %s branch %s", store.ErrNotFound, b.App, b.Branch)
	}
	cur.AutoDeploy, cur.Spec, cur.SpecCommit = b.AutoDeploy, bytes.Clone(b.Spec), b.SpecCommit
	m.deploy.branches[b.App][b.Branch] = cur
	return nil
}

func (m *memory) DeleteAppBranch(_ context.Context, app, branch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.deploy.branches[app][branch]
	if !ok {
		return fmt.Errorf("%w: app %s branch %s", store.ErrNotFound, app, branch)
	}
	delete(m.deploy.branches[app], branch)
	m.secrets.deleteSecrets(func(k secretKey) bool { return k.app == app && k.instance == b.Instance })
	m.deleteDeployments(func(d store.Deployment) bool { return d.App == app && d.Instance == b.Instance })
	return nil
}

func (m *memory) CreateDeployment(_ context.Context, d store.Deployment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.deploy.apps[d.App]
	if !ok {
		return fmt.Errorf("%w: app %s", store.ErrNotFound, d.App)
	}
	if d.Branch != a.Branch || d.Instance != a.Name {
		if b, ok := m.deploy.branches[d.App][d.Branch]; !ok || b.Instance != d.Instance {
			return fmt.Errorf("%w: app %s has no instance %s of branch %s", store.ErrInstanceNotFound, d.App, d.Instance, d.Branch)
		}
	}
	d, err := d.Clean()
	if err != nil {
		return err
	}
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

func (m *memory) ListInstanceDeployments(_ context.Context, instance string, limit int) ([]store.Deployment, error) {
	out := m.listDeployments(func(d store.Deployment) bool { return d.Instance == instance })
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
	d, err := d.Clean()
	if err != nil {
		return store.Deployment{}, err
	}
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
	cur.Spec = d.Spec
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
