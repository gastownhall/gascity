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
	// AwaitsHuman: the seat said it waits on a person's decision about this
	// work (the caller reads it off the claim). Its entry is only nudged, a
	// Cap apart, and never recycled or released, and its pacing raises none
	// of AlertStartedRecycled, AlertStartedUnanswered and
	// AlertReleaseUnfinished: no nudge can hurry a person, and one a Cap
	// apart still re-drives a seat whose turn ended after the answer came
	// but before the seat said the wait is over. A nudge the seat cannot
	// take still counts toward AlertNudgeRefused, and a pending release that
	// raced a start still raises AlertReleaseRacedStart.
	AwaitsHuman bool
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
// stop requested, kill pending, an idle stop pending, a user or wait hold,
// restart pending, starting, a heartbeat hold).
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
	// Conv is the row's conversation: its continuation epoch.
	Conv int64
	// Turns is what the seat's own turns said on this pass; the zero value
	// is no reading: progress is then judged on the activity clock, and no
	// rate-limit episode opens or moves on the reading's evidence.
	Turns TurnReading
	// RestartHold is a standing reason the seat must not be restarted fresh
	// now, read by the caller only while RestartDue: a person attached to
	// it, or work of it awaiting a person.
	RestartHold RestartHold
	// ProviderFreshAt is the newest fresh restart (RateLimit.LastFresh) of
	// another governed seat on this seat's provider, read off their saved
	// cells; zero when there is none.
	ProviderFreshAt time.Time
	// Partial: the caller could not list all of the seat's work this pass
	// (a store that did not answer), so Outstanding may lack some of it. A
	// fresh restart drops every claim's conversation, and nothing proves
	// the work it cannot see safe to drop (a claim waiting on a person), so
	// no restart lands while it is set: it blocks the restart as Hold does,
	// and paces nothing differently.
	Partial bool
}

// TurnReading is one read of the seat's turns, reduced to what Decide
// judges progress and rate limits by. A turn the provider failed is not
// progress: when the newest ended turn failed, only a turn the provider
// completed shows the seat at work. A reading of another conversation than
// the observation's is no reading. Nothing in it is read out of an error's
// text: every rate-limit fact is the turn's source's own classification.
type TurnReading struct {
	// Read is true when the turns were read; false is no reading (a family
	// that records no turns, a read that failed, a pass that read none).
	Read bool
	// Conv is the conversation (continuation epoch) the turns were read in.
	Conv int64
	// LastFailed reports that the newest ended turn failed, whatever the
	// cause, and no turn is open.
	LastFailed bool
	// ServedAt is when the newest turn the provider completed ended,
	// anywhere in the turns read; zero when none did.
	ServedAt time.Time
	// Refused is how many of the newest ended turns in a row the provider
	// refused with a rate limit, as their source classified them; 0 while a
	// turn is open (it has no answer yet).
	Refused int
	// RefusedAt is when the newest of those refused turns ended.
	RefusedAt time.Time
	// Quota reports that the newest refused turn's source classed its
	// refusal a spent usage quota (an allowance that comes back only when
	// the account's period resets), not a rate limit a fresh conversation
	// may get past. Such an episode is never restarted fresh: a new
	// conversation draws on the same spent allowance.
	Quota bool
}

// RestartHold is a standing reason not to restart a seat fresh now.
type RestartHold uint8

// Restart holds. RestartHoldNone is the zero value.
const (
	RestartHoldNone          RestartHold = iota
	RestartHoldAttached                  // a person is attached to the seat
	RestartHoldAwaitingHuman             // work of the seat awaits a person
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

	// RateLimitThreshold is how many turns in a row the provider must
	// refuse with a rate limit before the refusals open an episode.
	RateLimitThreshold int
	// FreshConfirm is how long an episode stands, from the refusal that
	// opened it, before it is confirmed (by a refusal of a nudge delivered
	// after it opened) and may take its one fresh restart.
	FreshConfirm time.Duration
	// SeatFreshGap: no second fresh restart of a seat within this of its
	// last; an episode confirmed inside it is exhausted instead.
	SeatFreshGap time.Duration
	// ProviderFreshGap: no fresh restart within this of another seat's on
	// the same provider (Observation.ProviderFreshAt).
	ProviderFreshGap time.Duration
	// RateLimitEvidenceMaxAge: an open episode with no new answered
	// refusal for this long (by the refusal's own end time) closes stale,
	// and an older refusal answers nothing. It must outlast the episode's
	// slowest pace, a wait on a person nudged a Cap apart, with time to read
	// the refusal of the next nudge (the default is Cap + PausedCap): at Cap
	// or less, such a seat's episode closes stale on the very pass that
	// sends each nudge, whenever that pass lands later than the last
	// refusal took, and is never confirmed.
	RateLimitEvidenceMaxAge time.Duration
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
// KeepClaim keeps a started claim on the seat across the restart.
type Recycle struct {
	Key       ClaimKey
	KeepClaim bool
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

// FreshRestart asks for the seat to be restarted in place on a new
// conversation, keeping its claims (the caller writes the restart request in
// the same CAS as the cell). It is the seat's, never a claim's: one per
// rate-limit episode at most.
type FreshRestart struct{}

// RateLimitMove is one move of the seat's rate-limit episode, for the
// caller to report (work.rate_limited).
type RateLimitMove struct {
	Transition RateLimitTransition
	Cause      RateLimitCause
}

func (Nudge) isAction()         {}
func (Recycle) isAction()       {}
func (Release) isAction()       {}
func (Alert) isAction()         {}
func (FreshRestart) isAction()  {}
func (RateLimitMove) isAction() {}

// Alert reasons.
const (
	AlertContinuationUnanswered = "continuation_unanswered"
	AlertStartedUnanswered      = "started_unanswered"
	AlertStartedRecycled        = "started_recycled"
	AlertReleaseUnfinished      = "release_unfinished"
	AlertReleaseRacedStart      = "release_raced_start"
	AlertNudgeRefused           = "nudge_refused"

	// AlertRateLimitedFreshRefused: the seat's fresh restart did not get it
	// served (its new conversation was refused too, or the restart did not
	// take).
	AlertRateLimitedFreshRefused = "rate_limited_fresh_refused"
	// AlertRateLimitedAgain: the seat was refused again within SeatFreshGap
	// of its last fresh restart.
	AlertRateLimitedAgain = "rate_limited_again"
	// AlertRateLimitedHeld: a standing hold, or a block lasting past
	// FreshConfirm + Cap, keeps the ladder from restarting the seat. It is
	// the episode's one alert: how the episode ends after it (a fresh
	// restart refused too, a spent quota) is reported only by its
	// RateLimitMove.
	AlertRateLimitedHeld = "rate_limited_held"
	// AlertRateLimitedQuota: the provider refuses the seat on a spent usage
	// quota, which no fresh restart gets past.
	AlertRateLimitedQuota = "rate_limited_quota"
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
	// RecycledGen is the generation the ladder last asked to recycle (or
	// restart fresh) the seat at: the stop that brings is the ladder's,
	// whatever the switch says by then.
	RecycledGen int64
	// Delivered is when the seat last took a nudge, about any of its work,
	// in this generation: the start of the attempt that delivered it,
	// direct or a notice the caller settled delivered. Zero until one
	// lands; a new generation clears it.
	Delivered time.Time
	// RateLimit is the seat's rate-limit state: its episode, if one is
	// open, and its last fresh restart. The zero value is neither. A new
	// generation keeps it: the episode is about the conversation.
	RateLimit RateLimit
}

// Entry is one outstanding piece of work's pacing.
type Entry struct {
	Key       ClaimKey
	Kind      Kind
	Rung      Rung
	Since     time.Time // the rung's (or the generation's) start
	Attempts  int       // counted nudges on this rung, this generation
	Paused    int       // nudges sent while the provider was degraded
	Last      time.Time // the last nudge or recycle, or the start of a wait on a person
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
	// Notice is the asynchronous delivery of the entry's last nudge, until
	// the caller settles it (SettleNotice); zero when there is none.
	Notice NoticeMark
	// AwaitsHuman: the entry's episode is a wait on a person
	// (Work.AwaitsHuman). The wait's start and its end each begin a new
	// episode, with a fresh count.
	AwaitsHuman bool
}

// ReleaseMark is a release of an unstarted claim in flight. While it names a
// key, the agent's hook refuses to hand that key out (§6.4).
type ReleaseMark struct {
	Key ClaimKey
	Gen int64
	At  time.Time
}
