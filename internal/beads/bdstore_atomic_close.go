package beads

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// This file gives BdStore the optional AtomicConditionalCloser capability: the
// all-or-nothing terminal write that commits a bead's close together with its
// terminal metadata. Without it, session.Store.Close falls back to the historical
// ClosePatch-then-Close pair, and a writer landing between the two produces a
// closed row still advertising a nonterminal lifecycle state (ga-f7v2ft.78.6).
//
// FENCE (read this before changing the argv): the guard is bd's status
// compare-and-swap, `bd update --if-status`, NOT a revision fence. The pinned
// schema-v59 bd (v1.1.1-0.20260805093327-bf97b73749ac) ships no --if-revision on
// any verb — beads#4682 is unlanded — so the revision-fenced shape cannot run on
// the exact configuration both incident cohorts used. --if-status is evaluated
// INSIDE the mutation transaction (issueops.CheckExpectedFieldsInTx) and shares
// the row_lock cell every mutating bd path rewrites, so it has both CAS limbs: a
// writer that committed before the transaction began is refused by the read-side
// check, and a writer that commits during it collides on row_lock and forces
// bd's whole-attempt retry, which re-reads and re-checks. That is the same
// mechanism a future --if-revision would use.
//
// EQUIVALENCE TO `bd close` (validated against the pinned bd; see
// bdstore_atomic_close_integration_test.go). `bd update --status closed` and
// `bd close --force` run different SQL, so this is not an assumption:
//
//	same: status=closed, closed_at stamped server-side, updated_at, row_lock
//	      rewritten, the claim lease deleted, a CLOSED event recorded, and
//	      is_blocked recomputed for every dependent (so the ready set moves
//	      identically).
//	differs: the top-level close_reason COLUMN is written only by `bd close`.
//	      Gas City reads the close reason from metadata.close_reason — which
//	      ClosePatch writes and this fused command persists — and beads.Bead has
//	      no CloseReason field at all, so no Gas City consumer observes the
//	      column. It remains visible to `bd show`, `bd export`, and any operator
//	      reading the row directly.
//	differs: bd fires its on_update hook rather than on_close. gc uninstalls the
//	      bd event-forwarding hooks (cmd/gc/hooks.go) and a scope carrying
//	      executable bd hooks is disqualified from the native store, so no
//	      supported Gas City deployment depends on which one fires.
//	differs: `bd close` runs the CLI-layer close-reason validator
//	      (validation.on-close) and gate-satisfaction checks; the fused update
//	      does not. Gas City already bypasses both by passing --force, and
//	      ClosePatch always supplies a canonical 20+ character reason.

// BdStore exposes the capability through the handle provider rather than a bare
// method set: discovery must report false when the live bd cannot perform the
// guarded single-command write, so an incapable bd keeps the historical
// fallback instead of hard-failing every close.
var _ AtomicConditionalCloserHandleProvider = (*BdStore)(nil)

// bdStatusGuardFlag is bd's semantic-field compare-and-swap on the status
// column. A mismatch writes nothing and exits 13.
const bdStatusGuardFlag = "--if-status"

// bdClosedStatus is the terminal status the fused write sets.
const bdClosedStatus = "closed"

// statusGuardCapabilityProbe reports whether the live bd parses --if-status on
// `bd update`. The verdict is memoized per store instance and the probe fires
// lazily — on the first capability discovery or close, never at construction —
// so read-only CLI paths pay no subprocess tax. A probe error degrades to
// incapable: the fallback close sequence is worse than the fused one but it
// works, whereas issuing a guard bd cannot parse would fail every close.
func (s *BdStore) statusGuardCapability() (bool, error) {
	s.statusGuardMu.Lock()
	defer s.statusGuardMu.Unlock()
	if s.statusGuardProbed {
		return s.statusGuardCapable, s.statusGuardProbeErr
	}
	s.statusGuardProbed = true
	out, err := s.runner(s.dir, "bd", "update", "--help")
	if err != nil {
		s.statusGuardProbeErr = err
		return false, err
	}
	if !bytes.Contains(out, []byte(bdStatusGuardFlag)) {
		return false, nil
	}
	s.statusGuardCapable = true
	return true, nil
}

// AtomicConditionalCloserHandle exposes the atomic terminal-close capability
// only when the live bd can honor the status guard. Callers of
// AtomicConditionalCloserFor must be able to trust a true answer: this is not a
// rollout seam with a legacy arm, it is an all-or-nothing capability.
func (s *BdStore) AtomicConditionalCloserHandle() (AtomicConditionalCloser, bool) {
	capable, _ := s.statusGuardCapability()
	if !capable {
		return nil, false
	}
	return s, true
}

// CloseWithMetadataIfMatch merges metadata and closes id in ONE guarded bd
// write, so no reader can observe — and no crash can strand — a closed row
// carrying nonterminal metadata.
//
// expectedRevision is honored as a read-side staleness check, not as the
// durable fence. It is compared against the token this store reads back from
// `bd show`, and skipped when either side is 0: only `bd show` projects the
// row_lock token as `revision`, so a bead observed through a CachingStore
// primed from `bd list` legitimately carries 0, and refusing those would wedge
// every session close on a bd-backed city. The durable fence in every case is
// the --if-status CAS above.
//
// A row already closed when this store reads it returns *PreconditionFailedError
// rather than re-closing: another actor reached the terminal state first, and
// reporting that lets a caching layer evict and the caller re-read, exactly as a
// lost revision fence would. Re-closing instead would rewrite closed_at and
// claim a close this call did not perform.
func (s *BdStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	capable, probeErr := s.statusGuardCapability()
	if !capable {
		if probeErr != nil {
			return Bead{}, fmt.Errorf("atomic terminal close %q: %w: capability probe failed: %w",
				id, ErrConditionalWriteUnsupported, probeErr)
		}
		return Bead{}, fmt.Errorf("atomic terminal close %q: %w: bd lacks %s",
			id, ErrConditionalWriteUnsupported, bdStatusGuardFlag)
	}

	current, err := s.Get(id)
	if err != nil {
		return Bead{}, fmt.Errorf("atomic terminal close %q: %w", id, err)
	}
	if current.Status == bdClosedStatus {
		return Bead{}, &PreconditionFailedError{
			ID:       id,
			Expected: expectedRevision,
			Current:  current.Revision,
			Raw:      "bead was already closed when the fenced terminal close read it",
		}
	}
	if expectedRevision != 0 && current.Revision != 0 && current.Revision != expectedRevision {
		return Bead{}, &PreconditionFailedError{
			ID:       id,
			Expected: expectedRevision,
			Current:  current.Revision,
			Raw:      "bd row revision moved before the fenced terminal close",
		}
	}

	if strings.TrimSpace(current.Status) == "" {
		// bd validates --if-status against the configured status set, so an
		// empty guard would be rejected as invalid input rather than evaluated.
		// Refuse here instead, naming the real problem.
		return Bead{}, fmt.Errorf("atomic terminal close %q: bd reported no status to fence on", id)
	}

	status := bdClosedStatus
	args := bdUpdateArgs(id, UpdateOpts{Status: &status, Metadata: metadata})
	args = append(args, bdStatusGuardFlag, current.Status)
	// The retry wrapper is fenced on the token this store just observed, not on
	// the caller's: its serialization-class replay re-reads the revision to
	// decide whether the fence went permanently stale, and a caller-side 0 would
	// turn every transient into a bogus precondition.
	if err := s.runConditionalWrite(id, current.Revision, args...); err != nil {
		var pfe *PreconditionFailedError
		if errors.As(err, &pfe) && expectedRevision != 0 {
			pfe.Expected = expectedRevision
		}
		return Bead{}, err
	}

	// Honesty guard, mirroring the unconditional close: bd can exit 0 and still
	// leave the bead open when an import-revert race (gastownhall/beads#3948)
	// rolls the committed write back. Re-read and confirm the terminal state
	// landed; the re-read also supplies the committed revision, which bd's
	// `update --json` echo omits.
	closed, err := s.Get(id)
	if err != nil {
		return Bead{}, fmt.Errorf("atomic terminal close %q: reading the committed row: %w", id, err)
	}
	if closed.Status != bdClosedStatus {
		return Bead{}, fmt.Errorf("atomic terminal close %q: bd exited 0 but status is %q, not closed; suspected gastownhall/beads#3948 import-revert race", id, closed.Status)
	}
	return closed, nil
}
