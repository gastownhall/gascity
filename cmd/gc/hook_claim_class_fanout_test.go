package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// The claim-time class escalation as the FEDERATED loop sees it. The unit rows
// in claim_class_route_test.go own the per-call escalation rule; these rows own
// the two properties only the loop can state:
//
//   - "work first" means every WORK leg, not the first leg the ranking picked.
//     A rig-scoped agent — the shipped shape for every worker that claims — runs
//     its rig store FIRST and the city work store LAST, so an escalation that
//     fires on the leg that answered preempts an untried work leg that holds the
//     bead.
//   - a binding-side refusal is a fact about ONE bead, never about the tick. The
//     escalation only runs after a work store returned not-found, which proves
//     the session owns nothing there, so the claim can be skipped exactly as the
//     work-side not-found is.

// hookFanoutRigFirstStores is the fan-out hookWorkQueryStores builds for a
// rig-scoped agent: the rig store as the PRIMARY entry, the agent's own
// (rig-scoped) store, then the city work store appended LAST.
func hookFanoutRigFirstStores() []hookStore {
	return []hookStore{
		{dir: "rig-a", env: []string{"GC_STORE_SCOPE=rig"}},
		{dir: "city", env: []string{"GC_STORE_SCOPE=city"}},
	}
}

// hookFanoutClaimOpts is the worker identity every row below claims as.
func hookFanoutClaimOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}
}

func hookFanoutBaseOps(claim hookClaimFunc) hookClaimOps {
	return hookClaimOps{
		Claim:             claim,
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
		DrainAck:          func(io.Writer) error { return nil },
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			return nil
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, error) {
			return beads.Bead{ID: beadID, Assignee: assignee}, nil
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
		PublishRunMap:            func(string, string, ...string) error { return nil },
	}
}

// newFanoutClassRoute opens a claim-time class route over a store a row
// controls, bypassing hookClaimClassRouteForCity (which needs a city on disk).
func newFanoutClassRoute(t *testing.T, class beads.Store) *hookClaimClassRoute {
	t.Helper()
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	return route
}

// TestClassEscalationWaitsForEveryWorkLeg is the "work first" statement at the
// level the phrase is about.
//
// A co-resident id — the documented steady state of a migrated city, because
// `gc storage migrate` copies and never deletes back — is served by the
// federated reader from the CITY work row (readyFederationLegs runs city, then
// rigs, then graph, first-leg-wins). The claim must agree. On a rig-scoped agent
// the ranking selects the RIG leg first, its work-scope claim answers not-found,
// and an escalation that fires there takes the binding copy while the reader
// keeps re-serving the still-open city work row — the treadmill this slice
// exists to end, reintroduced by the slice itself.
func TestClassEscalationWaitsForEveryWorkLeg(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "gcg-77", nil)
	route := newFanoutClassRoute(t, class)

	// Both work legs are served the same co-resident row; only the CITY leg's
	// bd context can resolve it, exactly as appendCityHookStore documents.
	row := `[{"id":"gcg-77","status":"open","assignee":"worker-1","metadata":{"gc.kind":"wisp"}}]`
	run := func(_, _ string, _ []string) (string, error) { return row, nil }

	var claimDirs []string
	base := hookFanoutBaseOps(func(_ context.Context, dir string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
		claimDirs = append(claimDirs, dir)
		if dir != "city" {
			return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", beadID, beads.ErrNotFound)
		}
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
	})
	// Residence, read through each leg's own bd context: the CITY work store
	// holds the migration copy and the rig store does not.
	base.ReadWorkMeta = func(_ context.Context, dir string, _ []string, beadID, assignee string) (beads.Bead, error) {
		if dir != "city" {
			return beads.Bead{}, fmt.Errorf("getting bead %q: %w", beadID, beads.ErrNotFound)
		}
		return beads.Bead{ID: beadID, Assignee: assignee}, nil
	}

	stores := hookFanoutRigFirstStores()
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("gc ready --json", "city", stores[0].env, stores, hookFanoutClaimOpts(),
		classRoutedHookClaimOps(base, route), run, func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if got := strings.Join(claimDirs, ","); got != "rig-a,city" {
		t.Fatalf("work-scope claim ran against dirs %q, want rig-a,city: every WORK leg must answer before the class escalation opens", got)
	}
	if held, err := class.Get("gcg-77"); err != nil || strings.TrimSpace(held.Assignee) != "" {
		t.Fatalf("the binding's copy of gcg-77 is claimed for %q (err=%v); a co-resident id belongs to the WORK copy the reader that served it answers from", held.Assignee, err)
	}
}

// TestClassEscalationStillReachesABindingOnlyBead is the other side of the same
// rule and the slice's headline capability: once EVERY work leg has answered
// not-found, no work store holds the bead, and the binding is where it lives.
func TestClassEscalationStillReachesABindingOnlyBead(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "gcg-88", nil)
	route := newFanoutClassRoute(t, class)

	row := `[{"id":"gcg-88","status":"open","assignee":"worker-1","metadata":{"gc.kind":"wisp"}}]`
	run := func(_, _ string, _ []string) (string, error) { return row, nil }
	var claimDirs []string
	base := hookFanoutBaseOps(func(_ context.Context, dir string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
		claimDirs = append(claimDirs, dir)
		return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", beadID, beads.ErrNotFound)
	})
	base.ReadWorkMeta = func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
		return beads.Bead{}, fmt.Errorf("getting bead %q: %w", beadID, beads.ErrNotFound)
	}

	stores := hookFanoutRigFirstStores()
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("gc ready --json", "city", stores[0].env, stores, hookFanoutClaimOpts(),
		classRoutedHookClaimOps(base, route), run, func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %q", err, stdout.String())
	}
	if result.BeadID != "gcg-88" || !result.OK {
		t.Fatalf("claim result = %+v, want gcg-88 claimed: a bead NO work leg holds is exactly what the class escalation is for", result)
	}
	// And on the FIRST leg it was selected on. Deferring the escalation until
	// the fan-out drained would let unrelated lower-tier work in a later leg be
	// claimed ahead of an ASSIGNED graph step, which is the starvation this
	// slice closes.
	if got := strings.Join(claimDirs, ","); got != "rig-a" {
		t.Fatalf("work-scope claim ran against dirs %q, want rig-a alone: once no work leg holds the bead the escalation is immediate, at the candidate's own rank", got)
	}
	held, err := class.Get("gcg-88")
	if err != nil || !strings.EqualFold(strings.TrimSpace(held.Status), "in_progress") || strings.TrimSpace(held.Assignee) != "worker-1" {
		t.Fatalf("binding holds gcg-88 as status=%q assignee=%q (err=%v), want in_progress owned by worker-1", held.Status, held.Assignee, err)
	}
}

// noCASBindingLeaf is the production binding leaf that cannot CAS-claim:
// beadsworkspace's OpenEngine returns a *beads.NativeDoltStore, which carries
// ReleaseIfCurrent and no two-argument Claim. beads.MemStore HAS the capability,
// so it is hidden behind an embedding that does not re-export it.
type noCASBindingLeaf struct{ beads.Store }

// newCapabilityRefusingRoute builds a route whose binding answers reads but
// refuses the claim CAS, bypassing the constructor's capability check so the row
// states what happens if a refusal reaches the claim anyway.
func newCapabilityRefusingRoute(t *testing.T, ids ...string) *hookClaimClassRoute {
	t.Helper()
	leaf := newClaimRouteClassStore(t)
	for _, id := range ids {
		mintClaimRouteBead(t, leaf, id, nil)
	}
	hidden := noCASBindingLeaf{Store: leaf}
	graph, err := storebinding.NewBeadsGraphStore(hidden)
	if err != nil {
		t.Fatalf("projecting the graph front door over a claim-incapable leaf: %v", err)
	}
	return newClaimClassRouteOver(hidden, graph)
}

// TestBindingClaimRefusalIsPerBeadNotPerTick is the control-vs-refusal pair.
//
// The escalation runs only after a work store returned not-found, which PROVES
// this session owns nothing there and that nothing was mutated anywhere. A
// binding-side refusal on that bead is therefore a per-bead fact, exactly like
// the work-side not-found it followed — and must be skipped the same way, or one
// unclaimable id takes the whole tick down and the worker claims nothing at all,
// including healthy work in stores that answer perfectly.
func TestBindingClaimRefusalIsPerBeadNotPerTick(t *testing.T) {
	// The graph step ranks AHEAD of the routed work (assigned beats routed), so
	// the refusal is reached before the healthy bead either way.
	row := `[{"id":"gcg-6","status":"open","assignee":"worker-1","metadata":{"gc.kind":"wisp"}},` +
		`{"id":"gc-77","status":"open","metadata":{"gc.routed_to":"worker"}}]`
	run := func(_, _ string, _ []string) (string, error) { return row, nil } //nolint:unparam // the runner seam returns an error the healthy rows never produce
	newBase := func() hookClaimOps {
		return hookFanoutBaseOps(func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			if strings.HasPrefix(beadID, "gcg-") {
				return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", beadID, beads.ErrNotFound)
			}
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		})
	}

	for _, tt := range []struct {
		name  string
		ops   func(*testing.T) hookClaimOps
		stdin string
	}{
		{
			name: "control: no class route (main's behavior)",
			ops:  func(*testing.T) hookClaimOps { return newBase() },
		},
		{
			name: "class route whose binding refuses the claim CAS",
			ops: func(t *testing.T) hookClaimOps {
				return classRoutedHookClaimOps(newBase(), newCapabilityRefusingRoute(t, "gcg-6"))
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stores := []hookStore{{dir: "city", env: []string{"GC_STORE_SCOPE=city"}}}
			var stdout, stderr bytes.Buffer
			code := claimHookWorkWithRunner("gc ready --json", "city", stores[0].env, stores, hookFanoutClaimOpts(),
				tt.ops(t), run, func(string, error) {}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("claimHookWorkWithRunner = %d, want 0: a bead this session provably does not own in the work store cannot make the tick terminal; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("stdout is not JSON: %v\nraw: %q", err, stdout.String())
			}
			if result.BeadID != "gc-77" {
				t.Fatalf("claim result = %+v, want the healthy routed bead gc-77 claimed", result)
			}
		})
	}
}

// TestBindingClaimRefusalAloneStillDrains pins the drain contract for the same
// refusal with no other work behind it: a structured claims_errored drain a
// --json caller can read, not a bare exit 1 with empty stdout.
func TestBindingClaimRefusalAloneStillDrains(t *testing.T) {
	row := `[{"id":"gcg-6","status":"open","assignee":"worker-1","metadata":{"gc.kind":"wisp"}}]`
	run := func(_, _ string, _ []string) (string, error) { return row, nil }
	base := hookFanoutBaseOps(func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
		return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", beadID, beads.ErrNotFound)
	})
	drained := false
	base.DrainAck = func(io.Writer) error {
		drained = true
		return nil
	}

	stores := []hookStore{{dir: "city", env: []string{"GC_STORE_SCOPE=city"}}}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("gc ready --json", "city", stores[0].env, stores, hookFanoutClaimOpts(),
		classRoutedHookClaimOps(base, newCapabilityRefusingRoute(t, "gcg-6")), run, func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0 (structured drain); stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !drained {
		t.Fatal("drain ack was not called; the tick exited without completing the drain contract")
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %q", err, stdout.String())
	}
	if result.Reason != "claims_errored" {
		t.Fatalf("drain reason = %q, want claims_errored: a refused claim must not be laundered into a healthy no_work", result.Reason)
	}
}
