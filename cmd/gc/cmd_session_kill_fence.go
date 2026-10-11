package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionKillFence is the durable kill intent `gc session kill` writes before
// it stops the runtime (see session.KillPendingPatch). It remembers the values
// the patch replaced so a failed Stop can put the row back exactly as it was,
// and the slept_at stamp that identifies this kill's fence so the CLI clears or
// rolls back only its own write.
type sessionKillFence struct {
	sessionID string
	sleptAt   string
	restore   map[string]string
}

// sessionKillFenceAttempts bounds the re-read/re-decide loop around a
// revision-fenced write. A precondition failure is an observation, not a
// verdict: the revision can move for an unrelated key, and on stores that
// emulate the fence over a whole-row revision even a derived-column rewrite
// moves it. Each retry re-reads and re-checks the guard before writing again.
const sessionKillFenceAttempts = 3

// errSessionKillFenceSuperseded reports that the row no longer satisfies the
// guard of a fence write: it closed before the kill could record its intent,
// or someone else rewrote it after the fence landed.
var errSessionKillFenceSuperseded = errors.New("session row changed underneath the kill")

// writeSessionKillFence durably records the kill intent before the runtime is
// torn down. Where the store resolves a conditional writer the write is fenced
// on the revision of the row it observed, so it cannot silently overwrite a
// transition that landed between the read and the write; otherwise it is the
// same unconditional batch write the post-kill sync always was.
func writeSessionKillFence(store beads.Store, sessionID string, now time.Time) (*sessionKillFence, error) {
	patch := session.KillPendingPatch(now)
	patch["synced_at"] = now.UTC().Format(time.RFC3339)
	fence := &sessionKillFence{sessionID: sessionID, sleptAt: patch["slept_at"]}
	err := applySessionKillFencePatch(store, sessionID, func(b beads.Bead) (map[string]string, bool) {
		if b.Status == "closed" {
			return nil, false
		}
		// The kill clears a user-hold intent (SleepPatch); its held_until goes
		// too, so no heartbeat-shaped hold is left behind.
		write := make(map[string]string, len(patch)+1)
		for key, value := range patch {
			write[key] = value
		}
		if session.Holds(b.Metadata, now).In&session.HoldUser != 0 {
			write["held_until"] = ""
		}
		// Capture the pre-image from the same observation the fenced write is
		// conditioned on, so a rollback restores the row the kill replaced.
		restore := make(map[string]string, len(write))
		for key := range write {
			restore[key] = b.Metadata[key]
		}
		fence.restore = restore
		return write, true
	})
	if err != nil {
		return nil, err
	}
	return fence, nil
}

// owns reports whether b still carries this kill's fence.
func (f *sessionKillFence) owns(b beads.Bead) bool {
	return b.Status != "closed" &&
		strings.TrimSpace(b.Metadata["state"]) == string(session.StateAsleep) &&
		strings.TrimSpace(b.Metadata["state_reason"]) == session.KillPendingReason &&
		strings.TrimSpace(b.Metadata["slept_at"]) == f.sleptAt
}

// clear lifts the fence after the runtime is confirmed gone. The row keeps the
// asleep/killed intent; only the kill-pending marker goes, handing the session
// back to the ordinary lifecycle rules (which is what restarts it).
func (f *sessionKillFence) clear(store beads.Store) error {
	return applySessionKillFencePatch(store, f.sessionID, func(b beads.Bead) (map[string]string, bool) {
		if !f.owns(b) {
			return nil, false
		}
		return map[string]string{"state_reason": ""}, true
	})
}

// rollback restores the pre-kill values after a Stop that left the runtime
// alive, so the row does not claim a runtime is gone when it is not. It only
// touches a row that still carries this kill's fence: if anything else has
// rewritten the row since, that newer state is left alone.
func (f *sessionKillFence) rollback(store beads.Store) error {
	return applySessionKillFencePatch(store, f.sessionID, func(b beads.Bead) (map[string]string, bool) {
		if !f.owns(b) {
			return nil, false
		}
		return f.restore, true
	})
}

// applySessionKillFencePatch reads the row, asks decide for the patch to write
// against that observation, and writes it: revision-fenced when the store
// resolves a conditional writer (re-reading and re-deciding on a precondition
// failure), unconditionally otherwise. decide returning false means the row no
// longer qualifies and nothing is written (errSessionKillFenceSuperseded).
func applySessionKillFencePatch(store beads.Store, sessionID string, decide func(beads.Bead) (map[string]string, bool)) error {
	writer, _, err := beads.ResolveConditionalWriter(store)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < sessionKillFenceAttempts; attempt++ {
		b, err := store.Get(sessionID)
		if err != nil {
			return err
		}
		patch, ok := decide(b)
		if !ok {
			return errSessionKillFenceSuperseded
		}
		if len(patch) == 0 {
			return nil
		}
		if writer == nil {
			return store.SetMetadataBatch(sessionID, patch)
		}
		lastErr = writer.UpdateIfMatch(sessionID, b.Revision, beads.UpdateOpts{Metadata: patch})
		if lastErr == nil || !beads.IsPreconditionFailed(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("conditional write kept losing to concurrent updates: %w", lastErr)
}
