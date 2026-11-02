package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// releaseBlockedAssignment probes the raw blocked status before any generic
// normalized-open release can clear affinity or reopen the hold. The caller
// has already established holder eligibility and checked the release lane.
// The assignee and obsolete session affinity are cleared inside the backend's
// status-and-owner guard. A routeless claim gets a fallback only when its
// release policy admits one. A refusal never authorizes an unguarded fallback.
func releaseBlockedAssignment(store beads.Store, b beads.Bead, fallback string, clearDetached bool) (released, handled bool, err error) {
	if (b.Status != "open" && b.Status != "blocked") || strings.TrimSpace(b.Assignee) == "" {
		return false, false, nil
	}
	if _, _, err := beads.ResolveConditionalWriter(store); err != nil {
		return false, true, err
	}
	releaser, ok := beads.BlockedAssignmentReleaserFor(store)
	if !ok {
		if b.Status == "blocked" {
			return false, true, beads.ErrConditionalWriteUnsupported
		}
		return false, false, nil
	}
	metadata := clearedSessionAffinityMetadata()
	if clearDetached {
		metadata[detachedProbeMetadataKey] = ""
	}
	if fallback = strings.TrimSpace(fallback); fallback != "" && strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) == "" && strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey]) == "" {
		metadata[beadmeta.RunTargetMetadataKey] = fallback
	}
	released, handled, err = releaser.ReleaseBlockedIfCurrent(b.ID, strings.TrimSpace(b.Assignee), metadata)
	if err != nil {
		return false, true, fmt.Errorf("releasing blocked assignment %q: %w", b.ID, err)
	}
	return released, handled, nil
}

// livePoolReleaseSnapshotMatches checks the holder before the detached probe.
// Normalized-open snapshots need an ID query without a raw status filter:
// bd's --status=open excludes blocked rows before projecting them as open.
// This read establishes only snapshot currency, not the raw release status;
// the blocked writer still owns the raw status and mutation guards.
func livePoolReleaseSnapshotMatches(store beads.Store, b beads.Bead) (bool, error) {
	if b.Status != "open" && b.Status != "blocked" {
		return liveWorkAssignmentAssigneeMatches(store, b.ID, b.Status, b.Assignee)
	}
	rows, err := store.List(beads.ListQuery{IDs: []string{b.ID}, Live: true, TierMode: beads.TierBoth})
	if err != nil {
		return false, fmt.Errorf("live pool release check %q: %w", b.ID, err)
	}
	for _, row := range rows {
		if row.ID == b.ID {
			return row.Status == b.Status && strings.TrimSpace(row.Assignee) == strings.TrimSpace(b.Assignee), nil
		}
	}
	return false, nil
}
