package hopcharlie.residency

import rego.v1

# Inputs/configuration are structurally validated by Go. Geography decisions
# live only here. A verdict covers residency, never parity or permission to run.
default allow := false

allow if {
	count(input.movements) > 0
	count(deny) == 0
}

decision := {"allowed": allow, "reasons": deny}

environment_exists(id) if {
	some env in data.policy.environments
	env.id == id
}

catalog_for(scope) := entry if {
	some entry in data.policy.catalog
	entry.scope == scope
}

classified(entry, dataset) if {
	some row in entry.datasets
	row.dataset == dataset
}

country_allowed(label, country) if {
	label == "global"
	country != ""
}

country_allowed(label, country) if {
	label == "kr-only"
	country == "KR"
}

country_allowed(label, country) if {
	label == "apac"
	country in data.policy.apacCountries
}

deny contains {"code": "unknown_source", "movementId": m.id} if {
	some m in input.movements
	not environment_exists(m.source.environment)
}

deny contains {"code": "unknown_destination", "movementId": m.id} if {
	some m in input.movements
	not environment_exists(m.destination)
}

deny contains {"code": "unclassified_cluster", "movementId": m.id} if {
	some m in input.movements
	not catalog_for(m.source)
}

deny contains {"code": "unclassified_dataset", "movementId": m.id, "dataset": dataset} if {
	some m in input.movements
	entry := catalog_for(m.source)
	some dataset in m.inventory.datasets
	not classified(entry, dataset)
}

# A caller cannot omit a cataloged table and get a less restrictive result.
# The collector still has to discover newly-created, uncataloged relations.
deny contains {"code": "inventory_catalog_mismatch", "movementId": m.id, "dataset": row.dataset} if {
	some m in input.movements
	entry := catalog_for(m.source)
	some row in entry.datasets
	not row.dataset in m.inventory.datasets
}

# Buckets are optional in the configuration. A bucket's country is its
# declared environment's, and the declaration only counts once the gate's
# measurement (files.sourceObserved / targetObserved) confirms it.
buckets := object.get(data.policy, "buckets", [])

bucket_entry(name) := b if {
	some b in buckets
	b.bucket == name
}

# "remote:bucket/path" -> "bucket", the name a replication rule names.
bucket_name(spec) := split(split(spec, ":")[1], "/")[0]

env_country(id) := env.country if {
	some env in data.policy.environments
	env.id == id
}

deny contains {"code": "unregistered_bucket", "movementId": m.id, "bucket": name} if {
	some m in input.movements
	some name in [m.files.source, m.files.target]
	not bucket_entry(name)
}

# --- 1. Location: the declaration must match a measurement ----------------

observed contains {"id": m.id, "name": m.files.source, "obs": m.files.sourceObserved} if {
	some m in input.movements
	m.files
}

observed contains {"id": m.id, "name": m.files.target, "obs": m.files.targetObserved} if {
	some m in input.movements
	m.files
}

expected_locations(b) := object.get(b, ["expect", "locations"], [])

expected_cidrs(b) := object.get(b, ["expect", "endpointCIDRs"], [])

# A provider reports where it stores the bucket. A self-hosted store's region
# is its own setting, so only its network address can place it.
verifiable(b, obs) if {
	obs.hosting == "provider"
	count(expected_locations(b)) > 0
}

verifiable(b, obs) if {
	obs.hosting == "self"
	count(expected_cidrs(b)) > 0
}

deny contains {"code": "bucket_location_unverified", "movementId": x.id, "bucket": x.name} if {
	some x in observed
	b := bucket_entry(x.name)
	not verifiable(b, x.obs)
}

deny contains {"code": "bucket_location_mismatch", "movementId": x.id, "bucket": x.name} if {
	some x in observed
	b := bucket_entry(x.name)
	count(expected_locations(b)) > 0
	not x.obs.location in expected_locations(b)
}

# A dual- or multi-region bucket is in more than one country at once.
deny contains {"code": "bucket_multi_region", "movementId": x.id, "bucket": x.name} if {
	some x in observed
	bucket_entry(x.name)
	x.obs.hosting == "provider"
	x.obs.locationType != "region"
}

inside(ips, nets) if {
	count(ips) > 0
	every ip in ips {
		some n in nets
		net.cidr_contains(n, ip)
	}
}

deny contains {"code": "bucket_endpoint_outside_network", "movementId": x.id, "bucket": x.name} if {
	some x in observed
	b := bucket_entry(x.name)
	count(expected_cidrs(b)) > 0
	not inside(x.obs.endpointIPs, expected_cidrs(b))
}

# A self-hosted store's lifecycle can move objects to another store (MinIO
# ILM tiers can be cloud buckets anywhere), so the copy would not stay put.
deny contains {"code": "bucket_tiering_not_allowed", "movementId": x.id, "bucket": x.name} if {
	some x in observed
	bucket_entry(x.name)
	x.obs.tiered
}

# --- 2. Contents: classified prefixes, like a cluster's tables -------------

has_prefixes(b) if count(object.get(b, "prefixes", [])) > 0

prefix_classified(b, p) if {
	some e in b.prefixes
	e.prefix == p
}

deny contains {"code": "unclassified_prefix", "movementId": m.id, "bucket": m.files.source, "prefix": p} if {
	some m in input.movements
	src := bucket_entry(m.files.source)
	has_prefixes(src)
	some p in m.files.sourceObserved.prefixes
	p != ""
	not prefix_classified(src, p)
}

# As with tables, a cataloged prefix that is not found denies: the listing or
# the catalog is stale, and neither should decide silently.
deny contains {"code": "prefix_catalog_mismatch", "movementId": m.id, "bucket": m.files.source, "prefix": e.prefix} if {
	some m in input.movements
	src := bucket_entry(m.files.source)
	some e in object.get(src, "prefixes", [])
	not e.prefix in m.files.sourceObserved.prefixes
}

# What the copy carries: the bucket label for the whole bucket when it has no
# prefix catalog, or for the objects directly under its path when it has one,
# and each listed prefix's own label.
carried contains {"id": m.id, "source": m.files.source, "target": m.files.target, "label": src.label, "prefix": ""} if {
	some m in input.movements
	src := bucket_entry(m.files.source)
	not has_prefixes(src)
}

carried contains {"id": m.id, "source": m.files.source, "target": m.files.target, "label": src.label, "prefix": ""} if {
	some m in input.movements
	src := bucket_entry(m.files.source)
	has_prefixes(src)
	"" in m.files.sourceObserved.prefixes
}

carried contains {"id": m.id, "source": m.files.source, "target": m.files.target, "label": e.label, "prefix": e.prefix} if {
	some m in input.movements
	src := bucket_entry(m.files.source)
	some e in object.get(src, "prefixes", [])
	e.prefix in m.files.sourceObserved.prefixes
}

# File inspection can only add to carried labels. The same onward-replication
# rules below therefore apply to detected and operator-reviewed classifications.
carried contains {"id": m.id, "source": m.files.source, "target": m.files.target, "label": label, "prefix": ""} if {
	some m in input.movements
	bucket_entry(m.files.source)
	some label in object.get(m.files, "contentLabels", [])
}

target_allows(c) if {
	dst := bucket_entry(c.target)
	country_allowed(c.label, env_country(dst.environment))
}

deny contains {"code": "bucket_country_not_allowed", "movementId": c.id, "bucket": c.source} if {
	some c in carried
	c.prefix == ""
	bucket_entry(c.target)
	not target_allows(c)
}

deny contains {"code": "prefix_country_not_allowed", "movementId": c.id, "bucket": c.source, "prefix": c.prefix} if {
	some c in carried
	c.prefix != ""
	bucket_entry(c.target)
	not target_allows(c)
}

# --- 3. Onward copies: where the target replicates to ---------------------

target_replicas(id) := m.files.targetObserved.replicas if {
	some m in input.movements
	m.id == id
}

replica_registered(r) if {
	some b in buckets
	bucket_name(b.bucket) == r
}

deny contains {"code": "bucket_replication_unregistered", "movementId": m.id, "bucket": m.files.target, "replica": r} if {
	some m in input.movements
	bucket_entry(m.files.target)
	some r in m.files.targetObserved.replicas
	not replica_registered(r)
}

# Every registered bucket of that name must be allowed: a replication rule
# names a bucket, not a remote, so the strictest reading decides.
deny contains {"code": "bucket_replica_country_not_allowed", "movementId": c.id, "bucket": c.target, "replica": r} if {
	some c in carried
	some r in target_replicas(c.id)
	some b in buckets
	bucket_name(b.bucket) == r
	not country_allowed(c.label, env_country(b.environment))
}

deny contains {"code": "country_not_allowed", "movementId": m.id, "dataset": row.dataset} if {
	some m in input.movements
	entry := catalog_for(m.source)
	some row in entry.datasets
	some env in data.policy.environments
	env.id == m.destination
	not country_allowed(row.label, env.country)
}
