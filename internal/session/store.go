package session

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file extends the session-class domain wrapper (InfoStore) with the
// WRITE half of the front door per OBJECT-MODEL-FRONT-DOOR-DESIGN sec 3.1. The
// read half (Get / List, projecting beads.Bead -> session.Info via
// InfoFromPersistedBead) already lives in info_store.go. Together they form the
// single typed seam over a session-class bead store: callers speak session.Info
// / session.State / session.MetadataPatch, and beads.Bead / SetMetadataBatch /
// Update / Close are confined inside the impl.
//
// PHASE 0 STATUS: these write methods are the skeleton front door. Their
// SIGNATURES are the contract Phase 4 routes call sites through; the bodies
// already emit byte-identical bead writes to the raw ops they replace
// (ApplyPatch == setMetaBatch == store.SetMetadataBatch with empty-skip), so a
// recording-fake store can prove parity now. No production caller is routed
// through them yet — that is Phase 4/5.

// ApplyPatch applies a MetadataPatch to the session bead identified by id. It is
// the single write chokepoint for session metadata transitions: every typed
// write method below funnels through it, and it is the byte-identical
// replacement for setMetaBatch(store, id, patch) (cmd/gc/session_beads.go) and
// the ~20 reconciler SetMetadataBatch(session.ID, patch) sites.
//
// An empty patch is a no-op (matching setMetaBatch). Empty-string values in the
// patch are written verbatim; the cross-backend contract that an empty-string
// metadata value reads back as empty (observationally "cleared") is pinned by
// TestMetadataEmptyStringClearContract.
func (s *InfoStore) ApplyPatch(id string, patch MetadataPatch) error {
	if len(patch) == 0 {
		return nil
	}
	if err := s.store.SetMetadataBatch(id, map[string]string(patch)); err != nil {
		return fmt.Errorf("applying session patch to %q: %w", id, err)
	}
	return nil
}

// SetState heals a session to the given lifecycle state with a state_reason.
// It replaces the canonical state-heal SetMetadataBatch(id, {state, state_reason})
// in session_reconcile.go (healState / healStateWithRollback).
func (s *InfoStore) SetState(id string, state State, reason string) error {
	return s.ApplyPatch(id, MetadataPatch{
		"state":        string(state),
		"state_reason": reason,
	})
}

// Sleep records a non-terminal sleep/drain result via SleepPatch. It replaces
// the max-age and idle-timeout sleep writes in session_reconciler.go.
func (s *InfoStore) Sleep(id, reason string, now time.Time) error {
	return s.ApplyPatch(id, SleepPatch(now, reason))
}

// BeginDrainAckStopPending moves a drain-acked session into durable
// stop-pending state via DrainAckStopPendingPatch. Replaces markDrainAckStopPending.
func (s *InfoStore) BeginDrainAckStopPending(id string, now time.Time) error {
	return s.ApplyPatch(id, DrainAckStopPendingPatch(now))
}

// RequestRestart records a controller handoff to a fresh provider conversation
// via RestartRequestPatch. Replaces the restart-request write in session_reconciler.go.
func (s *InfoStore) RequestRestart(id, sessionKey string, now time.Time) error {
	return s.ApplyPatch(id, RestartRequestPatch(sessionKey, now))
}

// ResetConfigDrift records an in-place named-session repair after core config
// drift via ConfigDriftResetPatch. Replaces the config-drift reset writes in
// session_reconciler.go and soft_reload.go.
func (s *InfoStore) ResetConfigDrift(id string, next State, sessionKey string, now time.Time) error {
	return s.ApplyPatch(id, ConfigDriftResetPatch(next, sessionKey, now))
}

// SetWaitHold sets or clears the wait-hold + sleep-intent markers. Replaces the
// SetMetadataBatch(sessionID, {wait_hold, sleep_intent}) writes in cmd_wait.go.
// When on is false both keys are cleared (empty-string write).
func (s *InfoStore) SetWaitHold(id string, on bool, reason string) error {
	if on {
		return s.ApplyPatch(id, MetadataPatch{
			"wait_hold":    reason,
			"sleep_intent": reason,
		})
	}
	return s.ApplyPatch(id, MetadataPatch{
		"wait_hold":    "",
		"sleep_intent": "",
	})
}

// RecordCurrentBead stamps the work bead a session is currently processing.
// Replaces recordCurrentBeadIDOnWake (session_bead_cycle.go).
func (s *InfoStore) RecordCurrentBead(id, beadID string) error {
	return s.ApplyPatch(id, MetadataPatch{CurrentBeadIDKey: beadID})
}

// GetState returns the persisted lifecycle state for id and whether the bead is
// closed. It replaces the Get(id) + read .Status/.Metadata["state"] pattern at
// the reconciler / session_beads close-path sites. Returns ErrSessionNotFound
// when no session bead exists.
func (s *InfoStore) GetState(id string) (state State, closed bool, err error) {
	info, err := s.Get(id)
	if err != nil {
		return "", false, err
	}
	return info.State, info.Closed, nil
}

// Close closes the session bead with terminal close metadata via ClosePatch,
// then sets status closed. It is the front door for closeBead /
// closeFailedCreateBead. stateCode is the canonical short state code recorded
// before close; ClosePatch expands it to a validator-safe close_reason.
//
// Reports whether the bead was actually closed (false when it was already
// closed). PHASE 0: the work-reassignment side effect that closeBead performs
// (releaseWorkFromClosedSessionBead) is intentionally NOT part of this method —
// that is a cross-class WORK op owned by the Phase 6 work/assignment API.
func (s *InfoStore) Close(id, stateCode string, now time.Time) (bool, error) {
	info, err := s.Get(id)
	if err != nil {
		return false, err
	}
	if info.Closed {
		return false, nil
	}
	if err := s.ApplyPatch(id, ClosePatch(now, stateCode)); err != nil {
		return false, err
	}
	if err := s.store.Close(id); err != nil {
		return false, fmt.Errorf("closing session %q: %w", id, err)
	}
	return true, nil
}

// Store returns the embedded strongly-typed session-class bead store. It is a
// transition-period accessor for call sites that still need raw bead access
// while their reads/writes are migrated behind the typed methods above. New
// code must prefer the typed methods; this exists so Phase 4/5 can land
// incrementally without a flag-day rewrite.
func (s *InfoStore) Store() beads.SessionStore { return s.store }
