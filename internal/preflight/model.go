package preflight

import "regexp"

// Deliberately restrict v1 to simple PostgreSQL identifiers. Quoted names
// containing spaces, dots or control characters need an explicit extension.
var datasetID = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$-]{0,62}$`)

// ValidDataset reports whether every part of d is a supported identifier. The
// collector and the policy use the same rule, so a relation the policy cannot
// name makes an inventory incomplete instead of being silently dropped.
func ValidDataset(d Dataset) bool {
	return datasetID.MatchString(d.Database) && datasetID.MatchString(d.Schema) && datasetID.MatchString(d.Table)
}

// ClusterScope identifies the whole PostgreSQL instance, not a single database.
// Cluster is a CNPG resource name; Environment identifies the Kubernetes cluster.
type ClusterScope struct {
	Environment string `json:"environment"`
	Namespace   string `json:"namespace"`
	Cluster     string `json:"cluster"`
}

// Dataset identifies a relation without ambiguous dotted-name concatenation.
// Every database and schema in the physical copy must be inventoried. Names are
// identifiers only: credentials, connection strings and data values do not belong here.
type Dataset struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
	Table    string `json:"table"`
}

// Inventory is a snapshot produced by a trusted collector. Complete is a
// prerequisite, not an attestation: this package cannot prove that a collector
// actually inspected the entire cluster or that its snapshot is still current.
type Inventory struct {
	Scope    ClusterScope `json:"scope"`
	Complete bool         `json:"complete"`
	Datasets []Dataset    `json:"datasets"`
}

const PhysicalReplication = "physical-replication"

// ObjectCopy is a movement of an app without a database (ADR 0049): only
// Files is copied. Its Source has only Environment and its Inventory is
// empty; the policy's database rules do not apply to it.
const ObjectCopy = "object-copy"

// Movement describes one entire-cluster physical replication path. The first
// version supports no other kinds and has no caller-provided labels, countries,
// table selection or verification filter. Backup and restore execution are not
// implemented; their destination resolution needs a separate integration.
type Movement struct {
	ID          string       `json:"id"`
	Kind        string       `json:"kind"`
	Source      ClusterScope `json:"source"`
	Destination string       `json:"destination"`
	Inventory   Inventory    `json:"inventory"`
	// Files is the object copy that runs with the database, if any. Both
	// buckets must be registered in the policy, which says where each one is
	// and how the source's contents are classified (as one unit).
	Files *BucketCopy `json:"files,omitempty"`
}

// BucketCopy copies every object of Source to Target. Buckets are written as
// the plan writes them ("remote:bucket[/path]") and matched exactly. The
// observations are what the server measured through the same rclone remotes
// the copy uses; both are required whenever Files is set.
type BucketCopy struct {
	Source         string             `json:"source"`
	Target         string             `json:"target"`
	SourceObserved *BucketObservation `json:"sourceObserved"`
	TargetObserved *BucketObservation `json:"targetObserved"`
	// ContentLabels are computed from the staged bytes by trusted server code.
	// They add restrictions; they never replace the declared bucket/prefix labels.
	ContentLabels []string `json:"contentLabels,omitempty"`
}

// ContentReview is an operator-reviewed classification of exact object bytes.
type ContentReview struct {
	SHA256 string `json:"sha256"`
	Label  string `json:"label"`
}

var contentHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ValidContentReview(r ContentReview) bool {
	return contentHash.MatchString(r.SHA256) && (r.Label == "global" || r.Label == "apac" || r.Label == "kr-only")
}

// FileInspection is safe audit evidence. No object keys or data values belong here.
type FileInspection struct {
	Rules          string   `json:"rules"`
	Objects        int      `json:"objects"`
	Bytes          int64    `json:"bytes"`
	ManifestDigest string   `json:"manifestDigest"`
	Labels         []string `json:"labels"`
	// Findings counts matched scan segments (body/key/member/comment), not
	// individual values or necessarily distinct objects.
	Findings map[string]int `json:"findings"`
}

// Hosting of an observed bucket.
const (
	// HostingProvider: a cloud store (AWS S3, Google Cloud Storage) reached at
	// the provider's own endpoint. Its reported location is the provider's.
	HostingProvider = "provider"
	// HostingSelf: a store the operator runs (MinIO or any other S3-compatible
	// endpoint). Its "region" is whatever the operator configured and proves
	// nothing about where the disks are; only its network address does.
	HostingSelf = "self"
)

// BucketObservation is a measurement, never a declaration: the gate fills it
// from the store itself (bucketprobe package), not from a plan or an agent.
type BucketObservation struct {
	Hosting string `json:"hosting"`
	// Location is the store's lowercase location code (ap-northeast-2,
	// asia-northeast1, asia) and LocationType region, dual-region or
	// multi-region. A self-hosted store reports its configured region.
	Location     string `json:"location"`
	LocationType string `json:"locationType"`
	// EndpointIPs are the addresses the endpoint host resolved to, sorted.
	EndpointIPs []string `json:"endpointIPs"`
	// Prefixes are the first-level prefixes ("photos/") under the bucket path,
	// and "" when objects sit directly under it. Listed for the copy source.
	Prefixes []string `json:"prefixes"`
	// Replicas are the destination bucket names of the bucket's enabled
	// replication rules. Read for the copy target, where copied objects land.
	Replicas []string `json:"replicas"`
	// Tiered is true when a self-hosted store's lifecycle moves objects to
	// another tier (MinIO ILM transition), which can be another provider.
	Tiered bool `json:"tiered"`
}

// Request must be constructed server-side from resolved, trusted resources.
// Do not decode an external API body into this type and treat it as trusted.
// Callers must not mutate a request while evaluating it.
type Request struct {
	Movements []Movement `json:"movements"`
}

// Reason is a deterministic, machine-readable explanation without raw inputs
// or SDK error messages. MovementID and Dataset are present only after input
// identifier validation. Code is not a user-facing translation.
type Reason struct {
	Code       string   `json:"code"`
	MovementID string   `json:"movementId,omitempty"`
	Dataset    *Dataset `json:"dataset,omitempty"`
	Bucket     string   `json:"bucket,omitempty"`
	// Prefix is a first-level prefix of Bucket ("" is never reported).
	Prefix string `json:"prefix,omitempty"`
	// Replica is a bucket that Bucket replicates into.
	Replica string `json:"replica,omitempty"`
}

// ResidencyVerdict applies only to residency under the given policy snapshot.
// The zero value denies. An evaluation error also always returns Allowed=false.
// PolicyDigest binds the embedded policy and its configuration, NOT a plan.
type ResidencyVerdict struct {
	Allowed      bool     `json:"allowed"`
	PolicyDigest string   `json:"policyDigest"`
	Reasons      []Reason `json:"reasons"`
	// Buckets is what the gate measured about a files copy's buckets, as
	// evidence for the audit log. It never holds host names or credentials.
	Buckets []ObservedBucket `json:"buckets,omitempty"`
	Content *FileInspection  `json:"content,omitempty"`
}

// ObservedBucket pairs a registered bucket name with its observation.
type ObservedBucket struct {
	Bucket      string            `json:"bucket"`
	Observation BucketObservation `json:"observation"`
}
