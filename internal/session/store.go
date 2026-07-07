package session

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file extends the session-class domain wrapper (Store) with the
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
func (s *Store) ApplyPatch(id string, patch MetadataPatch) error {
	if len(patch) == 0 {
		return nil
	}
	// Return the bare store error: this method confines the write codec, it does
	// not re-message failures. Callers (the reconciler, setMetaBatch, the circuit
	// breaker) log/wrap the error themselves, and several tests assert their exact
	// diagnostic text — wrapping here would change that caller-visible text and
	// break runtime fidelity.
	return s.store.SetMetadataBatch(id, map[string]string(patch))
}

// SetState heals a session to the given lifecycle state with a state_reason.
// It replaces the canonical state-heal SetMetadataBatch(id, {state, state_reason})
// in session_reconcile.go (healState / healStateWithRollback).
func (s *Store) SetState(id string, state State, reason string) error {
	return s.ApplyPatch(id, MetadataPatch{
		"state":        string(state),
		"state_reason": reason,
	})
}

// Sleep records a non-terminal sleep/drain result via SleepPatch. It replaces
// the max-age and idle-timeout sleep writes in session_reconciler.go.
func (s *Store) Sleep(id, reason string, now time.Time) error {
	return s.ApplyPatch(id, SleepPatch(now, reason))
}

// BeginDrainAckStopPending moves a drain-acked session into durable
// stop-pending state via DrainAckStopPendingPatch. Replaces markDrainAckStopPending.
func (s *Store) BeginDrainAckStopPending(id string, now time.Time) error {
	return s.ApplyPatch(id, DrainAckStopPendingPatch(now))
}

// RequestRestart records a controller handoff to a fresh provider conversation
// via RestartRequestPatch. Replaces the restart-request write in session_reconciler.go.
func (s *Store) RequestRestart(id, sessionKey string, now time.Time) error {
	return s.ApplyPatch(id, RestartRequestPatch(sessionKey, now))
}

// ResetConfigDrift records an in-place named-session repair after core config
// drift via ConfigDriftResetPatch. Replaces the config-drift reset writes in
// session_reconciler.go and soft_reload.go.
func (s *Store) ResetConfigDrift(id string, next State, sessionKey string, now time.Time) error {
	return s.ApplyPatch(id, ConfigDriftResetPatch(next, sessionKey, now))
}

// SetWaitHold sets or clears the wait-hold + sleep-intent markers. Replaces the
// SetMetadataBatch(sessionID, {wait_hold, sleep_intent}) writes in cmd_wait.go.
// When on is false both keys are cleared (empty-string write).
func (s *Store) SetWaitHold(id string, on bool, reason string) error {
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

// setMetadataValue is the single-key write chokepoint. It is the byte-identical
// replacement for the raw store.SetMetadata(id, key, value) sites that write a
// single session-attribute key. Unlike ApplyPatch (which emits SetMetadataBatch),
// this emits SetMetadata so the bead op is identical to the raw single-key write
// it replaces.
func (s *Store) setMetadataValue(id, key, value string) error {
	// Bare store error — callers own their diagnostic text (see ApplyPatch).
	return s.store.SetMetadata(id, key, value)
}

// SetMarker writes a single session-attribute marker key. It is the front door
// for the raw store.SetMetadata(session.ID, key, value) sites: the stranded
// throttle marker (session_reconciler.go), the sleep_intent clear, and the
// city-stop sleep_reason (cmd_stop.go). It emits a single SetMetadata op,
// byte-identical to the raw write. An empty value clears the key per the
// empty-string-clear contract.
func (s *Store) SetMarker(id, key, value string) error {
	return s.setMetadataValue(id, key, value)
}

// RecordCurrentBead stamps the work bead a session is currently processing.
// Replaces recordCurrentBeadIDOnWake (session_bead_cycle.go), which uses a
// single-key SetMetadata write — so this emits SetMetadata, not a batch.
func (s *Store) RecordCurrentBead(id, beadID string) error {
	return s.setMetadataValue(id, CurrentBeadIDKey, beadID)
}

// CloseWithoutReason closes the session bead identified by id without stamping
// terminal close metadata. It is the front door for the raw store.Close(id)
// call in closeBead, which stamps ClosePatch via setMetaBatch separately and
// then closes the bead. It emits a single Close op, byte-identical to the raw
// write.
func (s *Store) CloseWithoutReason(id string) error {
	// Bare store error — callers own their diagnostic text (see ApplyPatch).
	return s.store.Close(id)
}

// Backed reports whether this front door wraps a usable (non-nil) underlying
// store. It is the typed probe for the `sessFront == nil || sessFront.Store().Store == nil`
// guard at the controller/CLI roots: a front door constructed over a nil store
// (the documented typed-nil pattern, where construction yields a real nil
// *Store when the store is nil) reports false, and so does a nil receiver.
// Callers use `if !sessFront.Backed() { return }` instead of reaching for the
// raw embedded store to nil-check it.
func (s *Store) Backed() bool {
	return s != nil && s.store.Store != nil
}

// CircuitResetGeneration returns the persisted session-circuit-breaker reset
// generation metadata value for id, verbatim (the raw string; "" when unset).
//
// It is the front door for the raw store.Get(sessionID) + read
// .Metadata[SessionCircuitResetGenerationMetadataKey] pattern in
// loadPersistedSessionCircuitResetGeneration (cmd/gc/session_circuit_breaker.go).
// The bead read and the metadata-key access are confined here; the caller still
// owns parsing the value and observing it into the breaker, and owns its own
// diagnostic wrapping (the error is returned bare — see ApplyPatch). It does NOT
// validate the bead as a session bead: the raw read it replaces did not either,
// so a non-session bead carrying the key reads back identically.
func (s *Store) CircuitResetGeneration(id string) (string, error) {
	b, err := s.store.Get(id)
	if err != nil {
		return "", err
	}
	return b.Metadata[SessionCircuitResetGenerationMetadataKey], nil
}

// PersistedMarkers is a narrow typed view of the persisted session-attribute
// markers the wait paths read off a session bead: the bead Title (used to build
// the wait bead title), the tmux session_name, the continuation_epoch (stamped
// onto wait beads as registered_epoch), and the sleep_reason (consulted when
// clearing a wait-hold). It carries the raw bead fields verbatim.
type PersistedMarkers struct {
	Title             string
	SessionName       string
	ContinuationEpoch string
	SleepReason       string
}

// PersistedMarkers returns the persisted Title / session_name /
// continuation_epoch / sleep_reason markers for id, verbatim (each "" when
// unset).
//
// It is the front door for the raw store.Get(sessionID) + read .Title/.Metadata[...]
// pattern in the wait registration (cmd_wait.go session-wait creation), the
// closed-wait retry path, and the wait-hold clear path. The bead read and the
// field access are confined here; the caller still owns observing the values and
// its own diagnostic wrapping (the error is returned bare — see ApplyPatch).
// Like CircuitResetGeneration, it does NOT validate the bead as a session bead:
// the raw reads it replaces did not either.
func (s *Store) PersistedMarkers(id string) (PersistedMarkers, error) {
	b, err := s.store.Get(id)
	if err != nil {
		return PersistedMarkers{}, err
	}
	return PersistedMarkers{
		Title:             b.Title,
		SessionName:       b.Metadata["session_name"],
		ContinuationEpoch: b.Metadata["continuation_epoch"],
		SleepReason:       b.Metadata["sleep_reason"],
	}, nil
}

// GetState returns the persisted lifecycle state for id and whether the bead is
// closed. It replaces the Get(id) + read .Status/.Metadata["state"] pattern at
// the reconciler / session_beads close-path sites. Returns ErrSessionNotFound
// when no session bead exists.
func (s *Store) GetState(id string) (state State, closed bool, err error) {
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
func (s *Store) Close(id, stateCode string, now time.Time) (bool, error) {
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

// SetStatusOpen sets the session bead status to "open". It is the front door
// for the raw store.Update(id, UpdateOpts{Status: &"open"}) writes in the
// reopen and named-session retire-archive paths (session_beads.go), which open
// the bead row after stamping archive/reopen metadata via setMetaBatch. It
// emits a single Update op with only Status set, byte-identical to the raw
// write.
func (s *Store) SetStatusOpen(id string) error {
	open := "open"
	if err := s.store.Update(id, beads.UpdateOpts{Status: &open}); err != nil {
		return err
	}
	return nil
}

// RepairType sets the session bead Type to the canonical session bead type. It
// is the front door for the empty-type repair write in session_beads.go, where
// a session-labeled bead with an empty Type (left by a schema migration or a
// partial write) is healed back to the session type. It emits a single Update
// op with only Type set, byte-identical to the raw write.
func (s *Store) RepairType(id string) error {
	t := BeadType
	if err := s.store.Update(id, beads.UpdateOpts{Type: &t}); err != nil {
		return err
	}
	return nil
}

// Store returns the embedded strongly-typed session-class bead store. It is a
// transition-period accessor for call sites that still need raw bead access
// while their reads/writes are migrated behind the typed methods above. New
// code must prefer the typed methods; this exists so Phase 4/5 can land
// incrementally without a flag-day rewrite.
func (s *Store) Store() beads.SessionStore { return s.store }
