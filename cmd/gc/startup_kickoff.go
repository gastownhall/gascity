package main

// Metadata keys for the named/direct startup kickoff contract. These are
// seeded by syncSessionBeads at the same lifecycle point as named_session /
// session_origin (see the isConfiguredNamed block), and later read/written
// by nudgeOutstandingStartupKickoffs (startup_kickoff_nudge.go) to decide
// whether a named session's first turn is still outstanding. Deliberately
// unprefixed, matching the sibling idle_claim_nudge_* keys rather than the
// beadmeta "gc." convention.
const (
	startupKickoffStateKey       = "startup_kickoff_state"
	startupKickoffStartedAtKey   = "startup_kickoff_started_at"
	startupKickoffAttemptsKey    = "startup_kickoff_attempts"
	startupKickoffLastNudgeAtKey = "startup_kickoff_last_nudge_at"
)

// Values for startupKickoffStateKey.
const (
	startupKickoffStatePending   = "pending"
	startupKickoffStateNudged    = "nudged"
	startupKickoffStateConfirmed = "confirmed"
	startupKickoffStateGivenUp   = "given_up"
)
