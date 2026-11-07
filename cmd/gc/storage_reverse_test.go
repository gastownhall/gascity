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
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// reverseCityTOMLText is a split city's city.toml, with a comment the
// reverse must keep, an optional [daemon] session_reconciler, and its rigs.
func reverseCityTOMLText(reconciler string, rigs ...config.Rig) string {
	var b strings.Builder
	b.WriteString("# a city that was split\n[workspace]\nname = \"reverse-city\"\n\n")
	if reconciler != "" {
		fmt.Fprintf(&b, "[daemon]\n# pinned by the operator\nsession_reconciler = %q # keep this comment\n\n", reconciler)
	}
	for _, rig := range rigs {
		fmt.Fprintf(&b, "[[rigs]]\nname = %q\npath = %q\nprefix = %q\n\n", rig.Name, rig.Path, rig.Prefix)
	}
	b.WriteString("# the split\n[storage.classes]\nwork = \"work\"\ngraph = \"infra\"\nsessions = \"infra\"\nmessaging = \"infra\"\norders = \"infra\"\nnudges = \"infra\"\n\n")
	b.WriteString("[storage.bindings.infra]\nprovider = \"sqlite-beads\"\npath = \".gc/store\"\n\n# trailing comment\n")
	return b.String()
}

// reverseEngineCity is an MC-shaped city on a real SQLite work store,
// migrated forward and cleared, with its city.toml on disk: the state a
// reverse starts from.
type reverseEngineCity struct {
	engineCity
	request storageOperatorRequest
	// before is the work store as the forward migration found it.
	before map[string]beads.Bead
}

func newReverseEngineCity(t *testing.T, rows int, reconciler string) reverseEngineCity {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	smallEngineBatches(t)
	city := newEngineCity(t, rows, "sqlite")
	if err := os.WriteFile(filepath.Join(city.cityPath, "city.toml"), []byte(reverseCityTOMLText(reconciler)), 0o644); err != nil {
		t.Fatal(err)
	}
	// GC_BEADS=file names a file store, whose exact snapshot copies its file;
	// the rows themselves are read from the stubbed work store.
	if err := os.MkdirAll(filepath.Join(city.cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city.cityPath, ".gc", "beads.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city.cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loading the city: %v", err)
	}
	stubInfraControllerPing(t, 0)
	before := map[string]beads.Bead{}
	for _, b := range rigTestList(t, city.source) {
		before[b.ID] = b
	}
	if report, said := city.migrate(t); report.Outcome != infraMigrationConverged {
		t.Fatalf("the forward migration = %v: %s", report.Outcome, said)
	}
	// The served-binding note, as the command and the first boot write it.
	plan, err := resolveCityStoragePlan(city.cityPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordServedBinding(plan, city.cityPath, cfg.EffectiveStorage(), city.target); err != nil {
		t.Fatal(err)
	}
	return reverseEngineCity{engineCity: city, request: storageOperatorRequest{CityPath: city.cityPath, Cfg: cfg, FleetStopped: true}, before: before}
}

func (c reverseEngineCity) reverse(t *testing.T, opts reverseOptions) (int, string, string) {
	t.Helper()
	if opts.Mode == "" {
		opts.Mode = reverseToWork
	}
	var stdout, stderr bytes.Buffer
	code := doStorageReverse(context.Background(), c.request, opts, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func (c reverseEngineCity) abort(t *testing.T) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := doStorageAbortReverse(context.Background(), c.request, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// reverseStampKeys are the keys a migration and its reverse own, which a
// round trip compares modulo.
var reverseStampKeys = []string{infraMigrationStampKey, beadmeta.InfraReversedFromMetadataKey, beadmeta.MigratedCrossEdgesMetadataKey}

func withoutReverseStamps(b beads.Bead) beads.Bead {
	metadata := make(beads.StringMap, len(b.Metadata))
	for k, v := range b.Metadata {
		metadata[k] = v
	}
	b.Metadata = metadata
	for _, key := range reverseStampKeys {
		delete(b.Metadata, key)
	}
	b.IsBlocked, b.Dependencies = nil, nil
	return b
}

// assertReversedNotLossy proves a reverse carried everything back: the work
// store holds every row the forward migration found, byte-equal modulo the
// migrations' stamps, each infrastructure row stamped with the reverse's
// stamp, every original edge with its type and payload and no other, and no
// cross-store gate.
func (c reverseEngineCity) assertReversedNotLossy(t *testing.T) {
	t.Helper()
	got := map[string]beads.Bead{}
	for _, b := range rigTestList(t, c.source) {
		got[b.ID] = b
	}
	if len(got) != len(c.before) {
		t.Fatalf("the work store holds %d rows after the reverse, want the %d it held before the split", len(got), len(c.before))
	}
	for id, want := range c.before {
		g, ok := got[id]
		if !ok {
			t.Fatalf("the work store lost %s", id)
		}
		w, gg := withoutReverseStamps(want), withoutReverseStamps(g)
		if want.Metadata[beadmeta.MigratedCrossEdgesMetadataKey] == "" && g.Metadata[beadmeta.MigratedCrossEdgesMetadataKey] != "" {
			// The forward clear recorded the edges it dropped on this work
			// bead, which bumped its updated_at; the reverse leaves it be.
			w.UpdatedAt, gg.UpdatedAt = time.Time{}, time.Time{}
		}
		if diff := beadCopyDifference(w, gg); diff != "" {
			t.Fatalf("row %s differs after the round trip: %s", id, diff)
		}
		stamped := g.Metadata[beadmeta.InfraReversedFromMetadataKey] != ""
		if stamped != isInfra(want) {
			t.Fatalf("row %s carries the reverse stamp = %v, want %v", id, stamped, isInfra(want))
		}
		if g.Metadata[infraMigrationStampKey] != "" || (isInfra(want) && g.Metadata[beadmeta.MigratedCrossEdgesMetadataKey] != "") {
			t.Fatalf("row %s keeps a forward migration key: %v", id, g.Metadata)
		}
	}
	reader := c.source.(beads.DepMetadataReader)
	wantEdges := map[[2]string]string{}
	for _, d := range c.deps {
		wantEdges[[2]string{d.IssueID, d.DependsOnID}] = d.Type
	}
	gotEdges := 0
	for id := range got {
		deps, err := c.source.DepList(id, "down")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range deps {
			kind, ok := wantEdges[[2]string{id, d.DependsOnID}]
			if !ok || kind != d.Type {
				t.Fatalf("the work store holds edge %s -[%s]-> %s, want %q", id, d.Type, d.DependsOnID, kind)
			}
			gotEdges++
			payload, carried, err := reader.DepMetadata(id, d.DependsOnID)
			if err != nil {
				t.Fatal(err)
			}
			wantPayload, wantCarried := c.payloads[[2]string{id, d.DependsOnID}]
			if carried != wantCarried || payload != wantPayload {
				t.Fatalf("edge %s -> %s carries (%q, %v), want (%q, %v)", id, d.DependsOnID, payload, carried, wantPayload, wantCarried)
			}
		}
	}
	if gotEdges != len(wantEdges) {
		t.Fatalf("the work store holds %d edges after the reverse, want the %d it held before the split", gotEdges, len(wantEdges))
	}
}

// assertRevertedCityServesSingleStore proves a finished --to-work: no
// [storage], the binding archived, the split's notes renamed, the journal
// complete, and boot serving the work store with no hold.
func (c reverseEngineCity) assertRevertedCityServesSingleStore(t *testing.T) {
	t.Helper()
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(c.cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage != nil {
		t.Fatal("city.toml still configures [storage] after the reverse")
	}
	if fileExists(c.target.Root) {
		t.Fatalf("the binding root %s is still in place", c.target.Root)
	}
	archived, _ := filepath.Glob(c.target.Root + ".reverted-*")
	if len(archived) != 1 {
		t.Fatalf("the binding root was archived as %v, want one *.reverted-<run>", archived)
	}
	for _, path := range []string{bornSplitServedNotePath(c.cityPath), infraClearedNotePath(c.cityPath)} {
		if fileExists(path) {
			t.Fatalf("%s is still in place after the reverse", path)
		}
	}
	journal, present, err := readStorageReverseJournal(c.cityPath)
	if err != nil || !present || journal.Phase != reverseComplete {
		t.Fatalf("the journal = %+v (present %v, %v), want complete", journal, present, err)
	}
	routes, err := storageBootGate(c.cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil || routes != nil {
		t.Fatalf("boot on the reverted city = (%v, %v), want the single-store identity path", routes, err)
	}
	if code, _, _ := c.reverse(t, reverseOptions{}); code == 0 {
		t.Fatal("a second --to-work on a reverted city did not refuse")
	}
}

// TestReverseToWorkRoundTripsAnMCShapedCity: forward then --to-work leaves the
// work store as the forward migration found it, edges and payloads included,
// and the city serving single-store.
func TestReverseToWorkRoundTripsAnMCShapedCity(t *testing.T) {
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	floor := filepath.Join(city.cityPath, ".gc", reverseMinGCVersionName)
	if err := os.WriteFile(floor, []byte("1.6.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := city.reverse(t, reverseOptions{})
	if code != 0 {
		t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
	}
	city.assertReversedNotLossy(t)
	city.assertRevertedCityServesSingleStore(t)
	if fileExists(floor) {
		t.Fatal(".gc/min-gc-version survived the reverse: an older gc could not serve the single-store city")
	}
	journal, _, _ := readStorageReverseJournal(city.cityPath)
	if _, err := os.Stat(filepath.Join(journal.SnapshotTo, journal.Snapshot, storageSnapshotPinName)); err != nil {
		t.Fatalf("the mandatory snapshot is not pinned: %v", err)
	}
	m, err := readSnapshotManifest(filepath.Join(journal.SnapshotTo, journal.Snapshot))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, rec := range m.Stores {
		names = append(names, rec.Name)
	}
	if strings.Join(names, ",") != "binding,work" {
		t.Fatalf("the mandatory snapshot holds %v, want the binding and the work store", names)
	}
	// The pre-split city.toml had no [storage]; R4 removed it in place and
	// kept every other line.
	data, _ := os.ReadFile(filepath.Join(city.cityPath, "city.toml"))
	if strings.Contains(string(data), "[storage") || !strings.Contains(string(data), "# a city that was split") || !strings.Contains(string(data), `name = "reverse-city"`) {
		t.Fatalf("city.toml after the reverse:\n%s", data)
	}
}

// reverseBoundaries are the seam points one reverse passes, in order.
func reverseBoundaries(t *testing.T) []string {
	t.Helper()
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	var points []string
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(point string) error { points = append(points, point); return nil }
	defer func() { infraEngineCrashPoint = prev }()
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("dry run = %d: %s%s", code, stdout, stderr)
	}
	return points
}

// TestReverseResumesFromEveryBoundary kills the reverse at every phase and
// batch boundary: boot refuses the city from the moment the journal exists,
// the next run finishes it, and nothing is lost.
func TestReverseResumesFromEveryBoundary(t *testing.T) {
	points := reverseBoundaries(t)
	for _, want := range []string{"reverse city prepare", "reverse city copying rows 100/", "reverse city copying edges 40/", "reverse city verify", "reverse phase", "reverse R4 config", "reverse R5 record removed"} {
		found := false
		for _, p := range points {
			found = found || strings.HasPrefix(p, want)
		}
		if !found {
			t.Fatalf("the reverse passes no %q boundary: %v", want, points)
		}
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			city := newReverseEngineCity(t, 300, "")
			closeBindingGates(t, city)
			killed := errors.New("killed at " + point)
			prev := infraEngineCrashPoint
			infraEngineCrashPoint = func(p string) error {
				if p == point {
					return killed
				}
				return nil
			}
			code, _, stderr := city.reverse(t, reverseOptions{})
			infraEngineCrashPoint = prev
			if code == 0 || !strings.Contains(stderr, killed.Error()) {
				t.Fatalf("the kill at %s did not stop the run: %d: %s", point, code, stderr)
			}
			if point == "reverse phase "+reversePhaseName(reverseComplete) {
				// Killed once the journal recorded the end: nothing is left.
				city.assertReversedNotLossy(t)
				city.assertRevertedCityServesSingleStore(t)
				return
			}
			if _, err := storageBootGate(city.cityPath, city.request.Cfg, "gc start", nil, io.Discard); err == nil || !strings.Contains(err.Error(), "reverse migration") {
				t.Fatalf("boot after a kill at %s = %v, want the reverse's refusal", point, err)
			}
			if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
				t.Fatalf("the run after a kill at %s = %d: %s%s", point, code, stdout, stderr)
			}
			city.assertReversedNotLossy(t)
			city.assertRevertedCityServesSingleStore(t)
		})
	}
}

// TestReverseTimeBudgetPausesAndResumes: a spent budget stops the reverse at a
// batch boundary with exit 75, and reruns finish it without loss.
func TestReverseTimeBudgetPausesAndResumes(t *testing.T) {
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	city.request.TimeBudget = time.Nanosecond
	paused := 0
	for {
		code, stdout, stderr := city.reverse(t, reverseOptions{})
		if code == 0 {
			break
		}
		if code != storagePausedExitCode || !strings.Contains(stderr, "paused in reverse") {
			t.Fatalf("a run on a spent budget = %d: %s%s", code, stdout, stderr)
		}
		paused++
		if paused > 50 {
			t.Fatal("the reverse never finished on reruns")
		}
	}
	if paused < 2 {
		t.Fatalf("the reverse paused %d time(s), want one per batch boundary it crossed", paused)
	}
	city.assertReversedNotLossy(t)
	city.assertRevertedCityServesSingleStore(t)
}

// workStoreFingerprint is every row and edge a store holds, rendered.
func workStoreFingerprint(t *testing.T, store beads.Store) string {
	t.Helper()
	rows := rigTestList(t, store)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	var b strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&b, "%s %s %s %v\n", row.ID, row.Status, row.Title, row.Metadata)
		deps, err := store.DepList(row.ID, "down")
		if err != nil {
			t.Fatal(err)
		}
		edges := censusEdges(deps)
		sort.Strings(edges)
		fmt.Fprintf(&b, "  %v\n", edges)
	}
	return b.String()
}

// TestAbortReverseRemovesExactlyTheStampedRows kills a reverse after it
// wrote rows (and after R4 rewrote the config), then abandons it: the work
// store is exactly as it was, a row carrying another run's stamp and an
// unstamped row survive, R4's changes are put back, and boot serves the split
// again.
func TestAbortReverseRemovesExactlyTheStampedRows(t *testing.T) {
	for _, point := range []string{"reverse city copying rows 100/", "reverse phase " + reversePhaseName(reverseR2), "reverse R4 config"} {
		t.Run(point, func(t *testing.T) {
			city := newReverseEngineCity(t, 300, "")
			closeBindingGates(t, city)
			tomlBefore, _ := os.ReadFile(filepath.Join(city.cityPath, "city.toml"))
			if err := os.WriteFile(filepath.Join(city.cityPath, ".gc", reverseMinGCVersionName), []byte("1.6.0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			under, _, _ := unwrapBeadPolicyStore(city.source)
			creator := under.(beads.ForeignIDCreator)
			if _, err := creator.CreateWithForeignID(beads.Bead{ID: "mc-other-run", Title: "an earlier run's", Type: "task", Status: "closed", Metadata: beads.StringMap{beadmeta.InfraReversedFromMetadataKey: "infra/20260101T000000Z"}}); err != nil {
				t.Fatal(err)
			}
			before := workStoreFingerprint(t, city.source)
			prev := infraEngineCrashPoint
			infraEngineCrashPoint = func(p string) error {
				if strings.HasPrefix(p, point) {
					return errors.New("killed")
				}
				return nil
			}
			code, _, stderr := city.reverse(t, reverseOptions{})
			infraEngineCrashPoint = prev
			if code == 0 {
				t.Fatalf("the kill at %s did not stop the reverse: %s", point, stderr)
			}
			if after := workStoreFingerprint(t, city.source); after == before {
				t.Fatal("the killed reverse wrote nothing, so the abort proves nothing")
			}
			code, stdout, stderr := city.abort(t)
			if code != 0 {
				t.Fatalf("--abort-reverse = %d: %s%s", code, stdout, stderr)
			}
			if after := workStoreFingerprint(t, city.source); after != before {
				t.Fatalf("the abort did not leave the work store as it was:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if got, _ := os.ReadFile(filepath.Join(city.cityPath, "city.toml")); !bytes.Equal(got, tomlBefore) {
				t.Fatalf("the abort did not put city.toml back:\n%s", got)
			}
			for _, path := range []string{bornSplitServedNotePath(city.cityPath), infraClearedNotePath(city.cityPath), filepath.Join(city.cityPath, ".gc", reverseMinGCVersionName), city.target.MarkerPath()} {
				if !fileExists(path) {
					t.Fatalf("%s is gone after the abort", path)
				}
			}
			for _, path := range []string{storageReverseJournalPath(city.cityPath), filepath.Join(city.target.Root, infraRevertingRecordName), filepath.Join(city.target.Root, infraReversedManifestName)} {
				if fileExists(path) {
					t.Fatalf("%s is left behind by the abort", path)
				}
			}
			if _, err := storageBootGate(city.cityPath, city.request.Cfg, "gc start", nil, io.Discard); err != nil {
				t.Fatalf("boot after the abort = %v, want the split served again", err)
			}
		})
	}
}

// TestAbortReverseRefusesOnceTheBindingIsArchived: R5 has begun, so the only
// way out is forward.
func TestAbortReverseRefusesOnceTheBindingIsArchived(t *testing.T) {
	city := newReverseEngineCity(t, 120, "")
	closeBindingGates(t, city)
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse R5 archived" {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ := city.reverse(t, reverseOptions{})
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the reverse")
	}
	if code, _, stderr := city.abort(t); code == 0 || !strings.Contains(stderr, "can only be finished") {
		t.Fatalf("--abort-reverse after R5 = %d: %s", code, stderr)
	}
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("finishing = %d: %s%s", code, stdout, stderr)
	}
	city.assertRevertedCityServesSingleStore(t)
}

// TestReverseRefusesAnExplicitV2Reconciler: an explicit v2 would refuse the
// reverted city's boot (C0.7), so --to-work refuses unless --reconciler=legacy
// rewrites the key in R4's edit, which keeps the line's comment.
func TestReverseRefusesAnExplicitV2Reconciler(t *testing.T) {
	city := newReverseEngineCity(t, 60, "v2")
	closeBindingGates(t, city)
	before := workStoreFingerprint(t, city.source)
	code, _, stderr := city.reverse(t, reverseOptions{})
	if code == 0 || !strings.Contains(stderr, "--reconciler=legacy") {
		t.Fatalf("--to-work on an explicit v2 city = %d: %s", code, stderr)
	}
	if after := workStoreFingerprint(t, city.source); after != before || fileExists(storageReverseJournalPath(city.cityPath)) {
		t.Fatal("the refusal wrote something")
	}
	if code, stdout, stderr := city.reverse(t, reverseOptions{Reconciler: "legacy"}); code != 0 {
		t.Fatalf("--to-work --reconciler=legacy = %d: %s%s", code, stdout, stderr)
	}
	data, _ := os.ReadFile(filepath.Join(city.cityPath, "city.toml"))
	if !strings.Contains(string(data), `session_reconciler = "legacy" # keep this comment`) || !strings.Contains(string(data), "# pinned by the operator") {
		t.Fatalf("the reconciler was not rewritten in place:\n%s", data)
	}
	city.assertRevertedCityServesSingleStore(t)
}

// TestReverseRestoresThePreSplitCityTOML: with the split journal holding the
// pre-split bytes and nothing else changed since, R4 writes those bytes back;
// once something else changed, it removes [storage] in place instead.
func TestReverseRestoresThePreSplitCityTOML(t *testing.T) {
	for _, tc := range []struct{ changed, badSum bool }{{false, false}, {true, false}, {false, true}} {
		changed := tc.changed || tc.badSum
		t.Run(fmt.Sprintf("changed=%v badSum=%v", tc.changed, tc.badSum), func(t *testing.T) {
			city := newReverseEngineCity(t, 60, "")
			closeBindingGates(t, city)
			path := filepath.Join(city.cityPath, "city.toml")
			current, _ := os.ReadFile(path)
			removed, err := config.RemoveStorageFromCityTOML(current)
			if err != nil {
				t.Fatal(err)
			}
			pre := strings.Replace(string(removed), "# a city that was split", "# the operator's own layout", 1)
			if tc.changed {
				pre = strings.Replace(pre, `name = "reverse-city"`, `name = "renamed-since"`, 1)
			}
			sum := sha256Hex([]byte(pre))
			if tc.badSum {
				sum = sha256Hex([]byte("other bytes"))
			}
			journal := fmt.Sprintf(`{"city_toml_before": %q, "city_toml_before_sha256": %q}`, pre, sum)
			if err := os.WriteFile(filepath.Join(city.cityPath, ".gc", storageSplitJournalName), []byte(journal), 0o644); err != nil {
				t.Fatal(err)
			}
			if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
				t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
			}
			got, _ := os.ReadFile(path)
			if want := !changed; (string(got) == pre) != want {
				t.Fatalf("restored the pre-split bytes = %v, want %v:\n%s", string(got) == pre, want, got)
			}
			if changed && !bytes.Equal(got, removed) {
				t.Fatalf("[storage] was not removed in place:\n%s", got)
			}
		})
	}
}

// TestReverseRefusesWhatItCannotCarry: an open cross-store gate, an open
// binding-born rig-scope row (D8) and an edge payload the work store cannot
// write each refuse by id, naming the fix, and nothing is written.
func TestReverseRefusesWhatItCannotCarry(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, city reverseEngineCity)
		want  []string
	}{
		{"open gate", func(*testing.T, reverseEngineCity) {}, []string{"open cross-store gate", "gc workflow delete"}},
		{"open binding-born rig-scope row", func(t *testing.T, city reverseEngineCity) {
			closeBindingGates(t, city)
			createInBinding(t, city, beads.Bead{Title: "minted for a rig", Type: "molecule", Metadata: beads.StringMap{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.RootStoreRefMetadataKey: "rig:alpha"}}, false)
		}, []string{"(D8)", "rig:alpha"}},
		{"payload into a store without a writer", func(t *testing.T, city reverseEngineCity) {
			closeBindingGates(t, city)
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) { return payloadMuteDestination{Store: city.source}, nil }
			t.Cleanup(func() { openInfraMigrationSource = prev })
		}, []string{"carries a payload", "native Dolt has no edge-payload writer"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city := newReverseEngineCity(t, 300, "")
			tc.setup(t, city)
			before := workStoreFingerprint(t, city.source)
			code, _, stderr := city.reverse(t, reverseOptions{})
			if code == 0 {
				t.Fatal("the reverse did not refuse")
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Fatalf("the refusal does not say %q: %s", want, stderr)
				}
			}
			if after := workStoreFingerprint(t, city.source); after != before {
				t.Fatal("the refused reverse wrote to the work store")
			}
			if fileExists(storageReverseJournalPath(city.cityPath)) {
				t.Fatal("the refused reverse wrote its journal")
			}
		})
	}
}

// closeBindingGates closes every cross-store gate the forward copy minted, as
// the release lane does once their blockers close.
func closeBindingGates(t *testing.T, city reverseEngineCity) {
	t.Helper()
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
	for _, b := range rigTestList(t, binding) {
		if isCrossStoreGate(b) && rigRowOpen(b) {
			if err := binding.Close(b.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// createInBinding creates b in the binding as the runtime does after the
// cutover (a binding-born row), closed when closed is set.
func createInBinding(t *testing.T, city reverseEngineCity, b beads.Bead, closed bool) beads.Bead {
	t.Helper()
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
	created := mustCreateInfraBead(t, binding, b)
	if closed {
		if err := binding.Close(created.ID); err != nil {
			t.Fatal(err)
		}
		if created, err = binding.Get(created.ID); err != nil {
			t.Fatal(err)
		}
	}
	return created
}

// payloadMuteDestination is a work store that can read payloads but not
// write one, as native Dolt is today.
type payloadMuteDestination struct{ beads.Store }

func (s payloadMuteDestination) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	return s.Store.(beads.DepMetadataReader).DepMetadata(issueID, dependsOnID)
}

func (s payloadMuteDestination) CreateWithForeignID(b beads.Bead) (beads.Bead, error) {
	return s.Store.(beads.ForeignIDCreator).CreateWithForeignID(b)
}

// TestReverseClosedGatesAndD8: closed gates are not carried and their
// owners' records restore the edges they stood for; a closed binding-born
// rig-scope row goes to the city store, readable by id.
func TestReverseClosedGatesAndD8(t *testing.T) {
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	born := createInBinding(t, city, beads.Bead{Title: "minted for a rig", Type: "molecule", Metadata: beads.StringMap{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.RootStoreRefMetadataKey: "rig:alpha"}}, true)
	code, stdout, stderr := city.reverse(t, reverseOptions{})
	if code != 0 {
		t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
	}
	got, err := city.source.Get(born.ID)
	if err != nil || got.Status != "closed" || got.Metadata[beadmeta.RootStoreRefMetadataKey] != "rig:alpha" {
		t.Fatalf("the closed rig-scope row in the city store = %+v, %v", got, err)
	}
	for _, b := range rigTestList(t, city.source) {
		if isCrossStoreGate(b) {
			t.Fatalf("the reverse carried the gate %s", b.ID)
		}
	}
	if !strings.Contains(stdout, "closed cross-store gate(s) not carried") {
		t.Fatalf("the reverse did not report the gates: %s", stdout)
	}
	// Every original edge is back, gated ones included.
	city.before[born.ID] = got
	city.assertReversedNotLossy(t)
}

// TestReverseDryRunWritesNothing: --dry-run prints the plan and writes
// nothing at all.
func TestReverseDryRunWritesNothing(t *testing.T) {
	city := newReverseEngineCity(t, 120, "")
	closeBindingGates(t, city)
	before := workStoreFingerprint(t, city.source)
	code, stdout, stderr := city.reverse(t, reverseOptions{DryRun: true})
	if code != 0 || !strings.Contains(stdout, "back into the city work store") || !strings.Contains(stdout, "Dry run") {
		t.Fatalf("--dry-run = %d: %s%s", code, stdout, stderr)
	}
	if after := workStoreFingerprint(t, city.source); after != before {
		t.Fatal("--dry-run wrote to the work store")
	}
	for _, path := range []string{storageReverseJournalPath(city.cityPath), filepath.Join(city.target.Root, infraRevertingRecordName), storageSnapshotDefaultDir(city.cityPath)} {
		if fileExists(path) {
			t.Fatalf("--dry-run wrote %s", path)
		}
	}
}

// TestReverseHoldsEveryOtherStorageVerb: while a reverse is open, the forward
// migration, the rig import and the binding restore refuse, naming it.
func TestReverseHoldsEveryOtherStorageVerb(t *testing.T) {
	city := newReverseEngineCity(t, 120, "")
	closeBindingGates(t, city)
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if strings.HasPrefix(p, "reverse city verify") {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ := city.reverse(t, reverseOptions{})
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the reverse")
	}
	journal, _, _ := readStorageReverseJournal(city.cityPath)
	if !fileExists(filepath.Join(city.target.Root, infraRevertingRecordName)) {
		t.Fatal("an open reverse left no infra.reverting beside the binding")
	}
	var stdout, stderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), city.request, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "reverse migration") {
		t.Fatalf("migrate --from-work during a reverse = %d: %s", code, stderr.String())
	}
	stderr.Reset()
	if code := doStorageMigrateFromRigs(context.Background(), city.request, nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "reverse migration") {
		t.Fatalf("migrate --from-rigs during a reverse = %d: %s", code, stderr.String())
	}
	stderr.Reset()
	if code := doStorageSnapshotRestore(context.Background(), city.request, journal.SnapshotTo, journal.Snapshot, "restore", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "reverse migration") {
		t.Fatalf("restore during a reverse = %d: %s", code, stderr.String())
	}
	stdout.Reset()
	if code := doStorageStatusCity(city.request, &stdout, &stderr); code == 0 || !strings.Contains(stdout.String(), "reverse: BLOCKED") {
		t.Fatalf("status during a reverse = %d: %s", code, stdout.String())
	}
	if code, out, errOut := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("finishing = %d: %s%s", code, out, errOut)
	}
	// On the reverted city the binding restore finds no binding to restore.
	stderr.Reset()
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(city.cityPath, io.Discard)
	reverted := storageOperatorRequest{CityPath: city.cityPath, Cfg: cfg, FleetStopped: true}
	if code := doStorageSnapshotRestore(context.Background(), reverted, journal.SnapshotTo, journal.Snapshot, "restore", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "no binding to restore") {
		t.Fatalf("restore on a reverted city = %d: %s", code, stderr.String())
	}
}

// TestReverseKeepsIDsAndRaisesTheFloor: reversed rows keep their ids, and the
// work store's id floor covers the ids of its own prefix the reverse put back
// (S5-D's floor, never lowered).
func TestReverseKeepsIDsAndRaisesTheFloor(t *testing.T) {
	city := newReverseEngineCity(t, 120, "")
	closeBindingGates(t, city)
	conf := &fakeRigHashConfig{values: map[string]string{bdMinHashLengthKey: "3"}}
	prev := cityHashConfigOpener
	cityHashConfigOpener = func(string) (cityHashConfig, error) { return cityHashConfig{Conf: conf, Prefix: "mc"}, nil }
	t.Cleanup(func() { cityHashConfigOpener = prev })
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
	}
	if conf.values[bdMinHashLengthKey] != "7" {
		t.Fatalf("min_hash_length = %q after the reverse, want 7 (one past the 6-character suffixes of the mc- ids it put back)", conf.values[bdMinHashLengthKey])
	}
	for id, b := range city.before {
		if _, err := city.source.Get(id); err != nil {
			t.Fatalf("the reverse did not keep %s (%s): %v", id, coordclass.Classify(b), err)
		}
	}
}

// TestReverseRefusesAnExecWorkStoreWithoutImport: a generic exec work store
// whose provider offers no import op cannot take a bead under its own id, so
// --to-work refuses it by name before anything is written, and names the
// snapshot as the way back.
func TestReverseRefusesAnExecWorkStoreWithoutImport(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	script := filepath.Join(t.TempDir(), "provider.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncase \"$1\" in\n  list) echo '[]' ;;\n  *) exit 2 ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return beadsexec.NewStore(script), nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	code, _, stderr := city.reverse(t, reverseOptions{})
	if code == 0 || !strings.Contains(stderr, "offers no import op") || !strings.Contains(stderr, "restoring the snapshot") {
		t.Fatalf("--to-work into an exec store without import = %d: %s", code, stderr)
	}
	if fileExists(storageReverseJournalPath(city.cityPath)) {
		t.Fatal("the refused reverse wrote its journal")
	}
}

// TestStorageMigrateReverseFlags pins the flag rules of the reverse verbs:
// one source, --reconciler with --to-work, --dry-run with a reverse, no
// budget on the abort, and a --rig a reverse takes only with --to-rigs.
func TestStorageMigrateReverseFlags(t *testing.T) {
	surface, err := parseOperatorCommandSpelling(storageMigrationCommand)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--" + storageToWorkFlag, "--" + storageToRigsFlag}, "pass exactly one of"},
		{[]string{"--" + storageAbortReverseFlag, "--" + surface.Flag}, "pass exactly one of"},
		{[]string{"--" + storageToRigsFlag, "--" + storageReconcilerFlag + "=legacy"}, "--" + storageReconcilerFlag + " goes with --" + storageToWorkFlag},
		{[]string{"--" + surface.Flag, "--" + storageDryRunFlag}, "--" + storageDryRunFlag + " with"},
		{[]string{"--" + storageAbortReverseFlag, "--" + storageTimeBudgetFlag + "=1m"}, "refused rather than ignored"},
		{[]string{"--" + storageAbortReverseFlag, "--" + storageRigFlag, "r"}, "means nothing without it"},
	} {
		var stdout, stderr bytes.Buffer
		cmd := newStorageMigrateCmd(surface, &stdout, &stderr)
		cmd.SetArgs(tc.args)
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		if err := cmd.Execute(); err == nil || !strings.Contains(stderr.String(), tc.want) {
			t.Fatalf("migrate %v = %v: %s, want %q", tc.args, err, stderr.String(), tc.want)
		}
	}
	city := newReverseEngineCity(t, 30, "")
	for _, tc := range []struct {
		opts reverseOptions
		want string
	}{
		{reverseOptions{Mode: reverseToRigs}, "needs at least one --" + storageRigFlag},
		{reverseOptions{Mode: reverseToWork, Rigs: []string{"packs"}}, "returns every rig with the city"},
		{reverseOptions{Mode: reverseToWork, Reconciler: "v2"}, "takes \"legacy\""},
	} {
		if code, _, stderr := city.reverse(t, tc.opts); code == 0 || !strings.Contains(stderr, tc.want) {
			t.Fatalf("reverse %+v = %d: %s, want %q", tc.opts, code, stderr, tc.want)
		}
	}
	if code, _, stderr := city.abort(t); code == 0 || !strings.Contains(stderr, "no reverse migration is in progress") {
		t.Fatalf("--abort-reverse with nothing open = %d: %s", code, stderr)
	}
}

// TestReverseResumeRecopiesWhatTheBindingChanged: a row an earlier run copied
// is kept only while it still equals the binding's row with exactly its
// planned edges; one the binding changed since, or whose edge the binding
// dropped, is deleted and copied again, and the reverse ends equal to the
// binding as it is now.
func TestReverseResumeRecopiesWhatTheBindingChanged(t *testing.T) {
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse city verify" {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ := city.reverse(t, reverseOptions{})
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the reverse")
	}
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	var retitled, unlinked, from string
	for _, d := range city.deps {
		if isInfra(city.before[d.IssueID]) && isInfra(city.before[d.DependsOnID]) && d.Type == "blocks" && unlinked == "" {
			from, unlinked = d.IssueID, d.DependsOnID
		}
	}
	for id, b := range city.before {
		if isInfra(b) && b.Type == "message" && retitled == "" {
			retitled = id
		}
	}
	title := "retitled in the binding"
	if err := binding.Update(retitled, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := binding.DepRemove(from, unlinked); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("the rerun = %d: %s%s", code, stdout, stderr)
	}
	got, err := city.source.Get(retitled)
	if err != nil || got.Title != title {
		t.Fatalf("%s in the work store = %+v, %v; want the binding's new title", retitled, got, err)
	}
	deps, err := city.source.DepList(from, "down")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deps {
		if d.DependsOnID == unlinked {
			t.Fatalf("the work store keeps %s -> %s, which the binding dropped", from, unlinked)
		}
	}
}

// TestReverseVerifyCatchesAFaultyCopy: a destination that writes something
// other than the plan (a changed field, an invented row, a dropped edge, a
// changed payload) fails the reverse at its verify, before the phase is
// recorded done, and the next run with an honest store repairs it.
func TestReverseVerifyCatchesAFaultyCopy(t *testing.T) {
	faults := map[string]func(*reverseTarget){
		"a changed field": func(target *reverseTarget) {
			create := target.create
			target.create = func(rows []beads.Bead) error {
				rows = append([]beads.Bead(nil), rows...)
				rows[0].Title = "not what the binding holds"
				return create(rows)
			}
		},
		"an invented row": func(target *reverseTarget) {
			create := target.create
			target.create = func(rows []beads.Bead) error {
				extra := rows[0]
				extra.ID = "mc-invented-" + rows[0].ID
				return create(append(append([]beads.Bead(nil), rows...), extra))
			}
		},
		"a dropped edge": func(target *reverseTarget) {
			add := target.addEdges
			target.addEdges = func(edges []beads.DepWithPayload) error { return add(edges[1:]) }
		},
		"a changed payload": func(target *reverseTarget) {
			add := target.addEdges
			target.addEdges = func(edges []beads.DepWithPayload) error {
				edges = append([]beads.DepWithPayload(nil), edges...)
				for i := range edges {
					if edges[i].Payload != "" {
						edges[i].Payload = `{"gate":"changed"}`
					}
				}
				return add(edges)
			}
		},
	}
	for name, fault := range faults {
		t.Run(name, func(t *testing.T) {
			city := newReverseEngineCity(t, 300, "")
			closeBindingGates(t, city)
			prev := reverseTargetOpener
			reverseTargetOpener = func(cityPath string, cfg *config.City, dest string) (*reverseTarget, error) {
				target, err := openReverseTarget(cityPath, cfg, dest)
				if err == nil {
					fault(target)
				}
				return target, err
			}
			code, _, stderr := city.reverse(t, reverseOptions{})
			reverseTargetOpener = prev
			if code == 0 || !strings.Contains(stderr, "verifying the city work store") {
				t.Fatalf("a faulty copy (%s) = %d: %s", name, code, stderr)
			}
			if journal, _, _ := readStorageReverseJournal(city.cityPath); journal.Phase >= reverseR2 {
				t.Fatalf("the journal recorded the city copied (phase %d) over a failed verify", journal.Phase)
			}
			if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
				t.Fatalf("the honest rerun = %d: %s%s", code, stdout, stderr)
			}
			city.assertReversedNotLossy(t)
		})
	}
}
