package main

import (
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// RED-phase placeholders for the named-seat claim-pickup backstop
// (ga-zogqc1.2.8). They exist only so named_claim_pickup_test.go compiles and
// every assertion fails for its own reason instead of an undefined-symbol
// build error hiding which acceptance criteria are unmet. The stubs do
// nothing on purpose: a panic would take every other test in this package
// down with it. GREEN deletes this file and adds the real implementation, a
// backstopPredicate run through runNudgeBackstop, in a non-test file.
const (
	claimPickupStateKey       = "claim_pickup_state"
	claimPickupStartedAtKey   = "claim_pickup_started_at"
	claimPickupAttemptsKey    = "claim_pickup_attempts"
	claimPickupLastNudgeAtKey = "claim_pickup_last_nudge_at"
	claimPickupBeadIDKey      = "claim_pickup_bead_id"
	claimPickupStatePending   = "pending"
	claimPickupStateNudged    = "nudged"
	claimPickupStateGivenUp   = "given_up"
)

type namedClaimPickupBackstop struct{}

func (namedClaimPickupBackstop) governs(beads.Bead) bool { return false }

func nudgeNamedClaimPickups(_ runtime.Provider, _ beads.Store, _, _ []beads.Bead, _ bool, _ time.Time, _ events.Recorder, _ io.Writer) {
}
