package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// smallEngineBatches cuts a city of a few hundred rows into several batches
// of every phase, so each batch boundary is a boundary a test can stop at.
func smallEngineBatches(t *testing.T) {
	t.Helper()
	rows, edges, reads, clears := infraCopyRowBatch, infraCopyEdgeBatch, infraReadBatch, infraClearChunk
	infraCopyRowBatch, infraCopyEdgeBatch, infraReadBatch, infraClearChunk = 100, 40, 70, 90
	t.Cleanup(func() {
		infraCopyRowBatch, infraCopyEdgeBatch, infraReadBatch, infraClearChunk = rows, edges, reads, clears
	})
}

// engineCity is an MC-shaped city ready to migrate: its work store (the kind
// named), the city directory and its binding target.
type engineCity struct {
	mcShapedInfraCity
	source   beads.Store
	cityPath string
	target   infraBindingTarget
}

func newEngineCity(t *testing.T, rows int, kind string) engineCity {
	t.Helper()
	city := newMCShapedInfraCity(t, rows)
	var source beads.Store
	switch kind {
	case "sqlite":
		source = city.sqliteSource(t)
	case "memory":
		source = city.source()
	default:
		t.Fatalf("unknown source kind %q", kind)
	}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	cityPath := t.TempDir()
	return engineCity{mcShapedInfraCity: city, source: source, cityPath: cityPath, target: mustResolveInfraTarget(t, cityPath, infraSplitConfig(".gc/store"))}
}

func (c engineCity) migrate(t *testing.T) (infraMigrationReport, string) {
	t.Helper()
	var log bytes.Buffer
	report := migrateInfraClasses(t, c.cityPath, infraSplitConfig(".gc/store"), &log)
	return report, log.String()
}

// isInfra reports whether the fixture row b is in the infrastructure slice.
func isInfra(b beads.Bead) bool { return coordclass.Classify(b) != coordclass.ClassWork }

// assertNotLossy proves a converged city carried everything: the binding
// holds exactly the source's infrastructure rows, every field the copy
// carries equal, every within-infra edge with its type and payload and no
// other; the retained-source backup records every infrastructure row with
// every edge touching it, cross-store edges and payloads included; and the
// work store holds its work rows and nothing else.
func (c engineCity) assertNotLossy(t *testing.T) {
	t.Helper()
	infraIDs := map[string]bool{}
	want := map[string]beads.Bead{}
	for _, b := range c.rows {
		if isInfra(b) {
			infraIDs[b.ID] = true
			want[b.ID] = b
		}
	}
	binding, err := openInfraBindingReadOnly(c.target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
	held, err := binding.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != len(want) {
		t.Fatalf("the binding holds %d rows, want the %d infrastructure rows", len(held), len(want))
	}
	for _, got := range held {
		w, ok := want[got.ID]
		if !ok {
			t.Fatalf("the binding holds %s, which is not an infrastructure row of the source", got.ID)
		}
		if diff := beadCopyDifference(w, got); diff != "" {
			t.Fatalf("row %s differs in the binding: %s", got.ID, diff)
		}
	}
	reader := binding.(beads.DepMetadataReader)
	wantEdges := map[[2]string]string{}
	for _, d := range c.deps {
		if infraIDs[d.IssueID] && infraIDs[d.DependsOnID] {
			wantEdges[[2]string{d.IssueID, d.DependsOnID}] = d.Type
		}
	}
	gotEdges := 0
	for id := range want {
		deps, err := binding.DepList(id, "down")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range deps {
			key := [2]string{id, d.DependsOnID}
			kind, ok := wantEdges[key]
			if !ok || kind != d.Type {
				t.Fatalf("the binding holds edge %s -> %s (%s), want %q", id, d.DependsOnID, d.Type, kind)
			}
			gotEdges++
			payload, carried, err := reader.DepMetadata(id, d.DependsOnID)
			if err != nil {
				t.Fatal(err)
			}
			wantPayload, wantCarried := c.payloads[key]
			if carried != wantCarried || payload != wantPayload {
				t.Fatalf("edge %s -> %s carries (%q, %v) in the binding, want (%q, %v)", id, d.DependsOnID, payload, carried, wantPayload, wantCarried)
			}
		}
	}
	if gotEdges != len(wantEdges) {
		t.Fatalf("the binding holds %d within-infra edges, want %d", gotEdges, len(wantEdges))
	}

	backup, present, err := readInfraRetainedBackup(c.target)
	if err != nil || !present {
		t.Fatalf("reading the retained-source backup: present=%v err=%v", present, err)
	}
	backed := map[string]infraBackupEntry{}
	for _, e := range backup {
		backed[e.Bead.ID] = e
	}
	if len(backed) != len(want) {
		t.Fatalf("the backup records %d rows, want %d", len(backed), len(want))
	}
	edgeKey := func(e infraBackupEdge) string {
		return fmt.Sprintf("%s>%s:%s:%v:%s", e.IssueID, e.DependsOnID, e.Type, e.Carried, e.Payload)
	}
	wantDeps, wantDependents := map[string][]string{}, map[string][]string{}
	for _, d := range c.deps {
		payload, carried := c.payloads[[2]string{d.IssueID, d.DependsOnID}]
		key := edgeKey(infraBackupEdge{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type, Payload: payload, Carried: carried})
		if infraIDs[d.IssueID] {
			wantDeps[d.IssueID] = append(wantDeps[d.IssueID], key)
		} else if infraIDs[d.DependsOnID] {
			wantDependents[d.DependsOnID] = append(wantDependents[d.DependsOnID], key)
		}
	}
	for id := range want {
		e, ok := backed[id]
		if !ok {
			t.Fatalf("the backup does not record %s", id)
		}
		if diff := beadCopyDifference(want[id], e.Bead); diff != "" {
			t.Fatalf("the backup's %s differs: %s", id, diff)
		}
		for _, side := range []struct {
			name      string
			got, want []string
		}{
			{"deps", backupEdgeKeys(e.Deps, edgeKey), wantDeps[id]},
			{"dependents", backupEdgeKeys(e.Dependents, edgeKey), wantDependents[id]},
		} {
			sort.Strings(side.want)
			if strings.Join(side.got, ",") != strings.Join(side.want, ",") {
				t.Fatalf("the backup's %s of %s = %v, want %v", side.name, id, side.got, side.want)
			}
		}
	}

	left, err := c.source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range left {
		if infraIDs[b.ID] {
			t.Fatalf("the work store still holds the infrastructure row %s after the clear", b.ID)
		}
	}
	if len(left) != len(c.rows)-len(want) {
		t.Fatalf("the work store holds %d rows after the clear, want its %d work rows", len(left), len(c.rows)-len(want))
	}

	// Every cross-store edge from a work bead into a moved row is back in
	// the work store after the clear, unless it was a blocking edge the
	// binding's copy already satisfies, which the clear releases. A resumed
	// clear that forgot a row an earlier run removed would leave it out.
	kept := 0
	for _, d := range c.deps {
		if infraIDs[d.IssueID] || !infraIDs[d.DependsOnID] {
			continue
		}
		target := want[d.DependsOnID]
		released := beads.IsReadyBlockingDependencyType(d.Type) && beads.DependencySatisfied(target.Status, beads.ReadinessWorkOutcome(target.Metadata))
		deps, err := c.source.DepList(d.IssueID, "down")
		if err != nil {
			t.Fatal(err)
		}
		held := false
		for _, got := range deps {
			if got.DependsOnID == d.DependsOnID && got.Type == d.Type {
				held = true
			}
		}
		if held == released {
			t.Fatalf("the cross-store edge %s -[%s]-> %s is held=%v after the clear, want held=%v (released only when the binding satisfies it)", d.IssueID, d.Type, d.DependsOnID, held, !released)
		}
		if held {
			kept++
		}
	}
	if kept == 0 {
		t.Fatal("the fixture has no kept cross-store edge, so the check above proves nothing")
	}
}

func backupEdgeKeys(edges []infraBackupEdge, key func(infraBackupEdge) string) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, key(e))
	}
	sort.Strings(out)
	return out
}

// TestInfraEngineCopiesAnMCShapedCityWithoutLoss: the batched engine converges
// an MC-shaped city out of a SQLite work store (every batch capability) and an
// in-memory one (the per-row fallbacks), and loses nothing: rows, edges and
// payloads in the binding, every edge in the backup, only work left behind.
func TestInfraEngineCopiesAnMCShapedCityWithoutLoss(t *testing.T) {
	for _, kind := range []string{"sqlite", "memory"} {
		t.Run(kind, func(t *testing.T) {
			smallEngineBatches(t)
			city := newEngineCity(t, 700, kind)
			report, said := city.migrate(t)
			if report.Outcome != infraMigrationConverged {
				t.Fatalf("outcome = %v: %s", report.Outcome, said)
			}
			city.assertNotLossy(t)
		})
	}
}

// infraEngineBoundaries are the seam points one run of the engine passes, in
// order, collected from a dry run.
func infraEngineBoundaries(t *testing.T, kind string) []string {
	t.Helper()
	smallEngineBatches(t)
	city := newEngineCity(t, 700, kind)
	var points []string
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(point string) error { points = append(points, point); return nil }
	defer func() { infraEngineCrashPoint = prev }()
	if report, said := city.migrate(t); report.Outcome != infraMigrationConverged {
		t.Fatalf("dry run outcome = %v: %s", report.Outcome, said)
	}
	return points
}

var infraCopiedCount = regexp.MustCompile(`\((\d+) beads copied\)`)

// TestInfraEngineResumesFromEveryBoundary kills the engine at every batch and
// phase boundary it passes, then runs it again: each run converges without
// loss, and a run after a kill inside the copy keeps the rows the killed run
// already wrote instead of copying them again.
func TestInfraEngineResumesFromEveryBoundary(t *testing.T) {
	for _, kind := range []string{"sqlite", "memory"} {
		points := infraEngineBoundaries(t, kind)
		for _, want := range []string{"plan", "prepare", "copying rows 100/", "copying edges 40/", "verify", "manifest", "marker", "clearing 90/"} {
			found := false
			for _, p := range points {
				found = found || strings.HasPrefix(p, want)
			}
			if !found {
				t.Fatalf("%s: the engine passes no %q boundary: %v", kind, want, points)
			}
		}
		for _, point := range points {
			t.Run(kind+"/"+point, func(t *testing.T) {
				smallEngineBatches(t)
				city := newEngineCity(t, 700, kind)
				killed := errors.New("killed at " + point)
				prev := infraEngineCrashPoint
				infraEngineCrashPoint = func(p string) error {
					if p == point {
						return killed
					}
					return nil
				}
				first, said := city.migrate(t)
				infraEngineCrashPoint = prev
				if first.Outcome == infraMigrationConverged || !strings.Contains(said, killed.Error()) {
					t.Fatalf("the kill at %s did not stop the run: %v: %s", point, first.Outcome, said)
				}
				report, said := city.migrate(t)
				if report.Outcome != infraMigrationConverged {
					t.Fatalf("the run after a kill at %s = %v: %s", point, report.Outcome, said)
				}
				city.assertNotLossy(t)
				if !strings.HasPrefix(point, "copying rows ") {
					return
				}
				done, _ := strconv.Atoi(strings.Split(strings.TrimPrefix(point, "copying rows "), "/")[0])
				m := infraCopiedCount.FindStringSubmatch(said)
				if m == nil {
					t.Fatalf("the resumed run did not say what it copied: %s", said)
				}
				if copied, _ := strconv.Atoi(m[1]); copied != city.infra-done {
					t.Fatalf("the resumed run copied %d rows, want the %d the killed run had not: the copy did not resume", copied, city.infra-done)
				}
			})
		}
	}
}

// TestInfraEngineRecopiesAStampedRowTheSourceChanged: a row an earlier run
// stamped into the binding is kept only while it still equals the source; a
// row the source changed since, or one the binding holds an edge of that the
// source no longer has, is deleted and copied again; a kept edge whose payload
// is stale is written again; and the run converges.
func TestInfraEngineRecopiesAStampedRowTheSourceChanged(t *testing.T) {
	smallEngineBatches(t)
	city := newEngineCity(t, 300, "sqlite")
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "verify" {
			return errors.New("killed before the manifest")
		}
		return nil
	}
	if report, _ := city.migrate(t); report.Outcome == infraMigrationConverged {
		t.Fatal("the kill did not stop the run")
	}
	infraEngineCrashPoint = prev

	var changed, edged string
	for _, b := range city.rows {
		if !isInfra(b) {
			continue
		}
		if changed == "" && b.Type == "message" {
			changed = b.ID
			continue
		}
		if edged == "" && b.Type == "session" {
			edged = b.ID
		}
	}
	title := "changed after the killed run"
	if err := city.source.Update(changed, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	updated, err := city.source.Get(changed)
	if err != nil {
		t.Fatal(err)
	}
	for i := range city.rows {
		if city.rows[i].ID == changed {
			city.rows[i] = updated
		}
	}
	// An edge only the binding holds, as a source edge removed since; and a
	// kept edge whose payload the binding holds stale.
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.DepAdd(edged, changed, "related"); err != nil {
		t.Fatal(err)
	}
	var stale [2]string
	for key := range city.payloads {
		if key[0] != changed && key[1] != changed && key[0] != edged && key[1] != edged {
			stale = key
			break
		}
	}
	if err := binding.(beads.DepMetadataWriter).DepAddWithMetadata(stale[0], stale[1], "waits-for", `{"gate":"stale"}`); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)

	report, said := city.migrate(t)
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("the run after the source changed = %v: %s", report.Outcome, said)
	}
	m := infraCopiedCount.FindStringSubmatch(said)
	if m == nil || m[1] != "2" {
		t.Fatalf("the run copied %v rows, want exactly the 2 it could not keep: %s", m, said)
	}
	city.assertNotLossy(t)
}

// TestStorageMigrateTimeBudgetPausesAndResumes: a spent budget stops the run at
// the next batch boundary with exit 75, naming the phase and how far it got,
// before the marker and in the clear; the same command run again continues,
// and the city converges without loss.
func TestStorageMigrateTimeBudgetPausesAndResumes(t *testing.T) {
	smallEngineBatches(t)
	city := newEngineCity(t, 700, "sqlite")
	request := storageTestRequest(t, infraSplitConfig(".gc/store"))
	request.CityPath = city.cityPath
	city.target = mustResolveInfraTarget(t, city.cityPath, request.Cfg)
	stubInfraControllerPing(t, 0)
	stubSessionInventory(t, nil, true, nil)
	stubOtherCities(t)
	stubMigrationWindowProvider(t, &migrationWindowProvider{})
	request.TimeBudget = time.Nanosecond
	var phases []string
	for run := 0; run < 60; run++ {
		var stdout, stderr bytes.Buffer
		code := doStorageMigrate(context.Background(), request, &stdout, &stderr)
		if code == 0 {
			if len(phases) == 0 {
				t.Fatal("a nanosecond budget never paused the run")
			}
			joined := strings.Join(phases, "\n")
			for _, want := range []string{"paused in copying rows at 100/", "paused in clearing at 90/"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("no pause %q among %v", want, phases)
				}
			}
			city.assertNotLossy(t)
			return
		}
		if code != storagePausedExitCode {
			t.Fatalf("run %d = exit %d, want 0 or %d: %s", run, code, storagePausedExitCode, stderr.String())
		}
		for _, line := range strings.Split(stderr.String(), "\n") {
			if strings.Contains(line, "paused in ") && strings.Contains(line, "run the same command again") {
				phases = append(phases, line)
			}
		}
	}
	t.Fatalf("the city did not converge in 60 budgeted runs: %v", phases)
}

// TestVerifyInfraCopyNamesARowTheCopyInvented: a binding holding a row the
// work store's slice does not is refused by name, whatever else it holds.
func TestVerifyInfraCopyNamesARowTheCopyInvented(t *testing.T) {
	city := newEngineCity(t, 60, "memory")
	if report, said := city.migrate(t); report.Outcome != infraMigrationConverged {
		t.Fatalf("outcome = %v: %s", report.Outcome, said)
	}
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	invented := beads.Bead{ID: "mc-invented", Title: "never in the work store", Type: "session", Labels: []string{"gc:session"}}
	if _, err := binding.(beads.ForeignIDCreator).CreateWithForeignID(invented); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	// The work store as it was before the clear: the fixture is deterministic.
	restored := newMCShapedInfraCity(t, 60).source()
	_, err = verifyInfraCopy(func() (beads.Store, error) { return openInfraDestination(city.target) }, restored)
	if err == nil || !strings.Contains(err.Error(), "mc-invented") {
		t.Fatalf("verifyInfraCopy = %v, want a refusal naming mc-invented", err)
	}
}

// batchRewritingStore is referenceRewritingStore with a batched delete that
// rewrites mentions as bd's `bd delete --force` does, for each row in the
// batch.
type batchRewritingStore struct{ referenceRewritingStore }

func (s batchRewritingStore) DeleteBatch(ids []string) error {
	for _, id := range ids {
		if err := s.Delete(id); err != nil {
			return err
		}
	}
	return nil
}

// TestTheBatchedClearNeverRewritesWorkBeadText is ga-w4p3pw (#8786) on the
// batched delete: every row of a chunk is detached before the chunk is
// deleted, so a store whose batched delete rewrites a linked neighbor's
// mention finds no neighbor to rewrite.
func TestTheBatchedClearNeverRewritesWorkBeadText(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := batchRewritingStore{referenceRewritingStore{MemStore: beads.NewMemStore()}}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	moved := mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	text := "blocked on " + moved.ID + " until it lands"
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "work", Type: "task", Description: text})
	if err := source.MemStore.DepAdd(work.ID, moved.ID, "related"); err != nil {
		t.Fatal(err)
	}
	if _, ok := beads.As[beads.BatchDeleter](source); !ok {
		t.Fatal("precondition: the double does not delete in batches, so this proves nothing about the batched path")
	}
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	var log bytes.Buffer
	if report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log); report.Outcome != infraMigrationConverged || report.Clear.Cleared != 1 {
		t.Fatalf("migrate = %s (cleared %d): %s", report.Outcome, report.Clear.Cleared, log.String())
	}
	got, err := source.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != text {
		t.Fatalf("the batched clear rewrote work bead %s:\n got %q\nwant %q", work.ID, got.Description, text)
	}
}
