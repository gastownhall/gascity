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
	"github.com/gastownhall/gascity/internal/dispatch"
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
	root := graphRow("gp-root1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs", "gc.kind": "workflow"})
	step := graphRow("gp-step1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"})
	unscoped := graphRow("gp-step2", "gp-root1", "closed", nil) // the import annotates its scope
	wisp := graphRow("gp-wisp-a1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"})
	wisp.Ephemeral = true
	open := graphRow("gp-d1", "gp-d1", "open", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"})
	return newReverseRigFixtureOf(t, []beads.Bead{root, step, unscoped, wisp, open, workRow("gp-w1", "closed"), workRow("gp-w2", "closed"), workRow("gp-w3", "open")}, []beads.Dep{
		{IssueID: "gp-step1", DependsOnID: "gp-root1", Type: "parent-child"},
		{IssueID: "gp-step2", DependsOnID: "gp-step1", Type: "blocks"},
		{IssueID: "gp-d1", DependsOnID: "gp-w1", Type: "blocks"},    // dropped (W closed), recorded on gp-d1
		{IssueID: "gp-w2", DependsOnID: "gp-step1", Type: "tracks"}, // dropped (W closed), recorded on gp-step1
		{IssueID: "gp-w3", DependsOnID: "gp-w1", Type: "related"},   // work to work: never moves
	})
}

// newReverseRigFixtureOf is newReverseRigFixture with rig packs seeded with
// rows and deps before its import.
func newReverseRigFixtureOf(t *testing.T, rows []beads.Bead, deps []beads.Dep) *reverseRigFixture {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "other", Prefix: "go"})
	f.seed("packs", rows, deps)
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

// TestReverseToRigsReturnsARuntimeGatesEdge: after the import, the runtime
// mints cross-store gates in the binding as a drain does
// (internal/dispatch/drain.go): owned by the drain control, here gp-d1, a row
// the import moved, with no import stamp (dispatch.NewCrossStoreGate), and
// waited on by the rows of the drain's item, never by the control, which
// waits on the item's root. While they are open, --to-rigs refuses them, dry
// run included, and writes nothing. Once they are closed, each returned row
// that waited on one gets back the edge it stood for, row -> blocker, and the
// control gets none. A gate a row the binding keeps still waits on stays in
// the binding with that row's edge; a gate only returned rows waited on
// leaves with them. A gate whose owner the binding keeps stays there as it
// is, open, with its edge.
func TestReverseToRigsReturnsARuntimeGatesEdge(t *testing.T) {
	r := newReverseRigFixture(t)
	opts := reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}
	binding, err := openInfraDestination(r.target)
	if err != nil {
		t.Fatal(err)
	}
	mint := func(owner beads.Bead, blocker string, waiting ...string) beads.Bead {
		t.Helper()
		gate, err := binding.Create(dispatch.NewCrossStoreGate(beads.GraphStore{Store: binding}, owner, blocker))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range waiting {
			if err := binding.DepAdd(id, gate.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
		}
		return gate
	}
	control, err := binding.Get("gp-d1")
	if err != nil {
		t.Fatal(err)
	}
	// The drain's item root, which the binding minted after the import: the
	// control holds it, finished, and it waits on the first gate together
	// with gp-step2, an imported step the drain projected onto that gate.
	// Only gp-step1 waits on the second.
	item := mustCreateInfraBead(t, binding, beads.Bead{Title: "a drain item the binding minted", Type: "task"})
	if err := binding.DepAdd(control.ID, item.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := binding.Close(item.ID); err != nil {
		t.Fatal(err)
	}
	keptGate := mint(control, "gp-w2", item.ID, "gp-step2")
	leavingGate := mint(control, "gp-w3", "gp-step1")
	other := mustCreateInfraBead(t, binding, beads.Bead{Title: "a drain control the binding minted", Type: "task"})
	otherItem := mustCreateInfraBead(t, binding, beads.Bead{Title: "its item", Type: "task"})
	if err := binding.DepAdd(other.ID, otherItem.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	otherGate := mint(other, "gp-w1", otherItem.ID)
	_ = closeBeadStoreHandle(binding)
	if keptGate.Metadata[infraMigrationStampKey] != "" {
		t.Fatalf("the runtime's gate carries an import stamp: %v", keptGate.Metadata)
	}

	rigBefore, bindingBefore := workStoreFingerprint(t, r.rig("packs")), workStoreFingerprint(t, r.binding())
	dry := opts
	dry.DryRun = true
	for _, run := range []reverseOptions{dry, opts} {
		code, stdout, stderr := r.reverse(t, run)
		if code == 0 || !strings.Contains(stderr, keptGate.ID+" (gate for gp-d1 on gp-w2)") || !strings.Contains(stderr, leavingGate.ID+" (gate for gp-d1 on gp-w3)") || !strings.Contains(stderr, "open cross-store gate") || strings.Contains(stderr, otherGate.ID) {
			t.Fatalf("--to-rigs (dry run %v) over the open gates of a row it returns = %d: %s%s", run.DryRun, code, stdout, stderr)
		}
	}
	if workStoreFingerprint(t, r.rig("packs")) != rigBefore || workStoreFingerprint(t, r.binding()) != bindingBefore || fileExists(storageReverseJournalPath(r.request.CityPath)) {
		t.Fatal("a refused reverse wrote to the rig, the binding or its journal")
	}

	binding, err = openInfraDestination(r.target)
	if err != nil {
		t.Fatal(err)
	}
	for _, gate := range []beads.Bead{keptGate, leavingGate} {
		releaseCrossStoreGate(t, binding, gate)
	}
	_ = closeBeadStoreHandle(binding)
	code, stdout, stderr := r.reverse(t, opts)
	if code != 0 {
		t.Fatalf("--to-rigs once the gates are closed = %d: %s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "1 closed cross-store gate(s) not carried") {
		t.Fatalf("the reverse did not report the one gate that left the binding: %s", stdout)
	}
	r.seededEdges = append(r.seededEdges, "gp-step1 -[blocks]-> gp-w3", "gp-step2 -[blocks]-> gp-w2")
	sort.Strings(r.seededEdges)
	r.assertRigAsSeeded(t)
	after := r.binding()
	if _, err := after.Get(leavingGate.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("the binding still holds a gate only returned rows waited on: %v", err)
	}
	if got, err := after.Get(keptGate.ID); err != nil || got.Status != "closed" {
		t.Fatalf("the gate a row the binding keeps waits on = %+v, %v; want it there, closed", got, err)
	}
	if deps, err := after.DepList(item.ID, "down"); err != nil || len(deps) != 1 || deps[0].DependsOnID != keptGate.ID {
		t.Fatalf("the edges of the item the binding keeps = %v, %v; want its edge onto its gate", deps, err)
	}
	if got, err := after.Get(otherGate.ID); err != nil || got.Status != "open" {
		t.Fatalf("the gate of a control the binding keeps = %+v, %v; want it there, open", got, err)
	}
	if deps, err := after.DepList(otherItem.ID, "down"); err != nil || len(deps) != 1 || deps[0].DependsOnID != otherGate.ID {
		t.Fatalf("the edges of that control's item = %v, %v; want its edge onto its gate", deps, err)
	}
}

// TestReverseToRigsRefusesToHoldARowAnOperatorReleased: the rig's import moved
// gp-g1, open, behind a cross-store gate on gp-w9, open work that stays in the
// rig, and recorded the edge on gp-g1 as gated. An operator's close of that
// gate released gp-g1 whatever gp-w9 does, whether the close is marked
// overridden or not. --to-rigs would put gp-g1 -> gp-w9 back in the rig with
// both open, from the record and from gp-g1's edge onto the gate, so it
// refuses, dry run included, naming the edge once with its gate, and writes
// nothing; once gp-w9 closes, it restores the edge. A gate the controller
// released restores the edge with gp-w9 open: the runtime reopens such a gate
// when its blocker holds again.
func TestReverseToRigsRefusesToHoldARowAnOperatorReleased(t *testing.T) {
	for _, tc := range []struct {
		name    string
		close   func(t *testing.T, binding beads.Store, gate beads.Bead)
		refused bool
	}{
		{"marked overridden", func(t *testing.T, binding beads.Store, gate beads.Bead) {
			t.Helper()
			closed := "closed"
			if err := binding.Update(gate.ID, beads.UpdateOpts{Status: &closed, Metadata: map[string]string{beadmeta.CrossStoreGateStateMetadataKey: dispatch.CrossStoreGateStateOverridden}}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"closed with no reason", func(t *testing.T, binding beads.Store, gate beads.Bead) {
			t.Helper()
			if err := binding.Close(gate.ID); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"released by the controller", releaseCrossStoreGate, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReverseRigFixtureOf(t, []beads.Bead{
				graphRow("gp-g1", "gp-g1", "open", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"}),
				workRow("gp-w9", "open"),
			}, []beads.Dep{{IssueID: "gp-g1", DependsOnID: "gp-w9", Type: "blocks"}})
			binding, err := openInfraDestination(r.target)
			if err != nil {
				t.Fatal(err)
			}
			var gate beads.Bead
			for _, b := range rigTestList(t, binding) {
				if isCrossStoreGate(b) && b.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey] == "gp-g1" {
					gate = b
				}
			}
			owner, err := binding.Get("gp-g1")
			if err != nil || gate.ID == "" || !hasEdge(t, binding, "gp-g1", gate.ID) || !strings.Contains(owner.Metadata[beadmeta.MigratedCrossEdgesMetadataKey], `"action":"gated"`) {
				t.Fatalf("the import did not gate gp-g1 on gp-w9: gate %+v, owner %+v, %v", gate, owner, err)
			}
			tc.close(t, binding, gate)
			_ = closeBeadStoreHandle(binding)
			opts := reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}

			if tc.refused {
				rigBefore, bindingBefore := workStoreFingerprint(t, r.rig("packs")), workStoreFingerprint(t, r.binding())
				want := fmt.Sprintf("gp-g1 -[blocks]-> gp-w9 (gate %s)", gate.ID)
				dry := opts
				dry.DryRun = true
				for _, run := range []reverseOptions{dry, opts} {
					code, stdout, stderr := r.reverse(t, run)
					if code == 0 || strings.Count(stderr, want) != 1 || !strings.Contains(stderr, "an operator closed each gate") {
						t.Fatalf("--to-rigs (dry run %v) over an operator's close of the import's gate = %d, want one refusal of %s: %s%s", run.DryRun, code, want, stdout, stderr)
					}
				}
				if workStoreFingerprint(t, r.rig("packs")) != rigBefore || workStoreFingerprint(t, r.binding()) != bindingBefore || fileExists(storageReverseJournalPath(r.request.CityPath)) {
					t.Fatal("a refused reverse wrote to the rig, the binding or its journal")
				}
				if err := r.rig("packs").Close("gp-w9"); err != nil {
					t.Fatal(err)
				}
			}
			if code, stdout, stderr := r.reverse(t, opts); code != 0 {
				t.Fatalf("--to-rigs = %d: %s%s", code, stdout, stderr)
			}
			if !hasEdge(t, r.rig("packs"), "gp-g1", "gp-w9") {
				t.Fatalf("the rig lacks gp-g1 -> gp-w9: %v", rigEdgeFingerprint(t, r.rig("packs")))
			}
		})
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
		infraEngineCrashPoint = func(p string) error {
			if p != reverseGuardedSeam {
				points = append(points, p)
			}
			return nil
		}
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

// writeSplitCityTOML writes the fixture's city.toml, with its rigs and its
// binding, which a --to-work edits at R4.
func (r *reverseRigFixture) writeSplitCityTOML(t *testing.T) {
	t.Helper()
	toml := reverseCityTOMLTextAt(r.cfg.Storage.Bindings["infra"].Path, "", r.cfg.Rigs...)
	if err := os.WriteFile(filepath.Join(r.request.CityPath, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReverseToWorkReturnsARigImport: --to-work returns the rig's imported
// rows to the rig (S4c inside the whole reverse) and retires the city's split.
func TestReverseToWorkReturnsARigImport(t *testing.T) {
	r := newReverseRigFixture(t)
	r.writeSplitCityTOML(t)
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

// TestReverseRefusesTheStrandedRowsItWouldReviveInARig: an imported rig is
// relic-free, so its readers skip its store, and a row a raw writer put there
// after the import is hidden from them: the census calls the rig stranded.
// Each reverse gives that store every class back (--to-work retires the
// split, --to-rigs gives the rig its relic legs), so each refuses, dry run
// included, every open stranded row the rig would serve again, names the
// rig's ways to settle it, and writes nothing. It refuses and reports exactly
// the rows the census calls stranded: a closed one, one that is
// infrastructure class only because of metadata, and open mail, which
// nothing reads from a rig's store (mail reads the city's), stay in the rig
// as they are, which the reverse reports. A rig awaiting import hides
// nothing, its relic legs serving it, so its open row is not refused. Once
// the open session is closed, the reverse finishes and leaves the four as
// the writer left them.
func TestReverseRefusesTheStrandedRowsItWouldReviveInARig(t *testing.T) {
	for _, mode := range []reverseMode{reverseToRigs, reverseToWork} {
		t.Run(string(mode), func(t *testing.T) {
			r := newReverseRigFixture(t)
			opts := reverseOptions{Mode: mode, Rigs: []string{"packs"}}
			if mode == reverseToWork {
				r.writeSplitCityTOML(t)
				opts.Rigs = nil
			}
			rig := r.rig("packs")
			mail := mustCreateInfraBead(t, rig, beads.Bead{Title: "stranded mail", Type: "message", Assignee: "worker"})
			session := mustCreateInfraBead(t, rig, beads.Bead{Title: "stranded session", Type: "session", Labels: []string{"gc:session"}})
			read := mustCreateInfraBead(t, rig, beads.Bead{Title: "read mail", Type: "message", Assignee: "worker"})
			if err := rig.Close(read.ID); err != nil {
				t.Fatal(err)
			}
			work := mustCreateInfraBead(t, rig, beads.Bead{Title: "patrol finding", Type: "task", Metadata: beads.StringMap{beadmeta.RootBeadIDMetadataKey: "gp-root1"}})
			awaiting := mustCreateInfraBead(t, r.rig("other"), beads.Bead{Title: "mail its relic legs serve", Type: "message", Assignee: "worker"})
			census := map[string]rigCensusRow{}
			for _, row := range censusRigs(r.request.CityPath, r.cfg, r.target, false) {
				census[row.Rig] = row
			}
			stranded := []string{mail.ID, session.ID, read.ID, work.ID}
			sort.Strings(stranded)
			if got := census["packs"]; got.State != rigCensusStranded || strings.Join(got.Stranded, ",") != strings.Join(stranded, ",") || !r.relicFree("packs") {
				t.Fatalf("rig packs before the reverse: %s stranded %v (relic-free %v), want stranded %v", got.State, got.Stranded, r.relicFree("packs"), stranded)
			}
			if got := census["other"]; got.State != rigCensusAwaiting || r.relicFree("other") {
				t.Fatalf("rig other before the reverse: %s (relic-free %v), want awaiting import", got.State, r.relicFree("other"))
			}
			packs, _ := rigImportRigNamed(r.cfg, "packs")
			before := workStoreFingerprint(t, r.rig("packs"))
			dry := opts
			dry.DryRun = true
			for _, run := range []reverseOptions{dry, opts} {
				code, stdout, stderr := r.reverse(t, run)
				if code == 0 || !strings.Contains(stderr, "REFUSED — 1 shape(s)") {
					t.Fatalf("--%s (dry run %v) over open stranded rig rows = %d: %s%s", mode, run.DryRun, code, stdout, stderr)
				}
				for _, want := range []string{session.ID + " (session, open)", "rig packs holds", "bd close <id>", rigStoreRoot(r.request.CityPath, packs), rigImportInstruction("packs")} {
					if !strings.Contains(stderr, want) {
						t.Fatalf("the refusal does not say %q: %s", want, stderr)
					}
				}
				for _, id := range []string{mail.ID, read.ID, work.ID, awaiting.ID} {
					if strings.Contains(stderr, id+" (") {
						t.Fatalf("the refusal names %s, which the reverse leaves as it is: %s", id, stderr)
					}
				}
				for _, want := range []string{"1 closed row(s) the store of rig packs holds", read.ID, "1 row(s) the store of rig packs holds", work.ID, beadmeta.RootBeadIDMetadataKey, "1 open mail or nudge row(s) the store of rig packs holds", "nothing delivers them from there: " + mail.ID} {
					if !strings.Contains(stdout, want) {
						t.Fatalf("the plan does not report %q: %s", want, stdout)
					}
				}
			}
			if after := workStoreFingerprint(t, r.rig("packs")); after != before || fileExists(storageReverseJournalPath(r.request.CityPath)) {
				t.Fatal("a refused reverse wrote to the rig or its journal")
			}
			if err := rig.Close(session.ID); err != nil {
				t.Fatal(err)
			}
			if code, stdout, stderr := r.reverse(t, opts); code != 0 {
				t.Fatalf("--%s once the open stranded session is closed = %d: %s%s", mode, code, stdout, stderr)
			}
			for store, want := range map[string]map[string]string{
				"packs": {mail.ID: "open", session.ID: "closed", read.ID: "closed", work.ID: "open"},
				"other": {awaiting.ID: "open"},
			} {
				for id, status := range want {
					b, err := r.rig(store).Get(id)
					if err != nil || b.Status != status || b.Metadata[beadmeta.InfraReversedFromMetadataKey] != "" {
						t.Fatalf("after the reverse rig %s holds %s as %q (stamp %q, %v), want it %s as the writer left it", store, id, b.Status, b.Metadata[beadmeta.InfraReversedFromMetadataKey], err, status)
					}
				}
			}
		})
	}
}

// TestReverseRefusesARigRowUnderAnIDItsImportMoved: the rig minted again an
// id its import moved, and the binding holds the original under it. Each
// reverse returns that original to the rig under its id, so it refuses the
// rig's row, of either class and open or closed, with the import's own fix
// for such a row, not as an id held "as another bead", whose hand delete
// would lose one of them.
func TestReverseRefusesARigRowUnderAnIDItsImportMoved(t *testing.T) {
	for _, mode := range []reverseMode{reverseToRigs, reverseToWork} {
		for _, again := range []beads.Bead{
			{Title: "minted again", Type: "message", Assignee: "worker", Status: "closed"},
			{Title: "a task minted again", Type: "task", Status: "open"},
		} {
			t.Run(string(mode)+"/"+again.Type, func(t *testing.T) {
				r := newReverseRigFixture(t)
				opts := reverseOptions{Mode: mode, Rigs: []string{"packs"}, DryRun: true}
				if mode == reverseToWork {
					r.writeSplitCityTOML(t)
					opts.Rigs = nil
				}
				again.ID = "gp-step1"
				again.CreatedAt = r.seeded["gp-step1"].CreatedAt.Add(time.Hour)
				again.UpdatedAt = again.CreatedAt
				if _, err := r.rig("packs").(beads.ForeignIDCreator).CreateWithForeignID(again); err != nil {
					t.Fatal(err)
				}
				stranded := map[string]string{"message": "gp-step1", "task": ""}[again.Type]
				for _, row := range censusRigs(r.request.CityPath, r.cfg, r.target, false) {
					if row.Rig == "packs" && (strings.Join(row.Reused, ",") != "gp-step1" || strings.Join(row.Stranded, ",") != stranded) {
						t.Fatalf("the census of rig packs: %s (%v), reused %v, stranded %v, want gp-step1 reused and %q stranded", row.State, row.Err, row.Reused, row.Stranded, stranded)
					}
				}
				before := workStoreFingerprint(t, r.rig("packs"))
				code, stdout, stderr := r.reverse(t, opts)
				if code == 0 || !strings.Contains(stderr, "REFUSED — 1 shape(s)") || !strings.Contains(stderr, fmt.Sprintf("gp-step1 (%s, %s)", again.Type, again.Status)) || !strings.Contains(stderr, rigReusedIDRemedy) {
					t.Fatalf("--%s --dry-run over a rig row under an id its import moved = %d: %s%s", mode, code, stdout, stderr)
				}
				if strings.Contains(stderr, "as another bead") || strings.Contains(stdout, "stay there as") {
					t.Fatalf("the reverse sends the reused id to a hand delete, or reports it as left in the rig: %s%s", stdout, stderr)
				}
				if after := workStoreFingerprint(t, r.rig("packs")); after != before {
					t.Fatal("a refused reverse wrote to the rig")
				}
			})
		}
		// The binding no longer holds the original (its row was deleted
		// there): the reverse returns nothing under the id, so the rig's row
		// is not refused as a second bead under it.
		t.Run(string(mode)+"/original gone from the binding", func(t *testing.T) {
			r := newReverseRigFixture(t)
			opts := reverseOptions{Mode: mode, Rigs: []string{"packs"}, DryRun: true}
			if mode == reverseToWork {
				r.writeSplitCityTOML(t)
				opts.Rigs = nil
			}
			again := beads.Bead{ID: "gp-step1", Title: "a task minted again", Type: "task", Status: "open"}
			again.CreatedAt = r.seeded["gp-step1"].CreatedAt.Add(time.Hour)
			again.UpdatedAt = again.CreatedAt
			if _, err := r.rig("packs").(beads.ForeignIDCreator).CreateWithForeignID(again); err != nil {
				t.Fatal(err)
			}
			binding, err := openInfraDestination(r.target)
			if err != nil {
				t.Fatal(err)
			}
			if err := binding.Delete("gp-step1"); err != nil {
				t.Fatal(err)
			}
			_ = closeBeadStoreHandle(binding)
			code, stdout, stderr := r.reverse(t, opts)
			if strings.Contains(stderr, rigReusedIDRemedy) || strings.Contains(stderr, "minted each of these ids again") {
				t.Fatalf("--%s --dry-run refused a rig row whose id the binding no longer holds = %d: %s%s", mode, code, stdout, stderr)
			}
			if code != 0 {
				t.Fatalf("--%s --dry-run with the original gone from the binding = %d: %s%s", mode, code, stdout, stderr)
			}
		})
	}
}

// TestReverseRigCheckLeavesOutItsOwnRows: the rig check leaves out a row
// that carries this run's stamp, one a resumed run wrote itself, however the
// rig's manifest reads it; the same row under another run's stamp is a
// stranded row like any other.
func TestReverseRigCheckLeavesOutItsOwnRows(t *testing.T) {
	r := newReverseRigFixture(t)
	manifest, _, err := r.loc("packs").readManifest()
	if err != nil || manifest == nil {
		t.Fatalf("rig packs's manifest = %v, %v", manifest, err)
	}
	const stamp = "infra/20261110T000000.000000000Z-0a0b0c0d"
	run := reverseRun{request: r.request, opts: reverseOptions{Mode: reverseToRigs}, target: r.target, journal: storageReverseJournal{Rigs: []string{"packs"}, Stamp: stamp}}
	for _, tc := range []struct {
		stamp   string
		refused bool
	}{{stamp, false}, {"infra/20261109T000000.000000000Z-0a0b0c0d", true}} {
		sess := beads.Bead{ID: "gp-sess", Title: "session", Type: "session", Status: "open", Labels: []string{"gc:session"}, Metadata: beads.StringMap{beadmeta.InfraReversedFromMetadataKey: tc.stamp}}
		in := reverseInput{Held: map[string]map[string]beads.Bead{rigImportStamp("packs"): {sess.ID: sess}}, Manifests: map[string]*infraCopyManifest{"packs": manifest}}
		refusals := run.rigStoreLeftovers(in, &reversePlan{})
		if refused := len(refusals) == 1 && strings.Contains(refusals[0].String(), "gp-sess (session, open)"); refused != tc.refused || len(refusals) > 1 {
			t.Fatalf("a stranded open row stamped %q: refusals %v, want refused %v", tc.stamp, refusals, tc.refused)
		}
	}
}

// TestReverseToWorkChecksARigRecordedClean: a rig recorded clean is
// relic-free too, so an open infrastructure row a raw writer put there
// afterwards is hidden, and a --to-work refuses it. While the rig awaits
// import, its relic legs serve the same row, and it is not refused.
func TestReverseToWorkChecksARigRecordedClean(t *testing.T) {
	r := newReverseRigFixture(t)
	r.writeSplitCityTOML(t)
	sess := mustCreateInfraBead(t, r.rig("other"), beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	if code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToWork, DryRun: true}); code != 0 || r.relicFree("other") {
		t.Fatalf("--to-work --dry-run over an open row in a rig awaiting import (relic-free %v) = %d: %s%s", r.relicFree("other"), code, stdout, stderr)
	}
	other, _ := rigImportRigNamed(r.cfg, "other")
	loc := r.loc("other")
	if err := loc.writeRecord(loc.CleanPath(), "rig other holds no infrastructure bead (a census before the write)", rigStoreRoot(r.request.CityPath, other)); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToWork, DryRun: true})
	if code == 0 || !r.relicFree("other") || !strings.Contains(stderr, sess.ID+" (session, open)") || !strings.Contains(stderr, "rig other holds") {
		t.Fatalf("--to-work --dry-run over an open row in a rig recorded clean (relic-free %v) = %d: %s%s", r.relicFree("other"), code, stdout, stderr)
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
// realDoltReverseCity is a city split from a real (embedded) Dolt work store
// holding a workflow, its steps, a mail row and a work row; edge, if set,
// runs on the work store before the forward migration.
func realDoltReverseCity(t *testing.T, edge func(*beads.NativeDoltStore)) (*beads.NativeDoltStore, string, *config.City, map[string]beads.Bead, []string) {
	t.Helper()
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
	if edge != nil {
		edge(work)
	}
	stubInfraControllerPing(t, 0)
	var log bytes.Buffer
	if report := migrateInfraClasses(t, cityPath, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("forward from Dolt = %v: %s", report.Outcome, log.String())
	}
	return work, cityPath, cfg, seeded, seededEdges
}

func TestReverseToWorkIntoARealDoltWorkStore(t *testing.T) {
	work, cityPath, cfg, seeded, seededEdges := realDoltReverseCity(t, nil)
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

// TestReverseToRigsReprovesTheBindingOnAResume (SHOULD 9): a binding row
// changed between a run killed once its copies were proven (R2) and the
// resume, which plans again, puts the reverse back to R0 before the clear
// can remove the newer row, and the next run returns the newer row to the
// rig.
func TestReverseToRigsReprovesTheBindingOnAResume(t *testing.T) {
	r := newReverseRigFixture(t)
	opts := reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}
	title := "written after the copy"
	var updated time.Time
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p != "reverse phase "+reversePhaseName(reverseR2) {
			return nil
		}
		binding, err := openInfraDestination(r.target)
		if err != nil {
			t.Fatal(err)
		}
		defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
		if err := binding.Update("gp-step1", beads.UpdateOpts{Title: &title}); err != nil {
			t.Fatal(err)
		}
		got, err := binding.Get("gp-step1")
		if err != nil {
			t.Fatal(err)
		}
		updated = got.UpdatedAt
		return errors.New("killed")
	}
	code, _, stderr := r.reverse(t, opts)
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatalf("the kill did not stop the reverse: %s", stderr)
	}
	code, stdout, stderr := r.reverse(t, opts)
	if code == 0 || !strings.Contains(stderr, "the binding changed after its rows were copied") {
		t.Fatalf("a resume over a changed binding = %d: %s%s", code, stdout, stderr)
	}
	if journal, _, _ := readStorageReverseJournal(r.request.CityPath); journal.Phase != reverseR0 || journal.ClearStarted {
		t.Fatalf("journal phase %d, clear started %v; want R0 and no clear", journal.Phase, journal.ClearStarted)
	}
	if code, stdout, stderr := r.reverse(t, opts); code != 0 {
		t.Fatalf("the next run = %d: %s%s", code, stdout, stderr)
	}
	want := r.seeded["gp-step1"]
	want.Title, want.UpdatedAt = title, updated
	r.seeded["gp-step1"] = want
	r.assertRigAsSeeded(t)
}

// TestAbortReverseRefusesAnEdgeARealDoltStoreWouldNotTakeBack: a native Dolt
// work store refuses an edge onto its own prefix it does not hold, so the
// abort of a reverse that wrote such an edge's target refuses, before it
// deletes anything, rather than lose the edge. bd keeps no such edge through
// its own API (it refuses the add, and a delete takes a row's edges with it),
// so the edge an older clear wrote is recorded as the first plan records it.
func TestAbortReverseRefusesAnEdgeARealDoltStoreWouldNotTakeBack(t *testing.T) {
	work, cityPath, cfg, _, _ := realDoltReverseCity(t, nil)
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse city verify" {
			return errors.New("killed")
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := doStorageReverse(context.Background(), request, reverseOptions{Mode: reverseToWork}, &stdout, &stderr)
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatalf("the kill did not stop the reverse: %s", stdout.String())
	}
	if _, err := work.Get("gd-s3"); err != nil {
		t.Fatalf("precondition: the reverse wrote gd-s3 into the Dolt work store: %v", err)
	}
	journal, _, err := readStorageReverseJournal(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	journal.Preserved = map[string][]reverseEdge{reverseCityDest: {{From: "gd-w1", To: "gd-s3", Type: "blocks"}}}
	if err := writeStorageReverseJournal(cityPath, journal); err != nil {
		t.Fatal(err)
	}
	before := workStoreFingerprint(t, work)
	stdout.Reset()
	stderr.Reset()
	if code := doStorageAbortReverse(context.Background(), request, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "gd-w1 -[blocks]-> gd-s3") || !strings.Contains(stderr.String(), "Nothing was removed") {
		t.Fatalf("--abort-reverse over an edge Dolt would not take back = %d: %s%s", code, stdout.String(), stderr.String())
	}
	if workStoreFingerprint(t, work) != before {
		t.Fatal("the refused abort changed the Dolt work store")
	}
}
