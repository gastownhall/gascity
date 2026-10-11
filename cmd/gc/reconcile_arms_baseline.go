package main

import (
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// decideRow's baseline arm (CONTRACT v5 M2, A8; plan C7c): after A6's row
// heals, before the liveness hold (A9) and the drift arm (A14).

// Baseline decide reasons.
const (
	decideBaselineVersion    = "baseline:version-artifact" // SESS-551/564/567
	decideBaselineLive       = "baseline:live-backfill"    // SESS-563
	decideBaselineSoftReload = "baseline:soft-reload"      // MAINT-025
)

// armBaseline is A8. A legacy or version-mismatched stored config hash is
// rebaselined, whatever the liveness, as legacy's silent rebaseline does (no
// drain, no restart); a missing or empty one never is (the next start commit
// baselines it), nor is a row the start arms start this pass (the start
// commits it). Once the core
// hash matches, an alive row's empty live hash with no session_live config
// is backfilled, and a legacy or mismatched one rebaselined, as legacy's
// alive drift block does. Under an open soft reload for this generation, a
// desired row's drifted config is accepted (legacy's
// acceptConfigDriftAcrossSessions), with a config-drift stop request still
// only requested canceled in the same CAS. The hashes are this
// generation's template for the row as written (the memo).
func armBaseline(r *rowFacts) (intent, bool) {
	info, e := r.row.Info, r.entry
	stored := strings.TrimSpace(info.StartedConfigHash)
	if e == nil {
		return intent{}, false
	}
	cfg, core, ok := r.w.coreHash(info)
	if !ok {
		return intent{}, false
	}
	propose := func(reason string, patch session.MetadataPatch) (intent, bool) { // a row the start arms start this pass is its commit's
		if r.startsThisPass() {
			return intent{}, false
		}
		return r.heal(intentBaseline, reason, patch)
	}
	switch storedLive, live := info.StartedLiveHash, runtime.LiveFingerprint(cfg); {
	case stored != core && runtime.IsLegacyOrMismatchedVersion(stored): // the rebaseline, below
	case r.w.owesSoftReload(r.k, e, stored, core):
		patch := baselinePatch(cfg, baselineSoftReload)
		if req, ok := activeStop(r.row); ok && req.Reason == drainConfigDrift && req.Phase == stopRequested {
			maps.Copy(patch, stopCancelPatch())
		}
		return propose(r.w.SoftReload.Reason, patch)
	case stored != core || e.Liveness != livenessAlive || storedLive == live:
		return intent{}, false
	case storedLive == "" && len(cfg.SessionLive) == 0:
		return propose(decideBaselineLive, baselinePatch(cfg, baselineLiveBackfill))
	case !runtime.IsLegacyOrMismatchedVersion(storedLive):
		return intent{}, false // live drift: A14's RunLive (C7b)
	} // otherwise a legacy or mismatched live hash: the rebaseline
	return propose(decideBaselineVersion, baselinePatch(cfg, baselineVersionArtifact))
}

// coreHash is the row's hash config and core fingerprint under this
// generation's template for it as written (the memo); false when it does
// not resolve.
func (w *World) coreHash(info session.Info) (runtime.Config, string, bool) {
	res, ok := w.Templates.lookup(info)
	if !ok || res.Err != nil {
		return runtime.Config{}, "", false
	}
	cfg := sessionCoreConfigForHashInfo(res.TP, info)
	return cfg, runtime.CoreFingerprint(cfg), true
}

// owesSoftReload reports a desired row whose drifted stored hash the open
// soft reload has yet to accept; a missing one is never drift.
func (w *World) owesSoftReload(k rowKey, e *selectionEntry, stored, core string) bool {
	return stored != "" && stored != core && e.InDesired && w.Env != nil && w.SoftReload.accepts(k, w.Env.Gen)
}

// owesSoftReload is World's, but for a row the start arms start this pass,
// which A8 leaves to its start's commit: the rule A8 decides by.
func (r *rowFacts) owesSoftReload(stored, core string) bool {
	return r.w.owesSoftReload(r.k, r.entry, stored, core) && !r.startsThisPass()
}

// startsThisPass reports a start the start arms (A17, A18) propose for the
// row this pass.
func (r *rowFacts) startsThisPass() bool {
	it, ok := r.restart()
	return ok && it.Kind == intentStart
}
