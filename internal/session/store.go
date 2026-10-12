package session

import (
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

// startCommitMaxAttempts bounds how many times CommitStartedIfCurrent re-reads
// and re-decides when another writer keeps winning its revision fence.
const startCommitMaxAttempts = 3

// CommitStartedIfCurrent commits a start result only while the row still
// passes PendingCreateLease.CommitVerdict against expected, the row the start
// was prepared from. The lease is re-read under the mutation lock shared with
// pending-create rollback and incarnation allocation, which fences those
// writers in this process only.
//
// Writers in other processes (a `gc session kill` CLI, a successor
// controller), and a cached re-read that has not seen them, are fenced by the
// store. Where it resolves a conditional writer (beads.ResolveConditionalWriter)
// the patch is written with UpdateIfMatch on the re-read revision. If another
// writer lands first, the row is re-read and the verdict runs again, up to
// startCommitMaxAttempts, so the patch lands only on a row that still passes.
// A row that no longer passes reports (false, nil) with nothing written. A
// writer that keeps winning leaves nothing written and returns the
// precondition error, which the caller handles like any failed commit.
//
// Without a conditional writer (conditional writes off, or auto on a store
// that cannot fence) the patch is written unconditionally after the re-read,
// as before, which leaves the window between that read and the write. A
// require-mode store that cannot fence, and a store that reports
// beads.ErrConditionalWriteUnsupported at call time, return an error.
func (s *Store) CommitStartedIfCurrent(expected Info, patch MetadataPatch) (bool, error) {
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return false, fmt.Errorf("committing start of session %q: %w", expected.ID, err)
	}
	applied := false
	err = WithSessionMutationLock(expected.ID, func() error {
		var conflict error
		for attempt := 0; attempt < startCommitMaxAttempts; attempt++ {
			bead, err := s.validatedBead(expected.ID)
			if err != nil {
				return err
			}
			if LeaseFromInfo(expected).CommitVerdict(LeaseFromInfo(infoFromPersistedBead(bead))) != LeaseCommit {
				return nil
			}
			if writer == nil || len(patch) == 0 {
				if err := s.ApplyPatch(expected.ID, patch); err != nil {
					return err
				}
				applied = true
				return nil
			}
			err = writer.UpdateIfMatch(expected.ID, bead.Revision, beads.UpdateOpts{Metadata: map[string]string(patch)})
			switch {
			case err == nil:
				applied = true
				return nil
			case !beads.IsPreconditionFailed(err):
				return fmt.Errorf("committing start of session %q: %w", expected.ID, err)
			}
			conflict = err
		}
		return fmt.Errorf("committing start of session %q: lost the revision fence %d times: %w", expected.ID, startCommitMaxAttempts, conflict)
	})
	return applied, err
}

// WithPendingCreateRollback runs the rollback transaction only while the exact
// observed creation is still pending. Completion and incarnation allocation use
// the same process-local lock. fn must not acquire that lock again or call a
// provider; retired-session cleanup belongs after this critical section.
func (s *Store) WithPendingCreateRollback(expected Info, fn func() error) (bool, error) {
	applied := false
	err := WithSessionMutationLock(expected.ID, func() error {
		current, _, err := s.GetPersistedResponse(expected.ID)
		if err != nil {
			return err
		}
		if !LeaseFromInfo(expected).CanRollback(LeaseFromInfo(current)) {
			return nil
		}
		if err := fn(); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

// ApplyPatchIfLifecycleUnchanged persists patch for expected.ID only while the
// durable row still carries the lifecycle facts expected was read with. It is
// the write path for a patch DECIDED from an earlier read (the reconciler's
// advisory status heal, computed from its tick snapshot): an unconditional
// write of such a patch is a lost update. A `gc session suspend`, wake, kill, or
// close that lands after the read would be overwritten by a decision that never
// saw it.
//
// "Lifecycle facts" are exactly what LifecycleInputFromInfo carries: the
// open/closed status plus the persisted keys the lifecycle projection reads
// (state, sleep_reason, held_until, quarantined_until, session_key,
// started_config_hash, the pending-create lease, wake_request, pin_awake, ...).
// Writes to other keys (a claim stamp, a nudge timestamp) do not block the
// patch.
//
// The row is re-read, and the patch is refused with (false, nil) when the row
// is closed or its lifecycle facts differ from expected's. Where the store
// resolves a conditional writer (beads.ResolveConditionalWriter), the write is
// then fenced on the re-read row's revision, so a writer that lands between the
// re-read and the write also wins: the fenced write is refused with (false,
// nil). The revision is passed through as-is: bd revisions are signed, and
// whether a token is usable is the store's call. Without a conditional writer
// the patch is written unconditionally after the re-read check, which leaves
// only the window between that re-read and the write.
//
// A refused patch writes nothing, and the caller must not fold it. A
// require-mode store that cannot fence, and a store that reports
// beads.ErrConditionalWriteUnsupported at call time, return an error rather
// than falling back to an unconditional write.
func (s *Store) ApplyPatchIfLifecycleUnchanged(expected Info, patch MetadataPatch) (bool, error) {
	if len(patch) == 0 {
		return false, nil
	}
	bead, err := s.validatedBead(expected.ID)
	if err != nil {
		return false, err
	}
	if bead.Status == "closed" {
		return false, nil
	}
	if !sameLifecycleFacts(expected, infoFromPersistedBead(bead)) {
		return false, nil
	}
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return false, fmt.Errorf("updating session %q: %w", expected.ID, err)
	}
	if writer == nil {
		if err := s.ApplyPatch(expected.ID, patch); err != nil {
			return false, err
		}
		return true, nil
	}
	err = writer.UpdateIfMatch(expected.ID, bead.Revision, beads.UpdateOpts{Metadata: map[string]string(patch)})
	switch {
	case err == nil:
		return true, nil
	case beads.IsPreconditionFailed(err):
		return false, nil
	default:
		return false, fmt.Errorf("updating session %q: %w", expected.ID, err)
	}
}

// UpdateMetadataFenced writes a patch decided from a fresh read of the row. It
// re-reads the row, asks decide for the patch, and writes it in one Update.
// Where the store resolves a conditional writer the Update is fenced on the
// re-read revision; when another writer lands first, the row is re-read and
// decide runs again, up to attempts times, after which nothing is written.
// Without a conditional writer the patch is written unfenced right after the
// re-read, leaving only that window, as ApplyPatchIfLifecycleUnchanged does. A
// require-mode store that cannot fence returns an error. decide returning an
// empty patch writes nothing. It reports whether a patch was written.
func (s *Store) UpdateMetadataFenced(id string, attempts int, decide func(Info, PersistedResponse) MetadataPatch) (bool, error) {
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return false, fmt.Errorf("updating session %q: %w", id, err)
	}
	for attempt := 0; attempt < attempts; attempt++ {
		bead, err := s.validatedBead(id)
		if err != nil {
			return false, err
		}
		patch := decide(infoFromPersistedBead(bead), PersistedResponseFromBead(bead))
		if len(patch) == 0 {
			return false, nil
		}
		opts := beads.UpdateOpts{Metadata: map[string]string(patch)}
		if writer == nil {
			if err := s.store.Update(id, opts); err != nil {
				return false, err
			}
			return true, nil
		}
		err = writer.UpdateIfMatch(id, bead.Revision, opts)
		switch {
		case err == nil:
			return true, nil
		case beads.IsPreconditionFailed(err):
			continue
		default:
			return false, fmt.Errorf("updating session %q: %w", id, err)
		}
	}
	return false, nil
}

// sameLifecycleFacts reports whether a and b agree on every persisted fact the
// lifecycle projection reads (LifecycleInputFromInfo). Comparing the projected
// inputs rather than a hand-picked key list keeps this check in step with the
// projection when it learns a new key.
func sameLifecycleFacts(a, b Info) bool {
	return reflect.DeepEqual(LifecycleInputFromInfo(a), LifecycleInputFromInfo(b))
}

// ApplyPatchInfo persists patch for info.ID (via ApplyPatch) and returns the
// refreshed Info as a LOCAL fold — info.ApplyPatch(patch) — never a re-Get. It
// is the write-returns-Info chokepoint the reconciler routes its direct
// write+fold two-steps through: a store Get per patch would blow the tick budget
// under Dolt (~2s/bd-op; the reconciler does ~57-61 patch writes per tick), and
// the coherent caller-held Info already carries the pre-image the fold needs, so
// no read is required.
//
// An empty patch is a no-op: it returns info unchanged with no write (matching
// ApplyPatch's len==0 short-circuit). On a persist error the INPUT info is
// returned UNCHANGED with the error — the snapshot never advances past a write
// the store rejected, so an error-ignoring caller stays consistent with the
// store and an error-checking caller can bail.
//
// The fold is byte-identical to re-projecting the patched bead
// (TestInfoApplyPatchMatchesReprojection is the equivalence oracle). It cannot
// express a status close: patches never flip Info.Closed (see info_apply_patch.go),
// so in-memory closes fold via MarkClosed instead, and the one NDI witness close
// (finalizeDrainAckStoppedSession) is the single documented Store.Get refresh.
// The handle-only ApplyPatch(id, patch) form remains for callers that hold no
// coherent Info snapshot.
func (s *Store) ApplyPatchInfo(info Info, patch MetadataPatch) (Info, error) {
	if len(patch) == 0 {
		return info, nil
	}
	if err := s.ApplyPatch(info.ID, patch); err != nil {
		return info, err
	}
	return info.ApplyPatch(patch), nil
}

// UpdateMetadataInfo persists patch for info.ID via a SINGLE
// Store.Update(id, UpdateOpts{Metadata: patch}) and folds the patch onto Info on
// success. It is the write-returns-Info chokepoint for provenance clusters that
// must commit ALL-OR-NOTHING across every supported backend.
//
// One-operation contract (why this is NOT ApplyPatchInfo): ApplyPatch routes
// through SetMetadataBatch, which some backends decompose into one op PER KEY
// (the exec: store issues one `bd` subprocess per map key, in nondeterministic
// order), so a failure on the Nth key leaves an arbitrary subset of the cluster
// committed — a mixed identity/provenance row. A single Update carries the whole
// metadata map in one backend operation: exec: emits one JSON --set-metadata
// subprocess, native Dolt keeps its read/merge/write transaction isolation, and
// the caching/DoltLite stores keep their existing single-write refresh path. The
// trigger/provenance cluster (trigger id, store ref, brain parent, pack,
// workspace, workdir) therefore commits atomically or not at all.
//
// An empty patch is a no-op: it returns info unchanged with no write. On a
// persist error the INPUT info is returned UNCHANGED with the error, so a caller
// that logs-and-continues keeps its pre-write in-memory Info (never a partially
// applied fold) and the durable row is left exactly as the backend left it. On
// success the fold is info.ApplyPatch(patch) — byte-identical to re-projecting
// the patched bead, exactly as ApplyPatchInfo folds.
func (s *Store) UpdateMetadataInfo(info Info, patch MetadataPatch) (Info, error) {
	if len(patch) == 0 {
		return info, nil
	}
	if err := s.store.Update(info.ID, beads.UpdateOpts{Metadata: map[string]string(patch)}); err != nil {
		return info, err
	}
	return info.ApplyPatch(patch), nil
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

// SetCurrentClaim stamps the work bead this session claimed for itself through
// `gc hook --claim` (beadmeta.CurrentClaimBeadIDMetadataKey) — or clears the
// stamp when beadID is empty. It reports whether a write was actually issued.
//
// It is deliberately a different key from RecordCurrentBead's: that one records
// a controller-side assignment the reconciler made at wake time, this one
// records a claim the session made for itself, and a shared key would let the
// two lanes overwrite each other. Callers must clear it on every path that takes
// the work back off the session — a stale stamp names a bead the session no
// longer owns.
//
// The read is validatedBead, so the id is resolved EXACTLY (the bead store's
// Get surfaces a prefix collision as beads.ErrIDCollision) and a non-session
// bead is refused before any write: bd's fuzzy id resolver would otherwise let a
// post-claim update land on a prefix-colliding session bead, which is why the
// claim path historically refused to decorate the session bead at all
// (cmd/gc/cmd_hook_claim.go publishHookClaimRunMap). The write targets the
// canonical bead id the read returned, never the caller's raw identifier.
//
// The current value is compared first and an unchanged value writes nothing: the
// claim path re-runs on every hook tick through its adoption branches, so an
// unconditional write would emit one bead.updated per tick per in-progress bead.
// It emits a single-key SetMetadata, not a batch, matching RecordCurrentBead.
func (s *Store) SetCurrentClaim(id, beadID string) (bool, error) {
	b, err := s.validatedBead(id)
	if err != nil {
		return false, err
	}
	beadID = strings.TrimSpace(beadID)
	if strings.TrimSpace(b.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey]) == beadID {
		return false, nil
	}
	if err := s.setMetadataValue(b.ID, beadmeta.CurrentClaimBeadIDMetadataKey, beadID); err != nil {
		return false, err
	}
	return true, nil
}

// CurrentClaimBeadID returns the id of the work bead this session most recently
// claimed through `gc hook --claim` ("" when unset). It is the read half of
// SetCurrentClaim and the front door for `gc hook current`.
//
// It shares Get's validation and error contract (both route through
// validatedBead): a present-but-non-session bead is ErrSessionNotFound and an
// absent id is the wrapped store not-found error, so a caller can tell "this is
// not my session" from "nothing is claimed".
func (s *Store) CurrentClaimBeadID(id string) (string, error) {
	b, err := s.validatedBead(id)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(b.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey]), nil
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

// terminalCloseMaxAttempts bounds how many times Close re-reads and re-fences
// when a concurrent writer keeps winning the atomic close's revision fence.
const terminalCloseMaxAttempts = 3

// Close closes the session bead and stamps its terminal close metadata
// (ClosePatch). The controller's closeBead / closeFailedCreateBead use its
// sibling CloseWithTerminalPatch, which shares the atomic arm. stateCode is
// the canonical short state code recorded before close; ClosePatch expands it
// to a validator-safe close_reason.
//
// When the backing store provides beads.AtomicConditionalCloser (native Dolt,
// SQLite with a revision column, FileStore, and a CachingStore over any of
// them), the metadata and the closed status commit as ONE write fenced on the
// revision Close observed. No reader can see, and no writer can land between,
// a closed row and its terminal metadata. If a concurrent writer wins the
// fence, the atomic close writes nothing and Close re-reads. It retries, up to
// terminalCloseMaxAttempts. If the row was closed by someone else meanwhile,
// Close reports false. If a writer keeps winning without touching the facts
// the close was decided on, Close returns the precondition error with the row
// still open, and the caller's next pass retries.
//
// expected is the row the caller decided to close. Every row Close reads, the
// first read included, must still carry expected's facts (closePremiseHolds);
// otherwise Close returns ErrSessionCloseSuperseded with nothing written, so a
// wake that lands after the caller's decision, before or after Close's first
// read, is never closed over. The caller's next pass decides again.
//
// Stores WITHOUT the capability (BdStore, the exec store, plain MemStore, a
// legacy SQLite layout, or a store whose conditional writes are disabled) keep
// the historical two-write sequence: SetMetadataBatch(ClosePatch), then Close,
// after the same premise check on the first read. That sequence has a known
// residual race. A writer that lands after the read or between the two
// writes leaves the row status=closed while its metadata still looks live. A
// store that advertises the capability but refuses it at call time with
// beads.ErrConditionalWriteUnsupported (which contractually writes nothing)
// falls back to the same sequence.
//
// Reports whether this call closed the bead (false when it was already
// closed). PHASE 0: the work-reassignment side effect that closeBead performs
// (releaseWorkFromClosedSessionBead) is intentionally NOT part of this method —
// that is a cross-class WORK op owned by the Phase 6 work/assignment API.
func (s *Store) Close(expected Info, stateCode string, now time.Time) (bool, error) {
	id := expected.ID
	bead, err := s.validatedBead(id)
	if err != nil {
		return false, err
	}
	if bead.Status == "closed" {
		return false, nil
	}
	premise := func(open beads.Bead) error {
		if !closePremiseHolds(expected, infoFromPersistedBead(open)) {
			return fmt.Errorf("closing session %q: %w", id, ErrSessionCloseSuperseded)
		}
		return nil
	}
	if err := premise(bead); err != nil {
		return false, err
	}
	patch := ClosePatch(now, stateCode)
	if closer, ok := beads.AtomicConditionalCloserFor(s.store); ok {
		closed, err := s.closeAtomically(closer, bead, patch, premise)
		if !beads.IsConditionalWriteUnsupported(err) {
			return closed, err
		}
	}
	if err := s.ApplyPatch(id, patch); err != nil {
		return false, err
	}
	if err := s.store.Close(id); err != nil {
		return false, fmt.Errorf("closing session %q: %w", id, err)
	}
	return true, nil
}

// ErrSessionCloseSuperseded reports that a terminal close found the row no
// longer carrying the facts the caller decided to close on (see
// closePremiseHolds), on its first read or on a re-read after a lost fence, so
// it wrote nothing. The row is still open, and the caller's next pass decides
// again from a fresh read.
var ErrSessionCloseSuperseded = errors.New("session changed since the close was decided")

// closePremiseHolds reports whether open, a row a terminal close read, still
// carries the facts of decided, the row the caller decided to close: the
// lifecycle facts (sameLifecycleFacts: state, sleep reason, holds, the
// pending-create lease, wake request, pin, ...) and the incarnation
// (generation and instance token). A `gc session kill` fence moves state and
// sleep_reason, so it fails this check too. The caller decided to close from
// those facts, so the close may land only while they hold; a writer that moved
// one of them, such as a wake, owns the next decision.
func closePremiseHolds(decided, open Info) bool {
	return sameLifecycleFacts(decided, open) &&
		decided.Generation == open.Generation &&
		decided.InstanceToken == open.InstanceToken
}

// closeAtomically runs Close's fenced single-write arm, starting from the
// observed open row. The observed revision is passed through as-is, including
// 0: whether a token is usable is the store's call (a fresh SQLite row fences
// on 0), and a store that rejects it answers with a precondition failure,
// which this loop re-reads and bounds. An ErrConditionalWriteUnsupported error
// is returned unwrapped so Close can fall back. A non-nil guard vets every open
// row the loop is about to close, the first read and each re-read alike; its
// error aborts the close before the write.
func (s *Store) closeAtomically(closer beads.AtomicConditionalCloser, bead beads.Bead, patch MetadataPatch, guard func(beads.Bead) error) (bool, error) {
	id := bead.ID
	var conflict error
	for attempt := 1; ; attempt++ {
		if guard != nil {
			if err := guard(bead); err != nil {
				return false, err
			}
		}
		_, err := closer.CloseWithMetadataIfMatch(id, bead.Revision, map[string]string(patch))
		switch {
		case err == nil:
			return true, nil
		case beads.IsConditionalWriteUnsupported(err):
			return false, err
		case !beads.IsPreconditionFailed(err):
			return false, fmt.Errorf("closing session %q: %w", id, err)
		}
		conflict = err
		if attempt == terminalCloseMaxAttempts {
			return false, fmt.Errorf("closing session %q: lost the revision fence %d times: %w", id, terminalCloseMaxAttempts, conflict)
		}
		bead, err = s.validatedBead(id)
		if err != nil {
			return false, err
		}
		if bead.Status == "closed" {
			return false, nil
		}
	}
}

// CloseWithTerminalPatch is the controller's terminal close (closeBead and
// closeFailedCreateBead in cmd/gc). patch is the complete terminal metadata the
// caller built, normally ClosePatch plus any path-specific clears (the
// failed-create close also clears pending_create_claim,
// pending_create_started_at and sleep_intent). commitMsg names the fallback
// transaction.
//
// When the backing store provides beads.AtomicConditionalCloser, patch and the
// closed status commit as ONE write fenced on the revision this call read,
// exactly as in Close: a lost fence re-reads and retries, bounded by
// terminalCloseMaxAttempts, and never degrades to a split write. It reports
// false, having written nothing, when the row is already closed, whether it
// was closed before the first read or by a concurrent closer that won the
// fence. expected is the row the caller decided to close, and every row this
// call reads, the first read included, must still carry its facts
// (closePremiseHolds). A wake, a wake request, or a new incarnation that landed
// after the caller's decision, before or after this call's first read, makes
// it return ErrSessionCloseSuperseded with nothing written. It also yields to
// a fresh `gc session kill` fence (IsKillPendingInfo at now) on any row it
// reads, returning ErrSessionKillPending with nothing written: the kill owns
// that row, and lifecycle passes must not close it. The caller decided to
// close on an older read, and without these checks the close would land over
// the wake or the kill fence. The caller's next pass decides again.
//
// Stores without the capability (BdStore, the exec store, plain MemStore, a
// legacy SQLite layout, or conditional writes disabled), and a store that
// refuses the capability at call time with beads.ErrConditionalWriteUnsupported,
// keep the controller's historical write: one store.Tx(commitMsg) of
// SetMetadataBatch(patch) then Close, after the same checks on the row this
// call read. An already-closed row reports false, a kill-fenced row returns
// ErrSessionKillPending, and a row that no longer carries expected's facts
// returns ErrSessionCloseSuperseded, each with nothing written. On BdStore
// that Tx is staged so `bd close` carries patch's close_reason; on the exec
// store and MemStore it runs as sequential writes. The metadata is ordered
// first, so if the Close then fails the clears have still landed and the
// caller retries the close. That arm keeps the residual race documented on
// Close: a writer that lands after the read, or between the Tx's two writes,
// is still closed over, because nothing fences the write on that read.
func (s *Store) CloseWithTerminalPatch(expected Info, patch MetadataPatch, commitMsg string, now time.Time) (bool, error) {
	id := expected.ID
	bead, err := s.validatedBead(id)
	if err != nil {
		return false, err
	}
	if bead.Status == "closed" {
		return false, nil
	}
	guard := func(open beads.Bead) error {
		row := infoFromPersistedBead(open)
		if IsKillPendingInfo(row, now) {
			return fmt.Errorf("closing session %q: %w", id, ErrSessionKillPending)
		}
		if !closePremiseHolds(expected, row) {
			return fmt.Errorf("closing session %q: %w", id, ErrSessionCloseSuperseded)
		}
		return nil
	}
	if closer, ok := beads.AtomicConditionalCloserFor(s.store); ok {
		closed, err := s.closeAtomically(closer, bead, patch, guard)
		if !beads.IsConditionalWriteUnsupported(err) {
			return closed, err
		}
	}
	if err := guard(bead); err != nil {
		return false, err
	}
	if err := s.store.Tx(commitMsg, func(tx beads.Tx) error {
		if err := tx.SetMetadataBatch(id, map[string]string(patch)); err != nil {
			return err
		}
		return tx.Close(id)
	}); err != nil {
		return false, err
	}
	return true, nil
}

// errPendingCreateRollbackSuperseded is the guard verdict that stops
// RollbackPendingCreateAtomically when the row it read is no longer the
// pending create the caller observed. It never leaves this file.
var errPendingCreateRollbackSuperseded = errors.New("pending create is no longer rollback-eligible")

// RollbackPendingCreateAtomically is the atomic arm of the controller's
// pending-create rollback (rollbackPendingCreateClears in cmd/gc). closePatch
// is the whole terminal patch, which the close commits with the closed status.
// postClosePatch holds the writes that must land only AFTER the close, the
// explicit session_name clear. They are applied only once the close succeeded.
//
// It holds the same per-session mutation lock as WithPendingCreateRollback,
// start completion and incarnation allocation. Under that lock it reads the
// row once. That read's revision fences the close, and the read must pass
// PendingCreateLease.CanRollback against expected, as in
// WithPendingCreateRollback. So closePatch and the closed status commit as ONE
// write, and only on the exact row the rollback approved. A writer from another
// process that lands after the read (a wake stamping state=awake, a start
// completion, a new incarnation, a `gc session kill` fence) wins the fence. The
// close writes nothing, re-reads, and closes only if the re-read row still
// passes CanRollback. A kill fence never does, because KillPendingPatch clears
// pending_create_claim. Re-reads are bounded by terminalCloseMaxAttempts. The
// close never degrades to a split write, so no reader can see the row closed
// with live-looking metadata.
//
// postClosePatch is then written as a separate step, still under the lock, and
// only while the row still reads closed. A row reopened in between keeps its
// values. Where the store resolves a conditional writer, that write is fenced
// on the revision it re-read.
//
// Results:
//   - closed is true when this call closed the row. It is false, with a nil
//     error and nothing written, when the row was already closed or is no
//     longer rollback-eligible.
//   - postClosed reports whether postClosePatch was written.
//   - err is non-nil when a write failed. With closed true it is the
//     post-close write that failed, and the close itself stands.
//   - A store without beads.AtomicConditionalCloser, or one that refuses it at
//     call time, returns an error matching beads.IsConditionalWriteUnsupported,
//     with closed false and nothing written. The caller then keeps its
//     transaction.
func (s *Store) RollbackPendingCreateAtomically(expected Info, closePatch, postClosePatch MetadataPatch) (closed, postClosed bool, err error) {
	closer, ok := beads.AtomicConditionalCloserFor(s.store)
	if !ok {
		return false, false, beads.ErrConditionalWriteUnsupported
	}
	lease := LeaseFromInfo(expected)
	err = WithSessionMutationLock(expected.ID, func() error {
		bead, err := s.validatedBead(expected.ID)
		if err != nil {
			return err
		}
		if bead.Status == "closed" {
			return nil
		}
		closed, err = s.closeAtomically(closer, bead, closePatch, func(open beads.Bead) error {
			if !lease.CanRollback(LeaseFromInfo(infoFromPersistedBead(open))) {
				return errPendingCreateRollbackSuperseded
			}
			return nil
		})
		if errors.Is(err, errPendingCreateRollbackSuperseded) {
			return nil
		}
		if err != nil || !closed || len(postClosePatch) == 0 {
			return err
		}
		postClosed, err = s.applyPatchIfClosed(expected.ID, postClosePatch)
		return err
	})
	return closed, postClosed, err
}

// applyPatchIfClosed writes patch to id only while the row reads closed. It
// reports whether it wrote. Where the store resolves a conditional writer, the
// write is fenced on the revision of the closed row it read, and a lost fence
// re-reads, bounded by terminalCloseMaxAttempts. Without a conditional writer
// it writes after the check, which leaves only the window between the read and
// the write.
func (s *Store) applyPatchIfClosed(id string, patch MetadataPatch) (bool, error) {
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return false, fmt.Errorf("updating closed session %q: %w", id, err)
	}
	for attempt := 1; ; attempt++ {
		bead, err := s.validatedBead(id)
		if err != nil {
			return false, err
		}
		if bead.Status != "closed" {
			return false, nil
		}
		if writer == nil {
			if err := s.ApplyPatch(id, patch); err != nil {
				return false, err
			}
			return true, nil
		}
		err = writer.UpdateIfMatch(id, bead.Revision, beads.UpdateOpts{Metadata: map[string]string(patch)})
		switch {
		case err == nil:
			return true, nil
		case !beads.IsPreconditionFailed(err):
			return false, fmt.Errorf("updating closed session %q: %w", id, err)
		case attempt == terminalCloseMaxAttempts:
			return false, fmt.Errorf("updating closed session %q: lost the revision fence %d times: %w", id, terminalCloseMaxAttempts, err)
		}
	}
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

// RepairTypeBestEffort re-issues the empty-type heal (RepairType) and logs a
// failure instead of returning it, for the read paths that heal a type-lost
// session bead as a side effect (the API/worker Get compositions and the raw
// assignee-normalize lane). It preserves the best-effort logging the retired
// RepairEmptyType emitted — the heal must never abort the current operation, but
// a silent drop would hide a failing write. The log line matches RepairEmptyType.
func (s *Store) RepairTypeBestEffort(id string) {
	if err := s.RepairType(id); err != nil {
		log.Printf("session %s: repairing empty bead type: %v", id, err)
	}
}

// Store returns the embedded strongly-typed session-class bead store. It is a
// transition-period accessor for call sites that still need raw bead access
// while their reads/writes are migrated behind the typed methods above. New
// code must prefer the typed methods; this exists so Phase 4/5 can land
// incrementally without a flag-day rewrite.
func (s *Store) Store() beads.SessionStore { return s.store }

// SetLocalString stores a clone-local session value without exposing the
// underlying Beads store through the sessions front door.
func (s *Store) SetLocalString(id, key, value string) error {
	return s.store.SetLocalString(id, key, value)
}

// GetLocalString returns a clone-local session value without exposing the
// underlying Beads store through the sessions front door.
func (s *Store) GetLocalString(id, key string) (string, error) {
	return s.store.GetLocalString(id, key)
}
