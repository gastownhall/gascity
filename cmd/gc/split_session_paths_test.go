package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Session paths on a split city (write-audit B5, B6, B8). Each one resolved,
// materialized or swept session rows through whatever store its caller held —
// a work store — while on a split city every session row lives only in the
// class binding. The fixtures relocate sessions to a real SQLite binding and
// keep work on a strict bd-semantics work store.

// sessionSplitRoutes stages a converged whole split over a real SQLite binding
// for cityPath, in the one-shot memo, and returns the binding.
func sessionSplitRoutes(t *testing.T, cityPath string) beads.Store {
	t.Helper()
	binding := newSplitEnvClassLeaf(t)
	seedCLIStorageRoutes(t, cityPath, splitEnvRoutes(binding))
	return binding
}

func countSessionBeads(t *testing.T, store beads.Store) int {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Label: session.LabelSession, IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	return len(rows)
}

// TestSplitRetryRecycleKillsTheSessionTheBindingHolds is B5: a pooled retry,
// ralph or retry-eval recycles its subject's session through the hook the
// dispatcher installs over the SCOPE store, which is a work store. The session
// row lives only in the binding, so the recycle could not resolve it and the
// retry pended or quarantined.
func TestSplitRetryRecycleKillsTheSessionTheBindingHolds(t *testing.T) {
	cityPath := t.TempDir()
	binding := sessionSplitRoutes(t, cityPath)
	work := splittest.NewWorkStore(t, "gc")
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	row, err := binding.Create(beads.Bead{
		Title:  "polecat-1",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "polecat-1",
			"state":        "awake",
			"template":     "polecat",
		},
	})
	if err != nil {
		t.Fatalf("seeding the session row in the binding: %v", err)
	}
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "polecat-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	prev := dispatchControlSessionProvider
	dispatchControlSessionProvider = func() (runtime.Provider, error) { return fake, nil }
	t.Cleanup(func() { dispatchControlSessionProvider = prev })

	recycle := recycleDispatchSubjectSession(cityPath, work, cfg)
	if err := recycle(beads.Bead{ID: "gc-1", Assignee: "polecat-1"}); err != nil {
		t.Fatalf("recycling the subject's session %s (held by the binding as %s): %v", "polecat-1", row.ID, err)
	}
	stopped := false
	for _, call := range fake.Calls {
		if call.Method == "Stop" && call.Name == "polecat-1" {
			stopped = true
		}
	}
	if !stopped {
		t.Errorf("the recycle did not stop polecat-1; calls = %+v", fake.Calls)
	}
	if n := countSessionBeads(t, work); n != 0 {
		t.Errorf("the work store holds %d session bead(s) after the recycle, want 0", n)
	}
	// The kill fences through the session ROW (its runtime lease) when it
	// resolves one; resolving nothing, it falls back to a runtime handle named
	// after the assignee and stops the runtime with no fence at all.
	after, err := binding.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Metadata["runtime_lease_epoch"] == "" {
		t.Errorf("the binding's session row %s carries no runtime lease after the recycle (metadata %v): the kill never resolved the row it fences through", row.ID, after.Metadata)
	}
}

// TestSplitDirectSessionResolverMaterializesInTheBindingNotTheCallerStore is
// B6 (and the sling half of it): a rig-scope drain or sling hands the resolver
// the RIG work store. Materializing a configured named session through it
// minted a duplicate gc:session bead there — invisible to every session reader,
// bound to a phantom that never wakes — instead of finding the canonical one in
// the binding.
func TestSplitDirectSessionResolverMaterializesInTheBindingNotTheCallerStore(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	cityPath := t.TempDir()
	binding := sessionSplitRoutes(t, cityPath)
	rig := splittest.NewWorkStore(t, "mr")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "refinery", Provider: "test-agent"}},
		NamedSessions: []config.NamedSession{{
			Template: "refinery",
		}},
		Providers: map[string]config.ProviderSpec{"test-agent": {DisplayName: "Test Agent"}},
	}

	id, ok, err := cliDirectSessionResolver(rig, "test-city", cityPath, cfg, "refinery", "")
	if err != nil || !ok {
		t.Fatalf("resolving the configured named session = %q, %v, %v", id, ok, err)
	}
	if n := countSessionBeads(t, rig); n != 0 {
		t.Fatalf("the rig work store holds %d session bead(s); the named session was materialized in the caller's store, not the sessions binding", n)
	}
	if _, err := binding.Get(id); err != nil {
		t.Fatalf("the resolved session %s is not in the binding: %v", id, err)
	}
	again, _, err := cliDirectSessionResolver(rig, "test-city", cityPath, cfg, "refinery", "")
	if err != nil || again != id {
		t.Fatalf("resolving again = %q, %v; want the canonical %s, not a second materialization", again, err, id)
	}
}

// TestSplitStartupNameClaimSweepReleasesClaimsInTheBinding is B8: the one-time
// startup sweep that releases stale runtime name claims held by closed
// configured named-session beads read cs.cityBeadStore, the work store. On a
// split city that is a silent no-op: the claims live in the binding.
func TestSplitStartupNameClaimSweepReleasesClaimsInTheBinding(t *testing.T) {
	binding := newSplitEnvClassLeaf(t)
	work := splittest.NewWorkStore(t, "gc")
	cs := newRoutedControllerStateForTest(t, splitEnvRoutes(binding), work)
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "refinery", BindingName: "gastown"}},
		NamedSessions: []config.NamedSession{{Template: "refinery", BindingName: "gastown"}},
	}
	runtimeName := config.NamedSessionRuntimeName("test-city", cfg.Workspace, "gastown.refinery")
	stale, err := binding.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": runtimeName,
			"alias":        "gastown.refinery",
			"template":     "gastown.refinery",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.Close(stale.ID); err != nil {
		t.Fatal(err)
	}

	released, err := releaseStaleNameClaimsAtStartup(cs, cfg, "test-city")
	if err != nil {
		t.Fatalf("startup name-claim sweep: %v", err)
	}
	if released != 1 {
		t.Errorf("released %d stale claim(s), want 1: the claim lives in the binding", released)
	}
	got, err := binding.Get(stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if name := got.Metadata["session_name"]; name != "" {
		t.Errorf("session_name = %q after the startup sweep, want released", name)
	}
}
