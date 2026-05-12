package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// acceptConfigDriftAcrossSessions writes the current per-session config
// hash into every open session bead's started_config_hash metadata
// whose desired-state entry produces a different hash than the one
// recorded on the bead. After the function returns, the reconciler's
// drift-detection check (storedHash != currentHash) sees no drift for
// any updated session, so the immediately-following reconcile tick
// proceeds without firing config-drift drains for those sessions.
//
// Used by `gc reload --soft` so an operator editing a running city's
// .gc/settings.json doesn't drain every drifted session — the new
// config is accepted in-place instead. The caller is expected to have
// just rebuilt desired state from the freshly reloaded config.
//
// Sessions are skipped (no metadata write) when:
//   - the session is closed
//   - the session has no session_name metadata
//   - the session has no started_config_hash yet (the existing drift
//     check already skips these — the next first-start path will
//     stamp the right value)
//   - the session's name has no entry in desired (orphaned by the
//     config edit; normal orphan/suspended drain handles them on the
//     next tick)
//   - the recomputed current hash already equals the stored hash
//
// The hash computation mirrors the canonical reconciler path:
// templateParamsToConfig → applyTemplateOverridesToConfig →
// runtime.CoreFingerprint. Any future change to that sequence in the
// reconciler must be mirrored here, otherwise --soft will write a
// stale hash and the next tick will still detect drift.
//
// Returns the number of session beads whose started_config_hash was
// updated.
func acceptConfigDriftAcrossSessions(
	store beads.Store,
	desired map[string]TemplateParams,
	stderr io.Writer,
) int {
	if store == nil || len(desired) == 0 {
		return 0
	}
	sessionBeads, err := loadSessionBeadSnapshot(store)
	if err != nil {
		fmt.Fprintf(stderr, "soft reload: listing session beads: %v\n", err) //nolint:errcheck // best-effort stderr
		return 0
	}
	open := sessionBeads.Open()
	if len(open) == 0 {
		return 0
	}

	updated := 0
	for i := range open {
		session := open[i]
		if session.Status == "closed" {
			continue
		}
		name := strings.TrimSpace(session.Metadata["session_name"])
		if name == "" {
			continue
		}
		storedHash := strings.TrimSpace(session.Metadata["started_config_hash"])
		if storedHash == "" {
			continue
		}
		tp, ok := desired[name]
		if !ok {
			continue
		}
		agentCfg := templateParamsToConfig(tp)
		applyTemplateOverridesToConfig(&agentCfg, session, tp)
		currentHash := runtime.CoreFingerprint(agentCfg)
		if storedHash == currentHash {
			continue
		}
		if err := store.SetMetadata(session.ID, "started_config_hash", currentHash); err != nil {
			fmt.Fprintf(stderr, "soft reload: updating started_config_hash for %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
			continue
		}
		updated++
	}
	return updated
}
