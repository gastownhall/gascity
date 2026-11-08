package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// reverseRigFixture is a split city with a rig whose graph rows a S4b import
// moved into the binding: the state --to-rigs and a rig's half of --to-work
// start from.
type reverseRigFixture struct {
	*rigImportFixture
	// seeded is the rig as it was before its import: rows by id and edges.
	seeded      map[string]beads.Bead
	seededEdges []string
}

func newReverseRigFixture(t *testing.T) *reverseRigFixture {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "other", Prefix: "go"})
	root := graphRow("gp-root1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs", "gc.kind": "workflow"})
	step := graphRow("gp-step1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"})
	unscoped := graphRow("gp-step2", "gp-root1", "closed", nil) // the import annotates its scope
	wisp := graphRow("gp-wisp-a1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"})
	wisp.Ephemeral = true
	open := graphRow("gp-d1", "gp-d1", "open", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"})
	f.seed("packs", []beads.Bead{root, step, unscoped, wisp, open, workRow("gp-w1", "closed"), workRow("gp-w2", "closed"), workRow("gp-w3", "open")}, []beads.Dep{
		{IssueID: "gp-step1", DependsOnID: "gp-root1", Type: "parent-child"},
		{IssueID: "gp-step2", DependsOnID: "gp-step1", Type: "blocks"},
		{IssueID: "gp-d1", DependsOnID: "gp-w1", Type: "blocks"},    // dropped (W closed), recorded on gp-d1
		{IssueID: "gp-w2", DependsOnID: "gp-step1", Type: "tracks"}, // dropped (W closed), recorded on gp-step1
		{IssueID: "gp-w3", DependsOnID: "gp-w1", Type: "related"},   // work to work: never moves
	})
	f.seed("other", []beads.Bead{workRow("go-w1", "open")}, nil)
	// GC_BEADS=file names a file store for the city too, whose exact
	// snapshot copies its file; its rows come from the stubbed work store.
	if err := os.WriteFile(filepath.Join(f.request.CityPath, ".gc", "beads.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &reverseRigFixture{rigImportFixture: f, seeded: map[string]beads.Bead{}}
	for _, b := range rigTestList(t, f.rig("packs")) {
		r.seeded[b.ID] = b
	}
	r.seededEdges = rigEdgeFingerprint(t, f.rig("packs"))
	if code, stdout, stderr := f.importRigs("packs"); code != 0 {
		t.Fatalf("the import = %d: %s%s", code, stdout, stderr)
	}
	return r
}

// rigEdgeFingerprint is every edge a store holds, sorted.
func rigEdgeFingerprint(t *testing.T, store beads.Store) []string {
	t.Helper()
	var out []string
	for _, b := range rigTestList(t, store) {
		deps, err := store.DepList(b.ID, "down")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range deps {
			out = append(out, fmt.Sprintf("%s -[%s]-> %s", b.ID, d.Type, d.DependsOnID))
		}
	}
	sort.Strings(out)
	return out
}

func (r *reverseRigFixture) reverse(t *testing.T, opts reverseOptions) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := doStorageReverse(context.Background(), r.request, opts, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// assertRigAsSeeded proves the rig holds every row it held before its
// import, byte-equal modulo the migrations' stamps and the scope the import
// annotated, with exactly its edges.
func (r *reverseRigFixture) assertRigAsSeeded(t *testing.T) {
	t.Helper()
	rig := r.rig("packs")
	got := map[string]beads.Bead{}
	for _, b := range rigTestList(t, rig) {
		got[b.ID] = b
	}
	if len(got) != len(r.seeded) {
		t.Fatalf("the rig holds %v, want %v", sortedMapKeys(got), sortedMapKeys(r.seeded))
	}
	for id, want := range r.seeded {
		g, ok := got[id]
		if !ok {
			t.Fatalf("the rig lost %s", id)
		}
		if diff := beadCopyDifference(withoutReverseStamps(want), withoutReverseStamps(g)); diff != "" {
			t.Fatalf("%s differs from the row the rig held before its import: %s", id, diff)
		}
		if _, scoped := g.Metadata[beadmeta.RootStoreRefMetadataKey]; scoped != (want.Metadata[beadmeta.RootStoreRefMetadataKey] != "") {
			t.Fatalf("%s carries gc.root_store_ref = %v, want %v (the import's annotation comes off)", id, scoped, !scoped)
		}
	}
	if edges := rigEdgeFingerprint(t, rig); strings.Join(edges, "\n") != strings.Join(r.seededEdges, "\n") {
		t.Fatalf("the rig's edges after the reverse:\n%s\nwant:\n%s", strings.Join(edges, "\n"), strings.Join(r.seededEdges, "\n"))
	}
}

// TestReverseToRigsRoundTripsARigImport: import then --to-rigs puts the rig
// back as it was, edges dropped at the boundary included; the binding keeps
// none of its rows; the rig reads as awaiting import and boot serves the
// split. Then the rig imports again (reverse then re-import), into the
// binding rows the first import wrote, modulo stamps.
func TestReverseToRigsRoundTripsARigImport(t *testing.T) {
	r := newReverseRigFixture(t)
	firstImport := map[string]beads.Bead{}
	for _, b := range rigTestList(t, r.binding()) {
		firstImport[b.ID] = b
	}
	code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	if code != 0 {
		t.Fatalf("--to-rigs = %d: %s%s", code, stdout, stderr)
	}
	r.assertRigAsSeeded(t)
	for _, b := range rigTestList(t, r.binding()) {
		if b.Metadata[infraMigrationStampKey] == rigImportStamp("packs") {
			t.Fatalf("the binding still holds %s from rig packs", b.ID)
		}
	}
	loc := r.loc("packs")
	for _, path := range []string{loc.MarkerPath(), loc.ManifestPath(), loc.BackupPath(), loc.NotePath()} {
		if fileExists(path) {
			t.Fatalf("%s was not retired", path)
		}
	}
	rows := censusRigs(r.request.CityPath, r.cfg, r.target, false)
	if rows[0].Rig != "packs" || rows[0].State != rigCensusAwaiting {
		t.Fatalf("the rig's census after the reverse = %+v, want awaiting import", rows[0])
	}
	if _, err := storageBootGate(r.request.CityPath, r.cfg, "gc start", nil, io.Discard); err != nil {
		t.Fatalf("boot after --to-rigs = %v, want the split served", err)
	}
	journal, _, _ := readStorageReverseJournal(r.request.CityPath)
	if journal.Phase != reverseComplete {
		t.Fatalf("journal phase %d, want complete", journal.Phase)
	}
	// The snapshot from before the reverse no longer restores: its records
	// name the rig's import, and the rig's rows are in the rig now.
	var out, errOut bytes.Buffer
	if code := doStorageSnapshotRestore(context.Background(), r.request, journal.SnapshotTo, journal.Snapshot, "restore", &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "records have changed") {
		t.Fatalf("restoring the pre-reverse snapshot = %d: %s", code, errOut.String())
	}

	code, stdout, stderr = r.importRigs("packs")
	if code != 0 {
		t.Fatalf("the re-import = %d: %s%s", code, stdout, stderr)
	}
	second := map[string]beads.Bead{}
	for _, b := range rigTestList(t, r.binding()) {
		second[b.ID] = b
	}
	if len(second) != len(firstImport) {
		t.Fatalf("the re-import left %v in the binding, the first import %v", sortedMapKeys(second), sortedMapKeys(firstImport))
	}
	for id, want := range firstImport {
		got, ok := second[id]
		if !ok {
			t.Fatalf("the re-import did not bring back %s", id)
		}
		if diff := beadCopyDifference(withoutReverseStamps(want), withoutReverseStamps(got)); diff != "" {
			t.Fatalf("%s differs between the two imports: %s", id, diff)
		}
	}
	if got := rigTestIDs(rigTestList(t, r.rig("packs"))); strings.Join(got, ",") != "gp-w1,gp-w2,gp-w3" {
		t.Fatalf("after the re-import the rig holds %v, want only its work", got)
	}
}

// TestReverseToRigsResumesFromEveryBoundary kills --to-rigs at every
// boundary: boot refuses while the journal is open, and the next run
// finishes it without loss.
func TestReverseToRigsResumesFromEveryBoundary(t *testing.T) {
	var points []string
	{
		r := newReverseRigFixture(t)
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error { points = append(points, p); return nil }
		code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		infraEngineCrashPoint = prev
		if code != 0 {
			t.Fatalf("dry run = %d: %s%s", code, stdout, stderr)
		}
	}
	for _, want := range []string{"reverse rig:packs prepare", "reverse rig:packs verify", "reverse R4 cleared"} {
		found := false
		for _, p := range points {
			found = found || strings.HasPrefix(p, want)
		}
		if !found {
			t.Fatalf("--to-rigs passes no %q boundary: %v", want, points)
		}
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			r := newReverseRigFixture(t)
			prev := infraEngineCrashPoint
			infraEngineCrashPoint = func(p string) error {
				if p == point {
					return errors.New("killed")
				}
				return nil
			}
			code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
			infraEngineCrashPoint = prev
			if code == 0 {
				t.Fatalf("the kill at %s did not stop the run: %s", point, stderr)
			}
			if point != "reverse phase "+reversePhaseName(reverseComplete) {
				if _, err := storageBootGate(r.request.CityPath, r.cfg, "gc start", nil, io.Discard); err == nil {
					t.Fatalf("boot served the city after a kill at %s", point)
				}
				if code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}); code != 0 {
					t.Fatalf("the run after a kill at %s = %d: %s%s", point, code, stdout, stderr)
				}
			}
			r.assertRigAsSeeded(t)
			if _, err := storageBootGate(r.request.CityPath, r.cfg, "gc start", nil, io.Discard); err != nil {
				t.Fatalf("boot after finishing = %v", err)
			}
		})
	}
}

// TestReverseToRigsAbortRemovesOnlyItsRows: an abandoned --to-rigs leaves the
// rig and the binding as the import left them.
func TestReverseToRigsAbortRemovesOnlyItsRows(t *testing.T) {
	r := newReverseRigFixture(t)
	rigBefore := workStoreFingerprint(t, r.rig("packs"))
	bindingBefore := workStoreFingerprint(t, r.binding())
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse rig:packs verify" {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the reverse")
	}
	if workStoreFingerprint(t, r.rig("packs")) == rigBefore {
		t.Fatal("the killed reverse wrote nothing to the rig, so the abort proves nothing")
	}
	var stdout, stderr bytes.Buffer
	if code := doStorageAbortReverse(context.Background(), r.request, &stdout, &stderr); code != 0 {
		t.Fatalf("--abort-reverse = %d: %s%s", code, stdout.String(), stderr.String())
	}
	if got := workStoreFingerprint(t, r.rig("packs")); got != rigBefore {
		t.Fatalf("the rig after the abort:\n%s\nwant:\n%s", got, rigBefore)
	}
	if got := workStoreFingerprint(t, r.binding()); got != bindingBefore {
		t.Fatal("the abort changed the binding")
	}
	if !fileExists(r.loc("packs").MarkerPath()) {
		t.Fatal("the abort lost the rig's import marker")
	}
	if _, err := storageBootGate(r.request.CityPath, r.cfg, "gc start", nil, io.Discard); err != nil {
		t.Fatalf("boot after the abort = %v", err)
	}
}

// TestReverseToWorkReturnsARigImport: --to-work returns the rig's imported
// rows to the rig (S4c inside the whole reverse) and retires the city's split.
func TestReverseToWorkReturnsARigImport(t *testing.T) {
	r := newReverseRigFixture(t)
	toml := reverseCityTOMLText("", r.cfg.Rigs...)
	toml = strings.Replace(toml, `path = ".gc/store"`, fmt.Sprintf("path = %q", r.cfg.Storage.Bindings["infra"].Path), 1)
	if err := os.WriteFile(filepath.Join(r.request.CityPath, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToWork})
	if code != 0 {
		t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
	}
	r.assertRigAsSeeded(t)
	if fileExists(r.target.Root) || fileExists(r.loc("packs").MarkerPath()) {
		t.Fatal("the binding root or the rig's records were not retired")
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(r.request.CityPath, io.Discard)
	if err != nil || cfg.Storage != nil {
		t.Fatalf("the city after --to-work: storage %v, %v", cfg.Storage, err)
	}
	if _, err := storageBootGate(r.request.CityPath, cfg, "gc start", nil, io.Discard); err != nil {
		t.Fatalf("boot on the reverted city = %v", err)
	}
}

// TestReverseToRigsIntoARealDoltRig returns an import to a native Dolt rig
// store (embedded Dolt, the engine bd stores rows in): the batch create, one
// commit per call, and the store's created form (bd routes its infra types to
// the wisp tables) are the real backend's, and the rig ends as it was seeded.
func TestReverseToRigsIntoARealDoltRig(t *testing.T) {
	if testing.Short() {
		t.Skip("real Dolt rig")
	}
	clearGCEnv(t)
	// The embedded store takes no Dolt tag, so the snapshot falls back to bd
	// export, which this test stands in for.
	prevExport := snapshotBdExport
	snapshotBdExport = func(_ string, _ *config.City, _, out string) error { return os.WriteFile(out, []byte("{}\n"), 0o644) }
	t.Cleanup(func() { snapshotBdExport = prevExport })
	rig := newRealDoltRigStore(t)
	mk := func(b beads.Bead, closed bool) {
		t.Helper()
		seedRealDoltRow(t, rig, b, closed)
	}
	mk(graphRow("gd-root", "gd-root", "", map[string]string{"gc.kind": "workflow", beadmeta.RootStoreRefMetadataKey: "rig:dolt"}), true)
	for i := 0; i < 6; i++ {
		mk(graphRow(fmt.Sprintf("gd-s%d", i), "gd-root", "", nil), true)
	}
	mk(beads.Bead{ID: "gd-m1", Title: "mail", Type: "message", Assignee: "mayor"}, false)
	mk(workRow("gd-w1", ""), true)
	for i := 1; i < 6; i++ {
		if err := rig.DepAdd(fmt.Sprintf("gd-s%d", i), fmt.Sprintf("gd-s%d", i-1), "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	if err := rig.DepAdd("gd-s0", "gd-w1", "blocks"); err != nil { // closed D -> closed W: dropped, recorded
		t.Fatal(err)
	}
	seeded := map[string]beads.Bead{}
	for _, b := range rigTestList(t, rig) {
		seeded[b.ID] = b
	}
	seededEdges := rigEdgeFingerprint(t, rig)

	f := newRigImportFixture(t, config.Rig{Name: "dolt", Prefix: "gd"})
	f.fixed["dolt"] = borrowedStoreWithBeadPolicies(rig, f.cfg)
	if err := os.WriteFile(filepath.Join(f.request.CityPath, ".gc", "beads.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s%s", code, stdout, stderr)
	}
	start := time.Now()
	var stdout, stderr bytes.Buffer
	if code := doStorageReverse(context.Background(), f.request, reverseOptions{Mode: reverseToRigs, Rigs: []string{"dolt"}}, &stdout, &stderr); code != 0 {
		t.Fatalf("--to-rigs into a real Dolt rig = %d: %s%s", code, stdout.String(), stderr.String())
	}
	t.Logf("--to-rigs into a real Dolt rig in %s", time.Since(start).Round(time.Millisecond))
	got := map[string]beads.Bead{}
	for _, b := range rigTestList(t, rig) {
		got[b.ID] = b
	}
	if len(got) != len(seeded) {
		t.Fatalf("the Dolt rig holds %v, want %v", sortedMapKeys(got), sortedMapKeys(seeded))
	}
	for id, want := range seeded {
		w, g := withoutReverseStamps(want), withoutReverseStamps(got[id])
		w.UpdatedAt, g.UpdatedAt = w.CreatedAt, g.CreatedAt // Dolt stamps its own
		if diff := beadCopyDifference(w, g); diff != "" {
			t.Fatalf("%s differs from the row seeded in the Dolt rig: %s", id, diff)
		}
	}
	if edges := rigEdgeFingerprint(t, rig); strings.Join(edges, "\n") != strings.Join(seededEdges, "\n") {
		t.Fatalf("the Dolt rig's edges:\n%s\nwant:\n%s", strings.Join(edges, "\n"), strings.Join(seededEdges, "\n"))
	}
}

// TestReverseRefusesARigImportNotFinished: a rig whose import is in flight or
// whose clear is pending may hold its rows in both stores, so a reverse
// refuses until the import finishes.
func TestReverseRefusesARigImportNotFinished(t *testing.T) {
	r := newReverseRigFixture(t)
	if err := writeInfraClearedNoteAt(r.loc("packs").NotePath(), infraClearedNote{Binding: r.target.Binding, Pending: []string{"gp-root1"}}); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	if code == 0 || !strings.Contains(stderr, "the rig's import has not finished") || !strings.Contains(stderr, rigImportInstruction("packs")) {
		t.Fatalf("--to-rigs over an unfinished import = %d: %s", code, stderr)
	}
}

// TestReverseToRigsClearRefusesARowItDidNotReturn: the binding clear removes
// only rows the reverse manifest records (and the rig's closed gates); a row
// stamped as the rig's import that appeared since refuses, and nothing goes.
// Once the clear has begun, --abort-reverse refuses.
func TestReverseToRigsClearRefusesARowItDidNotReturn(t *testing.T) {
	r := newReverseRigFixture(t)
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse phase "+reversePhaseName(reverseR3) {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the reverse")
	}
	binding, err := openInfraDestination(r.target)
	if err != nil {
		t.Fatal(err)
	}
	stray := graphRow("gp-stray", "gp-root1", "closed", map[string]string{infraMigrationStampKey: rigImportStamp("packs")})
	if _, err := binding.(beads.ForeignIDCreator).CreateWithForeignID(stray); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	before := workStoreFingerprint(t, r.binding())
	// The binding's re-proof (S5 §2.3) sees the stray first and puts the
	// reverse back to R0.
	code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	if code == 0 || !strings.Contains(stderr, "the binding changed after its rows were copied") {
		t.Fatalf("the clear over a changed binding = %d: %s", code, stderr)
	}
	if workStoreFingerprint(t, r.binding()) != before {
		t.Fatal("the refused clear removed rows from the binding")
	}
	if journal, _, _ := readStorageReverseJournal(r.request.CityPath); journal.Phase != reverseR0 {
		t.Fatalf("the reproof left the journal at phase %d, want R0", journal.Phase)
	}
	// Behind the re-proof, the clear refuses a row it did not return as well.
	journal, _, _ := readStorageReverseJournal(r.request.CityPath)
	read, err := readReverseBinding(r.target)
	if err != nil {
		t.Fatal(err)
	}
	journal.Phase, journal.BindingSHA256 = reverseR3, read.fingerprint()
	if err := writeStorageReverseJournal(r.request.CityPath, journal); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	if code == 0 || !strings.Contains(stderr, "gp-stray") || !strings.Contains(stderr, "nothing was removed") {
		t.Fatalf("the clear over a stray row = %d: %s", code, stderr)
	}
	if workStoreFingerprint(t, r.binding()) != before {
		t.Fatal("the refused clear removed rows from the binding")
	}

	r2 := newReverseRigFixture(t)
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse R4 cleared" {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ = r2.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the reverse")
	}
	var stdout, errOut bytes.Buffer
	if code := doStorageAbortReverse(context.Background(), r2.request, &stdout, &errOut); code == 0 || !strings.Contains(errOut.String(), "can only be finished") {
		t.Fatalf("--abort-reverse after the binding clear began = %d: %s", code, errOut.String())
	}
}

// TestReverseToWorkIntoARealDoltWorkStore (MUST 4): a city whose work store
// is native Dolt (embedded Dolt, the engine bd stores rows in) migrates
// forward and back. The binding minted a retry bead after the cutover, with
// a parent and no parent-child edge, and a step ref: Dolt holds the parent as
// that edge, so the reverse writes it, and keeps the ref in gc.step_ref.
func TestReverseToWorkIntoARealDoltWorkStore(t *testing.T) {
	if testing.Short() {
		t.Skip("real Dolt work store")
	}
	clearGCEnv(t)
	prevExport := snapshotBdExport
	snapshotBdExport = func(_ string, _ *config.City, _, out string) error { return os.WriteFile(out, []byte("{}\n"), 0o644) }
	t.Cleanup(func() { snapshotBdExport = prevExport })
	work := newRealDoltRigStore(t)
	mk := func(b beads.Bead, closed bool) {
		t.Helper()
		seedRealDoltRow(t, work, b, closed)
	}
	mk(graphRow("gd-root", "gd-root", "", map[string]string{"gc.kind": "workflow"}), false)
	for i := 0; i < 4; i++ {
		mk(graphRow(fmt.Sprintf("gd-s%d", i), "gd-root", "", nil), i < 2)
		if err := work.DepAdd(fmt.Sprintf("gd-s%d", i), "gd-root", "parent-child"); err != nil {
			t.Fatal(err)
		}
	}
	mk(beads.Bead{ID: "gd-m1", Title: "mail", Type: "message", Assignee: "mayor"}, false)
	mk(workRow("gd-w1", ""), false)
	seeded := map[string]beads.Bead{}
	for _, b := range rigTestList(t, work) {
		seeded[b.ID] = b
	}
	seededEdges := rigEdgeFingerprint(t, work)

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(reverseCityTOMLText("")), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return borrowedStoreWithBeadPolicies(work, cfg), nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	stubInfraControllerPing(t, 0)
	var log bytes.Buffer
	if report := migrateInfraClasses(t, cityPath, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("forward from Dolt = %v: %s", report.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	binding, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	// Pinned at x.6 s: Dolt keeps whole seconds and rounds a fraction it is
	// handed, so only a write that truncates first stores the second the
	// binding's row reads as.
	created := time.Date(2026, 8, 1, 12, 0, 5, 600_000_000, time.UTC)
	deferred := created.Add(time.Hour)
	retry, err := binding.Create(beads.Bead{Title: "retry of s2", Type: "task", ParentID: "gd-s2", Ref: "step-s2", CreatedAt: created, DeferUntil: &deferred, Metadata: beads.StringMap{beadmeta.RootBeadIDMetadataKey: "gd-root"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)

	start := time.Now()
	var stdout, stderr bytes.Buffer
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
	if code := doStorageReverse(context.Background(), request, reverseOptions{Mode: reverseToWork}, &stdout, &stderr); code != 0 {
		t.Fatalf("--to-work into a real Dolt work store = %d: %s%s", code, stdout.String(), stderr.String())
	}
	t.Logf("--to-work into a real Dolt work store in %s", time.Since(start).Round(time.Millisecond))
	if !strings.Contains(stdout.String(), "1 row(s) carry a step ref") {
		t.Fatalf("the reverse did not count the kept ref: %s", stdout.String())
	}
	got, err := work.Get(retry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Unix() != created.Unix() || got.DeferUntil == nil || got.DeferUntil.Unix() != deferred.Unix() {
		t.Fatalf("the retry bead's times in Dolt = %v, %v, want the seconds of %v, %v", got.CreatedAt, got.DeferUntil, created, deferred)
	}
	if got.ParentID != "gd-s2" || got.Metadata[beadmeta.StepRefMetadataKey] != "step-s2" {
		t.Fatalf("the retry bead in Dolt = parent %q, step_ref %q", got.ParentID, got.Metadata[beadmeta.StepRefMetadataKey])
	}
	for id, want := range seeded {
		g, err := work.Get(id)
		if err != nil {
			t.Fatalf("the Dolt work store lost %s: %v", id, err)
		}
		w, gg := withoutReverseStamps(want), withoutReverseStamps(g)
		w.UpdatedAt, gg.UpdatedAt = w.CreatedAt, g.CreatedAt
		if diff := beadCopyDifference(w, gg); diff != "" {
			t.Fatalf("%s differs from the row the Dolt work store held: %s", id, diff)
		}
	}
	want := append(append([]string(nil), seededEdges...), fmt.Sprintf("%s -[parent-child]-> gd-s2", retry.ID))
	sort.Strings(want)
	if edges := rigEdgeFingerprint(t, work); strings.Join(edges, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the Dolt work store's edges:\n%s\nwant:\n%s", strings.Join(edges, "\n"), strings.Join(want, "\n"))
	}
}

// TestReverseToRigsReListsTheRigBeforeTheClear (SHOULD 9): a row the rig no
// longer holds under the run's stamp keeps the binding's copies, puts the
// reverse back to R0, and the next run copies it again.
func TestReverseToRigsReListsTheRigBeforeTheClear(t *testing.T) {
	r := newReverseRigFixture(t)
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse phase "+reversePhaseName(reverseR3) {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the reverse")
	}
	if err := r.rig("packs").Delete("gp-step1"); err != nil {
		t.Fatal(err)
	}
	before := workStoreFingerprint(t, r.binding())
	code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	if code == 0 || !strings.Contains(stderr, "no longer hold 1 row(s)") || !strings.Contains(stderr, "gp-step1") {
		t.Fatalf("a clear over a rig that lost a row = %d: %s", code, stderr)
	}
	if workStoreFingerprint(t, r.binding()) != before {
		t.Fatal("the binding lost rows while the rig did not hold them")
	}
	if code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}); code != 0 {
		t.Fatalf("the next run = %d: %s%s", code, stdout, stderr)
	}
	r.assertRigAsSeeded(t)
}
