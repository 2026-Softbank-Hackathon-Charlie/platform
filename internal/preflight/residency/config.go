// Package residency evaluates whole-cluster physical replication against a
// trusted, immutable residency catalog using embedded OPA/Rego policy.
package residency

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"sigs.k8s.io/yaml"
)

var ErrInvalidConfig = errors.New("invalid residency configuration")

// Config is server-owned policy data, not caller-supplied migration metadata.
// Version 1 supports only kr-only, apac and global classifications. APAC is an
// explicitly configured country set; there is no inferred regional membership.
type Config struct {
	Version       int              `json:"version"`
	Environments  []Environment    `json:"environments"`
	APACCountries []string         `json:"apacCountries"`
	Catalog       []ClusterCatalog `json:"catalog"`
	// Buckets plans may copy files between. Optional: without an entry, a plan
	// that copies files is denied.
	Buckets []Bucket `json:"buckets,omitempty"`
	// InspectionEnvironment attests where this API's temporary staging storage
	// resides. Unknown content must be inspected in KR, also in the APAC set.
	InspectionEnvironment string `json:"inspectionEnvironment,omitempty"`
}

// Bucket is a registered object bucket. Environment is where the bucket is
// stored (its country is that environment's), as the operator declares it:
// the location is not looked up from the cloud. Label classifies the whole
// bucket's contents, which apply when it is the source of a copy.
type Bucket struct {
	Bucket      string `json:"bucket"`
	Environment string `json:"environment"`
	Label       string `json:"label"`
	// Expect is how the gate confirms that the bucket is where Environment
	// says. Without it every copy that uses the bucket is denied
	// (bucket_location_unverified): a declaration alone is not evidence.
	Expect *BucketExpect `json:"expect,omitempty"`
	// Prefixes classifies the bucket's first-level prefixes, as Catalog does
	// a cluster's tables. When set, the source's listed prefixes must match
	// it exactly, each prefix's label applies to its objects, and Label
	// covers only objects directly under the bucket path.
	Prefixes      []ClassifiedPrefix        `json:"prefixes,omitempty"`
	ReviewedFiles []preflight.ContentReview `json:"reviewedFiles,omitempty"`
}

// BucketExpect is what the measured bucket must show. A provider-hosted
// bucket (AWS S3, GCS) needs Locations; a self-hosted one (MinIO) needs
// EndpointCIDRs, since its reported region is only its own configuration.
type BucketExpect struct {
	// Locations are lowercase provider location codes the bucket may report,
	// e.g. ap-northeast-2 (S3) or asia-northeast3 (GCS).
	Locations []string `json:"locations,omitempty"`
	// EndpointCIDRs are the networks every address of the endpoint must be
	// in, e.g. the on-prem LAN or its Tailscale range.
	EndpointCIDRs []string `json:"endpointCIDRs,omitempty"`
}

// ClassifiedPrefix labels the objects under one first-level prefix.
type ClassifiedPrefix struct {
	Prefix string `json:"prefix"`
	Label  string `json:"label"`
}

type Environment struct {
	ID      string `json:"id"`
	Country string `json:"country"`
}

type ClusterCatalog struct {
	Scope    preflight.ClusterScope `json:"scope"`
	Datasets []ClassifiedDataset    `json:"datasets"`
}

type ClassifiedDataset struct {
	Dataset preflight.Dataset `json:"dataset"`
	Label   string            `json:"label"`
}

// ParseConfig rejects duplicate YAML keys and unknown fields. Parser errors are
// deliberately not wrapped: they can contain values from a malformed secret.
// No filesystem reads, environment expansion or network access take place.
func ParseConfig(raw []byte) (Config, error) {
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("%w: malformed document or unknown fields", ErrInvalidConfig)
	}
	return cfg.normalized()
}

var (
	// locationID is a provider location code (lowercase).
	locationID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	// prefixID is one first-level prefix, always ending in "/".
	prefixID   = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._=-]{0,127}/$`)
	resourceID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// bucketID is "remote:bucket[/path]", the form plans accept for files.
	bucketID = regexp.MustCompile(`^[A-Za-z0-9_-]+:[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9._-]+)*$`)
)

// ValidBucket reports whether s is a bucket the policy can name.
func ValidBucket(s string) bool {
	if len(s) > 512 || !bucketID.MatchString(s) {
		return false
	}
	for _, segment := range strings.Split(s, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// ValidPrefix reports whether s is a first-level prefix the policy can name.
func ValidPrefix(s string) bool { return prefixID.MatchString(s) }

// ValidLocation reports whether s is a location code the policy can name.
func ValidLocation(s string) bool { return locationID.MatchString(s) }

func validLabel(s string) bool { return s == "kr-only" || s == "apac" || s == "global" }

// ValidResourceID reports whether s is an identifier the policy accepts for
// environments, namespaces, clusters and movements.
func ValidResourceID(s string) bool { return resourceID.MatchString(s) }

// EnvironmentIDs returns the environments the configuration registers.
func (c Config) EnvironmentIDs() []string {
	out := make([]string, 0, len(c.Environments))
	for _, e := range c.Environments {
		out = append(out, e.ID)
	}
	return out
}

// ISO 3166-1 alpha-2 country/territory codes. Unknown/user-assigned codes and
// macroregions are rejected, never interpreted as global. Changes are reviewed
// code changes rather than geography inferred from an agent's claim.
const countries = " AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW "

func validCountry(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z' && strings.Contains(countries, " "+s+" ")
}

func validScope(s preflight.ClusterScope) bool {
	return resourceID.MatchString(s.Environment) && resourceID.MatchString(s.Namespace) && resourceID.MatchString(s.Cluster)
}

func validDataset(d preflight.Dataset) bool { return preflight.ValidDataset(d) }

func compareScope(a, b preflight.ClusterScope) int {
	return cmp.Or(cmp.Compare(a.Environment, b.Environment), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Cluster, b.Cluster))
}

func compareDataset(a, b preflight.Dataset) int {
	return cmp.Or(cmp.Compare(a.Database, b.Database), cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Table, b.Table))
}

// normalized validates every entry and deep-copies all slices before sorting.
// Returning a new configuration avoids both caller mutation and order-dependent
// policy digests. Errors describe locations, never echo untrusted values.
func (c Config) normalized() (Config, error) {
	bad := func(location string) (Config, error) {
		return Config{}, fmt.Errorf("%w: %s", ErrInvalidConfig, location)
	}
	if c.Version != 1 {
		return bad("version must be 1")
	}
	if len(c.Environments) == 0 || len(c.Catalog) == 0 || len(c.APACCountries) == 0 {
		return bad("environments, catalog and apacCountries must be nonempty")
	}
	out := Config{Version: c.Version, Environments: slices.Clone(c.Environments), APACCountries: slices.Clone(c.APACCountries)}
	envs := map[string]bool{}
	for i, e := range out.Environments {
		if !resourceID.MatchString(e.ID) || !validCountry(e.Country) || envs[e.ID] {
			return bad(fmt.Sprintf("environments[%d]: invalid or duplicate entry", i))
		}
		envs[e.ID] = true
	}
	seenCountries := map[string]bool{}
	for i, country := range out.APACCountries {
		if !validCountry(country) || seenCountries[country] {
			return bad(fmt.Sprintf("apacCountries[%d]: invalid or duplicate entry", i))
		}
		seenCountries[country] = true
	}
	if c.InspectionEnvironment != "" {
		found := false
		for _, e := range out.Environments {
			if e.ID == c.InspectionEnvironment && e.Country == "KR" && slices.Contains(out.APACCountries, "KR") {
				found = true
			}
		}
		if !found {
			return bad("inspectionEnvironment must identify a KR environment included in apacCountries")
		}
		out.InspectionEnvironment = c.InspectionEnvironment
	}
	seenScopes := map[preflight.ClusterScope]bool{}
	for i, entry := range c.Catalog {
		if !validScope(entry.Scope) || !envs[entry.Scope.Environment] || seenScopes[entry.Scope] || len(entry.Datasets) == 0 {
			return bad(fmt.Sprintf("catalog[%d]: invalid, unregistered or duplicate scope, or empty datasets", i))
		}
		seenScopes[entry.Scope] = true
		copyEntry := ClusterCatalog{Scope: entry.Scope, Datasets: slices.Clone(entry.Datasets)}
		seenDatasets := map[preflight.Dataset]bool{}
		for j, row := range copyEntry.Datasets {
			if !validDataset(row.Dataset) || seenDatasets[row.Dataset] {
				return bad(fmt.Sprintf("catalog[%d].datasets[%d]: invalid or duplicate identifier", i, j))
			}
			seenDatasets[row.Dataset] = true
			if !validLabel(row.Label) {
				return bad(fmt.Sprintf("catalog[%d].datasets[%d]: unsupported label", i, j))
			}
		}
		slices.SortFunc(copyEntry.Datasets, func(a, b ClassifiedDataset) int { return compareDataset(a.Dataset, b.Dataset) })
		out.Catalog = append(out.Catalog, copyEntry)
	}
	seenBuckets := map[string]bool{}
	for i, b := range c.Buckets {
		if !ValidBucket(b.Bucket) || seenBuckets[b.Bucket] || !envs[b.Environment] || !validLabel(b.Label) {
			return bad(fmt.Sprintf("buckets[%d]: invalid, duplicate or unregistered environment, or unsupported label", i))
		}
		seenBuckets[b.Bucket] = true
		nb, err := normalizedBucket(b)
		if err != nil {
			return bad(fmt.Sprintf("buckets[%d]: %v", i, err))
		}
		out.Buckets = append(out.Buckets, nb)
	}
	slices.SortFunc(out.Buckets, func(a, b Bucket) int { return cmp.Compare(a.Bucket, b.Bucket) })
	slices.SortFunc(out.Environments, func(a, b Environment) int { return cmp.Compare(a.ID, b.ID) })
	slices.Sort(out.APACCountries)
	slices.SortFunc(out.Catalog, func(a, b ClusterCatalog) int { return compareScope(a.Scope, b.Scope) })
	return out, nil
}

// normalizedBucket validates and canonicalizes expect and prefixes. CIDRs are
// rewritten to their masked form so equal networks give equal digests.
func normalizedBucket(b Bucket) (Bucket, error) {
	out := Bucket{Bucket: b.Bucket, Environment: b.Environment, Label: b.Label}
	if e := b.Expect; e != nil {
		if len(e.Locations) == 0 && len(e.EndpointCIDRs) == 0 {
			return Bucket{}, errors.New("expect needs locations or endpointCIDRs")
		}
		ne := &BucketExpect{}
		for _, l := range e.Locations {
			if !locationID.MatchString(l) || slices.Contains(ne.Locations, l) {
				return Bucket{}, errors.New("expect.locations: invalid (lowercase location codes) or duplicate entry")
			}
			ne.Locations = append(ne.Locations, l)
		}
		for _, c := range e.EndpointCIDRs {
			p, err := netip.ParsePrefix(c)
			if err != nil {
				return Bucket{}, errors.New("expect.endpointCIDRs: invalid CIDR")
			}
			// A network that holds every address verifies nothing.
			if p.Bits() == 0 {
				return Bucket{}, errors.New("expect.endpointCIDRs: a /0 network is not a location")
			}
			c = p.Masked().String()
			if slices.Contains(ne.EndpointCIDRs, c) {
				return Bucket{}, errors.New("expect.endpointCIDRs: duplicate entry")
			}
			ne.EndpointCIDRs = append(ne.EndpointCIDRs, c)
		}
		slices.Sort(ne.Locations)
		slices.Sort(ne.EndpointCIDRs)
		out.Expect = ne
	}
	seen := map[string]bool{}
	for _, p := range b.Prefixes {
		if !prefixID.MatchString(p.Prefix) || seen[p.Prefix] || !validLabel(p.Label) {
			return Bucket{}, errors.New("prefixes: invalid (one level ending in /), duplicate entry or unsupported label")
		}
		seen[p.Prefix] = true
		out.Prefixes = append(out.Prefixes, p)
	}
	slices.SortFunc(out.Prefixes, func(a, b ClassifiedPrefix) int { return cmp.Compare(a.Prefix, b.Prefix) })
	seenHashes := map[string]bool{}
	for _, r := range b.ReviewedFiles {
		if !preflight.ValidContentReview(r) || seenHashes[r.SHA256] {
			return Bucket{}, errors.New("reviewedFiles: invalid or duplicate digest or unsupported label")
		}
		seenHashes[r.SHA256] = true
		out.ReviewedFiles = append(out.ReviewedFiles, r)
	}
	slices.SortFunc(out.ReviewedFiles, func(a, b preflight.ContentReview) int { return cmp.Compare(a.SHA256, b.SHA256) })
	return out, nil
}
