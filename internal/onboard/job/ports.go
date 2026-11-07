// Package job drives an onboarding (ADR 0042) through its states: the
// background runner, the state machine and the operations the API calls
// (approve, cutover, decommission, resume, delete). Every effect outside
// the onboarding's own row goes through one of the interfaces below, so
// the machine is tested with fakes; cmd/api wires the real ones.
//
// Discovery and VM connect (internal/onboard/discover and
// internal/onboard/connect) are another workstream: Discoverer and
// Connector are what this package expects from them.
package job

import (
	"context"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/airun"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/apps"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/github"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// Sealed is a value the secrets key sealed for an onboarding:
// secrets.Cipher.SealOnboarding(id, field, value), field one of
// secrets.OnboardingSecrets, OnboardingSuperuserPassword,
// OnboardingReplicationPassword. Nil Ciphertext is "not known".
type Sealed struct{ Nonce, Ciphertext []byte }

// DiscoverRequest is what discovery reads from.
type DiscoverRequest struct {
	ID, Repo, ServiceURL string
	// TargetEnv is the environment asked for; empty lets discovery choose
	// (Facts.Target.Env).
	TargetEnv string
}

// Discovered is one discovery: the facts (no secret values) and the values
// it decrypted from the VM, sealed again for the onboarding's row.
type Discovered struct {
	Facts facts.Facts
	// Secrets is the JSON object {"<container>": {"<NAME>": "<value>"}}
	// (secrets.OnboardingSecrets); SuperuserPassword the fence user's.
	Secrets, SuperuserPassword Sealed
	// KeyVersion and KeyCheck are the secrets key's that sealed them.
	KeyVersion int
	KeyCheck   string
	// HumanActions are what a person must do: install_github_app not done
	// makes the onboarding wait for resume; delete_dns_record is done in
	// syncing, when the name points at the gateway.
	HumanActions []apiv1.OnboardingHumanAction
}

// Discoverer reads GitHub, DNS, the cloud, the VM and the database. It only
// reads. A blocker goes into Facts.Report, not into the error; the error is
// for a discovery that could not finish.
type Discoverer interface {
	Discover(ctx context.Context, req DiscoverRequest) (Discovered, error)
}

// ConnectRequest names the onboarding and its source.
type ConnectRequest struct {
	ID, App string
	Facts   facts.Facts
}

// Connected is what Connect did.
type Connected struct {
	Changes []apiv1.OnboardingLegacyChange
	// ReplicationPassword is sealed as secrets.OnboardingReplicationPassword.
	ReplicationPassword Sealed
	// Rotated says the role's password changed: an app registered with the
	// old one needs it sealed again.
	Rotated bool
}

// Blocker is an error that is also a report blocker (facts Block* code).
type Blocker struct{ Code, Message string }

func (b *Blocker) Error() string { return b.Code + ": " + b.Message }

// Connector changes the source VM and its database (ADR 0042 "3." steps 2
// to 4: instance tag, Tailscale key, HopCharlie-Connect) and undoes it.
type Connector interface {
	// Connect returns what it did, in order, also with an error (what was
	// done before it failed), and hopcharlie_replicator's password. Run
	// again it does only what is missing. A *Blocker error is a finding the
	// operator must fix on the source before resume.
	Connect(ctx context.Context, req ConnectRequest) (Connected, error)
	// Disconnect undoes changes (its own kinds; others are left as they
	// are) in reverse order and returns them with UndoneAt set on those it
	// undid, also with an error.
	Disconnect(ctx context.Context, req ConnectRequest, changes []apiv1.OnboardingLegacyChange) ([]apiv1.OnboardingLegacyChange, error)
	// RemoveDevice deletes the source's tailnet device and nothing else
	// (decommissioning: the source is fenced, its database changes stay).
	RemoveDevice(ctx context.Context, req ConnectRequest) ([]apiv1.OnboardingLegacyChange, error)
	// StopInstance stops (never terminates) the instance once it checked
	// that its onboarding tag is req.ID, then removes the tag; it returns
	// the instance_tagged change undone.
	StopInstance(ctx context.Context, req ConnectRequest) ([]apiv1.OnboardingLegacyChange, error)
}

// GitHub is the App's calls (internal/github.Client).
type GitHub interface {
	ResolveRef(ctx context.Context, repo, ref string) (string, error)
	OpenPR(ctx context.Context, repo, head, base, title, body string) (github.PullRequest, error)
	GetPR(ctx context.Context, repo string, number int) (github.PullRequest, error)
	MergePR(ctx context.Context, repo string, number int, sha string) error
	ClosePR(ctx context.Context, repo string, number int) error
	BehindBy(ctx context.Context, repo, base, head string) (int, error)
	DisableWorkflow(ctx context.Context, repo string, id int64) error
	EnableWorkflow(ctx context.Context, repo string, id int64) error
	ActiveRuns(ctx context.Context, repo string, workflowID int64) ([]github.WorkflowRun, error)
	CancelRun(ctx context.Context, repo string, runID int64) error
}

// AI starts hopai (internal/airun.Runner).
type AI interface {
	StartOnboard(ctx context.Context, req airun.OnboardRequest, actor string) (airun.Run, error)
	ListOnboard(ctx context.Context, id string) ([]airun.Run, error)
	StopOnboard(ctx context.Context, id string) error
	Outcome(ctx context.Context, job string) (airun.OnboardOutcome, error)
}

// TestApp is checking's temporary app: the PR branch in one environment,
// no database pair, never routed by the gateway (onb- name), the secrets
// with test values.
type TestApp struct {
	Name, Repo, Branch, Env string
	Secrets                 []string
}

// Registration is the onboarded app (registering): one store write of the
// app, its database row (source_kind external) and its secrets, then its
// first deployment.
type Registration struct {
	App, Repo, Branch, Env, Peer string
	Actor                        string
	// Image, Database, Owner, StorageSize, ExternalHost and Reindex go to
	// app_databases.
	Image, Database, Owner, StorageSize, ExternalHost string
	Reindex                                           []string
	// Parameters are the standby-relevant settings (StandbyParameters).
	Parameters map[string]string
	// AppPassword is the app account's password (the source's own);
	// ReplicationPassword hopcharlie_replicator's.
	AppPassword, ReplicationPassword string
	// Secrets are the app secrets of its first deployment (ADR 0019).
	Secrets map[string]string
}

// Apps registers, deploys and removes apps (internal/api implements it with
// the same code as POST /v1/apps).
type Apps interface {
	// DeployTest registers the test app unless it is there and queues a
	// deployment of Branch; it returns the deployment id.
	DeployTest(ctx context.Context, t TestApp) (string, error)
	// Register stores the onboarded app, its database row and its secrets
	// unless the app is there; an app of that name that is not this
	// onboarding's (Owned) is an error. Again, it writes the secrets again.
	Register(ctx context.Context, r Registration) error
	// Deploy returns the app's unfinished or healthy deployment, or queues
	// a deployment of its branch's head when it has none.
	Deploy(ctx context.Context, app, actor string) (string, error)
	// Owned says the app is an onboarding's: its database row has
	// source_kind external (or had: peer after the cutover) and the
	// external host onboard.TailnetHost(app).
	Owned(ctx context.Context, app string) (bool, error)
	// Redeploy queues a deployment of the app's current commit to all its
	// environments, rendered from its rows as they are now.
	Redeploy(ctx context.Context, app, actor string) (string, error)
	// Deployment is a deployment's status (store.Deploy*) and message.
	Deployment(ctx context.Context, id string) (status, message string, err error)
	// DeploymentLog is the end of a deployment's log.
	DeploymentLog(ctx context.Context, id string) ([]byte, error)
	// Remove deletes an app with everything it has; one that is not there
	// is no error. keepGateway leaves its gateway app as it is.
	Remove(ctx context.Context, app string, keepGateway bool) error
	// SetReplicationPassword seals hopcharlie_replicator's new password into
	// the app's database row.
	SetReplicationPassword(ctx context.Context, app, password string) error
	// FinishSource sets the app's database source_kind to peer.
	FinishSource(ctx context.Context, app string) error
	// BackendURL is the gateway backend URL of the app in env.
	BackendURL(ctx context.Context, app, env string) (string, error)
	// Target is the cutover plan's target (internal/apps.Catalog).
	Target(ctx context.Context, app, env string) (apps.OnboardTarget, error)
}

// Kube reads and patches the workload clusters.
type Kube interface {
	// StartSeconds is from the Deployment's creation until it was Ready.
	StartSeconds(ctx context.Context, env, namespace, deployment string) (float64, error)
	// SetReplicaSource patches the Cluster's spec.replica.source.
	SetReplicaSource(ctx context.Context, env, namespace, cluster, source string) error
}

// Files copies the source bucket into the app's (rclone, with the legacy
// remote only in env).
type Files interface {
	// Copy copies src to dst and returns how many objects it copied and how
	// many still differ after.
	Copy(ctx context.Context, env []string, src, dst string) (copied, pending int, err error)
}

// DNS checks where a name points.
type DNS interface {
	// PointsAtGateway says host resolves to the gateway (the wildcard) and
	// no answer is vmIP, the VM's address.
	PointsAtGateway(ctx context.Context, host, vmIP string) (bool, error)
}

// Gateway is the traffic gateway's app API (internal/gateway.Client).
type Gateway interface {
	Apps(ctx context.Context) (gateway.AppList, error)
	PutApp(ctx context.Context, name string, spec gateway.AppSpec) error
	DeleteApp(ctx context.Context, name string) error
}

// Routes are the management cluster's routes to sources (internal/dbroute).
type Routes interface {
	EnsureLegacy(ctx context.Context, app string) (string, error)
	RemoveLegacy(ctx context.Context, app string) error
}

// Cutover reserves the plan and runs it (internal/plans, internal/runs).
type Cutover interface {
	ReserveOnboarding(ctx context.Context, raw []byte, actor string, scope plans.OnboardingScope) (plans.Reserved, string, error)
	Started(ctx context.Context, ref, runID string) error
	// RejectOnboarding ends a reserved plan whose run did not start.
	RejectOnboarding(ctx context.Context, ref, reason string)
	Start(ctx context.Context, o runs.StartOptions) (apiv1.Run, error)
	Get(ctx context.Context, runID string) (apiv1.Run, error)
	// FindRun is the newest run of the plan ref that rolls back nothing.
	FindRun(ctx context.Context, planRef string) (apiv1.Run, bool, error)
}

// SourceDB checks the route to the source and changes the new primary.
type SourceDB interface {
	// Check connects to the source at host as the fence user and as
	// hopcharlie_replicator (IDENTIFY_SYSTEM over a replication connection).
	Check(ctx context.Context, host, database, user, password, replicationPassword string) error
	// Replication says whether hopcharlie_replicator streams from the
	// source (pg_stat_replication, read as the fence user) and its replay lag.
	Replication(ctx context.Context, host, database, user, password string) (streaming bool, lag time.Duration, err error)
	// ReadOnly says the source refuses writes (default_transaction_read_only
	// on: still fenced).
	ReadOnly(ctx context.Context, host, database, user, password string) (bool, error)
	// DropRole drops role on the database dsn reaches.
	DropRole(ctx context.Context, dsn, role string) error
}

// Store is what the runner needs of the store.
type Store interface {
	store.OnboardingStore
	GetApp(ctx context.Context, name string) (store.App, error)
}
