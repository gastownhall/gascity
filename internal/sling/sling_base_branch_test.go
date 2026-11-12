package sling

import (
	"context"
	"path/filepath"
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
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	requireBaseBranchRefusal(t, err, "ghost/base", SlingFormulaRepoDir(b.ID, deps, a))
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

// requireBaseBranchRefusal asserts err is the base_branch refusal: it names the
// ref, the repository it was looked up in (the base name only, so no absolute
// path reaches an API client) and that retrying unchanged is pointless.
func requireBaseBranchRefusal(t *testing.T, err error, ref, repoDir string) {
	t.Helper()
	if err == nil {
		t.Fatal("sling with an unresolvable base_branch succeeded, want a refusal")
	}
	for _, want := range []string{"base_branch", ref, filepath.Base(repoDir), "do not retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), repoDir) {
		t.Errorf("error %q leaks the absolute repository path %q", err, repoDir)
	}
}

// ghostBaseSling runs DoSling over a bead a prior actor claimed
// (in_progress, assigned), for a target and options shaped by build, against a
// resolver that finds no "ghost/base" ref. The sling must be refused and the
// claimed bead left exactly as it was: --reassign reopens and unassigns a bead
// before routing, so a refusal that comes after it strands the bead for good.
func ghostBaseSling(t *testing.T, a config.Agent, build func(beadID string) SlingOpts) {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Branches = refCheckResolver{missing: map[string]bool{"ghost/base": true}}
	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Assignee: "prior-actor"})
	if err != nil {
		t.Fatal(err)
	}
	// Create does not take a status; claim the bead the way `bd update --claim` does.
	claimed := "in_progress"
	if err := deps.Store.Update(b.ID, beads.UpdateOpts{Status: &claimed}); err != nil {
		t.Fatal(err)
	}
	opts := build(b.ID)
	opts.Target = a
	opts.Vars = []string{"base_branch=ghost/base"}
	_, err = DoSling(opts, deps, deps.Store)
	beadID := b.ID
	if opts.IsFormula {
		beadID = ""
	}
	requireBaseBranchRefusal(t, err, "ghost/base", SlingFormulaRepoDir(beadID, deps, a))
	got, getErr := deps.Store.Get(b.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != "in_progress" || got.Assignee != "prior-actor" || got.Metadata["gc.routed_to"] != "" {
		t.Errorf("refused sling changed the claimed bead: status=%q assignee=%q routed_to=%q",
			got.Status, got.Assignee, got.Metadata["gc.routed_to"])
	}
}

// Every formula-backed way into DoSling gets the same refusal, and the
// reassigning ones get it before the bead is reopened and unassigned.
func TestDoSlingRefusesUnresolvableBaseBranchInEveryFormulaShape(t *testing.T) {
	mayor := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	withDefault := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("code-review")}
	for _, tt := range []struct {
		name  string
		agent config.Agent
		opts  func(beadID string) SlingOpts
	}{
		{"standalone formula", mayor, func(string) SlingOpts {
			return SlingOpts{IsFormula: true, BeadOrFormula: "code-review"}
		}},
		{"--on formula", mayor, func(id string) SlingOpts {
			return SlingOpts{BeadOrFormula: id, OnFormula: "code-review"}
		}},
		{"--on formula with --reassign", mayor, func(id string) SlingOpts {
			return SlingOpts{BeadOrFormula: id, OnFormula: "code-review", Reassign: true}
		}},
		{"default formula", withDefault, func(id string) SlingOpts {
			return SlingOpts{BeadOrFormula: id}
		}},
		{"default formula with --reassign", withDefault, func(id string) SlingOpts {
			return SlingOpts{BeadOrFormula: id, Reassign: true}
		}},
		{"--on formula dry run", mayor, func(id string) SlingOpts {
			return SlingOpts{BeadOrFormula: id, OnFormula: "code-review", DryRun: true}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) { ghostBaseSling(t, tt.agent, tt.opts) })
	}
}

// A plain bead sling has no molecule and so no base_branch to check, even when
// the caller passes the var and the resolver would call it missing.
func TestDoSlingPlainBeadIgnoresBaseBranch(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	var asked []string
	deps.Branches = refCheckResolver{missing: map[string]bool{"ghost/base": true}, asked: &asked}
	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	opts := SlingOpts{Target: config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, BeadOrFormula: b.ID, NoFormula: true, Vars: []string{"base_branch=ghost/base"}}
	if _, err := DoSling(opts, deps, deps.Store); err != nil {
		t.Fatalf("plain bead sling: %v", err)
	}
	if len(asked) != 0 {
		t.Errorf("resolver asked about %v for a plain bead sling, want no calls", asked)
	}
}

// A container sling fans the formula out over its children without going
// through DoSling per child, so the batch loop must refuse on its own, before
// any child is touched.
func TestDoSlingBatchRefusesUnresolvableBaseBranch(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Branches = refCheckResolver{missing: map[string]bool{"ghost/base": true}}
	convoy, err := deps.Store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	var children []beads.Bead
	for _, title := range []string{"one", "two"} {
		c, err := deps.Store.Create(beads.Bead{Title: title, Type: "task", ParentID: convoy.ID, Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, c)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	opts := SlingOpts{Target: a, BeadOrFormula: convoy.ID, OnFormula: "code-review", Vars: []string{"base_branch=ghost/base"}}
	_, err = DoSlingBatch(opts, deps, deps.Store)
	requireBaseBranchRefusal(t, err, "ghost/base", SlingFormulaRepoDir(children[0].ID, deps, a))
	for _, c := range children {
		got, getErr := deps.Store.Get(c.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Metadata["gc.routed_to"] != "" || got.Assignee != "" {
			t.Errorf("child %s touched by a refused batch sling: routed_to=%q assignee=%q", c.ID, got.Metadata["gc.routed_to"], got.Assignee)
		}
	}
}
