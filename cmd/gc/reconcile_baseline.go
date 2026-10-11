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
// (CommitStartedPatch, ConversationResetPatch, RestartRequestPatch), and
// cleared only through clearBaseline.
// TestBaselineKeysOnlyThroughTheBuilder lints it.

// baselineReason is why a baseline moves; it picks the keys written.
type baselineReason string

const (
	// baselineVersionArtifact rebaselines a legacy or version-mismatched
	// stored hash (SESS-551/564/567): the config did not change, so every
	// baseline, the live half included, moves to this binary's hashes
	// (legacy's sessionHashRebaselineMetadata).
	baselineVersionArtifact baselineReason = "version-artifact"
	// baselineLiveBackfill stamps an empty live hash with no session_live
	// config (SESS-563).
	baselineLiveBackfill baselineReason = "live-backfill"
)

// baselinePatch is cfg's baseline for reason.
func baselinePatch(cfg runtime.Config, reason baselineReason) session.MetadataPatch {
	live := runtime.LiveFingerprint(cfg)
	patch := session.MetadataPatch{"started_live_hash": live, "live_hash": live}
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

// clearBaseline is the stored config hash cleared with a continuation reset,
// with the priming markers its clear always takes (S19's lifetime rule): a
// missing hash never drifts, and the next start commit baselines it.
func clearBaseline() session.MetadataPatch {
	return session.MetadataPatch{"started_config_hash": "", session.PrimedAtMetadataKey: "", session.PrimingAttemptedAtMetadataKey: "", session.PromptHashMetadataKey: ""}
}
