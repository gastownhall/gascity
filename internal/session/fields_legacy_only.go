package session

import (
	"maps"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// pendingTriage is the marker for a key whose v2 answer waits on the
// architect's per-item ruling (ga-a8dp0uk8).
const pendingTriage = "ruling-pending:ga-a8dp0uk8: awaiting the architect's per-item ruling: a v2 writer, a ledger row or an accepted reason"

// legacyOnlyMarkers names, for each registry key that only the legacy session
// reconciler pass writes, where the v2 reconciler answers it: a ledger row
// (cmd/gc parityLegacyOnly or parityFindings), a latch refusal, an accepted
// reason, or a ruling still pending on a bead. A key with a pending v2 site
// carries that site on its Fields row instead and is not listed here.
var legacyOnlyMarkers = buildLegacyOnlyMarkers()

func buildLegacyOnlyMarkers() map[string]string {
	m := map[string]string{}
	mark := func(marker string, keys ...string) {
		for _, k := range keys {
			m[k] = marker
		}
	}
	mark("ledger:sleep-policy-keys",
		"requested_sleep_after_idle", "effective_sleep_after_idle", "sleep_policy_source",
		"sleep_capability", "sleep_policy_adjustment_reason", "config_wake_suppressed")
	mark("ledger:unknown-state-diagnostic",
		"unknown_state_first_seen", "unknown_state_value", "unknown_state_escalated_at")
	mark("ledger:PAR-FORCEKILL",
		"drain_escalation_at", "drain_escalation_count", "drain_escalation_drain")
	mark("accepted: PAR-BRK: the identity circuit breaker is accepted and ignored by v2 (v2IgnoredSettings in cmd/gc/reconcile_latch.go), which logs the deprecation at boot; legacy keeps honoring it",
		"session_circuit_restarts", "session_circuit_opened_at", "session_circuit_open_restart_count",
		"session_circuit_state", "session_circuit_reset_generation", "session_circuit_last_restart",
		"session_circuit_last_progress", "session_circuit_last_observed", "session_circuit_progress_signature")
	mark(pendingTriage,
		"slept_at", "reset_committed_at", "stranded_event_emitted_at", beadmeta.InfraMigratedFromMetadataKey,
		"mcp_identity", "mcp_servers_snapshot", "provider_terminal_error", "provider_terminal_error_at",
		"session_health", "session_health_reason", "session_drainable",
		"drain_reminder_at", "drain_reminder_count", "drain_reminder_drain", "drain_reminder_failed",
		"drain_reminder_settled", "drain_reminder_hold",
		"awake_started_at", "runtime_thread_id", "runtime_project_id",
		"usage_compute_emitted_at", "usage_model_swept_at", "usage_close_pending", "usage_closed_at",
		"usage_closed_awake_started_at", "usage_closed_session_key", "runtime_lease_epoch")
	return m
}

// LegacyOnlyMarkers returns a copy of the legacy-only key markers.
func LegacyOnlyMarkers() map[string]string { return maps.Clone(legacyOnlyMarkers) }
