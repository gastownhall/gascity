package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/test/toolhome"
)

// mcShapedRig is a rig store shaped like maintainer-city's gascity-packs rig
// (design §6.1), generated from its statistics; no production content:
//
//   - 3,180 rows: 2,356 infrastructure (all graph, 1,004 of them wisps) and
//     824 work;
//   - the graph kind mix (694 scope-check, 582 task, 425 spec, 354 retry,
//     92 scope, 64 workflow, 64 workflow-finalize, 57 ralph, 14 cleanup,
//     5 drain, 5 convoy);
//   - 2,218 closed and 138 open graph rows, the open ones in 4 abandoned
//     trees (68 and 66 rows with their roots open; 2 and 2 under closed
//     roots), 28 of them open and routed;
//   - 5,640 internal edges (3,403 blocks, 2,237 tracks);
//   - 44 tracks edges from work drain-unit convoys onto drained graph rows,
//     42 closed at both ends and 2 open at both ends;
//   - 49 graph rows with no gc.root_store_ref, including the 2 open drained
//     rows;
//   - non-wisp hash suffixes of 3-5 characters;
//   - 1 work row's discovered-from edge into another rig.
type mcShapedRig struct {
	store      *beads.MemStore
	openGraph  []string // the 138 open graph rows
	openOpen   [2][2]string
	infraCount int
	unscoped   int
}

func newMCShapedRig(t *testing.T) mcShapedRig {
	t.Helper()
	rng := rand.New(rand.NewSource(20261015)) //nolint:gosec // deterministic fixture
	kinds := []struct {
		kind string
		n    int
	}{{"scope-check", 694}, {"task", 582}, {"spec", 425}, {"retry", 354}, {"scope", 92}, {"workflow", 64}, {"workflow-finalize", 64}, {"ralph", 57}, {"cleanup", 14}, {"drain", 5}, {"convoy", 5}}
	var kindList []string
	for _, k := range kinds {
		for i := 0; i < k.n; i++ {
			if k.kind != "workflow" {
				kindList = append(kindList, k.kind)
			}
		}
	}
	rng.Shuffle(len(kindList), func(i, j int) { kindList[i], kindList[j] = kindList[j], kindList[i] })
	const roots, wisps = 64, 1004
	created := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	nonWisp := 0
	nextID := func(wisp bool) string {
		if wisp {
			return fmt.Sprintf("gp-wisp-%s", strconv.FormatInt(int64(46656+len(kindList)*7+rng.Intn(1<<30)), 36))
		}
		nonWisp++
		width := 5
		switch {
		case nonWisp <= 158:
			width = 3
		case nonWisp <= 158+779:
			width = 4
		}
		s := strconv.FormatInt(int64(nonWisp)*7919, 36)
		for len(s) < width {
			s = "0" + s
		}
		return "gp-" + s[len(s)-width:]
	}
	seen := map[string]bool{}
	uniqueID := func(wisp bool) string {
		for {
			id := nextID(wisp)
			if !seen[id] {
				seen[id] = true
				return id
			}
		}
	}
	type row struct {
		bead beads.Bead
		tree int
	}
	var graph []row
	// Roots: 64 workflows, non-wisp.
	for i := 0; i < roots; i++ {
		id := uniqueID(false)
		graph = append(graph, row{tree: i, bead: beads.Bead{
			ID: id, Title: "workflow " + id, Type: "task", Status: "closed",
			Metadata: beads.StringMap{"gc.kind": "workflow", beadmeta.RootBeadIDMetadataKey: id, beadmeta.RootStoreRefMetadataKey: "rig:packs"},
		}})
	}
	graph[0].bead.Status, graph[1].bead.Status = "open", "open"
	// Members: tree 0 gets 67 open, tree 1 65 open, trees 2 and 3 two open
	// each under closed roots; the rest are closed, spread over all trees.
	members := len(kindList)
	for i := 0; i < members; i++ {
		tree, status := 4+rng.Intn(roots-4), "closed"
		switch {
		case i < 67:
			tree, status = 0, "open"
		case i < 67+65:
			tree, status = 1, "open"
		case i < 67+65+2:
			tree, status = 2, "open"
		case i < 67+65+4:
			tree, status = 3, "open"
		case i < 67+65+4+40:
			tree = 2 + rng.Intn(2) // closed history in the closed-root trees too
		}
		wisp := i >= members-wisps
		id := uniqueID(wisp)
		meta := beads.StringMap{"gc.kind": kindList[i], beadmeta.RootBeadIDMetadataKey: graph[tree].bead.ID, beadmeta.RootStoreRefMetadataKey: "rig:packs"}
		graph = append(graph, row{tree: tree, bead: beads.Bead{ID: id, Title: kindList[i] + " " + id, Type: "task", Status: status, Ephemeral: wisp, Metadata: meta}})
	}
	m := mcShapedRig{infraCount: len(graph)}
	// 28 open routed rows, in the two open trees.
	routed := 0
	for i := range graph {
		if graph[i].bead.Status == "open" && graph[i].tree < 2 && graph[i].bead.Metadata["gc.kind"] != "workflow" && routed < 28 {
			graph[i].bead.Metadata[beadmeta.RoutedToMetadataKey] = "packs/worker"
			routed++
		}
	}
	// The two open drained rows (open-open tracks targets): the open member
	// of tree 2 and of tree 3, unscoped; plus 47 closed unscoped rows.
	var drainedOpen []int
	for i := range graph {
		if graph[i].bead.Status == "open" && (graph[i].tree == 2 || graph[i].tree == 3) && graph[i].bead.Metadata["gc.kind"] != "workflow" && len(drainedOpen) < 2 {
			if len(drainedOpen) == 1 && graph[drainedOpen[0]].tree == graph[i].tree {
				continue
			}
			drainedOpen = append(drainedOpen, i)
		}
	}
	unscoped := 0
	for _, i := range drainedOpen {
		delete(graph[i].bead.Metadata, beadmeta.RootStoreRefMetadataKey)
		unscoped++
	}
	var closedMembers []int
	for i := roots; i < len(graph); i++ {
		if graph[i].bead.Status == "closed" {
			closedMembers = append(closedMembers, i)
		}
	}
	for _, i := range closedMembers[:47] {
		delete(graph[i].bead.Metadata, beadmeta.RootStoreRefMetadataKey)
		unscoped++
	}
	m.unscoped = unscoped
	// Internal edges: blocks and tracks inside a tree, never two edges on one
	// pair.
	byTree := map[int][]int{}
	for i, r := range graph {
		byTree[r.tree] = append(byTree[r.tree], i)
	}
	pairs := map[[2]string]bool{}
	var deps []beads.Dep
	addEdge := func(from, to, typ string) bool {
		if from == to || pairs[[2]string{from, to}] || pairs[[2]string{to, from}] {
			return false
		}
		pairs[[2]string{from, to}] = true
		deps = append(deps, beads.Dep{IssueID: from, DependsOnID: to, Type: typ})
		return true
	}
	treeIDs := make([]int, 0, len(byTree))
	for tree := range byTree {
		treeIDs = append(treeIDs, tree)
	}
	sort.Ints(treeIDs)
	for blocks, tracks := 0, 0; blocks < 3403 || tracks < 2237; {
		tree := treeIDs[rng.Intn(len(treeIDs))]
		rows := byTree[tree]
		if len(rows) < 3 {
			continue
		}
		a, b := graph[rows[rng.Intn(len(rows))]].bead.ID, graph[rows[rng.Intn(len(rows))]].bead.ID
		if blocks < 3403 {
			if addEdge(a, b, "blocks") {
				blocks++
			}
		} else if addEdge(a, b, "tracks") {
			tracks++
		}
	}
	// Work: 824 rows, 44 of them drain-unit convoys tracking a drained graph
	// row (2 open-open, 42 closed at both ends), and one discovered-from into
	// another rig.
	var all []beads.Bead
	for i := range graph {
		b := graph[i].bead
		b.CreatedAt = created.Add(time.Duration(i) * time.Minute)
		all = append(all, b)
		if b.Status == "open" {
			m.openGraph = append(m.openGraph, b.ID)
		}
	}
	for i := 0; i < 824; i++ {
		id := fmt.Sprintf("gp-w%04d", i)
		status := "closed"
		if i%3 == 0 {
			status = "open"
		}
		w := beads.Bead{ID: id, Title: "work " + id, Type: "task", Status: status, CreatedAt: created.Add(time.Duration(4000+i) * time.Minute)}
		switch {
		case i < 2:
			w.Status = "open"
			target := graph[drainedOpen[i]].bead.ID
			deps = append(deps, beads.Dep{IssueID: id, DependsOnID: target, Type: "tracks"})
			m.openOpen[i] = [2]string{id, target}
		case i < 44:
			w.Status = "closed"
			deps = append(deps, beads.Dep{IssueID: id, DependsOnID: graph[closedMembers[100+i]].bead.ID, Type: "tracks"})
		case i == 44:
			deps = append(deps, beads.Dep{IssueID: id, DependsOnID: "ga-9m4nyy", Type: "discovered-from"})
		}
		all = append(all, w)
	}
	m.store = beads.NewMemStoreFrom(0, all, deps)
	m.store.IDPrefix = "gp"
	return m
}

// TestRigImportMCShapedFixture runs the importer over the gascity-packs
// shape: carried live, the 2 open-open tracks edges refuse and nothing is
// written; after the 4 abandoned trees are closed (owner decision D1), it
// imports 2,356 rows with 0 gates and 49 scope annotations, clears them from
// the rig and leaves its 824 work rows.
func TestRigImportMCShapedFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("MC-shaped fixture: 3,180 rows into a real SQLite binding")
	}
	mc := newMCShapedRig(t)
	if mc.infraCount != 2356 || mc.unscoped != 49 || len(mc.openGraph) != 138 {
		t.Fatalf("fixture shape: %d infra, %d unscoped, %d open; want 2356, 49, 138", mc.infraCount, mc.unscoped, len(mc.openGraph))
	}
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp", Suspended: true})
	f.fixed["packs"] = mc.store

	start := time.Now()
	code, stdout, stderr := f.importRigs()
	t.Logf("carried live: refused in %s", time.Since(start).Round(time.Millisecond))
	if code == 0 {
		t.Fatalf("the open-open tracks edges did not refuse: %s", stdout)
	}
	if !strings.Contains(stderr, "2 shape(s)") {
		t.Errorf("want exactly 2 refusals: %s", stderr)
	}
	for _, pair := range mc.openOpen {
		if !strings.Contains(stderr, pair[0]+", "+pair[1]) {
			t.Errorf("the refusal does not name %s -> %s: %s", pair[0], pair[1], stderr)
		}
	}
	if n := len(rigTestList(t, f.binding())); n != 0 {
		t.Fatalf("a refused import wrote %d binding rows", n)
	}

	// D1: cancel the abandoned trees first.
	for _, id := range mc.openGraph {
		if err := mc.store.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	start = time.Now()
	code, stdout, stderr = f.importRigs()
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("import after closing the trees = %d: %s %s", code, stdout, stderr)
	}
	t.Logf("imported in %s: %s", elapsed.Round(time.Millisecond), strings.TrimSpace(stdout))
	for _, want := range []string{"imported 2356 bead(s) and 5640 edge(s)", "0 cross-store gate(s) minted", "49 scope annotation(s)", "2356 rig cop(ies) cleared", "rig min_hash_length 7"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report lacks %q: %s", want, stdout)
		}
	}
	// The fixture's non-wisp suffixes are 3-5 characters and its wisp
	// suffixes up to 6, so the floor is 7 (bd sizes wisp ids too).
	if got := f.hashConf["packs"].values[bdMinHashLengthKey]; got != "7" {
		t.Errorf("min_hash_length = %q, want 7", got)
	}
	rest := rigTestList(t, mc.store)
	if len(rest) != 824 {
		t.Errorf("the rig holds %d rows after the import, want its 824 work rows", len(rest))
	}
	binding := f.binding()
	gates, _ := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if len(gates) != 0 {
		t.Errorf("gates = %d, want 0", len(gates))
	}
	stamped, err := binding.List(beads.ListQuery{Metadata: map[string]string{infraMigrationStampKey: "rig:packs"}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil || len(stamped) != 2356 {
		t.Fatalf("stamped binding rows = %d (%v), want 2356", len(stamped), err)
	}
	for _, b := range stamped {
		if b.Metadata[beadmeta.RootStoreRefMetadataKey] == "" {
			t.Fatalf("%s carries no gc.root_store_ref in the binding", b.ID)
		}
	}
	manifest, _, _ := f.loc("packs").readManifest()
	if manifest == nil || manifest.Len() != 2356 || len(manifest.ScopeAnnotated) != 49 {
		t.Errorf("manifest = %v", manifest)
	}
	start = time.Now()
	if code, stdout, _ := f.importRigs(); code != 0 || !strings.Contains(stdout, "nothing to import") {
		t.Errorf("the re-run = %d %q", code, stdout)
	}
	t.Logf("idempotent re-run in %s", time.Since(start).Round(time.Millisecond))
	if !f.cfg.Rigs[0].Suspended {
		t.Error("the import resumed the suspended rig")
	}
}

// TestRigImportFromARealDoltRig runs the importer against a native Dolt rig
// store (embedded Dolt, the engine bd stores rows in): DepListBatch,
// DepMetadata and the chunked DeleteBatch are the real backend's.
func TestRigImportFromARealDoltRig(t *testing.T) {
	if testing.Short() {
		t.Skip("real Dolt rig")
	}
	rig := newRealDoltRigStore(t)
	mk := func(b beads.Bead, closed bool) {
		t.Helper()
		seedRealDoltRow(t, rig, b, closed)
	}
	mk(graphRow("gd-root", "gd-root", "", map[string]string{"gc.kind": "workflow", beadmeta.RootStoreRefMetadataKey: "rig:dolt"}), true)
	for i := 0; i < 6; i++ {
		mk(graphRow(fmt.Sprintf("gd-s%d", i), "gd-root", "", nil), true)
	}
	mk(graphRow("gd-open", "gd-root", "", nil), false)
	mk(workRow("gd-w1", ""), false)
	for i := 1; i < 6; i++ {
		if err := rig.DepAdd(fmt.Sprintf("gd-s%d", i), fmt.Sprintf("gd-s%d", i-1), "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	if err := rig.DepAdd("gd-open", "gd-w1", "blocks"); err != nil { // open D -> open W: a gate
		t.Fatal(err)
	}

	f := newRigImportFixture(t, config.Rig{Name: "dolt", Prefix: "gd"})
	f.fixed["dolt"] = nonClosingStore{rig}
	start := time.Now()
	code, stdout, stderr := f.importRigs()
	if code != 0 {
		t.Fatalf("import from a real Dolt rig = %d: %s %s", code, stdout, stderr)
	}
	t.Logf("real Dolt rig imported in %s: %s", time.Since(start).Round(time.Millisecond), strings.TrimSpace(stdout))
	if !strings.Contains(stdout, "imported 8 bead(s) and 5 edge(s)") || !strings.Contains(stdout, "1 cross-store gate(s) minted") {
		t.Errorf("report = %s", stdout)
	}
	left := rigTestList(t, rig)
	if len(left) != 1 || left[0].ID != "gd-w1" {
		t.Errorf("the Dolt rig holds %v after the import, want only gd-w1", rigTestIDs(left))
	}
	binding := f.binding()
	deps, err := binding.DepList("gd-s5", "down")
	if err != nil || len(deps) != 1 || deps[0].DependsOnID != "gd-s4" {
		t.Errorf("the chain edge did not cross: %v %v", deps, err)
	}
	assertNoCrossStoreEdge(t, binding)
}

// newRealDoltRigStore stands up a native Dolt rig store (embedded Dolt, the
// engine bd stores rows in) with the id prefix gd, skipping the test where
// embedded Dolt is unavailable.
func newRealDoltRigStore(t *testing.T) *beads.NativeDoltStore {
	t.Helper()
	ctx := context.Background()
	scopeRoot := t.TempDir()
	seed, err := beadslib.OpenBestAvailable(ctx, filepath.Join(scopeRoot, ".beads"))
	if err != nil {
		t.Skipf("embedded Dolt unavailable: %v", err)
	}
	if err := seed.SetConfig(ctx, "issue_prefix", "gd"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	rig, err := beads.OpenNativeDoltStoreAt(ctx, scopeRoot, nil)
	if err != nil {
		t.Skipf("native Dolt store unavailable: %v", err)
	}
	t.Cleanup(func() { _ = rig.CloseStore() })
	return rig
}

// seedRealDoltRow writes b into a native Dolt rig under its own id, closing
// it when closed is set.
func seedRealDoltRow(t *testing.T, rig *beads.NativeDoltStore, b beads.Bead, closed bool) {
	t.Helper()
	if _, err := beads.ForeignIDCreator(rig).CreateWithForeignID(b); err != nil {
		t.Fatalf("seeding %s: %v", b.ID, err)
	}
	if closed {
		if err := rig.Close(b.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// nonClosingStore keeps the test's own handle on a real store open across the
// importer's close.
type nonClosingStore struct{ beads.Store }

func (nonClosingStore) CloseStore() error { return nil }

// DepListBatch, DepMetadata and DeleteBatch forward the real backend's
// capabilities, which the embedded interface would otherwise hide.
func (s nonClosingStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	if batch, ok := beads.DepListBatchFor(s.Store); ok {
		return batch.DepListBatch(ids)
	}
	return nil, beads.ErrDepListBatchUnsupported
}

func (s nonClosingStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	return s.Store.(beads.DepMetadataReader).DepMetadata(issueID, dependsOnID)
}

func (s nonClosingStore) DeleteBatch(ids []string) error {
	if batch, ok := s.Store.(beads.BatchDeleter); ok {
		return batch.DeleteBatch(ids)
	}
	return beads.ErrBatchDeleteUnsupported
}

// TestRigImportFromARealBdRigRaisesItsMinHashLength runs the importer over a
// real bd scope (the bd CLI over embedded Dolt): bd mints the hashed ids, the
// import moves the graph rows out through `bd delete --force` in batches,
// and `bd config` then reads the raised min_hash_length, so a short id bd
// mints after the clear cannot re-mint an imported one (the mc-wisp-jkl
// defect moved to rigs). bd's dependency payloads are not readable through
// the CLI, so this rig carries no edges; the native Dolt test covers them.
func TestRigImportFromARealBdRigRaisesItsMinHashLength(t *testing.T) {
	if testing.Short() {
		t.Skip("real bd rig")
	}
	rig, dir := newRealBdRigStore(t)
	var graphIDs []string
	root, err := rig.Create(beads.Bead{Title: "a pre-S4 workflow", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow", beadmeta.RootBeadIDMetadataKey: "self"}})
	if err != nil {
		t.Fatalf("bd create: %v", err)
	}
	graphIDs = append(graphIDs, root.ID)
	for i := 0; i < 3; i++ {
		step, err := rig.Create(beads.Bead{Title: fmt.Sprintf("step %d", i), Type: "task", Metadata: beads.StringMap{"gc.kind": "task", beadmeta.RootBeadIDMetadataKey: root.ID}})
		if err != nil {
			t.Fatalf("bd create: %v", err)
		}
		if err := rig.Close(step.ID); err != nil {
			t.Fatal(err)
		}
		graphIDs = append(graphIDs, step.ID)
	}
	work, err := rig.Create(beads.Bead{Title: "ordinary work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	f := newRigImportFixture(t, config.Rig{Name: "bdrig", Prefix: "tst", Path: dir})
	f.fixed["bdrig"] = rig
	prevHash := rigImportHashConfigOpener
	rigImportHashConfigOpener = func(string, *config.City, config.Rig) (rigHashLengthConfig, error) { return rig, nil }
	t.Cleanup(func() { rigImportHashConfigOpener = prevHash })

	code, stdout, stderr := f.importRigs()
	if code != 0 {
		t.Fatalf("import from a real bd rig = %d: %s %s", code, stdout, stderr)
	}
	t.Logf("real bd rig: %s", strings.TrimSpace(stdout))
	left := rigTestList(t, rig)
	if len(left) != 1 || left[0].ID != work.ID {
		t.Errorf("the bd rig holds %v after the import, want only %s", rigTestIDs(left), work.ID)
	}
	want := strconv.Itoa(rigImportHashFloor(graphIDs))
	got, err := rig.ConfigGet(bdMinHashLengthKey)
	if err != nil || strings.TrimSpace(got) != want {
		t.Errorf("bd config get min_hash_length = %q (%v), want %s", got, err, want)
	}
	for _, id := range graphIDs {
		if _, err := f.binding().Get(id); err != nil {
			t.Errorf("the binding does not hold %s: %v", id, err)
		}
	}
}

// newRealBdRigStore stands up a real bd scope (git init + bd init, embedded
// Dolt) with its environment pinned to the scope, so no ambient BEADS_DIR or
// shared Dolt server can be reached.
func newRealBdRigStore(t *testing.T) (*beads.BdStore, string) {
	t.Helper()
	dir := beadstest.GuardedTempDir(t)
	git := exec.Command("git", "init", "--quiet", dir)
	var env []string
	for _, kv := range git.Environ() {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=") {
			continue
		}
		env = append(env, kv)
	}
	git.Env = env
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	home := filepath.Join(filepath.Dir(dir), "tool-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	for _, kv := range toolhome.Environ(os.Environ(), home) {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	vars["BEADS_DIR"] = filepath.Join(dir, ".beads")
	vars["BEADS_DOLT_AUTO_START"] = "0"
	exact := beads.ExecCommandRunnerWithExactEnvContext(context.Background(), beadstest.BdSubprocessEnv(vars))
	realBD := realBDPath(t)
	runner := func(d, name string, args ...string) ([]byte, error) {
		if name == "bd" {
			name = realBD
		}
		return exact(d, name, args...)
	}
	if out, err := runner(dir, "bd", "init", "-p", "tst", "--skip-hooks", "--skip-agents"); err != nil {
		t.Skipf("bd init failed (bd broken?): %v\n%s", err, out)
	}
	return beads.NewBdStore(dir, runner), dir
}

// realBDPath finds the real bd binary on PATH, past the provider stubs this
// package's TestMain puts first.
func realBDPath(t *testing.T) string {
	t.Helper()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		candidate := filepath.Join(dir, "bd")
		if info, err := os.Stat(candidate); err != nil || info.IsDir() {
			continue
		}
		out, err := exec.Command(candidate, "version").CombinedOutput() //nolint:gosec // a test locating its own tool
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "bd version") {
			return candidate
		}
	}
	t.Skip("no real bd on PATH")
	return ""
}
