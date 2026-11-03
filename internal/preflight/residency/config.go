// Package residency evaluates whole-cluster physical replication against a
// trusted, immutable residency catalog using embedded OPA/Rego policy.
package residency

import (
	"cmp"
	"errors"
	"fmt"
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
	resourceID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// Deliberately restrict v1 to simple PostgreSQL identifiers. Quoted names
	// containing spaces, dots or control characters need an explicit extension.
	datasetID = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$-]{0,62}$`)
)

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

func validDataset(d preflight.Dataset) bool {
	return datasetID.MatchString(d.Database) && datasetID.MatchString(d.Schema) && datasetID.MatchString(d.Table)
}

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
			switch row.Label {
			case "kr-only", "apac", "global":
			default:
				return bad(fmt.Sprintf("catalog[%d].datasets[%d]: unsupported label", i, j))
			}
		}
		slices.SortFunc(copyEntry.Datasets, func(a, b ClassifiedDataset) int { return compareDataset(a.Dataset, b.Dataset) })
		out.Catalog = append(out.Catalog, copyEntry)
	}
	slices.SortFunc(out.Environments, func(a, b Environment) int { return cmp.Compare(a.ID, b.ID) })
	slices.Sort(out.APACCountries)
	slices.SortFunc(out.Catalog, func(a, b ClusterCatalog) int { return compareScope(a.Scope, b.Scope) })
	return out, nil
}
