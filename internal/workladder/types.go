// Package workladder is the work-progress ladder's pure core: one ladder per
// seat, keyed by (claim, generation), that paces nudges for outstanding work
// with a growing backoff that never reaches zero, recycles a started claim at
// most MaxRecycles times, and releases an unstarted claim only through a
// release the caller re-proves by CAS (PHASE1-2-EPOCH-LIVENESS-LADDER §6).
//
// The package does no I/O and reads no clock: Decide is a function of its
// inputs (WL7). The caller observes the seat, calls Decide, commits the
// returned Record by CAS, and only then performs the returned actions.
package workladder

import (
	"cmp"
	"strings"
	"time"
)

// ClaimKey names one piece of work: the store ref it lives in and its bead.
// The caller normalizes Store the way the claim stores are named
// (normalizeIdleClaimStoreRef) so the agent's and the controller's keys
// compare equal.
type ClaimKey struct{ Store, Bead string }

func (k ClaimKey) compare(o ClaimKey) int {
	return cmp.Or(strings.Compare(k.Store, o.Store), strings.Compare(k.Bead, o.Bead))
}

// Kind is what makes a piece of work outstanding for a seat (§6.2).
type Kind uint8

// Kinds.
const (
	KindTrigger      Kind = iota + 1 // an open, unclaimed trigger bead routed to the seat
	KindContinuation                 // one ready graph.v2 successor pinned to the seat
	KindClaim                        // an in_progress bead the seat holds
)

// Rung is how far an entry has come; it only rises while the entry lives.
type Rung uint8

// Rungs, in order.
const (
	RungAssigned  Rung = iota + 1 // routed to the seat, not claimed
	RungClaimed                   // claimed, not proven handed to a turn
	RungStarted                   // the hook handed it to a turn (or a seed says so)
	RungEscalated                 // started, recycled MaxRecycles times, still unanswered
)

// StartSource is the evidence that names a claim started.
type StartSource uint8

// Start sources. Only SrcAgent survives the ladder's first observation of a
// generation; the rest are seeds (§6.7).
const (
	SrcAgent                 StartSource = iota + 1 // the agent's work_started cell
	SrcExecutionClaimStarted                        // execution_claim_started == "<token>|<bead>"
	SrcCurrentClaim                                 // current_claim_bead_id
	SrcCurrentlyProcessing                          // currently_processing_bead_id
	SrcClaimSession                                 // the work bead's gc.session_id is the seat: the hook stamped it at hand-over
)

// Work is one outstanding piece of work for the seat.
type Work struct {
	Key  ClaimKey
	Kind Kind
}

// Tri is a three-valued reading.
type Tri uint8

// Tri values. TriUnknown is the zero value and is never treated as true.
const (
	TriUnknown Tri = iota
	TriTrue
	TriFalse
)

// QuietState is whether the seat is idle enough to nudge.
type QuietState uint8

// Quiet states. QuietUnknown is the zero value and reads as not quiet.
const (
	QuietUnknown QuietState = iota
	Quiescent
	NotQuiet
)

// Quiet is the seat's quiet reading and, when not quiet, why.
type Quiet struct {
	State  QuietState
	Reason string
}

// Gate is open, or held by something that owns the seat for now (draining,
// stop requested, kill pending, a user or wait hold, restart pending,
// starting, a heartbeat hold).
type Gate struct {
	Held   bool
	Reason string
}

// ProviderState is the seat's provider health.
type ProviderState struct {
	Degraded bool
	Reason   string
}

// Observation is everything Decide reads about one seat, at one instant.
type Observation struct {
	Now         time.Time
	Gen         int64 // the row's generation
	Outstanding []Work
	// Started is the agent cell's claims, plus the seeds when the ladder
	// cell is absent or names another generation (§6.7).
	Started map[ClaimKey]StartSource
	// StartedRead is false when the agent cell is unreadable: then every
	// outstanding claim is treated as started (fail safe).
	StartedRead bool
	// Hold is work whose live re-read could not tell this pass: it is kept,
	// with its count, and nothing is done about it until a read answers.
	Hold         map[ClaimKey]bool
	Runtime      Tri // the seat's runtime is alive
	Quiet        Quiet
	LastActivity time.Time
	Gate         Gate
	Provider     ProviderState
	// RateLimited is how many of the seat's latest turns, newest first, its
	// provider refused with a rate limit (as the agent's own transcript
	// classifies them: HTTP 429), counted up to the first it did not refuse,
	// in the seat's current conversation, and RateLimitedAt when the newest
	// of them ended. A seat refused like that cannot answer a nudge (the
	// rate-limit rule); the rule acts only on a refusal that ended after the
	// ladder's own last nudge, so refusals already in a transcript when the
	// ladder first sees it recycle nothing (WL8).
	RateLimited   int
	RateLimitedAt time.Time
	// ServedAt is when the seat's newest completed turn ended: the provider
	// answered it, positive evidence of service. Zero when its newest ended
	// turn did not complete, or its turns were not read.
	ServedAt time.Time
	// FreshHold holds the rate-limit rule's fresh recycle back, by the
	// caller's evidence, and says how the claims it holds are paced.
	FreshHold FreshHold
}

// FreshHold is why the rate-limit rule's fresh recycle of a seat must wait.
type FreshHold uint8

// The fresh holds.
const (
	// FreshHoldNone: the fresh recycle may go ahead.
	FreshHoldNone FreshHold = iota
	// FreshHoldQueued: the seat waits its turn, a wait that ends on its own
	// (another seat is the provider's fresh-recycle canary, or the provider
	// refused a canary's fresh conversation, for a bounded time). Meanwhile
	// each started claim the rule would recycle is paced as on a degraded
	// provider, counting nothing, so it keeps its one recycle for the fresh
	// conversation rather than spend it on a drain that resumes the refused
	// one.
	FreshHoldQueued
	// FreshHoldStanding: the seat must not be restarted fresh, by a hold with
	// no end of its own (a person is, or may be, attached; it waits on a
	// person for another claim). Its claims are paced as ever, so its silence
	// still reaches the drain and the alerts.
	FreshHoldStanding
)

// Policy is the ladder's pacing. DefaultPolicy is OD7's.
type Policy struct {
	Grace         time.Duration // first nudge, after an entry appears
	Base          time.Duration // the backoff after the first nudge; doubles
	Cap           time.Duration // the longest backoff; the interval forever after
	PausedCap     time.Duration // the longest backoff while the provider is degraded
	ProgressReset time.Duration // activity this long after the last nudge restarts the count
	RecycleRepost time.Duration // a trigger's recycle request is reposted no sooner
	Threshold     map[Kind]map[Rung]int
	MaxRecycles   int // R: recycles of a started claim per seat
	// RateLimitThreshold is the turns in a row the seat's provider refuses
	// with a rate limit before its started claims are recycled to a fresh
	// conversation (within MaxRecycles), or, past it, alerted on.
	RateLimitThreshold int
}

// Action is one effect Decide asks the caller for.
type Action interface{ isAction() }

// Nudge asks for one nudge of the seat about Key. Attempt is the entry's
// attempts after this one; Paused marks a nudge sent while the provider is
// degraded, which does not count toward escalation.
type Nudge struct {
	Key     ClaimKey
	Kind    Kind
	Rung    Rung
	Attempt int
	Paused  bool
}

// Recycle asks for the seat to be recycled through the planner's inbox.
// KeepClaim keeps a started claim on the seat across the restart. Fresh
// restarts the seat in place with fresh provider conversation state (gc
// session reset): the seat, its claims and its pins are kept, the
// conversation is not.
type Recycle struct {
	Key       ClaimKey
	KeepClaim bool
	Fresh     bool
}

// Release asks the caller to release an unstarted claim, after re-proving by
// CAS that it is unstarted (§6.5).
type Release struct{ Key ClaimKey }

// Alert is a condition a human should see. Decide emits the escalations
// at most once per entry, and one recycle alert per recycle.
type Alert struct {
	Key    ClaimKey
	Reason string
}

func (Nudge) isAction()   {}
func (Recycle) isAction() {}
func (Release) isAction() {}
func (Alert) isAction()   {}

// Alert reasons.
const (
	AlertContinuationUnanswered = "continuation_unanswered"
	AlertStartedUnanswered      = "started_unanswered"
	AlertStartedRecycled        = "started_recycled"
	AlertReleaseUnfinished      = "release_unfinished"
	AlertReleaseRacedStart      = "release_raced_start"
	AlertNudgeRefused           = "nudge_refused"
	// AlertRateLimitedRecycled: the seat's provider refused its latest turns
	// with a rate limit, so the claim's seat was recycled to a fresh
	// conversation.
	AlertRateLimitedRecycled = "rate_limited_recycled"
	// AlertProviderRateLimited: the provider refused the claim's fresh seat
	// too. The pool itself is refusing, and a human must reroute the lane.
	AlertProviderRateLimited = "provider_rate_limited"
	// AlertRateLimitedUnrecycled: the provider refuses the claim's seat with
	// a rate limit, and the claim's one recycle already went to a drain that
	// resumed the same conversation. A human must reset the seat or reroute
	// the lane.
	AlertRateLimitedUnrecycled = "rate_limited_unrecycled"
	// AlertRateLimitedAgain: the provider refuses the claim's seat with a
	// rate limit again after its one recycle gave it a fresh conversation
	// the provider served, or long (more than Cap) after that recycle: no
	// evidence about the provider's pool, only that this claim is stuck
	// again. A human must reset the seat or reroute.
	AlertRateLimitedAgain = "rate_limited_again"
)

// Decision is Decide's answer: the cell to CAS (no write when !Changed) and
// the actions to take once it lands.
type Decision struct {
	Next    Record
	Changed bool
	Actions []Action
}

// Record is the work_ladder cell. V == 0 is an absent cell.
type Record struct {
	V       int
	Gen     int64
	Entries []Entry
	Release *ReleaseMark
	// LastNudge is the seat's last nudge, about any of its work: the seat
	// is nudged at most once per Base (WL3), whatever work comes and goes.
	LastNudge time.Time
	// RecycledGen is the generation the ladder last asked to recycle the
	// seat at: the stop that recycle brings is the ladder's, whatever the
	// switch says by then.
	RecycledGen int64
}

// Entry is one outstanding piece of work's pacing.
type Entry struct {
	Key       ClaimKey
	Kind      Kind
	Rung      Rung
	Since     time.Time // the rung's (or the generation's) start
	Attempts  int       // counted nudges on this rung, this generation
	Paused    int       // nudges sent while the provider was degraded
	Last      time.Time // the last nudge or recycle
	Next      time.Time // when the entry is next due if the seat stays quiet
	Recycles  int       // across generations
	Escalated bool      // the entry's alert was raised (cleared on a rung change)
	Seed      StartSource
	RecycleAt time.Time
	// Unreleased: a release of this unstarted claim ran to an outcome that
	// did not release it (kept, or failed). It is then nudged at Cap, with
	// one alert, and the release retried at Cap.
	Unreleased bool
	// Refused counts the nudges the seat could not take in a row (a
	// permission prompt, a runtime unavailable): while it is above zero the
	// entry is nudged no more often than PausedCap.
	Refused int
	// RateLimitAlerted: a rate-limit alert (provider_rate_limited or
	// rate_limited_unrecycled) was raised for the entry; one is raised in
	// the entry's life.
	RateLimitAlerted bool
	// FreshRecycled: the entry's last recycle restarted its seat to a fresh
	// conversation (the rate-limit rule's), not a drain that resumes it.
	FreshRecycled bool
	// FreshServed: the provider served that fresh conversation (a turn of
	// the seat completed after the recycle). A refusal of it afterwards says
	// nothing about the provider refusing fresh conversations.
	FreshServed bool
}

// ReleaseMark is a release of an unstarted claim in flight. While it names a
// key, the agent's hook refuses to hand that key out (§6.4).
type ReleaseMark struct {
	Key ClaimKey
	Gen int64
	At  time.Time
}
