package session

import (
	"context"
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
//
// ctx bounds only the wait for the session mutation lock (see
// WithSessionMutationLockContext), never the work done under it. When the
// lock is still held at ctx's end the call returns an error matching
// ErrSessionMutationBusy, with no session row read or written.
func (s *Store) CommitStartedIfCurrent(ctx context.Context, expected Info, patch MetadataPatch) (bool, error) {
	return s.CommitStartedIfCurrentUnder(ctx, expected, patch, nil)
}

// ErrStartCommitHeld refuses a start commit (StartCommitRefusingHolds) whose
// locked re-read shows an operator's suspend (Disposition.KeepsSlot) that
// landed while the start ran: the start does not commit, and the runtime it
// spawned is the caller's to stop (CONTRACT S1 v5.6, §12.2 row 65).
var ErrStartCommitHeld = errors.New("an operator suspended the session during its start; the start does not commit")

// StartCommitOpt adjusts CommitStartedIfCurrentUnder.
type StartCommitOpt func(*startCommitOpts)

type startCommitOpts struct{ refuseHeld bool }

// StartCommitRefusingHolds refuses the commit with ErrStartCommitHeld when
// the row it re-reads keeps an operator's suspend, as the start's own
// prepare refuses one before the PreWake, and as a kill fence refuses a
// close (CommitClose).
func StartCommitRefusingHolds() StartCommitOpt {
	return func(o *startCommitOpts) { o.refuseHeld = true }
}

// CommitStartedIfCurrentUnder is CommitStartedIfCurrent for a start run under
// lease: a re-read row that no longer records the lease is not current, so a
// start whose lease was taken over commits nothing. A nil lease checks none.
// ctx bounds only the lock wait, as for CommitStartedIfCurrent.
func (s *Store) CommitStartedIfCurrentUnder(ctx context.Context, expected Info, patch MetadataPatch, lease *RuntimeLease, opts ...StartCommitOpt) (bool, error) {
	var o startCommitOpts
	for _, opt := range opts {
		opt(&o)
	}
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return false, fmt.Errorf("committing start of session %q: %w", expected.ID, err)
	}
	applied := false
	err = WithSessionMutationLockContext(ctx, expected.ID, func() error {
		var conflict error
		for attempt := 0; attempt < startCommitMaxAttempts; attempt++ {
			bead, err := s.validatedBead(expected.ID)
			if err != nil {
				return err
			}
			if LeaseFromInfo(expected).CommitVerdict(LeaseFromInfo(infoFromPersistedBead(bead))) != LeaseCommit ||
				(lease != nil && !lease.HoldsMeta(bead.Metadata)) {
				return nil
			}
			if o.refuseHeld && DispositionOfInfo(infoFromPersistedBead(bead)).KeepsSlot() {
				return fmt.Errorf("committing start of session %q: %w", expected.ID, ErrStartCommitHeld)
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
//
// ctx bounds only the wait for the session mutation lock (see
// WithSessionMutationLockContext), never the work done under it. When the
// lock is still held at ctx's end the call returns an error matching
// ErrSessionMutationBusy, with no session row read or written.
func (s *Store) WithPendingCreateRollback(ctx context.Context, expected Info, fn func() error) (bool, error) {
	applied := false
	err := WithSessionMutationLockContext(ctx, expected.ID, func() error {
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
// is closed or its lifecycle facts differ from expected's. The re-read and the
// write both run inside the row's session mutation lock, taken without
// waiting: every in-process lifecycle verb (suspend, wake, close) writes under
// that lock, so none can land between the check and the write. While another
// holder has the lock the call writes nothing and returns (false, nil); the
// advisory caller recomputes the patch on its next tick from a snapshot that
// includes the holder's write. Where the store resolves a conditional writer
// (beads.ResolveConditionalWriter), the write is also fenced on the re-read
// row's revision, so a writer in another process that lands between the re-read
// and the write wins: the fenced write is refused with (false, nil). The
// revision is passed through as-is: bd revisions are signed, and whether a
// token is usable is the store's call. Without a conditional writer an
// other-process writer is not fenced; only the in-process half is closed.
//
// A refused patch writes nothing, and the caller must not fold it. A
// require-mode store that cannot fence, and a store that reports
// beads.ErrConditionalWriteUnsupported at call time, return an error rather
// than falling back to an unconditional write.
func (s *Store) ApplyPatchIfLifecycleUnchanged(expected Info, patch MetadataPatch) (bool, error) {
	applied := false
	_, err := tryWithSessionMutationLock(expected.ID, func() error {
		var err error
		applied, err = s.applyPatchIfLifecycleUnchanged(expected, patch, nil, 1)
		return err
	})
	return applied, err
}

// ApplyPatchIfLifecycleUnchangedUnder is ApplyPatchIfLifecycleUnchanged for a
// writer holding lease: the re-read row must also still record the lease, or
// the patch is refused with (false, nil). The write is fenced at that read's
// revision, so a takeover that lands after the read refuses it too; a fence
// lost to another writer re-reads and checks again, up to three times, as the
// start commit does. A nil lease checks none.
func (s *Store) ApplyPatchIfLifecycleUnchangedUnder(expected Info, patch MetadataPatch, lease *RuntimeLease) (bool, error) {
	return s.applyPatchIfLifecycleUnchanged(expected, patch, lease, startCommitMaxAttempts)
}

func (s *Store) applyPatchIfLifecycleUnchanged(expected Info, patch MetadataPatch, lease *RuntimeLease, attempts int) (bool, error) {
	res, err := s.commitIf(expected.ID, patch, lease, attempts, s.validatedBead, func(b beads.Bead) bool {
		return sameLifecycleFacts(expected, infoFromPersistedBead(b))
	})
	return res == CommitLanded, err
}

// commitIf writes patch to row id while holds accepts read's read of it
// (and, with lease, the read still records the lease), fenced at that read's
// revision; a fence lost to another writer re-reads, up to attempts times.
// Without conditional writes the write is unfenced: its window runs from the
// read to the write, plus the cache's lag when read goes through the cache
// (ApplyPatchIfLifecycleUnchanged; Commit reads live). The in-process half of
// that window is closed where the caller holds the row's mutation lock across
// the call, as ApplyPatchIfLifecycleUnchanged does.
func (s *Store) commitIf(id string, patch MetadataPatch, lease *RuntimeLease, attempts int, read func(string) (beads.Bead, error), holds func(beads.Bead) bool) (CommitResult, error) {
	if len(patch) == 0 {
		return 0, nil // nothing to write
	}
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return 0, fmt.Errorf("updating session %q: %w", id, err)
	}
	for attempt := 0; attempt < attempts; attempt++ {
		bead, err := read(id)
		if err != nil {
			return 0, err
		}
		if bead.Status == "closed" {
			return CommitClosed, nil
		}
		if !holds(bead) || (lease != nil && !lease.HoldsMeta(bead.Metadata)) {
			return CommitMoved, nil
		}
		if writer == nil {
			if err := s.ApplyPatch(id, patch); err != nil {
				return 0, err
			}
			return CommitLanded, nil
		}
		err = writer.UpdateIfMatch(id, bead.Revision, beads.UpdateOpts{Metadata: map[string]string(patch)})
		switch {
		case err == nil:
			return CommitLanded, nil
		case !beads.IsPreconditionFailed(err):
			return 0, fmt.Errorf("updating session %q: %w", id, err)
		}
	}
	return CommitContended, nil
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
				UnmintDisposition(id, patch)
				return false, err
			}
			return true, nil
		}
		err = writer.UpdateIfMatch(id, bead.Revision, opts)
		if err != nil {
			UnmintDisposition(id, patch) // decide mints again on the next attempt
		}
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

// RowPatch is one fenced row write: a metadata patch, a title and a type
// (nil leaves them) and label deltas. Labels travel as deltas, never as a whole set.
type RowPatch struct {
	Metadata     MetadataPatch
	Title        *string
	Type         *string // an empty type's repair (RepairType's, in the same CAS)
	AddLabels    []string
	RemoveLabels []string
}

func (p RowPatch) empty() bool {
	return len(p.Metadata) == 0 && p.Title == nil && p.Type == nil && len(p.AddLabels) == 0 && len(p.RemoveLabels) == 0
}

// UpdateRowFenced writes metadata, title and labels decided from a fresh read
// of the row in one UpdateIfMatch at that read's revision. A lost CAS re-reads
// and runs decide again, up to attempts times, after which nothing is written.
// decide returning false or an empty patch writes nothing. Unlike
// UpdateMetadataFenced it never writes blind: without a conditional writer it
// returns beads.ErrConditionalWriteUnsupported, and a store that cannot guard
// labels returns its *beads.ConditionalUpdateFieldUnsupportedError; either
// way nothing is written. A success means the deltas applied, not that the
// label set equals the read set plus the deltas (a legacy label-only write on
// native Dolt does not move the revision). It reports whether it wrote.
func (s *Store) UpdateRowFenced(id string, attempts int, decide func(Info, PersistedResponse) (RowPatch, bool)) (bool, error) {
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return false, fmt.Errorf("updating session %q: %w", id, err)
	}
	if writer == nil {
		return false, fmt.Errorf("updating session %q: %w", id, beads.ErrConditionalWriteUnsupported)
	}
	for attempt := 0; attempt < attempts; attempt++ {
		bead, err := s.validatedBead(id)
		if err != nil {
			return false, err
		}
		patch, ok := decide(infoFromPersistedBead(bead), PersistedResponseFromBead(bead))
		if !ok || patch.empty() {
			return false, nil
		}
		err = writer.UpdateIfMatch(id, bead.Revision, beads.UpdateOpts{
			Metadata:     map[string]string(patch.Metadata),
			Title:        patch.Title,
			Type:         patch.Type,
			Labels:       patch.AddLabels,
			RemoveLabels: patch.RemoveLabels,
		})
		if err != nil {
			UnmintDisposition(id, patch.Metadata) // the write did not land
		}
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

// mintPremiseHolds is MintIncarnation's premise on one fresh read of the row
// (meta its metadata): fresh still carries the lifecycle facts, the
// generation and the token expect was read with, and still records lease (a
// nil lease checks none). LifecycleInput holds no generation or token, so the
// lifecycle facts alone would let a retry after a lost fence mint a stale g+1.
func mintPremiseHolds(expect, fresh Info, lease *RuntimeLease, meta map[string]string) bool {
	return sameLifecycleFacts(expect, fresh) &&
		strings.TrimSpace(fresh.Generation) == strings.TrimSpace(expect.Generation) &&
		strings.TrimSpace(fresh.InstanceToken) == strings.TrimSpace(expect.InstanceToken) &&
		(lease == nil || lease.HoldsMeta(meta))
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

// Sleep records a non-terminal sleep/drain result: SleepPatch's lifecycle
// with the disposition's Sleep cell (ApplySleep), which keeps an operator's
// user hold. The city stop writes it.
func (s *Store) Sleep(id, reason string, now time.Time) error {
	_, err := s.ApplySleep(id, SleepPatch(now, reason))
	return err
}

// operatorSuspendAttempts bounds an operator's or agent's own disposition
// write (ApplyDispositionFresh), and the failed-stop rollback: an operator's
// write competes with every controller write to the row.
const operatorSuspendAttempts = 8

// OperatorSuspend writes an operator's suspend: the disposition table's
// Suspend cell, decided on each fresh read of the row, with its lifecycle
// (operatorSuspendLife). Every suspend writes, a suspend already in force
// included: it is stamped again, so an operator's resume that read the old
// stamp refuses its consume. A row closed since the caller read it is refused
// with an illegal-transition error and written nothing; a CAS lost on every
// attempt writes nothing and returns an error to retry. It returns what it
// wrote, for a failed stop's rollback.
func (s *Store) OperatorSuspend(id string, now time.Time) (DispositionWrite, error) {
	return s.operatorSuspend(id, now, operatorSuspendLife)
}

// operatorSuspend is OperatorSuspend with life as the suspend's lifecycle.
func (s *Store) operatorSuspend(id string, now time.Time, life DispositionLife) (DispositionWrite, error) {
	w, err := s.ApplyDispositionFresh(id, ActorOperator, DispEvent{Kind: EvSuspend}, life, now)
	switch {
	case err != nil:
		return DispositionWrite{}, err
	case w.Result == CommitClosed:
		return DispositionWrite{}, &IllegalTransitionError{From: StateClosed, Command: CmdSuspend}
	case w.Result != CommitLanded:
		return DispositionWrite{}, fmt.Errorf("suspending session %q: lost to concurrent writes; retry", id)
	}
	return w, nil
}

// operatorSuspendLife is the lifecycle of an operator's suspend:
// state=suspended, and the pending wake cleared as newer intent (D7.3).
func operatorSuspendLife(Disposition, Disposition, Info) MetadataPatch {
	patch := ClearWakeRequestPatch()
	patch["state"] = string(StateSuspended)
	patch["slept_at"] = ""
	patch["sleep_reason"] = ""
	return patch
}

// operatorSuspendStopLife is the lifecycle of an operator's suspend that
// stops the runtime itself (Manager.Suspend): operatorSuspendLife, with the
// wake stamp cleared in the same write, so the runtime's death is no rapid
// crash or churn of that wake, which would discard the conversation the next
// wake resumes. A managed suspend leaves the stamp to its drain.
func operatorSuspendStopLife(was, next Disposition, info Info) MetadataPatch {
	patch := operatorSuspendLife(was, next, info)
	patch["last_woke_at"] = ""
	return patch
}

// SetWaitHold begins (on) or ends an agent's wait on the row: the
// disposition's WaitBegin or WaitEnd cell, decided on a fresh read. A wait
// never replaces an operator's user-hold intent. reason is the wait's, which
// the wait hold no longer records. A closed row takes no write and no error,
// as main's blind write succeeded there (`gc session wait --sleep` exits 0).
func (s *Store) SetWaitHold(id string, on bool, _ string) error {
	ev := DispEvent{Kind: EvWaitEnd}
	if on {
		ev.Kind = EvWaitBegin
	}
	w, err := s.ApplyDispositionFresh(id, ActorAgent, ev, nil, time.Now())
	switch {
	case err != nil:
		return err
	case w.Result == CommitClosed:
		return nil
	case w.Result != CommitLanded:
		return fmt.Errorf("session %s: wait hold lost to concurrent writes; retry", id)
	}
	return nil
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

// SetPinAwake writes an operator's pin (pin_awake "true", or cleared): the
// one writer gc session pin|unpin and POST /session/{id}/pin|unpin share.
func (s *Store) SetPinAwake(id string, pinned bool) error {
	value := ""
	if pinned {
		value = "true"
	}
	return s.setMetadataValue(id, "pin_awake", value)
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
// onto wait beads as registered_epoch), the sleep_reason (consulted when
// clearing a wait-hold), and the continuation_reset_pending marker (stamped
// onto wait beads as registered_reset_pending). It carries the raw bead fields
// verbatim.
type PersistedMarkers struct {
	Title                    string
	SessionName              string
	ContinuationEpoch        string
	SleepReason              string
	ContinuationResetPending string
}

// PersistedMarkers returns the persisted Title / session_name /
// continuation_epoch / sleep_reason / continuation_reset_pending markers for
// id, verbatim (each "" when unset).
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
		Title:                    b.Title,
		SessionName:              b.Metadata["session_name"],
		ContinuationEpoch:        b.Metadata["continuation_epoch"],
		SleepReason:              b.Metadata["sleep_reason"],
		ContinuationResetPending: b.Metadata["continuation_reset_pending"],
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

// Close is CommitClose's bool form for a close decided on expected
// (FactsLegacyClose) with ClosePatch(now, stateCode): it reports whether this
// call closed the row (false when it was already closed), and a refusal as
// its error: ErrSessionCloseSuperseded, ErrSessionKillPending or
// ErrRuntimeLeaseLost. stateCode is the canonical short
// state code ClosePatch expands to a validator-safe close_reason.
//
// PHASE 0: the work-reassignment side effect that closeBead performs is
// intentionally NOT part of this method: that is a cross-class WORK op owned
// by the Phase 6 work/assignment API.
func (s *Store) Close(expected Info, stateCode string, now time.Time) (bool, error) {
	res, err := s.CommitClose(Decide(expected, FactsLegacyClose), ClosePatch(now, stateCode), now)
	switch {
	case err != nil:
		return false, err
	case res == CommitMoved:
		return false, fmt.Errorf("closing session %q: %w", expected.ID, ErrSessionCloseSuperseded)
	case res == CommitKillFenced:
		return false, fmt.Errorf("closing session %q: %w", expected.ID, ErrSessionKillPending)
	case res == CommitLeaseLost:
		return false, fmt.Errorf("closing session %q: %w", expected.ID, ErrRuntimeLeaseLost)
	case res == CommitContended:
		return false, fmt.Errorf("closing session %q: lost the revision fence %d times", expected.ID, terminalCloseMaxAttempts)
	}
	return res == CommitLanded, nil
}

// ErrSessionCloseSuperseded reports that a terminal close found the row no
// longer carrying the facts the caller decided to close on (its Decided's
// site), on its first read or on a re-read after a lost fence, so
// it wrote nothing. The row is still open, and the caller's next pass decides
// again from a fresh read.
var ErrSessionCloseSuperseded = errors.New("session changed since the close was decided")

// CloseWithMetadataIfMatch reads id's row, asks decide for its terminal
// patch, and closes the row with it in one atomic conditional close at the
// read's revision: one attempt, no retry and no re-decide, so the caller
// (the v2 effect transaction) owns every retry. A lost fence returns
// (false, nil). decide returning false, or a row already closed, writes
// nothing. Without an atomic conditional closer it returns
// beads.ErrConditionalWriteUnsupported and writes nothing: there is no
// two-write fallback. The close clears the runtime lease record, which a
// closed row holds no more.
func (s *Store) CloseWithMetadataIfMatch(id string, decide func(Info, PersistedResponse) (MetadataPatch, bool)) (bool, error) {
	closer, ok := beads.AtomicConditionalCloserFor(s.store)
	if !ok {
		return false, fmt.Errorf("closing session %q: %w", id, beads.ErrConditionalWriteUnsupported)
	}
	bead, err := s.validatedBead(id)
	if err != nil || bead.Status == "closed" {
		return false, err
	}
	patch, ok := decide(infoFromPersistedBead(bead), PersistedResponseFromBead(bead))
	if !ok {
		return false, nil
	}
	_, err = closer.CloseWithMetadataIfMatch(id, bead.Revision, map[string]string(withRuntimeLeaseCleared(patch)))
	if err != nil {
		UnmintDisposition(id, patch) // the close did not land
	}
	switch {
	case err == nil:
		return true, nil
	case beads.IsPreconditionFailed(err):
		return false, nil
	}
	return false, fmt.Errorf("closing session %q: %w", id, err)
}

// closeAtomically runs Close's fenced single-write arm, starting from the
// observed open row. The observed revision is passed through as-is, including
// 0: whether a token is usable is the store's call (a fresh SQLite row fences
// on 0), and a store that rejects it answers with a precondition failure,
// which this loop re-reads and bounds. An ErrConditionalWriteUnsupported error
// is returned unwrapped so Close can fall back. A non-nil guard vets every open
// row the loop is about to close, the first read and each re-read alike; its
// error aborts the close before the write.
func (s *Store) closeAtomically(closer beads.AtomicConditionalCloser, bead beads.Bead, patch MetadataPatch, attempts int, guard func(beads.Bead) error) (bool, error) {
	id := bead.ID
	var conflict error
	for attempt := 1; ; attempt++ {
		if guard != nil {
			if err := guard(bead); err != nil {
				return false, err
			}
		}
		_, err := closer.CloseWithMetadataIfMatch(id, bead.Revision, map[string]string(withRuntimeLeaseCleared(patch)))
		switch {
		case err == nil:
			return true, nil
		case beads.IsConditionalWriteUnsupported(err):
			return false, err
		case !beads.IsPreconditionFailed(err):
			return false, fmt.Errorf("closing session %q: %w", id, err)
		}
		conflict = err
		if attempt == attempts {
			return false, fmt.Errorf("closing session %q: %w %d times: %w", id, errCloseContended, attempts, conflict)
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

// CommitOpt qualifies a CommitClose.
type CommitOpt func(*commitOpts)

type commitOpts struct {
	lease         *RuntimeLease
	pastKillFence bool
	attempts      int
}

// CommitAsOperator bounds a close's lost fences at operatorSuspendAttempts
// instead of terminalCloseMaxAttempts: an operator's close stops the runtime
// before it commits, so it competes with every controller write the tick
// after the stop makes to the row, as an operator's suspend does.
func CommitAsOperator() CommitOpt {
	return func(o *commitOpts) { o.attempts = operatorSuspendAttempts }
}

// CommitUnder makes a close land only while every row it reads still records
// lease, the closer's runtime lease. A nil lease checks none.
func CommitUnder(lease *RuntimeLease) CommitOpt {
	return func(o *commitOpts) { o.lease = lease }
}

// CommitPastKillFence makes a close land on a row a fresh `gc session kill`
// fence holds: an operator's close, which ends the session the kill was
// tearing down, as it did before CommitClose.
func CommitPastKillFence() CommitOpt {
	return func(o *commitOpts) { o.pastKillFence = true }
}

// CommitClose closes d's row with terminal, the complete terminal metadata
// the caller built (ClosePatch plus any path's clears), while the row still
// carries the facts d was decided on (Match, at d's site). Every row it
// reads, the first included, must match, or the close refuses as CommitMoved;
// a row a fresh `gc session kill` fence holds at now (IsKillPendingInfo) is
// the kill's (CommitKillFenced), unless CommitPastKillFence; and one that no
// longer records the CommitUnder lease is its taker's (CommitLeaseLost). A
// refusal writes nothing, and the caller decides again on a later read. A
// row already closed is CommitClosed.
//
// Where the store provides beads.AtomicConditionalCloser (native Dolt,
// SQLite with a revision column, FileStore, a cache over one), terminal and
// the closed status commit as ONE write fenced on the revision read, which
// also clears the runtime lease record: a lost fence re-reads and checks
// again, up to terminalCloseMaxAttempts (operatorSuspendAttempts under
// CommitAsOperator), then CommitContended with the row still open. It never
// degrades to a split write.
//
// Other stores (BdStore, the exec store, plain MemStore, a legacy SQLite
// layout, conditional writes disabled), and a store that refuses the closer
// at call time with beads.ErrConditionalWriteUnsupported, keep one
// store.Tx("gc: close session <id>") of SetMetadataBatch(terminal) then
// Close, after the same checks on the row read. On BdStore that Tx is staged
// so `bd close` carries terminal's close_reason. The metadata is ordered
// first, so a failed Close leaves the clears landed and the caller retries.
// That arm keeps a residual race: a writer that lands after the read, or
// between the Tx's two writes, is closed over. A Tx answering
// beads.ErrWriteUnconfirmed (its writes committed, a read-back timed out) is
// settled by a re-read: a row that reads closed is CommitLanded; otherwise the
// error stands, still wrapping beads.ErrWriteUnconfirmed, so a caller can tell
// "retry" from "failed".
func (s *Store) CommitClose(d Decided, terminal MetadataPatch, now time.Time, opts ...CommitOpt) (res CommitResult, err error) {
	defer func() {
		if res != CommitLanded {
			UnmintDisposition(d.info.ID, terminal) // the close did not land
		}
	}()
	o := commitOpts{attempts: terminalCloseMaxAttempts}
	for _, opt := range opts {
		opt(&o)
	}
	id := d.info.ID
	if id == "" {
		return 0, errors.New("session: close of a zero decision")
	}
	bead, err := s.validatedBead(id)
	switch {
	case err != nil:
		return 0, err
	case bead.Status == "closed":
		return CommitClosed, nil
	}
	guard := func(open beads.Bead) error {
		row := infoFromPersistedBead(open)
		switch {
		case !o.pastKillFence && IsKillPendingInfo(row, now):
			return errCloseKillFenced
		case !d.Match(row):
			return errCloseMoved
		case o.lease != nil && !o.lease.HoldsMeta(open.Metadata):
			return errCloseLeaseLost
		}
		return nil
	}
	if closer, ok := beads.AtomicConditionalCloserFor(s.store); ok {
		closed, err := s.closeAtomically(closer, bead, terminal, o.attempts, guard)
		switch {
		case closeRefusals[err] != 0:
			return closeRefusals[err], nil
		case errors.Is(err, errCloseContended):
			return CommitContended, nil
		case err == nil && closed:
			return CommitLanded, nil
		case err == nil:
			return CommitClosed, nil
		case !beads.IsConditionalWriteUnsupported(err):
			return 0, err
		}
	}
	if err := guard(bead); err != nil {
		return closeRefusals[err], nil
	}
	if err := s.store.Tx("gc: close session "+id, func(tx beads.Tx) error {
		if err := tx.SetMetadataBatch(id, map[string]string(terminal)); err != nil {
			return err
		}
		return tx.Close(id)
	}); err != nil {
		if errors.Is(err, beads.ErrWriteUnconfirmed) && s.readsClosed(id) {
			// The Tx's writes committed and only a read-back timed out: a
			// row that now reads closed is this close landing, not a failure.
			log.Printf("session %s: close landed; %v", id, err)
			return CommitLanded, nil
		}
		return 0, err
	}
	return CommitLanded, nil
}

// readsClosed reports whether id reads closed now. A failed read is not
// evidence of a close.
func (s *Store) readsClosed(id string) bool {
	b, err := s.validatedBead(id)
	return err == nil && b.Status == "closed"
}

// errCloseContended is closeAtomically's exhausted fence.
var errCloseContended = errors.New("lost the revision fence")

// CommitClose's guard verdicts, and the refusal each is. They never leave
// this file.
var (
	errCloseMoved      = errors.New("the row moved since the close was decided")
	errCloseKillFenced = errors.New("a kill fence holds the row")
	errCloseLeaseLost  = errors.New("the row no longer records the lease")
	closeRefusals      = map[error]CommitResult{errCloseMoved: CommitMoved, errCloseKillFenced: CommitKillFenced, errCloseLeaseLost: CommitLeaseLost}
)

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
//
// ctx bounds only the wait for the session mutation lock (see
// WithSessionMutationLockContext), never the work done under it. When the
// lock is still held at ctx's end the call returns an error matching
// ErrSessionMutationBusy, with no session row read or written.
func (s *Store) RollbackPendingCreateAtomically(ctx context.Context, expected Info, closePatch, postClosePatch MetadataPatch) (closed, postClosed bool, err error) {
	return s.RollbackPendingCreateAtomicallyUnder(ctx, expected, closePatch, postClosePatch, nil)
}

// RollbackPendingCreateAtomicallyUnder is RollbackPendingCreateAtomically for
// a start holding held: a row that no longer records the lease is not rolled
// back, as a superseded one is not. A nil lease checks none.
// ctx bounds only the lock wait, as for RollbackPendingCreateAtomically.
func (s *Store) RollbackPendingCreateAtomicallyUnder(ctx context.Context, expected Info, closePatch, postClosePatch MetadataPatch, held *RuntimeLease) (closed, postClosed bool, err error) {
	closer, ok := beads.AtomicConditionalCloserFor(s.store)
	if !ok {
		return false, false, beads.ErrConditionalWriteUnsupported
	}
	lease := LeaseFromInfo(expected)
	err = WithSessionMutationLockContext(ctx, expected.ID, func() error {
		bead, err := s.validatedBead(expected.ID)
		if err != nil {
			return err
		}
		if bead.Status == "closed" {
			return nil
		}
		closed, err = s.closeAtomically(closer, bead, closePatch, terminalCloseMaxAttempts, func(open beads.Bead) error {
			if !lease.CanRollback(LeaseFromInfo(infoFromPersistedBead(open))) || (held != nil && !held.HoldsMeta(open.Metadata)) {
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
// write, plus the runtime lease clear (RuntimeLeaseClearPatch): a reopened
// row holds no lease.
func (s *Store) SetStatusOpen(id string) error {
	open := "open"
	if err := s.store.Update(id, beads.UpdateOpts{Status: &open, Metadata: map[string]string(RuntimeLeaseClearPatch())}); err != nil {
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
