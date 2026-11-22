package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// workLegFixture is a split city's work axis for the work-leg route: the city
// work store and one store per serving rig, each reached by the dir its leg's
// bd context is rooted in. Its claim ops dispatch by that dir, so a claim run
// against the wrong leg's dir cannot resolve the bead, exactly as a bd
// subprocess rooted there cannot.
type workLegFixture struct {
	t        *testing.T
	cityPath string
	cfg      *config.City
	agent    config.Agent
	stores   map[string]*beads.MemStore
	// envBuilds is the agent dir of each leg env the route built ("" for the
	// city leg), in build order.
	envBuilds []string
	// calls is each op the fixture served, as "<op> <dir>".
	calls []string
}

// newWorkLegFixture is a city named "town" with the named rigs, in that config
// order, and a city-scoped agent named "worker".
func newWorkLegFixture(t *testing.T, rigs ...string) *workLegFixture {
	t.Helper()
	f := &workLegFixture{
		t:        t,
		cityPath: t.TempDir(),
		agent:    config.Agent{Name: "worker", Scope: "city"},
		stores:   map[string]*beads.MemStore{},
	}
	f.cfg = &config.City{Workspace: config.Workspace{Name: "town"}}
	f.stores[f.cityPath] = beads.NewMemStore()
	for _, name := range rigs {
		path := filepath.Join(f.cityPath, "rigs", name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		f.cfg.Rigs = append(f.cfg.Rigs, config.Rig{Name: name, Path: path})
		f.stores[path] = beads.NewMemStore()
	}
	prev := hookStoreQueryEnv
	hookStoreQueryEnv = func(_ string, _ *config.City, a *config.Agent) (map[string]string, error) {
		f.envBuilds = append(f.envBuilds, a.Dir)
		return map[string]string{"GC_TEST_LEG": "leg-" + a.Dir}, nil
	}
	t.Cleanup(func() { hookStoreQueryEnv = prev })
	return f
}

// dirOf is the dir of the named rig's leg, or of the city leg for "".
func (f *workLegFixture) dirOf(rig string) string {
	if rig == "" {
		return f.cityPath
	}
	return filepath.Join(f.cityPath, "rigs", rig)
}

// mint creates an open bead routed to the agent in the named leg's store.
func (f *workLegFixture) mint(rig string) string {
	f.t.Helper()
	b, err := f.stores[f.dirOf(rig)].Create(beads.Bead{
		Title:    "routed work",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": f.agent.Name},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return b.ID
}

// primary is the primary leg of the agent's claim: the leg its own
// work-query env reads.
func (f *workLegFixture) primary(rig string) hookStore {
	if rig == "" {
		return hookStore{dir: f.cityPath, env: []string{"GC_TEST_LEG=primary"}, storeRef: "city:town"}
	}
	return hookStore{dir: f.dirOf(rig), env: []string{"GC_TEST_LEG=primary"}, rig: rig, storeRef: "rig:" + rig}
}

// leg is the store a bd context rooted in dir reaches, or an error wrapping
// beads.ErrNotFound, as a bd subprocess reports an id its workspace lacks.
func (f *workLegFixture) get(dir, beadID string) (*beads.MemStore, beads.Bead, error) {
	store := f.stores[filepath.Clean(dir)]
	if store == nil {
		return nil, beads.Bead{}, fmt.Errorf("bead %q in %s: %w", beadID, dir, beads.ErrNotFound)
	}
	b, err := store.Get(beadID)
	if err != nil {
		return nil, beads.Bead{}, fmt.Errorf("bead %q in %s: %w", beadID, dir, err)
	}
	return store, b, nil
}

// ops are claim ops served by the fixture's stores, by dir.
func (f *workLegFixture) ops() hookClaimOps {
	return hookClaimOps{
		Claim: func(_ context.Context, dir string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			f.calls = append(f.calls, "claim "+dir)
			store, _, err := f.get(dir, beadID)
			if err != nil {
				return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", beadID, err)
			}
			inProgress := "in_progress"
			if err := store.Update(beadID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
				return beads.Bead{}, false, err
			}
			claimed, err := store.Get(beadID)
			return claimed, true, err
		},
		ReadWorkMeta: func(_ context.Context, dir string, _ []string, beadID, _ string) (beads.Bead, error) {
			f.calls = append(f.calls, "read "+dir)
			_, b, err := f.get(dir, beadID)
			return b, err
		},
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		DrainAck:          func(io.Writer) error { return nil },
	}
}

// claim runs one `gc hook --claim --json` tick over the primary leg alone,
// whose reader serves rows, with the route under test.
func (f *workLegFixture) claim(primary hookStore, route *hookClaimWorkLegRoute, rows string) (int, hookClaimJSONResult, string) {
	f.t.Helper()
	run := func(_, dir string, _ []string) (string, error) {
		if dir != primary.dir {
			f.t.Fatalf("work query ran against %q, want only the primary %q", dir, primary.dir)
		}
		return rows, nil
	}
	opts := hookClaimOptions{
		Assignee:           f.agent.Name,
		IdentityCandidates: []string{f.agent.Name},
		RouteTargets:       []string{f.agent.Name},
		DrainAck:           true,
		JSON:               true,
	}
	ops := workLegRoutedHookClaimOps(f.ops(), route)
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("gc ready --json", primary.dir, primary.env, []hookStore{primary}, opts, ops, run, func(string, error) {}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		f.t.Fatalf("stdout is not JSON: %v\nraw: %q\nstderr: %q", err, stdout.String(), stderr.String())
	}
	return code, result, stderr.String()
}

// routedRow is the reader's row for an open bead routed to the fixture agent.
func (f *workLegFixture) routedRow(beadID string) string {
	return fmt.Sprintf(`[{"id":%q,"status":"open","metadata":{"gc.routed_to":%q}}]`, beadID, f.agent.Name)
}

// assertClaimedIn asserts the bead is in_progress for the agent in the named
// leg's store, and that no other leg's store holds it.
func (f *workLegFixture) assertClaimedIn(rig, beadID string) {
	f.t.Helper()
	b, err := f.stores[f.dirOf(rig)].Get(beadID)
	if err != nil {
		f.t.Fatalf("leg %q lost bead %s: %v", rig, beadID, err)
	}
	if b.Status != "in_progress" || b.Assignee != f.agent.Name {
		f.t.Fatalf("bead %s in leg %q = status %q assignee %q, want in_progress for %q", beadID, rig, b.Status, b.Assignee, f.agent.Name)
	}
	for dir, store := range f.stores {
		if dir == f.dirOf(rig) {
			continue
		}
		if _, err := store.Get(beadID); err == nil {
			f.t.Fatalf("leg %s holds a copy of %s; the claim must not write a leg that lacked it", dir, beadID)
		}
	}
}

// TestHookClaimWorkLegRouteClaimsInTheLegThatHoldsIt pins AC-1: on the
// primary-scoped branch the federated reader serves a city-scoped agent a bead
// only a rig store holds, and the claim lands in that rig store instead of
// draining claims_errored on the primary's not-found.
func TestHookClaimWorkLegRouteClaimsInTheLegThatHoldsIt(t *testing.T) {
	t.Run("city agent, rig bead", func(t *testing.T) {
		f := newWorkLegFixture(t, "alpha")
		bead := f.mint("alpha")
		route := newHookClaimWorkLegRoute(f.cityPath, f.cfg, &f.agent, nil)

		code, result, stderr := f.claim(f.primary(""), route, f.routedRow(bead))
		if code != 0 || result.Action != "work" || result.BeadID != bead {
			t.Fatalf("claim = exit %d %+v, want exit 0 action=work for %s; stderr=%q", code, result, bead, stderr)
		}
		f.assertClaimedIn("alpha", bead)
	})
}
