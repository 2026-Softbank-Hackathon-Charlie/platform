// Package enginecompat is the one place that knows whether the pinned
// migration engine has the external source of ADR 0042 (engine ADR 0017,
// feat/external-source). The engine main this module pins now does not:
// its plan has no source.kind, name or target.reindex (plan.Parse refuses
// them) and its Options no External. Everything of the platform that
// needs them goes through here, so moving the pin to an engine with them
// changes this file only:
//
//	const External = true
//	type ExternalSource = engine.ExternalSource
//	func SourceExternal(p *plan.Plan) bool { return p.Source.External() }
//	func SourceName(p *plan.Plan) string   { return p.Source.Name }
//	func SetExternal(o *engine.Options, x ExternalSource) { o.External = x }
package enginecompat

import (
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// External says the pinned engine has the external source.
const External = false

// ExternalSource is engine.ExternalSource once the engine has it.
type ExternalSource = any

// SourceExternal reports source.kind external.
func SourceExternal(*plan.Plan) bool { return false }

// SourceName is source.name of an external source.
func SourceName(*plan.Plan) string { return "" }

// SetExternal gives the engine the external source's adapter.
func SetExternal(*engine.Options, ExternalSource) {}

// TestPlan is a plan with an external source named name before target, for
// tests; nil while the engine has no external source. With it:
//
//	return &plan.Plan{ID: id, Source: plan.Endpoint{Kind: plan.SourceExternal, Name: name, Env: name}, Target: target}
func TestPlan(id, name string, target plan.Endpoint) *plan.Plan { return nil }

// TestSource is an ExternalSource that does nothing, for tests.
func TestSource() ExternalSource { return struct{}{} }
