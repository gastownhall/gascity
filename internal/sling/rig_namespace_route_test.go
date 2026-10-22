package sling

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func rigNamespaceSlingFixture(t *testing.T, storeRef string) (SlingDeps, *fakeRunner) {
	t.Helper()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "hq"},
		Rigs: []config.Rig{
			{Name: "beta", Prefix: "bt", Path: t.TempDir(), Federation: config.RigFederationRig, Namespace: config.RigNamespaceExclusive},
			{Name: "alpha", Prefix: "al", Path: t.TempDir()},
		},
	}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "bt-1", Title: "work", Type: "task", Status: "open"},
		{ID: "al-1", Title: "work", Type: "task", Status: "open"},
	}, nil)
	deps.StoreRef = storeRef
	return deps, runner
}

func TestSlingRefusesRouteOutsideGuardedRig(t *testing.T) {
	city := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	custom := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), SlingQuery: "echo {}"}
	otherRig := config.Agent{Name: "crew.worker", Dir: "alpha", MaxActiveSessions: intPtr(1)}
	cases := []struct {
		name     string
		storeRef string
		target   config.Agent
		force    bool
		dryRun   bool
	}{
		{"store ref names the rig", "rig:beta", city, false, false},
		{"--force does not override", "rig:beta", city, true, false},
		{"dry run is refused too", "rig:beta", city, false, true},
		{"custom sling_query", "rig:beta", custom, true, false},
		{"bead prefix names the rig", "city:test", city, true, false},
		{"another rig's agent", "rig:beta", otherRig, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, runner := rigNamespaceSlingFixture(t, tc.storeRef)
			_, err := DoSling(SlingOpts{Target: tc.target, BeadOrFormula: "bt-1", Force: tc.force, DryRun: tc.dryRun, NoConvoy: true, NoFormula: true}, deps, deps.Store)
			if !errors.Is(err, agentutil.ErrRigNamespaceRoute) {
				t.Fatalf("DoSling err = %v, want a rig namespace refusal", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("routing commands ran: %v", runner.calls)
			}
			b, _ := deps.Store.Get("bt-1")
			if len(b.Metadata) != 0 || b.Assignee != "" {
				t.Fatalf("bead was written: %+v", b)
			}
		})
	}
}

func TestSlingAllowsRigOwnAgent(t *testing.T) {
	deps, runner := rigNamespaceSlingFixture(t, "rig:beta")
	own := config.Agent{Name: "crew.worker", Dir: "beta", MaxActiveSessions: intPtr(1)}
	if _, err := DoSling(SlingOpts{Target: own, BeadOrFormula: "bt-1", NoConvoy: true, NoFormula: true}, deps, deps.Store); err != nil {
		t.Fatalf("DoSling to the rig's own agent: %v", err)
	}
	if len(runner.calls) == 0 {
		t.Fatal("the routing command did not run")
	}
}

func TestSlingUnguardedRigUnchanged(t *testing.T) {
	deps, _ := rigNamespaceSlingFixture(t, "rig:alpha")
	city := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	if err := rigNamespaceRouteRefusal(deps, "al-1", city); err != nil {
		t.Fatalf("unguarded rig refused: %v", err)
	}
	deps.Cfg = nil
	if err := rigNamespaceRouteRefusal(deps, "bt-1", city); err != nil {
		t.Fatalf("nil config refused: %v", err)
	}
}

func TestSlingBatchRefusesRouteOutsideGuardedRig(t *testing.T) {
	city := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	// The container itself lives in the guarded rig: refused whole.
	deps, runner := rigNamespaceSlingFixture(t, "rig:beta")
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "bt-c", Title: "convoy", Type: "convoy", Status: "open"},
		{ID: "bt-2", Title: "child", Type: "task", Status: "open"},
	}, nil)
	if err := store.DepAdd("bt-c", "bt-2", "tracks"); err != nil {
		t.Fatal(err)
	}
	deps.Store = store
	if _, err := DoSlingBatch(SlingOpts{Target: city, BeadOrFormula: "bt-c", Force: true, NoFormula: true}, deps, store); !errors.Is(err, agentutil.ErrRigNamespaceRoute) {
		t.Fatalf("container in a guarded rig: err = %v, want a refusal", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("routing commands ran: %v", runner.calls)
	}

	// A city container tracking a guarded rig's bead: that child fails.
	deps, runner = rigNamespaceSlingFixture(t, "city:test")
	store = beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "hq-c", Title: "convoy", Type: "convoy", Status: "open"},
		{ID: "bt-2", Title: "child", Type: "task", Status: "open"},
	}, nil)
	if err := store.DepAdd("hq-c", "bt-2", "tracks"); err != nil {
		t.Fatal(err)
	}
	deps.Store = store
	result, _ := DoSlingBatch(SlingOpts{Target: city, BeadOrFormula: "hq-c", Force: true, NoFormula: true}, deps, store)
	if result.Routed != 0 || result.Failed != 1 {
		t.Fatalf("routed %d failed %d, want 0 and 1 (%+v)", result.Routed, result.Failed, result.Children)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("routing commands ran: %v", runner.calls)
	}
}
