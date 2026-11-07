// Package facts is what the platform found out about an app that ran
// outside it, on one VM with docker compose, before onboarding it (ADR
// 0042, "1. 조사"). The platform's discovery writes one Facts per
// onboarding (onboardings.facts, a JSON column); hopai (ai-sandbox) reads
// the same JSON to write the contract branch, so this package is under
// pkg/ and only depends on the standard library.
//
// Facts never hold a secret value. An environment variable is its name,
// the container it came from and whether it is a secret; the values the
// discovery decrypted are only in the onboarding row, sealed (ADR 0019).
// Values written in the repository (compose.yaml) are in the clone hopai
// gets, not here.
//
// Every field is in the JSON a discovery that did not get that far leaves
// out: GitHub without the App installed, VM and DB on GCP (inside the VM is
// the next step there, ADR 0042 "범위"), and so on. Report says why.
package facts

import (
	"fmt"
	"net/url"
	"time"
)

// Version is Facts.Version of this package's shape. A reader refuses a
// higher one.
const Version = 1

// Facts is one discovery's result.
type Facts struct {
	Version int `json:"version"`
	// At is when the discovery finished.
	At time.Time `json:"at"`
	// Repo is owner/name and ServiceURL https://<label>.hopcharlie.dev, as
	// the onboarding was started with; App is the label.
	Repo       string `json:"repo"`
	ServiceURL string `json:"serviceUrl"`
	App        string `json:"app"`

	GitHub   *GitHub   `json:"github,omitempty"`
	HTTPS    *HTTPS    `json:"https,omitempty"`
	Instance *Instance `json:"instance,omitempty"`
	VM       *VM       `json:"vm,omitempty"`
	DB       *DB       `json:"db,omitempty"`
	// Storage is the app's object storage, read from its environment
	// (names) and their values (the endpoint and bucket only).
	Storage *Storage `json:"storage,omitempty"`
	// Target is what the platform chose from the above.
	Target *Target `json:"target,omitempty"`
	// Report is the blockers, warnings and drift (ADR 0042, "1. 조사").
	Report Report `json:"report"`
}

// GitHub is the repository as the GitHub App installation token reads it.
type GitHub struct {
	// Installed is false while the App is not installed on the repository;
	// the rest is then empty and the onboarding waits for
	// install_github_app.
	Installed     bool   `json:"installed"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// Head is the default branch's head commit.
	Head string `json:"head,omitempty"`
	// ComposeFiles are the paths matching compose*.y*ml, Dockerfiles every
	// Dockerfile (any directory), Workflows every .github/workflows/* file.
	ComposeFiles []string   `json:"composeFiles"`
	Dockerfiles  []string   `json:"dockerfiles"`
	Workflows    []Workflow `json:"workflows"`
	// PlatformYAML says platform.yaml is at the repository's root on Head:
	// patching is skipped when it fits.
	PlatformYAML bool `json:"platformYaml"`
}

// Workflow is one GitHub Actions workflow.
type Workflow struct {
	ID    int64  `json:"id"`
	Path  string `json:"path"` // .github/workflows/deploy.yml
	Name  string `json:"name"`
	State string `json:"state"` // GitHub's: active, disabled_manually, ...
	// DeploysToVM marks a workflow that deploys to the VM (it runs docker
	// compose over ssh or on the host): connecting disables these.
	DeploysToVM bool `json:"deploysToVm"`
	// LastSuccess is its newest successful run, if any.
	LastSuccess *WorkflowRun `json:"lastSuccess,omitempty"`
}

// WorkflowRun is one workflow run.
type WorkflowRun struct {
	ID     int64     `json:"id"`
	Commit string    `json:"commit"` // head_sha
	At     time.Time `json:"at"`
}

// HTTPS is what resolving and connecting to the service address found.
type HTTPS struct {
	Host string `json:"host"` // <label>.hopcharlie.dev
	// Addresses are the public IPv4 addresses the name resolves to, and
	// TTLSeconds the record's TTL: syncing waits that long after the
	// record is gone.
	Addresses  []string `json:"addresses"`
	TTLSeconds int      `json:"ttlSeconds"`
	// Record is the individual record that points the name at the VM,
	// which its owner deletes so the wildcard reaches the gateway (human
	// action delete_dns_record).
	Record *DNSRecord `json:"record,omitempty"`
	// Responds says a TLS connection and an HTTP request to the host
	// answered; Status is that answer's status code.
	Responds bool `json:"responds"`
	Status   int  `json:"status,omitempty"`
	// CertNames are the leaf certificate's DNS names, CertNotAfter its
	// expiry.
	CertNames    []string  `json:"certNames"`
	CertNotAfter time.Time `json:"certNotAfter,omitzero"`
	// HealthCandidates are paths that answered 2xx (/healthz, /health,
	// /readyz, /), for platform.yaml's healthCheck.
	HealthCandidates []string `json:"healthCandidates"`
}

// DNSRecord is one DNS record.
type DNSRecord struct {
	Name  string `json:"name"`  // moimroom-legacy.hopcharlie.dev
	Type  string `json:"type"`  // A
	Value string `json:"value"` // 203.0.113.10
}

// Cloud providers.
const (
	CloudAWS = "aws"
	CloudGCP = "gcp"
)

// Instance is the VM the public address belongs to, from the cloud API.
type Instance struct {
	Cloud    string `json:"cloud"` // CloudAWS or CloudGCP
	ID       string `json:"id"`    // i-0abc... or the GCE instance name
	Region   string `json:"region"`
	Zone     string `json:"zone,omitempty"`    // AWS availability zone, GCE zone
	Project  string `json:"project,omitempty"` // GCP only
	Type     string `json:"type,omitempty"`    // t3.small, e2-small
	PublicIP string `json:"publicIp"`
	// ElasticIP is the allocation id when the public IP is an Elastic IP.
	ElasticIP string            `json:"elasticIp,omitempty"`
	Tags      map[string]string `json:"tags"`
	// AutoScalingGroup and EKSCluster are set when the instance belongs to
	// one (blocker vm_in_autoscaling_group, vm_is_eks_node).
	AutoScalingGroup string `json:"autoScalingGroup,omitempty"`
	EKSCluster       string `json:"eksCluster,omitempty"`
	// SSMManaged says SSM Run Command reaches it (AWS); without it the VM
	// is not inspected (blocker vm_not_ssm_managed).
	SSMManaged bool `json:"ssmManaged"`
}

// VM is what the SSM document HopCharlie-Inspect read inside the VM.
type VM struct {
	DockerVersion  string `json:"dockerVersion"`
	ComposeVersion string `json:"composeVersion"`
	// FreeDiskBytes is free space on docker's data root: under 2 GiB is
	// blocker disk_too_small (no room for wal_keep_size).
	FreeDiskBytes int64 `json:"freeDiskBytes"`
	// Project is the compose project (com.docker.compose.project), its
	// working directory and config files from the containers' labels.
	Project     string      `json:"project"`
	WorkingDir  string      `json:"workingDir"`
	ConfigFiles []string    `json:"configFiles"`
	Networks    []Network   `json:"networks"`
	Containers  []Container `json:"containers"`
	Volumes     []Volume    `json:"volumes"`
}

// Network is one docker network of the project.
type Network struct {
	Name   string `json:"name"` // moimroom_default
	Driver string `json:"driver"`
	// Subnet is the IPAM subnet (docker network inspect), the source of
	// the replication hba line.
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway,omitempty"`
	// IPAMConfigured says the compose file set the subnet itself; docker
	// refuses --ip on a network without it.
	IPAMConfigured bool `json:"ipamConfigured"`
}

// Container is one container of the project.
type Container struct {
	Name    string `json:"name"`    // moimroom-db-1
	Service string `json:"service"` // db (com.docker.compose.service)
	Image   string `json:"image"`   // as configured: postgres:17.11-alpine
	// ImageID is the local image id, RepoDigest its registry digest when
	// docker knows one.
	ImageID    string `json:"imageId"`
	RepoDigest string `json:"repoDigest,omitempty"`
	// Revision is the image's org.opencontainers.image.revision label:
	// one that is not GitHub.Head is drift.
	Revision string    `json:"revision,omitempty"`
	State    string    `json:"state"` // running, exited, ...
	Restart  string    `json:"restart,omitempty"`
	Ports    []Port    `json:"ports"`
	Networks []string  `json:"networks"`
	Mounts   []Mount   `json:"mounts"`
	Env      []EnvVar  `json:"env"`
	Health   string    `json:"health,omitempty"` // docker's health status
	Started  time.Time `json:"started,omitzero"`
	// MemoryLimitBytes is the container's memory limit, 0 for none.
	MemoryLimitBytes int64 `json:"memoryLimitBytes,omitempty"`
	// ReverseProxy marks the TLS reverse proxy in front of the app (caddy,
	// nginx, traefik and the like, publishing only 80 and 443): the
	// platform's gateway replaces it, and its volumes (certificate caches)
	// are VolumeProxy, not data.
	ReverseProxy bool `json:"reverseProxy,omitempty"`
}

// Port is one published or exposed port.
type Port struct {
	Container int    `json:"container"`
	Host      int    `json:"host,omitempty"` // 0 when not published
	Protocol  string `json:"protocol"`       // tcp, udp
}

// Mount is one mount of a container.
type Mount struct {
	Type        string `json:"type"`        // volume, bind, tmpfs
	Source      string `json:"source"`      // volume name or host path
	Destination string `json:"destination"` // path in the container
	ReadOnly    bool   `json:"readOnly,omitempty"`
}

// Volume is one named volume of the project.
type Volume struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	// UsedBy are the containers that mount it; Role is what the platform
	// takes it for: VolumeDatabase (the DB's PGDATA), VolumeObjectStorage
	// (an in-VM MinIO, blocker storage_out_of_scope), VolumeProxy (the
	// reverse proxy's), VolumeEmpty or VolumeData (any other data, blocker
	// data_volume_out_of_scope).
	UsedBy []string `json:"usedBy"`
	Role   string   `json:"role"`
}

// Volume roles.
const (
	VolumeDatabase      = "database"
	VolumeObjectStorage = "object_storage"
	VolumeData          = "data"
	// VolumeEmpty is a volume with no data worth moving (size 0, a cache).
	VolumeEmpty = "empty"
	// VolumeProxy is a volume of the ReverseProxy container: its
	// certificates and caches, which the gateway does not need.
	VolumeProxy = "proxy"
)

// EnvVar is one environment variable of a container, without its value.
type EnvVar struct {
	Name string `json:"name"`
	// Secret marks a value the platform keeps sealed and gives the app as
	// a secret (platform.yaml secrets) rather than in env.
	Secret bool `json:"secret"`
	// Excluded says the value is not carried over, and why: ExcludePlatform
	// (a name the platform sets: DATABASE_URL, PG*, S3_*, ADR 0024's
	// reserved names), ExcludeComposeRef (the value names a compose
	// service, such as db:5432) or ExcludeDBContainer (a variable of the
	// database container, such as POSTGRES_PASSWORD, which the platform
	// keeps only as the fence user's password). Empty means it is carried
	// over.
	Excluded string `json:"excluded,omitempty"`
}

// Why an environment variable is not carried over.
const (
	ExcludePlatform    = "platform"
	ExcludeComposeRef  = "compose_ref"
	ExcludeDBContainer = "db_container"
)

// Libc of the source database's image.
const (
	LibcGlibc = "glibc"
	LibcMusl  = "musl"
)

// DB is what HopCharlie-Inspect read with psql inside the database
// container (ADR 0042, the table under "1. 조사").
type DB struct {
	Container string `json:"container"` // moimroom-db-1
	Service   string `json:"service"`   // db, the host name on the compose network
	Port      int    `json:"port"`      // 5432
	// ServerVersionNum is server_version_num (170011); Version is version().
	ServerVersionNum int    `json:"serverVersionNum"`
	Version          string `json:"version"`
	// Libc is LibcMusl when version() says linux-musl, else LibcGlibc;
	// Debian is the glibc build's release from pgdg11/12/13 (bullseye,
	// bookworm, trixie), empty for musl.
	Libc   string `json:"libc"`
	Debian string `json:"debian,omitempty"`
	// EnvDatabase and EnvUser are the container's POSTGRES_DB and
	// POSTGRES_USER; Database and Owner the database the app uses and its
	// owner, which the chart gets (app_databases.database, owner).
	EnvDatabase string     `json:"envDatabase,omitempty"`
	EnvUser     string     `json:"envUser,omitempty"`
	Database    string     `json:"database"`
	Owner       string     `json:"owner"`
	Databases   []Database `json:"databases"`
	// LCMonetary, LCNumeric, LCTime and LCMessages are the server's
	// lc_* settings: any that is not C or POSIX rules out minimal-*.
	LCMonetary string `json:"lcMonetary"`
	LCNumeric  string `json:"lcNumeric"`
	LCTime     string `json:"lcTime"`
	LCMessages string `json:"lcMessages"`
	// AutoConf are the names (never the values) of the settings whose
	// sourcefile is postgresql.auto.conf (blocker auto_conf_settings):
	// bootstrap copies them to the target.
	AutoConf []string `json:"autoConf"`
	// AllowAlterSystem is allow_alter_system (PostgreSQL 17 and later; empty
	// before): off is blocker alter_system_disabled, as the fence is ALTER
	// SYSTEM. WALLevel minimal is blocker wal_level_minimal and
	// MaxWALSenders 0 blocker max_wal_senders_zero: either rules out
	// streaming. PasswordEncryption md5 is blocker password_encryption_md5:
	// the replication account would get an md5 hash its scram hba line
	// refuses.
	AllowAlterSystem   string `json:"allowAlterSystem,omitempty"`
	WALLevel           string `json:"walLevel"`
	MaxWALSenders      int    `json:"maxWalSenders"`
	PasswordEncryption string `json:"passwordEncryption"`
	// ReadOnlyRoleSettings are pg_db_role_setting entries that set
	// default_transaction_read_only (blocker role_read_only_setting), as
	// "role@database" with an empty side for "any".
	ReadOnlyRoleSettings []string `json:"readOnlyRoleSettings"`
	// WALKeepSize is wal_keep_size and WALKeepSizeSource its source in
	// pg_settings: "command line" is blocker wal_keep_size_command_line.
	WALKeepSize       string `json:"walKeepSize"`
	WALKeepSizeSource string `json:"walKeepSizeSource"`
	// Superusers are the roles with rolsuper and rolcanlogin;
	// PostgresRole says a role named postgres exists (connecting creates
	// it when not, ADR 0042 "3."). FenceUser is the superuser the fence
	// uses: EnvUser when it is one of them.
	Superusers   []string `json:"superusers"`
	PostgresRole bool     `json:"postgresRole"`
	FenceUser    string   `json:"fenceUser,omitempty"`
	// HBAFile and ConfigFile are hba_file and config_file, in PGDATA for
	// the official image; DataDirectory is data_directory.
	HBAFile       string `json:"hbaFile"`
	ConfigFile    string `json:"configFile"`
	DataDirectory string `json:"dataDirectory"`
	// WALBytesPerSecond is the WAL written per second over the inspection
	// (two pg_current_wal_lsn reads), for the sync time estimate.
	WALBytesPerSecond int64 `json:"walBytesPerSecond"`
	Connections       int   `json:"connections"`
	MaxConnections    int   `json:"maxConnections"`
	// Indexes are the indexes whose collation is not C-like
	// (pg_index.indcollation, oid 100 read as the database's), the
	// REINDEX candidates when the source is musl with a libc locale.
	Indexes []Index `json:"indexes"`
	// CollationDependent are text range partition keys and CHECK
	// constraints that depend on a collation that is not C, as
	// "schema.table.name": REINDEX cannot fix them (blocker
	// collation_dependent_constraint).
	CollationDependent []string `json:"collationDependent"`
	// Tables are the user tables with their sizes, for the fence gate's
	// time estimate.
	Tables []Table `json:"tables"`
}

// Database is one row of pg_database (templates left out).
type Database struct {
	Name        string `json:"name"`
	Owner       string `json:"owner"`
	LocProvider string `json:"locProvider"` // c (libc), i (icu), b (builtin)
	Collate     string `json:"collate"`
	CType       string `json:"ctype"`
	// ICULocale is datlocale (icu or builtin provider).
	ICULocale   string `json:"iculocale,omitempty"`
	CollVersion string `json:"collVersion,omitempty"`
	SizeBytes   int64  `json:"sizeBytes"`
}

// Index is one index.
type Index struct {
	Name      string `json:"name"` // schema.index, the form of target.reindex
	Table     string `json:"table"`
	Collation string `json:"collation"` // the collation it sorts by
	SizeBytes int64  `json:"sizeBytes"`
}

// Table is one user table.
type Table struct {
	Name      string `json:"name"` // schema.table
	Rows      int64  `json:"rows"` // reltuples
	SizeBytes int64  `json:"sizeBytes"`
}

// Object storage providers.
const (
	StorageAWS = "aws_s3"
	StorageGCS = "gcs"
	// StorageInVM is a store inside the VM, such as compose's MinIO
	// (blocker storage_out_of_scope).
	StorageInVM  = "in_vm"
	StorageOther = "other"
)

// Storage is the app's object storage.
type Storage struct {
	Provider string `json:"provider"`
	// Endpoint is scheme://host[:port] only (Endpoint strips the rest).
	Endpoint string `json:"endpoint"` // https://s3.ap-northeast-2.amazonaws.com
	Region   string `json:"region,omitempty"`
	Bucket   string `json:"bucket"`
	// The variables that hold them, of the container that uses them.
	Container      string `json:"container"`
	EndpointEnv    string `json:"endpointEnv,omitempty"`
	BucketEnv      string `json:"bucketEnv,omitempty"`
	AccessKeyEnv   string `json:"accessKeyEnv,omitempty"`
	SecretKeyEnv   string `json:"secretKeyEnv,omitempty"`
	RegionEnv      string `json:"regionEnv,omitempty"`
	ObjectCount    int64  `json:"objectCount"`
	TotalSizeBytes int64  `json:"totalSizeBytes"`
}

// Target is what the platform chose for the app's database.
type Target struct {
	Env string `json:"env"` // eks for AWS, gke for GCP unless asked otherwise
	// Image is the CNPG image with its digest
	// (ghcr.io/cloudnative-pg/postgresql:17.11-standard-bookworm@sha256:...),
	// app_databases.image; ImageReason says which rule picked it.
	Image       string `json:"image"`
	ImageReason string `json:"imageReason"`
	// StorageSize is the PVC size: twice pg_database_size, at least 1Gi.
	StorageSize string `json:"storageSize"` // 2Gi
	// Reindex are the "schema.index" names rebuilt after the promotion
	// (app_databases.reindex, the engine plan's target.reindex).
	Reindex []string `json:"reindex"`
	// PublicService, Port and HealthCheck are the service platform.yaml
	// makes public: the compose service the reverse proxy sends to.
	PublicService string `json:"publicService"`
	Port          int    `json:"port"`
	HealthCheck   string `json:"healthCheck,omitempty"`
}

// Report is what discovery and checking concluded.
type Report struct {
	Blockers []Finding `json:"blockers"`
	Warnings []Finding `json:"warnings"`
	// Drift is set when what runs differs from the default branch's head;
	// it goes first in the report and does not block.
	Drift *Drift `json:"drift,omitempty"`
	// Hold is the estimate of the write hold, checked in checking.
	Hold *HoldEstimate `json:"hold,omitempty"`
}

// Finding is one blocker or warning.
type Finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Subject is what it is about: a container, a setting, an index.
	Subject string `json:"subject,omitempty"`
}

// Drift compares what runs on the VM with the repository.
type Drift struct {
	Head string `json:"head"` // the default branch's head
	// RunningRevision is the revision label of the image that runs (the
	// public service's), LastDeployCommit the newest successful deploying
	// workflow run's commit; either may be empty when unknown.
	RunningRevision  string `json:"runningRevision,omitempty"`
	LastDeployCommit string `json:"lastDeployCommit,omitempty"`
}

// HoldBudgetSeconds is the write hold budget: the gateway's MaxHold (55s)
// less 15s of margin (ADR 0042 "5.").
const HoldBudgetSeconds = 40

// HoldEstimate is the expected time of what runs while writes are held.
// TotalSeconds over BudgetSeconds is blocker hold_budget_exceeded.
type HoldEstimate struct {
	FinalCopySeconds float64 `json:"finalCopySeconds"`
	ReplaySeconds    float64 `json:"replaySeconds"`
	FenceGateSeconds float64 `json:"fenceGateSeconds"`
	PromoteSeconds   float64 `json:"promoteSeconds"`
	ReindexSeconds   float64 `json:"reindexSeconds"`
	AppStartSeconds  float64 `json:"appStartSeconds"` // measured in checking
	TotalSeconds     float64 `json:"totalSeconds"`
	BudgetSeconds    float64 `json:"budgetSeconds"`
}

// Blocker codes (ADR 0042, "차단 사유를 한곳에 모으면").
const (
	BlockInstanceNotFound    = "instance_not_found"      // no EC2/GCE instance has the public IP
	BlockGCPInspection       = "gcp_inspection_not_yet"  // GCE: inside the VM is the next step
	BlockNotSSMManaged       = "vm_not_ssm_managed"      // no SSM agent or instance profile
	BlockAutoScalingGroup    = "vm_in_autoscaling_group" // aws:autoscaling:groupName
	BlockEKSNode             = "vm_is_eks_node"          // eks:cluster-name
	BlockNoCompose           = "no_compose_project"      // no compose project runs on the VM
	BlockDBNotContainer      = "db_not_postgres_container"
	BlockManagedDatabase     = "managed_database"     // RDS, Cloud SQL
	BlockStorageOutOfScope   = "storage_out_of_scope" // files in the VM (compose MinIO)
	BlockDataVolume          = "data_volume_out_of_scope"
	BlockDomainOutOfScope    = "domain_out_of_scope" // not <label>.hopcharlie.dev
	BlockNotHTTPS            = "not_https"
	BlockNoImage             = "image_unavailable" // no CNPG image of the major and libc
	BlockICUVersion          = "icu_version_unavailable"
	BlockCollationConstraint = "collation_dependent_constraint"
	BlockAutoConf            = "auto_conf_settings"
	BlockRoleReadOnly        = "role_read_only_setting"
	BlockWALKeepSizeCmdLine  = "wal_keep_size_command_line"
	BlockNoSuperuser         = "no_superuser"
	BlockDiskTooSmall        = "disk_too_small"
	BlockHoldBudget          = "hold_budget_exceeded"
	BlockAppCodeChange       = "app_code_change_required" // the contract needs app code changes
	BlockCheckFailed         = "check_failed"             // the test deployment failed twice
	BlockAlterSystemOff      = "alter_system_disabled"    // allow_alter_system off
	BlockWALLevelMinimal     = "wal_level_minimal"
	BlockNoWALSenders        = "max_wal_senders_zero"
	BlockPasswordMD5         = "password_encryption_md5"
	BlockNameTooLong         = "name_too_long" // <cluster>-to-legacy over 63 characters
)

// BlockerCodes are every Block* code, in the order above; the OpenAPI enum
// of OnboardingFinding.code holds them and WarningCodes.
var BlockerCodes = []string{
	BlockInstanceNotFound, BlockGCPInspection, BlockNotSSMManaged, BlockAutoScalingGroup, BlockEKSNode,
	BlockNoCompose, BlockDBNotContainer, BlockManagedDatabase, BlockStorageOutOfScope, BlockDataVolume,
	BlockDomainOutOfScope, BlockNotHTTPS, BlockNoImage, BlockICUVersion, BlockCollationConstraint,
	BlockAutoConf, BlockRoleReadOnly, BlockWALKeepSizeCmdLine, BlockNoSuperuser, BlockDiskTooSmall,
	BlockHoldBudget, BlockAppCodeChange, BlockCheckFailed, BlockAlterSystemOff, BlockWALLevelMinimal,
	BlockNoWALSenders, BlockPasswordMD5, BlockNameTooLong,
}

// Warning codes.
const (
	WarnDriftRevision    = "drift_running_revision" // Drift.RunningRevision is not Head
	WarnDriftDeploy      = "drift_last_deploy"      // Drift.LastDeployCommit is not Head
	WarnGlibcRelease     = "glibc_release_mismatch" // collation version warnings remain
	WarnReindex          = "reindex_required"       // Target.Reindex is not empty
	WarnAppSuperuser     = "app_account_superuser"  // advice: the app account is a superuser
	WarnWorkflowsStopped = "legacy_deploy_stopped"  // shown from connecting until decommissioning
)

// WarningCodes are every Warn* code, in the order above.
var WarningCodes = []string{
	WarnDriftRevision, WarnDriftDeploy, WarnGlibcRelease, WarnReindex, WarnAppSuperuser, WarnWorkflowsStopped,
}

// Endpoint returns an object storage endpoint as Storage.Endpoint keeps it:
// scheme://host[:port], without user info (a key written in the URL),
// path, query or fragment.
func Endpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("object storage endpoint is not an http(s) URL with a host")
	}
	return u.Scheme + "://" + u.Host, nil
}
