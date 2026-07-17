package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

const (
	nudgeMailSweepDefaultNudgeTTL     = 10 * time.Minute
	nudgeMailSweepDefaultMailTTL      = 60 * time.Minute
	nudgeMailSweepCloseBudget         = 50
	nudgeMailSweepWatchdogInterval    = 5 * time.Minute
	nudgeMailSweepWatchdogCloseBudget = 500

	// nudgeMailSweepNudgeCloseReason is the close_reason stamped on stale nudge
	// beads before close. The 20-character floor satisfies validation.on-close=error.
	nudgeMailSweepNudgeCloseReason = "nudge gc-swept: stale nudge bead past gc retention window"

	// nudgeMailSweepMailCloseReason is the close_reason stamped on read mail
	// beads before close.
	nudgeMailSweepMailCloseReason = "mail gc-swept: read mail bead past gc retention window"
)

// nudgeMailSweepResult holds per-category close counts from sweepStaleNudgeMail.
type nudgeMailSweepResult struct {
	NudgeClosed int
	MailClosed  int
}

// sweepStaleNudgeMail closes stale consumed nudge beads and read mail beads.
//
// Nudge candidates are open beads with label gc:nudge created before now-nudgeTTL
// whose nudge_id is not present in nudgeState.Pending or nudgeState.InFlight.
// Terminal metadata is stamped via nudgequeue.Store.SweepStale before each close
// so the bead audit trail is intact.
//
// Mail candidates are open message beads with label "read" created before now-mailTTL.
//
// limit caps total closes (nudge + mail combined). Pass 0 for no cap.
// Per-bead errors do not abort the sweep; they are returned via errors.Join so
// the caller can report them without treating the sweep as fatal.
//
// The nudge phase is sourced from the strongly-typed nudgeStore (the nudges
// class); the mail phase from the strongly-typed mailStore (the messaging class).
// Both wrap the same underlying work store until either class relocates, so
// behavior is unchanged today.
func sweepStaleNudgeMail(nudgeStore beads.NudgesStore, mailStore beads.MailStore, nudgeState *nudgequeue.State, now time.Time, nudgeTTL, mailTTL time.Duration, limit int) (nudgeMailSweepResult, error) {
	var result nudgeMailSweepResult
	var beadErrs []error

	liveIDs := liveNudgeIDSet(nudgeState)
	nq := nudgequeue.NewStore(nudgeStore)

	// Phase 1: close stale nudge beads. The live flock-queue exclusion is carried
	// inside StaleShadowsBefore; the cross-phase close budget stays in this loop.
	nudgeCutoff := now.Add(-nudgeTTL)
	// nudge/mail beads are NoHistory (wisp-tier); StaleShadowsBefore reads both tiers.
	nudgeShadows, err := nq.StaleShadowsBefore(nudgeCutoff, limit, liveIDs)
	if err != nil {
		return result, fmt.Errorf("nudge-mail-sweep: listing stale nudge beads: %w", err)
	}

	for _, shadow := range nudgeShadows {
		if limit > 0 && result.NudgeClosed+result.MailClosed >= limit {
			break
		}
		if !shadow.Open {
			continue
		}
		if err := nq.SweepStale(shadow.BeadID, nudgeMailSweepNudgeCloseReason, now); err != nil {
			beadErrs = append(beadErrs, err)
			continue
		}
		result.NudgeClosed++
	}

	// Phase 2: close read mail beads. The candidate query + close-with-reason
	// loop live inside the messaging edge (beadmail); only the shared close
	// budget is passed in. mailBudget is the remaining share of the combined
	// limit, so a fatal listing failure early-returns (discarding accumulated
	// per-bead errors) exactly as the inline loop did.
	mailCutoff := now.Add(-mailTTL)
	remaining := limit - result.NudgeClosed - result.MailClosed
	if limit == 0 || remaining > 0 {
		mailBudget := remaining
		if limit == 0 {
			mailBudget = 0
		}
		mailClosed, mailCloseErrs, mailListErr := beadmail.SweepReadMessagesBefore(mailStore, mailCutoff, mailBudget, nudgeMailSweepMailCloseReason)
		if mailListErr != nil {
			return result, fmt.Errorf("nudge-mail-sweep: listing read mail beads: %w", mailListErr)
		}
		result.MailClosed += mailClosed
		beadErrs = append(beadErrs, mailCloseErrs...)
	}

	return result, errors.Join(beadErrs...)
}

// countStaleNudgeMail returns what sweepStaleNudgeMail would close without
// making any changes. Used by --dry-run to report candidate count without side
// effects. The limit parameter caps the count the same way sweepStaleNudgeMail
// caps closes; pass 0 for no cap. The nudge phase is counted from the typed
// nudgeStore (nudges class); the mail phase from the typed mailStore (messaging class).
func countStaleNudgeMail(nudgeStore beads.NudgesStore, mailStore beads.MailStore, nudgeState *nudgequeue.State, now time.Time, nudgeTTL, mailTTL time.Duration, limit int) (nudgeMailSweepResult, error) {
	var result nudgeMailSweepResult

	liveIDs := liveNudgeIDSet(nudgeState)
	nq := nudgequeue.NewStore(nudgeStore)

	// Dry-run twin of the sweep: same typed read, same cross-phase budget, no writes.
	nudgeCutoff := now.Add(-nudgeTTL)
	nudgeShadows, err := nq.StaleShadowsBefore(nudgeCutoff, limit, liveIDs)
	if err != nil {
		return result, fmt.Errorf("nudge-mail-sweep (dry-run): listing stale nudge beads: %w", err)
	}
	for _, shadow := range nudgeShadows {
		if limit > 0 && result.NudgeClosed+result.MailClosed >= limit {
			break
		}
		if !shadow.Open {
			continue
		}
		result.NudgeClosed++
	}

	mailCutoff := now.Add(-mailTTL)
	remaining := limit - result.NudgeClosed - result.MailClosed
	if limit == 0 || remaining > 0 {
		mailBudget := remaining
		if limit == 0 {
			mailBudget = 0
		}
		mailCount, err := beadmail.CountReadMessagesBefore(mailStore, mailCutoff, mailBudget)
		if err != nil {
			return result, fmt.Errorf("nudge-mail-sweep (dry-run): listing read mail beads: %w", err)
		}
		result.MailClosed += mailCount
	}
	return result, nil
}

// liveNudgeIDSet returns the set of nudge IDs currently in pending or in-flight state.
// Returns nil (no live IDs) when nudgeState is nil.
func liveNudgeIDSet(state *nudgequeue.State) map[string]bool {
	if state == nil {
		return nil
	}
	live := make(map[string]bool, len(state.Pending)+len(state.InFlight))
	for _, item := range state.Pending {
		live[item.ID] = true
	}
	for _, item := range state.InFlight {
		live[item.ID] = true
	}
	return live
}
