package store

import (
	"bytes"
	"fmt"
	"maps"
	"time"
)

// AppDatabase is the database pair the platform made for the default
// instance of an app registered in exactly two environments (ADR 0030):
// the database is born primary in PrimaryEnv and a replica of it in
// PeerEnv. Both clusters get the same password of the app account and
// issue their certificates with the same CA. The password and the CA key
// are only stored encrypted (secrets.Cipher.DatabaseRow); the store never
// sees them.
type AppDatabase struct {
	App        string
	PrimaryEnv string // the app's first environment
	PeerEnv    string // the other one
	// KeyVersion and KeyCheck are those of the secrets key that encrypted
	// the password and the CA key, as in AppSecret.
	KeyVersion         int
	KeyCheck           string
	PasswordNonce      []byte
	PasswordCiphertext []byte
	CACert             string // PEM; public
	CAKeyNonce         []byte
	CAKeyCiphertext    []byte
	// PeerGeneration counts the removals of PeerEnv from the app (ADR
	// 0043). The peer's backup path has it once it is not 0, so a peer
	// added back archives to a new, empty path.
	PeerGeneration int
	// MemberGenerations counts the removals of each environment of the
	// database group from the app (ADR 0064), for the backup path of a
	// member added back. An environment not in it has Generation 0, but
	// PeerEnv, whose generation is PeerGeneration then. RemoveDatabaseMember
	// keeps PeerGeneration equal to the generation of PeerEnv.
	MemberGenerations map[string]int
	// ExplicitMembers says a member left the database group of three
	// environments (ADR 0064): the files keep values.dbTopology.members,
	// also with two environments, so the Clusters keep the members label
	// under which Argo CD owns their externalClusters (infra ADR 0098).
	ExplicitMembers bool
	CreatedAt       time.Time

	// SourceKind is DBSourcePeer for a pair born on the platform, or
	// DBSourceExternal for an onboarded app's database (ADR 0042), born in
	// PrimaryEnv as a replica of a source outside the platform until the
	// cutover succeeded. Empty is stored as DBSourcePeer. The deploy
	// renderer derives the onboarding values (deploy.File.Postgres,
	// DBTopology.External, OnboardingCold) from this row and the app's
	// onboarding on every deployment, not from the deployment that asked,
	// so a push while the onboarding is syncing still renders the app cold.
	SourceKind string
	// Image is the CNPG image, pinned by digest, of every database of the
	// app (the peer's too); empty leaves the operator's default. Database
	// and Owner are the database's name and owner; empty is app.
	// StorageSize is the database's PVC size (2Gi); empty is the chart's
	// default.
	Image, Database, Owner, StorageSize string
	// ExternalHost is the source's tailnet hostname (hop-db-legacy-<app>),
	// ReplicationPassword* hopcharlie_replicator's password encrypted like
	// the app account's, and Reindex the "schema.index" names rebuilt after
	// the promotion. All empty without an external source; they stay after
	// the cutover, as the target's externalClusters entry legacy still
	// names the password's Secret.
	ExternalHost                  string
	ReplicationPasswordNonce      []byte
	ReplicationPasswordCiphertext []byte
	Reindex                       []string
	// Parameters are the PostgreSQL parameters every database of the app
	// is drawn with (deploy values.postgres.parameters, ADR 0042); nil for
	// every other app.
	Parameters map[string]string
}

// AppDatabase source kinds (ADR 0042).
const (
	DBSourcePeer     = "peer"
	DBSourceExternal = "external"
)

// Clean returns d with every string passed through CleanText and its byte
// slices copied. Stores apply it on every write.
func (d AppDatabase) Clean() AppDatabase {
	for _, f := range []*string{&d.App, &d.PrimaryEnv, &d.PeerEnv, &d.KeyCheck, &d.CACert,
		&d.SourceKind, &d.Image, &d.Database, &d.Owner, &d.StorageSize, &d.ExternalHost} {
		*f = CleanText(*f)
	}
	if d.SourceKind == "" {
		d.SourceKind = DBSourcePeer
	}
	d.PasswordNonce, d.PasswordCiphertext = bytes.Clone(d.PasswordNonce), bytes.Clone(d.PasswordCiphertext)
	d.CAKeyNonce, d.CAKeyCiphertext = bytes.Clone(d.CAKeyNonce), bytes.Clone(d.CAKeyCiphertext)
	d.ReplicationPasswordNonce = bytes.Clone(d.ReplicationPasswordNonce)
	d.ReplicationPasswordCiphertext = bytes.Clone(d.ReplicationPasswordCiphertext)
	d.Reindex = cleanList(d.Reindex)
	if len(d.Parameters) == 0 {
		d.Parameters = nil
	} else {
		p := make(map[string]string, len(d.Parameters))
		for k, v := range d.Parameters {
			p[CleanText(k)] = CleanText(v)
		}
		d.Parameters = p
	}
	if len(d.MemberGenerations) == 0 {
		d.MemberGenerations = nil
	} else {
		g := make(map[string]int, len(d.MemberGenerations))
		for k, v := range d.MemberGenerations {
			g[CleanText(k)] = v
		}
		d.MemberGenerations = g
	}
	return d
}

// Generation is the number of times env was removed from the app of the
// database group d (ADR 0043, 0064): its MemberGenerations entry, 0
// without one, and for PeerEnv at least PeerGeneration, which a binary
// from before ADR 0064 counts up alone.
func (d AppDatabase) Generation(env string) int {
	g := d.MemberGenerations[env]
	if env == d.PeerEnv {
		g = max(g, d.PeerGeneration)
	}
	return g
}

// cleanList returns l's strings passed through CleanText in a new slice,
// or nil when l is empty, as a text[] column reads back.
func cleanList(l []string) []string {
	if len(l) == 0 {
		return nil
	}
	out := make([]string, len(l))
	for i, v := range l {
		out[i] = CleanText(v)
	}
	return out
}

// RemoveMember returns d after env left the app of its database group, as
// Store.RemoveDatabaseMember stores it (ADR 0064): the Generation of env
// one more, and with peer other than "" peer as PeerEnv with its
// Generation kept. PeerGeneration is the Generation of PeerEnv after it.
// group (env leaves a group of three) sets ExplicitMembers, which then
// stays. ErrConflict when env is PrimaryEnv, peer is PrimaryEnv or env, or
// peer is given for an env that is not PeerEnv.
func (d AppDatabase) RemoveMember(env, peer string, group bool) (AppDatabase, error) {
	switch {
	case env == "" || env == d.PrimaryEnv:
		return d, fmt.Errorf("%w: %q is not a removable member of the database group of %s", ErrConflict, env, d.App)
	case peer != "" && (peer == d.PrimaryEnv || peer == env || env != d.PeerEnv):
		return d, fmt.Errorf("%w: %s cannot become the peer of the database group of %s in place of %s", ErrConflict, peer, d.App, env)
	}
	gens := make(map[string]int, len(d.MemberGenerations)+2)
	maps.Copy(gens, d.MemberGenerations)
	gens[env] = d.Generation(env) + 1
	switch {
	case peer != "":
		gens[peer] = d.Generation(peer)
		d.PeerEnv, d.PeerGeneration = peer, gens[peer]
	case env == d.PeerEnv:
		d.PeerGeneration = gens[env]
	default:
		d.PeerGeneration = d.Generation(d.PeerEnv)
	}
	d.MemberGenerations = gens
	d.ExplicitMembers = d.ExplicitMembers || group
	return d, nil
}
