package sling

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// refCheckResolver is a BranchResolver whose BaseRefMissing answers from a
// fixed set, and which records every ref it was asked about.
type refCheckResolver struct {
	missing map[string]bool
	asked   *[]string
}

func (r refCheckResolver) DefaultBranch(string) string { return "main" }

func (r refCheckResolver) BaseRefMissing(_, ref string) bool {
	if r.asked != nil {
		*r.asked = append(*r.asked, ref)
	}
	return r.missing[ref]
}

func baseBranchSling(t *testing.T, branches BranchResolver, vars ...string) (SlingResult, SlingDeps, beads.Bead, error) {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Branches = branches
	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	res, err := s.AttachFormula(context.Background(), "code-review", b.ID, a, FormulaOpts{Vars: vars})
	return res, deps, b, err
}

// A base_branch that names no ref is refused before anything is written: the
// bead is created unbuildable otherwise, and the molecule only finds out when
// its workspace step fails (ga-c1gvvf).
func TestSlingRefusesUnresolvableBaseBranch(t *testing.T) {
	res, deps, b, err := baseBranchSling(t, refCheckResolver{missing: map[string]bool{"ghost/base": true}}, "base_branch=ghost/base")
	if err == nil {
		t.Fatal("sling with an unresolvable base_branch succeeded, want a refusal")
	}
	for _, want := range []string{"base_branch", "ghost/base", "do not retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if res.WispRootID != "" {
		t.Errorf("WispRootID = %q, want none: refusal must precede molecule creation", res.WispRootID)
	}
	got, getErr := deps.Store.Get(b.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Metadata["gc.routed_to"] != "" || got.Assignee != "" {
		t.Errorf("refused sling still touched the bead: routed_to=%q assignee=%q", got.Metadata["gc.routed_to"], got.Assignee)
	}
}

func TestSlingAcceptsResolvableBaseBranch(t *testing.T) {
	var asked []string
	res, _, _, err := baseBranchSling(t, refCheckResolver{asked: &asked}, "base_branch=release/v2")
	if err != nil {
		t.Fatalf("sling with a resolvable base_branch: %v", err)
	}
	if res.WispRootID == "" {
		t.Error("expected a molecule root for the accepted sling")
	}
	if len(asked) == 0 || asked[0] != "release/v2" {
		t.Errorf("resolver asked about %v, want release/v2", asked)
	}
}

func TestSlingWithoutBaseBranchOrResolverIsUnchanged(t *testing.T) {
	var asked []string
	if _, _, _, err := baseBranchSling(t, refCheckResolver{asked: &asked}); err != nil {
		t.Fatalf("sling without base_branch: %v", err)
	}
	if len(asked) != 0 {
		t.Errorf("resolver asked about %v with no base_branch var, want no calls", asked)
	}
	if _, _, _, err := baseBranchSling(t, nil, "base_branch=ghost/base"); err != nil {
		t.Fatalf("sling with a nil resolver must skip the check: %v", err)
	}
}
