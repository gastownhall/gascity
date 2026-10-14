package beads

import "fmt"

// vetoReadyProjectionWorkOutcomes stamps IsBlocked=true, in place, on every row
// of projected that the backing's own Ready would drop for a blocker's work
// outcome. filter is that store's filterReadyByWorkOutcome: it returns the
// candidates it keeps.
//
// bd's is_blocked column counts every closed blocker as satisfied. A blocker
// closed with gc.work_outcome=blocked does not satisfy its dependents, so the
// backing's Ready applies filterReadyByWorkOutcome on top of the column.
// cachedBeadReady applies the same check, but it can only see a blocker the
// cache holds, and a reconcile lists only the active set. The first pass after
// such a close therefore evicts the blocker, and without this veto the cache
// offers the dependent for the rest of its life.
//
// Only rows the projection calls unblocked over a ready-blocking edge to a row
// outside projected are candidates. Every other blocker is resident beside its
// dependent, and cachedBeadReady already checks those. A cycle with no
// candidate spends nothing. A cycle with candidates calls filter once for all
// of them, which is the blocker read the backing's Ready makes on every call.
// For BdStore that read is its closed-inclusive `bd list --all` and
// `bd query --all`, so its cost grows with closed history, not active work.
//
// If filter fails, enrichReadyProjectionForCache returns its rows without the
// is_blocked column, exactly as it does when the is_blocked read fails. A
// prime marks the snapshot partial. A reconcile records the problem and keeps
// each cached verdict whose row's edges and resident blockers did not change,
// so an evicted blocker's dependents stay held, but a row with no cached
// verdict takes the weaker dependency-derived predicate until the next healthy
// cycle. That is an accepted trade: no persistent trigger is known, and the
// same failure fails the backing's own Ready. Keeping the column and failing
// closed for the candidates alone is tracked as ga-2h0q6q.
//
// A blocker that closes between cycles is still resident when the cache
// absorbs the close, so cachedBeadReady holds its dependents back until the
// cycle that evicts it. That cycle runs this veto.
func vetoReadyProjectionWorkOutcomes(projected []Bead, filter func([]Bead) ([]Bead, error)) error {
	candidates := readyProjectionWorkOutcomeCandidates(projected)
	if len(candidates) == 0 {
		return nil
	}
	kept, err := filter(candidates)
	if err != nil {
		return fmt.Errorf("ready projection work-outcome veto: %w", err)
	}
	vetoed := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		vetoed[c.ID] = true
	}
	for _, b := range kept {
		delete(vetoed, b.ID)
	}
	blocked := true
	for i := range projected {
		if vetoed[projected[i].ID] && projectedUnblocked(projected[i]) {
			projected[i].IsBlocked = cloneBoolPtr(&blocked)
		}
	}
	return nil
}

// readyProjectionWorkOutcomeCandidates returns each row of projected that
// vetoReadyProjectionWorkOutcomes must ask its filter about, once per id.
func readyProjectionWorkOutcomeCandidates(projected []Bead) []Bead {
	listed := make(map[string]bool, len(projected))
	for _, b := range projected {
		listed[b.ID] = true
	}
	var candidates []Bead
	seen := make(map[string]bool)
	for _, b := range projected {
		if seen[b.ID] || !projectedUnblocked(b) || !dependsOnRowOutside(b, listed) {
			continue
		}
		seen[b.ID] = true
		candidates = append(candidates, b)
	}
	return candidates
}

// projectedUnblocked reports whether b is an open row the projection called
// unblocked.
func projectedUnblocked(b Bead) bool {
	return b.ID != "" && b.Status != "closed" && b.IsBlocked != nil && !*b.IsBlocked
}

// dependsOnRowOutside reports whether b has a ready-blocking edge to a row that
// is not in listed.
func dependsOnRowOutside(b Bead, listed map[string]bool) bool {
	for _, dep := range b.Dependencies {
		if IsReadyBlockingDependencyType(dep.Type) && !listed[dep.DependsOnID] {
			return true
		}
	}
	return false
}
