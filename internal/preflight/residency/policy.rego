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

deny contains {"code": "country_not_allowed", "movementId": m.id, "dataset": row.dataset} if {
	some m in input.movements
	entry := catalog_for(m.source)
	some row in entry.datasets
	some env in data.policy.environments
	env.id == m.destination
	not country_allowed(row.label, env.country)
}
