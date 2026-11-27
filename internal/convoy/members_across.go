package convoy

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// MembersAcross returns the members convoyID tracks as the UNION of what each
// scope records: each scope answers with MembersIn over its own Convoy handle,
// so a store that has never seen the convoy contributes nothing and no store's
// silence can subtract a member another records. A member one scope reports as
// an unresolved placeholder that another resolves as a real bead keeps the real
// bead.
//
// Reading membership as a union rather than from the one store that "should"
// hold the convoy is what keeps a whole convoy from being silently lost: a
// store that does not know the convoy answers both the legacy-children List
// and the tracks DepList EMPTY rather than erroring, and an empty membership
// is indistinguishable from a genuinely empty convoy. The union is order-free
// and monotone, so a zero-row answer over a non-empty convoy is structurally
// impossible.
//
// A scope whose read fails fails the union with that scope as provenance
// unless its policy is storeref.PolicyPartialDegrade, which skips that scope
// and continues with the remaining ones — the policy a degraded plan leg
// carries.
func MembersAcross(scopes []MemberScope, convoyID string, includeClosed bool) ([]beads.Bead, error) {
	var members []beads.Bead
	at := make(map[string]int)
	for _, scope := range scopes {
		if beads.IsNilStore(scope.Classes.Convoy) {
			continue
		}
		scopeMembers, err := MembersIn(scope.Classes, convoyID, includeClosed)
		if err != nil {
			if scope.OnError == storeref.PolicyPartialDegrade {
				continue
			}
			return nil, fmt.Errorf("reading members of convoy %s across stores: %w", convoyID, err)
		}
		for _, member := range scopeMembers {
			i, seen := at[member.ID]
			if !seen {
				at[member.ID] = len(members)
				members = append(members, member)
				continue
			}
			if IsUnresolvedTrackedItem(members[i]) && !IsUnresolvedTrackedItem(member) {
				members[i] = member
			}
		}
	}
	// The same member order MembersIn returns within one store, so the merged
	// order does not depend on which scope answered first.
	sort.SliceStable(members, func(a, b int) bool {
		if members[a].CreatedAt.Equal(members[b].CreatedAt) {
			return members[a].ID < members[b].ID
		}
		return members[a].CreatedAt.Before(members[b].CreatedAt)
	})
	return members, nil
}

// MemberScope is one source a MembersAcross union reads: the classes that
// source resolves membership and members through, and what a failure of its
// read means.
type MemberScope struct {
	// Classes names the class handles this source answers with; its Convoy
	// handle is the store whose membership edges are read.
	Classes MemberClasses
	// OnError is the storeref error policy of this scope's read. The zero
	// value, PolicyFatal, fails the union; PolicyPartialDegrade records the
	// failure and continues with the other scopes.
	OnError storeref.ErrPolicy
}
