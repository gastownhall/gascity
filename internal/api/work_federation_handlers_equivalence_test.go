package api

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The T7e.2 equivalence table: every reader T7e.2 moves onto storeref's
// WorkFederation legs, run whole beside its frozen copy from main
// (work_federation_handlers_reference_test.go) on T7d's corpus (t7dShapes)
// under T7b.3's and T7e's rig variants, gate on and off, and for the status
// readers with and without a ctx-bound scoped clone, each side on a fresh
// fixture. A run must give the same response, read and write the same
// physical stores in the same order with the same split-strict verdict, and
// leave the same rows behind. The changes the owner decided are declared and
// checked as such (t7e2Declared):
//   - Q2: status drops suspended rigs before deduping shared stores; the
//     status readers are compared against main's with that one rule applied
//     (q2StatusWorkCounts), and main must agree with it but on the variants
//     T7b.3 declared for it;
//   - the sweep's reads are federated, where main read the raw stores strict:
//     a hand-off's log is compared against main's with the verdict of each
//     reassign List (the reads by the retired session's id) moved from strict
//     to federated (t7e2SweepDeclared), and nothing else moved.

// t7e2Variant reshapes a built fixture's rigs.
type t7e2Variant struct {
	name string
	set  func(t *testing.T, fs *fakeState, fx t7dFixture, city string)
	q2   bool // Q2's rule changes this variant's status legs
}

// t7e2Variants are T7b.3's rig variants (cold rigs, aliases, a cold rig named
// like the city), T7e's failure variants (a failing city or rig store), and a
// warm configured rig the epoch opened no store for. That rig is configured
// only, as buildStores leaves a rig it opened no store for out of
// BeadStores(): T7e's "rig without a store" maps it to a nil store, which
// main's GET /v0/beads dereferences.
func t7e2Variants() []t7e2Variant {
	var out []t7e2Variant
	for _, v := range t7bVariants {
		out = append(out, t7e2Variant{name: v.name, q2: v.q2, set: func(_ *testing.T, fs *fakeState, fx t7dFixture, city string) {
			v.set(fs, fx, city)
		}})
	}
	for _, name := range []string{"failing city", "failing rig"} {
		v := t7eVariants[name]
		out = append(out, t7e2Variant{name: name, set: func(t *testing.T, fs *fakeState, fx t7dFixture, _ string) {
			v(t, fx, fs)
		}})
	}
	return append(out, t7e2Variant{name: "configured rig with no store", set: func(_ *testing.T, fs *fakeState, _ t7dFixture, _ string) {
		fs.cfg.Rigs = append([]config.Rig{{Name: "ghost", Path: "/nonexistent/ghost"}}, fs.cfg.Rigs...)
	}})
}

// t7e2Op is one reader, frozen and live: each renders what it answered.
type t7e2Op struct {
	name string
	kind string // "list", "status", "snapshot", "health", "sweep": the log rules key on it
	// scoped hands the status readers a ctx-bound "clone" (the store itself,
	// so the clone's reads are the store's): the bd branch, the one
	// ScopedStoreLike answers true for.
	scoped bool
	// moved is the retired session whose reassign reads the sweep's legs
	// make federated: the declared verdict move (sweep ops only).
	moved string
	// prep seeds what the op reads, on each side's fixture, before its log
	// is reset.
	prep   func(t *testing.T, fx t7dFixture)
	frozen func(t *testing.T, fx t7dFixture) string
	live   func(t *testing.T, fx t7dFixture) string
	// oracle, when set, is what live must match on a variant the change is
	// declared for: main's reader with the declared change applied.
	oracle func(t *testing.T, fx t7dFixture) string
}

// t7e2List renders a GET /v0/beads response.
func t7e2List(out *ListOutput[beads.Bead], err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	ids := t7eIDsOf(out.Body.Items)
	return fmt.Sprintf("%v total=%d partial=%v errs=%v more=%v", ids, out.Body.Total, out.Body.Partial, out.Body.PartialErrors, out.Body.NextCursor != "")
}

// t7e2Counts renders a status work count.
func t7e2Counts(wc workCounts, errs []string) string {
	return fmt.Sprintf("%+v errs=%q", wc, errs)
}

// t7e2Snapshot renders a status session snapshot.
func t7e2Snapshot(snap statusSessionSnapshot) string {
	names := make([]string, 0, len(snap.bySessionName))
	for name, info := range snap.bySessionName {
		names = append(names, fmt.Sprintf("%s=%s/%s/%s", name, info.id, info.template, info.state))
	}
	sort.Strings(names)
	return fmt.Sprintf("%v templates=%d errs=%q", names, len(snap.byTemplate), snap.partialErrors)
}

// t7e2Health renders a store-health measurement.
func t7e2Health(h *StatusStoreHealth, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	if h == nil {
		return "<nil>"
	}
	return fmt.Sprintf("rows=%d measured=%v warning=%v", h.LiveRows, h.RowsMeasured, h.Warning)
}

// t7e2SessionSeed gives the epoch's sessions placement one session bead.
func t7e2SessionSeed(t *testing.T, fx t7dFixture) {
	t.Helper()
	sessions := fx.state.Stores().Sessions()
	if sessions == (beads.SessionStore{}) {
		return
	}
	if _, err := sessions.Create(beads.Bead{Title: "worker", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
		"session_name": "s-worker", "template": "worker", "agent_name": "worker",
	}}); err != nil {
		t.Fatalf("seeding a session bead: %v", err)
	}
}

// q2StatusWorkCounts is the declared-change oracle for the status work
// counts: main's statusWorkCounts with its queries from q2StatusWorkQueries
// (main's builder with Q2's rule applied). Every other line is main's.
func q2StatusWorkCounts(ctx context.Context, s *Server, cacheColdRigs map[string]bool) (workCounts, []string) {
	queries, excluded := q2StatusWorkQueries(s, cacheColdRigs)
	results := make([]statusWorkResult, len(queries))
	var wg sync.WaitGroup
	for i, query := range queries {
		wg.Add(1)
		go func(i int, query frozenWorkQuery) {
			defer wg.Done()
			results[i] = frozenStatusStoreWorkCountsFor(ctx, s.state, query.label, query.store, query.includeStored, query.includeReady)
		}(i, query)
	}
	wg.Wait()
	var wc workCounts
	var errs []string
	seenReady := make(map[string]bool)
	for _, r := range results {
		wc.Open += r.wc.Open
		wc.InProgress += r.wc.InProgress
		for _, id := range r.readyIDs {
			if seenReady[id] {
				continue
			}
			seenReady[id] = true
			wc.Ready++
		}
		errs = append(errs, r.errs...)
	}
	wc.SuspendedRigsExcluded = excluded
	return wc, errs
}

// t7e2Ops is every converted reader.
func t7e2Ops(city string) []t7e2Op {
	var ops []t7e2Op
	inputs := []BeadListInput{
		{All: true},
		{Status: "in_progress"},
		{All: true, Assignee: "tw-worker"},
		{All: true, Assignee: "sess-old", Limit: 1},
	}
	for _, rig := range []string{"", "alpha", "beta", city, "a0", "zz", "ghost", "missing"} {
		inputs = append(inputs, BeadListInput{Rig: rig})
	}
	for _, input := range inputs {
		ops = append(ops, t7e2Op{
			name: fmt.Sprintf("humaHandleBeadList %+v", input),
			kind: "list",
			frozen: func(t *testing.T, fx t7dFixture) string {
				in := input
				return t7e2List(frozenHumaHandleBeadList(t.Context(), New(fx.state), &in))
			},
			live: func(t *testing.T, fx t7dFixture) string {
				in := input
				return t7e2List(New(fx.state).humaHandleBeadList(t.Context(), &in))
			},
		})
	}
	for _, scoped := range []bool{false, true} {
		ops = append(ops,
			t7e2Op{
				name: fmt.Sprintf("statusWorkCounts scoped=%v", scoped), kind: "status", scoped: scoped,
				frozen: func(t *testing.T, fx t7dFixture) string {
					s := New(fx.state)
					return t7e2Counts(frozenStatusWorkCounts(t.Context(), s, s.loadCacheColdRigs()))
				},
				oracle: func(t *testing.T, fx t7dFixture) string {
					s := New(fx.state)
					return t7e2Counts(q2StatusWorkCounts(t.Context(), s, s.loadCacheColdRigs()))
				},
				live: func(t *testing.T, fx t7dFixture) string {
					s := New(fx.state)
					return t7e2Counts(s.statusWorkCounts(t.Context(), fx.state.Stores(), s.loadCacheColdRigs()))
				},
			},
			t7e2Op{
				name: fmt.Sprintf("statusSessionSnapshot scoped=%v", scoped), kind: "snapshot", scoped: scoped,
				prep: t7e2SessionSeed,
				frozen: func(t *testing.T, fx t7dFixture) string {
					return t7e2Snapshot(frozenStatusSessionSnapshot(t.Context(), New(fx.state)))
				},
				live: func(t *testing.T, fx t7dFixture) string {
					return t7e2Snapshot(New(fx.state).statusSessionSnapshot(t.Context(), fx.state.Stores()))
				},
			},
			t7e2Op{
				name: fmt.Sprintf("computeStoreHealth scoped=%v", scoped), kind: "health", scoped: scoped,
				frozen: func(t *testing.T, fx t7dFixture) string {
					return t7e2Health(frozenComputeStoreHealth(t.Context(), New(fx.state)))
				},
				live: func(t *testing.T, fx t7dFixture) string {
					return t7e2Health(New(fx.state).computeStoreHealth(t.Context()))
				},
			},
		)
	}
	for _, held := range []bool{false, true} {
		move, frozenMove := reassignOpenWorkAssignedToSession, frozenReassignOpenWorkAssignedToSession
		if held {
			move, frozenMove = reassignHeldOpenWorkAssignedToSession, frozenReassignHeldOpenWorkAssignedToSession
		}
		for _, old := range []string{"sess-old", "tw-worker"} {
			retired := []session.Info{{ID: old}}
			ops = append(ops, t7e2Op{
				name:  fmt.Sprintf("handOffRetiredNamedSessionState held=%v %s", held, old),
				kind:  "sweep",
				moved: old,
				frozen: func(t *testing.T, fx t7dFixture) string {
					return fmt.Sprint(frozenHandOffRetiredNamedSessionState(t.Context(), New(fx.state), fx.state.Stores().Sessions(), retired, "sess-new", frozenMove))
				},
				live: func(t *testing.T, fx t7dFixture) string {
					return fmt.Sprint(New(fx.state).handOffRetiredNamedSessionState(t.Context(), fx.state.Stores().Sessions(), retired, "sess-new", move))
				},
			})
		}
	}
	return ops
}

// t7e2Normalize is an op's log as the table compares it. The status work
// counts read their legs on concurrent goroutines, so their log is compared
// as a sorted multiset of whole entries; every other reader reads in one
// order, compared as it is.
func t7e2Normalize(kind string, log []string) []string {
	if kind == "status" {
		return slices.Sorted(slices.Values(log))
	}
	return log
}

// t7e2SweepDeclared is main's hand-off log with the sweep's declared verdict
// move applied: each List by the retired session's id (the reassign reads,
// which the sweep's legs make; the waits and binding reads list by label)
// reads federated instead of strict.
func t7e2SweepDeclared(log []string, old string) []string {
	out := make([]string, len(log))
	for i, entry := range log {
		if strings.Contains(entry, " List ") && strings.Contains(entry, " Assignee:"+old+" ") && strings.HasSuffix(entry, " [strict]") {
			entry = strings.TrimSuffix(entry, "[strict]") + "[federated]"
		}
		out[i] = entry
	}
	return out
}

// TestWorkFederationReadersMatchTheFrozenReaders is the equivalence table.
func TestWorkFederationReadersMatchTheFrozenReaders(t *testing.T) {
	q2Moved := map[string]int{}
	variants := t7e2Variants()
	// shapesRun counts the shape runs that executed: a -run filter that
	// selects some shapes cannot judge a declaration stale.
	shapesRun := 0
	defer func() {
		if shapesRun != 2*len(t7dShapes) {
			return
		}
		for _, v := range variants {
			if v.q2 && q2Moved[v.name] == 0 && !t.Failed() {
				t.Errorf("variant %q is declared for Q2 but Q2's rule changed no status count on any shape", v.name)
			}
		}
	}()
	for _, gate := range []bool{false, true} {
		t.Run(fmt.Sprintf("gate=%v", gate), func(t *testing.T) {
			if gate {
				t.Setenv(splittest.StrictEnv, "1")
			} else {
				t.Setenv(splittest.StrictEnv, "")
			}
			for _, shapeName := range slices.Sorted(func(yield func(string) bool) {
				for name := range t7dShapes {
					if !yield(name) {
						return
					}
				}
			}) {
				shape := t7dShapes[shapeName]
				t.Run(shapeName, func(t *testing.T) {
					shapesRun++
					templates := shape.fixtures(t)
					// One city path for every side: it is the store-health
					// walk's root.
					cityPath := t.TempDir()
					for _, variant := range variants {
						for _, op := range t7e2Ops(shape.cityName()) {
							build := func() t7dFixture {
								fx := templates()
								fs := t7bFakeState(t, fx)
								fs.cityPath = cityPath
								variant.set(t, fs, fx, shape.cityName())
								t7bSeed(t, fx)
								if op.scoped {
									fs.scopedStoreFn = func(_ context.Context, existing beads.Store) (beads.Store, error) { return existing, nil }
								}
								if op.prep != nil {
									op.prep(t, fx)
								}
								*fx.log = nil
								return fx
							}
							ffx, lfx := build(), build()
							want, got := op.frozen(t, ffx), op.live(t, lfx)
							if strings.Contains(want+got, "<unknown>") {
								t.Fatalf("%s / %s: a result names a store the fixture does not hold: %s / %s", variant.name, op.name, got, want)
							}
							if op.oracle != nil {
								ofx := build()
								declared := op.oracle(t, ofx)
								if declared != want {
									if !variant.q2 {
										t.Errorf("%s / %s: Q2's rule changes a variant it is not declared for:\n  q2     %s\n  frozen %s", variant.name, op.name, declared, want)
									}
									q2Moved[variant.name]++
								}
								want, ffx = declared, ofx
							}
							if got != want {
								t.Errorf("%s / %s:\n  live   %s\n  frozen %s", variant.name, op.name, got, want)
								continue
							}
							w, g := t7e2Normalize(op.kind, *ffx.log), t7e2Normalize(op.kind, *lfx.log)
							if op.kind == "sweep" {
								w = t7e2SweepDeclared(w, op.moved)
							}
							if shape.cached {
								w, g = t7dSortOpRuns(w), t7dSortOpRuns(g)
							}
							if !slices.Equal(w, g) {
								t.Errorf("%s / %s: the live reader's reads and writes differ from the frozen one's%s", variant.name, op.name, t7dFirstDiff(w, g))
								continue
							}
							if w, g := ffx.dump(t), lfx.dump(t); w != g {
								t.Errorf("%s / %s: the live reader left\n%s\nthe frozen one\n%s", variant.name, op.name, g, w)
							}
						}
					}
				})
			}
		})
	}
}

// t7e2CountingStore logs each Count with the leg role its stack declares, so
// a test can tell a count through a leg's strict reader from one through its
// federated reader: the equivalence table cannot, since its fixtures count
// in memory below the log.
type t7e2CountingStore struct {
	beads.Store
	mu    sync.Mutex
	roles []beads.LegRole
}

func (c *t7e2CountingStore) Count(_ context.Context, _ beads.ListQuery, _ ...string) (int, error) {
	pcs := make([]uintptr, 96)
	pcs = pcs[:runtime.Callers(2, pcs)]
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roles = append(c.roles, beads.LegRoleOnStack(pcs))
	return 1, nil
}

func (c *t7e2CountingStore) take() []beads.LegRole {
	c.mu.Lock()
	defer c.mu.Unlock()
	roles := c.roles
	c.roles = nil
	return roles
}

// TestWorkLegCountsReadStrict pins the Counter reads of GET /v0/beads'
// bounding, the status work counts and store health to the leg's strict
// reader (beads.ReaderAs on TypedLeg.Strict), as main's unmarked counts of
// the raw store were, so the split-strict gate stays as sensitive as it was.
// The leg's own reader is federated, and its Counter declares it: the
// control shows the pin tells the two apart.
func TestWorkLegCountsReadStrict(t *testing.T) {
	store := &t7e2CountingStore{Store: beads.NewMemStore()}
	leg := testWorkLeg(t, store)
	state := newFakeState(t)

	counter, ok := beads.ReaderAs[beads.Counter](leg.Reader)
	if !ok {
		t.Fatal("the leg's federated reader hands out no Counter")
	}
	if _, err := counter.Count(t.Context(), beads.ListQuery{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.take(), []beads.LegRole{beads.FederatedLegRole}) {
		t.Fatal("control: a count through the leg's federated reader does not declare the federated role")
	}
	for name, count := range map[string]func(){
		"beadListLegCount":       func() { beadListLegCount(t.Context(), leg, "", &BeadListInput{All: true}) },
		"statusStoredWorkCounts": func() { _, _ = statusStoredWorkCounts(t.Context(), state, leg) },
		"countBeadStoreRows":     func() { _, _ = countBeadStoreRows(t.Context(), state, leg) },
	} {
		count()
		roles := store.take()
		if len(roles) == 0 {
			t.Errorf("%s made no count", name)
		}
		for _, role := range roles {
			if role != beads.NoLegRole {
				t.Errorf("%s counted through a %v leg; want the strict reader, which declares none", name, role)
			}
		}
	}
}

// t7e2EpochStates are an epoch swapped after its first read: from a split
// city with rig alpha to one with neither the binding nor the rig. A reader
// that read Stores() twice would lose the graph leg or the rig.
func t7e2EpochStates(t *testing.T) (*t7dEpochSwapState, *beads.MemStore, *beads.MemStore) {
	t.Helper()
	mem := func() *beads.MemStore { return &beads.MemStore{HonorExplicitIDs: true} }
	city, binding, alpha := mem(), mem(), mem()
	for store, id := range map[*beads.MemStore]string{city: "tc-1", binding: "gcg-1", alpha: "al-1"} {
		if _, err := store.Create(beads.Bead{ID: id, Title: id, Type: "task", Assignee: "sess-old"}); err != nil {
			t.Fatal(err)
		}
	}
	before := newFakeState(t)
	before.cityName = t7dCity
	before.cityBeadStore, before.graphBeadStore = city, binding
	before.stores = map[string]beads.Store{"alpha": alpha}
	after := newFakeState(t)
	after.cityName = t7dCity
	after.cityBeadStore = city
	return &t7dEpochSwapState{fakeState: before, first: before.Stores(), later: after.Stores()}, alpha, binding
}

// TestBeadListReadsOneEpoch pins that GET /v0/beads reads Stores() once: the
// cache gate, the assignee's sessions store and every leg come from one
// epoch.
func TestBeadListReadsOneEpoch(t *testing.T) {
	t7dGateOff(t)
	state, _, _ := t7e2EpochStates(t)
	out, err := New(state).humaHandleBeadList(t.Context(), &BeadListInput{Assignee: "sess-old"})
	if err != nil {
		t.Fatal(err)
	}
	if got := t7eIDsOf(out.Body.Items); len(got) != 3 || state.calls != 1 {
		t.Errorf("the list over a swapped epoch served %v after %d Stores() reads; want the city's, alpha's and the binding's rows after one", got, state.calls)
	}
}

// TestStatusWorkCountsReadTheRequestsEpoch pins that a status build counts
// work over the epoch it read first: the session snapshot's, not one read
// again for the counts.
func TestStatusWorkCountsReadTheRequestsEpoch(t *testing.T) {
	t7dGateOff(t)
	state, _, _ := t7e2EpochStates(t)
	body := New(state).buildStatusBody(t.Context(), false)
	if body.Work.Open != 2 {
		t.Errorf("status over a swapped epoch counted %d open items; want 2, the city's and alpha's (the graph binding is a ready leg only)", body.Work.Open)
	}
}

// TestRetiredSessionHandOffReadsOneEpoch pins that the hand-off plans its
// sweep and takes the messaging store from one Stores() read (t7b3-design.md
// Q7): alpha's and the binding's work move, as on the epoch it read first.
func TestRetiredSessionHandOffReadsOneEpoch(t *testing.T) {
	t7dGateOff(t)
	state, alpha, binding := t7e2EpochStates(t)
	sessions := state.first.Sessions()
	err := New(state).handOffRetiredNamedSessionState(t.Context(), sessions, []session.Info{{ID: "sess-old"}}, "sess-new", reassignOpenWorkAssignedToSession)
	if err != nil {
		t.Fatal(err)
	}
	for store, id := range map[*beads.MemStore]string{alpha: "al-1", binding: "gcg-1"} {
		if b, err := store.Get(id); err != nil || b.Assignee != "sess-new" {
			t.Errorf("%s after the hand-off: %+v, %v; want it reassigned to sess-new", id, b.Assignee, err)
		}
	}
	if state.calls != 1 {
		t.Errorf("the hand-off read Stores() %d times; want once", state.calls)
	}
}
