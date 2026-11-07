package runs

import "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

// allowSourceRewire is the engine plan's allowSourceRewire. The engine this
// module pins for the external source of ADR 0042 (migration-engine
// feat/stateless-migration 0d6a91a) branched before the field (main
// f520e9f), and its plan.Parse refuses the key, so no parsed plan has it.
// Read the field again once the engine pin has both.
func allowSourceRewire(*plan.Plan) bool { return false }
