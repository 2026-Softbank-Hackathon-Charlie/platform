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
// cutting_over reserves one plan the platform writes itself. It does not
// go through POST /v1/plans: the onboarding job calls the plan service's
// reservation (plans.Reserve) directly with the onboarding's own policy,
// so the API's EnginePlanEndpoint (which requires namespace and cluster
// and refuses unknown fields) stays as it is. The plan is stored and shown
// in GET /v1/plans like any other, with an empty source namespace and
// cluster. It is the engine's plan (migration-engine pkg/plan, external
// source of its ADR 0017):
//
//	{
//	  "id": "<onboarding id>-<attempt>",          // PlanID: onb-3f9a1c2b7d4e-1
//	  "source": {
//	    "kind": "external",                       // plan.SourceExternal; no namespace, cluster, kubeconfig or context
//	    "name": "legacy",                         // SourceName: the target Cluster's externalClusters name
//	    "env": "legacy",                          // also the gateway's source backend
//	    "dsn": "postgres://<fence user>:${LEGACY_PGPASSWORD}@db-legacy-<app>.<route namespace>.svc.cluster.local:5432/<database>?sslmode=prefer"
//	  },
//	  "target": {
//	    "env": "<target env>",
//	    "context": "<the env's context>",         // as for any target; never kubeconfig
//	    "namespace": "<app>",
//	    "cluster": "<app>-<postgres resource>",
//	    "dsn": "...",                             // the app account, as for any target
//	    "reindex": ["public.users_email_idx"],    // app_databases.reindex; omitted when empty
//	    "app": {
//	      "mode": "cold",
//	      "kind": "Deployment",
//	      "name": "<app>-<public service>",
//	      "replicas": <platform.yaml replicas of the public service>
//	    }
//	  },
//	  "gateway": {"mode": "http", "sourceBackend": "legacy", "targetBackend": "<target env>", ...},
//	  "files": {"source": "legacy:<source bucket>", "target": "<target env>:<app bucket>"},
//	  "verify": {"fenceGate": true}
//	}
//
// There is no decommission block: decommissioning is the onboarding's. The
// onboarding's policy is the server's with these additions, only for this
// plan:
//
//   - target.app (cold) and source.kind external are accepted (the server
//     refuses target.app for every other plan, policy.go).
//   - LegacyPasswordEnv is an allowed password variable bound to the one
//     host db-legacy-<app>.<route namespace>.svc.cluster.local:5432
//     (Policy.HostPasswordEnvs), and that host is one of the app's
//     database hosts for the gateway scope.
//   - The source bucket of the onboarding row is a registered bucket, and
//     the rclone remote "legacy" (RCLONE_CONFIG_LEGACY_*) is only in the
//     onboarding job's and the run's process environment, like
//     LegacyPasswordEnv.
//
// A retried cutover (after a run rolled back) reserves a new plan with the
// next attempt number.
//
// # The hopai Job
//
// patching and checking start hopai (internal/airun) with kind onboard. Its
// input is a ConfigMap the platform creates next to the Job, mounted at
// InputDir; its container gets, besides what every AI run gets:
//
//	KIND=onboard
//	ONBOARDING=<onboarding id>
//	REPO=<owner/name>
//	BRANCH=hopcharlie/onboard              // Branch: the only branch it pushes
//	FACTS_FILE=/onboarding/facts.json      // facts.Facts as JSON
//	CHECK_LOG_FILE=/onboarding/check.log   // on a retry only: the failed test
//	                                       // deployment's log, its last CheckLogMax bytes
//
// Files, not variables, because a variable is capped at 128 KiB and the
// facts of a large compose project or a log can exceed it. Facts hold no
// secret value, so a ConfigMap is enough. It asks for its push token with
// POST /v1/github/push-tokens, which gives one for a repository whose
// onboarding is patching or checking.
//
// # Names
//
// One instance per app: an onboarded app has only its default instance,
// whose namespace and route names are the app's (ADR 0015). The legacy
// route in the management cluster (RouteName) needs its own template next
// to the environments' in internal/dbroute, and dbroute's Reconcile must
// not delete it as a route of no environment.
package onboard

import (
	"fmt"
	"regexp"
	"time"
)

// FeatureEnv turns the onboarding API on ("1"); unset or "0" leaves its
// routes unregistered.
const FeatureEnv = "PLATFORM_ONBOARDING"

// IDPrefix starts every onboarding id: onb-<12 lowercase hex digits>. The
// id is a valid app name (3 to 32 characters) and plan id, so it is also
// the name of the test app and the start of the plan ids.
const IDPrefix = "onb-"

// ID matches an onboarding id.
var ID = regexp.MustCompile(`^onb-[0-9a-f]{12}$`)

// TestApp is the temporary app checking deploys the PR branch as: no
// database pair, no gateway, test secrets. It is the onboarding id, which
// no app name taken from a <label>.hopcharlie.dev ever is while onboardings
// exist (POST /v1/apps refuses the onb- prefix in the implementation).
func TestApp(id string) string { return id }

// PlanID is the engine plan id of a cutover attempt (1, 2, ...).
func PlanID(id string, attempt int) string { return fmt.Sprintf("%s-%d", id, attempt) }

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

// The hopai Job's input (see the package comment).
const (
	InputDir     = "/onboarding"
	FactsFile    = InputDir + "/facts.json"
	CheckLogFile = InputDir + "/check.log"
	// CheckLogMax is how much of the end of the failed deployment's log
	// CHECK_LOG_FILE holds.
	CheckLogMax = 32 << 10
)

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
	// MaxNameLen is the longest Kubernetes Service name: discovery blocks
	// (facts.BlockNameTooLong) when EgressService would be longer.
	MaxNameLen = 63
)

// RouteName is the management cluster's egress Service to the source
// database (internal/dbroute), tailnet FQDN TailnetHost(app).
func RouteName(app string) string { return "db-" + SourceName + "-" + app }

// TailnetHost is the tailnet hostname of the Tailscale container connecting
// starts next to the source database, values.dbTopology.external.host.
func TailnetHost(app string) string { return "hop-db-" + SourceName + "-" + app }

// EgressService is the chart's Service in the app's namespace to the
// source, which externalClusters legacy connects to; cluster is
// <app>-<postgres resource>.
func EgressService(cluster string) string { return cluster + "-to-" + SourceName }

// TailscaleContainer is that container's name on the VM.
func TailscaleContainer(app string) string { return "hopcharlie-ts-" + app }

// TailscaleVolume is the named volume of the Tailscale container's state.
func TailscaleVolume(app string) string { return TailscaleContainer(app) + "-state" }

// AppName matches an app name (pkg/appspec): the label of an onboarding's
// service URL.
var AppName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)
