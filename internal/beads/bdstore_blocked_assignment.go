package beads

import (
	"fmt"
	"strings"
)

var _ BlockedAssignmentReleaser = (*BdStore)(nil)

// ReleaseBlockedIfCurrent preserves a raw blocked hold and clears only its
// assignee under bd's native status/holder guards. A lost blocked guard owns
// the outcome; it cannot become permission for an ordinary open release.
func (s *BdStore) ReleaseBlockedIfCurrent(id, expectedAssignee string) (released, handled bool, err error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(expectedAssignee) == "" {
		return false, true, fmt.Errorf("blocked release %q: full bead ID and holder required", id)
	}
	if err := s.guardRelocatedClassIDs("blocked release "+id, id); err != nil {
		return false, true, err
	}
	out, err := s.runBDTransientRead("show", "--json", id)
	if err != nil {
		if !isBdBeadNotFound(err) && !isBdNotFoundEnvelope(out) {
			return false, true, fmt.Errorf("blocked release %q: %w", id, err)
		}
		if !isWispQueryableID(id) {
			return false, true, fmt.Errorf("blocked release %q: %w", id, err)
		}
		// The existing Get fallback for the wisp plane, retaining the raw row.
		out, err = s.runBDTransientRead("query", "--json", "ephemeral=true AND id="+id, "--all", "--limit", "1")
		if err != nil {
			return false, true, fmt.Errorf("blocked release %q: reading wisp: %w", id, err)
		}
	}
	rows, err := parseIssuesTolerant(extractJSON(out))
	if err != nil {
		return false, true, fmt.Errorf("blocked release %q: decoding raw row: %w", id, err)
	}
	if len(rows) != 1 {
		return false, true, nil
	}
	row := rows[0]
	if row.ID != id {
		return false, true, fmt.Errorf("blocked release %q resolved to %q: %w", id, row.ID, ErrIDCollision)
	}
	if strings.TrimSpace(row.Assignee) != strings.TrimSpace(expectedAssignee) {
		return false, true, nil
	}
	if row.Status == "open" {
		return false, false, nil
	}
	if row.Status != "blocked" {
		return false, true, nil
	}
	empty := ""
	released, err = s.UpdateIfAssignment(id, "blocked", expectedAssignee, UpdateOpts{Assignee: &empty})
	return released, true, err
}
