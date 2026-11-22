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
		ops := classRoutedHookClaimOps(workLegRoutedHookClaimOps(f.ops(), f.route(""), true), newClaimRouteFor(t, binding))

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

// TestHookClaimWorkLegRouteWorkLegsBeforeBinding pins AC-5: a work-shaped id
// only the class binding holds is read, once, through every other serving
// work leg, each proving absence, before the class route consults the
// binding; and the claim lands in the binding as it does without the route.
func TestHookClaimWorkLegRouteWorkLegsBeforeBinding(t *testing.T) {
	f := newWorkLegFixture(t, "alpha", "beta")
	stored := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, stored, "wk-77", map[string]string{"gc.routed_to": f.agent.Name})
	primary := f.primary("")
	ops := classRoutedHookClaimOps(workLegRoutedHookClaimOps(f.ops(), f.route(""), true), newClaimRouteFor(t, workLegRecordingBinding{Store: stored, f: f}))

	code, result, stderr := f.claimWith(primary, ops, f.routedRow("wk-77"))
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
				ops := classRoutedHookClaimOps(workLegRoutedHookClaimOps(f.ops(), f.route(""), true), newClaimRouteFor(t, workLegRecordingBinding{Store: stored, f: f}))

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
