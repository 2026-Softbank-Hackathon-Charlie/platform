package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Deployment statuses, mirrored from apiv1.DeploymentStatus. Healthy, failed
// and superseded are final (DeploymentFinished).
const (
	DeployQueued     = "queued"
	DeployBuilding   = "building"
	DeployCommitting = "committing"
	DeploySyncing    = "syncing"
	DeployHealthy    = "healthy"
	DeployFailed     = "failed"
	DeploySuperseded = "superseded"
)

// finishedDeployment lists the final statuses, for queries.
var finishedDeployment = []string{DeployHealthy, DeployFailed, DeploySuperseded}

// DeploymentFinished reports whether status is final: healthy, failed or
// superseded. A finished deployment is never updated again.
func DeploymentFinished(status string) bool { return slices.Contains(finishedDeployment, status) }

// App is an app registered from GitHub (POST /v1/apps). Apps that are only
// in PLATFORM_APPS_FILE are not stored.
type App struct {
	Name   string // platform.yaml's name; the key
	Repo   string // owner/name
	Branch string
	// Environments are the target environments, in PLATFORM_ENVS_FILE order.
	// Reads return a non-nil slice.
	Environments []string
	AutoDeploy   bool
	// Spec is the appspec.Spec JSON (the API's AppSpec) of the newest
	// platform.yaml that passed the check, read at SpecCommit.
	Spec       []byte
	SpecCommit string
	CreatedBy  string
	CreatedAt  time.Time
}

// AppBranch is a branch an operator added to an app (POST
// /v1/apps/{name}/instances, ADR 0015), deployed as its own instance. The
// app's own branch is App.Branch, the default instance named after the app;
// it is never an AppBranch.
type AppBranch struct {
	App    string
	Branch string
	// Instance is appspec.InstanceName(App, Branch). It is unique across
	// every app's branches and is never an app's name.
	Instance   string
	AutoDeploy bool
	// Spec is the appspec.Spec JSON of the newest platform.yaml on the
	// branch that passed the check, read at SpecCommit.
	Spec       []byte
	SpecCommit string
	CreatedBy  string
	CreatedAt  time.Time
}

// DeploymentImage is a service's built image, pinned by digest. The JSON
// matches apiv1.DeploymentImage.
type DeploymentImage struct {
	Service string `json:"service"`
	Image   string `json:"image"`
}

// DeploymentEnvironment is a deployment's progress in one environment. The
// JSON matches apiv1.DeploymentEnvironment.
type DeploymentEnvironment struct {
	Env          string `json:"env"`
	Status       string `json:"status"` // apiv1.DeploymentEnvironmentStatus
	SyncStatus   string `json:"syncStatus,omitempty"`
	HealthStatus string `json:"healthStatus,omitempty"`
	URL          string `json:"url,omitempty"`
	Message      string `json:"message,omitempty"`
}

// Deployment is one deployment of an app's commit into one of its instances,
// in some of its environments.
type Deployment struct {
	ID  string
	App string
	// Branch and Instance name the instance it deploys: the app's Branch and
	// Name for the default instance, or an AppBranch's Branch and Instance.
	Branch      string
	Instance    string
	Ref         string // as asked: a branch, tag or commit sha
	Commit      string // the full sha Ref resolved to
	Trigger     string // apiv1.DeploymentTrigger
	TriggeredBy string
	Status      string
	// Images and Environments read back as non-nil slices.
	Images       []DeploymentImage
	Environments []DeploymentEnvironment
	Error        string
	BuildID      string
	CreatedAt    time.Time
	StartedAt    *time.Time // nil until the deployer takes it
	FinishedAt   *time.Time // nil until it is finished
}

// DeploymentEvent is one stored deployment event.
type DeploymentEvent struct {
	// Seq is assigned by AppendDeploymentEvent. It increases across all
	// deployments, so it is also increasing within one and usable as an SSE id.
	Seq          int64
	DeploymentID string
	Time         time.Time
	Type         string // apiv1.DeploymentEventType: phase, log, environment, done
	Phase        string // apiv1.DeploymentStatus
	Env          string
	Message      string
}

// Clean returns a copy of a with every string passed through CleanText and
// Spec through CleanJSON. Stores apply it on every write.
func (a App) Clean() (App, error) {
	for _, f := range []*string{&a.Name, &a.Repo, &a.Branch, &a.SpecCommit, &a.CreatedBy} {
		*f = CleanText(*f)
	}
	envs := make([]string, len(a.Environments))
	for i, e := range a.Environments {
		envs[i] = CleanText(e)
	}
	a.Environments = envs
	spec, err := CleanJSON(a.Spec)
	if err != nil {
		return App{}, fmt.Errorf("app spec: %w", err)
	}
	a.Spec = spec
	return a, nil
}

// Clean returns a copy of b with every string passed through CleanText and
// Spec through CleanJSON. Stores apply it on every write.
func (b AppBranch) Clean() (AppBranch, error) {
	for _, f := range []*string{&b.App, &b.Branch, &b.Instance, &b.SpecCommit, &b.CreatedBy} {
		*f = CleanText(*f)
	}
	spec, err := CleanJSON(b.Spec)
	if err != nil {
		return AppBranch{}, fmt.Errorf("app branch spec: %w", err)
	}
	b.Spec = spec
	return b, nil
}

// Clean returns a copy of d with every string passed through CleanText.
// Stores apply it on every write; the slices of the copy are its own.
func (d Deployment) Clean() Deployment {
	for _, f := range []*string{&d.ID, &d.App, &d.Branch, &d.Instance, &d.Ref, &d.Commit, &d.Trigger, &d.TriggeredBy, &d.Status, &d.Error, &d.BuildID} {
		*f = CleanText(*f)
	}
	images := make([]DeploymentImage, len(d.Images))
	for i, im := range d.Images {
		images[i] = DeploymentImage{Service: CleanText(im.Service), Image: CleanText(im.Image)}
	}
	envs := make([]DeploymentEnvironment, len(d.Environments))
	for i, e := range d.Environments {
		for _, f := range []*string{&e.Env, &e.Status, &e.SyncStatus, &e.HealthStatus, &e.URL, &e.Message} {
			*f = CleanText(*f)
		}
		envs[i] = e
	}
	d.Images, d.Environments = images, envs
	return d
}

// Clean returns e with its strings passed through CleanText. DeploymentID is
// a reference and stays as it is: an id that is not clean names nothing.
func (e DeploymentEvent) Clean() DeploymentEvent {
	for _, f := range []*string{&e.Type, &e.Phase, &e.Env, &e.Message} {
		*f = CleanText(*f)
	}
	return e
}

// DeployStore persists the apps registered from GitHub, the branches added to
// them, their deployments and the deployments' events, and the GitHub webhook
// deliveries already handled. It is part of Store. Every string is stored as
// Clean leaves it, and a lookup by a key that is not clean finds nothing.
type DeployStore interface {
	// CreateApp inserts an app. ErrConflict if the name exists.
	CreateApp(ctx context.Context, a App) error
	// GetApp returns ErrNotFound when no app has the name.
	GetApp(ctx context.Context, name string) (App, error)
	// ListApps returns every app, by name.
	ListApps(ctx context.Context) ([]App, error)
	// UpdateApp replaces Branch, Environments, AutoDeploy, Spec and
	// SpecCommit of the app named a.Name. ErrNotFound if it does not exist.
	UpdateApp(ctx context.Context, a App) error
	// DeleteApp removes an app together with its added branches, its
	// deployments and their events. ErrNotFound if it does not exist.
	DeleteApp(ctx context.Context, name string) error

	// CreateAppBranch adds a branch to an existing app. ErrNotFound when the
	// app does not exist; ErrConflict when b.Branch is the app's own branch,
	// the app already has the branch, or b.Instance is taken by any app's
	// branch or is an app's name. A concurrent DeleteApp either sees the
	// branch and removes it or makes the create fail.
	CreateAppBranch(ctx context.Context, b AppBranch) error
	// ListAppBranches returns an app's added branches, by branch name. It is
	// empty when the app has none or does not exist.
	ListAppBranches(ctx context.Context, app string) ([]AppBranch, error)
	// UpdateAppBranch replaces AutoDeploy, Spec and SpecCommit of the branch
	// b.Branch of app b.App. ErrNotFound if the app has no such branch.
	UpdateAppBranch(ctx context.Context, b AppBranch) error
	// DeleteAppBranch removes a branch of an app together with the
	// deployments of its instance and their events, in one transaction.
	// ErrNotFound if the app has no such branch.
	DeleteAppBranch(ctx context.Context, app, branch string) error

	// CreateDeployment inserts a deployment into an instance of an existing
	// app. ErrNotFound when the app does not exist, or when d.Branch and
	// d.Instance are neither the app's Branch and Name nor the Branch and
	// Instance of one of its AppBranches; ErrConflict if the id exists. A
	// concurrent DeleteApp, or DeleteAppBranch of the deployment's branch,
	// either sees the deployment and removes it or makes the create fail.
	CreateDeployment(ctx context.Context, d Deployment) error
	// GetDeployment returns ErrNotFound when the id does not exist.
	GetDeployment(ctx context.Context, id string) (Deployment, error)
	// ListDeployments returns an app's deployments, newest CreatedAt first
	// (then id descending), at most limit (<= 0 means no limit).
	ListDeployments(ctx context.Context, app string, limit int) ([]Deployment, error)
	// UpdateDeployment replaces Status, Images, Environments, Error, BuildID,
	// StartedAt and FinishedAt of a deployment that is not finished, in one
	// conditional write, and returns the stored deployment. ErrNotFound when
	// the id does not exist; ErrConflict (wrapped, with the current status)
	// when it is finished, so the worker of a superseded deployment learns
	// to stop.
	UpdateDeployment(ctx context.Context, d Deployment) (Deployment, error)
	// ListUnfinishedDeployments returns every deployment that is not
	// finished, oldest CreatedAt first (then id), to resume after a restart.
	ListUnfinishedDeployments(ctx context.Context) ([]Deployment, error)

	// AppendDeploymentEvent stores an event of an existing deployment and
	// returns it with Seq set. ErrNotFound when the deployment does not
	// exist. Appends to one deployment are serialized, so its events commit
	// in Seq order and a reader resuming after a Seq misses none.
	AppendDeploymentEvent(ctx context.Context, e DeploymentEvent) (DeploymentEvent, error)
	// ListDeploymentEvents returns a deployment's events with Seq > afterSeq
	// in Seq order, at most limit (<= 0 means no limit).
	ListDeploymentEvents(ctx context.Context, deploymentID string, afterSeq int64, limit int) ([]DeploymentEvent, error)

	// RecordWebhookDelivery records a GitHub webhook delivery id
	// (X-GitHub-Delivery) and reports whether it is new; false means it was
	// recorded before, so the delivery is a redelivery.
	RecordWebhookDelivery(ctx context.Context, id string, at time.Time) (bool, error)
}
