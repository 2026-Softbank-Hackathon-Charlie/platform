package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// GitOps writes the gitops repository: the desired state Argo CD applies.
type GitOps interface {
	// Write commits files (repository paths to content) in one commit on the
	// branch. Files whose content is already there are left alone; when
	// nothing changes no commit is made, changed is false and commit is the
	// branch head. A push that is not a fast-forward is retried on the new
	// head.
	Write(ctx context.Context, files map[string][]byte, message string) (commit string, changed bool, err error)
	// Delete removes every file under each of dirs, or the file a dir names
	// (FilePath), in one commit, like Write. It refuses no dirs, the
	// repository's root and apps/ itself (AppDir of an empty name) with an
	// error, as none of them is one instance's directory or file.
	Delete(ctx context.Context, dirs []string, message string) (commit string, changed bool, err error)
	// Contains reports whether revision includes commit: it is commit or a
	// descendant of it. An unknown revision does not include it.
	Contains(ctx context.Context, revision, commit string) (bool, error)
}

// AppDir is an instance's directory in the gitops repository. The default
// instance is named after the app (ADR 0015).
func AppDir(instance string) string { return "apps/" + instance }

// FilePath is the gitops file of an instance in an environment.
func FilePath(instance, env string) string { return AppDir(instance) + "/" + env + ".yaml" }

// checkDelete is GitOps.Delete's refusal of what is not one instance's
// directory.
func checkDelete(dirs []string) error {
	if len(dirs) == 0 {
		return errors.New("no directory to delete")
	}
	for _, d := range dirs {
		switch strings.Trim(path.Clean("/"+d), "/") {
		case "", "apps":
			return fmt.Errorf("refusing to delete %q from the gitops repository: it is not one instance's directory", d)
		}
	}
	return nil
}

// Namespace is where an instance runs: the namespace of its gitops files.
func Namespace(instance string) string { return instance }

// ApplicationName is the Argo CD Application that the ApplicationSet makes
// from a gitops file: {{.namespace}}-{{.env}}.
func ApplicationName(namespace, env string) string { return namespace + "-" + env }

// ServiceName is the Kubernetes Service the app chart makes for a service.
func ServiceName(app, service string) string { return app + "-" + service }

// File is the content of one gitops file.
type File struct {
	// App is the app's name, after which the chart names the Deployments,
	// Services and database (<app>-<part>). Instance is the instance's
	// name: the namespace and the directory (App for the default instance).
	App, Instance, Env string
	Commit             string // the app commit
	Deployment         string // the deployment id
	Spec               appspec.Spec
	// Images maps every service to its image, pinned by digest.
	Images map[string]string
	// Sealed is values.sealedSecrets; nil or empty leaves the key out.
	Sealed *Sealed
	// DBTopology is values.dbTopology (ADR 0030); nil leaves the key out.
	// With it, Sealed must have PGApp and PGCA and Spec a postgres resource.
	DBTopology *DBTopology
	// Postgres is values.postgres (ADR 0042); nil leaves the key out, and
	// the chart draws the database as before.
	Postgres *PostgresValues
	// OnboardingCold is values.onboardingCold (ADR 0042): the app is being
	// onboarded and stays down until the cutover starts it.
	OnboardingCold bool
}

// The gitops values of an onboarded app (ADR 0042), the contract with the
// app chart's infra side. Render refuses them until the implementation
// renders them (errOnboardingValues); every key below is left out when
// unset, so the files of every other app stay byte for byte as they are.
// The shape, in Render's key order (values elided are as before):
//
//	values:
//	  app: "moimroom-legacy"
//	  services:
//	    - name: "web"
//	      ...
//	      replicas: 0            # every service while onboardingCold; else as before
//	      ...
//	  resources:
//	    - name: "db"
//	      type: "postgres"
//	  postgres:                  # File.Postgres; each key only when not empty
//	    image: "ghcr.io/cloudnative-pg/postgresql:17.11-standard-bookworm@sha256:<digest>"
//	    database: "moimroom"     # bootstrap database; the chart's default stays app
//	    owner: "moimroom"        # its owner and the app account; default app
//	    storageSize: "2Gi"       # the PVC; before topologyStorageSize and storageSize
//	  dbTopology:
//	    primary: "eks"           # the onboarding's target environment
//	    peer: "gke"
//	    peerPresent: false
//	    external:                # DBTopology.External, only in primary's file
//	      name: "legacy"
//	      host: "hop-db-legacy-moimroom-legacy"   # tailnet hostname; the chart appends profile.dbNetwork.tailnetDomain
//	      user: "hopcharlie_replicator"
//	      passwordSecret: "moimroom-legacy-db-legacy"   # <cluster>-legacy, from sealedSecrets.pgLegacy
//	      sslmode: "prefer"
//	  onboardingCold: true       # File.OnboardingCold; the key only when true
//	  sealedSecrets:
//	    ...
//	    pgApp: {...}             # as before: the app account, here the source's password
//	    pgCA: {...}
//	    pgLegacy:                # Sealed.PGLegacy: SealedSecret <cluster>-legacy,
//	      password: "<ciphertext>"   # kubernetes.io/basic-auth with username too
//	      username: "<ciphertext>"
//	    pgBackup: {...}
//	  backup: {...}
//
// The renderer takes them from the app's database row (source_kind
// external) and its onboarding on every deployment, not from the
// deployment that asked, so a push while syncing still renders cold.
//
// What the chart draws from them (ADR 0042 "4."), <cluster> being
// <app>-<postgres resource>:
//
//   - postgres.image is the Cluster's imageName in every file of the app,
//     postgres.storageSize its storage size, database and owner its
//     bootstrap's.
//   - With dbTopology.external, the Cluster is born from the source:
//     bootstrap.pg_basebackup {source: legacy, database, owner, secret:
//     <cluster>-app}; replica {primary: legacy, self: <cluster>-<primary>,
//     source: legacy}; externalClusters legacy (host <cluster>-to-legacy,
//     an egress Service to <host>.<tailnetDomain>, user, password from
//     passwordSecret, sslmode), itself, and the peer as for any pair; the
//     platform CA certificates; the egress Service and a replication
//     NetworkPolicy to the source. Without external (after the cutover)
//     the chart draws initdb again, which the ApplicationSet ignores with
//     /spec/bootstrap, and no Service or policy to the source.
//   - sealedSecrets.pgLegacy is drawn whenever it is there, with or
//     without external, as externalClusters legacy still names it.
//   - onboardingCold puts the label hopcharlie.dev/onboarding-cold: "true"
//     on every Deployment, whose /spec/replicas the ApplicationSet then
//     ignores, and replicas 0 is drawn as 0 (not default 1).
type PostgresValues struct {
	Image, Database, Owner, StorageSize string
}

// ExternalSource is values.dbTopology.external (ADR 0042): the database
// outside the platform the primary's Cluster is born a replica of.
type ExternalSource struct {
	Name           string // "legacy"
	Host           string // the source's tailnet hostname, hop-db-legacy-<app>
	User           string // hopcharlie_replicator
	PasswordSecret string // <cluster>-legacy, key password
	SSLMode        string // "prefer"
}

// errOnboardingValues is Render's refusal of the values of ADR 0042 until
// it renders them.
var errOnboardingValues = errors.New("the gitops values of an onboarding (ADR 0042) are not rendered yet")

// DBTopology is where an app's database pair is born (ADR 0030): Primary
// is the environment where it is born primary, Peer the other one, where it
// is born a replica. Both environments' files have the same values; the
// chart takes the side that is not the file's env as the one to follow.
// PeerAbsent says Peer is not one of the app's environments yet (ADR 0043):
// only Primary's file is rendered then, with values.dbTopology.peerPresent
// false, and the chart draws the same Cluster without the tailnet objects.
// With the peer present the key is left out, which the chart reads as true,
// so the files of a pair registered in two environments stay as they were.
type DBTopology struct {
	Primary, Peer string
	PeerAbsent    bool
	// Members extends the original pair; nil preserves the legacy values.
	Members []string
	// External is the source outside the platform of an onboarded app's
	// database (ADR 0042, see PostgresValues); nil for every other pair.
	External *ExternalSource
}

// fileHeader starts every gitops file.
const fileHeader = "# Generated by the HopCharlie platform. Do not edit.\n"

// Render returns the gitops file apps/<instance>/<env>.yaml in the format of
// ADR 0011, the contract with the app chart: app, env, namespace (the
// instance name), commit and deployment, then the chart values (app,
// services with their images, resources, and sealedSecrets when there is
// something sealed, ADR 0019). app and values.app are the app name in every
// instance (ADR 0015). A service env entry with the name of a sealed app
// secret is left out: the secret wins. Keys are always in that order and env
// keys sorted, so the same deployment always renders the same bytes. Every
// string is a double-quoted scalar, so no value or env key can turn into a
// number, boolean or null in any YAML reader (Helm reads YAML 1.1, where yes
// and 0755 are not strings).
func Render(f File) ([]byte, error) {
	var b bytes.Buffer
	q := func(s string) string {
		out, _ := json.Marshal(s) // a JSON string is a YAML double-quoted scalar
		return string(out)
	}
	if f.Instance == "" {
		return nil, fmt.Errorf("render the gitops file of %s in %s: no instance", f.App, f.Env)
	}
	if f.Postgres != nil || f.OnboardingCold || (f.DBTopology != nil && f.DBTopology.External != nil) ||
		(f.Sealed != nil && len(f.Sealed.PGLegacy) > 0) {
		return nil, fmt.Errorf("render %s: %w", FilePath(f.Instance, f.Env), errOnboardingValues)
	}
	b.WriteString(fileHeader)
	fmt.Fprintf(&b, "app: %s\n", q(f.App))
	fmt.Fprintf(&b, "env: %s\n", q(f.Env))
	fmt.Fprintf(&b, "namespace: %s\n", q(Namespace(f.Instance)))
	fmt.Fprintf(&b, "commit: %s\n", q(f.Commit))
	fmt.Fprintf(&b, "deployment: %s\n", q(f.Deployment))
	b.WriteString("values:\n")
	fmt.Fprintf(&b, "  app: %s\n", q(f.App))
	b.WriteString("  services:\n")
	for _, sv := range f.Spec.Services {
		image := f.Images[sv.Name]
		if image == "" {
			return nil, fmt.Errorf("render %s: no image for service %s", FilePath(f.Instance, f.Env), sv.Name)
		}
		fmt.Fprintf(&b, "    - name: %s\n", q(sv.Name))
		fmt.Fprintf(&b, "      image: %s\n", q(image))
		if sv.Port != 0 {
			fmt.Fprintf(&b, "      port: %d\n", sv.Port)
		}
		if sv.HealthCheck != "" {
			fmt.Fprintf(&b, "      healthCheck: %s\n", q(sv.HealthCheck))
		}
		fmt.Fprintf(&b, "      public: %t\n", sv.Public)
		fmt.Fprintf(&b, "      replicas: %d\n", max(sv.Replicas, 1))
		memory, cpu, err := sv.Quantities()
		if err != nil {
			return nil, fmt.Errorf("render %s: service %s: %w", FilePath(f.Instance, f.Env), sv.Name, err)
		}
		if memory != "" {
			fmt.Fprintf(&b, "      memory: %s\n", q(memory))
		}
		if cpu != "" {
			fmt.Fprintf(&b, "      cpu: %s\n", q(cpu))
		}
		var env []string
		for _, k := range sortedKeys(sv.Env) {
			if f.Sealed == nil || f.Sealed.App[k] == "" {
				env = append(env, k)
			}
		}
		if len(env) > 0 {
			b.WriteString("      env:\n")
			for _, k := range env {
				fmt.Fprintf(&b, "        %s: %s\n", q(k), q(sv.Env[k]))
			}
		}
	}
	if len(f.Spec.Resources) == 0 {
		b.WriteString("  resources: []\n")
	} else {
		b.WriteString("  resources:\n")
		for _, r := range f.Spec.Resources {
			fmt.Fprintf(&b, "    - name: %s\n", q(r.Name))
			fmt.Fprintf(&b, "      type: %s\n", q(r.Type))
		}
	}
	if t := f.DBTopology; t != nil {
		members := t.Members
		if len(members) == 0 {
			members = []string{t.Primary, t.Peer}
		}
		hasPG := slices.ContainsFunc(f.Spec.Resources, func(r appspec.Resource) bool { return r.Type == appspec.TypePostgres })
		switch {
		case !hasPG:
			return nil, fmt.Errorf("render %s: a database topology needs a postgres resource", FilePath(f.Instance, f.Env))
		case f.Sealed == nil || len(f.Sealed.PGApp) == 0 || len(f.Sealed.PGCA) == 0:
			return nil, fmt.Errorf("render %s: a database topology needs the sealed app account and CA", FilePath(f.Instance, f.Env))
		case t.Primary == t.Peer || !slices.Contains(members, f.Env):
			return nil, fmt.Errorf("render %s: database topology %s/%s does not fit environment %s", FilePath(f.Instance, f.Env), t.Primary, t.Peer, f.Env)
		case len(t.Members) > 0 && (len(members) < 2 || len(members) > 3 || t.PeerAbsent || !slices.Contains(members, t.Primary) || !slices.Contains(members, t.Peer) || slices.Contains(members, "") || len(slices.Compact(slices.Sorted(slices.Values(members)))) != len(members)):
			return nil, fmt.Errorf("render %s: invalid database group membership", FilePath(f.Instance, f.Env))
		case t.PeerAbsent && f.Env != t.Primary:
			return nil, fmt.Errorf("render %s: the peer %s of the database topology is not one of the app's environments, so only the primary's file is rendered", FilePath(f.Instance, f.Env), t.Peer)
		}
		b.WriteString("  dbTopology:\n")
		fmt.Fprintf(&b, "    primary: %s\n", q(t.Primary))
		fmt.Fprintf(&b, "    peer: %s\n", q(t.Peer))
		if len(t.Members) > 0 {
			b.WriteString("    members:\n")
			for _, member := range t.Members {
				fmt.Fprintf(&b, "      - %s\n", q(member))
			}
		}
		if t.PeerAbsent {
			b.WriteString("    peerPresent: false\n")
		}
	}
	if !f.Sealed.empty() {
		b.WriteString("  sealedSecrets:\n")
		if s3 := f.Sealed.S3; s3 != nil {
			b.WriteString("    s3:\n")
			fmt.Fprintf(&b, "      accessKey: %s\n", q(s3.AccessKey))
			fmt.Fprintf(&b, "      secretKey: %s\n", q(s3.SecretKey))
		}
		if len(f.Sealed.App) > 0 {
			b.WriteString("    app:\n")
			for _, k := range sortedKeys(f.Sealed.App) {
				fmt.Fprintf(&b, "      %s: %s\n", q(k), q(f.Sealed.App[k]))
			}
			fmt.Fprintf(&b, "    appChecksum: %s\n", q(f.Sealed.AppChecksum))
		}
		// Before pgBackup, whose values.backup ends the sealedSecrets map.
		for _, m := range []struct {
			key    string
			values map[string]string
		}{{"pgApp", f.Sealed.PGApp}, {"pgCA", f.Sealed.PGCA}} {
			if len(m.values) == 0 {
				continue
			}
			fmt.Fprintf(&b, "    %s:\n", m.key)
			for _, k := range sortedKeys(m.values) {
				fmt.Fprintf(&b, "      %s: %s\n", q(k), q(m.values[k]))
			}
		}
		if pb := f.Sealed.PGBackup; pb != nil {
			b.WriteString("    pgBackup:\n")
			fmt.Fprintf(&b, "      accessKey: %s\n", q(pb.AccessKey))
			fmt.Fprintf(&b, "      secretKey: %s\n", q(pb.SecretKey))
			b.WriteString("  backup:\n")
			fmt.Fprintf(&b, "    bucket: %s\n", q(pb.Bucket))
			fmt.Fprintf(&b, "    path: %s\n", q(pb.Path))
		}
	}
	return b.Bytes(), nil
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
