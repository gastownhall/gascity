package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// TestSessionClassifierInfoEquivalence is the byte-identical oracle for P2 of
// NONWORK-BEAD-FIELDDOOR-PLAN.md. Each converted classifier has a *Info sibling
// that reads typed session.Info fields instead of raw bead metadata. For every
// representative session-bead shape, the Info form (fed
// session.InfoFromPersistedBead(b)) must agree with the original bead form.
//
// This proves the Info projection plus the predicate mirror are semantically
// identical to the existing metadata reads, so later caller migration (P4) is
// safe. Any divergence here is a real fidelity bug in the codec or a mirror.
func TestSessionClassifierInfoEquivalence(t *testing.T) {
	pastRFC3339 := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	futureRFC3339 := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
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
			// Hits the pendingResumePreservingNamedRestart TRUE branch: creating
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
	}

	const tmpl = "worker"

	boolChecks := map[string]struct {
		bead func(beads.Bead) bool
		info func(session.Info) bool
	}{
		"isPoolManagedSessionBead":            {isPoolManagedSessionBead, isPoolManagedSessionInfo},
		"isEphemeralSessionBead":              {isEphemeralSessionBead, isEphemeralSessionInfo},
		"isManualSessionBead":                 {isManualSessionBead, isManualSessionInfo},
		"isNamedSessionBead":                  {isNamedSessionBead, isNamedSessionInfo},
		"isDrainedSessionBead":                {isDrainedSessionBead, isDrainedSessionInfo},
		"isFailedCreateSessionBead":           {isFailedCreateSessionBead, isFailedCreateSessionInfo},
		"shouldRollbackPendingCreate":         {func(b beads.Bead) bool { return shouldRollbackPendingCreate(&b) }, shouldRollbackPendingCreateInfo},
		"isPendingPoolCreate":                 {isPendingPoolCreate, isPendingPoolCreateInfo},
		"isStaleCreating":                     {isStaleCreating, isStaleCreatingInfo},
		"isKnownState":                        {isKnownState, isKnownStateInfo},
		"isPoolSessionSlotFreeable":           {isPoolSessionSlotFreeable, isPoolSessionSlotFreeableInfo},
		"beadOwnsPoolSessionName":             {beadOwnsPoolSessionName, infoOwnsPoolSessionName},
		"sessionHasProviderTerminalError":     {sessionHasProviderTerminalError, sessionHasProviderTerminalErrorInfo},
		"poolSessionConsumesNewDemand":        {poolSessionConsumesNewDemand, poolSessionConsumesNewDemandInfo},
		"scaleCheckPartialSessionRetainable":  {scaleCheckPartialSessionRetainable, scaleCheckPartialSessionRetainableInfo},
		"scaleCheckPartialSessionPreservable": {scaleCheckPartialSessionPreservable, scaleCheckPartialSessionPreservableInfo},
		"isDrainAckStopPending":               {isDrainAckStopPending, isDrainAckStopPendingInfo},
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
		// A non-canonical-singleton agent exercises the identical
		// UsesCanonicalSingletonPoolIdentity() short-circuit (both forms → false)
		// on every shape.
		"staleNonExpandingPoolSessionBead": {
			func(b beads.Bead) bool { return staleNonExpandingPoolSessionBead(agentFixture, b) },
			func(i session.Info) bool { return staleNonExpandingPoolSessionBeadInfo(agentFixture, i) },
		},
	}

	// singletonAgent is a canonical-singleton pool agent (max=1, no namepool);
	// UsesCanonicalSingletonPoolIdentity() returns true for it, so it drives the
	// non-short-circuit branches of staleNonExpandingPoolSessionBead: the
	// agent_name/label/alias/title identity-slot matches, the pool_slot fallback,
	// and the manual-session exclusion.
	singletonAgent := &config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	singletonAgentBoolChecks := map[string]struct {
		bead func(beads.Bead) bool
		info func(session.Info) bool
	}{
		"staleNonExpandingPoolSessionBead[singleton]": {
			func(b beads.Bead) bool { return staleNonExpandingPoolSessionBead(singletonAgent, b) },
			func(i session.Info) bool { return staleNonExpandingPoolSessionBeadInfo(singletonAgent, i) },
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
		"sessionOrigin":                {sessionOrigin, sessionOriginInfo},
		"sessionMetadataState":         {sessionMetadataState, sessionMetadataStateInfo},
		"sessionBeadStoredTemplate":    {sessionBeadStoredTemplate, sessionBeadStoredTemplateInfo},
		"sessionBeadAgentName":         {sessionBeadAgentName, sessionBeadAgentNameInfo},
		"namedSessionIdentity":         {namedSessionIdentity, namedSessionIdentityInfo},
		"stampedPoolQualifiedIdentity": {stampedPoolQualifiedIdentity, stampedPoolQualifiedIdentityInfo},
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
		// lifecycleTimerBlocker feeds the max-age / idle-timeout blocker fact in
		// the reconciler forward pass; its Info sibling reads Info.HeldUntil /
		// Info.QuarantinedUntil (clk captured for the metadataTimeInFuture rule).
		"lifecycleTimerBlocker": {
			func(b beads.Bead) string { return lifecycleTimerBlocker(b.Metadata, clk.Now()) },
			func(i session.Info) string { return lifecycleTimerBlockerInfo(i, clk.Now()) },
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

	intChecks := map[string]struct {
		bead func(beads.Bead) int
		info func(session.Info) int
	}{
		"sessionWakeAttempts": {sessionWakeAttempts, sessionWakeAttemptsInfo},
	}

	sliceChecks := map[string]struct {
		bead func(beads.Bead) []string
		info func(session.Info) []string
	}{
		"sessionBeadAssigneeIdentities": {sessionBeadAssigneeIdentities, sessionBeadAssigneeIdentitiesInfo},
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

	const leaseStartupTimeout = 90 * time.Second
	// leaseCfg resolves template "worker" to a live (non-suspended) agent so
	// pendingCreateSessionStillLeased's agent-resolved tail (`return !agent.Suspended`)
	// is exercised on the worker-template fixtures rather than only the nil-agent
	// fallthrough. Both forms take the same cfg, so byte-identity is preserved.
	leaseCfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	clkBoolChecks := map[string]struct {
		bead func(beads.Bead) bool
		info func(session.Info) bool
	}{
		"staleCreatingState": {
			func(b beads.Bead) bool { return staleCreatingState(b, clk) },
			func(i session.Info) bool { return staleCreatingStateInfo(i, clk) },
		},
		"sessionStartRequested": {
			func(b beads.Bead) bool { return sessionStartRequested(b, clk) },
			func(i session.Info) bool { return sessionStartRequestedInfo(i, clk) },
		},
		"pendingCreateSessionStillLeased": {
			func(b beads.Bead) bool { return pendingCreateSessionStillLeased(b, leaseCfg, clk) },
			func(i session.Info) bool { return pendingCreateSessionStillLeasedInfo(i, leaseCfg, clk) },
		},
		"sessionIsQuarantined": {
			func(b beads.Bead) bool { return sessionIsQuarantined(b, clk) },
			func(i session.Info) bool { return sessionIsQuarantinedInfo(i, clk) },
		},
		"pendingCreateAttemptStale": {
			func(b beads.Bead) bool { return pendingCreateAttemptStale(b, clk) },
			func(i session.Info) bool { return pendingCreateAttemptStaleInfo(i, clk) },
		},
		"pendingCreateNeverStartedLeaseExpired": {
			func(b beads.Bead) bool { return pendingCreateNeverStartedLeaseExpired(b, clk) },
			func(i session.Info) bool { return pendingCreateNeverStartedLeaseExpiredInfo(i, clk) },
		},
		"pendingCreateStartInFlight": {
			func(b beads.Bead) bool { return pendingCreateStartInFlight(b, clk, leaseStartupTimeout) },
			func(i session.Info) bool { return pendingCreateStartInFlightInfo(i, clk, leaseStartupTimeout) },
		},
		"pendingCreateLeaseActive": {
			func(b beads.Bead) bool { return pendingCreateLeaseActive(b, clk, leaseStartupTimeout) },
			func(i session.Info) bool { return pendingCreateLeaseActiveInfo(i, clk, leaseStartupTimeout) },
		},
		"pendingCreateClaimStillLeasedForSweep": {
			func(b beads.Bead) bool { return pendingCreateClaimStillLeasedForSweep(b, leaseStartupTimeout) },
			func(i session.Info) bool { return pendingCreateClaimStillLeasedForSweepInfo(i, leaseStartupTimeout) },
		},
		"pendingCreateNeverStartedExpired": {
			func(b beads.Bead) bool { return pendingCreateNeverStartedExpired(b, clk) },
			func(i session.Info) bool { return pendingCreateNeverStartedExpiredInfo(i, clk) },
		},
		"pendingCreateLeaseExpiredForRollback": {
			func(b beads.Bead) bool { return pendingCreateLeaseExpiredForRollback(b, clk, leaseStartupTimeout) },
			func(i session.Info) bool {
				return pendingCreateLeaseExpiredForRollbackInfo(i, clk, leaseStartupTimeout)
			},
		},
		"pendingResumePreservingNamedRestart": {
			func(b beads.Bead) bool { return pendingResumePreservingNamedRestart(b, clk, leaseStartupTimeout) },
			func(i session.Info) bool {
				return pendingResumePreservingNamedRestartInfo(i, clk, leaseStartupTimeout)
			},
		},
	}

	// The "pending-resume-preserve" fixture must hit the true branch under
	// leaseStartupTimeout so the equivalence case above is a real true-branch
	// comparison (exercising the Info.StartedConfigHash gate + the lease tail),
	// not a trivial both-false pass.
	if !pendingResumePreservingNamedRestart(beadsByShape["pending-resume-preserve"], clk, leaseStartupTimeout) {
		t.Fatal("pendingResumePreservingNamedRestart(pending-resume-preserve) = false; fixture no longer exercises the resume-preserve true branch")
	}
	// The drain-ack fixture must hit the true branch so isDrainAckStopPending's
	// equivalence case is a real comparison, not a trivial both-false pass.
	if !isDrainAckStopPending(beadsByShape["drain-ack-stop-pending"]) {
		t.Fatal("isDrainAckStopPending(drain-ack-stop-pending) = false; fixture no longer exercises the true branch")
	}
	// The hold/quarantine fixture must drive lifecycleTimerBlocker's non-empty
	// branch so its equivalence case is a real comparison, not a both-empty pass.
	if lifecycleTimerBlocker(beadsByShape["hold-and-quarantine"].Metadata, clk.Now()) == "" {
		t.Fatal(`lifecycleTimerBlocker(hold-and-quarantine) = ""; fixture no longer exercises the blocker branch`)
	}

	for shape, b := range beadsByShape {
		b := b
		info := session.InfoFromPersistedBead(b)
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
			for name, c := range singletonAgentBoolChecks {
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
			for name, c := range intChecks {
				if got, want := c.info(info), c.bead(b); got != want {
					t.Errorf("%s: info=%d bead=%d", name, got, want)
				}
			}
			for name, c := range sliceChecks {
				if got, want := c.info(info), c.bead(b); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: info=%v bead=%v", name, got, want)
				}
			}
			// resetPendingCommittedAt returns a (raw, parsed-time, pending) tuple,
			// so it can't ride the scalar check maps — compare all three fields.
			rawS, rawT, rawOK := resetPendingCommittedAt(b)
			infoS, infoT, infoOK := resetPendingCommittedAtInfo(info)
			if rawS != infoS || !rawT.Equal(infoT) || rawOK != infoOK {
				t.Errorf("resetPendingCommittedAt: info=(%q,%v,%v) bead=(%q,%v,%v)", infoS, infoT, infoOK, rawS, rawT, rawOK)
			}
		})
	}
}
