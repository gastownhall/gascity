package session

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
)

// ListAllSessionBeads returns every session bead from the store using a
// type+label union so canonical session beads that have lost their
// gc:session label (after a crash, partial write, or schema migration)
// still surface alongside legacy records that retain the label but have
// an empty type.
//
// Two indexed store.List queries are issued:
//   - one with Type=BeadType — the authoritative source for session beads
//   - one with Label=LabelSession — catches repairable Type="" beads
//
// Results are unioned, deduped by bead ID, and filtered through
// IsSessionBeadOrRepairable so the returned slice is exactly the set of
// beads downstream code treats as sessions.
//
// base is preserved for any filter fields the caller cares about
// (IncludeClosed, Sort, Status, Assignee, Metadata, Limit, Live, etc.).
// base.Type and base.Label are overridden by the union queries
// internally — callers should not set them.
//
// PartialResultError semantics: if either underlying List returns a
// PartialResultError, its (partial) rows are still folded into the
// union, and a PartialResultError is returned alongside the merged
// result so callers can surface degraded-but-non-empty output. Any
// other (hard) error short-circuits and returns nil rows. The hard
// error is wrapped with context naming which leg failed so logs are
// diagnosable.
func ListAllSessionBeads(store beads.Store, base beads.ListQuery) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}

	// Limit is applied globally after the union (see below); passing
	// base.Limit into each leg independently could return up to 2× the
	// requested rows or drop the correct top-N when the union spans
	// both legs.
	byTypeQuery := base
	byTypeQuery.Type = BeadType
	byTypeQuery.Label = ""
	byTypeQuery.Limit = 0
	byType, typeErr := store.List(byTypeQuery)
	if typeErr != nil && !beads.IsPartialResult(typeErr) {
		return nil, fmt.Errorf("listing session beads by type: %w", typeErr)
	}

	byLabelQuery := base
	byLabelQuery.Type = ""
	byLabelQuery.Label = LabelSession
	byLabelQuery.Limit = 0
	byLabel, labelErr := store.List(byLabelQuery)
	if labelErr != nil && !beads.IsPartialResult(labelErr) {
		return nil, fmt.Errorf("listing session beads by label: %w", labelErr)
	}

	seen := make(map[string]struct{}, len(byType)+len(byLabel))
	out := make([]beads.Bead, 0, len(byType)+len(byLabel))
	for _, b := range byType {
		if _, dup := seen[b.ID]; dup {
			continue
		}
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		seen[b.ID] = struct{}{}
		out = append(out, b)
	}
	for _, b := range byLabel {
		if _, dup := seen[b.ID]; dup {
			continue
		}
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		seen[b.ID] = struct{}{}
		out = append(out, b)
	}

	// Each leg's store.List honored base.Sort within its result set, but
	// the union concatenates them — sort globally so mixed-shape rows
	// interleave correctly. Unknown Sort values are left alone for
	// forward-compat with future sort modes.
	switch base.Sort {
	case beads.SortCreatedAsc:
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		})
	case beads.SortCreatedDesc:
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		})
	}

	if base.Limit > 0 && len(out) > base.Limit {
		out = out[:base.Limit]
	}

	// Surface the first partial-result error encountered. Either leg
	// being partial means the merged set may be missing rows; callers
	// already handle PartialResultError to render a degraded view.
	if typeErr != nil {
		return out, typeErr
	}
	if labelErr != nil {
		return out, labelErr
	}
	return out, nil
}
