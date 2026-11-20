package main

import (
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// namedClaimPickupLabel names the named-seat claim-pickup backstop in its log
// lines and its planner notices.
const namedClaimPickupLabel = "named-claim-pickup"

// Session-bead metadata for the named-seat claim-pickup backstop's pacing. The
// keys are written lazily, on the first sighting of unclaimed work, so a seat
// that never has any carries none of them.
const (
	claimPickupStateKey       = "claim_pickup_state"
	claimPickupStartedAtKey   = "claim_pickup_started_at"
	claimPickupAttemptsKey    = "claim_pickup_attempts"
	claimPickupLastNudgeAtKey = "claim_pickup_last_nudge_at"
	// claimPickupBeadIDKey pins the open bead the pacing is for, so a seat
	// whose oldest open bead changes starts that bead's own grace instead of
	// inheriting the previous one's spent attempts.
	claimPickupBeadIDKey = "claim_pickup_bead_id"

	claimPickupStatePending = "pending"
	claimPickupStateNudged  = "nudged"
	// claimPickupStateGivenUp latches the bounded retries' end for
	// claimPickupBeadIDKey; the seat is never nudged for that bead again.
	claimPickupStateGivenUp = "given_up"
)

// defaultNamedClaimPickupNudge is the nudge for a named seat whose template
// configures none. It carries no --drain-ack: a drain ack on the hook's
// no-work answer recycles the seat, and the seat-claim lane that did so was
// reverted for restarting named seats (#6524).
const defaultNamedClaimPickupNudge = "You have open work assigned to you. Run " + runtime.GCCommandName + " hook --claim --json now; if it returns work, execute it immediately."

// nudgeNamedClaimPickups is the forward-progress backstop for a
// configured-named seat that has no bound step bead. The pool lanes govern
// pool seats only, and the startup-kickoff backstop needs a bound step, so
// without this lane a named seat whose own open work sits unclaimed is never
// told it is waiting. This re-delivers the claim nudge over the shared
// delivery engine (runNudgeBackstop), on the same bounded grace -> nudge ->
// backoff -> give-up pacing, nudge-only: exhaustion latches given_up and emits
// events.SessionClaimPickupExhausted, and never drains or recycles the seat.
//
// work is the tick's assigned-work snapshot. A snapshot the planner could not
// read in full cannot show that nothing is waiting or that the seat claimed,
// so partial does nothing.
func nudgeNamedClaimPickups(
	sp runtime.Provider,
	cfg *config.City,
	store beads.Store,
	sessionBeads []beads.Bead,
	work []beads.Bead,
	partial bool,
	now time.Time,
	rec events.Recorder,
	stdout io.Writer,
) {
	if partial || sp == nil || store == nil {
		return
	}
	runNudgeBackstop(sp, store, sessionBeads, work, now, stdout, namedClaimPickupLabel, namedClaimPickupBackstop{
		cfg: cfg,
		sp:  sp,
		now: now,
		rec: rec,
	})
}

// namedClaimPickupBackstop is the backstopPredicate for a configured-named
// seat with open assigned work it has not picked up. See nudgeNamedClaimPickups.
type namedClaimPickupBackstop struct {
	cfg *config.City
	sp  runtime.Provider
	now time.Time
	rec events.Recorder
}

// governs: a configured-named seat that is not pool-managed and has no bound
// step bead (a bound seat is the startup-kickoff backstop's).
func (namedClaimPickupBackstop) governs(s beads.Bead) bool {
	return session.IsNamedSessionBead(s) &&
		strings.TrimSpace(s.Metadata["pool_managed"]) != "true" &&
		strings.TrimSpace(s.Metadata[beadmeta.BoundStepIDMetadataKey]) == ""
}

// resolve: a bead the seat holds in_progress means it picked its work up, so
// the lane stands down. Otherwise the target is the seat's oldest open
// assigned bead that is not on a dispatch hold. Assignees match any of the
// seat's current identities, since named work is routinely assigned by the
// configured identity rather than the runtime session name.
func (namedClaimPickupBackstop) resolve(s beads.Bead, work map[string]beads.Bead, _ string) (backstopTarget, backstopResolution) {
	mine := make(map[string]struct{})
	for _, id := range currentSessionAssigneeIdentities(s) {
		mine[id] = struct{}{}
	}
	var open []beads.Bead
	for _, w := range work {
		if _, ok := mine[strings.TrimSpace(w.Assignee)]; !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(w.Status)) {
		case "in_progress":
			return backstopTarget{}, backstopResolutionClear
		case "open":
			if !beadmeta.HasDispatchHold(w.Labels) {
				open = append(open, w)
			}
		}
	}
	if len(open) == 0 {
		return backstopTarget{}, backstopResolutionClear
	}
	sort.Slice(open, func(i, j int) bool {
		if !open[i].CreatedAt.Equal(open[j].CreatedAt) {
			return open[i].CreatedAt.Before(open[j].CreatedAt)
		}
		return open[i].ID < open[j].ID
	})
	target := backstopTarget{ID: open[0].ID, Assignee: strings.TrimSpace(open[0].Assignee)}
	if strings.TrimSpace(s.Metadata[claimPickupStateKey]) == claimPickupStateGivenUp &&
		strings.TrimSpace(s.Metadata[claimPickupBeadIDKey]) == target.ID {
		// Latched for this very bead: hold, so the latch stands and the
		// give-up event is not emitted again.
		return backstopTarget{}, backstopResolutionHold
	}
	return target, backstopResolutionOutstanding
}

// state reads the pacing for target. A different bead, a state that is not
// pending or nudged, or an unreadable clock reads as an unobserved
// assignment, so observe restarts the grace.
func (namedClaimPickupBackstop) state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time) {
	if strings.TrimSpace(s.Metadata[claimPickupBeadIDKey]) != target.ID {
		return false, 0, time.Time{}
	}
	switch strings.TrimSpace(s.Metadata[claimPickupStateKey]) {
	case claimPickupStatePending, claimPickupStateNudged:
	default:
		return false, 0, time.Time{}
	}
	attempts = atoiOr0(s.Metadata[claimPickupAttemptsKey])
	last = parseRFC3339OrZero(s.Metadata[claimPickupStartedAtKey])
	if attempts > 0 {
		if at := parseRFC3339OrZero(s.Metadata[claimPickupLastNudgeAtKey]); !at.IsZero() {
			last = at
		}
	}
	return !last.IsZero(), attempts, last
}

// content is the template's configured nudge, else the default. A configured
// nudge that carries --drain-ack is replaced by the default, since an ack on
// the hook's no-work answer recycles a named seat.
func (p namedClaimPickupBackstop) content(s beads.Bead) string {
	if p.cfg != nil {
		if nudge := claimNudgeFor(p.cfg, s); nudge != "" && !strings.Contains(nudge, "--drain-ack") {
			return nudge
		}
	}
	return defaultNamedClaimPickupNudge
}

// revalidate holds while the runtime shows the seat busy: a nudge typed into a
// running turn can abort it, and a busy seat is most often already working. A
// hold spends no attempt.
func (p namedClaimPickupBackstop) revalidate(s beads.Bead, sessName string, _ backstopTarget) (backstopResolution, string) {
	if hold := claimNudgeBusyHold(p.sp, s, sessName, p.now); hold != "" {
		return backstopResolutionHold, hold
	}
	return backstopResolutionOutstanding, ""
}

func (namedClaimPickupBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer) {
	writeSessionMetadata(store, s, map[string]string{
		claimPickupStateKey:       claimPickupStatePending,
		claimPickupStartedAtKey:   now.UTC().Format(time.RFC3339),
		claimPickupAttemptsKey:    "0",
		claimPickupLastNudgeAtKey: "",
		claimPickupBeadIDKey:      target.ID,
	}, namedClaimPickupLabel, stdout)
}

// reserve records attempts at now. attempts == 0 is the engine refunding a
// deferred first delivery, which puts the pacing back to pending.
func (namedClaimPickupBackstop) reserve(store beads.Store, s *beads.Bead, _ backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	if attempts == 0 {
		return writeSessionMetadata(store, s, map[string]string{
			claimPickupStateKey:       claimPickupStatePending,
			claimPickupAttemptsKey:    "0",
			claimPickupLastNudgeAtKey: "",
		}, namedClaimPickupLabel, stdout)
	}
	return writeSessionMetadata(store, s, map[string]string{
		claimPickupStateKey:       claimPickupStateNudged,
		claimPickupAttemptsKey:    strconv.Itoa(attempts),
		claimPickupLastNudgeAtKey: now.UTC().Format(time.RFC3339),
	}, namedClaimPickupLabel, stdout)
}

// exhausted latches given_up and emits the typed event once. The event follows
// the latch, so a failed write retries next tick without a duplicate event.
// It nudges and drains nothing further: a named seat is never recycled for
// work it did not pick up.
func (p namedClaimPickupBackstop) exhausted(store beads.Store, s *beads.Bead, stdout io.Writer) {
	attempts := atoiOr0(s.Metadata[claimPickupAttemptsKey])
	beadID := strings.TrimSpace(s.Metadata[claimPickupBeadIDKey])
	if !writeSessionMetadata(store, s, map[string]string{
		claimPickupStateKey:       claimPickupStateGivenUp,
		claimPickupLastNudgeAtKey: "",
	}, namedClaimPickupLabel, stdout) {
		return
	}
	if p.rec == nil {
		return
	}
	payload, err := json.Marshal(events.SessionClaimPickupExhaustedPayload{
		SessionID:   s.ID,
		SessionName: strings.TrimSpace(s.Metadata["session_name"]),
		BeadID:      beadID,
		Attempts:    attempts,
	})
	if err != nil {
		return
	}
	p.rec.Record(events.Event{
		Type:      events.SessionClaimPickupExhausted,
		Actor:     eventActor(),
		Subject:   beadID,
		SessionID: s.ID,
		Payload:   payload,
	})
}

// clear wipes the pacing once nothing is outstanding, and reports whether it
// wiped any: false when there was none, or the write failed.
func (namedClaimPickupBackstop) clear(store beads.Store, s *beads.Bead, stdout io.Writer) bool {
	if s.Metadata[claimPickupStateKey] == "" && s.Metadata[claimPickupStartedAtKey] == "" && s.Metadata[claimPickupAttemptsKey] == "" &&
		s.Metadata[claimPickupLastNudgeAtKey] == "" && s.Metadata[claimPickupBeadIDKey] == "" {
		return false
	}
	return writeSessionMetadata(store, s, map[string]string{
		claimPickupStateKey:       "",
		claimPickupStartedAtKey:   "",
		claimPickupAttemptsKey:    "",
		claimPickupLastNudgeAtKey: "",
		claimPickupBeadIDKey:      "",
	}, namedClaimPickupLabel, stdout)
}
