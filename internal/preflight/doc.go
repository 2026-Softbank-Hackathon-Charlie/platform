// Package preflight defines inputs and verdicts for migration safety checks.
//
// It is not wired into approval or execution. A residency verdict is NOT an
// execution permit or proof of parity, rehearsal, inventory freshness or human
// approval. Production callers must obtain inventories from trusted collectors,
// never directly from a submitted plan or an AI-generated report.
package preflight
