package runproj

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// RunMembers returns the beads that belong to the run rooted at rootID. The
// rule is beads.MembershipDirectRootID — the root plus every bead carrying
// gc.root_bead_id == rootID — widened by two extras that only a run projection
// needs:
//
//   - a bead whose ParentID is the root (one level, not the transitive parent
//     closure), and
//   - a bead whose id starts with "<rootID>.", the dotted shape run/iteration
//     ids take.
//
// It is deliberately NOT beads.MembershipDepReachable: a run's spec sidecars
// carry no dependency edges, so a dep walk would drop them and still report a
// plausible step count. See beads.Membership for the measurement.
//
// It is the exported form of the member selection snapshotForRun applies, so a
// consumer (e.g. the typed /v0 runs API) can list a run's steps off a folded
// bead set without re-deriving the membership rule. Order follows beadList
// (root-first is not guaranteed; callers that need the root separately match on
// id). Returns nil for an empty rootID.
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
