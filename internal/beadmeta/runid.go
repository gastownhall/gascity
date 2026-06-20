package beadmeta

import "strings"

// runIDChainKeys is the bead-metadata run-chain precedence: a graph workflow
// root (workflow_id), then a poured/wisp molecule root (molecule_id), then the
// nested-workflow root (gc.root_bead_id). workflow_id and molecule_id are the
// engine's bare (non-"gc."-namespaced) run-chain keys written by internal/sling;
// RootBeadIDMetadataKey is the gc.-namespaced nesting root.
var runIDChainKeys = []string{"workflow_id", "molecule_id", RootBeadIDMetadataKey}

// ResolveRunID derives the run-root identifier for a bead from its metadata run
// chain, falling back to the bead's own id and then a caller-supplied fallback.
// Precedence: workflow_id || molecule_id || gc.root_bead_id || selfID ||
// fallbackID, skipping blank values at each step.
//
// Every usage-fact emitter MUST resolve a run id through this one helper so a
// run's model facts (worker prompt path) and compute facts (controller reconcile
// path) carry the same RunID and `gc costs` can group them; two independent
// copies could silently drift and split one run's rows (see
// engdocs/design/usage-facts-v0.md). The worker passes the session id as
// fallbackID so a manual chat with no work bead still resolves to its session
// bead as the run root; the compute path passes "" because the session bead is
// always present.
func ResolveRunID(metadata map[string]string, selfID, fallbackID string) string {
	for _, k := range runIDChainKeys {
		if v := strings.TrimSpace(metadata[k]); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(selfID); v != "" {
		return v
	}
	return strings.TrimSpace(fallbackID)
}
