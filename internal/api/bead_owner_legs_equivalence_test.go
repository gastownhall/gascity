package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workrecord"
)

// The T7e equivalence table: every reader T7e typed, run beside its frozen
// copy (bead_owner_legs_reference_test.go) on T7d's corpus (t7dShapes) and
// T7e's rig variants (t7eVariants), gate on and off, each on a fresh fixture.
// A run must give the same result, read and write the same physical stores in
// the same order with the same split-strict verdict, and leave the same rows
// behind. T7e moves no verdict: the by-id readers read the owner
// resolveBeadOwner hands both sides, and the ready legs read the class handles
// over the stores BeadStores() returned, strictly, as the raw stores read.

// t7eVariant reshapes a built fixture's rigs.
type t7eVariant func(t *testing.T, fx t7dFixture, fs *fakeState)

// t7eVariants are the rig shapes the by-name walks and the identity matches
// tell apart.
var t7eVariants = map[string]t7eVariant{
	"": nil,
	// Legacy shared-file mode: a0 shares beta's store and sorts before every
	// rig, so the store-deduped names read beta's store under a0; zz is a rig
	// over the city store, sorting after the city.
	"aliased rigs": func(_ *testing.T, fx t7dFixture, fs *fakeState) {
		fs.stores["a0"] = fx.tops["beta"]
		fs.stores["zz"] = fx.tops["city"]
		fs.cfg.Rigs = append(fs.cfg.Rigs, config.Rig{Name: "zz", Path: "/nonexistent/zz"}, config.Rig{Name: "a0", Path: "/nonexistent/a0"})
	},
	// The city store's reads fail: the ready federation reports it under the
	// city leg's label and serves the rest.
	"failing city": func(_ *testing.T, fx t7dFixture, fs *fakeState) {
		failing := t7eFailingStore{&t7dRecordingStore{Store: fx.mems["city"], name: "city-failing", log: fx.log}}
		fx.tops["city-failing"] = failing
		fs.cityBeadStore = failing
	},
	// A rig whose reads fail: the ready federation reports it and serves the
	// rest.
	"failing rig": func(_ *testing.T, fx t7dFixture, fs *fakeState) {
		failing := t7eFailingStore{&t7dRecordingStore{Store: fx.mems["beta"], name: "beta-failing", log: fx.log}}
		fx.tops["beta-failing"] = failing
		fs.stores["beta"] = failing
	},
	// A configured rig the epoch opened no store for.
	"rig without a store": func(_ *testing.T, _ t7dFixture, fs *fakeState) {
		fs.stores["ghost"] = nil
		fs.cfg.Rigs = append([]config.Rig{{Name: "ghost", Path: "/nonexistent/ghost"}}, fs.cfg.Rigs...)
	},
}

// errT7eFailing is a failing rig's read error.
var errT7eFailing = errors.New("t7e: this rig's store is down")

// t7eFailingStore fails every list and ready read, and logs it.
type t7eFailingStore struct{ *t7dRecordingStore }

func (s t7eFailingStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.note("Ready", q)
	return nil, errT7eFailing
}

func (s t7eFailingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.note("List", q)
	return nil, errT7eFailing
}

// t7eFakeState is the fake under fx's state.
func t7eFakeState(t *testing.T, state State) *fakeState {
	t.Helper()
	switch s := state.(type) {
	case *fakeState:
		return s
	case relicReportingState:
		return s.fakeState
	}
	t.Fatalf("fixture state is %T", state)
	return nil
}

// t7eIDs are the by-id readers' ids: each run's root and a step, the twin,
// the co-resident ids, a work convoy, order history, and a miss.
func t7eIDs(home string) []string {
	return []string{home + "1-wf", home + "1-s2", "al9-wf", "al9-s1", "be5-wf", "tx7-wf", "tc3-wf", "tw-wf", "tw-s1", "al-cv1", "co-1", "co-2", "al-or1", "missing"}
}

// t7eShipped is a shipped work record's metadata.
var t7eShipped = map[string]string{
	beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
	beadmeta.WorkCommitMetadataKey:  "0000000000000000000000000000000000000000",
	beadmeta.WorkBranchMetadataKey:  "main",
}

// t7eRecord is a gated row for b: its id as a task with no kind, carrying
// b's gc.root_store_ref when owned, so the repo dir comes from the owner,
// else none, so it comes from the store that answered.
func t7eRecord(b beads.Bead, owned bool) beads.Bead {
	meta := map[string]string{}
	if owned {
		meta[beadmeta.RootStoreRefMetadataKey] = b.Metadata[beadmeta.RootStoreRefMetadataKey]
	}
	return beads.Bead{ID: b.ID, Type: "task", Metadata: meta}
}

// t7eGateCall runs one close-gate check with the reachability oracle stubbed
// to answer false and record each repository it was asked about, and renders
// the error, the dirs asked and the gate's log: a gate that asked the wrong
// checkout differs even where both answers are "not reachable".
func t7eGateCall(t *testing.T, gate func() error) string {
	t.Helper()
	var asked []string
	original := workRecordCommitReachable
	workRecordCommitReachable = func(_ context.Context, repoDir, _, _ string) bool {
		asked = append(asked, repoDir)
		return false
	}
	defer func() { workRecordCommitReachable = original }()
	logged := captureWorkRecordGateLog(t)
	err := gate()
	return fmt.Sprintf("%v | asked %q | %s", err, asked, logged)
}

// t7eGraph renders a bead graph with its beads and edges in id order: a
// CachingStore lists in map order.
func t7eGraph(graph []beads.Bead, edges []workflowDepResponse, membership beads.Membership, err error) string {
	ids := make([]string, 0, len(graph))
	for _, b := range graph {
		ids = append(ids, b.ID+"<"+b.ParentID)
	}
	sort.Strings(ids)
	es := make([]string, 0, len(edges))
	for _, e := range edges {
		es = append(es, e.From+">"+e.To+":"+e.Kind)
	}
	sort.Strings(es)
	return fmt.Sprintf("%v %v %s err=%v", ids, es, membership, err)
}

func t7eIDsOf(rows []beads.Bead) []string {
	out := make([]string, 0, len(rows))
	for _, b := range rows {
		out = append(out, b.ID)
	}
	return out
}

// t7eReady renders a ready response.
func t7eReady(out *ListOutput[beads.Bead], err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("%v total=%d partial=%v errs=%v", t7eIDsOf(out.Body.Items), out.Body.Total, out.Body.Partial, out.Body.PartialErrors)
}

// t7eOps is every converted reader. The by-id ones take the owner the live
// resolveBeadOwner names (both sides resolve it, so its reads match); the
// frozen side receives it as main's callers handed it over, boxed into a
// beads.Store.
func t7eOps(home string) []t7dOp {
	ops := []t7dOp{{
		name: "humaHandleBeadReady",
		frozen: func(t *testing.T, fx t7dFixture) string {
			return t7eReady(frozenHumaHandleBeadReady(t.Context(), New(fx.state), &BeadReadyInput{}))
		},
		live: func(t *testing.T, fx t7dFixture) string {
			return t7eReady(New(fx.state).humaHandleBeadReady(t.Context(), &BeadReadyInput{}))
		},
	}}
	owner := func(fx t7dFixture, id string) (beads.ResidentStore, beads.Bead, string) {
		store, b, err := New(fx.state).resolveBeadOwner(id)
		if err != nil {
			return store, b, "owner: " + err.Error()
		}
		return store, b, ""
	}
	for _, id := range t7eIDs(home) {
		ops = append(ops,
			t7dOp{
				name: "collectBeadGraph " + id,
				frozen: func(_ *testing.T, fx t7dFixture) string {
					store, root, miss := owner(fx, id)
					if miss != "" {
						return miss
					}
					return t7eGraph(frozenCollectBeadGraph(store, root))
				},
				live: func(_ *testing.T, fx t7dFixture) string {
					store, root, miss := owner(fx, id)
					if miss != "" {
						return miss
					}
					return t7eGraph(collectBeadGraph(store, root))
				},
			},
			t7dOp{
				name: "appendMetadataAttachedChildren " + id,
				frozen: func(_ *testing.T, fx t7dFixture) string {
					store, parent, miss := owner(fx, id)
					if miss != "" {
						return miss
					}
					parent.Metadata = map[string]string{beadmeta.MoleculeIDMetadataKey: "al9-wf", "workflow_id": "tw-wf"}
					return fmt.Sprint(t7eIDsOf(frozenAppendMetadataAttachedChildren(store, parent, []beads.Bead{{ID: "tw-wf"}})))
				},
				live: func(_ *testing.T, fx t7dFixture) string {
					store, parent, miss := owner(fx, id)
					if miss != "" {
						return miss
					}
					parent.Metadata = map[string]string{beadmeta.MoleculeIDMetadataKey: "al9-wf", "workflow_id": "tw-wf"}
					return fmt.Sprint(t7eIDsOf(appendMetadataAttachedChildren(store, parent, []beads.Bead{{ID: "tw-wf"}})))
				},
			},
		)
		for _, owned := range []bool{false, true} {
			ops = append(ops,
				t7dOp{
					name: fmt.Sprintf("workRecordRepoDir %s owned=%v", id, owned),
					frozen: func(_ *testing.T, fx t7dFixture) string {
						store, b, miss := owner(fx, id)
						if miss != "" {
							return miss
						}
						return frozenWorkRecordRepoDir(New(fx.state), store, t7eRecord(b, owned))
					},
					live: func(_ *testing.T, fx t7dFixture) string {
						store, b, miss := owner(fx, id)
						if miss != "" {
							return miss
						}
						return New(fx.state).workRecordRepoDir(store, t7eRecord(b, owned))
					},
				},
				t7dOp{
					name: fmt.Sprintf("gateWorkRecordClose %s owned=%v", id, owned),
					frozen: func(t *testing.T, fx t7dFixture) string {
						store, b, miss := owner(fx, id)
						if miss != "" {
							return miss
						}
						return t7eGateCall(t, func() error {
							return frozenGateWorkRecordClose(t.Context(), New(fx.state), id, store, t7eRecord(b, owned), t7eShipped)
						})
					},
					live: func(t *testing.T, fx t7dFixture) string {
						store, b, miss := owner(fx, id)
						if miss != "" {
							return miss
						}
						return t7eGateCall(t, func() error {
							return New(fx.state).gateWorkRecordClose(t.Context(), id, store, t7eRecord(b, owned), t7eShipped)
						})
					},
				},
			)
		}
	}
	return ops
}

// t7eNormalize is an op's log as the table compares it. The ready legs' own
// reads (Ready) keep their order; the cross-store ready gate's blocker reads
// (DepList, Get) follow map order on both sides, so they are compared as a
// set of whole entries: which store was asked about which id, how, under
// which verdict. A graph walk's frontier is the order its last List served,
// map order over a CachingStore, so its ids are compared sorted.
func t7eNormalize(op string, log []string) []string {
	out := make([]string, 0, len(log))
	var gate []string
	for _, entry := range log {
		entry = t7eSortedParentIDs(entry)
		if strings.HasPrefix(op, "humaHandleBeadReady") && !strings.Contains(entry, " Ready ") {
			gate = append(gate, entry)
			continue
		}
		out = append(out, entry)
	}
	if len(gate) > 0 {
		out = append(out, "-- the ready gate's reads --")
		out = append(out, slices.Compact(slices.Sorted(slices.Values(gate)))...)
	}
	return out
}

// t7eSortedParentIDs sorts the ids of a ParentIDs query in a log entry.
func t7eSortedParentIDs(entry string) string {
	const key = "ParentIDs:["
	i := strings.Index(entry, key)
	if i < 0 {
		return entry
	}
	start := i + len(key)
	end := strings.Index(entry[start:], "]")
	if end < 0 {
		return entry
	}
	ids := strings.Fields(entry[start : start+end])
	sort.Strings(ids)
	return entry[:start] + strings.Join(ids, " ") + entry[start+end:]
}

// TestBeadOwnerReadersMatchTheFrozenReaders is the equivalence table.
func TestBeadOwnerReadersMatchTheFrozenReaders(t *testing.T) {
	t.Setenv(workrecord.EnforceEnvVar, "1")
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
				for _, variantName := range slices.Sorted(func(yield func(string) bool) {
					for name := range t7eVariants {
						if !yield(name) {
							return
						}
					}
				}) {
					variant := t7eVariants[variantName]
					t.Run(strings.TrimSuffix(shapeName+", "+variantName, ", "), func(t *testing.T) {
						templates := shape.fixtures(t)
						// One city path for both sides: it is the city checkout
						// the repo dir answers.
						cityPath := t.TempDir()
						fixture := func() t7dFixture {
							fx := templates()
							t7eFakeState(t, fx.state).cityPath = cityPath
							if variant != nil {
								variant(t, fx, t7eFakeState(t, fx.state))
								*fx.log = nil
							}
							return fx
						}
						home := "tc"
						if shape.split {
							home = "gcg"
						}
						for _, op := range t7eOps(home) {
							ffx, lfx := fixture(), fixture()
							want, got := op.frozen(t, ffx), op.live(t, lfx)
							if got != want {
								t.Errorf("%s: live %s\n  frozen %s", op.name, got, want)
								continue
							}
							w, g := t7eNormalize(op.name, *ffx.log), t7eNormalize(op.name, *lfx.log)
							if shape.cached {
								w, g = t7dSortOpRuns(w), t7dSortOpRuns(g)
							}
							if !slices.Equal(w, g) {
								t.Errorf("%s: the live reader's reads and writes differ from the frozen one's%s", op.name, t7dFirstDiff(w, g))
								continue
							}
							if w, g := ffx.dump(t), lfx.dump(t); w != g {
								t.Errorf("%s: the live reader left\n%s\nthe frozen one\n%s", op.name, g, w)
							}
						}
					})
				}
			}
		})
	}
}

// TestBeadReadyReadsOneEpoch pins that a ready request reads Stores() once:
// the work legs, the graph leg and its reserved prefixes come from one epoch.
// The epoch is swapped after the first read, from a split city with a rig to
// one that relocates nothing and has none; a request that read it twice would
// lose the graph leg or the rig.
func TestBeadReadyReadsOneEpoch(t *testing.T) {
	t7dGateOff(t)
	mem := func() *beads.MemStore { return &beads.MemStore{HonorExplicitIDs: true} }
	city, binding, alpha := mem(), mem(), mem()
	for store, id := range map[*beads.MemStore]string{city: "tc-1", binding: "gcg-1", alpha: "al-1"} {
		if _, err := store.Create(beads.Bead{ID: id, Title: id, Type: "task"}); err != nil {
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
	state := &t7dEpochSwapState{fakeState: before, first: before.Stores(), later: after.Stores()}

	out, err := New(state).humaHandleBeadReady(context.Background(), &BeadReadyInput{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(out.Body.Items); got != 3 || state.calls != 1 {
		t.Errorf("ready over a swapped epoch served %d rows after %d Stores() reads; want 3 (city, alpha, the graph binding) after one", got, state.calls)
	}
}

// TestAppendMetadataAttachedChildrenWithoutAnOwner pins the zero owner: no
// store to read, so the children pass through unread, as a nil store did.
func TestAppendMetadataAttachedChildrenWithoutAnOwner(t *testing.T) {
	parent := beads.Bead{ID: "p", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: "m"}}
	children := []beads.Bead{{ID: "c"}}
	got := appendMetadataAttachedChildren(beads.ResidentStore{}, parent, children)
	if want := frozenAppendMetadataAttachedChildren(nil, parent, children); !slices.Equal(t7eIDsOf(got), t7eIDsOf(want)) {
		t.Errorf("attached children over the zero owner = %v, want %v", t7eIDsOf(got), t7eIDsOf(want))
	}
}

// TestWorkRecordLegacyRepoDirReadsOneEpoch pins that the close gate matches
// every configured rig against one Stores() epoch. The epoch is swapped after
// the first read, from one where rig beta holds the answering store to one
// where it holds another: a match that read the epoch per rig would judge the
// bead against the city checkout.
func TestWorkRecordLegacyRepoDirReadsOneEpoch(t *testing.T) {
	t7dGateOff(t)
	alpha, beta, other := beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore()
	before := newFakeState(t)
	before.cityBeadStore = beads.NewMemStore()
	before.stores = map[string]beads.Store{"alpha": alpha, "beta": beta}
	before.cfg.Rigs = []config.Rig{{Name: "alpha", Path: "/nonexistent/alpha"}, {Name: "beta", Path: "/nonexistent/beta"}}
	after := newFakeState(t)
	after.cityBeadStore = before.cityBeadStore
	after.stores = map[string]beads.Store{"alpha": alpha, "beta": other}
	state := &t7dEpochSwapState{fakeState: before, first: before.Stores(), later: after.Stores()}

	s := New(state)
	got := s.workRecordLegacyRepoDir(beadstest.Resident(beta), s.workRecordScopeDirs())
	if got != "/nonexistent/beta" || state.calls != 1 {
		t.Errorf("legacy repo dir over a swapped epoch = %q after %d Stores() reads; want rig beta's checkout after one", got, state.calls)
	}
}

// TestBeadReadyWithoutACityStore pins the ready federation over a city with no
// work store: the zero work handle is no leg, as a nil CityBeadStore() was,
// so the rigs answer alone and nothing is reported short.
func TestBeadReadyWithoutACityStore(t *testing.T) {
	t7dGateOff(t)
	alpha := &beads.MemStore{HonorExplicitIDs: true}
	if _, err := alpha.Create(beads.Bead{ID: "al-1", Title: "ready", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	state := newFakeState(t)
	state.cityBeadStore = nil
	state.stores = map[string]beads.Store{"alpha": alpha}
	got := t7eReady(New(state).humaHandleBeadReady(t.Context(), &BeadReadyInput{}))
	want := t7eReady(frozenHumaHandleBeadReady(t.Context(), New(state), &BeadReadyInput{}))
	if got != want || !strings.HasPrefix(got, "[al-1] total=1 partial=false") {
		t.Errorf("ready without a city store = %s, frozen %s; want alpha's row alone, not partial", got, want)
	}
}

// TestBeadReadyTakesTheGraphBindingsVerdictOnItsOwnNamespace pins the graph
// leg's reserved prefixes (graphBindingPrefixes) on the ready federation: a
// row the stranding graph binding serves, blocked by a closed target in the
// binding's own gcg namespace, is settled by the binding's verdict, so the
// gate asks no work store about that target. Without the prefixes every work
// leg is read for a copy no work store can hold.
func TestBeadReadyTakesTheGraphBindingsVerdictOnItsOwnNamespace(t *testing.T) {
	t7dGateOff(t)
	binding := &beads.MemStore{HonorExplicitIDs: true, IDPrefix: "gcg"}
	for _, b := range []beads.Bead{{ID: "gcg-1", Title: "done step", Type: "task"}, {ID: "gcg-2", Title: "next step", Type: "task"}} {
		if _, err := binding.Create(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := binding.Close("gcg-1"); err != nil {
		t.Fatal(err)
	}
	if err := binding.DepAdd("gcg-2", "gcg-1", "blocks"); err != nil {
		t.Fatal(err)
	}
	log := &[]string{}
	city := &t7dRecordingStore{Store: &beads.MemStore{HonorExplicitIDs: true, IDPrefix: "tc"}, name: "city", log: log}
	alpha := &t7dRecordingStore{Store: &beads.MemStore{HonorExplicitIDs: true, IDPrefix: "al"}, name: "alpha", log: log}
	state := newFakeState(t)
	state.cityBeadStore, state.graphBeadStore = city, binding
	state.stores = map[string]beads.Store{"alpha": alpha}

	out, err := New(state).humaHandleBeadReady(t.Context(), &BeadReadyInput{})
	if err != nil {
		t.Fatal(err)
	}
	if got := t7eIDsOf(out.Body.Items); !slices.Equal(got, []string{"gcg-2"}) {
		t.Fatalf("ready = %v, want [gcg-2]", got)
	}
	for _, entry := range *log {
		if strings.Contains(entry, "gcg-1") {
			t.Errorf("the gate read a work store about gcg-1, a target in the graph binding's own namespace: %s", entry)
		}
	}
}
