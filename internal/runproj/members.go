package runproj

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// RunMembers returns the beads that belong to the run rooted at rootID: the root
// itself plus every child that references it by parent id, gc.root_bead_id
// metadata, or a dotted-id prefix. It is the exported form of the member
// selection snapshotForRun applies, so a consumer (e.g. the typed /v0 runs API)
// can list a run's steps off a folded bead set without re-deriving the
// membership rule. Order follows beadList (root-first is not guaranteed; callers
// that need the root separately match on id). Returns nil for an empty rootID.
func RunMembers(beadList []beads.Bead, rootID string) []beads.Bead {
	if rootID == "" {
		return nil
	}
	var members []beads.Bead
	for i := range beadList {
		if isRunMember(beadList[i], rootID) {
			members = append(members, beadList[i])
		}
	}
	return members
}

// isRunMember reports whether b belongs to the run rooted at rootID. It is the
// single source of the membership predicate shared by RunMembers and
// snapshotForRun.
func isRunMember(b beads.Bead, rootID string) bool {
	return b.ID == rootID ||
		b.ParentID == rootID ||
		b.Metadata[beadmeta.RootBeadIDMetadataKey] == rootID ||
		strings.HasPrefix(b.ID, rootID+".")
}
