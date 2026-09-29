package main

import (
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
// reader. store is reached only for a row that DOES have a staged handoff,
// to load and update the staged message bead itself, which is not part of
// the session row and has no Info projection; messaging beads live on the
// work store today (see resolveMailMessagesStore's own "Identity today: the
// work store"), the same store the reconciler already holds sessions on, so
// no second store resolution is threaded in here.
//
// A staged handoff is released only when the session bead's current
// instance_token differs from the token recorded on the message at staging
// time -- proof a later, genuine successor incarnation has adopted the
// session, never the incarnation that staged it. Absent that proof, a
// handoff stays staged until either a successor arrives or
// controllerRestartTimeout(cfg) elapses since it was staged, at which point
// it is marked failed (audited via handoffReleaseAttemptedAtKey so a later
// tick does not re-fire the failure) but left staged -- never exposed to the
// originating incarnation.
//
// Per-row errors are logged and skipped rather than aborting the tick: a
// staged handoff that cannot be resolved this tick is retried next tick, and
// one row's store error must not block every other row's reconciliation.
func releaseStagedSelfHandoffs(store beads.Store, rows []sessionpkg.ReconcileSession, cfg *config.City, clk clock.Clock, rec events.Recorder) (released, failed int) {
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

		message, err := store.Get(messageID)
		if err != nil {
			log.Printf("releaseStagedSelfHandoffs: loading staged message %s for session %s: %v", messageID, sessionAddress, err)
			continue
		}
		currentToken := strings.TrimSpace(row.Info.InstanceToken)
		stagedForToken := message.Metadata[mail.StagedForTokenMetadataKey]

		if currentToken != "" && currentToken != stagedForToken {
			if err := store.Update(message.ID, beads.UpdateOpts{
				RemoveLabels: []string{mail.StagedHandoffLabel},
				Metadata:     map[string]string{mail.StagedMetadataKey: "false"},
			}); err != nil {
				log.Printf("releaseStagedSelfHandoffs: releasing staged message %s for session %s: %v", message.ID, sessionAddress, err)
				continue
			}
			if err := store.SetMetadataBatch(sessionID, map[string]string{
				handoffStagedMessageIDKey:  "",
				handoffStageCommittedAtKey: "",
			}); err != nil {
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

		stagedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(row.Info.HandoffStageCommittedAt))
		if err != nil || now.Sub(stagedAt) < controllerRestartTimeout(cfg) {
			// Not timed out yet (or no parseable stage time to measure from):
			// wait for a successor on a later tick.
			continue
		}
		if err := store.SetMetadataBatch(sessionID, map[string]string{
			handoffReleaseAttemptedAtKey: now.Format(time.RFC3339),
		}); err != nil {
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
