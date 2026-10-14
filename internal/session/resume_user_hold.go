package session

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file is the consume of an operator's suspend by an operator's own
// resume (ActorOperator: Attach, and Start, Submit or Send by an operator):
// the disposition table's Resume cell, written by ApplyDisposition after the
// runtime is up, under the runtime lease (L1b) that fences it against the
// controller's kills. A refused or moved consume never stops the runtime it
// launched: the hold stands, and the fenced controller drain handles it
// (D-B).

// liveUserHoldDecision is userHoldDecision for a running row: only an
// operator's resume of an active or awake row consumes it, never a draining
// or stop-pending one, whose stop is already the controller's.
func liveUserHoldDecision(b beads.Bead, by Actor) (Decided, bool) {
	switch State(strings.TrimSpace(b.Metadata["state"])) {
	case StateActive, StateAwake:
	default:
		return Decided{}, false
	}
	if !by.consumesHold() {
		return Decided{}, false
	}
	return userHoldDecision(b)
}

// userHoldDecision is the decision to consume b's operator suspend, as the
// resume read the row before its start, or false when b carries none.
func userHoldDecision(b beads.Bead) (Decided, bool) {
	if !DecodeDisposition(b.Metadata).KeepsSlot() {
		return Decided{}, false
	}
	return Decide(infoFromPersistedBead(b), FactsResumeConsume), true
}

// consumeUserHoldLife is the lifecycle a consume writes beside the class:
// the resumed runtime, the wake request it satisfies and the pending-create
// claim.
func consumeUserHoldLife(Disposition, Disposition, Info) MetadataPatch {
	patch := ClearWakeRequestPatch()
	patch["state"] = string(StateActive)
	patch["state_reason"] = "creation_complete"
	for _, k := range []string{"sleep_reason", "slept_at", "pending_create_claim", "pending_create_started_at"} {
		patch[k] = ""
	}
	return patch
}

// consumeUserHold is the confirmation of a resumed runtime whose row the
// operator held: the Resume cell, written while the row still carries the
// facts d read. A refused write over a row already active, with no hold that
// blocks a consume (BlocksConsume), at the same generation and token is
// converged; otherwise it returns an ErrStateSync error, so the caller never
// stops the runtime it launched.
func (m *Manager) consumeUserHold(d Decided, now time.Time, clears MetadataPatch) error {
	store := m.PersistedStore()
	life := consumeUserHoldLife
	if len(clears) > 0 { // a runtime this resume started: as PreWake, it clears the stop request
		life = func(from, to Disposition, info Info) MetadataPatch {
			patch := consumeUserHoldLife(from, to, info)
			maps.Copy(patch, clears)
			return patch
		}
	}
	w, err := store.ApplyDisposition(d, ActorOperator, DispEvent{Kind: EvResume}, life, now)
	var refused *ErrDispositionRefused
	switch {
	case err != nil && !errors.As(err, &refused):
		return fmt.Errorf("%w: consuming the user hold: %w", ErrStateSync, err)
	case err == nil && w.Result == CommitLanded:
		return nil
	}
	b, rerr := store.freshBead(d.info.ID)
	if rerr == nil && b.Status != "closed" {
		state := State(strings.TrimSpace(b.Metadata["state"]))
		if (state == StateActive || state == StateAwake) && !DecodeDisposition(b.Metadata).BlocksConsume(now) &&
			b.Metadata["generation"] == d.info.Generation && b.Metadata["instance_token"] == d.info.InstanceToken {
			return nil
		}
	}
	return fmt.Errorf("%w: the user hold on session %s changed during the resume; the runtime is left running", ErrStateSync, d.info.ID)
}
