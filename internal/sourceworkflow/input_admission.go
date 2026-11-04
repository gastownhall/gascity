package sourceworkflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/molecule"
)

// InputStores names the classes participating in native execution admission.
// Graph owns the candidate and its root; Work owns the input convoy. Relations
// may reference either named class. No store outside this set is consulted.
type InputStores struct {
	Graph beads.GraphStore
	Work  []beads.WorkStore
}

// InputAdmission is the current input eligibility of a native workflow step.
// Ready permits admission; Reason names a durable prerequisite or park that
// withholds it. Read failures return an error and never a Ready result.
type InputAdmission struct {
	Ready bool
	// BlockerID is the prerequisite or input that withholds admission.
	BlockerID string
	Reason    string
}

// Admission reads the root's durable input-convoy relation and requires every
// nonterminal input's current prerequisites and explicit parks to permit work.
// An input need not close before execution. Routes and assignees are separate
// admission constraints. The result is scoped to this read, never persisted.
//
// Finalization, teardown and terminal-root cleanup retain their lifecycle
// semantics. Ordinary work with no native input relation keeps its eligibility.
func (s InputStores) Admission(ctx context.Context, candidate beads.Bead) (InputAdmission, error) {
	ready := InputAdmission{Ready: true}
	if candidate.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflowFinalize ||
		candidate.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown {
		return ready, nil
	}
	rootID := strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey])
	if strings.TrimSpace(candidate.Metadata[beadmeta.InputConvoyIDMetadataKey]) != "" {
		rootID = candidate.ID
	}
	if rootID == "" {
		return ready, nil
	}
	if err := ctx.Err(); err != nil {
		return InputAdmission{}, err
	}
	if beads.IsNilStore(s.Graph) {
		return InputAdmission{}, fmt.Errorf("input admission: graph store missing for %s", candidate.ID)
	}
	root, err := beads.HandlesFor(s.Graph).Live.Get(rootID)
	if err != nil {
		return InputAdmission{}, fmt.Errorf("input admission: reading workflow root %s: %w", rootID, err)
	}
	if root.ID != rootID {
		return InputAdmission{}, fmt.Errorf("input admission: root %s resolved to %s", rootID, root.ID)
	}
	if convoy.IsTerminalStatus(root.Status) || root.Metadata[beadmeta.CancelRequestedMetadataKey] != "" ||
		root.Metadata[beadmeta.OutcomeMetadataKey] == beadmeta.OutcomeCanceled {
		return ready, nil
	}
	convoyID := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
	if convoyID == "" {
		return ready, nil
	}
	walk := inputAdmissionWalk{ctx: ctx, stores: s, convoyID: convoyID, now: time.Now(), active: make(map[string]bool), done: make(map[string]bool)}
	input, owner, err := walk.resolve(convoyID, false)
	if err != nil {
		return InputAdmission{}, err
	}
	if err := walk.visit(input, owner, true); err != nil {
		return InputAdmission{}, err
	}
	if walk.reason != "" {
		// Attempts can inherit teardown through their step id alone. Reuse the
		// existing tail definition rather than copying a topology onto inputs.
		tail, err := molecule.IsTeardownTailMember(beads.GraphMolecule(s.Graph), candidate)
		if err != nil {
			return InputAdmission{}, fmt.Errorf("input admission: resolving teardown for %s: %w", candidate.ID, err)
		}
		if !tail {
			return InputAdmission{BlockerID: walk.blocker, Reason: walk.reason}, nil
		}
	}
	return ready, nil
}

// The bound limits malformed/cyclic relation walks independently of store size.
const maxInputAdmissionRelations = 1024

type inputAdmissionWalk struct {
	ctx          context.Context
	stores       InputStores
	convoyID     string
	now          time.Time
	active, done map[string]bool
	reads        int
	reason       string
	blocker      string
}

func (w *inputAdmissionWalk) check() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.reads++
	if w.reads > maxInputAdmissionRelations {
		return fmt.Errorf("input admission: relation walk exceeds %d reads", maxInputAdmissionRelations)
	}
	return nil
}

// resolve proves residence over the named classes. The convoy itself is Work;
// its members and prerequisites may also be Graph. A failed participant cannot
// prove absence, and duplicate residence is never resolved by picking a copy.
func (w *inputAdmissionWalk) resolve(id string, includeGraph bool) (beads.Bead, beads.LiveReader, error) {
	var found beads.Bead
	var owner beads.LiveReader
	var seen []beads.Store
	stores := make([]beads.Store, 0, len(w.stores.Work)+1)
	for _, work := range w.stores.Work {
		stores = append(stores, work)
	}
	if includeGraph && !beads.IsNilStore(w.stores.Graph) {
		stores = append(stores, w.stores.Graph)
	}
	for _, store := range stores {
		if beads.IsNilStore(store) {
			continue
		}
		repeat := false
		for _, prior := range seen {
			if beads.SameStore(store, prior) {
				repeat = true
				break
			}
		}
		if repeat {
			continue
		}
		seen = append(seen, store)
		if err := w.check(); err != nil {
			return beads.Bead{}, nil, err
		}
		live := beads.HandlesFor(store).Live
		b, err := live.Get(id)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return beads.Bead{}, nil, fmt.Errorf("input admission: reading relation target %s: %w", id, err)
		}
		if b.ID != id {
			return beads.Bead{}, nil, fmt.Errorf("input admission: relation target %s resolved to %s", id, b.ID)
		}
		if owner != nil {
			return beads.Bead{}, nil, fmt.Errorf("input admission: duplicate residence for %s: %w", id, convoy.ErrDuplicateResidence)
		}
		found, owner = b, live
	}
	if owner == nil {
		return beads.Bead{}, nil, fmt.Errorf("input admission: relation target %s: %w", id, beads.ErrNotFound)
	}
	return found, owner, nil
}

func (w *inputAdmissionWalk) hold(id, reason string) {
	if w.reason == "" {
		w.reason = reason
		w.blocker = id
	}
}

func (w *inputAdmissionWalk) visit(b beads.Bead, owner beads.LiveReader, input bool) error {
	if w.active[b.ID] {
		return fmt.Errorf("input admission: cyclic relation at %s", b.ID)
	}
	key := b.ID + ":prerequisite"
	if input {
		key = b.ID + ":input"
	}
	if w.done[key] {
		return nil
	}
	terminal := input && convoy.IsTerminalStatus(b.Status)
	if terminal && b.ID != w.convoyID {
		return nil
	}
	if !input && beads.DependencySatisfied(b.Status, beads.ReadinessWorkOutcome(b.Metadata)) {
		return nil
	}
	w.active[b.ID] = true
	defer delete(w.active, b.ID)
	if !input {
		w.hold(b.ID, fmt.Sprintf("prerequisite %s is not satisfied", b.ID))
	}
	if !terminal && (b.Status == "blocked" || b.SourceStatus == "blocked" || b.Status == "deferred" || b.SourceStatus == "deferred" ||
		b.IndefinitelyDeferred || (b.DeferUntil != nil && b.DeferUntil.After(w.now)) || beadmeta.HasDispatchHold(b.Labels)) {
		w.hold(b.ID, fmt.Sprintf("input %s is parked", b.ID))
	}
	if err := w.check(); err != nil {
		return err
	}
	deps, err := owner.DepList(b.ID, "down")
	if err != nil {
		return fmt.Errorf("input admission: reading relations of %s: %w", b.ID, err)
	}
	for _, dep := range deps {
		tracked := input && dep.Type == convoy.TrackingDepType
		if !tracked && (terminal || !beads.IsReadyBlockingDependencyType(dep.Type)) {
			continue
		}
		target, capability := beads.ExternalBlockerTarget(dep.DependsOnID)
		if capability && !tracked {
			continue
		}
		member, next, err := w.resolve(target, true)
		if err != nil {
			return err
		}
		if err := w.visit(member, next, tracked); err != nil {
			return err
		}
	}
	w.done[key] = true
	return nil
}
