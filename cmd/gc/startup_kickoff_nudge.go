package main

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// startupKickoffNudgeLabel names the named/direct startup backstop in its log
// lines and its planner notices.
const startupKickoffNudgeLabel = "startup-kickoff-nudge"

// nudgeOutstandingStartupKickoffs is the named/direct sibling to the pool
// claim, continuation and execution backstops. A named or direct session is
// seeded with an outstanding startup kickoff (startup_kickoff.go) when it is
// bound to a work step; if its first turn is missed (a swallowed submit, a
// warm session that survived a restart) the session sits idle at its prompt
// with a step it never began. This re-delivers the configured claim nudge over
// the existing delivery primitive (runNudgeBackstop), with the same bounded
// grace → nudge → backoff → give-up pacing the pool backstops use.
//
// boundSteps is the tick's work snapshot, which must be complete: a bound step
// that is absent from it, or no longer open, has advanced and confirms the
// kickoff, so the caller skips this backstop on a partial snapshot. Once the
// nudges are spent the session latches startup_kickoff_state=given_up and
// emits events.SessionStartupKickoffGivenUp once; a given-up (or confirmed)
// kickoff is never governed again, so there is no unbounded retry. Pool
// sessions are excluded: their own backstops own delivery there.
func nudgeOutstandingStartupKickoffs(
	sp runtime.Provider,
	cfg *config.City,
	store beads.Store,
	sessionBeads []beads.Bead,
	boundSteps []beads.Bead,
	now time.Time,
	rec events.Recorder,
	stdout io.Writer,
) {
	if sp == nil || cfg == nil || store == nil {
		return // hot reconcile path: never panic on a half-built dependency
	}
	runNudgeBackstop(sp, store, sessionBeads, boundSteps, now, stdout, startupKickoffNudgeLabel, namedDirectStartupBackstop{
		cfg: cfg,
		sp:  sp,
		now: now,
		rec: rec,
	})
}

// namedDirectStartupBackstop is the backstopPredicate for a named or direct
// session with an outstanding startup kickoff. See nudgeOutstandingStartupKickoffs.
type namedDirectStartupBackstop struct {
	cfg *config.City
	sp  runtime.Provider
	now time.Time
	rec events.Recorder
}

// governs: a pool-managed session is never governed, and only a kickoff that
// is still pending or nudged is. Confirmed and given_up are terminal.
func (namedDirectStartupBackstop) governs(s beads.Bead) bool {
	if strings.TrimSpace(s.Metadata["pool_managed"]) == "true" {
		return false
	}
	switch strings.TrimSpace(s.Metadata[startupKickoffStateKey]) {
	case startupKickoffStatePending, startupKickoffStateNudged:
		return true
	}
	return false
}

// resolve: the kickoff is outstanding only while the bound step is still open
// in the snapshot. A step that is claimed, closed or absent has advanced.
func (namedDirectStartupBackstop) resolve(s beads.Bead, work map[string]beads.Bead, _ string) (backstopTarget, backstopResolution) {
	stepID := strings.TrimSpace(s.Metadata[beadmeta.BoundStepIDMetadataKey])
	if stepID == "" {
		return backstopTarget{}, backstopResolutionHold
	}
	step, ok := work[stepID]
	if !ok || !strings.EqualFold(strings.TrimSpace(step.Status), "open") {
		return backstopTarget{}, backstopResolutionClear
	}
	return backstopTarget{ID: stepID}, backstopResolutionOutstanding
}

// state reads the kickoff's pacing. The first attempt's grace runs from
// startup_kickoff_started_at, never from a last-nudge timestamp a previous
// binding may have left behind. A missing or unreadable clock reads as an
// unobserved assignment, so observe restarts it.
func (namedDirectStartupBackstop) state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time) {
	if strings.TrimSpace(s.Metadata[beadmeta.BoundStepIDMetadataKey]) != target.ID {
		return false, 0, time.Time{}
	}
	attempts = atoiOr0(s.Metadata[startupKickoffAttemptsKey])
	last = parseRFC3339OrZero(s.Metadata[startupKickoffStartedAtKey])
	if attempts > 0 {
		if at := parseRFC3339OrZero(s.Metadata[startupKickoffLastNudgeAtKey]); !at.IsZero() {
			last = at
		}
	}
	return !last.IsZero(), attempts, last
}

func (p namedDirectStartupBackstop) content(s beads.Bead) string {
	return claimNudgeFor(p.cfg, s)
}

// revalidate holds while the runtime shows the session busy: a nudge typed
// into a running turn can abort it, and a busy session is most often the one
// already running its step. A hold spends no attempt.
func (p namedDirectStartupBackstop) revalidate(s beads.Bead, sessName string, _ backstopTarget) (backstopResolution, string) {
	if hold := claimNudgeBusyHold(p.sp, s, sessName, p.now); hold != "" {
		return backstopResolutionHold, hold
	}
	return backstopResolutionOutstanding, ""
}

func (namedDirectStartupBackstop) observe(store beads.Store, s *beads.Bead, _ backstopTarget, now time.Time, stdout io.Writer) {
	writeSessionMetadata(store, s, map[string]string{
		startupKickoffStateKey:       startupKickoffStatePending,
		startupKickoffStartedAtKey:   now.UTC().Format(time.RFC3339),
		startupKickoffAttemptsKey:    "0",
		startupKickoffLastNudgeAtKey: "",
	}, startupKickoffNudgeLabel, stdout)
}

// reserve records attempts at now. attempts == 0 is the engine refunding a
// deferred first delivery, which puts the kickoff back to pending.
func (namedDirectStartupBackstop) reserve(store beads.Store, s *beads.Bead, _ backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	if attempts == 0 {
		return writeSessionMetadata(store, s, map[string]string{
			startupKickoffStateKey:       startupKickoffStatePending,
			startupKickoffAttemptsKey:    "0",
			startupKickoffLastNudgeAtKey: "",
		}, startupKickoffNudgeLabel, stdout)
	}
	return writeSessionMetadata(store, s, map[string]string{
		startupKickoffStateKey:       startupKickoffStateNudged,
		startupKickoffAttemptsKey:    strconv.Itoa(attempts),
		startupKickoffLastNudgeAtKey: now.UTC().Format(time.RFC3339),
	}, startupKickoffNudgeLabel, stdout)
}

// exhausted latches given_up and emits the typed event once. The event follows
// the latch, so a failed write retries next tick without a duplicate event.
func (p namedDirectStartupBackstop) exhausted(store beads.Store, s *beads.Bead, stdout io.Writer) {
	attempts := atoiOr0(s.Metadata[startupKickoffAttemptsKey])
	stepID := strings.TrimSpace(s.Metadata[beadmeta.BoundStepIDMetadataKey])
	if !writeSessionMetadata(store, s, map[string]string{
		startupKickoffStateKey:       startupKickoffStateGivenUp,
		startupKickoffLastNudgeAtKey: "",
	}, startupKickoffNudgeLabel, stdout) {
		return
	}
	if p.rec == nil {
		return
	}
	payload, err := json.Marshal(events.SessionStartupKickoffGivenUpPayload{
		SessionID:   s.ID,
		BoundStepID: stepID,
		Attempts:    attempts,
	})
	if err != nil {
		return
	}
	p.rec.Record(events.Event{
		Type:      events.SessionStartupKickoffGivenUp,
		Actor:     eventActor(),
		Subject:   stepID,
		SessionID: s.ID,
		Payload:   payload,
	})
}

// clear confirms a kickoff whose bound step has advanced, and drops the
// last-nudge time so a later rebind starts its own grace.
func (namedDirectStartupBackstop) clear(store beads.Store, s *beads.Bead, stdout io.Writer) bool {
	return writeSessionMetadata(store, s, map[string]string{
		startupKickoffStateKey:       startupKickoffStateConfirmed,
		startupKickoffLastNudgeAtKey: "",
	}, startupKickoffNudgeLabel, stdout)
}
