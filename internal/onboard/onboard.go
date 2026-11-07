// Package onboard is the platform's side of onboarding an app that ran
// outside it with docker compose on one VM (ADR 0042). This file is the
// contract the other parts are built against: the names the platform
// gives what it makes, the hopai Job's input and the plan it hands the
// migration engine. The job that drives an onboarding through its states
// comes with the implementation; until then the API answers 501 behind
// PLATFORM_ONBOARDING=1 and nothing here runs.
//
// What the other parts read elsewhere:
//
//   - API: api/openapi.yaml, /v1/onboardings (pkg/apiv1 Onboarding*).
//   - Facts: pkg/onboard/facts (hopai imports it).
//   - Store: internal/store onboarding.go, migration 0016_onboardings.sql.
//   - Gitops values (chart): internal/deploy gitops.go, File.Postgres,
//     File.OnboardingCold, DBTopology.External, Sealed.PGLegacy.
//   - Gateway: internal/gateway AppSpec.Dial and AppInfo.Dial.
//
// # The engine plan of an onboarding
//
// cutting_over submits one plan (POST /v1/plans, then POST /v1/migrations)
// that the platform writes itself. It is the engine's plan (migration-engine
// pkg/plan, external source of its ADR 0017) with these fields; everything
// not listed is as in the plans the platform drafts for a database pair:
//
//	{
//	  "id": "onb-<onboarding id>",
//	  "source": {
//	    "kind": "external",              // plan.SourceExternal
//	    "name": "legacy",                // SourceName: the target Cluster's externalClusters name
//	    "env": "legacy",                 // also the gateway's source backend
//	    "dsn": "postgres://<fence user>:${LEGACY_PGPASSWORD}@db-legacy-<app>.<route namespace>.svc.cluster.local:5432/<database>?sslmode=prefer"
//	  },
//	  "target": {
//	    "env": "<target env>",
//	    "kubeconfig": "...", "context": "...",   // as for any target
//	    "namespace": "<app>",
//	    "cluster": "<app>-<postgres resource>",
//	    "dsn": "...",                            // the app account, as for any target
//	    "reindex": ["public.users_email_idx"],   // app_databases.reindex; omitted when empty
//	    "app": {
//	      "mode": "cold",
//	      "kind": "Deployment",
//	      "name": "<app>-<public service>",
//	      "replicas": <platform.yaml replicas of the public service>
//	    }
//	  },
//	  "gateway": {"mode": "http", "sourceBackend": "legacy", "targetBackend": "<target env>", ...},
//	  "files": {"source": "legacy:<source bucket>", "target": "<target env>:<app bucket>"}
//	}
//
// The fence user's password is only in the run's process environment as
// LegacyPasswordEnv, and the rclone remote "legacy" (RCLONE_CONFIG_LEGACY_*)
// only in the onboarding job's and the run's. The plan policy accepts
// target.app (cold) and an external source only for a plan of an
// onboarding, and adds db-legacy-<app> to the app's database hosts.
//
// # The hopai Job
//
// patching and checking start hopai (internal/airun) with kind onboard. Its
// container gets, besides what every AI run gets:
//
//	KIND=onboard
//	ONBOARDING=<onboarding id>
//	REPO=<owner/name>
//	BRANCH=hopcharlie/onboard        // Branch: the only branch it pushes
//	FACTS=<facts.Facts as JSON>
//	CHECK_LOG=<the failed test deployment's log, on a retry only>
//
// It asks for its push token with POST /v1/github/push-tokens, which gives
// one for a repository whose onboarding is patching or checking.
package onboard

import "time"

// FeatureEnv turns the onboarding API on ("1"); unset or "0" leaves its
// routes unregistered.
const FeatureEnv = "PLATFORM_ONBOARDING"

// SourceName is the source database's name everywhere the platform names
// it: the gateway backend, the target Cluster's externalClusters entry and
// replica.primary, the engine plan's source.name and source.env, and the
// rclone remote of the source bucket.
const SourceName = "legacy"

// ReplicationUser is the account connecting creates in the source
// database for the target's streaming replication.
const ReplicationUser = "hopcharlie_replicator"

// LegacyPasswordEnv is the variable of the run's process environment that
// holds the fence user's password, which the plan's source.dsn names.
const LegacyPasswordEnv = "LEGACY_PGPASSWORD"

// Branch is the branch hopai pushes the contract to and the PR's head.
const Branch = "hopcharlie/onboard"

// InstanceTag is the tag connecting puts on the VM, whose value is the
// onboarding id; the IAM conditions of Connect and Stop rely on it.
const InstanceTag = "hopcharlie-onboarding"

// TailscaleTag is the tag of the device connecting adds to the tailnet.
const TailscaleTag = "tag:hop-legacy"

// ColdLabel is the label the chart puts on the Deployments of an app with
// values.onboardingCold, whose /spec/replicas the ApplicationSet ignores.
const ColdLabel = "hopcharlie.dev/onboarding-cold"

// The SSM documents (infra Terraform) the platform runs on the VM.
const (
	InspectDocument = "HopCharlie-Inspect"
	ConnectDocument = "HopCharlie-Connect"
)

// HoldBudget is the write hold the cutover may take (facts.HoldBudgetSeconds).
const HoldBudget = 40 * time.Second

// Limits of ADR 0042.
const (
	// CheckAttempts is how often the test deployment is tried, hopai fixing
	// the branch in between.
	CheckAttempts = 2
	// WorkflowWait is how long connecting waits for a running deploy
	// workflow before cancelling it.
	WorkflowWait = 10 * time.Minute
	// MaxReplicationLag and MaxFileDiff are what syncing waits for.
	MaxReplicationLag = 2 * time.Second
	MaxFileDiff       = 100
	// WALKeepSize is the line connecting appends to postgresql.conf.
	WALKeepSize = "1GB"
	// MinFreeDisk is the free disk under which discovery blocks.
	MinFreeDisk = 2 << 30
)

// RouteName is the management cluster's egress Service to the source
// database (internal/dbroute), tailnet FQDN TailnetHost(app).
func RouteName(app string) string { return "db-" + SourceName + "-" + app }

// TailnetHost is the tailnet hostname of the Tailscale container connecting
// starts next to the source database, values.dbTopology.external.host.
func TailnetHost(app string) string { return "hop-db-" + SourceName + "-" + app }

// TailscaleContainer is that container's name on the VM, and
// TailscaleVolume its state volume.
func TailscaleContainer(app string) string { return "hopcharlie-ts-" + app }

// TailscaleVolume is the named volume of the Tailscale container's state.
func TailscaleVolume(app string) string { return TailscaleContainer(app) + "-state" }

// TestApp is the temporary app checking deploys the PR branch as: no
// database pair, no gateway, test secrets.
func TestApp(app, id string) string { return app + "-onb-" + short(id) }

// short is the id's last 6 characters, which are random.
func short(id string) string {
	if len(id) > 6 {
		return id[len(id)-6:]
	}
	return id
}

// IDPrefix starts every onboarding id: onb_<12 hex digits>.
const IDPrefix = "onb_"
