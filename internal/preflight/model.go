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
// the plan writes them ("remote:bucket[/path]") and matched exactly.
type BucketCopy struct {
	Source string `json:"source"`
	Target string `json:"target"`
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
}

// ResidencyVerdict applies only to residency under the given policy snapshot.
// The zero value denies. An evaluation error also always returns Allowed=false.
// PolicyDigest binds the embedded policy and its configuration, NOT a plan.
type ResidencyVerdict struct {
	Allowed      bool     `json:"allowed"`
	PolicyDigest string   `json:"policyDigest"`
	Reasons      []Reason `json:"reasons"`
}
