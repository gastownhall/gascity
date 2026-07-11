package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestSessionClassifierInfoEquivalence is the byte-identical oracle for P2 of
// NONWORK-BEAD-FIELDDOOR-PLAN.md. Each converted classifier has a *Info sibling
// that reads typed session.Info fields instead of raw bead metadata. For every
// representative session-bead shape, the Info form (seeded through the session
// store front door) must agree with the original bead form.
//
// This proves the Info projection plus the predicate mirror are semantically
// identical to the existing metadata reads, so later caller migration (P4) is
// safe. Any divergence here is a real fidelity bug in the codec or a mirror.
func TestSessionClassifierInfoEquivalence(t *testing.T) {
	pastRFC3339 := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	futureRFC3339 := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	// recentWokeRFC3339 is strictly AFTER the reap-boundary fixture's CreatedAt
	// (-30m), so staleReapStartBoundary must advance the boundary to this woke
	// time — the last_woke_at-upgrade branch. recentWoke is the parsed value the
	// direct true-branch assertion compares against.
	recentWokeRFC3339 := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	recentWoke, err := time.Parse(time.RFC3339, recentWokeRFC3339)
	if err != nil {
		t.Fatalf("parsing recentWokeRFC3339: %v", err)
	}
	clk := &clock.Fake{Time: time.Now()}

	beadsByShape := map[string]beads.Bead{
		"bare": {
			ID:     "ga-bare",
			Type:   session.BeadType,
			Title:  "bare",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "worker",
			},
		},
		// drain-ack stop-pending: state=draining + state_reason=drain-ack-stop-pending
		// exercises the true branch of isDrainAckStopPending / isDrainAckStopPendingInfo.
		"drain-ack-stop-pending": {
			ID:     "ga-drainack",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        string(session.StateDraining),
				"state_reason": session.DrainAckStopPendingReason,
				"session_name": "worker-drainack",
			},
		},
		"pool-managed-slot": {
			ID:     "ga-pool",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "frontend/worker",
				"agent_name":   "frontend/worker-1",
				"pool_managed": "true",
				"pool_slot":    "1",
				"state":        "awake",
				"session_name": "worker-ga-pool",
			},
		},
		"pool-managed-flag-only": {
			ID:     "ga-poolflag",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"pool_managed": "true",
				"state":        "active",
			},
		},
		"ephemeral-origin": {
			ID:     "ga-eph",
			Type:   session.BeadType,
			Title:  "eph",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":       "worker",
				"session_origin": "ephemeral",
			},
		},
		"ephemeral-via-pool-slot-name": {
			ID:     "ga-ephname",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"session_name": "worker-3",
			},
		},
		"named": {
			ID:     "ga-named",
			Type:   session.BeadType,
			Title:  "mayor",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "mayor",
				"configured_named_session":  "true",
				"configured_named_identity": "mayor",
				"configured_named_mode":     "singleton",
				"common_name":               "mayor",
				"alias":                     "mayor",
				"session_name":              "mayor",
				"session_name_explicit":     "true",
				"alias_history":             "mayor,boss",
			},
		},
		"manual": {
			ID:     "ga-manual",
			Type:   session.BeadType,
			Title:  "manual",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":       "worker",
				"manual_session": "true",
			},
		},
		"manual-padded-true": {
			// Edge: isManualSessionBead compares manual_session WITHOUT trimming,
			// so a padded "true" must read as NOT manual on both forms.
			ID:     "ga-manualpad",
			Type:   session.BeadType,
			Title:  "manual",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":       "worker",
				"manual_session": "  true  ",
			},
		},
		"manual-origin": {
			ID:     "ga-manualorigin",
			Type:   session.BeadType,
			Title:  "manual",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":       "worker",
				"session_origin": "manual",
			},
		},
		"drained-state": {
			ID:     "ga-drained",
			Type:   session.BeadType,
			Title:  "drained",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":  "worker",
				"state":     "drained",
				"pool_slot": "2",
			},
		},
		"drained-via-asleep": {
			ID:     "ga-drainasleep",
			Type:   session.BeadType,
			Title:  "drained",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        "asleep",
				"sleep_reason": "drained",
				"pool_slot":    "2",
			},
		},
		"asleep-idle-freeable": {
			ID:     "ga-idle",
			Type:   session.BeadType,
			Title:  "idle",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        "asleep",
				"sleep_reason": "idle",
				"pool_slot":    "2",
			},
		},
		"asleep-wait-hold-not-freeable": {
			ID:     "ga-wait",
			Type:   session.BeadType,
			Title:  "wait",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        "asleep",
				"sleep_reason": "wait-hold",
				"pool_slot":    "2",
			},
		},
		"failed-create": {
			ID:     "ga-failed",
			Type:   session.BeadType,
			Title:  "failed",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        string(session.StateFailedCreate),
				"pool_managed": "true",
			},
		},
		"pending-pool-create": {
			ID:     "ga-pending",
			Type:   session.BeadType,
			Title:  "pending",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":             "worker",
				"pool_managed":         "true",
				"pool_slot":            "1",
				"state":                string(session.StateStartPending),
				"pending_create_claim": "true",
			},
		},
		"pending-create-claim-old-markers": {
			// Exercises the lease family's non-empty last_woke_at + past
			// pending_create_started_at branches (attempt-stale / lease-active)
			// with the fidelity fields LastWokeAt / PendingCreateStartedAt.
			ID:        "ga-leasestale",
			Type:      session.BeadType,
			Title:     "leasestale",
			Labels:    []string{session.LabelSession},
			CreatedAt: time.Now().Add(-90 * time.Minute),
			Metadata: map[string]string{
				"template":                  "worker",
				"state":                     string(session.StateStartPending),
				"pending_create_claim":      "true",
				"pending_create_started_at": pastRFC3339,
				"last_woke_at":              pastRFC3339,
			},
		},
		"pending-create-inflight-lease": {
			// DECIDES pendingCreateLeaseActiveInfo's in-flight true-branch:
			// pending_create_claim=true with a RECENT last_woke_at (within the
			// startup lease window, so pendingCreateStartInFlightInfo fires and the
			// lease is active) BUT a pending_create_started_at aged past
			// staleCreatingStateTimeout (so pendingCreateAttemptStaleInfo is TRUE —
			// the non-in-flight tail would return false). Without this fixture,
			// mutating the in-flight `return true` to fall through survives the
			// equivalence sweep. leaseStartupTimeout is 90s, staleKeyDetectDelay 2s,
			// staleCreatingStateTimeout 1m; last_woke_at -30s stays in-flight, and
			// pending_create_started_at -5m is attempt-stale.
			ID:     "ga-inflightlease",
			Type:   session.BeadType,
			Title:  "inflightlease",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "worker",
				"state":                     string(session.StateCreating),
				"pending_create_claim":      "true",
				"last_woke_at":              clk.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339),
				"pending_create_started_at": clk.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339),
			},
		},
		"reap-boundary-recent-wake": {
			// Exercises staleReapStartBoundary's last_woke_at-upgrade branch: a
			// non-zero CreatedAt (-30m) with a parseable last_woke_at strictly AFTER
			// it (recentWokeRFC3339, -5m), so the boundary must advance to the woke
			// time (not CreatedAt) in BOTH the raw and Info forms. Without a fixture
			// on this path, dropping the woke-upgrade in either form would go
			// unnoticed and silently reap recently-woken creating sessions.
			ID:        "ga-reapwoke",
			Type:      session.BeadType,
			Title:     "reapwoke",
			Labels:    []string{session.LabelSession},
			CreatedAt: time.Now().Add(-30 * time.Minute),
			Metadata: map[string]string{
				"template":     "worker",
				"state":        string(session.StateCreating),
				"last_woke_at": recentWokeRFC3339,
			},
		},
		"post-create-protected": {
			// Exercises the StateReason / CreationCompleteAt fidelity fields via
			// the sweep's post-create protection window (state_reason=creation_complete).
			ID:     "ga-postcreate",
			Type:   session.BeadType,
			Title:  "postcreate",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":             "worker",
				"state":                "active",
				"state_reason":         "creation_complete",
				"creation_complete_at": futureRFC3339,
				"pool_managed":         "true",
				"pool_slot":            "1",
			},
		},
		"stale-creating-old-marker": {
			ID:        "ga-stale",
			Type:      session.BeadType,
			Title:     "stale",
			Labels:    []string{session.LabelSession},
			CreatedAt: time.Now().Add(-90 * time.Minute),
			Metadata: map[string]string{
				"template":                  "worker",
				"state":                     string(session.StateStartPending),
				"pending_create_started_at": pastRFC3339,
			},
		},
		"fresh-creating": {
			ID:        "ga-fresh",
			Type:      session.BeadType,
			Title:     "fresh",
			Labels:    []string{session.LabelSession},
			CreatedAt: time.Now(),
			Metadata: map[string]string{
				"template":                  "worker",
				"state":                     string(session.StateStartPending),
				"pending_create_started_at": futureRFC3339,
			},
		},
		"quarantined-active": {
			ID:     "ga-quar",
			Type:   session.BeadType,
			Title:  "quar",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":          "worker",
				"state":             "quarantined",
				"quarantined_until": futureRFC3339,
				"wake_attempts":     "3",
			},
		},
		"quarantine-expired": {
			ID:     "ga-quarexp",
			Type:   session.BeadType,
			Title:  "quar",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":          "worker",
				"quarantined_until": pastRFC3339,
				"wake_attempts":     "1",
			},
		},
		"agent-label-fallback": {
			ID:     "ga-label",
			Type:   session.BeadType,
			Title:  "labeled",
			Labels: []string{session.LabelSession, "agent:scout"},
			Metadata: map[string]string{
				"template": "scout",
			},
		},
		"dependency-only": {
			ID:     "ga-dep",
			Type:   session.BeadType,
			Title:  "dep",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":        "worker",
				"dependency_only": "true",
			},
		},
		"unknown-state": {
			ID:     "ga-unknown",
			Type:   session.BeadType,
			Title:  "unknown",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "worker",
				"state":    "some-future-state",
			},
		},
		"closed": {
			ID:     "ga-closed",
			Type:   session.BeadType,
			Title:  "closed",
			Status: "closed",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        string(session.StateFailedCreate),
				"pool_managed": "true",
				"pool_slot":    "1",
			},
		},
		"no-session-name-pool": {
			// Exercises the SessionNameMetadata-vs-SessionName divergence: the
			// raw session_name is empty, so beadOwnsPoolSessionName /
			// sessionBeadAssigneeIdentities must NOT see the sessionNameFor(ID)
			// fallback that Info.SessionName applies.
			ID:     "ga-noname",
			Type:   session.BeadType,
			Title:  "noname",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":  "worker",
				"pool_slot": "1",
			},
		},
		"owns-pool-session-name": {
			ID:     "ga-owns",
			Type:   session.BeadType,
			Title:  "owns",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"session_name": PoolSessionName("worker", "ga-owns"),
				"pool_slot":    "1",
			},
		},
		"acp-transport": {
			ID:     "ga-acptransport",
			Type:   session.BeadType,
			Title:  "acp",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":  "worker",
				"transport": "acp",
			},
		},
		"acp-provider": {
			ID:     "ga-acpprovider",
			Type:   session.BeadType,
			Title:  "acp",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "worker",
				"provider": "acp",
			},
		},
		"acp-mcp-identity": {
			ID:     "ga-acpmcpid",
			Type:   session.BeadType,
			Title:  "acp",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                     "worker",
				session.MCPIdentityMetadataKey: "mayor",
			},
		},
		"acp-mcp-snapshot": {
			ID:     "ga-acpmcpsnap",
			Type:   session.BeadType,
			Title:  "acp",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                            "worker",
				session.MCPServersSnapshotMetadataKey: "{}",
			},
		},
		"non-acp-transport": {
			ID:     "ga-nonacp",
			Type:   session.BeadType,
			Title:  "tmux",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":  "worker",
				"transport": "tmux",
			},
		},
		"provider-terminal-error": {
			ID:     "ga-provterm",
			Type:   session.BeadType,
			Title:  "term",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                "worker",
				"provider_terminal_error": "boom",
			},
		},
		"unhealthy-drainable-reasoned": {
			ID:     "ga-unhealthy",
			Type:   session.BeadType,
			Title:  "unhealthy",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":              "worker",
				"session_health":        "unhealthy",
				"session_drainable":     "true",
				"session_health_reason": "stuck",
			},
		},
		"unhealthy-not-drainable": {
			ID:     "ga-unhealthynd",
			Type:   session.BeadType,
			Title:  "unhealthy",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":              "worker",
				"session_health":        "unhealthy",
				"session_health_reason": "stuck",
			},
		},
		"creating-consumes-demand": {
			ID:     "ga-creating",
			Type:   session.BeadType,
			Title:  "creating",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "worker",
				"state":    "creating",
			},
		},
		"trigger-brain-marked": {
			ID:     "ga-trigger",
			Type:   session.BeadType,
			Title:  "trigger",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                              "worker",
				"pool_managed":                          "true",
				"pool_slot":                             "1",
				"state":                                 "creating",
				beadmeta.TriggerBeadIDMetadataKey:       "tb-1",
				beadmeta.TriggerBeadStoreRefMetadataKey: "riga",
				beadmeta.BrainParentSIDMetadataKey:      "brain-1",
			},
		},
		"reset-pending-committed": {
			// continuation_reset_pending=true + a valid reset_committed_at:
			// resetPendingCommittedAt returns the raw ts + parsed time + true.
			ID:     "ga-resetpending",
			Type:   session.BeadType,
			Title:  "resetpending",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                   "worker",
				"continuation_reset_pending": "true",
				session.ResetCommittedAtKey:  pastRFC3339,
			},
		},
		"reset-pending-no-committed": {
			// pending but no reset_committed_at → not pending (empty-raw branch).
			ID:     "ga-resetnocommit",
			Type:   session.BeadType,
			Title:  "resetnocommit",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                   "worker",
				"continuation_reset_pending": "true",
			},
		},
		"reset-pending-invalid-committed": {
			// pending but reset_committed_at is not RFC3339 → parse-error branch.
			ID:     "ga-resetbad",
			Type:   session.BeadType,
			Title:  "resetbad",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                   "worker",
				"continuation_reset_pending": "true",
				session.ResetCommittedAtKey:  "not-a-timestamp",
			},
		},
		"reset-not-pending": {
			// reset_committed_at set but pending!=true → short-circuit false.
			ID:     "ga-resetnotpending",
			Type:   session.BeadType,
			Title:  "resetnotpending",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "worker",
				session.ResetCommittedAtKey: pastRFC3339,
			},
		},
		"generation-padded": {
			// generation is read BOTH as strconv.Atoi (numeric drain-staleness
			// compare) and strings.TrimSpace (string ack compare). The
			// whitespace-padded value proves Info.Generation preserves the raw
			// bytes the TrimSpace path depends on — an int mirror could not.
			ID:     "ga-gen",
			Type:   session.BeadType,
			Title:  "gen",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":   "worker",
				"generation": " 3 ",
			},
		},
		"pending-resume-preserve": {
			// Hits the pendingResumePreservingNamedRestartInfo TRUE branch: creating
			// state + pending_create_claim + session_key + started_config_hash +
			// a recent pending_create_started_at (so the lease is start-in-flight,
			// not expired). Makes the clkBoolChecks equivalence case a real
			// true-branch comparison, not a trivial both-false pass, and exercises
			// the new Info.StartedConfigHash gate.
			ID:     "ga-resumepreserve",
			Type:   session.BeadType,
			Title:  "resumepreserve",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "worker",
				"state":                     "creating",
				"pending_create_claim":      "true",
				"session_key":               "sess-key-123",
				"started_config_hash":       "cfghash-abc",
				"pending_create_started_at": clk.Now().UTC().Format(time.RFC3339),
			},
		},
		"config-hash-and-pin": {
			// started_config_hash is read BOTH as a direct string compare (stored
			// hash vs recomputed Core fingerprint) and via strings.TrimSpace (the
			// firstStart emptiness gate). The whitespace-padded value proves
			// Info.StartedConfigHash preserves the raw bytes the TrimSpace path
			// depends on. pin_awake is read as an exact != "true" compare.
			ID:     "ga-cfghash",
			Type:   session.BeadType,
			Title:  "cfghash",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":            "worker",
				"started_config_hash": " abc123 ",
				"pin_awake":           "true",
			},
		},
		// --- reconciler decision-read cluster fixtures (front-door Phase 5) ---
		"hold-and-quarantine": {
			// held_until suppresses all wake reasons; the reconcile path also reads
			// quarantined_until alongside it. Parity fixture for the hold/quarantine
			// suppression branch.
			ID:     "ga-hold",
			Type:   session.BeadType,
			Title:  "hold",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":          "worker",
				"held_until":        futureRFC3339,
				"quarantined_until": futureRFC3339,
				"wake_attempts":     "2",
			},
		},
		"wait-hold-flag": {
			// wait_hold=="true" is the raw metadata compute_awake_bridge maps onto
			// LifecycleInput.WaitHold; distinct from sleep_reason=="wait-hold".
			ID:     "ga-waithold",
			Type:   session.BeadType,
			Title:  "waithold",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        "asleep",
				"sleep_reason": "wait-hold",
				"wait_hold":    "true",
			},
		},
		"churn-spiraling": {
			// churn_count read via strconv.Atoi (which does NOT trim): the padded
			// value proves Info.ChurnCount preserves the raw bytes verbatim.
			ID:     "ga-churn",
			Type:   session.BeadType,
			Title:  "churn",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":    "worker",
				"churn_count": " 5 ",
			},
		},
		"churn-cleared-zero": {
			// Exercises the churn_count == "0" clear branch explicitly.
			ID:     "ga-churnzero",
			Type:   session.BeadType,
			Title:  "churnzero",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":    "worker",
				"churn_count": "0",
			},
		},
		"wake-mode-and-intents": {
			// wake_mode=="fresh" (fresh-wake / drain finalize), sleep_intent branch,
			// instance_token wake match, detached_at detach gate (RFC3339), and
			// currently_processing_bead_id (LifecycleInput) in one shape.
			ID:     "ga-wakemode",
			Type:   session.BeadType,
			Title:  "wakemode",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":               "worker",
				"wake_mode":              "fresh",
				"wake_request":           "explicit",
				"restart_requested":      "true",
				"sleep_intent":           "idle-stop-pending",
				"instance_token":         "tok-xyz",
				"detached_at":            pastRFC3339,
				session.CurrentBeadIDKey: "ga-work-1",
				// Step 6a codec-gap mirrors. wake_attempts="0" is the raw/int edge:
				// WakeAttemptsMetadata must keep "0" verbatim while WakeAttempts parses 0
				// (the distinction clearWakeFailures's != "" && != "0" gate needs).
				"session_id_flag":    "--session-id",
				"template_overrides": `{"model":"opus"}`,
				"wake_attempts":      "0",
			},
		},
		"config-drift-full": {
			// The config-drift sub-hash decision keys (core_hash_breakdown JSON,
			// provision/launch/live fingerprints — launch padded to prove raw
			// fidelity) plus the named + attached deferral timers and the stranded
			// idempotency marker, all in one shape.
			ID:     "ga-drift",
			Type:   session.BeadType,
			Title:  "drift",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                           "worker",
				"core_hash_breakdown":                `{"command":"x","env":"y"}`,
				"started_provision_hash":             "prov-1",
				"started_launch_hash":                " launch-1 ",
				"started_live_hash":                  "live-1",
				"config_drift_deferred_at":           pastRFC3339,
				"config_drift_deferred_key":          "h1:h2",
				"attached_config_drift_deferred_at":  pastRFC3339,
				"attached_config_drift_deferred_key": "h3:h4",
				"stranded_event_emitted_at":          pastRFC3339,
			},
		},
		"rapid-crash-candidate": {
			// Dead crash candidate: awake with a recent last_woke_at (well within
			// stabilityThreshold), no deliberate sleep_reason and no pending-create
			// claim, so DecideSessionExit on the exit facts (alive=false) classifies
			// it ExitRapidCrash. Retained as a representative shape for the Info-form
			// classifiers now that the raw sessionExitFacts equivalence block is gone.
			ID:     "ga-rapidcrash",
			Type:   session.BeadType,
			Title:  "rapidcrash",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"state":        "awake",
				"last_woke_at": clk.Now().Add(-15 * time.Second).UTC().Format(time.RFC3339),
			},
		},
		"wake-attempts-overflow": {
			// wake_attempts beyond int64 range: strconv.Atoi returns the clamped
			// value together with ErrRange. Pins the recordWakeFailure counter lane,
			// which parses the raw WakeAttemptsMetadata string (not the pre-parsed
			// WakeAttempts int, which zeroes on ErrRange), so sessionWakeAttemptsInfo
			// clamps identically here while WakeAttemptsMetadata keeps the raw bytes
			// verbatim.
			ID:     "ga-wakeoverflow",
			Type:   session.BeadType,
			Title:  "wakeoverflow",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":      "worker",
				"wake_attempts": "999999999999999999999",
			},
		},
		// --- R2 sleep/wake-reason twin fixtures (display reason lane) ---
		"always-named": {
			// A configured always-mode named session with a session_name (so the
			// full sleep capability resolves). sessionWithinDesiredConfigInfo's named
			// arm and evaluateWakeReasonsInfo's isAlwaysNamed WakeConfig arm both fire.
			ID:     "ga-always",
			Type:   session.BeadType,
			Title:  "mayor",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "mayor",
				"configured_named_session":  "true",
				"configured_named_identity": "mayor",
				"configured_named_mode":     "always",
				"session_name":              "mayor",
				"state":                     "active",
			},
		},
		"named-mode-padded": {
			// configured_named_mode is whitespace-padded: NamedSessionMode trims it,
			// so namedSessionModeInfo must trim Info.ConfiguredNamedMode identically —
			// a raw (untrimmed) read would return " always " and diverge. Load-bearing
			// for the namedSessionMode trim.
			ID:     "ga-modepad",
			Type:   session.BeadType,
			Title:  "mayor",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "mayor",
				"configured_named_session":  "true",
				"configured_named_identity": "mayor",
				"configured_named_mode":     "  always  ",
				"session_name":              "mayor",
			},
		},
		"dependency-only-padded": {
			// dependency_only is whitespace-padded: sessionWithinDesiredConfig compares
			// it == "true" WITHOUT trimming, so the padded value reads NOT
			// dependency-only. sessionWithinDesiredConfigInfo must use the RAW
			// DependencyOnlyMetadata (== "true"), not the trimmed DependencyOnly bool,
			// or it would wrongly exclude this session. Load-bearing for the trap.
			ID:     "ga-deppad",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":        "worker",
				"session_name":    "worker-deppad",
				"state":           "active",
				"dependency_only": "  true  ",
			},
		},
		"idle-detached-interactive": {
			// A live interactive session detached in the past: drives
			// sessionIdleReference (detached_at branch), the configWakeSuppressed
			// duration window, and sessionKeepWarmEligible.
			ID:     "ga-idledetach",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"session_name": "worker-idledetach",
				"state":        "active",
				"detached_at":  pastRFC3339,
			},
		},
		"idle-timeout-latched": {
			// sleep_reason=idle-timeout is the configWakeSuppressed early-false branch.
			ID:     "ga-idletimeout",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"session_name": "worker-idletimeout",
				"state":        "asleep",
				"sleep_reason": "idle-timeout",
			},
		},
	}

	const tmpl = "worker"

	boolChecks := map[string]struct {
		bead func(beads.Bead) bool
		info func(session.Info) bool
	}{
		"isPoolManagedSessionBead":  {isPoolManagedSessionBead, isPoolManagedSessionInfo},
		"isEphemeralSessionBead":    {isEphemeralSessionBead, isEphemeralSessionInfo},
		"isManualSessionBead":       {isManualSessionBead, isManualSessionInfo},
		"isNamedSessionBead":        {isNamedSessionBead, isNamedSessionInfo},
		"isDrainedSessionBead":      {isDrainedSessionBead, isDrainedSessionInfo},
		"isFailedCreateSessionBead": {isFailedCreateSessionBead, isFailedCreateSessionInfo},
		// Raw reference inlined: the production shouldRollbackPendingCreate raw form
		// was deleted in WI-6 R4, so the Info twin is pinned against an independent
		// bead-metadata read (self-sufficient oracle, not a side door).
		"shouldRollbackPendingCreate": {func(b beads.Bead) bool { return strings.TrimSpace(b.Metadata["pending_create_claim"]) == "true" }, shouldRollbackPendingCreateInfo},
		"isStaleCreating":             {isStaleCreating, isStaleCreatingInfo},
		"isPoolSessionSlotFreeable":   {isPoolSessionSlotFreeable, isPoolSessionSlotFreeableInfo},
		"beadOwnsPoolSessionName":     {beadOwnsPoolSessionName, infoOwnsPoolSessionName},
	}

	// Agent-dependent classifiers. A bare pool agent (no instance-expansion, no
	// canonical-singleton identity) exercises existingPoolSlot's slot parsing and
	// isEphemeralSessionBeadForAgent's ephemeral-first branch.
	agentFixture := &config.Agent{Name: "worker"}
	agentBoolChecks := map[string]struct {
		bead func(beads.Bead) bool
		info func(session.Info) bool
	}{
		"isEphemeralSessionBeadForAgent": {
			func(b beads.Bead) bool { return isEphemeralSessionBeadForAgent(b, agentFixture) },
			func(i session.Info) bool { return isEphemeralSessionInfoForAgent(i, agentFixture) },
		},
		"isLegacyManualSessionBeadForAgent": {
			func(b beads.Bead) bool { return isLegacyManualSessionBeadForAgent(b, agentFixture) },
			func(i session.Info) bool { return isLegacyManualSessionInfoForAgent(i, agentFixture) },
		},
		"isManualSessionBeadForAgent": {
			func(b beads.Bead) bool { return isManualSessionBeadForAgent(b, agentFixture) },
			func(i session.Info) bool { return isManualSessionInfoForAgent(i, agentFixture) },
		},
	}

	agentIntChecks := map[string]struct {
		bead func(beads.Bead) int
		info func(session.Info) int
	}{
		"existingPoolSlot": {
			func(b beads.Bead) int { return existingPoolSlot(agentFixture, b) },
			func(i session.Info) int { return existingPoolSlotInfo(agentFixture, i) },
		},
	}

	stringChecks := map[string]struct {
		bead func(beads.Bead) string
		info func(session.Info) string
	}{
		"sessionOrigin": {sessionOrigin, sessionOriginInfo},
		// retiredSessionFallbackRoute twin (added by the #4088 stranded-repair port):
		// pins the run_target fallback (template-first, agent_name second) byte-
		// identical across the raw named-session-retirement path and the Info-form
		// stranded-repair reopen path.
		"retiredSessionFallbackRoute": {retiredSessionFallbackRoute, retiredSessionFallbackRouteInfo},
		// sessionMetadataStateInfo's raw sibling sessionMetadataState was deleted in
		// WI-6 R2 (its last caller, the wake-reason display lane, typed onto Info), so
		// this row pins the Info form against a reference implementation of the same
		// awake→active / start_pending→creating / drained→asleep normalization.
		"sessionMetadataStateInfo": {
			func(b beads.Bead) string {
				switch state := strings.TrimSpace(b.Metadata["state"]); state {
				case "awake":
					return "active"
				case string(session.StateStartPending):
					return "creating"
				case "drained":
					return "asleep"
				default:
					return state
				}
			},
			sessionMetadataStateInfo,
		},
		"namedSessionMode":          {namedSessionMode, namedSessionModeInfo},
		"sessionBeadStoredTemplate": {sessionBeadStoredTemplate, sessionBeadStoredTemplateInfo},
		"sessionBeadAgentName":      {sessionBeadAgentName, sessionBeadAgentNameInfo},
		"namedSessionIdentity":      {namedSessionIdentity, namedSessionIdentityInfo},
		"sessionBeadIdentifier":     {sessionBeadIdentifier, sessionBeadIdentifierInfo},
		// generation has no named classifier — it is read inline via Atoi/TrimSpace
		// in the drain/wake path — so this pins the raw codec mirror directly.
		"sessionGeneration": {
			func(b beads.Bead) string { return b.Metadata["generation"] },
			func(i session.Info) string { return i.Generation },
		},
		// started_config_hash / pin_awake have no named classifier — the reconciler
		// reads them inline (string compare / TrimSpace / != "true") in the desired
		// path's config-drift and wake branches — so these pin the raw codec mirrors
		// directly, the same way sessionGeneration does.
		"sessionStartedConfigHash": {
			func(b beads.Bead) string { return b.Metadata["started_config_hash"] },
			func(i session.Info) string { return i.StartedConfigHash },
		},
		"sessionPinAwake": {
			func(b beads.Bead) string { return b.Metadata["pin_awake"] },
			func(i session.Info) string { return i.PinAwake },
		},
		// Reconciler decision-read mirrors (front-door Phase 5). These have no
		// named classifier — the reconciler reads them inline — so each pins the
		// raw codec mirror directly. The symbolic-key cases feed the cmd/gc
		// constant, guarding the info_store.go literal against constant drift.
		"sessionHeldUntil": {
			func(b beads.Bead) string { return b.Metadata["held_until"] },
			func(i session.Info) string { return i.HeldUntil },
		},
		"sessionWaitHold": {
			func(b beads.Bead) string { return b.Metadata["wait_hold"] },
			func(i session.Info) string { return i.WaitHold },
		},
		"sessionChurnCount": {
			func(b beads.Bead) string { return b.Metadata["churn_count"] },
			func(i session.Info) string { return i.ChurnCount },
		},
		"sessionWakeMode": {
			func(b beads.Bead) string { return b.Metadata["wake_mode"] },
			func(i session.Info) string { return i.WakeMode },
		},
		"sessionSleepIntent": {
			func(b beads.Bead) string { return b.Metadata["sleep_intent"] },
			func(i session.Info) string { return i.SleepIntent },
		},
		"sessionInstanceToken": {
			func(b beads.Bead) string { return b.Metadata["instance_token"] },
			func(i session.Info) string { return i.InstanceToken },
		},
		"sessionDetachedAt": {
			func(b beads.Bead) string { return b.Metadata["detached_at"] },
			func(i session.Info) string { return i.DetachedAt },
		},
		"sessionCurrentlyProcessingBeadID": {
			func(b beads.Bead) string { return b.Metadata[session.CurrentBeadIDKey] },
			func(i session.Info) string { return i.CurrentlyProcessingBeadID },
		},
		"sessionCoreHashBreakdown": {
			func(b beads.Bead) string { return b.Metadata["core_hash_breakdown"] },
			func(i session.Info) string { return i.CoreHashBreakdown },
		},
		"sessionStartedProvisionHash": {
			func(b beads.Bead) string { return b.Metadata["started_provision_hash"] },
			func(i session.Info) string { return i.StartedProvisionHash },
		},
		"sessionStartedLaunchHash": {
			func(b beads.Bead) string { return b.Metadata["started_launch_hash"] },
			func(i session.Info) string { return i.StartedLaunchHash },
		},
		"sessionStartedLiveHash": {
			func(b beads.Bead) string { return b.Metadata["started_live_hash"] },
			func(i session.Info) string { return i.StartedLiveHash },
		},
		"sessionConfigDriftDeferredAt": {
			func(b beads.Bead) string { return b.Metadata[namedSessionConfigDriftDeferredAtMetadata] },
			func(i session.Info) string { return i.ConfigDriftDeferredAt },
		},
		"sessionConfigDriftDeferredKey": {
			func(b beads.Bead) string { return b.Metadata[namedSessionConfigDriftDeferredKeyMetadata] },
			func(i session.Info) string { return i.ConfigDriftDeferredKey },
		},
		"sessionAttachedConfigDriftDeferredAt": {
			func(b beads.Bead) string { return b.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] },
			func(i session.Info) string { return i.AttachedConfigDriftDeferredAt },
		},
		"sessionAttachedConfigDriftDeferredKey": {
			func(b beads.Bead) string { return b.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata] },
			func(i session.Info) string { return i.AttachedConfigDriftDeferredKey },
		},
		"sessionStrandedEventEmittedAt": {
			func(b beads.Bead) string { return b.Metadata[strandedEventEmittedKey] },
			func(i session.Info) string { return i.StrandedEventEmittedAt },
		},
		"sessionNameExplicit": {
			func(b beads.Bead) string { return b.Metadata["session_name_explicit"] },
			func(i session.Info) string { return i.SessionNameExplicit },
		},
		"sessionWakeRequest": {
			func(b beads.Bead) string { return b.Metadata["wake_request"] },
			func(i session.Info) string { return i.WakeRequest },
		},
		"sessionRestartRequested": {
			func(b beads.Bead) string { return b.Metadata["restart_requested"] },
			func(i session.Info) string { return i.RestartRequested },
		},
		"sessionSessionIDFlag": {
			func(b beads.Bead) string { return b.Metadata["session_id_flag"] },
			func(i session.Info) string { return i.SessionIDFlag },
		},
		"sessionTemplateOverrides": {
			func(b beads.Bead) string { return b.Metadata["template_overrides"] },
			func(i session.Info) string { return i.TemplateOverrides },
		},
		"sessionWakeAttemptsMetadata": {
			func(b beads.Bead) string { return b.Metadata["wake_attempts"] },
			func(i session.Info) string { return i.WakeAttemptsMetadata },
		},
	}

	sliceChecks := map[string]struct {
		bead func(beads.Bead) []string
		info func(session.Info) []string
	}{
		"sessionBeadAssigneeIdentities": {sessionBeadAssigneeIdentities, sessionBeadAssigneeIdentitiesInfo},
		"sessionAssignmentIdentifiers":  {sessionAssignmentIdentifiers, sessionAssignmentIdentifiersInfo},
	}

	// namedSpecCfg declares a singleton named session "mayor" backed by an agent
	// "mayor", so findNamedSessionSpec(cfg, "", "mayor") resolves — exercising the
	// configuredNamedSessionBeadHasSpec true branch on the "named" fixture rather
	// than a trivial both-false pass under nil cfg. The guard below fails loudly if
	// the fixture or cfg ever stops hitting that branch.
	namedSpecCfg := &config.City{
		Agents:        []config.Agent{{Name: "mayor"}},
		NamedSessions: []config.NamedSession{{Template: "mayor"}},
	}
	if !configuredNamedSessionBeadHasSpec(beadsByShape["named"], namedSpecCfg, "") {
		t.Fatal("configuredNamedSessionBeadHasSpec(named, namedSpecCfg) = false; fixture/cfg no longer exercise the has-spec true branch")
	}
	// The "named" fixture (session_name "mayor", no terminal state) must resolve
	// its spec AND hit the keep-alias true branch under namedSpecCfg, so the
	// preserveConfiguredNamedSessionBead equivalence case below is a real
	// true-branch comparison, not a trivial both-false pass.
	if !preserveConfiguredNamedSessionBead(beadsByShape["named"], namedSpecCfg, "") {
		t.Fatal("preserveConfiguredNamedSessionBead(named, namedSpecCfg) = false; fixture/cfg no longer exercise the keep-alias true branch")
	}

	// classifiers that take a cfg and/or a template argument.
	cfgBoolChecks := map[string]struct {
		bead func(beads.Bead) bool
		info func(session.Info) bool
	}{
		"isCanonicalPoolManagedSessionBeadForTemplate": {
			func(b beads.Bead) bool { return isCanonicalPoolManagedSessionBeadForTemplate(b, tmpl) },
			func(i session.Info) bool { return isCanonicalPoolManagedSessionInfoForTemplate(i, tmpl) },
		},
		// nil cfg exercises the transport / provider=="acp" / MCP-key branches;
		// the cfg-dependent agent/provider resolution is out of the codec's scope.
		"beadUsesACPTransport": {
			func(b beads.Bead) bool { return beadUsesACPTransport(b, nil) },
			func(i session.Info) bool { return infoUsesACPTransport(i, nil) },
		},
		"configuredNamedSessionBeadHasSpec": {
			func(b beads.Bead) bool { return configuredNamedSessionBeadHasSpec(b, namedSpecCfg, "") },
			func(i session.Info) bool { return configuredNamedSessionBeadHasSpecInfo(i, namedSpecCfg, "") },
		},
		"preserveConfiguredNamedSessionBead": {
			func(b beads.Bead) bool { return preserveConfiguredNamedSessionBead(b, namedSpecCfg, "") },
			func(i session.Info) bool { return preserveConfiguredNamedSessionBeadInfo(i, namedSpecCfg, "") },
		},
	}

	cfgStringChecks := map[string]struct {
		bead func(beads.Bead) string
		info func(session.Info) string
	}{
		"resolvedSessionTemplate": {
			func(b beads.Bead) string { return resolvedSessionTemplate(b, nil) },
			func(i session.Info) string { return resolvedSessionTemplateInfo(i, nil) },
		},
		"normalizedSessionTemplate": {
			func(b beads.Bead) string { return normalizedSessionTemplate(b, nil) },
			func(i session.Info) string { return normalizedSessionTemplateInfo(i, nil) },
		},
		"sessionAgentMetricIdentity": {
			func(b beads.Bead) string { return sessionAgentMetricIdentity(b, nil) },
			func(i session.Info) string { return sessionAgentMetricIdentityInfo(i, nil) },
		},
	}

	// assigneeCfg declares a "worker" agent plus a "mayor" named session backed by
	// a "mayor" agent so both the plain-template and named-session-fallback arms of
	// sessionAssignmentIdentifiersForConfig(Info) are exercised across fixtures,
	// and sessionAgentConfig(Info)'s findAgentByTemplate resolves for the worker/
	// mayor templates rather than only the nil-agent fallthrough.
	assigneeCfg := &config.City{
		Agents:        []config.Agent{{Name: "worker"}, {Name: "mayor"}},
		NamedSessions: []config.NamedSession{{Template: "mayor"}},
	}
	cfgSliceChecks := map[string]struct {
		bead func(beads.Bead) []string
		info func(session.Info) []string
	}{
		"sessionAssignmentIdentifiersForConfig": {
			func(b beads.Bead) []string { return sessionAssignmentIdentifiersForConfig(b, assigneeCfg) },
			func(i session.Info) []string { return sessionAssignmentIdentifiersForConfigInfo(i, assigneeCfg) },
		},
	}
	cfgAgentChecks := map[string]struct {
		bead func(beads.Bead) *config.Agent
		info func(session.Info) *config.Agent
	}{
		"sessionAgentConfig": {
			func(b beads.Bead) *config.Agent { return sessionAgentConfig(assigneeCfg, b) },
			func(i session.Info) *config.Agent { return sessionAgentConfigInfo(assigneeCfg, i) },
		},
	}

	const leaseStartupTimeout = 90 * time.Second
	// leaseCfg resolves template "worker" to a live (non-suspended) agent so the
	// config-agent-resolving fixtures (e.g. the crash-lane sessionExitFactsInfo
	// check below) see a real agent rather than only the nil-agent fallthrough.
	leaseCfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Reference implementations of the pending-create lease classifiers whose raw
	// bead forms were deleted in WI-6 R3. They reproduce the exact deleted logic
	// off beads.Bead so each surviving *Info twin keeps a byte-identical oracle
	// independent of the production Info projection (the sessionMetadataStateInfo
	// precedent, scaled to the interdependent lease family). A mutation of any
	// twin diverges from its reference here.
	refPendingCreateAttemptStale := func(b beads.Bead) bool {
		if clk == nil {
			return false
		}
		now := clk.Now()
		if started, ok := parseRFC3339Metadata(b.Metadata["pending_create_started_at"]); ok {
			return !now.Before(started.Add(staleCreatingStateTimeout))
		}
		if b.CreatedAt.IsZero() {
			return true
		}
		return !now.Before(b.CreatedAt.Add(staleCreatingStateTimeout))
	}
	refStaleCreatingState := func(b beads.Bead) bool {
		if clk == nil {
			return false
		}
		if strings.TrimSpace(b.Metadata["state"]) != string(session.StateCreating) {
			return false
		}
		return refPendingCreateAttemptStale(b)
	}
	refSessionStartRequested := func(b beads.Bead) bool {
		if strings.TrimSpace(b.Metadata["state"]) == string(session.StateStartPending) {
			return true
		}
		if strings.TrimSpace(b.Metadata["pending_create_claim"]) == "true" {
			return true
		}
		if strings.TrimSpace(b.Metadata["state"]) != "creating" {
			return false
		}
		return !refStaleCreatingState(b)
	}
	refPendingCreateStartInFlight := func(b beads.Bead) bool {
		if strings.TrimSpace(b.Metadata["pending_create_claim"]) != "true" &&
			session.State(strings.TrimSpace(b.Metadata["state"])) != session.StateCreating {
			return false
		}
		lastWoke := strings.TrimSpace(b.Metadata["last_woke_at"])
		if lastWoke == "" {
			return false
		}
		started, err := time.Parse(time.RFC3339, lastWoke)
		if err != nil {
			return false
		}
		st := leaseStartupTimeout
		if st <= 0 {
			st = time.Minute
		}
		return clk.Now().Before(started.Add(st + staleKeyDetectDelay + 5*time.Second))
	}
	refPendingCreateNeverStartedLeaseExpired := func(b beads.Bead) bool {
		if strings.TrimSpace(b.Metadata["pending_create_claim"]) != "true" {
			return false
		}
		if strings.TrimSpace(b.Metadata["last_woke_at"]) != "" {
			return false
		}
		anchor := b.CreatedAt
		if started, ok := parseRFC3339Metadata(b.Metadata["pending_create_started_at"]); ok {
			anchor = started
		}
		if anchor.IsZero() {
			return true
		}
		return clk.Now().After(anchor.Add(pendingCreateNeverStartedTimeout))
	}
	refPendingCreateLeaseActive := func(b beads.Bead) bool {
		if strings.TrimSpace(b.Metadata["pending_create_claim"]) != "true" {
			return false
		}
		if refPendingCreateStartInFlight(b) {
			return true
		}
		if strings.TrimSpace(b.Metadata["last_woke_at"]) == "" {
			return !refPendingCreateNeverStartedLeaseExpired(b)
		}
		return !refPendingCreateAttemptStale(b)
	}
	refPendingCreateNeverStartedExpired := func(b beads.Bead) bool {
		if strings.TrimSpace(b.Metadata["pending_create_claim"]) != "true" {
			return false
		}
		if !pendingCreateRollbackState(b.Metadata["state"]) {
			return false
		}
		return refPendingCreateNeverStartedLeaseExpired(b)
	}
	refPendingCreateLeaseExpiredForRollback := func(b beads.Bead) bool {
		if strings.TrimSpace(b.Metadata["pending_create_claim"]) != "true" {
			return false
		}
		state := session.State(strings.TrimSpace(b.Metadata["state"]))
		if !pendingCreateRollbackState(string(state)) {
			return false
		}
		if state == session.StateAsleep {
			if strings.TrimSpace(b.Metadata["last_woke_at"]) == "" {
				return refPendingCreateNeverStartedExpired(b)
			}
			return refPendingCreateAttemptStale(b)
		}
		if refPendingCreateStartInFlight(b) {
			return false
		}
		if strings.TrimSpace(b.Metadata["last_woke_at"]) == "" {
			return refPendingCreateNeverStartedExpired(b)
		}
		return refPendingCreateAttemptStale(b)
	}
	clkBoolChecks := map[string]struct {
		bead func(beads.Bead) bool
		info func(session.Info) bool
	}{
		"staleCreatingState": {
			refStaleCreatingState,
			func(i session.Info) bool { return staleCreatingStateInfo(i, clk) },
		},
		"sessionStartRequested": {
			refSessionStartRequested,
			func(i session.Info) bool { return sessionStartRequestedInfo(i, clk) },
		},
		"pendingCreateAttemptStale": {
			refPendingCreateAttemptStale,
			func(i session.Info) bool { return pendingCreateAttemptStaleInfo(i, clk) },
		},
		"pendingCreateNeverStartedLeaseExpired": {
			refPendingCreateNeverStartedLeaseExpired,
			func(i session.Info) bool { return pendingCreateNeverStartedLeaseExpiredInfo(i, clk) },
		},
		"pendingCreateStartInFlight": {
			refPendingCreateStartInFlight,
			func(i session.Info) bool { return pendingCreateStartInFlightInfo(i, clk, leaseStartupTimeout) },
		},
		"pendingCreateLeaseActive": {
			refPendingCreateLeaseActive,
			func(i session.Info) bool { return pendingCreateLeaseActiveInfo(i, clk, leaseStartupTimeout) },
		},
		"pendingCreateNeverStartedExpired": {
			refPendingCreateNeverStartedExpired,
			func(i session.Info) bool { return pendingCreateNeverStartedExpiredInfo(i, clk) },
		},
		"pendingCreateLeaseExpiredForRollback": {
			refPendingCreateLeaseExpiredForRollback,
			func(i session.Info) bool {
				return pendingCreateLeaseExpiredForRollbackInfo(i, clk, leaseStartupTimeout)
			},
		},
	}

	// timeBoolChecks pins the raw-vs-Info equivalence for classifiers that return a
	// (time.Time, bool) pair rather than a scalar. Times are compared with Equal so a
	// stripped monotonic reading never trips a false mismatch.
	timeBoolChecks := map[string]struct {
		bead func(beads.Bead) (time.Time, bool)
		info func(session.Info) (time.Time, bool)
	}{
		"staleReapStartBoundary": {
			staleReapStartBoundary,
			staleReapStartBoundaryInfo,
		},
	}

	// The "pending-resume-preserve" fixture must hit the true branch under
	// leaseStartupTimeout so the equivalence case above is a real true-branch
	// comparison (exercising the Info.StartedConfigHash gate + the lease tail),
	// not a trivial both-false pass.
	if !pendingResumePreservingNamedRestartInfo(sessiontest.SeedBead(t, beadsByShape["pending-resume-preserve"]), clk, leaseStartupTimeout) {
		t.Fatal("pendingResumePreservingNamedRestartInfo(pending-resume-preserve) = false; fixture no longer exercises the resume-preserve true branch")
	}
	// The "pending-create-inflight-lease" fixture MUST decide
	// pendingCreateLeaseActiveInfo via the in-flight branch: the lease is active
	// (pendingCreateStartInFlightInfo true) even though the attempt is stale, so a
	// regression that drops the in-flight `return true` would fall through to the
	// attempt-stale tail and wrongly report the lease inactive. This makes the
	// clkBoolChecks equivalence row for pendingCreateLeaseActive a real in-flight
	// true-branch decision, not a both-agree-by-accident pass.
	inflightInfo := sessiontest.SeedBead(t, beadsByShape["pending-create-inflight-lease"])
	if !pendingCreateStartInFlightInfo(inflightInfo, clk, leaseStartupTimeout) {
		t.Fatal("pendingCreateStartInFlightInfo(pending-create-inflight-lease) = false; fixture no longer exercises the in-flight lease window")
	}
	if !pendingCreateAttemptStaleInfo(inflightInfo, clk) {
		t.Fatal("pendingCreateAttemptStaleInfo(pending-create-inflight-lease) = false; fixture no longer makes the non-in-flight tail return false — the in-flight branch would not be decisive")
	}
	if !pendingCreateLeaseActiveInfo(inflightInfo, clk, leaseStartupTimeout) {
		t.Fatal("pendingCreateLeaseActiveInfo(pending-create-inflight-lease) = false; the in-flight true-branch regressed (an active in-flight lease must stay active despite a stale attempt)")
	}
	// staleReapStartBoundaryInfo must advance the boundary to the last_woke_at time
	// (not CreatedAt) on the recent-wake fixture, exercising the woke-upgrade branch
	// so a regression that returns CreatedAt is caught (both here and in the
	// timeBoolChecks equivalence row above).
	if got, ok := staleReapStartBoundaryInfo(sessiontest.SeedBead(t, beadsByShape["reap-boundary-recent-wake"])); !ok || !got.Equal(recentWoke) {
		t.Fatalf("staleReapStartBoundaryInfo(reap-boundary-recent-wake) = (%v, %v); want the last_woke_at time %v — fixture no longer exercises the woke-upgrade branch", got, ok, recentWoke)
	}
	// The drain-ack fixture must hit the true branch so isDrainAckStopPendingInfo
	// is exercised on a real stop-pending shape, not a trivial both-false pass.
	if !isDrainAckStopPendingInfo(sessiontest.SeedBead(t, beadsByShape["drain-ack-stop-pending"])) {
		t.Fatal("isDrainAckStopPendingInfo(drain-ack-stop-pending) = false; fixture no longer exercises the true branch")
	}
	// The hold/quarantine fixture must drive lifecycleTimerBlockerInfo's non-empty
	// branch so it is exercised on a real blocker shape, not a both-empty pass.
	if lifecycleTimerBlockerInfo(sessiontest.SeedBead(t, beadsByShape["hold-and-quarantine"]), clk.Now()) == "" {
		t.Fatal(`lifecycleTimerBlockerInfo(hold-and-quarantine) = ""; fixture no longer exercises the blocker branch`)
	}
	// The rapid-crash-candidate fixture must classify ExitRapidCrash under alive=false
	// so sessionExitFactsInfo is exercised on the crash lane, not a both-ExitNone pass.
	if got := session.DecideSessionExit(sessionExitFactsInfo(sessiontest.SeedBead(t, beadsByShape["rapid-crash-candidate"]), leaseCfg, false, nil, clk)); got != session.ExitRapidCrash {
		t.Fatalf("DecideSessionExit(rapid-crash-candidate, alive=false) = %v; want ExitRapidCrash — fixture no longer exercises the crash lane", got)
	}
	// stableLongEnoughInfo must be true on the old-marker fixture (past RFC3339
	// last_woke_at) so its true branch is exercised, not a trivial both-false pass.
	if !stableLongEnoughInfo(sessiontest.SeedBead(t, beadsByShape["pending-create-claim-old-markers"]), clk) {
		t.Fatal("stableLongEnoughInfo(pending-create-claim-old-markers) = false; fixture no longer exercises the stable-long-enough true branch")
	}

	for shape, b := range beadsByShape {
		b := b
		info := sessiontest.SeedBead(t, b)
		t.Run(shape, func(t *testing.T) {
			for name, c := range boolChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
			for name, c := range agentBoolChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
			for name, c := range agentIntChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%d bead=%d", name, got, want)
				}
			}
			for name, c := range cfgBoolChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
			for name, c := range clkBoolChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
			for name, c := range timeBoolChecks {
				gotT, gotOK := c.info(info)
				wantT, wantOK := c.bead(b)
				if gotOK != wantOK || !gotT.Equal(wantT) {
					t.Errorf("%s: info=(%v,%v) bead=(%v,%v)", name, gotT, gotOK, wantT, wantOK)
				}
			}
			for name, c := range stringChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%q bead=%q", name, got, want)
				}
			}
			for name, c := range cfgStringChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%q bead=%q", name, got, want)
				}
			}
			for name, c := range sliceChecks {
				if got, want := c.info(info), c.bead(b); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
			for name, c := range cfgSliceChecks {
				if got, want := c.info(info), c.bead(b); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
			for name, c := range cfgAgentChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
		})
	}

	// Async-start commit-protocol twins. Each reads TWO session views (prepared +
	// current), so they don't fit the single-bead loop above. Prove the Info form is
	// byte-identical to an independent raw bead-metadata reference across the fixture
	// corpus. The production raw forms were deleted in WI-6 R4, so these reference
	// implementations are inlined here as the self-sufficient oracle (the Info
	// projection is pinned against a direct metadata read, not a side door).
	refShouldRollback := func(b beads.Bead) bool {
		return strings.TrimSpace(b.Metadata["pending_create_claim"]) == "true"
	}
	refCommandStale := func(prepared preparedStart, current beads.Bead) bool {
		preparedCommand := strings.TrimSpace(prepared.candidate.tp.Command)
		currentCommand := strings.TrimSpace(current.Metadata["command"])
		return preparedCommand != "" && currentCommand != "" && preparedCommand != currentCommand
	}
	refIdentityMatches := func(prepared, current beads.Bead) bool {
		preparedToken := strings.TrimSpace(prepared.Metadata["instance_token"])
		if preparedToken != "" {
			return strings.TrimSpace(current.Metadata["instance_token"]) == preparedToken
		}
		preparedGeneration := strings.TrimSpace(prepared.Metadata["generation"])
		if preparedGeneration == "" {
			return true
		}
		return strings.TrimSpace(current.Metadata["generation"]) == preparedGeneration
	}
	refStillCurrent := func(prepared, current beads.Bead) bool {
		if strings.TrimSpace(current.Status) == "closed" {
			return false
		}
		if !refIdentityMatches(prepared, current) {
			return false
		}
		currentState := session.State(strings.TrimSpace(current.Metadata["state"]))
		if currentState == session.StateAwake || currentState == session.StateActive {
			return true
		}
		if refShouldRollback(prepared) && !refShouldRollback(current) {
			return false
		}
		return confirmPendingStart(string(currentState))
	}
	refCleanupAllowed := func(prepared, current beads.Bead) bool {
		if strings.TrimSpace(current.Status) == "closed" {
			return true
		}
		if !refIdentityMatches(prepared, current) {
			return true
		}
		currentState := session.State(strings.TrimSpace(current.Metadata["state"]))
		if refShouldRollback(prepared) && !refShouldRollback(current) {
			return currentState != session.StateAwake && currentState != session.StateActive
		}
		return !confirmPendingStart(string(currentState)) &&
			currentState != session.StateAwake &&
			currentState != session.StateActive
	}
	//
	// asyncStartPreparedCommandStale's prepared side is the resolved template command
	// (tp.Command), shared by both forms; only the current side switches bead↔Info
	// (Info.Command == metadata["command"]). The "closed" and "pool-managed-slot"
	// (state=awake) / "pool-managed-flag-only" (state=active) fixtures exercise the
	// Closed and awake/active branches the design calls out.
	preparedWithCommand := func(cmd string) preparedStart {
		return preparedStart{candidate: startCandidate{tp: TemplateParams{Command: cmd}}}
	}
	for currentShape, currentBead := range beadsByShape {
		currentBead := currentBead
		currentInfo := sessiontest.SeedBead(t, currentBead)
		t.Run("asyncCommandStale/"+currentShape, func(t *testing.T) {
			for _, cmd := range []string{"", "claude --resume", "codex exec", "  claude --resume  "} {
				pr := preparedWithCommand(cmd)
				if got, want := asyncStartPreparedCommandStaleInfo(pr, currentInfo), refCommandStale(pr, currentBead); got != want {
					t.Errorf("cmd=%q info=%v bead=%v", cmd, got, want)
				}
			}
		})
	}

	// The identity / still-current / cleanup-allowed twins take (prepared, current)
	// as two session views. Assert equivalence across every ordered pair of fixture
	// shapes, so the instance_token / generation identity fallback, the closed and
	// awake/active short-circuits, and the pending-create-claim rollback branch are
	// all covered on both the prepared and current sides.
	for prepShape, prepBead := range beadsByShape {
		prepBead := prepBead
		prepInfo := sessiontest.SeedBead(t, prepBead)
		for curShape, curBead := range beadsByShape {
			curBead := curBead
			curInfo := sessiontest.SeedBead(t, curBead)
			t.Run("asyncPair/"+prepShape+"->"+curShape, func(t *testing.T) {
				if got, want := asyncStartIdentityMatchesInfo(prepInfo, curInfo), refIdentityMatches(prepBead, curBead); got != want {
					t.Errorf("asyncStartIdentityMatches: info=%v bead=%v", got, want)
				}
				if got, want := asyncStartSessionStillCurrentInfo(prepInfo, curInfo), refStillCurrent(prepBead, curBead); got != want {
					t.Errorf("asyncStartSessionStillCurrent: info=%v bead=%v", got, want)
				}
				if got, want := asyncStartStaleRuntimeCleanupAllowedInfo(prepInfo, curInfo), refCleanupAllowed(prepBead, curBead); got != want {
					t.Errorf("asyncStartStaleRuntimeCleanupAllowed: info=%v bead=%v", got, want)
				}
			})
		}
	}

	// --- R2 sleep-cluster + wake-reason twin equivalence (display reason lane) ---
	// These twins take a resolved policy / provider / clock in addition to the
	// session view, so they don't fit the single-bead scalar loop above. wakeCfg
	// resolves the worker/mayor templates to live agents with an interactive-resume
	// sleep policy so the sleep-ENABLED branches actually fire; wakeSP reports full
	// sleep capability + activity/attachment so a session-named fixture yields an
	// enabled policy. Raw and Info forms share the SAME provider, so the runtime
	// probes (which stay raw in both) agree by construction — any divergence is a
	// real metadata-read fidelity bug in a twin.
	wakeCfg := &config.City{
		SessionSleep:  config.SessionSleepConfig{InteractiveResume: "60s"},
		Agents:        []config.Agent{{Name: "worker"}, {Name: "mayor"}},
		NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "always"}},
	}
	wakeSP := routedSleepProvider{
		Provider:     runtime.NewFake(),
		capabilities: runtime.ProviderCapabilities{CanReportActivity: true, CanReportAttachment: true},
		sleep:        runtime.SessionSleepCapabilityFull,
	}
	wakePools := []map[string]int{nil, {"worker": 1}, {"mayor": 1}}
	for shape, b := range beadsByShape {
		b := b
		info := sessiontest.SeedBead(t, b)
		t.Run("wakeTwins/"+shape, func(t *testing.T) {
			// The raw sleep-read forms were deleted in WI-6 R3; the *Info twins are
			// now the only form. Their non-trivial branches are pinned by the
			// load-bearing Info assertions below (fingerprint-match, idle-reference
			// detached branch, keep-warm true branch, pending-clear/held).
			for _, pd := range wakePools {
				// sessionWithinDesiredConfig (raw) survives until R3, so its Info twin
				// stays pinned against it directly here.
				gotA, gotOK := sessionWithinDesiredConfigInfo(info, wakeCfg, pd)
				wantA, wantOK := sessionWithinDesiredConfig(b, wakeCfg, pd)
				if gotA != wantA || gotOK != wantOK {
					t.Errorf("sessionWithinDesiredConfigInfo(pd=%v) = (%v,%v), want (%v,%v)", pd, gotA, gotOK, wantA, wantOK)
				}
				// evaluateWakeReasonsInfo's wrapper wakeReasonsInfo must return exactly
				// its .Reasons; the raw evaluateWakeReasons sibling was deleted in R2, so
				// its full behavior is pinned by the migrated wakeReasons unit tests
				// (session_reconcile_test.go) + the sessionReason display characterization.
				eval := evaluateWakeReasonsInfo(info, wakeCfg, wakeSP, pd, nil, nil, clk)
				if got := wakeReasonsInfo(info, wakeCfg, wakeSP, pd, nil, nil, clk); !reflect.DeepEqual(got, eval.Reasons) {
					t.Errorf("wakeReasonsInfo(pd=%v) = %+v, want eval.Reasons %+v", pd, got, eval.Reasons)
				}
			}
		})
	}

	// pendingInteractionKeepsAwake keeps its runtime pending probe raw; pendSP
	// reports a pending interaction for "worker-pending" so the readiness gate is
	// live. The held/quarantine fixtures drive the LifecycleInputFromInfo blocker
	// read, and the wait_hold fixture the trimmed-wait_hold gate.
	pendSP := runtime.NewFake()
	pendSP.SetPendingInteraction("worker-pending", &runtime.PendingInteraction{RequestID: "r"})
	pendFixtures := map[string]beads.Bead{
		"pending-clear":    makeBead("gp-clear", map[string]string{"template": "worker", "session_name": "worker-pending"}),
		"pending-held":     makeBead("gp-held", map[string]string{"template": "worker", "session_name": "worker-pending", "held_until": futureRFC3339}),
		"pending-quar":     makeBead("gp-quar", map[string]string{"template": "worker", "session_name": "worker-pending", "quarantined_until": futureRFC3339}),
		"pending-waithold": makeBead("gp-wh", map[string]string{"template": "worker", "session_name": "worker-pending", "wait_hold": " true "}),
		"no-pending":       makeBead("gp-none", map[string]string{"template": "worker", "session_name": "worker-none"}),
	}
	// Load-bearing: pending-clear keeps the session awake (true); pending-held does
	// NOT (BlockerHeld), exercising the LifecycleInputFromInfo blocker read rather
	// than a trivial both-false pass.
	if !pendingInteractionKeepsAwakeInfo(seedSessionInfo(pendFixtures["pending-clear"]), pendSP, "worker-pending", clk) {
		t.Fatal("pendingInteractionKeepsAwakeInfo(pending-clear) = false; want true — fixture no longer exercises the keep-awake true branch")
	}
	if pendingInteractionKeepsAwakeInfo(seedSessionInfo(pendFixtures["pending-held"]), pendSP, "worker-pending", clk) {
		t.Fatal("pendingInteractionKeepsAwakeInfo(pending-held) = true; want false — fixture no longer exercises the held-blocker branch")
	}
	// pending-quar: a still-future quarantine blocks the deferral (BlockerQuarantined).
	if pendingInteractionKeepsAwakeInfo(seedSessionInfo(pendFixtures["pending-quar"]), pendSP, "worker-pending", clk) {
		t.Fatal("pendingInteractionKeepsAwakeInfo(pending-quar) = true; want false — the quarantine-blocker branch regressed")
	}
	// pending-waithold: a non-empty (trimmed) wait_hold suppresses the deferral.
	if pendingInteractionKeepsAwakeInfo(seedSessionInfo(pendFixtures["pending-waithold"]), pendSP, "worker-pending", clk) {
		t.Fatal("pendingInteractionKeepsAwakeInfo(pending-waithold) = true; want false — the trimmed wait_hold gate regressed")
	}
	// no-pending: without a live pending interaction the readiness gate is closed.
	if pendingInteractionKeepsAwakeInfo(seedSessionInfo(pendFixtures["no-pending"]), pendSP, "worker-none", clk) {
		t.Fatal("pendingInteractionKeepsAwakeInfo(no-pending) = true; want false — the runtime readiness gate regressed")
	}

	// Load-bearing: an asleep idle session whose sleep_policy_fingerprint matches
	// the resolved policy is config-wake-suppressed via the exact fingerprint branch.
	// fpPolicy is computed from a template/session-name-only bead so its fingerprint
	// is independent of the sleep_reason/fingerprint metadata below.
	fpPolicy := resolveSessionSleepPolicyInfo(seedSessionInfo(makeBead("ga-fp0", map[string]string{"template": "worker", "session_name": "worker-fp"})), wakeCfg, wakeSP)
	fpBead := makeBead("ga-fp", map[string]string{
		"template":                 "worker",
		"session_name":             "worker-fp",
		"state":                    "asleep",
		"sleep_reason":             "idle",
		"sleep_policy_fingerprint": fpPolicy.Fingerprint,
	})
	fpInfo := seedSessionInfo(fpBead)
	if !configWakeSuppressedInfo(fpInfo, fpPolicy, wakeSP, clk) {
		t.Fatal("configWakeSuppressedInfo(idle-fingerprint-match) = false; want true — fixture no longer exercises the fingerprint-match branch")
	}

	// Load-bearing: sessionIdleReferenceInfo reads the detached_at branch (non-zero)
	// on a recently-detached session, and sessionKeepWarmEligibleInfo is true while
	// that session is still inside its idle window (keep-warm true branch).
	warmInfo := seedSessionInfo(makeBead("ga-warm", map[string]string{
		"template":     "worker",
		"session_name": "worker-warm",
		"state":        "active",
		"detached_at":  clk.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339),
	}))
	warmPolicy := resolveSessionSleepPolicyInfo(warmInfo, wakeCfg, wakeSP)
	if sessionIdleReferenceInfo(warmInfo, wakeSP).IsZero() {
		t.Fatal("sessionIdleReferenceInfo(recent-detach) = zero; the detached_at branch regressed")
	}
	if !sessionKeepWarmEligibleInfo(warmInfo, warmPolicy, wakeSP, clk) {
		t.Fatal("sessionKeepWarmEligibleInfo(recent-detach) = false; want true within the idle window — the keep-warm true branch regressed")
	}

	// Load-bearing: the always-named fixture must be config-eligible AND (under an
	// enabled interactive policy with no demand) still earn WakeConfig via the
	// isAlwaysNamed arm, so namedSessionModeInfo's "always" read is exercised.
	alwaysInfo := sessiontest.SeedBead(t, beadsByShape["always-named"])
	if _, ok := sessionWithinDesiredConfigInfo(alwaysInfo, wakeCfg, nil); !ok {
		t.Fatal("sessionWithinDesiredConfigInfo(always-named, pd=nil) = false; want true — fixture no longer exercises the always-named eligible branch")
	}
	if !containsWakeReason(wakeReasonsInfo(alwaysInfo, wakeCfg, wakeSP, nil, nil, nil, clk), WakeConfig) {
		t.Fatal("wakeReasonsInfo(always-named) missing WakeConfig; fixture no longer exercises the isAlwaysNamed arm")
	}
	// Load-bearing: the whitespace-padded dependency_only fixture must NOT read as
	// dependency-only (raw == "true" fails on the padded value), so it stays
	// config-eligible under demand — a trimmed DependencyOnly read would wrongly
	// exclude it.
	depPadInfo := sessiontest.SeedBead(t, beadsByShape["dependency-only-padded"])
	if _, ok := sessionWithinDesiredConfigInfo(depPadInfo, wakeCfg, map[string]int{"worker": 1}); !ok {
		t.Fatal("sessionWithinDesiredConfigInfo(dependency-only-padded) = false; want true — the untrimmed dependency_only trap regressed")
	}
}

// TestStaleNonExpandingPoolSessionBeadInfo characterizes staleNonExpandingPoolSessionBeadInfo
// (the singleton pool-reuse staleness predicate) over both a canonical-singleton agent
// — which drives the real identity-slot / pool_slot / manual-exclusion branches — and a
// non-canonical-singleton agent, which short-circuits to false. It replaced the two
// raw-vs-Info equivalence rows removed when the raw staleNonExpandingPoolSessionBead
// retired with the snapshot raw half in WI-7 W-delete, pinning the Info branches against
// a golden.
func TestStaleNonExpandingPoolSessionBeadInfo(t *testing.T) {
	singletonAgent := &config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	nonSingleton := &config.Agent{Name: "worker", MaxActiveSessions: intPtr(5)}

	gotSingleton := map[string]bool{}
	for _, sb := range oracleSessionBeadShapes() {
		info := sessiontest.SeedBead(t, sb)
		gotSingleton[sb.ID] = staleNonExpandingPoolSessionBeadInfo(singletonAgent, info)
		// A non-canonical-singleton agent short-circuits to false on every shape.
		if staleNonExpandingPoolSessionBeadInfo(nonSingleton, info) {
			t.Errorf("staleNonExpandingPoolSessionBeadInfo(nonSingleton, %s) = true, want false (short-circuit)", sb.ID)
		}
	}
	if len(staleSingletonGolden) == 0 || !reflect.DeepEqual(gotSingleton, staleSingletonGolden) {
		t.Errorf("stale-singleton characterization drift; got=%#v", gotSingleton)
	}
}

// staleSingletonGolden is the captured golden for TestStaleNonExpandingPoolSessionBeadInfo.
var staleSingletonGolden = map[string]bool{"ga-bare": false, "ga-named": false, "ga-named-fallback": false, "ga-noname": false, "ga-pool": true}

// The four tests below give the Info twins whose raw sibling (and its equivalence
// row) was deleted in WI-5 W5 direct table coverage, so their logic is pinned even
// though the byte-identical oracle above no longer carries them.

func TestLifecycleTimerBlockerInfo(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Hour).Format(time.RFC3339)
	tests := []struct {
		name string
		md   map[string]string
		want string
	}{
		{"none", map[string]string{}, ""},
		{"hold", map[string]string{"held_until": future}, "user_hold"},
		{"quarantine", map[string]string{"quarantined_until": future}, "quarantine"},
		{"hold wins", map[string]string{"held_until": future, "quarantined_until": future}, "user_hold"},
		{"expired hold", map[string]string{"held_until": past}, ""},
		{"expired quarantine", map[string]string{"quarantined_until": past}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := seedSessionInfo(makeBead("b1", tt.md))
			if got := lifecycleTimerBlockerInfo(info, now); got != tt.want {
				t.Errorf("lifecycleTimerBlockerInfo = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResetPendingCommittedAtInfo(t *testing.T) {
	const valid = "2026-03-08T12:00:00Z"
	tests := []struct {
		name    string
		md      map[string]string
		wantRaw string
		wantOK  bool
	}{
		{"not pending", map[string]string{session.ResetCommittedAtKey: valid}, "", false},
		{"pending + valid", map[string]string{"continuation_reset_pending": "true", session.ResetCommittedAtKey: valid}, valid, true},
		{"pending + empty marker", map[string]string{"continuation_reset_pending": "true"}, "", false},
		{"pending + invalid", map[string]string{"continuation_reset_pending": "true", session.ResetCommittedAtKey: "not-a-time"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := seedSessionInfo(makeBead("b1", tt.md))
			raw, ts, ok := resetPendingCommittedAtInfo(info)
			if raw != tt.wantRaw || ok != tt.wantOK {
				t.Fatalf("resetPendingCommittedAtInfo = (%q, %v, %v), want raw=%q ok=%v", raw, ts, ok, tt.wantRaw, tt.wantOK)
			}
			if tt.wantOK {
				want, _ := time.Parse(time.RFC3339, valid)
				if !ts.Equal(want) {
					t.Errorf("parsed committedAt = %v, want %v", ts, want)
				}
			}
		})
	}
}

func TestSessionHasProviderTerminalErrorInfo(t *testing.T) {
	tests := []struct {
		name string
		md   map[string]string
		want bool
	}{
		{"none", map[string]string{}, false},
		{"explicit terminal error", map[string]string{sessionProviderTerminalErrorMetadataKey: "boom"}, true},
		{"unhealthy drainable reason", map[string]string{
			sessionHealthStateMetadataKey:  "unhealthy",
			sessionDrainableMetadataKey:    boolMetadata(true),
			sessionHealthReasonMetadataKey: "why",
		}, true},
		{"unhealthy missing reason", map[string]string{
			sessionHealthStateMetadataKey: "unhealthy",
			sessionDrainableMetadataKey:   boolMetadata(true),
		}, false},
		{"unhealthy not drainable", map[string]string{
			sessionHealthStateMetadataKey:  "unhealthy",
			sessionHealthReasonMetadataKey: "why",
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := seedSessionInfo(makeBead("b1", tt.md))
			if got := sessionHasProviderTerminalErrorInfo(info); got != tt.want {
				t.Errorf("sessionHasProviderTerminalErrorInfo = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSessionExitFactsInfo(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	cfg := &config.City{}
	recent := now.Add(-15 * time.Second).UTC().Format(time.RFC3339)
	info := seedSessionInfo(makeBead("b1", map[string]string{
		"state":        "awake",
		"last_woke_at": recent,
	}))
	// Field threading: liveness and the verbatim last_woke_at mirror.
	dead := sessionExitFactsInfo(info, cfg, false, nil, clk)
	if dead.Alive {
		t.Error("Alive should thread false")
	}
	if dead.LastWokeAt != recent {
		t.Errorf("LastWokeAt = %q, want %q", dead.LastWokeAt, recent)
	}
	if !sessionExitFactsInfo(info, cfg, true, nil, clk).Alive {
		t.Error("Alive should thread true")
	}
	// A dead session that woke recently, with no deliberate sleep and no pending
	// create, is a rapid crash.
	if got := session.DecideSessionExit(dead); got != session.ExitRapidCrash {
		t.Errorf("DecideSessionExit(dead recent-woke) = %v, want ExitRapidCrash", got)
	}
}
