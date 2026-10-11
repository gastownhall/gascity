package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// decideRow's baseline arm (CONTRACT v5 M2, A8; plan C7c): after A6's row
// heals, before the liveness hold (A9) and the drift arm (A14).

// Baseline decide reasons.
const (
	decideBaselineVersion = "baseline:version-artifact" // SESS-551/564/567
	decideBaselineLive    = "baseline:live-backfill"    // SESS-563
)

// armBaseline is A8. A legacy or version-mismatched stored config hash is
// rebaselined, whatever the liveness, as legacy's silent rebaseline does (no
// drain, no restart); a missing or empty one never is (the next start commit
// baselines it), nor is a row its start will commit this pass. Once the core
// hash matches, an alive row's empty live hash with no session_live config
// is backfilled, and a legacy or mismatched one rebaselined, as legacy's
// alive drift block does. The hashes are this generation's template for
// the row as written (the memo).
func armBaseline(r *rowFacts) (intent, bool) {
	info, e := r.row.Info, r.entry
	stored := strings.TrimSpace(info.StartedConfigHash)
	if e == nil || (e.Desired == desireWake && e.Liveness.startCandidate()) {
		return intent{}, false
	}
	res, ok := r.w.Templates.lookup(info)
	if !ok || res.Err != nil {
		return intent{}, false
	}
	cfg := sessionCoreConfigForHashInfo(res.TP, info)
	core := runtime.CoreFingerprint(cfg)
	switch storedLive, live := info.StartedLiveHash, runtime.LiveFingerprint(cfg); {
	case stored != core && runtime.IsLegacyOrMismatchedVersion(stored): // the rebaseline, below
	case stored != core || e.Liveness != livenessAlive || storedLive == live:
		return intent{}, false
	case storedLive == "" && len(cfg.SessionLive) == 0:
		return r.heal(intentBaseline, decideBaselineLive, baselinePatch(cfg, baselineLiveBackfill))
	case !runtime.IsLegacyOrMismatchedVersion(storedLive):
		return intent{}, false // live drift: A14's RunLive (C7b)
	} // otherwise a legacy or mismatched live hash: the rebaseline
	return r.heal(intentBaseline, decideBaselineVersion, baselinePatch(cfg, baselineVersionArtifact))
}
