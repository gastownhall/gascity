package main

// The work ladder's rate-limit episode, wired into the lane (W3 rule 1,
// #8132 split C3; the pure episode is internal/workladder's ratelimit.go).
//
// A seat whose provider refuses its turns with a rate limit (two in a row, as
// the agent CLI classified them, the newest after a nudge the seat took) is
// paced paused, as on a degraded provider, and after FreshConfirm is
// restarted fresh once: gc session reset's request (restart_requested and
// continuation_reset_pending, Manager.RequestFreshRestart's pair) lands in
// the same CAS as the cell, and arm A13 restarts the seat in place on a new
// conversation, keeping its claims. If the new conversation is refused too,
// the episode raises its one alert and the seat stays paced paused until its
// provider serves it.
//
// The lane supplies what Decide cannot read itself: the seat's turns
// (work_ladder_turns.go), when it last took a nudge (Record.Delivered: a
// direct delivery that returned, a notice settled delivered at the start of
// its delivering attempt), the provider gap (the newest fresh restart of
// another governed seat on the same provider, read every pass off their
// saved cells), and, only once a restart is due, what holds one: a person
// attached to the seat, or work of the seat awaiting a person. Before a
// decision that restarts the seat lands, every claim of the seat is re-read
// live: one that cannot be read holds the restart, and one found awaiting a
// person holds it standing.
//
// Every move is reported as work.rate_limited (events.WorkRateLimitedPayload)
// for a pack's provider policy: whether a provider's pool is refusing every
// seat, which provider names share it and how long to hold them is judgment,
// and stays out of Go. A pack holds a provider through the ADR-0013
// provider-health file, which the ladder already honors (WL4).

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
)

// maxLadderErrorText bounds a refusal's text in an event or an alert, in
// bytes (the transcript's own bound, sessionlog.MaxTurnFailureMessage).
const maxLadderErrorText = 512

// ladderFreshIndex is, per provider, each governed seat's last fresh restart
// (RateLimit.LastFresh), read off the seats' saved cells at the start of a
// pass and updated as the pass restarts seats. It is pass-local: rebuilt
// from durable cells every pass, so a controller restart loses nothing. A
// cell that cannot be read contributes nothing.
type ladderFreshIndex map[string]map[string]time.Time

// ladderProvider is row's provider: its provider metadata, as it stands now.
func ladderProvider(row beads.Bead) string {
	return strings.TrimSpace(row.Metadata["provider"])
}

// buildLadderFreshIndex reads every governed seat's last fresh restart off
// raw.
func buildLadderFreshIndex(raw []beads.Bead) ladderFreshIndex {
	x := ladderFreshIndex{}
	for _, row := range raw {
		if !ladderGoverns(row) {
			continue
		}
		r, err := workladder.DecodeLadder(row.Metadata[session.WorkLadderKey])
		if err != nil || r.RateLimit.LastFresh.IsZero() {
			continue
		}
		x.record(ladderProvider(row), row.ID, r.RateLimit.LastFresh)
	}
	return x
}

// record notes seat's fresh restart at at on provider.
func (x ladderFreshIndex) record(provider, seat string, at time.Time) {
	if x[provider] == nil {
		x[provider] = map[string]time.Time{}
	}
	if at.After(x[provider][seat]) {
		x[provider][seat] = at
	}
}

// others is the newest fresh restart of a seat other than seat on provider;
// zero when there is none.
func (x ladderFreshIndex) others(provider, seat string) time.Time {
	var newest time.Time
	for s, at := range x[provider] {
		if s != seat && at.After(newest) {
			newest = at
		}
	}
	return newest
}

// noteHumanWait records that seat holds a claim whose seat asked a person
// and waits on their decision: a fresh restart would lose that
// conversation, so it holds the seat's restart (RestartHoldAwaitingHuman).
func (p *ladderPass) noteHumanWait(seat string) {
	if p.humanWaits == nil {
		p.humanWaits = map[string]bool{}
	}
	p.humanWaits[seat] = true
}

// probeAttached reports whether a person is attached to row's seat, or the
// probe cannot tell (runtime.AttachProbeHolds): either holds a fresh restart.
// The probe is not free, so the pass asks it only once a restart is due.
func (p *ladderPass) probeAttached(row beads.Bead) bool {
	attached, err := runtime.IsAttachedWithError(p.env.SP, strings.TrimSpace(row.Metadata["session_name"]))
	return runtime.AttachProbeHolds(attached, err)
}

// restartHold is what holds seat id's fresh restart now, when the pass read
// one is due (rt.restartDue): its work awaiting a person (read live, so a
// claim found waiting by a re-read counts), else a person attached.
func (p *ladderPass) restartHold(id string, rt ladderRuntime) workladder.RestartHold {
	switch {
	case !rt.restartDue:
		return workladder.RestartHoldNone
	case p.humanWaits[id]:
		return workladder.RestartHoldAwaitingHuman
	case rt.attached:
		return workladder.RestartHoldAttached
	}
	return workladder.RestartHoldNone
}

// freshRestartOf reports whether d restarts its seat fresh.
func freshRestartOf(d workladder.Decision) bool {
	return slices.ContainsFunc(d.Actions, func(a workladder.Action) bool {
		_, ok := a.(workladder.FreshRestart)
		return ok
	})
}

// isRateLimitAlert reports whether reason is one of the episode's alerts.
func isRateLimitAlert(reason string) bool {
	switch reason {
	case workladder.AlertRateLimitedFreshRefused, workladder.AlertRateLimitedAgain, workladder.AlertRateLimitedHeld, workladder.AlertRateLimitedQuota:
		return true
	}
	return false
}

// rateLimitAlerted reports whether d raised its episode's one alert.
func rateLimitAlerted(d workladder.Decision) bool {
	return slices.ContainsFunc(d.Actions, func(a workladder.Action) bool {
		al, ok := a.(workladder.Alert)
		return ok && isRateLimitAlert(al.Reason)
	})
}

// startedClaims are r's started claims, in key order.
func startedClaims(r workladder.Record) []string {
	var out []string
	for _, e := range r.Entries {
		if e.Kind == workladder.KindClaim && e.Rung >= workladder.RungStarted {
			out = append(out, e.Key.Bead)
		}
	}
	return out
}

// ladderErrorText is a refusal's text for an event or an alert: control
// characters as spaces, cut on a rune boundary to maxLadderErrorText bytes.
func ladderErrorText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) <= maxLadderErrorText {
		return s
	}
	n := maxLadderErrorText
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// optionalTime is t, or nil when it is zero (the wire omits it).
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// rateLimitMoved reports move m of the seat's episode as work.rate_limited.
// before is the episode as the commit found it (a closed episode's facts
// are gone from the landed cell), rt the pass's reading of the seat.
func (p *ladderPass) rateLimitMoved(l ladderLanded, m workladder.RateLimitMove, rt ladderRuntime) {
	if p.l.events == nil {
		return
	}
	row, d := l.row, l.d
	ep := d.Next.RateLimit
	if m.Transition == workladder.RateLimitTransitionClosed {
		ep = l.before
	}
	provider := ladderProvider(row)
	payload := events.WorkRateLimitedPayload{
		SessionID: row.ID, SessionName: strings.TrimSpace(row.Metadata["session_name"]), Template: strings.TrimSpace(row.Metadata["template"]),
		Provider: provider, Generation: d.Next.Gen, Conversation: rowConversation(row.Metadata),
		Transition: string(m.Transition), Cause: string(m.Cause), Since: ep.Since,
		FreshAt: optionalTime(ep.FreshAt), Claims: startedClaims(d.Next),
		Alerted: (m.Transition == workladder.RateLimitTransitionHeld || m.Transition == workladder.RateLimitTransitionExhausted) && rateLimitAlerted(d),
	}
	if t := rt.turns; t.Read && t.Refused > 0 {
		payload.Refused, payload.RefusedAt = t.Refused, optionalTime(t.RefusedAt)
		payload.ErrorCode, payload.HTTPStatus, payload.ErrorText = rt.refusal.Code, rt.refusal.HTTPStatus, ladderErrorText(rt.refusal.Message)
	}
	message := fmt.Sprintf("provider %s rate-limit episode %s", provider, m.Transition)
	if m.Cause != "" {
		message += " (" + string(m.Cause) + ")"
	}
	fmt.Fprintf(p.l.stderr, "work-ladder: %s: %s\n", row.ID, message) //nolint:errcheck // best-effort stderr
	p.recordEvent(events.WorkRateLimited, row.ID, row.ID, message, payload)
}

// rateLimitAlertMessage is a rate-limit alert's text for people: it names
// the provider and quotes its newest refusal (the provider's own words, such
// as a pool's cooldown), so whoever reroutes the lane sees what it said.
// cause is the held move's cause, if the alert is held's.
func rateLimitAlertMessage(row beads.Bead, reason string, cause workladder.RateLimitCause, rt ladderRuntime) string {
	provider, text := ladderProvider(row), ladderErrorText(rt.refusal.Message)
	var msg string
	switch reason {
	case workladder.AlertRateLimitedFreshRefused:
		msg = fmt.Sprintf("provider %s refused the seat's fresh conversation too; the seat stays paced paused, not restarted again, until the provider serves it", provider)
	case workladder.AlertRateLimitedAgain:
		msg = fmt.Sprintf("provider %s refused the seat again within %v of its last fresh restart; the seat stays paced paused until the provider serves it", provider, workladder.DefaultCap)
	case workladder.AlertRateLimitedQuota:
		msg = fmt.Sprintf("provider %s refuses the seat on a spent usage quota, which no fresh restart gets past; the seat stays paced paused until the provider serves it", provider)
	case workladder.AlertRateLimitedHeld:
		why := "a block"
		switch cause {
		case workladder.RateLimitCauseAttached:
			why = "a person attached to it"
		case workladder.RateLimitCauseAwaitingHuman:
			why = "its work awaiting a person"
		case workladder.RateLimitCauseBlocked:
			why = "another seat's fresh restart on the provider, a pending nudge or release, or an unread claim"
		}
		msg = fmt.Sprintf("provider %s refuses the seat with a rate limit, and %s keeps the work ladder from restarting it fresh", provider, why)
	default:
		return ""
	}
	if text != "" {
		msg += ": " + text
	}
	return msg
}

// heldCause is the cause of d's held move, if it has one.
func heldCause(d workladder.Decision) workladder.RateLimitCause {
	for _, a := range d.Actions {
		if m, ok := a.(workladder.RateLimitMove); ok && m.Transition == workladder.RateLimitTransitionHeld {
			return m.Cause
		}
	}
	return ""
}
