// Package preflight defines inputs and verdicts for migration safety checks.
//
// The residency verdict gates the start of a new migration run when the server
// has a residency policy (gate package, plans.Service.SetResidencyGate). A
// residency verdict is NOT an execution permit or proof of parity, rehearsal,
// inventory freshness or human approval. Inventories come from the server's own
// collector (inventory package), never directly from a submitted plan or an
// AI-generated report. Parity is recorded on start (paritygate, audit mode)
// and never stops a run.
package preflight
