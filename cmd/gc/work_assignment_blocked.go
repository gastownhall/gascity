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
// status-and-owner guard. A routeless claim also gets the approved fallback. A
// refusal stops this release; it never authorizes an unguarded fallback.
func releaseBlockedAssignment(store beads.Store, b beads.Bead, fallback string) (released, handled bool, err error) {
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
	if fallback = strings.TrimSpace(fallback); fallback != "" && strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) == "" && strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey]) == "" {
		metadata[beadmeta.RunTargetMetadataKey] = fallback
	}
	released, handled, err = releaser.ReleaseBlockedIfCurrent(b.ID, strings.TrimSpace(b.Assignee), metadata)
	if err != nil {
		return false, true, fmt.Errorf("releasing blocked assignment %q: %w", b.ID, err)
	}
	return released, handled, nil
}
