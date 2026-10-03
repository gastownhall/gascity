package session

import "strings"

// SleepReason is the typed vocabulary for the sleep_reason bead-metadata
// marker. Before this type the ~fifteen sleep-reason strings were matched as
// raw literals across four independent classifier case lists (churn
// suppression, continuation reset, display reason, wake-blocker clearing) plus
// scattered writers, so a misspelled writer ("rate-limit" vs "rate_limit")
// silently escaped every classifier. Routing both the writers and the
// classifiers through these constants makes such a typo a compile error.
//
// The on-store string values are unchanged; SleepReason is a thin string alias
// so a raw metadata value converts with SleepReason(value) at the read edge.
type SleepReason string

// Sleep-reason values written to and read from the sleep_reason metadata key.
// SleepReasonRuntimeMissing shares its string with LifecycleReasonRuntimeMissing
// (the display-reason surface of the same posture); it is defined from that
// constant so the two never drift.
const (
	SleepReasonIdle                  SleepReason = "idle"
	SleepReasonIdleTimeout           SleepReason = "idle-timeout"
	SleepReasonNoWakeReason          SleepReason = "no-wake-reason"
	SleepReasonConfigDrift           SleepReason = "config-drift"
	SleepReasonDrained               SleepReason = "drained"
	SleepReasonCityStop              SleepReason = "city-stop"
	SleepReasonUserHold              SleepReason = "user-hold"
	SleepReasonWaitHold              SleepReason = "wait-hold"
	SleepReasonRateLimit             SleepReason = "rate_limit"
	SleepReasonFailedCreate          SleepReason = "failed-create"
	SleepReasonProviderTerminalError SleepReason = "provider-terminal-error"
	SleepReasonRuntimeMissing        SleepReason = SleepReason(LifecycleReasonRuntimeMissing)
	SleepReasonQuarantine            SleepReason = "quarantine"
	SleepReasonContextChurn          SleepReason = "context-churn"
	SleepReasonMaxSessionAge         SleepReason = "max-session-age"
	SleepReasonAssignedWorkExhausted SleepReason = "assigned-work-exhausted"
	SleepReasonSuspended             SleepReason = "suspended"
	// SleepReasonStartupCrash records that the provider pane died during
	// Start/wake rather than a deliberate idle stop. It is intentionally
	// absent from IsDeliberateSleepReason so a real crash still accrues
	// churn instead of being classified as idle-timeout.
	SleepReasonStartupCrash SleepReason = "startup-crash"
)

// IsDeliberateSleepReason reports whether a sleep_reason records an
// intentional stop rather than a crash, so the death must not accrue churn.
// "city-stop" mirrors the CLI's stop sleep reason.
// "suspended" is a rig-suspend drain (gc rig suspend): an explicit operator
// stop, distinct from idle / assigned-work-exhausted so operators can see
// that suspend actually quiesced the session.
// "provider-terminal-error" is a classified, non-retryable provider failure
// (set by markProviderTerminalError); it is suppressed here so a session
// already parked terminal cannot also accrue a spurious wake failure, making
// the invariant explicit rather than relying on last_woke_at being cleared in
// the same metadata batch.
// The reason list deliberately diverges from shouldResetContinuation's
// near-identical list (this one has "failed-create" and lacks
// "runtime-missing"): that one decides continuation reset on wake, this one
// decides churn suppression — do not merge the lists.
func IsDeliberateSleepReason(reason string) bool {
	switch SleepReason(strings.TrimSpace(reason)) {
	case SleepReasonIdle, SleepReasonIdleTimeout, SleepReasonNoWakeReason,
		SleepReasonConfigDrift, SleepReasonDrained, SleepReasonCityStop,
		SleepReasonUserHold, SleepReasonWaitHold, SleepReasonRateLimit,
		SleepReasonFailedCreate, SleepReasonProviderTerminalError,
		SleepReasonSuspended:
		return true
	default:
		return false
	}
}
