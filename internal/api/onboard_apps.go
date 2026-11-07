package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/apps"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/job"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// onboardApps is the onboarding job's job.Apps with the same code as the
// apps API (ADR 0042): the test app and the onboarded app are registered as
// POST /v1/apps registers, without its checks of the name (onb- is
// reserved for the test app) and with the rows the onboarding brings.
type onboardApps struct{ s *Server }

const onboardActor = "onboarding"

// specAt reads and checks platform.yaml of repo at branch.
func (a onboardApps) specAt(ctx context.Context, repo, branch string) (string, *appspec.Spec, error) {
	if a.s.d.GitHub == nil {
		return "", nil, errors.New("GitHub is not configured")
	}
	commit, err := a.s.d.GitHub.ResolveRef(ctx, repo, branch)
	if err != nil {
		return "", nil, fmt.Errorf("resolve %s@%s: %w", repo, branch, err)
	}
	v, spec, err := deploy.CheckSpec(ctx, a.s.d.GitHub, repo, commit)
	if err != nil {
		return "", nil, err
	}
	if spec == nil {
		b, _ := json.Marshal(v)
		return "", nil, fmt.Errorf("platform.yaml of %s@%s is not valid: %s", repo, branch, b)
	}
	return commit, spec, nil
}

func (a onboardApps) queue(ctx context.Context, app store.App, ref, commit string, trigger apiv1.DeploymentTrigger) (string, error) {
	d := newDeployment(app, defaultInstance(app), ref, commit, trigger, onboardActor, storeTime())
	if err := a.s.d.Store.CreateDeployment(ctx, d); err != nil {
		return "", err
	}
	a.s.enqueue(ctx, d.ID)
	return d.ID, nil
}

// existing returns the newest deployment of an app registered already.
func (a onboardApps) existing(ctx context.Context, name string) (string, bool, error) {
	if _, err := a.s.d.Store.GetApp(ctx, name); errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	list, err := a.s.d.Store.ListDeployments(ctx, name, 1)
	if err != nil || len(list) == 0 {
		return "", true, err
	}
	return list[0].ID, true, nil
}

func (a onboardApps) DeployTest(ctx context.Context, t job.TestApp) (string, error) {
	commit, spec, err := a.specAt(ctx, t.Repo, t.Branch)
	if err != nil {
		return "", err
	}
	spec.Name = t.Name
	raw, _ := json.Marshal(spec)
	app := store.App{Name: t.Name, Repo: t.Repo, Branch: t.Branch, Environments: []string{t.Env}, Spec: raw, SpecCommit: commit,
		CreatedBy: onboardActor, CreatedAt: storeTime()}
	if _, ok, err := a.existing(ctx, t.Name); err != nil {
		return "", err
	} else if !ok {
		if err := a.s.d.Store.CreateApp(ctx, app); err != nil {
			return "", err
		}
	}
	if len(t.Secrets) > 0 && a.s.d.Secrets != nil {
		rows := make([]store.AppSecret, 0, len(t.Secrets))
		for _, name := range t.Secrets {
			row, err := a.s.d.Secrets.Row(t.Name, "", name, "onboarding-test", onboardActor)
			if err != nil {
				return "", err
			}
			row.UpdatedAt = storeTime()
			rows = append(rows, row)
		}
		if err := a.s.d.Store.ChangeAppSecrets(ctx, t.Name, "", rows, nil, maxSecretsPerScope); err != nil {
			return "", err
		}
	}
	return a.queue(ctx, app, t.Branch, commit, apiv1.DeploymentTriggerRegister)
}

func (a onboardApps) Register(ctx context.Context, r job.Registration) (string, error) {
	if id, ok, err := a.existing(ctx, r.App); err != nil || ok {
		return id, err
	}
	if a.s.d.Secrets == nil {
		return "", errors.New("an onboarded app's database needs PLATFORM_SECRETS_KEY")
	}
	commit, spec, err := a.specAt(ctx, r.Repo, r.Branch)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(spec)
	now := storeTime()
	app := store.App{Name: r.App, Repo: r.Repo, Branch: r.Branch, Environments: []string{r.Env}, AutoDeploy: true, Spec: raw, SpecCommit: commit,
		CreatedBy: r.Actor, CreatedAt: now}
	envs := []string{r.Env}
	if r.Peer != "" {
		envs = append(envs, r.Peer)
	}
	// A new CA (ADR 0030) with the source's own app password.
	made, err := deploy.NewAppDatabase(a.s.d.Secrets, r.App, envs, now)
	if err != nil {
		return "", err
	}
	_, caKey, err := a.s.d.Secrets.OpenDatabase(made)
	if err != nil {
		return "", err
	}
	db, err := a.s.d.Secrets.DatabaseRow(r.App, made.PrimaryEnv, made.PeerEnv, r.AppPassword, made.CACert, caKey)
	if err != nil {
		return "", err
	}
	db.CreatedAt = now
	db.SourceKind, db.Image, db.Database, db.Owner, db.StorageSize = store.DBSourceExternal, r.Image, r.Database, r.Owner, r.StorageSize
	db.ExternalHost, db.Reindex = r.ExternalHost, r.Reindex
	if db.ReplicationPasswordNonce, db.ReplicationPasswordCiphertext, err = a.s.d.Secrets.SealDatabaseField(r.App, secrets.DatabaseReplicationPassword, r.ReplicationPassword); err != nil {
		return "", err
	}
	if err := a.s.d.Store.CreateAppWithDatabase(ctx, app, db); err != nil {
		return "", err
	}
	if len(r.Secrets) > 0 {
		rows := make([]store.AppSecret, 0, len(r.Secrets))
		for name, v := range r.Secrets {
			row, err := a.s.d.Secrets.Row(r.App, "", name, v, r.Actor)
			if err != nil {
				return "", err
			}
			row.UpdatedAt = now
			rows = append(rows, row)
		}
		if err := a.s.d.Store.ChangeAppSecrets(ctx, r.App, "", rows, nil, maxSecretsPerScope); err != nil {
			return "", err
		}
	}
	if a.s.d.DBRoutes != nil {
		a.s.d.DBRoutes.Added(db)
	}
	return a.queue(ctx, app, r.Branch, commit, apiv1.DeploymentTriggerRegister)
}

func (a onboardApps) Redeploy(ctx context.Context, name, by string) (string, error) {
	app, err := a.s.d.Store.GetApp(ctx, name)
	if err != nil {
		return "", err
	}
	in := defaultInstance(app)
	list, err := a.s.d.Store.ListInstanceDeployments(ctx, in.Instance, 0)
	if err != nil {
		return "", err
	}
	for _, d := range list { // newest first
		if d.Status != store.DeployHealthy {
			continue
		}
		nd := newDeployment(app, in, d.Ref, d.Commit, apiv1.DeploymentTriggerRedeploy, cmp.Or(by, onboardActor), storeTime())
		nd.Images, nd.Spec = d.Images, d.Spec
		if err := a.s.d.Store.CreateDeployment(ctx, nd); err != nil {
			return "", err
		}
		a.s.enqueue(ctx, nd.ID)
		return nd.ID, nil
	}
	return "", fmt.Errorf("app %s has no healthy deployment to redeploy", name)
}


func (a onboardApps) Deployment(ctx context.Context, id string) (string, string, error) {
	d, err := a.s.d.Store.GetDeployment(ctx, id)
	if err != nil {
		return "", "", err
	}
	return d.Status, d.Error, nil
}

func (a onboardApps) DeploymentLog(ctx context.Context, id string) ([]byte, error) {
	evs, err := a.s.d.Store.ListDeploymentEvents(ctx, id, 0, 0)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Message)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

func (a onboardApps) Remove(ctx context.Context, name string) error {
	if a.s.d.Remover == nil {
		return errors.New("this server cannot remove apps")
	}
	err := a.s.d.Remover.RemoveApp(ctx, name)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (a onboardApps) FinishSource(ctx context.Context, name string) error {
	err := a.s.d.Store.SetAppDatabaseSourceKind(ctx, name, store.DBSourceExternal, store.DBSourcePeer)
	if err == nil && a.s.d.DBRoutes != nil {
		a.s.d.DBRoutes.Changed(name)
	}
	return err
}

func (a onboardApps) BackendURL(ctx context.Context, name, env string) (string, error) {
	if a.s.d.Backends == nil {
		return "", errors.New("this server has no gateway backends (PLATFORM_GATEWAY_BACKEND_TEMPLATE)")
	}
	app, err := a.s.d.Store.GetApp(ctx, name)
	if err != nil {
		return "", err
	}
	var spec appspec.Spec
	if err := json.Unmarshal(app.Spec, &spec); err != nil {
		return "", err
	}
	for _, svc := range spec.Services {
		if svc.Public {
			return a.s.d.Backends.URL(ctx, name, name, env, svc)
		}
	}
	return "", fmt.Errorf("app %s has no public service", name)
}

func (a onboardApps) Target(ctx context.Context, name, env string) (apps.OnboardTarget, error) {
	if a.s.d.Apps == nil {
		return apps.OnboardTarget{}, errors.New("this server has no app catalog")
	}
	return a.s.d.Apps.OnboardTarget(ctx, name, env)
}
