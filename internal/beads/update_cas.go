package beads

import "fmt"

// UpdateCASOutcome is the durable classification of one revision-guarded
// whole-row update. A conflict is an ordinary race result, not a transport
// error.
type UpdateCASOutcome string

const (
	// UpdateCASUpdated means the expected revision matched and the update was
	// applied with authoritative readback.
	UpdateCASUpdated UpdateCASOutcome = "updated"
	// UpdateCASAlreadyApplied means the requested field values were already
	// present. This makes a retry safe after an ambiguous prior commit.
	UpdateCASAlreadyApplied UpdateCASOutcome = "already_applied"
	// UpdateCASConflict means the expected revision is stale and the requested
	// field values are not the current state.
	UpdateCASConflict UpdateCASOutcome = "conflict"
)

// UpdateCASResult identifies the revision transition observed by
// ApplyUpdateCAS. Revision is always read from the same authoritative store
// handle after the classified operation.
type UpdateCASResult struct {
	Outcome          UpdateCASOutcome
	PreviousRevision int64
	Revision         int64
}

// ApplyUpdateCAS applies one UpdateOpts atomically when expectedRevision still
// names the current bead revision. It never searches another store or falls
// back to Store.Update.
//
// A stale revision is classified by authoritative readback: if all requested
// values are already present, the result is already_applied; otherwise it is a
// conflict. Transport errors are returned immediately because the caller
// cannot know whether they happened before or after commit. Retrying the whole
// operation is safe: a prior ambiguous commit is then classified as
// already_applied.
func ApplyUpdateCAS(store Store, id string, expectedRevision int64, opts UpdateOpts) (UpdateCASResult, error) {
	if expectedRevision == 0 {
		return UpdateCASResult{}, fmt.Errorf("conditional update %s: expected revision must be a nonzero opaque token", id)
	}
	if err := validateConditionalUpdateOpts(opts); err != nil {
		return UpdateCASResult{}, fmt.Errorf("conditional update %s: %w", id, err)
	}
	// Exact-store command callers commonly hold the policy wrapper returned by
	// cmd/gc's authoritative opener. Follow only its explicitly declared
	// resolution target; never guess at interface embedding or unwrap an
	// undeclared wrapper.
	target := followConditionalWritesResolveTarget(store)
	writer, ok := ConditionalWriterFor(target)
	if !ok {
		return UpdateCASResult{}, fmt.Errorf("conditional update for %q: %w", id, ErrConditionalWriteUnsupported)
	}

	current, err := readUpdateCASBead(target, id)
	if err != nil {
		return UpdateCASResult{}, fmt.Errorf("read conditional update base for %q: %w", id, err)
	}
	if current.Revision != expectedRevision || updateOptsMatchBead(current, opts) {
		return classifyUpdateCASReadback(current, expectedRevision, opts), nil
	}

	if err := writer.UpdateIfMatch(id, expectedRevision, opts); err != nil {
		if !IsPreconditionFailed(err) {
			return UpdateCASResult{}, fmt.Errorf("conditional update for %q: %w", id, err)
		}
		current, readErr := readUpdateCASBead(target, id)
		if readErr != nil {
			return UpdateCASResult{}, fmt.Errorf("read conditional update conflict for %q: %w", id, readErr)
		}
		return classifyUpdateCASReadback(current, expectedRevision, opts), nil
	}

	updated, err := readUpdateCASBead(target, id)
	if err != nil {
		return UpdateCASResult{}, fmt.Errorf("read conditional update result for %q: %w", id, err)
	}
	if !updateOptsMatchBead(updated, opts) {
		return UpdateCASResult{}, fmt.Errorf("conditional update for %q reported success but authoritative readback does not match the request", id)
	}
	if updated.Revision == current.Revision {
		return UpdateCASResult{}, fmt.Errorf("conditional update for %q changed row-backed fields without advancing revision %d", id, current.Revision)
	}
	return UpdateCASResult{
		Outcome:          UpdateCASUpdated,
		PreviousRevision: expectedRevision,
		Revision:         updated.Revision,
	}, nil
}

func readUpdateCASBead(store Store, id string) (Bead, error) {
	switch exact := store.(type) {
	case *NativeDoltStore:
		return exact.getPreservingUpstreamStatus(id)
	case *BdStore:
		return exact.getPreservingUpstreamStatus(id)
	default:
		return store.Get(id)
	}
}

func classifyUpdateCASReadback(current Bead, expectedRevision int64, opts UpdateOpts) UpdateCASResult {
	outcome := UpdateCASConflict
	if updateOptsMatchBead(current, opts) {
		outcome = UpdateCASAlreadyApplied
	}
	return UpdateCASResult{
		Outcome:          outcome,
		PreviousRevision: expectedRevision,
		Revision:         current.Revision,
	}
}

func updateOptsMatchBead(bead Bead, opts UpdateOpts) bool {
	if opts.Title != nil && bead.Title != *opts.Title {
		return false
	}
	if opts.Status != nil && bead.Status != *opts.Status {
		return false
	}
	if opts.Type != nil && bead.Type != *opts.Type {
		return false
	}
	if opts.Priority != nil && (bead.Priority == nil || *bead.Priority != *opts.Priority) {
		return false
	}
	if opts.Description != nil && bead.Description != *opts.Description {
		return false
	}
	if opts.Assignee != nil && bead.Assignee != *opts.Assignee {
		return false
	}
	for key, want := range opts.Metadata {
		if bead.Metadata[key] != want {
			return false
		}
	}
	return true
}
