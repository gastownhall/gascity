package storebindingtest

// Work topology and close-ownership conformance.
//
// A Work topology is where the scoped / unified / mixed shapes become
// observable: several semantic scopes may share ONE physical workspace, and a
// binding that opens or migrates that workspace once per scope instead of once
// per physical identity does duplicate work on the same file. The grouping the
// contract exposes is what every lifecycle participant is derived from, so a
// grouping defect reaches migration, fencing and activation alike.
//
// Close ownership is the other half. A handle that is closed twice, or that
// forgets its first result, turns a benign redundant shutdown into an error
// the operator has to explain.

import (
	"errors"

	"github.com/gastownhall/gascity/internal/storebinding"
)

// WorkTopologySuite configures one Work topology conformance run.
type WorkTopologySuite struct {
	// NewTopology returns a fresh resolved topology per assertion.
	NewTopology func(TB) storebinding.WorkTopology
	// WantPhysicalWorkspaces is how many distinct physical workspaces the
	// topology under test is composed from. Naming it is what makes the
	// unified and mixed shapes assertable rather than merely self-consistent.
	WantPhysicalWorkspaces int
}

// RunWorkTopologyTests runs the Work topology conformance suite.
func RunWorkTopologyTests(r Runner, suite WorkTopologySuite) {
	r.Helper()
	if suite.NewTopology == nil {
		r.Fatalf("storebindingtest: WorkTopologySuite.NewTopology is required")
	}
	if suite.WantPhysicalWorkspaces < 1 {
		r.Fatalf("storebindingtest: WorkTopologySuite.WantPhysicalWorkspaces must name the expected physical count")
	}

	r.Run("HQIsAlwaysResolvable", func(r Runner) {
		topology := suite.NewTopology(r)
		hq, err := topology.ForScope(storebinding.HQScope())
		if err != nil {
			r.Fatalf("ForScope(HQ): %v", err)
		}
		if hq.Store == nil {
			r.Fatalf("the HQ workspace carries no store")
		}
		if hq.Prefix == "" {
			r.Fatalf("the HQ workspace carries no pinned ID prefix; ScopeForID would have to consult mutable provider state")
		}
	})

	r.Run("UnknownScopeIsNotFound", func(r Runner) {
		topology := suite.NewTopology(r)
		_, err := topology.ForScope(storebinding.RigScope("no-such-rig"))
		if !errors.Is(err, storebinding.ErrWorkScopeNotFound) {
			r.Fatalf("ForScope(unknown) = %v, want ErrWorkScopeNotFound", err)
		}
	})

	r.Run("PhysicalWorkspacesComposeOncePerHandle", func(r Runner) {
		topology := suite.NewTopology(r)
		physical := topology.PhysicalWorkspaces()
		if len(physical) != suite.WantPhysicalWorkspaces {
			r.Fatalf("PhysicalWorkspaces = %d groups, want %d; a scope-per-open topology migrates the same file twice",
				len(physical), suite.WantPhysicalWorkspaces)
		}
		seenIdentity := map[string]bool{}
		seenScope := map[storebinding.WorkScope]bool{}
		for _, group := range physical {
			identity := group.Workspace.OpenerID + "/" + group.Workspace.ComponentID + "/" + group.Workspace.PhysicalID
			if seenIdentity[identity] {
				r.Errorf("physical identity %s appears in two groups; the same handle is opened twice", identity)
			}
			seenIdentity[identity] = true
			if len(group.Scopes) == 0 {
				r.Errorf("physical workspace %s carries no semantic scope", identity)
			}
			for _, scope := range group.Scopes {
				if seenScope[scope] {
					r.Errorf("scope %s is a member of two physical workspaces", scope)
				}
				seenScope[scope] = true
			}
		}
		for _, workspace := range topology.All() {
			if !seenScope[workspace.Scope] {
				r.Errorf("scope %s is in the topology but in no physical group; its workspace would never be migrated", workspace.Scope)
			}
		}
	})

	r.Run("MigrationSchedulesOneEntryPerPhysicalWorkspace", func(r Runner) {
		topology := suite.NewTopology(r)
		if got, want := len(topology.MigrationWorkspaces()), len(topology.PhysicalWorkspaces()); got != want {
			r.Fatalf("MigrationWorkspaces = %d entries, want the %d physical workspaces", got, want)
		}
	})

	r.Run("MemberPrefixesSurviveGrouping", func(r Runner) {
		topology := suite.NewTopology(r)
		byScope := map[storebinding.WorkScope]string{}
		for _, workspace := range topology.All() {
			byScope[workspace.Scope] = workspace.Prefix
		}
		for _, group := range topology.PhysicalWorkspaces() {
			for _, scope := range group.Scopes {
				prefix, ok := byScope[scope]
				if !ok {
					r.Errorf("grouped scope %s is not a topology member", scope)
					continue
				}
				if prefix == "" {
					r.Errorf("scope %s lost its ID prefix through grouping", scope)
				}
			}
		}
	})

	r.Run("ScopeForIDRoutesByPinnedPrefix", func(r Runner) {
		topology := suite.NewTopology(r)
		for _, workspace := range topology.All() {
			scope, err := topology.ScopeForID(workspace.Prefix + "-1")
			if err != nil {
				// A prefix shared by several scopes is a legitimate unified
				// shape; it must report the duplicate rather than pick one.
				if errors.Is(err, storebinding.ErrDuplicateWorkResidence) {
					continue
				}
				r.Errorf("ScopeForID(%q): %v", workspace.Prefix+"-1", err)
				continue
			}
			if scope != workspace.Scope {
				r.Errorf("ScopeForID(%q) = %s, want %s", workspace.Prefix+"-1", scope, workspace.Scope)
			}
		}
	})

	r.Run("UnroutableIDHasNoResidence", func(r Runner) {
		topology := suite.NewTopology(r)
		_, err := topology.ScopeForID("zzzz-unroutable-1")
		if !errors.Is(err, storebinding.ErrWorkResidenceNotFound) {
			r.Fatalf("ScopeForID(unroutable) = %v, want ErrWorkResidenceNotFound", err)
		}
	})
}

// CloseOwnershipSuite configures one close-ownership conformance run.
type CloseOwnershipSuite struct {
	// NewHandle returns a fresh opened handle per assertion.
	NewHandle func(TB) func() error
}

// RunCloseOwnershipTests proves a handle is released exactly once and that a
// redundant close is a no-op reporting the first close's verdict.
func RunCloseOwnershipTests(r Runner, suite CloseOwnershipSuite) {
	r.Helper()
	if suite.NewHandle == nil {
		r.Fatalf("storebindingtest: CloseOwnershipSuite.NewHandle is required")
	}

	r.Run("CloseIsIdempotent", func(r Runner) {
		release := suite.NewHandle(r)
		first := release()
		if first != nil {
			r.Fatalf("the first Close of an opened handle: %v", first)
		}
		if second := release(); second != nil {
			r.Fatalf("the second Close returned %v; a redundant close must be a no-op, not a new failure", second)
		}
	})
}
