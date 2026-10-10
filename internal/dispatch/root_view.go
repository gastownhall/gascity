package dispatch

import (
	"maps"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// rootView is the one read of a workflow root's direct members
// (beads.DirectMembers: the root and every bead carrying its gc.root_bead_id,
// closed included) a ProcessControl invocation makes when its kind needs the
// whole root anyway: retry and ralph to find the latest attempt,
// workflow-finalize for the run's outcome. Once read, it also answers the scope
// questions those kinds used to ask the store separately -- the scope body, the
// scope's open members and the scope snapshot -- and finalize's failure
// diagnostics. Kinds that do not need the whole root (scope-check) keep their
// narrow scoped reads (#1597); the view never adds a root-wide read.
//
// It is request-scoped by construction: ProcessControl makes a new, empty view
// for every invocation, so nothing read for one control decides another.
//
// Freshness. A view can only be older than the separate reads it replaces, and
// within one invocation the difference is a few hundred milliseconds of other
// writers. Only the dispatcher creates or closes control, scope and body beads,
// one control per root at a time (ProcessControl's single-controller rule); the
// other writers are workers closing their own work beads. A bead observed closed
// stays closed (reopen is an operator action, with the same window today). So a
// stale view can report a member open that has since closed, which delays a
// scope close to that member's own control -- each scoped member's control
// reconciles the scope when it settles -- and can never close a scope early.
// The dispatcher's own writes in the invocation are applied to the view
// (noteWrite) so it never contradicts them.
type rootView struct {
	rootID string
	loaded bool
	root   []beads.Bead
}

// rootViewMembers returns the direct members of rootID: from the invocation's
// view when opts carries one (reading them on first use), otherwise read live.
// gateRoot, when it is the root ProcessControl's gate already read, spares the
// read of the root itself.
func rootViewMembers(store beads.Store, rootID string, opts ProcessOptions) ([]beads.Bead, error) {
	view := opts.view
	if view == nil || (view.loaded && view.rootID != rootID) {
		// No view, or a second root in one invocation (not a shape any kind
		// takes): answer live.
		return readDirectMembers(store, rootID, opts.gateRoot)
	}
	if view.loaded {
		return view.root, nil
	}
	members, err := readDirectMembers(store, rootID, opts.gateRoot)
	if err != nil {
		return nil, err
	}
	view.rootID, view.loaded, view.root = rootID, true, members
	return members, nil
}

// rootViewAll returns the root's direct members when the invocation has
// already read them.
func rootViewAll(opts ProcessOptions, rootID string) ([]beads.Bead, bool) {
	if opts.view == nil || !opts.view.loaded || opts.view.rootID != rootID {
		return nil, false
	}
	return opts.view.root, true
}

func readDirectMembers(store beads.Store, rootID string, gateRoot *beads.Bead) ([]beads.Bead, error) {
	if gateRoot != nil && gateRoot.ID == rootID {
		return beads.DirectMembersWithRoot(store, *gateRoot)
	}
	return beads.DirectMembers(store, rootID)
}

// noteWrite applies one of this invocation's own writes to the view, so a later
// question answered from the view sees it.
func noteWrite(opts ProcessOptions, id string, row beads.UpdatedRow) {
	if opts.view == nil || !opts.view.loaded {
		return
	}
	for i, member := range opts.view.root {
		if member.ID != id {
			continue
		}
		// A new slice, so callers holding the old one keep what they read.
		root := slices.Clone(opts.view.root)
		member.Status = row.Status
		member.Metadata = maps.Clone(row.Metadata)
		root[i] = member
		opts.view.root = root
		return
	}
}

// viewScopeMembers is listByWorkflowRootAndScope answered from members: every
// bead carrying both gc.root_bead_id == rootID and gc.scope_ref == scopeRef,
// closed included when includeClosed.
func viewScopeMembers(members []beads.Bead, rootID, scopeRef string, includeClosed bool) []beads.Bead {
	var out []beads.Bead
	for _, member := range members {
		if member.Metadata[beadmeta.RootBeadIDMetadataKey] != rootID || member.Metadata[beadmeta.ScopeRefMetadataKey] != scopeRef {
			continue
		}
		if !includeClosed && member.Status == "closed" {
			continue
		}
		out = append(out, member)
	}
	return out
}

// viewScopeBody is resolveScopeBodyOnce answered from members: an open
// role-tagged body first, then a closed one, then any scope bead matching the
// ref.
func viewScopeBody(members []beads.Bead, rootID, scopeRef string) (beads.Bead, bool) {
	for _, open := range []bool{true, false} {
		for _, member := range members {
			if member.Metadata[beadmeta.RootBeadIDMetadataKey] != rootID ||
				member.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindScope ||
				member.Metadata[beadmeta.ScopeRoleMetadataKey] != beadmeta.ScopeRoleBody {
				continue
			}
			if open && strings.TrimSpace(member.Status) == "closed" {
				continue
			}
			if matchesScopeRef(member, scopeRef) {
				return member, true
			}
		}
	}
	return findScopeBody(members, rootID, scopeRef)
}
