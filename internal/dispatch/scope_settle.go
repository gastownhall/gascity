package dispatch

import (
	"errors"
	"fmt"
	"maps"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// scopeSettledAction is the action reported by a control that found its
// enclosing scope already settled by an earlier, interrupted run of itself and
// only had to close.
const scopeSettledAction = "scope-settled"

// scopeBodyCloser records a scope body's settled metadata (its gc.outcome
// included) and closes it.
type scopeBodyCloser func(store beads.Store, bodyID string, metadata map[string]string) error

// forceCloseScopeBody records metadata on a scope body and closes it with a
// forced Close. A control that settles its scope before closing itself still
// blocks the body, and bd refuses an unforced close of a blocked bead.
func forceCloseScopeBody(store beads.Store, bodyID string, metadata map[string]string) error {
	return settleLogicalBead(store, bodyID, metadata)
}

// scopeMemberState says whether the member driving a scope reconciliation is
// already closed in the store or is a control still open that is about to
// close with the terminal state the caller passes in.
type scopeMemberState int

const (
	scopeMemberClosed scopeMemberState = iota
	scopeMemberClosing
)

// reconcileScopeForTerminalMember drives a scope after one of its members
// reached a terminal state: a failed member aborts the scope; a passed member
// that leaves no other member open closes the scope as passed.
//
// For a closing member (scopeMemberClosing) the member is still open in the
// store, so it is ignored when counting open members and its terminal copy
// stands in for it in the snapshot; the body is closed with a forced close;
// and a body that is already closed means this member's own earlier settle
// completed, so the scope is left exactly as recorded and the result's action
// is scopeSettledAction.
func reconcileScopeForTerminalMember(store beads.Store, bead beads.Bead, opts ProcessOptions, state scopeMemberState) (ControlResult, error) {
	scopeRef := bead.Metadata[beadmeta.ScopeRefMetadataKey]
	if scopeRef == "" {
		return ControlResult{}, nil
	}
	rootID := bead.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}
	body, err := resolveScopeBody(store, rootID, scopeRef, bead.ID, opts)
	if err != nil {
		if errors.Is(err, errScopeBodyMissing) {
			return ControlResult{}, fmt.Errorf("%w: %w", ErrControlGraphMalformed, err)
		}
		return ControlResult{}, fmt.Errorf("%s: loading scope body for %s: %w", bead.ID, scopeRef, err)
	}

	closeBody := scopeBodyCloser(updateMetadataAndClose)
	var ignoreIDs []string
	if state == scopeMemberClosing {
		// body was read in this invocation, and only the dispatcher closes a
		// scope body, one control per root at a time.
		if body.Status == "closed" {
			return ControlResult{Action: scopeSettledAction}, nil
		}
		closeBody = forceCloseScopeBody
		ignoreIDs = []string{bead.ID}
	}
	loadSnapshot := func() (scopeSnapshot, error) {
		snapshot, err := loadScopeSnapshotWithBody(store, rootID, scopeRef, body)
		if err != nil {
			return scopeSnapshot{}, fmt.Errorf("%s: loading scope snapshot for %s: %w", bead.ID, scopeRef, err)
		}
		if state == scopeMemberClosing {
			snapshot.substituteMember(bead)
		}
		return snapshot, nil
	}

	if beadOutcomeFailed(bead) {
		snapshot, err := loadSnapshot()
		if err != nil {
			return ControlResult{}, err
		}
		skipped, err := abortScopeWith(store, snapshot, opts, bead.ID, closeBody)
		if err != nil {
			return ControlResult{}, err
		}
		return ControlResult{Processed: true, Action: "scope-fail", Skipped: skipped}, nil
	}

	remainingOpen, err := hasOpenScopeMembers(store, rootID, scopeRef, ignoreIDs...)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: checking scope completion: %w", bead.ID, err)
	}
	if remainingOpen {
		return ControlResult{}, nil
	}

	if state == scopeMemberClosed {
		bodyAfter, err := store.Get(body.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: reloading scope body: %w", body.ID, err)
		}
		if bodyAfter.Status == "closed" {
			return ControlResult{}, nil
		}
	}
	snapshot, err := loadSnapshot()
	if err != nil {
		return ControlResult{}, err
	}
	if err := closeScopeAsPassedWith(store, snapshot, bead, opts, bead.ID, closeBody); err != nil {
		return ControlResult{}, err
	}
	return ControlResult{Processed: true, Action: "scope-pass"}, nil
}

// substituteMember replaces the snapshot's copy of member (matched by ID) with
// member itself, so a control settling its scope before it closes is seen in
// the terminal state it is about to write.
func (s *scopeSnapshot) substituteMember(member beads.Bead) {
	for i := range s.members {
		if s.members[i].ID == member.ID {
			s.members[i] = member
		}
	}
	s.all = mergeScopeSnapshotBeads(s.members, s.body)
}

// closeScopedControl is the terminal close of a control that reconciles its
// own enclosing scope (fanout, drain: the scope-check-exempt kinds that pair
// with no scope-check). The scope is settled first, as if the control had
// already closed with closeMetadata, and the control closes last: it is the
// only thing that re-drives that settle, so it must stay open until the scope
// is durable. A re-run of an open control whose scope is already settled
// closes the control without touching the scope and reports
// scopeSettledAction. action is the control's own action, reported when this
// run did the settling.
func closeScopedControl(store beads.Store, controlID string, closeMetadata map[string]string, action string, opts ProcessOptions) (ControlResult, error) {
	current, err := store.Get(controlID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: reloading control before close: %w", controlID, err)
	}
	terminal := current
	terminal.Status = "closed"
	terminal.Metadata = maps.Clone(current.Metadata)
	if terminal.Metadata == nil {
		terminal.Metadata = make(map[string]string, len(closeMetadata))
	}
	maps.Copy(terminal.Metadata, closeMetadata)

	scopeResult, err := reconcileScopeForTerminalMember(store, terminal, opts, scopeMemberClosing)
	if err != nil {
		return ControlResult{}, err
	}
	if err := updateMetadataAndClose(store, controlID, closeMetadata); err != nil {
		return ControlResult{}, fmt.Errorf("%s: closing %s control: %w", controlID, current.Metadata[beadmeta.KindMetadataKey], err)
	}
	if scopeResult.Action == scopeSettledAction {
		action = scopeSettledAction
	}
	return ControlResult{Processed: true, Action: action, Skipped: scopeResult.Skipped}, nil
}

// isPartialInstantiationResidue reports whether bead is what is left of a
// partially instantiated fanout fragment or retry attempt: closed as skipped
// and re-created under fresh IDs by the next pass. Residue did no scope work,
// so its metadata (including the molecule_failed mark the failed
// instantiation left) must not bubble onto the scope body.
func isPartialInstantiationResidue(bead beads.Bead) bool {
	return bead.Metadata[beadmeta.PartialFragmentMetadataKey] == "true" ||
		bead.Metadata[beadmeta.PartialRetryMetadataKey] == "true"
}
