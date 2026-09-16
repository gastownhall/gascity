package sling

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// BlockerLister resolves a bead's direct dependency edges and the status of
// the beads on the far end of them. beads.Store satisfies it.
type BlockerLister interface {
	DepLister
	Get(id string) (beads.Bead, error)
}

// Blocker is one unclosed dependency holding a bead out of the ready set.
type Blocker struct {
	ID     string
	Status string // empty when the dependency target is missing from the store
	Title  string
}

// String renders one blocker for operator-facing output.
func (b Blocker) String() string {
	status := b.Status
	if status == "" {
		status = "not found in store"
	}
	if b.Title == "" {
		return fmt.Sprintf("%s (%s)", b.ID, status)
	}
	return fmt.Sprintf("%s (%s) %s", b.ID, status, b.Title)
}

// BlockedError reports that the routed bead is held out of the ready set by
// open dependencies. Routing such a bead writes gc.routed_to and reports
// success, yet still strands the work: the supervisor counts pool demand with
// a ready query, a blocked bead is never ready, and so no session can ever
// spawn to claim it. Refusing is better than a dry no-op that prints a convoy
// id, because the latter starts a clock the operator thinks is running.
type BlockedError struct {
	BeadID   string
	Target   string
	Blockers []Blocker
}

// Error returns the refusal diagnostic. Callers that can render multiple lines
// should also print Blockers, which carries the status and title of each one.
func (e *BlockedError) Error() string {
	return fmt.Sprintf(
		"REFUSED: %s is blocked by %s — routing it to %s cannot result in a claim, "+
			"because pool demand is counted with a ready query and a blocked bead is "+
			"never ready; close the blockers or re-run with --force",
		e.BeadID, strings.Join(BlockerIDs(e.Blockers), ", "), e.Target)
}

// BlockedWarning renders the non-fatal form of BlockedError, used where a dry
// run downgrades the refusal to a warning. It stays in the conditional: a dry
// run routes nothing, so claiming the bead "was routed" would be exactly the
// kind of misleading output this check exists to remove.
func BlockedWarning(beadID, target string, blockers []Blocker) string {
	return fmt.Sprintf(
		"warning: %s is blocked by %s — routing it to %s would not result in a "+
			"claim until they close",
		beadID, strings.Join(BlockerIDs(blockers), ", "), target)
}

// BlockerIDs returns just the bead IDs, in the order the blockers were found.
func BlockerIDs(blockers []Blocker) []string {
	ids := make([]string, 0, len(blockers))
	for _, b := range blockers {
		ids = append(ids, b.ID)
	}
	return ids
}

// OpenBlockers returns the unclosed blocking dependencies of beadID — the
// edges that keep it out of Ready().
//
// It mirrors the store's own readiness rule rather than reimplementing one: a
// dependency whose type is ready-blocking (beads.IsReadyBlockingDependencyType)
// and whose target is not closed blocks the bead. That keeps the answer aligned
// with what a pool's ready query will actually see. A dependency target missing
// from the store counts as blocking, because Ready() resolves its status to the
// empty string, which is likewise not "closed".
//
// Duplicate edges to the same target are reported once.
func OpenBlockers(beadID string, bl BlockerLister) ([]Blocker, error) {
	deps, err := bl.DepList(beadID, "down")
	if err != nil {
		return nil, fmt.Errorf("reading dependencies of %s: %w", beadID, err)
	}
	var blockers []Blocker
	seen := make(map[string]bool, len(deps))
	for _, d := range deps {
		if !beads.IsReadyBlockingDependencyType(d.Type) || seen[d.DependsOnID] {
			continue
		}
		seen[d.DependsOnID] = true
		target, getErr := bl.Get(d.DependsOnID)
		switch {
		case errors.Is(getErr, beads.ErrNotFound):
			blockers = append(blockers, Blocker{ID: d.DependsOnID})
		case getErr != nil:
			return nil, fmt.Errorf("reading dependency %s of %s: %w", d.DependsOnID, beadID, getErr)
		case target.Status != "closed":
			blockers = append(blockers, Blocker{ID: target.ID, Status: target.Status, Title: target.Title})
		}
	}
	return blockers, nil
}
