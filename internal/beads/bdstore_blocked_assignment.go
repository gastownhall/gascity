package beads

import (
	"fmt"
	"strings"
)

var _ BlockedAssignmentReleaser = (*BdStore)(nil)

// ReleaseBlockedIfCurrent preserves a raw blocked hold and clears its assignee
// with the metadata patch under bd's native status/holder guards. A lost guard owns
// the outcome; it cannot become permission for an ordinary open release.
func (s *BdStore) ReleaseBlockedIfCurrent(id, expectedAssignee string, metadata map[string]string) (released, handled bool, err error) {
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
			// A confirmed missing non-wisp no longer has an assignment to
			// release. Do not turn absence into an operational failure.
			return false, true, nil
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
	released, err = s.UpdateIfAssignment(id, "blocked", expectedAssignee, UpdateOpts{Assignee: &empty, Metadata: metadata})
	return released, true, err
}
