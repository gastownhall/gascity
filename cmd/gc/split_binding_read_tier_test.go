package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The wisp tier on a split city's binding reads (read-audit R5).
//
// A work store's bead-policy layer widens a zero-tier read to both tiers, so on
// a single-store city every class read the API makes (/status, /agents,
// /bead/{id}/deps, the dashboard tiles) has always seen ephemeral wisp rows. A
// split city hands those reads the binding instead, with no policy layer, so
// the same reads lost the ephemeral tier: agents running wisp steps read as not
// running, and a wisp root's deps came back empty. These tests drive the REAL
// controllerState — whose routes put the controller's CachingStore over a real
// SQLite binding — through the real API handler stack.

// readTierControllerState builds the production api.State over a split city:
// work on a policy-wrapped in-memory ledger, every infrastructure class on one
// real SQLite binding, routed through newControllerStateWithRoutes exactly as
// the controller builds it, with one singleton agent bd.dog. It returns the
// controller's handle on the binding.
func readTierControllerState(t *testing.T) (*controllerState, beads.Store) {
	t.Helper()
	binding := newSplitEnvClassLeaf(t)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "tier-city", Prefix: "gc"},
		Agents:    []config.Agent{{Name: "dog", BindingName: "bd", MaxActiveSessions: intPtr(1)}},
	}
	work := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)
	stubControllerCityStore(t, work)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs := newControllerStateWithRoutes(ctx, splitEnvRoutes(binding), cfg, runtime.NewFake(), nil, "tier-city", t.TempDir())
	// Writes go through the controller's own handle (the CachingStore over the
	// binding), as the controller's writes do, so the cache holds them.
	return cs, cs.GraphBeadStore().Store
}

// seedEphemeralWispStep writes an in-progress EPHEMERAL graph step into the
// binding, assigned in the "<sessionName>-gcg-session-<uuid>" shape a relocated
// graph store carries for live agent work, as a child of parent when one is
// given.
func seedEphemeralWispStep(t *testing.T, binding beads.Store, sessionName, parent string) beads.Bead {
	t.Helper()
	b, err := binding.Create(beads.Bead{Type: "task", Title: "wisp step", Ephemeral: true, ParentID: parent})
	if err != nil {
		t.Fatalf("seeding the wisp step: %v", err)
	}
	status := "in_progress"
	assignee := sessionName + "-gcg-session-917c4dd26bd772e2463919029a889e0e"
	if err := binding.Update(b.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("starting the wisp step: %v", err)
	}
	got, err := binding.Get(b.ID)
	if err != nil || !got.Ephemeral {
		t.Fatalf("fixture: wisp step %s = %+v, %v; want an ephemeral row", b.ID, got, err)
	}
	return got
}

func serveTierAPI(t *testing.T, cs *controllerState, path string, out any) {
	t.Helper()
	mux := api.NewSupervisorMux(&singleCityStateResolver{state: cs}, nil, false, "test", "", time.Now()).WithAnyHostAllowed()
	req := httptest.NewRequest(http.MethodGet, "/v0/city/"+cs.CityName()+path, nil)
	rec := httptest.NewRecorder()
	mux.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d (body=%q)", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decoding GET %s: %v (body=%q)", path, err, rec.Body.String())
	}
}

// TestSplitBindingZeroTierReadsSeeTheWispTier is the store-level half: every
// zero-tier read the controller's binding handle answers — the shape every API
// and controller reader of a class uses — includes the ephemeral tier, as the
// same read against a policy-wrapped work store always has. An explicit tier is
// still answered as asked.
func TestSplitBindingZeroTierReadsSeeTheWispTier(t *testing.T) {
	cs, binding := readTierControllerState(t)
	step := seedEphemeralWispStep(t, binding, "bd__dog", "")
	durable, err := binding.Create(beads.Bead{Type: "task", Title: "durable"})
	if err != nil {
		t.Fatal(err)
	}

	graph := cs.GraphBeadStore().Store
	if graph == cs.CityBeadStore() {
		t.Fatal("fixture: the graph class is not relocated")
	}
	ids := func(rows []beads.Bead) []string {
		out := make([]string, 0, len(rows))
		for _, b := range rows {
			out = append(out, b.ID)
		}
		return out
	}
	inProgress, err := graph.List(beads.ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids(inProgress), step.ID) {
		t.Errorf("binding List(status=in_progress) = %v, missing the ephemeral step %s", ids(inProgress), step.ID)
	}
	if counter, ok := graph.(beads.Counter); ok {
		if n, err := counter.Count(context.Background(), beads.ListQuery{Status: "in_progress"}); err != nil || n != 1 {
			t.Errorf("binding Count(status=in_progress) = %d, %v; want 1 (the ephemeral step)", n, err)
		}
	}
	sessions := cs.SessionsBeadStore().Store
	if rows, err := sessions.List(beads.ListQuery{Assignee: step.Assignee}); err != nil || !slices.Contains(ids(rows), step.ID) {
		t.Errorf("sessions-class handle List(assignee) = %v, %v; want the ephemeral step through the shared binding", ids(rows), err)
	}
	issues, err := graph.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierWisps})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ids(issues), durable.ID) || !slices.Contains(ids(issues), step.ID) {
		t.Errorf("explicit TierWisps read = %v; want the ephemeral step and not the durable %s", ids(issues), durable.ID)
	}
}

// TestSplitAPIReportsAnAgentRunningAWispStepAsRunning drives /status and
// /agents through the real handlers: an agent whose only live work is an
// ephemeral graph step in the binding is running, and that step is its active
// bead.
func TestSplitAPIReportsAnAgentRunningAWispStepAsRunning(t *testing.T) {
	cs, binding := readTierControllerState(t)
	step := seedEphemeralWispStep(t, binding, "bd__dog", "")

	var status struct {
		Agents struct {
			Running int `json:"running"`
		} `json:"agents"`
	}
	serveTierAPI(t, cs, "/status", &status)
	if status.Agents.Running != 1 {
		t.Errorf("/status agents.running = %d, want 1: bd.dog is running the ephemeral graph step %s", status.Agents.Running, step.ID)
	}

	var agents struct {
		Items []struct {
			Name       string `json:"name"`
			ActiveBead string `json:"active_bead"`
		} `json:"items"`
	}
	serveTierAPI(t, cs, "/agents", &agents)
	found := false
	for _, a := range agents.Items {
		if a.Name != "bd.dog" {
			continue
		}
		found = true
		if a.ActiveBead != step.ID {
			t.Errorf("/agents bd.dog active_bead = %q, want the ephemeral step %s", a.ActiveBead, step.ID)
		}
	}
	if !found {
		t.Fatalf("/agents lists no bd.dog: %+v", agents.Items)
	}
}

// TestSplitAPIDepsOfAWispRootListItsEphemeralChildren drives
// /bead/{id}/deps: a wisp root in the binding lists its ephemeral steps, so a
// molecule mid-execution does not read as empty.
func TestSplitAPIDepsOfAWispRootListItsEphemeralChildren(t *testing.T) {
	cs, binding := readTierControllerState(t)
	root, err := binding.Create(beads.Bead{Type: "task", Title: "wisp root", Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	step := seedEphemeralWispStep(t, binding, "bd__dog", root.ID)

	var deps struct {
		Children []beads.Bead `json:"children"`
	}
	serveTierAPI(t, cs, "/bead/"+root.ID+"/deps", &deps)
	if !slices.ContainsFunc(deps.Children, func(b beads.Bead) bool { return b.ID == step.ID }) {
		t.Errorf("/bead/%s/deps children = %+v, missing its ephemeral step %s", root.ID, deps.Children, step.ID)
	}
}
