package main

import (
	"encoding/json"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The baseline field group (CONTRACT v5 M2, I21): v2 writes the six keys
// started_config_hash, started_provision_hash, started_launch_hash,
// core_hash_breakdown, started_live_hash and live_hash only through
// baselinePatch, and through the legacy builders the reused paths emit
// (CommitStartedPatch, ConversationResetPatch, RestartRequestPatch,
// PreWakePatch's fresh wake), and the create and reopen effects' starting
// state. The session field registry declares each writer, and
// TestSessionFieldsFlowsWriteTheirKeys runs them.

// baselineReason is why a baseline moves; it picks the keys written.
type baselineReason string

const (
	// baselineVersionArtifact rebaselines a legacy or version-mismatched
	// stored hash (SESS-551/564/567): the config did not change, so every
	// baseline, the live half included, moves to this binary's hashes
	// (legacy's sessionHashRebaselineMetadata).
	baselineVersionArtifact baselineReason = "version-artifact"
	// baselinePrePolicy rebaselines a v6 core hash of unchanged config
	// (runtime.IsCoreHashArtifact): the core baseline, not the live pair,
	// which the live check compares on its own, so a changed session_live is
	// still applied.
	baselinePrePolicy baselineReason = "pre-policy"
	// baselineLiveBackfill stamps an empty live hash with no session_live
	// config (SESS-563).
	baselineLiveBackfill baselineReason = "live-backfill"
	// baselineSoftReload accepts a drifted row's config under a soft reload
	// (MAINT-025): the core baseline, not the live pair, as legacy's
	// softReloadAcceptedHashMetadata.
	baselineSoftReload baselineReason = "soft-reload"
	// baselineAliasRerender rebaselines a started row whose alias A7
	// changes (SESS-717), all six keys, as legacy's
	// queueAliasChangeDriftRebaseline: the alias rerenders GC_ALIAS.
	baselineAliasRerender baselineReason = "alias-rerender"
	// baselineRelaunch moves a relaunched row's core baseline (SESS-556/560)
	// to the config it runs, not the live pair, which a relaunch does not
	// reliably re-apply: legacy's rebaselineLaunchDriftHashesWithBatch.
	baselineRelaunch baselineReason = "relaunch"
)

// baselinePatch is cfg's baseline for reason.
func baselinePatch(cfg runtime.Config, reason baselineReason) session.MetadataPatch {
	patch := session.MetadataPatch{}
	if reason != baselineSoftReload && reason != baselineRelaunch && reason != baselinePrePolicy {
		live := runtime.LiveFingerprint(cfg)
		patch["started_live_hash"], patch["live_hash"] = live, live
	}
	if reason == baselineLiveBackfill {
		return patch
	}
	breakdown, _ := json.Marshal(runtime.CoreFingerprintBreakdown(cfg)) // strings and bools: it cannot fail
	patch["started_config_hash"] = runtime.CoreFingerprint(cfg)
	patch["started_provision_hash"] = runtime.ProvisionFingerprint(cfg)
	patch["started_launch_hash"] = runtime.LaunchFingerprint(cfg)
	patch["core_hash_breakdown"] = string(breakdown)
	return patch
}
