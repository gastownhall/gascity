package dispatch

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Fail-halts is the [workflows] fail_halts policy: a step's terminal outcome
// binds the rest of its workflow. Two rules, both off unless the switch is on:
//
//   - A needs edge on a step whose terminal outcome is fail halts the
//     dependent. The dependent is closed gc.outcome=skipped with gc.halted_by
//     naming the failed step instead of running, and a halted bead halts its
//     own dependents in turn, so the halt walks the graph one ready bead at a
//     time. It is enforced where a bead would start running: ProcessControl for
//     control beads and the hook claim for worker steps (HaltIfNeedsHaltingStep).
//   - A workflow root may not close pass while any step's terminal outcome is
//     fail. Finalize closes it fail and names the step: the switch widens the
//     abort-scope member scan (terminalAbortScopeFailureMember) to every
//     member, so the existing failure diagnostics name it.
//
// Finalizers are exempt from the halt and always run (haltExempt), so a
// halted workflow still closes rather than strands.
//
// A step's terminal outcome is its own gc.outcome, except for a retried step:
// every retry attempt or loop iteration (isRetryAttemptSubject) is graded by
// the retry, ralph or scope machinery that owns it, which records the last
// attempt's verdict on the logical bead. So an attempt that failed and was
// retried never halts anything, and a later attempt's pass is not outvoted by
// it. A fail of class transient counts once it is terminal: a retry that
// exhausted its attempts on transient failures did not do its job either.

// haltedCloseReasonPrefix starts the close_reason of a halted bead. bd's
// validation.on-close rejects a close reason shorter than 20 characters, so the
// reason is prose that also names the step.
const haltedCloseReasonPrefix = "halted: needs a step that did not succeed"

// failHalts reports whether the city has [workflows] fail_halts on. A config
// that cannot be loaded reads as off, which is today's behaviour.
func (opts ProcessOptions) failHalts() bool {
	cfg, err := opts.routeConfig()
	if err != nil {
		return false
	}
	return cfg.FailHaltsEnabled()
}

// terminalStepFailure reports whether a closed bead's terminal outcome is
// fail. A retry attempt or loop iteration is never terminal: its logical bead
// carries the last attempt's verdict.
func terminalStepFailure(b beads.Bead) bool {
	if b.Status != "closed" || isRetryAttemptSubject(b) {
		return false
	}
	return strings.TrimSpace(b.Metadata[beadmeta.OutcomeMetadataKey]) == beadmeta.OutcomeFail
}

// stepOutcomeHalts reports whether a closed bead's terminal outcome blocks the
// beads that need it. A halted bead always passes the halt on, attempt or
// not, so a halted attempt halts the retry control waiting for it rather than
// reading as an attempt to retry.
func stepOutcomeHalts(b beads.Bead) bool {
	if b.Status != "closed" {
		return false
	}
	if strings.TrimSpace(b.Metadata[beadmeta.OutcomeMetadataKey]) == beadmeta.OutcomeSkipped {
		return strings.TrimSpace(b.Metadata[beadmeta.HaltedByMetadataKey]) != ""
	}
	return terminalStepFailure(b)
}

// haltExempt reports whether a bead must run even when a step it needs did not
// succeed: the finalizer settles the root, a scope-check grades its subject and
// aborts or settles the scope, and teardown runs after settlement by contract.
func haltExempt(b beads.Bead) bool {
	switch b.Metadata[beadmeta.KindMetadataKey] {
	case beadmeta.KindWorkflowFinalize, beadmeta.KindScopeCheck:
		return true
	}
	return b.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown
}

// haltingBlocker returns the first closed blocker in the same workflow whose
// terminal outcome halts bead.
func haltingBlocker(store beads.Store, bead beads.Bead) (beads.Bead, bool, error) {
	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" {
		return beads.Bead{}, false, nil
	}
	deps, err := store.DepList(bead.ID, "down")
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("%s: listing blockers: %w", bead.ID, err)
	}
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		blocker, err := store.Get(dep.DependsOnID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return beads.Bead{}, false, fmt.Errorf("%s: loading blocker %s: %w", bead.ID, dep.DependsOnID, err)
		}
		if strings.TrimSpace(blocker.Metadata[beadmeta.RootBeadIDMetadataKey]) != rootID {
			continue
		}
		if stepOutcomeHalts(blocker) {
			return blocker, true, nil
		}
	}
	return beads.Bead{}, false, nil
}

// HaltIfNeedsHaltingStep closes bead skipped, naming the step that halted it,
// when one of the steps it needs closed with an outcome that halts its
// dependents. It reports the halting step's ID and whether bead was halted.
// The caller decides whether [workflows] fail_halts is on; this only applies
// the rule. A bead that is not open, not a workflow member, or exempt is left
// alone.
func HaltIfNeedsHaltingStep(store beads.Store, bead beads.Bead) (string, bool, error) {
	if bead.Status == "closed" || haltExempt(bead) {
		return "", false, nil
	}
	blocker, ok, err := haltingBlocker(store, bead)
	if err != nil || !ok {
		return "", false, err
	}
	outcome := strings.TrimSpace(blocker.Metadata[beadmeta.OutcomeMetadataKey])
	if err := updateMetadataAndClose(store, bead.ID, map[string]string{
		beadmeta.OutcomeMetadataKey:  beadmeta.OutcomeSkipped,
		beadmeta.HaltedByMetadataKey: blocker.ID,
		"close_reason":               fmt.Sprintf("%s: %s closed %s", haltedCloseReasonPrefix, blocker.ID, outcome),
	}); err != nil {
		return "", false, fmt.Errorf("%s: closing halted bead: %w", bead.ID, err)
	}
	// A halted scope member settles its scope like any other skipped member.
	// Best-effort, as for the other close paths that reconcile a scope.
	_, _ = reconcileClosedScopeMember(store, bead.ID)
	return blocker.ID, true, nil
}

// haltControl applies the halt rule to a control bead about to be processed.
func haltControl(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, bool, error) {
	if haltExempt(bead) || !opts.failHalts() {
		return ControlResult{}, false, nil
	}
	blockerID, halted, err := HaltIfNeedsHaltingStep(store, bead)
	if err != nil || !halted {
		return ControlResult{}, false, err
	}
	opts.tracef("process-control bead=%s kind=%s halted halted_by=%s",
		bead.ID, bead.Metadata[beadmeta.KindMetadataKey], blockerID)
	return ControlResult{Processed: true, Action: "halted"}, true, nil
}
