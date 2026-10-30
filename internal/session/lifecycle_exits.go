package session

import (
	"maps"
	"strconv"
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
// claim is still set. The churn lane also skips a start that never committed:
// no runtime of that incarnation was confirmed running, so its absence is a
// start in flight, or one a crashed controller abandoned, which the
// reconciler relaunches (CONTRACT S7; mc-5a1ma).

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
	// StartUncommitted reports the row carries pending_create_started_at,
	// which PreWake stamps and the start's commit clears: the start has not
	// committed. Suppresses churn only.
	StartUncommitted bool
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
	if f.PendingCreateClaim || f.StartUncommitted || f.SubprocessProvider || f.DrainPending ||
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

// ExitAccrual is the built write for one more crash or churn event plus the
// threshold verdict. Quarantined accruals carry the full quarantine patch;
// otherwise only the counter advances.
type ExitAccrual struct {
	Patch       MetadataPatch
	Quarantined bool
	// Until is a quarantining accrual's quarantined_until, the Quarantine
	// cell's to write.
	Until time.Time
}

// Class is a's class keys over row info: for a quarantining accrual the
// Quarantine cell's quarantined_until (ExtendQuarantine), else none. It
// mints nothing.
func (a ExitAccrual) Class(info Info, now time.Time) DispositionPatch {
	if !a.Quarantined {
		return DispositionPatch{}
	}
	return ExtendQuarantine(info, a.Until, now)
}

// ExtendQuarantine is the controller's Quarantine cell over row info to
// until, a crash, churn or rate-limit quarantine's: it never shortens a
// quarantine already in force (an operator's longer one), writing the later
// of the two deadlines (the #7706 review).
func ExtendQuarantine(info Info, until, now time.Time) DispositionPatch {
	if live := DispositionOfInfo(info).Quarantine; live.After(now) && live.After(until) {
		until = live
	}
	return CellInfo(info, ActorController, DispEvent{Kind: EvQuarantine, Until: until}, now)
}

// Over is a's patch over row info: a quarantining accrual during a live
// quarantine keeps the live quarantine's reason (KeepQuarantineReason;
// CONTRACT v5.9z), as it keeps its later deadline (Class). Legacy's
// recordWakeFailure and recordChurn and v2's accruals all write it.
func (a ExitAccrual) Over(info Info, now time.Time) MetadataPatch {
	return KeepQuarantineReason(maps.Clone(a.Patch), DispositionOfInfo(info), info.SleepReason, now)
}

// PatchFor is a's write to row info: its patch over info (Over) with its
// Class.
func (a ExitAccrual) PatchFor(info Info, now time.Time) MetadataPatch {
	return a.Class(info, now).Into(info.ID, a.Over(info, now))
}

// WakeFailureAccrualPatch builds the write for one more rapid-crash wake
// failure on top of priorAttempts. Reaching maxAttempts quarantines the
// session until the given time with sleep reason "quarantine". The patch is
// metadata-only; unlike QuarantinePatch it does not move the state machine.
func WakeFailureAccrualPatch(priorAttempts, maxAttempts int, until time.Time) ExitAccrual {
	return exitAccrual("wake_attempts", priorAttempts+1, maxAttempts, string(SleepReasonQuarantine), until)
}

// ChurnAccrualPatch builds the write for one more churn cycle on top of
// priorCycles. Reaching maxCycles quarantines the session until the given
// time with sleep reason "context-churn".
func ChurnAccrualPatch(priorCycles, maxCycles int, until time.Time) ExitAccrual {
	return exitAccrual("churn_count", priorCycles+1, maxCycles, string(SleepReasonContextChurn), until)
}

func exitAccrual(counterKey string, next, threshold int, sleepReason string, until time.Time) ExitAccrual {
	count := strconv.Itoa(next)
	if next >= threshold {
		return ExitAccrual{
			Quarantined: true,
			Until:       until,
			Patch: MetadataPatch{
				counterKey:     count,
				"sleep_reason": sleepReason,
			},
		}
	}
	return ExitAccrual{Patch: MetadataPatch{counterKey: count}}
}

// ConversationResetPatch clears the runtime conversation binding so the next
// wake starts a fresh conversation. Wake failures also clear
// started_config_hash so the next start runs as a first start instead of
// resuming a conversation that no longer exists; churn keeps the hash.
func ConversationResetPatch(clearStartedConfigHash bool) MetadataPatch {
	patch := MetadataPatch{
		"session_key":                "",
		"continuation_reset_pending": "true",
	}
	ClearResumeRun(patch) // a new conversation's run starts anew (mc-bm1af)
	if clearStartedConfigHash {
		patch["started_config_hash"] = ""
		// Priming markers share started_config_hash's lifetime (S19 Stage 2): a
		// wake failure re-primes; a churn keeps the hash and its markers.
		clearPrimingMarkers(patch)
	}
	return patch
}

// RateLimitQuarantinePatch is the lifecycle of backing a session off a
// provider rate-limit screen without counting a crash or resetting its
// conversation metadata; its quarantined_until is the Quarantine cell's.
func RateLimitQuarantinePatch() MetadataPatch {
	return MetadataPatch{
		"state":                     string(StateAsleep),
		"sleep_reason":              string(SleepReasonRateLimit),
		"last_woke_at":              "",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}
}
