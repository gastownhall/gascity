package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The T7d equivalence table: every reader that now reads storeref's Workflow
// legs, run beside its frozen copy (workflow_legs_reference_test.go) on every
// shape, gate on and off, each on a fresh fixture. A run must give the same
// result and touch the same physical stores in the same order, each read with
// the same split-strict verdict, and leave the same rows behind, but for the
// verdicts the plan moves on purpose (t7dMovedVerdicts). The log names the
// physical store a read or write reached, not the gate view it went through;
// the results name the view (t7dFixture.name), so a leg that moved to the
// other view of one store shows there.

// t7dCity is the fixtures' city name. Its rigs sort around it: alpha and beta
// before it, so a co-resident id shows which of a rig and the city a
// first-reader asks first.
const t7dCity = "test-city"

// t7dRecordingStore logs every operation it serves into its fixture's shared
// log: the store's name, the operation and its argument, and how the
// split-strict gate would declare a read made there (a read marker on the
// stack decides, else the innermost leg's role; splittest's declared()).
type t7dRecordingStore struct {
	beads.Store
	name string
	log  *[]string
}

func (r *t7dRecordingStore) note(op string, arg any) {
	pcs := make([]uintptr, 96)
	pcs = pcs[:runtime.Callers(2, pcs)]
	verdict := "strict"
	switch marker, role := beads.ReadMarkerOnStack(pcs), beads.LegRoleOnStack(pcs); {
	case marker == beads.RelicReadMarker, marker == beads.NoReadMarker && role == beads.RelicLegRole:
		verdict = "relic"
	case marker == beads.FederatedReadMarker, marker == beads.NoReadMarker && role == beads.FederatedLegRole:
		verdict = "federated"
	}
	entry := fmt.Sprintf("%s %s %+v [%s]", r.name, op, arg, verdict)
	t7dLogMu.Lock()
	defer t7dLogMu.Unlock()
	*r.log = append(*r.log, entry)
}

// t7dLogMu guards every fixture log: a reader may read from several
// goroutines (the cross-store ready gate's blocker reads, T7e).
var t7dLogMu sync.Mutex

func (r *t7dRecordingStore) Get(id string) (beads.Bead, error) {
	r.note("Get", id)
	return r.Store.Get(id)
}

// List answers in id order: a MemStore lists in map order, and the frozen
// and live runs must read the same rows in the same order to be compared.
func (r *t7dRecordingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	r.note("List", q)
	rows, err := r.Store.List(q)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, err
}

func (r *t7dRecordingStore) ListOpen(status ...string) ([]beads.Bead, error) {
	r.note("ListOpen", status)
	return r.Store.ListOpen(status...)
}

func (r *t7dRecordingStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	r.note("Ready", q)
	return r.Store.Ready(q...)
}

func (r *t7dRecordingStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	r.note("Children", parentID)
	return r.Store.Children(parentID, opts...)
}

func (r *t7dRecordingStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	r.note("ListByLabel", label)
	return r.Store.ListByLabel(label, limit, opts...)
}

func (r *t7dRecordingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	r.note("ListByMetadata", filters)
	return r.Store.ListByMetadata(filters, limit, opts...)
}

func (r *t7dRecordingStore) DepList(id, direction string) ([]beads.Dep, error) {
	r.note("DepList", id+" "+direction)
	return r.Store.DepList(id, direction)
}

func (r *t7dRecordingStore) Update(id string, opts beads.UpdateOpts) error {
	r.note("Update", id)
	return r.Store.Update(id, opts)
}

func (r *t7dRecordingStore) Close(id string) error { r.note("Close", id); return r.Store.Close(id) }

func (r *t7dRecordingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	// In id order: a CachingStore lists in map order, so the order a
	// teardown hands over its ids is not the run's to compare.
	r.note("CloseAll", fmt.Sprint(slices.Sorted(slices.Values(ids)), metadata))
	return r.Store.CloseAll(ids, metadata)
}

func (r *t7dRecordingStore) SetMetadata(id, key, value string) error {
	r.note("SetMetadata", id+" "+key+"="+value)
	return r.Store.SetMetadata(id, key, value)
}

func (r *t7dRecordingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	r.note("SetMetadataBatch", fmt.Sprint(id, kvs))
	return r.Store.SetMetadataBatch(id, kvs)
}

func (r *t7dRecordingStore) Delete(id string) error { r.note("Delete", id); return r.Store.Delete(id) }

func (r *t7dRecordingStore) DepRemove(issueID, dependsOnID string) error {
	r.note("DepRemove", issueID+" "+dependsOnID)
	return r.Store.DepRemove(issueID, dependsOnID)
}

// errT7dRefused is the worker site's standing refusal.
var errT7dRefused = errors.New("t7d: this worker site serves no binding")

// t7dRefusingStore is cmd/gc's refusedClassStore as the fixtures see it:
// every read refuses, and is logged.
type t7dRefusingStore struct{ *t7dRecordingStore }

func (t7dRefusingStore) StorageRefusal() error { return errT7dRefused }

func (s t7dRefusingStore) Get(id string) (beads.Bead, error) {
	s.note("Get", id)
	return beads.Bead{}, errT7dRefused
}

func (s t7dRefusingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.note("List", q)
	return nil, errT7dRefused
}

func (s t7dRefusingStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.note("DepList", id+" "+direction)
	return nil, errT7dRefused
}

// t7dShape is one fixture shape.
type t7dShape struct {
	city             string // the city's name; empty is t7dCity
	split            bool   // a whole split: every class in one binding
	rigNamedLikeCity bool   // a rig named like the city, holding a root
	relicFreeNamed   bool   // that rig is proven relic-free (the S4 bit)
	suspendBeta      bool   // the holder reports rig beta suspended
	relicFreeAlpha   bool   // rig alpha is proven relic-free (the S4 bit)
	refused          bool   // a worker site refusing every relocated class
	stranded         bool   // a graph root stranded in the city work store, served degraded
	cached           bool   // the city and rig stores behind a CachingStore
	ordersApart      bool   // the orders class in a binding of its own, apart from the graph binding
}

// cityName is the shape's city name.
func (sh t7dShape) cityName() string {
	if sh.city != "" {
		return sh.city
	}
	return t7dCity
}

// t7dShapes is the corpus.
var t7dShapes = map[string]t7dShape{
	"unsplit":                       {},
	"unsplit, cached":               {cached: true},
	"unsplit, suspended rig":        {suspendBeta: true},
	"unsplit, rig named like city":  {rigNamedLikeCity: true},
	"whole split":                   {split: true},
	"whole split, cached":           {split: true, cached: true},
	"whole split, suspended rig":    {split: true, suspendBeta: true},
	"whole split, relic-free rig":   {split: true, relicFreeAlpha: true},
	"whole split, rig named like":   {split: true, rigNamedLikeCity: true},
	"whole split, worker refused":   {split: true, refused: true},
	"stranded, served degraded":     {split: true, stranded: true},
	"stranded, relic-free, cached":  {split: true, stranded: true, relicFreeAlpha: true, cached: true},
	"whole split, every rig marked": {split: true, suspendBeta: true, relicFreeAlpha: true, rigNamedLikeCity: true},
	// A rig named like the city, proven relic-free, beside a graph run
	// stranded in the city store: isGraphConvoyID reads neither (the rig
	// takes the city's place in BeadStores(), and its leg is retired).
	"stranded, relic-free rig named like city": {split: true, rigNamedLikeCity: true, relicFreeNamed: true, stranded: true},
	// A city named "aaa", so the rig named like it, a leg the workflow scan
	// skips, sorts before every other rig.
	"skipped rig sorts first, split":   {city: "aaa", split: true, rigNamedLikeCity: true},
	"skipped rig sorts first, unsplit": {city: "aaa", rigNamedLikeCity: true},
	// Order tracking in an orders binding apart from the graph binding (a
	// per-class split no serving city has): the one shape where the order
	// reads' orders leg is not already a workflow leg.
	"orders binding apart": {split: true, ordersApart: true},
}

// t7dFixture is one built shape.
type t7dFixture struct {
	state State
	log   *[]string
	mems  map[string]*beads.MemStore
	// tops are the stores the state hands out, by name, before any view.
	tops map[string]beads.Store
}

// name is the store and side a reader or store reads: the named store, or the
// gate's work or class view of it.
func (fx t7dFixture) name(s beads.StoreReader) string {
	if beads.IsNilStore(s) {
		return "<nil>"
	}
	names := make([]string, 0, len(fx.tops))
	for name := range fx.tops {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		top := fx.tops[name]
		switch {
		case splittest.GateEnabled() && beads.SameStore(s, splittest.GateWorkView(top)):
			return name + "/work"
		case splittest.GateEnabled() && beads.SameStore(s, splittest.GateClassView(top)):
			return name + "/class"
		case beads.SameStore(s, top):
			return name
		}
	}
	return "<unknown>"
}

// dump is every row and edge the fixture's stores hold, for comparing what a
// write left behind.
func (fx t7dFixture) dump(t *testing.T) string {
	t.Helper()
	var out []string
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range fx.mems {
			if !yield(name) {
				return
			}
		}
	}) {
		rows, err := fx.mems[name].List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
		if err != nil {
			t.Fatalf("dumping %s: %v", name, err)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		for _, b := range rows {
			deps, _ := fx.mems[name].DepList(b.ID, "down")
			out = append(out, fmt.Sprintf("%s %s %s %s %v %v %v", name, b.ID, b.Status, b.Assignee, b.Metadata, b.Labels, deps))
		}
	}
	return strings.Join(out, "\n")
}

// t7dBead seeds one row with a pinned id.
func t7dBead(t *testing.T, store *beads.MemStore, b beads.Bead) {
	t.Helper()
	status := b.Status
	b.Status = ""
	if _, err := store.Create(b); err != nil {
		t.Fatalf("seeding %s: %v", b.ID, err)
	}
	if status == "" || status == "open" {
		return
	}
	if status == "closed" {
		if err := store.Close(b.ID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatal(err)
	}
}

// t7dWorkflow seeds a graph.v2 workflow root and its steps: one open, one in
// progress, one closed, the second blocked on the first.
func t7dWorkflow(t *testing.T, store *beads.MemStore, prefix, workflowID, scopeKind, scopeRef string) {
	t.Helper()
	root := prefix + "-wf"
	t7dBead(t, store, beads.Bead{ID: root, Title: "run " + workflowID, Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.WorkflowIDMetadataKey:      workflowID,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.ScopeKindMetadataKey:       scopeKind,
		beadmeta.ScopeRefMetadataKey:        scopeRef,
		beadmeta.RootStoreRefMetadataKey:    scopeKind + ":" + scopeRef,
	}})
	for i, status := range []string{"open", "in_progress", "closed"} {
		step := beads.Bead{ID: fmt.Sprintf("%s-s%d", prefix, i+1), Title: "step", Type: "task", Status: status, Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:   root,
			beadmeta.RootStoreRefMetadataKey: scopeKind + ":" + scopeRef,
		}}
		if status == "in_progress" {
			step.Assignee = prefix + "-worker"
		}
		t7dBead(t, store, step)
	}
	if err := store.DepAdd(prefix+"-s2", prefix+"-s1", "blocks"); err != nil {
		t.Fatal(err)
	}
}

// fixtures seeds shape sh's stores once and returns a maker of fresh
// fixtures over copies of them, so the frozen and live runs of an op read the
// same rows, creation stamps included, and neither sees the other's writes.
func (sh t7dShape) fixtures(t *testing.T) func() t7dFixture {
	t.Helper()
	return sh.fixturesWith(t, nil)
}

// fixturesWith is fixtures with more rows: seed adds them to the templates,
// by name ("city", "alpha", "beta", and "binding", "named" and "orders" where
// the shape has them), after the shape's own, so every fixture's copy holds
// them with the same stamps.
func (sh t7dShape) fixturesWith(t *testing.T, seed func(templates map[string]*beads.MemStore)) func() t7dFixture {
	t.Helper()
	city := sh.cityName()
	templates := map[string]*beads.MemStore{}
	mem := func(name, prefix string) *beads.MemStore {
		m := &beads.MemStore{HonorExplicitIDs: true, IDPrefix: prefix}
		templates[name] = m
		return m
	}
	cityMem, alpha, beta := mem("city", "tc"), mem("alpha", "al"), mem("beta", "be")
	var binding, named *beads.MemStore
	home := cityMem
	if sh.split {
		binding = mem("binding", "gcg")
		home = binding
	}
	if sh.rigNamedLikeCity {
		named = mem("named", "tx")
	}

	// The live run: in the binding on a split city, else in the city.
	t7dWorkflow(t, home, home.IDPrefix+"1", "run-1", beadmeta.ScopeKindCity, city)
	// A pre-split rig tree, a work convoy and a rig run.
	t7dWorkflow(t, alpha, "al9", "run-9", beadmeta.ScopeKindRig, "alpha")
	t7dBead(t, alpha, beads.Bead{ID: "al-cv1", Title: "work convoy", Type: "convoy"})
	t7dWorkflow(t, beta, "be5", "run-5", beadmeta.ScopeKindRig, "beta")
	// Co-resident ids: co-1 a plain task in alpha and a workflow root in the
	// city; co-2 a workflow root in the home store and a task in alpha.
	t7dBead(t, alpha, beads.Bead{ID: "co-1", Title: "plain", Type: "task"})
	t7dBead(t, cityMem, beads.Bead{ID: "co-1", Title: "graph", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.WorkflowIDMetadataKey: "co-1"}})
	if home != cityMem {
		t7dBead(t, home, beads.Bead{ID: "co-2", Title: "graph", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.WorkflowIDMetadataKey: "co-2"}})
	}
	t7dBead(t, alpha, beads.Bead{ID: "co-2", Title: "plain", Type: "task"})
	if named != nil {
		t7dWorkflow(t, named, "tx7", "run-7", beadmeta.ScopeKindRig, city)
		t7dBead(t, named, beads.Bead{ID: "co-1", Title: "named", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.WorkflowIDMetadataKey: "co-1"}})
	}
	if sh.stranded {
		t7dWorkflow(t, cityMem, "tc3", "run-3", beadmeta.ScopeKindCity, city)
	}
	// A twin: one run's tree in the home store and, under the same ids, in
	// alpha (an import's copy before its clear).
	t7dWorkflow(t, home, "tw", "run-tw", beadmeta.ScopeKindRig, "alpha")
	if home != alpha {
		t7dWorkflow(t, alpha, "tw", "run-tw", beadmeta.ScopeKindRig, "alpha")
	}
	// Order runs: the live history where tracking is born, pre-split history
	// in a rig.
	t7dBead(t, home, beads.Bead{ID: home.IDPrefix + "-or1", Title: "order:nightly", Type: "task", Status: "closed", Labels: []string{"order-run:nightly", "order-tracking"}})
	if sh.ordersApart {
		ordersMem := mem("orders", "or")
		t7dBead(t, ordersMem, beads.Bead{ID: "or-or1", Title: "order:nightly", Type: "task", Status: "closed", Labels: []string{"order-run:nightly", "order-tracking"}})
	}
	t7dBead(t, alpha, beads.Bead{ID: "al-or1", Title: "order:nightly", Type: "task", Status: "closed", Labels: []string{"order-run:nightly", "order-tracking"}})

	if seed != nil {
		seed(templates)
	}
	return func() t7dFixture { return sh.fixture(t, templates) }
}

// t7dClone is a copy of m: its rows, stamps and edges.
func t7dClone(t *testing.T, m *beads.MemStore) *beads.MemStore {
	t.Helper()
	rows, err := m.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	var deps []beads.Dep
	for _, b := range rows {
		down, err := m.DepList(b.ID, "down")
		if err != nil {
			t.Fatal(err)
		}
		deps = append(deps, down...)
	}
	c := beads.NewMemStoreFrom(0, rows, deps)
	c.HonorExplicitIDs, c.IDPrefix = true, m.IDPrefix
	return c
}

// fixture is one fresh fixture of shape sh over copies of templates.
func (sh t7dShape) fixture(t *testing.T, templates map[string]*beads.MemStore) t7dFixture {
	t.Helper()
	log := &[]string{}
	fx := t7dFixture{log: log, mems: map[string]*beads.MemStore{}, tops: map[string]beads.Store{}}
	for name, m := range templates {
		fx.mems[name] = t7dClone(t, m)
	}
	city, alpha, beta, binding, named := fx.mems["city"], fx.mems["alpha"], fx.mems["beta"], fx.mems["binding"], fx.mems["named"]
	top := func(name string, m *beads.MemStore) beads.Store {
		var s beads.Store = &t7dRecordingStore{Store: m, name: name, log: log}
		if sh.cached && name != "binding" {
			c := beads.NewCachingStoreForTest(s, nil)
			if err := c.Prime(t.Context()); err != nil {
				t.Fatal(err)
			}
			s = c
		}
		fx.tops[name] = s
		return s
	}
	fs := newFakeState(t)
	fs.cityName = sh.cityName()
	fs.cfg.Workspace.Name = sh.cityName()
	fs.cityBeadStore = top("city", city)
	fs.stores = map[string]beads.Store{"alpha": top("alpha", alpha), "beta": top("beta", beta)}
	fs.cfg.Rigs = []config.Rig{{Name: "alpha", Path: "/nonexistent/alpha"}, {Name: "beta", Path: "/nonexistent/beta"}}
	if named != nil {
		fs.stores[sh.cityName()] = top("named", named)
		fs.cfg.Rigs = append(fs.cfg.Rigs, config.Rig{Name: sh.cityName(), Path: "/nonexistent/tx"})
	}
	if binding != nil {
		b := top("binding", binding)
		fs.graphBeadStore, fs.ordersBeadStore, fs.sessionsBeadStore, fs.nudgesBeadStore = b, b, b, b
	}
	if orders := fx.mems["orders"]; orders != nil {
		fs.ordersBeadStore = top("orders", orders)
	}
	if sh.refused {
		refusal := t7dRefusingStore{&t7dRecordingStore{Store: beads.NewMemStore(), name: "refused", log: log}}
		fx.tops["refused"] = refusal
		fs.classRefusals = map[coordclass.Class]storeref.RefusingClassStore{}
		for _, c := range coordclass.Classes() {
			if c.IsInfrastructure() {
				fs.classRefusals[c] = refusal
			}
		}
	}
	if sh.suspendBeta {
		fs.rigBits.suspended = map[string]bool{"beta": true}
	}
	fx.state = fs
	if sh.relicFreeAlpha || sh.relicFreeNamed {
		relicFree := map[string]bool{"alpha": sh.relicFreeAlpha, sh.cityName(): sh.relicFreeNamed}
		fx.state = relicReportingState{fakeState: fs, relicFree: relicFree}
	}
	*log = nil
	return fx
}

// t7dFrozenInfo is main's workflowStoreInfo as T7d's frozen copies built it:
// a workflow leg, or an order read's store (T7d.2 retired that field from
// production; the frozen copies keep it).
type t7dFrozenInfo struct {
	ref       string
	scopeKind string
	scopeRef  string
	leg       storeref.TypedLeg
	store     beads.Store
}

// t7dFrozenRootMatch is main's workflowRootMatch over a frozen entry.
type t7dFrozenRootMatch struct {
	info t7dFrozenInfo
	root beads.Bead
}

// live is f as a live entry, for the pure live helpers the frozen copies call
// (they read its names and leg, never its store).
func (f t7dFrozenInfo) live() workflowStoreInfo {
	return workflowStoreInfo{ref: f.ref, scopeKind: f.scopeKind, scopeRef: f.scopeRef, leg: f.leg}
}

// t7dLive is a live entry as the renderer reads it.
func t7dLive(info workflowStoreInfo, ok bool) (t7dFrozenInfo, bool) {
	return t7dFrozenInfo{ref: info.ref, scopeKind: info.scopeKind, scopeRef: info.scopeRef, leg: info.leg}, ok
}

// t7dLives is t7dLive over a list.
func t7dLives(infos []workflowStoreInfo) []t7dFrozenInfo {
	out := make([]t7dFrozenInfo, 0, len(infos))
	for _, info := range infos {
		l, _ := t7dLive(info, true)
		out = append(out, l)
	}
	return out
}

// t7dInfo renders a workflow store entry: its names, and the store and side
// the live code reads through its leg (or, in the frozen code, through its
// store).
func (fx t7dFixture) t7dInfo(info t7dFrozenInfo, ok bool) string {
	if !ok {
		return "-"
	}
	reads := info.store
	if !beads.IsNilStore(info.leg.Reader) {
		if !beads.IsNilStore(info.store) && !beads.SameStore(info.store, info.leg.Reader) {
			return fmt.Sprintf("%s|%s|%s|leg %s, store %s", info.ref, info.scopeKind, info.scopeRef, fx.name(info.leg.Reader), fx.name(info.store))
		}
		return fmt.Sprintf("%s|%s|%s|%s", info.ref, info.scopeKind, info.scopeRef, fx.name(info.leg.Reader))
	}
	return fmt.Sprintf("%s|%s|%s|%s", info.ref, info.scopeKind, info.scopeRef, fx.name(reads))
}

func (fx t7dFixture) t7dInfos(infos []t7dFrozenInfo) string {
	out := make([]string, 0, len(infos))
	for _, info := range infos {
		out = append(out, fx.t7dInfo(info, true))
	}
	return strings.Join(out, "; ")
}

// t7dJSON renders a result and its error.
func t7dJSON(v any, err error) string {
	data, mErr := json.Marshal(v)
	if mErr != nil {
		data = []byte(fmt.Sprintf("%+v", v))
	}
	if err != nil {
		return string(data) + " error: " + err.Error()
	}
	return string(data)
}

// t7dOp is one reader, frozen and live, rendered.
type t7dOp struct {
	name         string
	frozen, live func(t *testing.T, fx t7dFixture) string
	resultsOnly  bool // a declared log divergence; see the op
	// orderVerdicts is an order read (the feed, the history detail): T7d.2
	// moved its verdicts to its order-history plan's roles, which
	// order_history_handlers_equivalence_test.go judges; here its results
	// and its reads, store by store and operation by operation, must match.
	orderVerdicts bool
	// writes is a teardown. Over a CachingStore, which lists and reads back a
	// batch in map order, the order of its reads and writes, and how many
	// edges it unlinks before their other end is gone, vary from run to run
	// on either side; there the table compares which stores it read and wrote
	// how, beside its result and the rows it leaves.
	writes bool
}

func t7dEventBead(id, rootID, hint string) beads.Bead {
	return beads.Bead{ID: id, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:   rootID,
		beadmeta.RootStoreRefMetadataKey: hint,
	}}
}

// t7dOps is every converted reader, on ids and refs covering each leg.
func t7dOps(city string) []t7dOp {
	var ops []t7dOp
	ops = append(ops,
		t7dOp{
			name:   "workflowStores",
			frozen: func(_ *testing.T, fx t7dFixture) string { return fx.t7dInfos(frozenWorkflowStores(fx.state)) },
			live:   func(_ *testing.T, fx t7dFixture) string { return fx.t7dInfos(t7dLives(workflowStores(fx.state))) },
		},
		t7dOp{
			name: "workflowStoresIncludingRetiredRigs",
			frozen: func(_ *testing.T, fx t7dFixture) string {
				return fx.t7dInfos(frozenWorkflowStoresIncludingRetiredRigs(fx.state))
			},
			live: func(_ *testing.T, fx t7dFixture) string {
				return fx.t7dInfos(t7dLives(workflowStoresIncludingRetiredRigs(fx.state)))
			},
		},
		t7dOp{
			name: "every order's legs",
			frozen: func(_ *testing.T, fx t7dFixture) string {
				return fx.t7dInfos(frozenAppendOrdersClassStoreInfo(frozenWorkflowStores(fx.state), fx.state, city))
			},
			live: func(_ *testing.T, fx t7dFixture) string {
				return fx.t7dInfos(t7dLives(everyOrderLegs(fx.state.Stores(), fx.state.CityName())))
			},
		},
	)
	for _, ref := range []string{"city:" + city, "graph:" + city, "orders:" + city, "rig:alpha", "rig:beta", "rig:" + city, " rig : alpha ", "city:other", "graph:other", "rig:missing", "rig:", "alpha", ""} {
		ops = append(ops, t7dOp{
			name:   "workflowStoreByRef " + ref,
			frozen: func(_ *testing.T, fx t7dFixture) string { return fx.t7dInfo(frozenWorkflowStoreByRef(fx.state, ref)) },
			live: func(_ *testing.T, fx t7dFixture) string {
				return fx.t7dInfo(t7dLive(workflowStoreByRef(fx.state, ref)))
			},
		})
	}
	for _, id := range []string{"tc1-wf", "gcg1-wf", "al9-wf", "be5-wf", "tx7-wf", "tc3-wf", "tw-wf", "al-cv1", "co-1", "co-2", "missing"} {
		ops = append(ops, t7dOp{
			name:   "isGraphConvoyID " + id,
			frozen: func(_ *testing.T, fx t7dFixture) string { return fmt.Sprint(frozenIsGraphConvoyID(New(fx.state), id)) },
			live:   func(_ *testing.T, fx t7dFixture) string { return fmt.Sprint(isGraphConvoyID(New(fx.state), id)) },
		})
		ops = append(ops, t7dOp{
			name: "workflowEventBeadFromSubject " + id,
			frozen: func(_ *testing.T, fx t7dFixture) string {
				b, ok := frozenWorkflowEventBeadFromSubject(fx.state, id)
				return fmt.Sprint(ok, " ", t7dJSON(b, nil))
			},
			live: func(_ *testing.T, fx t7dFixture) string {
				b, ok := workflowEventBeadFromSubject(fx.state, id)
				return fmt.Sprint(ok, " ", t7dJSON(b, nil))
			},
		})
	}
	for _, root := range []string{"tc1-wf", "gcg1-wf", "al9-wf", "be5-wf", "tx7-wf", "tc3-wf", "tw-wf", "co-1", "co-2", "missing"} {
		for _, hint := range []string{"", "city:" + city, "rig:alpha", "rig:beta", "rig:" + city, "graph:" + city, " rig : alpha ", "orders:" + city} {
			op := t7dOp{
				name: "workflowEventRoot " + root + " hint=" + hint,
				frozen: func(_ *testing.T, fx t7dFixture) string {
					info, b, ok := frozenWorkflowEventRoot(fx.state, t7dEventBead("evt", root, hint))
					return fx.t7dInfo(info, ok) + " " + b.ID
				},
				live: func(_ *testing.T, fx t7dFixture) string {
					info, b, ok := workflowEventRoot(fx.state, t7dEventBead("evt", root, hint))
					l, ok := t7dLive(info, ok)
					return fx.t7dInfo(l, ok) + " " + b.ID
				},
			}
			// An orders:<city> hint is no scope any writer stamps. The frozen
			// lookup resolved it to the orders binding where one is apart from
			// the city store and probed it once more; the legs name no orders
			// leg, so the live lookup skips it. Same answer, one read fewer.
			op.resultsOnly = strings.HasPrefix(hint, "orders:")
			ops = append(ops, op)
		}
	}
	for _, wid := range []string{"run-1", "run-9", "run-5", "run-7", "run-3", "run-tw", "tc1-wf", "gcg1-wf", "al9-wf", "tw-wf", "co-1", "co-2", "missing"} {
		for _, scope := range [][2]string{{"", ""}, {beadmeta.ScopeKindRig, "alpha"}, {beadmeta.ScopeKindCity, city}} {
			ops = append(ops, t7dOp{
				name: "buildWorkflowSnapshot " + wid + " " + scope[0] + ":" + scope[1],
				frozen: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(frozenBuildWorkflowSnapshot(New(fx.state), wid, scope[0], scope[1], 0))
				},
				live: func(t *testing.T, fx t7dFixture) string {
					s := New(fx.state)
					if _, err := s.tryFullWorkflowSQL(wid, scope[0], scope[1], 0); err == nil {
						t.Fatalf("the SQL fast path served %s; the frozen copy has none", wid)
					}
					*fx.log = nil
					return t7dJSON(s.buildWorkflowSnapshot(wid, scope[0], scope[1], 0))
				},
			})
		}
	}
	for _, scope := range [][2]string{{beadmeta.ScopeKindCity, city}, {beadmeta.ScopeKindRig, "alpha"}, {beadmeta.ScopeKindRig, "beta"}} {
		ops = append(ops,
			t7dOp{
				name: "buildWorkflowRunProjections " + scope[1],
				frozen: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(frozenBuildWorkflowRunProjections(fx.state, scope[0], scope[1], ""))
				},
				live: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(buildWorkflowRunProjections(fx.state, scope[0], scope[1], ""))
				},
			},
			t7dOp{
				name: "buildWorkflowRunProjectionsRootOnly " + scope[1],
				frozen: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(frozenBuildWorkflowRunProjectionsRootOnly(fx.state, scope[0], scope[1]))
				},
				live: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(buildWorkflowRunProjectionsRootOnly(fx.state, scope[0], scope[1]))
				},
			},
			t7dOp{
				name: "buildOrderRunFeedItems " + scope[1], orderVerdicts: true,
				frozen: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(frozenBuildOrderRunFeedItems(fx.state, scope[0], scope[1], 0))
				},
				live: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(buildOrderRunFeedItems(fx.state, scope[0], scope[1], 0))
				},
			},
		)
	}
	for _, id := range []string{"tc-or1", "gcg-or1", "al-or1", "or-or1", "missing"} {
		for _, ref := range []string{"", "city:" + city, "rig:alpha", "orders:" + city, "graph:" + city} {
			// The order history detail handler, frozen and live: its store
			// list now comes from the workflow legs, and the order reader it
			// calls is the one T7d leaves as it is.
			ops = append(ops, t7dOp{
				name: "humaHandleOrderHistoryDetail " + id + " ref=" + ref, orderVerdicts: true,
				frozen: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(frozenHumaHandleOrderHistoryDetail(context.Background(), New(fx.state), &OrderHistoryDetailInput{BeadID: id, StoreRef: ref}))
				},
				live: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(New(fx.state).humaHandleOrderHistoryDetail(context.Background(), &OrderHistoryDetailInput{BeadID: id, StoreRef: ref}))
				},
			})
		}
	}
	// The teardowns, on every run and on the co-resident ids and the twin:
	// each writes on every leg that holds a matching root.
	for _, wid := range []string{"run-1", "run-9", "run-5", "run-7", "run-3", "run-tw", "tw-wf", "co-1", "co-2", "missing"} {
		ops = append(ops,
			t7dOp{
				name: "humaDeleteWorkflow " + wid, writes: true,
				frozen: func(_ *testing.T, fx t7dFixture) string { return t7dJSON(frozenHumaDeleteWorkflow(New(fx.state), wid)) },
				live:   func(_ *testing.T, fx t7dFixture) string { return t7dJSON(New(fx.state).humaDeleteWorkflow(wid)) },
			},
			t7dOp{
				name: "cancelRun " + wid, writes: true,
				frozen: func(_ *testing.T, fx t7dFixture) string {
					var held []string
					res, err := frozenCancelRun(New(fx.state), wid, func(h []string) { held = append(held, h...) })
					return fmt.Sprintf("%+v %v", res, held) + t7dJSON(nil, err)
				},
				live: func(_ *testing.T, fx t7dFixture) string {
					var held []string
					res, err := New(fx.state).cancelRun(wid, func(h []string) { held = append(held, h...) })
					return fmt.Sprintf("%+v %v", res, held) + t7dJSON(nil, err)
				},
			},
		)
		for _, in := range []WorkflowDeleteInput{
			{WorkflowID: wid},
			{WorkflowID: wid, Delete: true},
			{WorkflowID: wid, Delete: true, ScopeKind: beadmeta.ScopeKindRig, ScopeRef: "alpha"},
			{WorkflowID: wid, ScopeKind: beadmeta.ScopeKindCity, ScopeRef: city},
		} {
			ops = append(ops, t7dOp{
				name: fmt.Sprintf("humaHandleWorkflowDelete %+v", in), writes: true,
				frozen: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(frozenHumaHandleWorkflowDelete(context.Background(), New(fx.state), &in))
				},
				live: func(_ *testing.T, fx t7dFixture) string {
					return t7dJSON(New(fx.state).humaHandleWorkflowDelete(context.Background(), &in))
				},
			})
		}
	}
	return ops
}

// t7dMovedVerdicts is the split-strict verdicts the plan moves on purpose,
// applied to the frozen log: a read the frozen code marked relic on a leg the
// Workflow plan declares federated — the relocated graph binding (the store
// that holds every live root), or any leg of a city that relocates nothing —
// is federated now. Every other read keeps its verdict, a strict read
// included. Under the gate an unsplit city's one store is both the graph
// leg's class view (federated) and the city leg's work view (relic), which
// its log cannot tell apart, so there either verdict of the two reads alike.
func t7dMovedVerdicts(fx t7dFixture, frozen, live []string) ([]string, []string) {
	plan, err := fx.state.Stores().Plan(storeref.Workflow{IncludeRetiredRigs: true, SuspendedRigs: storeref.WorkflowReadsSuspendedRigs})
	federated, relic := map[string]bool{}, map[string]bool{}
	if err == nil {
		for _, l := range plan.Legs {
			name, _, _ := strings.Cut(fx.name(l.Leg.Store), "/")
			switch l.Gate {
			case beads.FederatedLegRole:
				federated[name] = true
			case beads.RelicLegRole:
				relic[name] = true
			}
		}
	}
	norm := func(log []string, rewrite bool) []string {
		out := make([]string, len(log))
		for i, entry := range log {
			name, _, _ := strings.Cut(entry, " ")
			switch {
			case federated[name] && relic[name]:
				entry = strings.Replace(strings.Replace(entry, "[relic]", "[relic|federated]", 1), "[federated]", "[relic|federated]", 1)
			case federated[name] && rewrite:
				entry = strings.Replace(entry, "[relic]", "[federated]", 1)
			}
			out[i] = entry
		}
		return out
	}
	return norm(frozen, true), norm(live, false)
}

// TestWorkflowLegsMatchTheFrozenReaders is the equivalence table.
func TestWorkflowLegsMatchTheFrozenReaders(t *testing.T) {
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
					fixture := shape.fixtures(t)
					for _, op := range t7dOps(shape.cityName()) {
						ffx, lfx := fixture(), fixture()
						want, got := t7dCanonical(op.frozen(t, ffx)), t7dCanonical(op.live(t, lfx))
						if strings.Contains(want+got, "<unknown>") {
							t.Fatalf("%s: a result names a store the fixture does not hold: %s / %s", op.name, got, want)
						}
						if got != want {
							t.Errorf("%s: live %s\n  frozen %s", op.name, got, want)
							continue
						}
						if op.resultsOnly {
							continue
						}
						w, g := t7dSortDepRuns(*ffx.log), t7dSortDepRuns(*lfx.log)
						if op.orderVerdicts {
							w, g = t7dStripVerdicts(w), t7dStripVerdicts(g)
						} else {
							w, g = t7dMovedVerdicts(ffx, w, g)
						}
						switch {
						case shape.cached && op.writes:
							w, g = t7dOpSet(w), t7dOpSet(g)
						case shape.cached:
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
		})
	}
}

// t7dStripVerdicts drops each entry's split-strict verdict.
func t7dStripVerdicts(log []string) []string {
	out := make([]string, len(log))
	for i, entry := range log {
		out[i] = entry[:strings.LastIndex(entry, " [")]
	}
	return out
}

// t7dFirstDiff renders where two logs part.
func t7dFirstDiff(frozen, live []string) string {
	for i := 0; i < len(frozen) || i < len(live); i++ {
		var f, l string
		if i < len(frozen) {
			f = frozen[i]
		}
		if i < len(live) {
			l = live[i]
		}
		if f != l {
			return fmt.Sprintf(" at entry %d of %d/%d:\n  live   %s\n  frozen %s", i, len(live), len(frozen), l, f)
		}
	}
	return ""
}

// t7dSortDepRuns sorts each run of consecutive strict DepList and Get reads
// of one store: collectWorkflowDeps reads the edges in map order, on both
// sides, and under the gate each edge read is followed by the view's own
// gets. Such a run is compared as a set.
func t7dSortDepRuns(log []string) []string {
	out := slices.Clone(log)
	store := func(entry string) string { name, _, _ := strings.Cut(entry, " "); return name }
	edgeRead := func(entry string) bool {
		return strings.HasSuffix(entry, "[strict]") && (strings.Contains(entry, " DepList ") || strings.Contains(entry, " Get "))
	}
	for i := 0; i < len(out); {
		j := i
		for j < len(out) && edgeRead(out[j]) && store(out[j]) == store(out[i]) {
			j++
		}
		if j == i {
			i++
			continue
		}
		if slices.ContainsFunc(out[i:j], func(e string) bool { return strings.Contains(e, " DepList ") }) {
			sort.Strings(out[i:j])
		}
		i = j
	}
	return out
}

// t7dCanonical is a rendered result with a snapshot's beads and deps in id
// order: the snapshot gathers deps from a map, on both sides.
func t7dCanonical(result string) string {
	body, rest, _ := strings.Cut(result, " error: ")
	var snap workflowSnapshotResponse
	if err := json.Unmarshal([]byte(body), &snap); err != nil || snap.RootBeadID == "" {
		return result
	}
	sort.Slice(snap.Beads, func(i, j int) bool { return snap.Beads[i].ID < snap.Beads[j].ID })
	sort.Slice(snap.Deps, func(i, j int) bool {
		return snap.Deps[i].From+snap.Deps[i].To+snap.Deps[i].Kind < snap.Deps[j].From+snap.Deps[j].To+snap.Deps[j].Kind
	})
	data, _ := json.Marshal(snap)
	if rest != "" {
		return string(data) + " error: " + rest
	}
	return string(data)
}

// t7dOpSet is a log as the set of its stores, operations and verdicts.
func t7dOpSet(log []string) []string {
	set := map[string]bool{}
	for _, entry := range log {
		fields := strings.Fields(entry)
		set[fields[0]+" "+fields[1]+" "+fields[len(fields)-1]] = true
	}
	return slices.Sorted(func(yield func(string) bool) {
		for k := range set {
			if !yield(k) {
				return
			}
		}
	})
}

// t7dSortOpRuns sorts each run of consecutive entries that read one store
// with one operation and one verdict: a CachingStore lists in map order, so
// a reader that reads per listed row (each root's children) reads them in an
// order that is not the run's to compare. Such a run is compared as a set.
func t7dSortOpRuns(log []string) []string {
	out := slices.Clone(log)
	key := func(entry string) string {
		fields := strings.Fields(entry)
		return fields[0] + " " + fields[1] + " " + fields[len(fields)-1]
	}
	for i := 0; i < len(out); {
		j := i + 1
		for j < len(out) && key(out[j]) == key(out[i]) {
			j++
		}
		sort.Strings(out[i:j])
		i = j
	}
	return out
}
