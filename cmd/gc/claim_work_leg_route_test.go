package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	// unreadable fails every read and claim against a dir with its error.
	unreadable map[string]error
	// calls is each op the fixture served, as "<op> <dir> <leg>", leg being
	// the GC_TEST_LEG its env carried.
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
	newStore := func() *beads.MemStore {
		store := beads.NewMemStore()
		store.HonorExplicitIDs = true
		return store
	}
	f.stores[f.cityPath] = newStore()
	for _, name := range rigs {
		path := filepath.Join(f.cityPath, "rigs", name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		f.cfg.Rigs = append(f.cfg.Rigs, config.Rig{Name: name, Path: path})
		f.stores[path] = newStore()
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
	if err := f.unreadable[filepath.Clean(dir)]; err != nil {
		return nil, beads.Bead{}, err
	}
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

// storeAt is the store a bd context rooted in dir reaches: the leg's store,
// or an empty one for a dir that roots no leg.
func (f *workLegFixture) storeAt(dir string) *beads.MemStore {
	if store := f.stores[filepath.Clean(dir)]; store != nil {
		return store
	}
	return beads.NewMemStore()
}

// record notes one op served against dir with env.
func (f *workLegFixture) record(op, dir string, env []string) {
	leg := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GC_TEST_LEG="); ok {
			leg = v
		}
	}
	f.calls = append(f.calls, op+" "+dir+" "+leg)
}

// ops are claim ops served by the fixture's stores, by dir.
func (f *workLegFixture) ops() hookClaimOps {
	return hookClaimOps{
		Claim: func(_ context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
			f.record("claim", dir, env)
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
		ReadWorkMeta: func(_ context.Context, dir string, env []string, beadID, _ string) (beads.Bead, error) {
			f.record("read", dir, env)
			_, b, err := f.get(dir, beadID)
			return b, err
		},
		StampWorkMeta: func(_ context.Context, dir string, env []string, beadID, _ string, patch map[string]string) error {
			f.record("stamp", dir, env)
			store, _, err := f.get(dir, beadID)
			if err != nil {
				return err
			}
			return store.Update(beadID, beads.UpdateOpts{Metadata: patch})
		},
		Release: func(_ context.Context, dir string, env []string, beadID, assignee string) (bool, error) {
			f.record("release", dir, env)
			store, b, err := f.get(dir, beadID)
			if err != nil || b.Assignee != assignee {
				return false, err
			}
			open, nobody := "open", ""
			return true, store.Update(beadID, beads.UpdateOpts{Status: &open, Assignee: &nobody})
		},
		RestampAdopted: func(_ context.Context, dir string, env []string, beadID, from, to string) (bool, error) {
			f.record("restamp", dir, env)
			store, b, err := f.get(dir, beadID)
			if err != nil || b.Assignee != from {
				return false, err
			}
			return true, store.Update(beadID, beads.UpdateOpts{Assignee: &to})
		},
		EmitExecutionStepStarted: func(_ beads.Bead, dir string, env []string, _ string) {
			f.record("started", dir, env)
		},
		ListContinuation: func(_ context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
			f.record("list", dir, env)
			return f.storeAt(dir).List(beads.ListQuery{
				Status:   "open",
				Metadata: map[string]string{"gc.root_bead_id": rootID, "gc.continuation_group": group},
			})
		},
		AssignContinuation: func(_ context.Context, dir string, env []string, beadID, assignee string) error {
			f.record("assign", dir, env)
			store, _, err := f.get(dir, beadID)
			if err != nil {
				return err
			}
			return store.Update(beadID, beads.UpdateOpts{Assignee: &assignee})
		},
		InputVerdict: func(ctx context.Context, dir string, _ []string, candidate beads.Bead, identityCandidates []string, prove workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputDone(ctx, beads.WorkStore{Store: f.storeAt(dir)}, candidate, identityCandidates, prove)
		},
		WorkflowStore: func(_ context.Context, dir string, env []string, _, _ string) beads.Store {
			f.record("workflow", dir, env)
			return f.storeAt(dir)
		},
		StoreRef: func(dir string) string {
			return claimStoreRefAt(f.cityPath, f.cfg, dir)
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
	return f.claimWith(primary, workLegRoutedHookClaimOps(f.ops(), route, false), rows)
}

// claimWith is claim with the caller's composed ops.
func (f *workLegFixture) claimWith(primary hookStore, ops hookClaimOps, rows string) (int, hookClaimJSONResult, string) {
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
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("gc ready --json", primary.dir, primary.env, []hookStore{primary}, opts, ops, run, func(string, error) {}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		f.t.Fatalf("stdout is not JSON: %v\nraw: %q\nstderr: %q", err, stdout.String(), stderr.String())
	}
	return code, result, stderr.String()
}

// route is the work-leg route production builds for an agent whose primary
// leg is the named rig's, or the city's for "".
func (f *workLegFixture) route(primaryRig string) *hookClaimWorkLegRoute {
	identity := f.agent.Name
	if primaryRig != "" {
		identity = primaryRig + "/" + f.agent.Name
	}
	primary := f.primary(primaryRig)
	ref := primaryHookStoreRef(f.cityPath, f.cfg, identity, primary.dir, primary.env)
	return newHookClaimWorkLegRoute(f.cityPath, f.cfg, &f.agent, ref, nil)
}

// composed is the fixture's ops in the production composition
// (composeHookClaimOps): route under a class route over binding.
func (f *workLegFixture) composed(route *hookClaimWorkLegRoute, binding beads.Store) hookClaimOps {
	f.t.Helper()
	ops, proceed := composeHookClaimOps(f.ops(), route, newClaimRouteFor(f.t, binding), nil, nil, io.Discard)
	if !proceed {
		f.t.Fatal("composeHookClaimOps refused a class route that opened")
	}
	return ops
}

// mintCanceledRoot creates, under id in the named leg's store, a graph.v2
// workflow root closed gc.outcome=canceled.
func (f *workLegFixture) mintCanceledRoot(rig, id string) {
	f.t.Helper()
	store := f.stores[f.dirOf(rig)]
	if _, err := store.Create(beads.Bead{
		ID:       id,
		Title:    "canceled run",
		Type:     "molecule",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := store.CloseAll([]string{id}, map[string]string{"gc.outcome": "canceled"}); err != nil {
		f.t.Fatal(err)
	}
}

// mintStep creates, under id in the named leg's store, an open step of rootID
// routed to the agent, carrying extra metadata besides.
func (f *workLegFixture) mintStep(rig, id, rootID string, extra map[string]string) {
	f.t.Helper()
	meta := map[string]string{"gc.routed_to": f.agent.Name, "gc.root_bead_id": rootID}
	for k, v := range extra {
		meta[k] = v
	}
	if _, err := f.stores[f.dirOf(rig)].Create(beads.Bead{ID: id, Title: "workflow step", Type: "task", Metadata: meta}); err != nil {
		f.t.Fatal(err)
	}
}

// workflowRow is the reader's row for an open step of rootID routed to the
// fixture agent.
func (f *workLegFixture) workflowRow(beadID, rootID string) string {
	return fmt.Sprintf(`[{"id":%q,"status":"open","metadata":{"gc.routed_to":%q,"gc.root_bead_id":%q}}]`, beadID, f.agent.Name, rootID)
}

// callsIn is the ops the fixture served against the named leg's dir, in order.
func (f *workLegFixture) callsIn(rig string) []string {
	var ops []string
	for _, call := range f.calls {
		fields := strings.Fields(call)
		op, dir := fields[0], fields[1]
		if dir == f.dirOf(rig) {
			ops = append(ops, op)
		}
	}
	return ops
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

// TestHookClaimWorkLegRouteClaimsInTheLegThatHoldsIt pins AC-1 and AC-2: on
// the primary-scoped branch the federated reader serves an agent a bead only
// another work leg holds, and the claim lands in that leg's store instead of
// draining claims_errored on the primary's not-found. The primary has already
// answered, so the route never reads it again.
func TestHookClaimWorkLegRouteClaimsInTheLegThatHoldsIt(t *testing.T) {
	t.Run("city agent, rig bead", func(t *testing.T) {
		f := newWorkLegFixture(t, "alpha")
		bead := f.mint("alpha")
		code, result, stderr := f.claim(f.primary(""), f.route(""), f.routedRow(bead))
		if code != 0 || result.Action != "work" || result.BeadID != bead {
			t.Fatalf("claim = exit %d %+v, want exit 0 action=work for %s; stderr=%q", code, result, bead, stderr)
		}
		f.assertClaimedIn("alpha", bead)
	})
	t.Run("rig agent, city bead", func(t *testing.T) {
		f := newWorkLegFixture(t, "alpha")
		f.agent = config.Agent{Name: "worker", Dir: "alpha"}
		bead := f.mint("")

		code, result, stderr := f.claim(f.primary("alpha"), f.route("alpha"), f.routedRow(bead))
		if code != 0 || result.Action != "work" || result.BeadID != bead {
			t.Fatalf("claim = exit %d %+v, want exit 0 action=work for %s; stderr=%q", code, result, bead, stderr)
		}
		f.assertClaimedIn("", bead)
	})
	t.Run("rig agent, other rig's bead", func(t *testing.T) {
		f := newWorkLegFixture(t, "alpha", "beta")
		f.agent = config.Agent{Name: "worker", Dir: "alpha"}
		bead := f.mint("beta")

		code, result, stderr := f.claim(f.primary("alpha"), f.route("alpha"), f.routedRow(bead))
		if code != 0 || result.Action != "work" || result.BeadID != bead {
			t.Fatalf("claim = exit %d %+v, want exit 0 action=work for %s; stderr=%q", code, result, bead, stderr)
		}
		f.assertClaimedIn("beta", bead)
		if got := f.callsIn("alpha"); len(got) != 1 || got[0] != "claim" {
			t.Fatalf("ops on the primary leg = %v, want only its own claim: the route must not probe the primary again", got)
		}
	})
}

// TestHookClaimWorkLegRouteFollowOnOpsUseTheHomeLeg pins AC-3: once the route
// has claimed a bead in a non-primary leg, every op that finishes or unwinds
// that claim runs with the home leg's dir and env, the work_started entry keys
// it by that leg as resident, and the primary leg is not written.
func TestHookClaimWorkLegRouteFollowOnOpsUseTheHomeLeg(t *testing.T) {
	ctx := context.Background()
	f := newWorkLegFixture(t, "alpha")
	bead := f.mint("alpha")
	primary := f.primary("")
	ops := workLegRoutedHookClaimOps(f.ops(), f.route(""), true)
	ops.ClassesRelocated = true

	if _, ok, err := ops.Claim(ctx, primary.dir, primary.env, bead, "worker"); !ok || err != nil {
		t.Fatalf("Claim = ok %v err %v, want the route to land it in rig alpha", ok, err)
	}
	f.calls = nil

	if err := ops.StampWorkMeta(ctx, primary.dir, primary.env, bead, "worker", map[string]string{"work_dir": "/seat"}); err != nil {
		t.Fatalf("StampWorkMeta: %v", err)
	}
	readback, err := ops.ReadWorkMeta(ctx, primary.dir, primary.env, bead, "worker")
	if err != nil || readback.Metadata["work_dir"] != "/seat" {
		t.Fatalf("ReadWorkMeta = %+v, %v; want the stamped bead from rig alpha", readback, err)
	}
	ops.EmitExecutionStepStarted(readback, primary.dir, primary.env, "worker")
	if restamped, err := ops.RestampAdopted(ctx, primary.dir, primary.env, bead, "worker", "worker-2"); !restamped || err != nil {
		t.Fatalf("RestampAdopted = %v, %v; want the adoption restamped in rig alpha", restamped, err)
	}
	if released, err := ops.Release(ctx, primary.dir, primary.env, bead, "worker-2"); !released || err != nil {
		t.Fatalf("Release = %v, %v; want the claim released in rig alpha", released, err)
	}

	want := []string{"stamp", "read", "started", "restamp", "release"}
	if len(f.calls) != len(want) {
		t.Fatalf("calls = %v, want %v, each in rig alpha", f.calls, want)
	}
	for i, call := range f.calls {
		fields := strings.Fields(call)
		if fields[0] != want[i] || fields[1] != f.dirOf("alpha") || fields[2] != "leg-alpha" {
			t.Fatalf("call %d = %q, want %s in %s with rig alpha's env", i, call, want[i], f.dirOf("alpha"))
		}
	}
	if b, _ := f.stores[f.dirOf("alpha")].Get(bead); b.Status != "open" || b.Assignee != "" {
		t.Fatalf("rig alpha's bead after release = %q/%q, want open and unassigned", b.Status, b.Assignee)
	}

	for _, verdict := range []hookAdoptionVerdict{hookClaimMinted, hookAdoptionUnverified} {
		key := hookStartedKeyFor(ops, primary.dir, bead, verdict)
		if key.Store != "rig:alpha" || key.Bead != bead || !key.Resident {
			t.Fatalf("hookStartedKeyFor(verdict %d) = %+v, want rig:alpha %s resident", verdict, key, bead)
		}
	}
}

// TestHookClaimWorkLegRoutePrimaryHitBuildsNoOtherLeg pins AC-4: a candidate
// the primary leg holds, or one in a relocated class's reserved namespace,
// costs no other work leg's env build or read; and a probe builds legs only up
// to the one that holds the bead.
func TestHookClaimWorkLegRoutePrimaryHitBuildsNoOtherLeg(t *testing.T) {
	ctx := context.Background()
	t.Run("primary holds it", func(t *testing.T) {
		f := newWorkLegFixture(t, "alpha", "beta")
		bead := f.mint("")

		code, result, stderr := f.claim(f.primary(""), f.route(""), f.routedRow(bead))
		if code != 0 || result.Action != "work" || result.BeadID != bead {
			t.Fatalf("claim = exit %d %+v, want exit 0 action=work for %s; stderr=%q", code, result, bead, stderr)
		}
		f.assertClaimedIn("", bead)
		if len(f.envBuilds) != 0 || len(f.callsIn("alpha")) != 0 || len(f.callsIn("beta")) != 0 {
			t.Fatalf("env builds %v, alpha ops %v, beta ops %v; want none: the primary answered", f.envBuilds, f.callsIn("alpha"), f.callsIn("beta"))
		}
	})
	t.Run("reserved namespace id reaches the class route alone", func(t *testing.T) {
		f := newWorkLegFixture(t, "alpha")
		binding := newClaimRouteClassStore(t)
		mintClaimRouteBead(t, binding, "gcg-100", nil)
		primary := f.primary("")
		ops := f.composed(f.route(""), binding)

		if _, ok, err := ops.Claim(ctx, primary.dir, primary.env, "gcg-100", "worker"); !ok || err != nil {
			t.Fatalf("Claim(gcg-100) = ok %v err %v, want the class route to land it in the binding", ok, err)
		}
		if held, err := binding.Get("gcg-100"); err != nil || held.Assignee != "worker" {
			t.Fatalf("binding holds gcg-100 as %+v (err %v), want it claimed by worker", held, err)
		}
		if len(f.envBuilds) != 0 || len(f.callsIn("alpha")) != 0 {
			t.Fatalf("env builds %v, alpha ops %v; want none for a reserved-namespace id", f.envBuilds, f.callsIn("alpha"))
		}
	})
	t.Run("a probe builds legs up to the holder", func(t *testing.T) {
		f := newWorkLegFixture(t, "alpha", "beta")
		bead := f.mint("alpha")

		code, result, stderr := f.claim(f.primary(""), f.route(""), f.routedRow(bead))
		if code != 0 || result.Action != "work" || result.BeadID != bead {
			t.Fatalf("claim = exit %d %+v, want exit 0 action=work for %s; stderr=%q", code, result, bead, stderr)
		}
		if len(f.envBuilds) != 1 || f.envBuilds[0] != "alpha" || len(f.callsIn("beta")) != 0 {
			t.Fatalf("env builds %v, beta ops %v; want only alpha built and beta untouched", f.envBuilds, f.callsIn("beta"))
		}
	})
}

// workLegRecordingBinding is a class binding that notes each read and claim
// it serves in the fixture's call log, so a row can order the binding's
// answers against the work legs' reads.
type workLegRecordingBinding struct {
	beads.Store
	f *workLegFixture
}

func (b workLegRecordingBinding) Get(id string) (beads.Bead, error) {
	b.f.calls = append(b.f.calls, "binding-read binding -")
	return b.Store.Get(id)
}

func (b workLegRecordingBinding) Claim(id, assignee string) (beads.Bead, bool, error) {
	b.f.calls = append(b.f.calls, "binding-claim binding -")
	claimer, ok := b.Store.(interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	})
	if !ok {
		return beads.Bead{}, false, fmt.Errorf("the recorded binding %T has no compare-and-swap claim", b.Store)
	}
	return claimer.Claim(id, assignee)
}

// TestHookClaimWorkLegRouteWorkLegsBeforeBinding pins AC-5 on the production
// composition (composeHookClaimOps, the work-leg route under the class route):
// a work-shaped id only the class binding holds is read, once, through every
// other serving work leg, each proving absence, before the class route
// consults the binding, and the claim lands in the binding as it does without
// the route; and an id a work leg and the binding both hold (a co-resident
// relic) is claimed in the work leg, the copy the reader served, with the
// binding untouched.
func TestHookClaimWorkLegRouteWorkLegsBeforeBinding(t *testing.T) {
	t.Run("only the binding holds it", func(t *testing.T) {
		f := newWorkLegFixture(t, "alpha", "beta")
		stored := newClaimRouteClassStore(t)
		mintClaimRouteBead(t, stored, "wk-77", map[string]string{"gc.routed_to": f.agent.Name})
		ops := f.composed(f.route(""), workLegRecordingBinding{Store: stored, f: f})

		code, result, stderr := f.claimWith(f.primary(""), ops, f.routedRow("wk-77"))
		if code != 0 || result.Action != "work" || result.BeadID != "wk-77" {
			t.Fatalf("claim = exit %d %+v, want exit 0 action=work for wk-77; stderr=%q", code, result, stderr)
		}
		if held, err := stored.Get("wk-77"); err != nil || held.Assignee != f.agent.Name {
			t.Fatalf("binding holds wk-77 as %+v (err %v), want it claimed by %s", held, err, f.agent.Name)
		}

		firstBinding := -1
		for i, call := range f.calls {
			fields := strings.Fields(call)
			switch {
			case strings.HasPrefix(fields[0], "binding-"):
				if firstBinding < 0 {
					firstBinding = i
				}
			case fields[1] == f.dirOf("alpha") || fields[1] == f.dirOf("beta"):
				if firstBinding >= 0 {
					t.Fatalf("calls = %v: work leg op %q after the binding was consulted", f.calls, call)
				}
			}
		}
		if firstBinding < 0 {
			t.Fatalf("calls = %v: the binding was never consulted", f.calls)
		}
		for _, rig := range []string{"alpha", "beta"} {
			if got := f.callsIn(rig); len(got) != 1 || got[0] != "read" {
				t.Fatalf("ops on rig %s = %v, want exactly one read proving absence", rig, got)
			}
		}
	})
	t.Run("a work leg and the binding both hold it", func(t *testing.T) {
		f := newWorkLegFixture(t, "alpha", "beta")
		stored := newClaimRouteClassStore(t)
		mintClaimRouteBead(t, stored, "wk-78", map[string]string{"gc.routed_to": f.agent.Name})
		f.mintAs("wk-78", "alpha")
		ops := f.composed(f.route(""), workLegRecordingBinding{Store: stored, f: f})

		code, result, stderr := f.claimWith(f.primary(""), ops, f.routedRow("wk-78"))
		if code != 0 || result.Action != "work" || result.BeadID != "wk-78" {
			t.Fatalf("claim = exit %d %+v, want exit 0 action=work for wk-78; stderr=%q", code, result, stderr)
		}
		f.assertClaimedIn("alpha", "wk-78")
		if held, err := stored.Get("wk-78"); err != nil || held.Status != "open" || held.Assignee != "" {
			t.Fatalf("binding holds wk-78 as %+v (err %v), want its copy untouched: the work leg holds the copy the reader served", held, err)
		}
		for _, call := range f.calls {
			if strings.HasPrefix(call, "binding-claim") {
				t.Fatalf("calls = %v: the class route claimed in the binding an id a work leg holds", f.calls)
			}
		}
	})
}

// mintAs creates an open bead routed to the agent in each named leg's store,
// all under one id, the way a co-resident copy exists in more than one leg.
func (f *workLegFixture) mintAs(id string, rigs ...string) {
	f.t.Helper()
	for _, rig := range rigs {
		if _, err := f.stores[f.dirOf(rig)].Create(beads.Bead{
			ID:       id,
			Title:    "routed work",
			Type:     "task",
			Metadata: map[string]string{"gc.routed_to": f.agent.Name},
		}); err != nil {
			f.t.Fatal(err)
		}
	}
}

// TestHookClaimWorkLegRouteFirstHolderInReaderOrder pins AC-6: an id more than
// one non-primary work leg holds is claimed in the first holder in `gc
// ready`'s leg order — the city work store, then the serving rigs by name
// ascending, whatever the config order, a rig named like the city left out as
// the reader leaves it out — which is the copy the reader served.
func TestHookClaimWorkLegRouteFirstHolderInReaderOrder(t *testing.T) {
	for _, tt := range []struct {
		name    string
		rigs    []string
		primary string
		holders []string
		want    string
	}{
		{name: "city before rigs", rigs: []string{"alpha", "mid"}, primary: "mid", holders: []string{"alpha", ""}, want: ""},
		{name: "rigs by name, not config order", rigs: []string{"zeta", "alpha"}, primary: "", holders: []string{"zeta", "alpha"}, want: "alpha"},
		{name: "a rig named like the city is not a leg", rigs: []string{"town", "zulu"}, primary: "", holders: []string{"town", "zulu"}, want: "zulu"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newWorkLegFixture(t, tt.rigs...)
			if tt.primary != "" {
				f.agent = config.Agent{Name: "worker", Dir: tt.primary}
			}
			f.mintAs("wk-1", tt.holders...)

			code, result, stderr := f.claim(f.primary(tt.primary), f.route(tt.primary), f.routedRow("wk-1"))
			if code != 0 || result.Action != "work" || result.BeadID != "wk-1" {
				t.Fatalf("claim = exit %d %+v, want exit 0 action=work for wk-1; stderr=%q", code, result, stderr)
			}
			b, err := f.stores[f.dirOf(tt.want)].Get("wk-1")
			if err != nil || b.Status != "in_progress" || b.Assignee != f.agent.Name {
				t.Fatalf("leg %q copy = %+v (err %v), want it claimed by %s", tt.want, b, err, f.agent.Name)
			}
			for _, other := range tt.holders {
				if other == tt.want {
					continue
				}
				if b, _ := f.stores[f.dirOf(other)].Get("wk-1"); b.Status != "open" || b.Assignee != "" {
					t.Fatalf("leg %q copy = %q/%q, want it untouched", other, b.Status, b.Assignee)
				}
			}
		})
	}
}

// TestHookClaimWorkLegRouteUnreadableLegSkipsWithoutWriting pins AC-7: when a
// work leg the probe reaches cannot be read, no store is written for that
// candidate — not even a later leg that holds a copy, since the unread leg may
// hold the copy the reader served — the class route does not escalate, stderr
// names the leg and the read error, and the hook goes on to the next
// candidate on either tier, draining claims_errored if nothing is claimed.
func TestHookClaimWorkLegRouteUnreadableLegSkipsWithoutWriting(t *testing.T) {
	readErr := errors.New("bd show: database is locked")
	for _, tier := range []struct {
		name string
		row  func(id string, priority int) string
	}{
		{name: "routed", row: func(id string, priority int) string {
			return fmt.Sprintf(`{"id":%q,"status":"open","priority":%d,"metadata":{"gc.routed_to":"worker"}}`, id, priority)
		}},
		{name: "assigned", row: func(id string, priority int) string {
			return fmt.Sprintf(`{"id":%q,"status":"open","assignee":"worker","priority":%d}`, id, priority)
		}},
	} {
		for _, next := range []bool{false, true} {
			name := tier.name + ", alone"
			if next {
				name = tier.name + ", then a claimable candidate"
			}
			t.Run(name, func(t *testing.T) {
				f := newWorkLegFixture(t, "alpha", "beta")
				f.unreadable = map[string]error{f.dirOf("alpha"): readErr}
				f.mintAs("wk-1", "beta")
				rows := "[" + tier.row("wk-1", 0)
				if next {
					f.mintAs("wk-2", "")
					rows += "," + tier.row("wk-2", 1)
				}
				rows += "]"
				stored := newClaimRouteClassStore(t)
				ops := f.composed(f.route(""), workLegRecordingBinding{Store: stored, f: f})

				code, result, stderr := f.claimWith(f.primary(""), ops, rows)
				switch {
				case code != 0:
					t.Fatalf("claim exit = %d, want 0 (skip, not fail); stderr=%q", code, stderr)
				case next && (result.Action != "work" || result.BeadID != "wk-2"):
					t.Fatalf("claim = %+v, want action=work for wk-2, the next candidate; stderr=%q", result, stderr)
				case !next && (result.Action != "drain" || result.Reason != "claims_errored"):
					t.Fatalf("claim = %+v, want drain claims_errored; stderr=%q", result, stderr)
				}
				if !strings.Contains(stderr, "wk-1") || !strings.Contains(stderr, "rig:alpha") || !strings.Contains(stderr, readErr.Error()) {
					t.Fatalf("stderr = %q, want it to name wk-1, leg rig:alpha and the read error", stderr)
				}
				if b, _ := f.stores[f.dirOf("beta")].Get("wk-1"); b.Status != "open" || b.Assignee != "" {
					t.Fatalf("rig beta's wk-1 = %q/%q, want it untouched while rig alpha is unread", b.Status, b.Assignee)
				}
				for _, call := range f.calls {
					if strings.HasPrefix(call, "binding-claim") {
						t.Fatalf("calls = %v: the class route escalated an id a work leg could not be read for", f.calls)
					}
				}
			})
		}
	}
}

// TestHookClaimWorkLegRouteNotInstalledOffTheScopedBranch pins AC-8: only the
// primary-scoped branch carries a work-leg route. A single-store city and a
// split city whose agent sets a custom work_query keep the full fan-out and no
// route, and a nil route leaves every claim op the unwrapped value.
func TestHookClaimWorkLegRouteNotInstalledOffTheScopedBranch(t *testing.T) {
	cityDir := newReadyCityWithRigs(t)
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	resolveRigPaths(cityDir, cfg.Rigs)
	cityName := loadedCityName(cfg, cityDir)
	singleStoreTopo := config.QueryTopology{Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}}
	for _, tc := range []struct {
		name      string
		agent     config.Agent
		topo      config.QueryTopology
		wantRoute bool
	}{
		{name: "single-store city", agent: config.Agent{Name: "worker", Scope: "city"}, topo: singleStoreTopo},
		{name: "split city, custom work_query", agent: config.Agent{Name: "worker", Scope: "city", WorkQuery: "bd ready --json"}, topo: federatedHookTopology()},
		{name: "split city, generated work_query", agent: config.Agent{Name: "worker", Scope: "city"}, topo: federatedHookTopology(), wantRoute: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.agent
			overrides, err := hookQueryEnv(cityDir, cfg, &a)
			if err != nil {
				t.Fatalf("hookQueryEnv: %v", err)
			}
			workDir := agentCommandDir(cityDir, &a, cfg.Rigs)
			queryEnv := mergeRuntimeEnv(os.Environ(), overrides)
			workQuery := expandAgentCommandTemplate(cityDir, cityName, &a, cfg.Rigs, "work_query", a.EffectiveWorkQueryFor(tc.topo), io.Discard)
			singleStore := singleStoreHookWorkQuery(cityDir, cityName, cfg, &a, tc.topo, io.Discard)

			stores, route := hookClaimStores(cityDir, cfg, &a, "worker", workDir, queryEnv, overrides, workQuery, singleStore, func() map[string]bool { return nil })
			if got := route != nil; got != tc.wantRoute {
				t.Fatalf("work-leg route installed = %v, want %v", got, tc.wantRoute)
			}
			wantStores := len(hookWorkQueryStores(cityDir, cfg, &a, "worker", workDir, queryEnv, overrides, nil))
			if tc.wantRoute {
				wantStores = 1
			}
			if len(stores) != wantStores {
				t.Fatalf("claim stores = %d, want %d", len(stores), wantStores)
			}
			if wantStores < 2 && !tc.wantRoute {
				t.Fatalf("fixture has %d stores; the unscoped branch must fan out to every leg", wantStores)
			}
			if tc.wantRoute {
				return
			}

			ops := (&workLegFixture{t: t}).ops()
			for _, relocated := range []bool{false, true} {
				routed := workLegRoutedHookClaimOps(ops, route, relocated)
				if routed.WorkLegRoute != nil {
					t.Fatalf("relocated=%v: ops carry a work-leg route", relocated)
				}
				want, got := reflect.ValueOf(ops), reflect.ValueOf(routed)
				for i := 0; i < want.NumField(); i++ {
					if want.Field(i).Kind() != reflect.Func {
						continue
					}
					if want.Field(i).Pointer() != got.Field(i).Pointer() {
						t.Errorf("relocated=%v: op %s was wrapped", relocated, want.Type().Field(i).Name)
					}
				}
			}
		})
	}
}

// TestHookClaimWorkLegRouteStopGateReadsTheHomeLeg pins the workflow stop gate
// on a routed claim. A step only rig alpha holds, under a root alpha holds that
// was canceled, is closed canceled in alpha and never handed out, as it is when
// alpha is the agent's own primary leg. Before the claim lands the gate cannot
// know the step's home, so the routed claim takes the step and the post-claim
// re-check, reading the root in alpha, closes it; the hook then drains no_work.
// The class route over the work-leg route changes none of it.
func TestHookClaimWorkLegRouteStopGateReadsTheHomeLeg(t *testing.T) {
	workLegOnly := func(f *workLegFixture, primary string) hookClaimOps {
		return workLegRoutedHookClaimOps(f.ops(), f.route(primary), false)
	}
	underClassRoute := func(f *workLegFixture, primary string) hookClaimOps {
		return f.composed(f.route(primary), newClaimRouteClassStore(f.t))
	}
	for _, tc := range []struct {
		name    string
		primary string
		compose func(*workLegFixture, string) hookClaimOps
	}{
		{name: "claimed in its own leg", primary: "alpha", compose: workLegOnly},
		{name: "routed through the city primary", compose: workLegOnly},
		{name: "routed through the city primary, under the class route", compose: underClassRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkLegFixture(t, "alpha")
			if tc.primary != "" {
				f.agent = config.Agent{Name: "worker", Dir: tc.primary}
			}
			f.mintCanceledRoot("alpha", "wk-10")
			f.mintStep("alpha", "wk-11", "wk-10", nil)

			code, result, stderr := f.claimWith(f.primary(tc.primary), tc.compose(f, tc.primary), f.workflowRow("wk-11", "wk-10"))
			if code != 0 || result.Action != "drain" || result.Reason != "no_work" {
				t.Fatalf("claim = exit %d %+v, want exit 0 drain no_work: a step under a canceled root is never handed out; stderr=%q", code, result, stderr)
			}
			step, err := f.stores[f.dirOf("alpha")].Get("wk-11")
			if err != nil || step.Status != "closed" || step.Metadata["gc.outcome"] != "canceled" {
				t.Fatalf("rig alpha's wk-11 = %+v (err %v), want it closed gc.outcome=canceled so no later poll offers it", step, err)
			}
			if _, err := f.stores[f.cityPath].Get("wk-11"); err == nil {
				t.Fatal("the city store holds a copy of wk-11; the gate must not write a leg that lacked it")
			}
		})
	}
}

// TestHookClaimWorkLegRouteContinuationPinReadsTheHomeLeg pins the stop check
// of the continuation pin on a routed claim. The teardown step of a canceled
// root rig alpha holds is claimed through the city primary (teardown runs
// under a stopped root, so the gate lets it through), and its group sibling,
// ordinary work under the same root, stays unpinned: the pin reads the root in
// alpha, where the claim landed, not in the primary, which lacks it. The
// done-workflow check every candidate passes before its claim
// (retireDoneWorkflowCandidates) is what proves the root's home on a city that
// relocates a class, so the group is listed where it lives.
func TestHookClaimWorkLegRouteContinuationPinReadsTheHomeLeg(t *testing.T) {
	ctx := context.Background()
	f := newWorkLegFixture(t, "alpha")
	f.mintCanceledRoot("alpha", "wk-10")
	f.mintStep("alpha", "wk-11", "wk-10", map[string]string{"gc.continuation_group": "cleanup", "gc.scope_role": "teardown"})
	f.mintStep("alpha", "wk-12", "wk-10", map[string]string{"gc.continuation_group": "cleanup"})
	primary := f.primary("")
	ops := f.composed(f.route(""), newClaimRouteClassStore(t))
	opts := hookClaimOptions{
		Assignee:           f.agent.Name,
		IdentityCandidates: []string{f.agent.Name},
		RouteTargets:       []string{f.agent.Name},
		Env:                primary.env,
	}

	teardown, err := f.stores[f.dirOf("alpha")].Get("wk-11")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.InputVerdict(ctx, primary.dir, primary.env, teardown, opts.IdentityCandidates, nil); err != nil {
		t.Fatalf("InputVerdict(wk-11): %v", err)
	}
	claimed, ok, err := ops.Claim(ctx, primary.dir, primary.env, "wk-11", f.agent.Name)
	if !ok || err != nil {
		t.Fatalf("Claim(wk-11) = ok %v err %v, want the route to land it in rig alpha", ok, err)
	}
	f.calls = nil

	assigned, err := preassignHookContinuationGroup(claimed, opts, ops, primary.dir)
	if err != nil || len(assigned) != 0 {
		t.Fatalf("preassign = %v, %v; want no sibling pinned under the canceled root", assigned, err)
	}
	if got := f.callsIn("alpha"); len(got) == 0 || got[0] != "list" {
		t.Fatalf("ops on rig alpha = %v, want the group listed there first: a pin with no sibling to decide proves nothing", got)
	}
	if sibling, err := f.stores[f.dirOf("alpha")].Get("wk-12"); err != nil || sibling.Status != "open" || sibling.Assignee != "" {
		t.Fatalf("rig alpha's wk-12 = %+v (err %v), want it open and unpinned", sibling, err)
	}
}
