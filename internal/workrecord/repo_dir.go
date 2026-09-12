package workrecord

// Which repository a close is judged against.
//
// The contract's reachability clause — a shipped outcome must name a commit
// reachable on the stamped branch — cannot be answered from the bead alone: it
// needs a checkout to ask git about. Every close door has to pick the same one,
// or the same bead passes through one door and is refused at another.
//
// The pick is a residency question, and the residency doctrine answers it: the
// store a row is SERVED from says where it lives, and gc.root_store_ref says who
// OWNS it. Those diverge for the population a relocated class binding holds —
// a rig's work step whose commits are on the rig's checkout, living in a
// city-scope binding — so resolving the repository from the store that answered
// the read asks the city about a rig's commit and calls a landed commit
// unreachable. Under enforcement that is a false refusal of a close that
// satisfied the contract.

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// ReachabilityUnverifiedNote is what a door reports when it cannot pose the
// reachability question at all: the bead names an owning scope, and the plane
// can point at no checkout for it. Refusing there would block a close on a
// question the door cannot ask, so the clause degrades to this warning — and
// only this clause degrades. A bead with no outcome at all is still refused,
// because "the commit could not be checked" is not a reason to accept a close
// that recorded nothing.
//
// It is a constant so both doors report the same degradation in the same words;
// a reader comparing two logs should not have to decide whether two sentences
// describe one condition.
const ReachabilityUnverifiedNote = "reachability unverified: the scope that owns this bead names no checkout"

// ScopeDirs is the checkout table a plane knows: the city's directory, and each
// rig's. It is an interface because the two doors reach the same city config by
// different routes — the HTTP plane through the server's live State, the CLI
// through the config the invocation loaded — and neither route belongs here.
//
// A returned directory is for handing to git, not for comparing as a string.
// Each implementation resolves paths through its own plane's scope resolver, and
// those disagree on spelling — one canonicalizes symlinks, the other is purely
// lexical — so a city reached through a linked path names the same repository
// two ways. git answers reachability identically through either, which is why
// this rule can consume both; a consumer that instead compares, dedupes, or
// diffs these strings across planes would first have to route both tables
// through one canonicalizing resolver.
type ScopeDirs interface {
	// CityDir returns the city checkout's directory, or "" when the plane can
	// name none.
	CityDir() string
	// RigDir returns the checkout directory of the named rig, and whether the
	// plane configures that rig at all. A configured rig that names no
	// checkout answers ("", true); an unknown rig answers ("", false). Both
	// resolve to "unknown" — the distinction is kept because the two are
	// different operator-facing facts, not because the rule branches on it.
	RigDir(name string) (string, bool)
}

// ScopeKind says which arm of the rule answered, which is what lets a caller
// tell an empty result that means "unknown" from one that means "your turn".
type ScopeKind string

const (
	// ScopeWorkDir means the bead recorded its own gc.work_dir.
	ScopeWorkDir ScopeKind = "work_dir"
	// ScopeRig means the bead's owner is a rig, and the plane named its
	// checkout.
	ScopeRig ScopeKind = "rig"
	// ScopeCity means the bead's owner is city scope — the city store or a
	// relocated class binding, which serves the whole city and belongs to no
	// rig — and the plane named the city checkout.
	ScopeCity ScopeKind = "city"
	// ScopeUnknown means the bead names an owner the plane can point at no
	// checkout for. There is no repository to ask, so the reachability clause
	// degrades (see ReachabilityUnverifiedNote). It must never be answered
	// with a default directory: git run in whatever directory the process
	// occupies answers about the wrong repository.
	ScopeUnknown ScopeKind = "unknown"
	// ScopeUnrooted means the bead records no owner at all, so the caller's own
	// legacy answer stands — the scope it read the row through. Every city that
	// predates the residency census is in this population, and each door's
	// answer for it is unchanged.
	ScopeUnrooted ScopeKind = "unrooted"
)

// RepoDirFor names the repository a shipped bead's commit must be reachable in,
// and reports which arm of the rule answered.
//
// In order: the bead's own gc.work_dir, because the commit was made where the
// work happened; then its OWNER, read from gc.root_store_ref — a rig-owned bead
// answers to that rig's checkout, a city-owned or binding-owned one to the city
// checkout. An owner the plane can point at no checkout for is ScopeUnknown and
// never the city: falling back there would ask about a repository that is not
// the bead's, which under enforcement is a false refusal rather than a degraded
// clause. A bead that records no owner is ScopeUnrooted, and the caller applies
// the answer it gave before this rule existed.
//
// scopes may be nil; a caller that knows no checkouts learns nothing about the
// bead's owner, which is ScopeUnknown rather than a panic on a close path.
func RepoDirFor(bead beads.Bead, scopes ScopeDirs) (string, ScopeKind) {
	if dir := strings.TrimSpace(bead.Metadata[beadmeta.WorkDirMetadataKey]); dir != "" {
		return dir, ScopeWorkDir
	}
	rig, scoped := storeref.ScopeRigContext(bead.Metadata[beadmeta.RootStoreRefMetadataKey])
	if !scoped {
		return "", ScopeUnrooted
	}
	if scopes == nil {
		return "", ScopeUnknown
	}
	if rig == "" {
		if dir := strings.TrimSpace(scopes.CityDir()); dir != "" {
			return dir, ScopeCity
		}
		return "", ScopeUnknown
	}
	dir, configured := scopes.RigDir(rig)
	if dir = strings.TrimSpace(dir); !configured || dir == "" {
		return "", ScopeUnknown
	}
	return dir, ScopeRig
}
