package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// releaseStagedSelfHandoffs scans this tick's session rows for a durably
// staged self-handoff (Info.HandoffStagedMessageID set) and, for each one,
// either releases it to a genuine successor, marks it failed on timeout, or
// leaves it waiting. It reads every session-side fact off the row's already-
// projected Info -- never a per-row store.Get -- so a tick with no staged
// handoffs costs zero Gets (TestReconcileSessionBeadsFastPathGetBudget): rows
// are projected once from bead data the caller already fetched in bulk
// before the tick began, exactly like every other fast-path reconciler
// reader. The message handle is reached only for a row that DOES have a
// staged handoff, to load and update the staged message bead itself, which is
// not part of the session row and has no Info projection. That bead lives in
// the messaging class and the row in the sessions class: the two are one store
// only while neither class is relocated, so each side is read and written
// through its own typed handle. A zero mail handle skips every staged row
// (logged), since there is no store to read the brief from.
//
// A staged handoff is released only when the session bead's current
// instance_token differs from the token recorded on the message at staging
// time and the row shows a confirmed start (sessionStartConfirmed) -- proof a
// later, genuine successor incarnation runs the session, never the
// incarnation that staged it nor a token minted for a start that has not
// happened. A brief that a later staging closed or superseded is never
// released or written off. Before either write, the row is re-read live and
// the write is skipped unless the row still points at the evaluated brief
// and has not written it off; where the sessions store resolves a
// conditional writer, the write itself is conditional too
// (clearStagedHandoffPointer, UpdateIfMatch). Absent that proof, a
// handoff stays staged until either a successor arrives or
// controllerRestartTimeout(cfg) elapses since it was staged, at which point
// it is marked failed (audited via handoffReleaseAttemptedAtKey so a later
// tick does not re-fire the failure) but left staged -- never exposed to the
// originating incarnation.
//
// Per-row errors are logged and skipped rather than aborting the tick: a
// staged handoff that cannot be resolved this tick is retried next tick, and
// one row's store error must not block every other row's reconciliation.
func releaseStagedSelfHandoffs(mailStore beads.MailStore, sessions beads.SessionStore, rows []sessionpkg.ReconcileSession, cfg *config.City, clk clock.Clock, rec events.Recorder) (released, failed int) {
	now := clk.Now().UTC()
	for _, row := range rows {
		messageID := strings.TrimSpace(row.Info.HandoffStagedMessageID)
		if messageID == "" {
			continue
		}
		sessionID := row.Info.ID
		sessionAddress := row.Info.Alias
		if strings.TrimSpace(row.Info.HandoffReleaseAttemptedAt) != "" {
			// Already gave up waiting for a successor on an earlier tick;
			// never re-fire the failure for the same staged handoff.
			continue
		}

		// The zero mail handle is what a reconcile pass carries when its caller
		// supplied none (withHandoffMailStore). It is a presence check, not a
		// store-identity question, and the zero handle has no dynamic store type,
		// so the comparison cannot panic.
		if mailStore == (beads.MailStore{}) {
			log.Printf("releaseStagedSelfHandoffs: no mail store to load staged message %s for session %s; leaving it staged", messageID, sessionAddress)
			continue
		}
		message, err := mailStore.Get(messageID)
		if err != nil {
			log.Printf("releaseStagedSelfHandoffs: loading staged message %s for session %s: %v", messageID, sessionAddress, err)
			continue
		}
		if message.Status == "closed" || strings.TrimSpace(message.Metadata[mail.SupersededByMetadataKey]) != "" {
			// A later staging superseded this brief after the tick's snapshot
			// was taken. It is terminal: never release it or write it off.
			continue
		}
		currentToken := strings.TrimSpace(row.Info.InstanceToken)
		stagedForToken := message.Metadata[mail.StagedForTokenMetadataKey]
		successorStarted := currentToken != "" && currentToken != stagedForToken && sessionStartConfirmed(row.Info)

		var stagedAt time.Time
		if !successorStarted {
			stagedAt, err = time.Parse(time.RFC3339, strings.TrimSpace(row.Info.HandoffStageCommittedAt))
			if err != nil || now.Sub(stagedAt) < controllerRestartTimeout(cfg) {
				// Not timed out yet (or no parseable stage time to measure from):
				// wait for a successor on a later tick.
				continue
			}
		}

		// The row is this tick's snapshot; gc handoff may have restaged since.
		// Re-read it live and act only while it still points at the brief
		// evaluated above and nothing has written that brief off.
		live, ok := liveStagedHandoffRow(sessions, sessionID, messageID, sessionAddress)
		if !ok {
			continue
		}
		writer, _, err := beads.ResolveConditionalWriter(sessions)
		if err != nil {
			log.Printf("releaseStagedSelfHandoffs: resolving conditional writer for session %s: %v", sessionAddress, err)
			continue
		}

		if successorStarted {
			if err := mailStore.Update(message.ID, beads.UpdateOpts{
				RemoveLabels: []string{mail.StagedHandoffLabel},
				Metadata:     map[string]string{mail.StagedMetadataKey: "false"},
			}); err != nil {
				log.Printf("releaseStagedSelfHandoffs: releasing staged message %s for session %s: %v", message.ID, sessionAddress, err)
				continue
			}
			if err := clearStagedHandoffPointer(sessions, writer, live, messageID); err != nil {
				log.Printf("releaseStagedSelfHandoffs: clearing staged markers on session %s: %v", sessionAddress, err)
				continue
			}
			recordHandoffEvent(rec, events.SessionHandoffSuccessorStarted, "gc", sessionAddress, events.SessionHandoffSuccessorStartedPayload{
				SessionKey: sessionAddress,
				MessageID:  message.ID,
			})
			recordHandoffEvent(rec, events.SessionHandoffReleased, "gc", sessionAddress, events.SessionHandoffReleasedPayload{
				SessionKey: sessionAddress,
				MessageID:  message.ID,
			})
			released++
			continue
		}

		if err := recordHandoffReleaseAttempt(sessions, writer, live, now); err != nil {
			log.Printf("releaseStagedSelfHandoffs: recording release-attempt for session %s: %v", sessionAddress, err)
			continue
		}
		recordHandoffEvent(rec, events.SessionHandoffFailed, "gc", sessionAddress, events.SessionHandoffFailedPayload{
			SessionKey: sessionAddress,
			Reason:     "no successor session started before the handoff release timeout",
		})
		failed++
	}
	return released, failed
}

// recordHandoffReleaseAttempt stamps the release-attempt marker on the session
// row that live was read from. With a conditional writer the stamp lands only
// while the row is still at live's revision; without one it is written
// unconditionally, after the caller's live re-read.
func recordHandoffReleaseAttempt(sessions beads.SessionStore, writer beads.ConditionalWriter, live beads.Bead, now time.Time) error {
	marker := map[string]string{handoffReleaseAttemptedAtKey: now.Format(time.RFC3339)}
	if writer != nil {
		return writer.UpdateIfMatch(live.ID, live.Revision, beads.UpdateOpts{Metadata: marker})
	}
	return sessions.SetMetadataBatch(live.ID, marker)
}

// liveStagedHandoffRow re-reads the session row through the live handle and
// returns it only while it still points at messageID and carries no
// release-attempt marker. Anything else means the tick's snapshot is stale (a
// restage landed, or an earlier pass wrote the brief off), so the caller
// leaves the row alone this tick.
func liveStagedHandoffRow(sessions beads.SessionStore, sessionID, messageID, sessionAddress string) (beads.Bead, bool) {
	live, err := beads.HandlesFor(sessions).Live.Get(sessionID)
	if err != nil {
		log.Printf("releaseStagedSelfHandoffs: re-reading session %s: %v", sessionAddress, err)
		return beads.Bead{}, false
	}
	if pointer := strings.TrimSpace(live.Metadata[handoffStagedMessageIDKey]); pointer != messageID {
		log.Printf("releaseStagedSelfHandoffs: session %s now stages %q, not %s; skipping the stale snapshot this tick", sessionAddress, pointer, messageID)
		return beads.Bead{}, false
	}
	if strings.TrimSpace(live.Metadata[handoffReleaseAttemptedAtKey]) != "" {
		log.Printf("releaseStagedSelfHandoffs: session %s already wrote off staged message %s; skipping the stale snapshot this tick", sessionAddress, messageID)
		return beads.Bead{}, false
	}
	return live, true
}

// clearStagedHandoffPointer clears the session's staged-brief pointer and its
// stage time. With a conditional writer it swaps the pointer only while it
// still names messageID, and clears the stage time only after that swap wins
// and only while it is still the value live read, so a newer brief's pointer
// is never wiped. Without one it writes both keys, after the caller's live
// re-read.
func clearStagedHandoffPointer(sessions beads.SessionStore, writer beads.ConditionalWriter, live beads.Bead, messageID string) error {
	if writer == nil {
		return sessions.SetMetadataBatch(live.ID, map[string]string{
			handoffStagedMessageIDKey:  "",
			handoffStageCommittedAtKey: "",
		})
	}
	swapped, err := writer.CompareAndSetMetadataKey(live.ID, handoffStagedMessageIDKey, messageID, "")
	if err != nil {
		return err
	}
	if !swapped {
		return fmt.Errorf("session %s no longer stages %s", live.ID, messageID)
	}
	if _, err := writer.CompareAndSetMetadataKey(live.ID, handoffStageCommittedAtKey, live.Metadata[handoffStageCommittedAtKey], ""); err != nil {
		return err
	}
	return nil
}

// sessionStartConfirmed reports whether info shows a confirmed runtime start:
// state active (or its awake alias) with no pending-create claim or
// pending-create start time, the state ConfirmStartedPatch and
// CommitStartedPatch leave. preWakePatch mints the successor's instance_token
// before the runtime starts and leaves the row creating with a
// pending_create_started_at stamp, so a differing token alone is not proof a
// successor runs. Of the plan's two options this takes the start-confirmed
// predicate read from the projected Info, which needs no extra store or
// runtime read; awake alone is not enough, because a heal can mark a
// still-claimed creation awake before the provider start returns.
func sessionStartConfirmed(info sessionpkg.Info) bool {
	switch info.State {
	case sessionpkg.StateActive, sessionpkg.StateAwake:
	default:
		return false
	}
	return !info.PendingCreateClaim && strings.TrimSpace(info.PendingCreateStartedAt) == ""
}
