package workladder

import "time"

// RateLimit is a seat's rate-limit episode: its provider refusing its turns
// with a rate limit, from the first refusal that answered a delivered nudge
// until the provider serves the seat again (or the evidence goes stale, or
// the conversation changes under it). At most one fresh restart and one
// alert per episode. It lives on the seat's cell, never on an entry, so
// nothing that restarts an entry's count reaches it.
type RateLimit struct {
	Phase    RateLimitPhase // RateLimitClear when no episode is open
	Conv     int64          // the conversation (continuation epoch) it stands in
	Since    time.Time      // when the refusal that opened it ended
	Checked  time.Time      // when the newest answered refusal it read ended
	FreshAt  time.Time      // the episode's fresh restart; zero before it
	FromConv int64          // the conversation that restart left
	Alerted  string         // the episode's one alert reason, once raised
	// LastFresh is the seat's last fresh restart in any episode: the
	// per-seat gap (SeatFreshGap) and the provider gap other seats read
	// (ProviderFreshGap). It outlives the episode and is cleared, once the
	// episode is closed, when it is older than both gaps.
	LastFresh time.Time
}

// RateLimitPhase is where a seat's rate-limit episode stands.
type RateLimitPhase uint8

// Rate-limit phases. Every phase but RateLimitClear paces the seat paused,
// as a degraded provider does (WL4).
const (
	RateLimitClear     RateLimitPhase = iota // no episode
	RateLimitOpen                            // refused; no fresh restart yet
	RateLimitRestarted                       // fresh restart requested; the new conversation's fate unseen
	RateLimitExhausted                       // the ladder will not restart it again this episode
)

// RateLimitTransition names a move of an episode.
type RateLimitTransition string

// Rate-limit transitions.
const (
	RateLimitTransitionOpened    RateLimitTransition = "opened"
	RateLimitTransitionRestarted RateLimitTransition = "restarted"
	RateLimitTransitionHeld      RateLimitTransition = "held"
	RateLimitTransitionExhausted RateLimitTransition = "exhausted"
	RateLimitTransitionClosed    RateLimitTransition = "closed"
)

// RateLimitCause is why an episode moved: held, exhausted and closed moves
// carry one.
type RateLimitCause string

// Rate-limit causes.
const (
	RateLimitCauseAttached            RateLimitCause = "attached"             // held: a person is attached
	RateLimitCauseAwaitingHuman       RateLimitCause = "awaiting_human"       // held: work awaits a person
	RateLimitCauseBlocked             RateLimitCause = "blocked"              // held: the provider gap, a pending notice or release, or work unread or unlisted, past FreshConfirm + Cap
	RateLimitCauseFreshRefused        RateLimitCause = "fresh_refused"        // exhausted: the fresh conversation was refused too
	RateLimitCauseRestartLost         RateLimitCause = "restart_lost"         // exhausted: refused after the restart, in the conversation it left
	RateLimitCauseAgain               RateLimitCause = "again"                // exhausted: refused within SeatFreshGap of the last fresh restart
	RateLimitCauseQuota               RateLimitCause = "quota"                // exhausted: the refusal is a spent usage quota
	RateLimitCauseServed              RateLimitCause = "served"               // closed: the provider served the seat
	RateLimitCauseStale               RateLimitCause = "stale"                // closed: no answered refusal for RateLimitEvidenceMaxAge
	RateLimitCauseConversationChanged RateLimitCause = "conversation_changed" // closed: someone else changed the conversation
)

// IsZero reports whether rl holds neither an episode nor a fresh restart.
func (rl RateLimit) IsZero() bool {
	return rl.Phase == RateLimitClear && rl.Conv == 0 && rl.FromConv == 0 && rl.Alerted == "" &&
		rl.Since.IsZero() && rl.Checked.IsZero() && rl.FreshAt.IsZero() && rl.LastFresh.IsZero()
}

// clampRateLimit pulls rl's times from a clock that ran ahead back to now.
func clampRateLimit(rl RateLimit, now time.Time) RateLimit {
	rl.Since, rl.Checked, rl.FreshAt, rl.LastFresh = minTime(rl.Since, now), minTime(rl.Checked, now), minTime(rl.FreshAt, now), minTime(rl.LastFresh, now)
	return rl
}

// RestartDue reports whether r's episode may stand confirmed at now: open,
// and FreshConfirm past the refusal that opened it. Only then may Decide
// restart the seat fresh (and only on a refusal of a nudge delivered after
// the episode opened), so only then need the caller read what holds a
// restart (Observation.RestartHold), which is not free to read.
func RestartDue(r Record, now time.Time, p Policy) bool {
	p = p.withDefaults()
	return r.RateLimit.Phase == RateLimitOpen && now.Sub(r.RateLimit.Since) >= p.FreshConfirm
}

// seatActs reports whether the pass may act on the seat at all: the gate
// open, the runtime alive, the seat quiet. The episode moves only then.
func (obs Observation) seatActs() bool {
	return !obs.Gate.Held && obs.Runtime == TriTrue && obs.Quiet.State == Quiescent
}

// rateLimit is the step of d.Next's rate-limit episode, on the cell
// Decide reconciled from prev. It reports whether the step asked for a fresh
// restart: the pass then nudges nothing.
//
// The evidence is this conversation's reading only. A, the answered refusal:
// the provider refused at least RateLimitThreshold of the seat's newest
// turns in a row, the newest after the seat's last delivered nudge of this
// generation and no older than RateLimitEvidenceMaxAge, while it holds work.
// S, served: a turn the provider completed after the episode began (or after
// its fresh restart). The evidence ages by the refusal's own time, never by
// the pass that reads it: a seat whose nudges stop reaching it reads the
// same refusal on every pass, and that one refusal must not keep its episode
// standing past the bound.
//
// Nothing moves on a pass that cannot act on the seat. Every move but
// closing needs the provider not degraded (WL4): closing stays allowed so a
// provider hold can be lifted on the seat's own service.
func (p Policy) rateLimit(d *Decision, obs Observation) bool {
	now := obs.Now
	rl := &d.Next.RateLimit
	if !obs.seatActs() {
		return false
	}
	t := obs.Turns
	reading := t.Read && t.Conv == obs.Conv
	answered := reading && len(d.Next.Entries) > 0 && t.Refused >= p.RateLimitThreshold &&
		!d.Next.Delivered.IsZero() && t.RefusedAt.After(d.Next.Delivered) && now.Sub(t.RefusedAt) <= p.RateLimitEvidenceMaxAge
	refusedAt := minTime(t.RefusedAt, now)
	degraded := obs.Provider.Degraded
	move := func(tr RateLimitTransition, c RateLimitCause) {
		d.Actions = append(d.Actions, RateLimitMove{Transition: tr, Cause: c})
	}
	// alert raises the episode's one alert, if it has none yet.
	alert := func(reason string) bool {
		if rl.Alerted != "" {
			return false
		}
		rl.Alerted = reason
		d.Actions = append(d.Actions, Alert{Key: subject(d.Next), Reason: reason})
		return true
	}
	closeAs := func(c RateLimitCause) {
		*rl = RateLimit{LastFresh: rl.LastFresh}
		move(RateLimitTransitionClosed, c)
	}

	// Closing: service, a conversation someone else changed, stale evidence.
	switch rl.Phase {
	case RateLimitOpen, RateLimitExhausted:
		switch {
		case obs.Conv != rl.Conv:
			closeAs(RateLimitCauseConversationChanged)
		case reading && t.ServedAt.After(rl.Since):
			closeAs(RateLimitCauseServed)
		}
	case RateLimitRestarted:
		if reading && t.ServedAt.After(rl.FreshAt) {
			closeAs(RateLimitCauseServed)
		}
	}
	if rl.Phase != RateLimitClear && !answered && now.Sub(rl.Checked) > p.RateLimitEvidenceMaxAge {
		closeAs(RateLimitCauseStale)
	}
	if rl.Phase == RateLimitClear && !rl.LastFresh.IsZero() && now.Sub(rl.LastFresh) >= max(p.SeatFreshGap, p.ProviderFreshGap) {
		rl.LastFresh = time.Time{}
	}
	if !answered {
		return false
	}
	if rl.Phase != RateLimitClear && refusedAt.After(rl.Checked) {
		rl.Checked = refusedAt
	}
	if degraded {
		return false
	}

	switch rl.Phase {
	case RateLimitClear:
		*rl = RateLimit{Phase: RateLimitOpen, Conv: obs.Conv, Since: refusedAt, Checked: refusedAt, LastFresh: rl.LastFresh}
		move(RateLimitTransitionOpened, "")
	case RateLimitRestarted:
		if !t.RefusedAt.After(rl.FreshAt) {
			return false
		}
		cause := RateLimitCauseFreshRefused
		if obs.Conv == rl.FromConv {
			cause = RateLimitCauseRestartLost
		}
		rl.Phase, rl.Conv = RateLimitExhausted, obs.Conv
		alert(AlertRateLimitedFreshRefused)
		move(RateLimitTransitionExhausted, cause)
	case RateLimitOpen:
		// Confirmed only once it has stood FreshConfirm and a nudge
		// delivered after it opened drew a refusal too: the refusal that
		// opened it confirms nothing alone, however long ago it ended.
		if now.Sub(rl.Since) < p.FreshConfirm || !d.Next.Delivered.After(rl.Since) {
			return false
		}
		return p.confirmed(d, obs, alert, move)
	}
	return false
}

// confirmed is an open episode's step once it stands confirmed (FreshConfirm
// past its opening, and an answered refusal of a nudge delivered since):
// exhausted when no fresh restart can help (a spent quota, a seat restarted
// fresh within SeatFreshGap), held while a standing hold or a block applies,
// else restarted fresh.
func (p Policy) confirmed(d *Decision, obs Observation, alert func(string) bool, move func(RateLimitTransition, RateLimitCause)) bool {
	now := obs.Now
	rl := &d.Next.RateLimit
	providerFresh := minTime(obs.ProviderFreshAt, now)
	blocked := (!providerFresh.IsZero() && now.Sub(providerFresh) < p.ProviderFreshGap) ||
		anyMarked(d.Next) || d.Next.Release != nil || len(obs.Hold) > 0 || obs.Partial
	switch {
	case obs.Turns.Quota:
		rl.Phase = RateLimitExhausted
		alert(AlertRateLimitedQuota)
		move(RateLimitTransitionExhausted, RateLimitCauseQuota)
	case !rl.LastFresh.IsZero() && now.Sub(rl.LastFresh) < p.SeatFreshGap:
		rl.Phase = RateLimitExhausted
		alert(AlertRateLimitedAgain)
		move(RateLimitTransitionExhausted, RateLimitCauseAgain)
	case obs.RestartHold != RestartHoldNone:
		cause := RateLimitCauseAttached
		if obs.RestartHold == RestartHoldAwaitingHuman {
			cause = RateLimitCauseAwaitingHuman
		}
		if alert(AlertRateLimitedHeld) {
			move(RateLimitTransitionHeld, cause)
		}
	case blocked:
		if now.Sub(rl.Since) >= p.FreshConfirm+p.Cap && alert(AlertRateLimitedHeld) {
			move(RateLimitTransitionHeld, RateLimitCauseBlocked)
		}
	default:
		rl.Phase, rl.FreshAt, rl.FromConv, rl.LastFresh = RateLimitRestarted, now, rl.Conv, now
		d.Next.RecycledGen = obs.Gen
		d.Actions = append(d.Actions, FreshRestart{})
		move(RateLimitTransitionRestarted, "")
		return true
	}
	return false
}

// subject is the claim a seat-level alert is keyed by: the seat's first
// started claim in key order, else its first entry.
func subject(r Record) ClaimKey {
	for _, e := range r.Entries {
		if e.Kind == KindClaim && e.Rung >= RungStarted {
			return e.Key
		}
	}
	if len(r.Entries) > 0 {
		return r.Entries[0].Key
	}
	return ClaimKey{}
}
