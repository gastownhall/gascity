package session

import (
	"strings"
	"time"
)

// Exit classification for dead sessions, extracted from the reconciler's
// stability/churn pair. Pure decision ladder over caller-gathered facts:
// the reconciler owns the drain tracker, provider screen peeks, store
// writes, and counters; this package owns lane precedence and the band
// arithmetic.
//
// Lanes, in order:
//
//  1. rate-limit: a crash candidate whose provider screen shows a rate-limit
//     message is quarantined without counting a crash.
//  2. rapid crash: a crash candidate that died inside the stability window.
//  3. churn band: a death that survived the stability window but did not
//     reach the productivity threshold (context-exhaustion death spiral).
//     Deaths at or past the productivity threshold clear the churn counter.
//
// The rapid-crash lanes intentionally ignore pending_create_claim and
// sleep_reason; the churn lane respects both. That asymmetry is inherited
// reconciler behavior: an expired-lease pending create must retry its wake
// (not accrue churn), while a genuine rapid crash counts even when a stale
// claim is still set.

// ScreenFact is the tri-state provider-screen fact. It is Unknown until the
// caller has peeked the session's terminal content for a rate-limit screen.
type ScreenFact int

// Provider-screen fact states.
const (
	ScreenUnknown ScreenFact = iota
	ScreenRateLimit
	ScreenOther
)

// ExitOutcome is the classification of one dead session on one tick.
type ExitOutcome int

const (
	// ExitNone means nothing to record for this session.
	ExitNone ExitOutcome = iota
	// ExitGatherScreen means supply ExitFacts.Screen and decide again.
	ExitGatherScreen
	// ExitRateLimitQuarantine means back the session off with a rate-limit
	// quarantine instead of counting a crash.
	ExitRateLimitQuarantine
	// ExitRapidCrash means record a wake failure and clear the wake stamp.
	ExitRapidCrash
	// ExitChurn means record a churn event and clear the wake stamp.
	ExitChurn
	// ExitProductiveDeath means the session died after doing useful work;
	// clear any accrued churn count and continue processing the session.
	ExitProductiveDeath
)

// ExitFacts are the inputs for classifying one dead session's exit.
type ExitFacts struct {
	// Alive short-circuits the ladder; live sessions are never classified.
	Alive bool
	// SubprocessProvider reports the city runs the subprocess session
	// provider, whose workers exit intentionally after a unit of work.
	SubprocessProvider bool
	// DrainPending reports an intentional drain is tracked for the session.
	DrainPending bool
	// PendingCreateClaim reports pending_create_claim=true on the bead.
	// Suppresses churn only; see the package comment for the asymmetry.
	PendingCreateClaim bool
	// PendingCreateStartInFlight reports the create lease is still live, so
	// the death is startup noise rather than a crash.
	PendingCreateStartInFlight bool
	// SleepReason is the bead's sleep_reason metadata. Deliberate reasons
	// suppress churn only.
	SleepReason string
	// LastWokeAt is the raw last_woke_at metadata (RFC3339). Empty or
	// unparseable values disable exit tracking entirely.
	LastWokeAt string
	// Now is the tick clock reading.
	Now time.Time
	// StabilityThreshold bounds the rapid-crash window; deaths inside it
	// count as wake failures.
	StabilityThreshold time.Duration
	// ProductivityThreshold bounds the churn band; deaths at or past it are
	// productive.
	ProductivityThreshold time.Duration
	// ScreenAvailable reports the caller can peek the provider screen.
	// Without it the rate-limit lane is skipped.
	ScreenAvailable bool
	// Screen is the provider-screen fact, gathered on demand.
	Screen ScreenFact
}

// DecideSessionExit classifies a dead session's exit. It performs no I/O;
// when the rate-limit lane needs the provider screen it returns
// ExitGatherScreen and the caller supplies the fact and decides again.
func DecideSessionExit(f ExitFacts) ExitOutcome {
	if f.Alive {
		return ExitNone
	}
	woke, wokeValid := parseWakeStamp(f.LastWokeAt)
	crashCandidate := !f.SubprocessProvider && !f.DrainPending && wokeValid &&
		!f.PendingCreateStartInFlight
	if crashCandidate && f.ScreenAvailable {
		switch f.Screen {
		case ScreenUnknown:
			return ExitGatherScreen
		case ScreenRateLimit:
			return ExitRateLimitQuarantine
		}
	}
	if crashCandidate && f.Now.Sub(woke) < f.StabilityThreshold {
		return ExitRapidCrash
	}
	if f.PendingCreateClaim || f.SubprocessProvider || f.DrainPending ||
		IsDeliberateSleepReason(f.SleepReason) || !wokeValid {
		return ExitNone
	}
	elapsed := f.Now.Sub(woke)
	if elapsed < f.StabilityThreshold {
		return ExitNone
	}
	if elapsed >= f.ProductivityThreshold {
		return ExitProductiveDeath
	}
	return ExitChurn
}

// IsDeliberateSleepReason reports whether a sleep_reason records an
// intentional stop rather than a crash, so the death must not accrue churn.
// "city-stop" mirrors the CLI's stop sleep reason.
func IsDeliberateSleepReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "idle", "idle-timeout", "no-wake-reason", "config-drift", "drained",
		"city-stop", "user-hold", "wait-hold", "rate_limit", "failed-create":
		return true
	default:
		return false
	}
}

func parseWakeStamp(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
