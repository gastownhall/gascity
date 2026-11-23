package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// reverseCityTOMLText is a split city's city.toml, with a comment the
// reverse must keep, an optional [daemon] session_reconciler, and its rigs.
func reverseCityTOMLText(reconciler string, rigs ...config.Rig) string {
	return reverseCityTOMLTextAt(".gc/store", reconciler, rigs...)
}

// reverseCityTOMLTextAt is reverseCityTOMLText with the binding's path at
// bindingPath.
func reverseCityTOMLTextAt(bindingPath, reconciler string, rigs ...config.Rig) string {
	var b strings.Builder
	b.WriteString("# a city that was split\n[workspace]\nname = \"reverse-city\"\n\n")
	if reconciler != "" {
		fmt.Fprintf(&b, "[daemon]\n# pinned by the operator\nsession_reconciler = %q # keep this comment\n\n", reconciler)
	}
	for _, rig := range rigs {
		fmt.Fprintf(&b, "[[rigs]]\nname = %q\npath = %q\nprefix = %q\n\n", rig.Name, rig.Path, rig.Prefix)
	}
	b.WriteString("# the split\n[storage.classes]\nwork = \"work\"\ngraph = \"infra\"\nsessions = \"infra\"\nmessaging = \"infra\"\norders = \"infra\"\nnudges = \"infra\"\n\n")
	fmt.Fprintf(&b, "[storage.bindings.infra]\nprovider = \"sqlite-beads\"\npath = %q\n\n# trailing comment\n", bindingPath)
	return b.String()
}

// reverseEngineCity is an MC-shaped city on a real SQLite work store,
// migrated forward and cleared, with its city.toml on disk: the state a
// reverse starts from.
type reverseEngineCity struct {
	engineCity
	request storageOperatorRequest
	// before is the work store as the forward migration found it, and
	// afterForward as the forward migration left it.
	before, afterForward map[string]beads.Bead
	// extraEdges are edges a test added to the work store beside the
	// fixture's own (c.deps).
	extraEdges map[[2]string]string
}

func newReverseEngineCity(t *testing.T, rows int, reconciler string) reverseEngineCity {
	t.Helper()
	return newReverseEngineCityAt(t, rows, reconciler, ".gc/store")
}

// newReverseEngineCityAt is newReverseEngineCity with the binding's path at
// bindingPath.
func newReverseEngineCityAt(t *testing.T, rows int, reconciler, bindingPath string) reverseEngineCity {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	smallEngineBatches(t)
	city := newEngineCity(t, rows, "sqlite")
	city.target = mustResolveInfraTarget(t, city.cityPath, infraSplitConfig(bindingPath))
	if err := os.WriteFile(filepath.Join(city.cityPath, "city.toml"), []byte(reverseCityTOMLTextAt(bindingPath, reconciler)), 0o644); err != nil {
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
	seedEveryCarriedField(t, city.source)
	before := map[string]beads.Bead{}
	for _, b := range rigTestList(t, city.source) {
		before[b.ID] = b
	}
	var said bytes.Buffer
	if report := migrateInfraClasses(t, city.cityPath, infraSplitConfig(bindingPath), &said); report.Outcome != infraMigrationConverged {
		t.Fatalf("the forward migration = %v: %s", report.Outcome, said.String())
	}
	// The served-binding note, as the command and the first boot write it.
	plan, err := resolveCityStoragePlan(city.cityPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordServedBinding(plan, city.cityPath, cfg.EffectiveStorage(), city.target); err != nil {
		t.Fatal(err)
	}
	afterForward := map[string]beads.Bead{}
	for _, b := range rigTestList(t, city.source) {
		afterForward[b.ID] = b
	}
	return reverseEngineCity{engineCity: city, request: storageOperatorRequest{CityPath: city.cityPath, Cfg: cfg, FleetStopped: true}, before: before, afterForward: afterForward, extraEdges: map[[2]string]string{}}
}

// seedEveryCarriedField adds infrastructure rows carrying every field a copy
// carries (priority, description, defer_until, step ref, close reason,
// no-history, a parent with no parent-child edge, as retry and Ralph beads
// are created) to the work store before the split.
func seedEveryCarriedField(t *testing.T, source beads.Store) {
	t.Helper()
	under, _, _ := unwrapBeadPolicyStore(source)
	creator := under.(beads.ForeignIDCreator)
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	deferred := at.Add(30 * 24 * time.Hour)
	one, zero := 1, 0
	for _, b := range []beads.Bead{
		{
			ID: "mc-f00001", Title: "a root with every field", Type: "molecule", Status: "closed", CloseReason: "finished", Priority: &one, Description: "the root's body",
			Metadata: beads.StringMap{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}, CreatedAt: at, UpdatedAt: at.Add(time.Hour),
		},
		{
			ID: "mc-f00002", Title: "a retry step", Type: "task", Status: "open", ParentID: "mc-f00001", Ref: "step-review", Priority: &zero, DeferUntil: &deferred, Description: "retry 2",
			Metadata: beads.StringMap{beadmeta.RootBeadIDMetadataKey: "mc-f00001"}, CreatedAt: at.Add(time.Minute), UpdatedAt: at.Add(2 * time.Minute),
		},
		{
			ID: "mc-f00003", Title: "a no-history session", Type: "session", Status: "closed", CloseReason: "drained", NoHistory: true, Labels: []string{"gc:session"},
			Metadata: beads.StringMap{"session_id": "mc-f00003"}, CreatedAt: at.Add(2 * time.Minute), UpdatedAt: at.Add(3 * time.Minute),
		},
	} {
		if _, err := creator.CreateWithForeignID(b); err != nil {
			t.Fatal(err)
		}
	}
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
		// An infrastructure row is the row the split found, but for the
		// reverse's own stamp: gc.infra_migrated_from and
		// gc.migrated_cross_edges compare too. A work row never moved, so it
		// is the row the forward migration left (its clear recorded the edges
		// it dropped on it).
		w := want
		if !isInfra(want) {
			w = c.afterForward[id]
		}
		gg := g
		gg.Metadata = beads.StringMap{}
		for k, v := range g.Metadata {
			if k != beadmeta.InfraReversedFromMetadataKey {
				gg.Metadata[k] = v
			}
		}
		gg.IsBlocked, w.IsBlocked = nil, nil
		if diff := reverseExactDifference(w, gg); diff != "" {
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
	for k, v := range c.extraEdges {
		wantEdges[k] = v
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

// reverseExactDifference is beadCopyDifference with every key compared,
// gc.infra_migrated_from included.
func reverseExactDifference(want, got beads.Bead) string {
	if diff := beadCopyDifference(want, got); diff != "" {
		return diff
	}
	if want.Metadata[infraMigrationStampKey] != got.Metadata[infraMigrationStampKey] {
		return fmt.Sprintf("%s %q != %q", infraMigrationStampKey, want.Metadata[infraMigrationStampKey], got.Metadata[infraMigrationStampKey])
	}
	return ""
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
	floor := minGCVersionPath(city.cityPath)
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
	infraEngineCrashPoint = func(point string) error {
		if point != reverseGuardedSeam {
			points = append(points, point)
		}
		return nil
	}
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
	for _, point := range []string{"reverse city copying rows 100/", "reverse phase " + reversePhaseName(reverseR2), "reverse R4 config", "reverse phase " + reversePhaseName(reverseR4)} {
		t.Run(point, func(t *testing.T) {
			city := newReverseEngineCity(t, 300, "")
			closeBindingGates(t, city)
			tomlBefore, _ := os.ReadFile(filepath.Join(city.cityPath, "city.toml"))
			if err := os.WriteFile(minGCVersionPath(city.cityPath), []byte("1.6.0\n"), 0o644); err != nil {
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
			for _, path := range []string{bornSplitServedNotePath(city.cityPath), infraClearedNotePath(city.cityPath), minGCVersionPath(city.cityPath), city.target.MarkerPath()} {
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
	// edited: the operator changed city.toml after the split (a comment
	// only, or a value), so the split's after-sum no longer matches.
	for _, tc := range []struct {
		name   string
		edit   func(string) string
		badSum bool
	}{
		{"unchanged since the split", nil, false},
		{"a comment edited since", func(s string) string { return strings.Replace(s, "# trailing comment", "# an operator's note", 1) }, false},
		{"a value edited since", func(s string) string { return strings.Replace(s, `name = "reverse-city"`, `name = "renamed-since"`, 1) }, false},
		{"a journal that does not prove", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newReverseEngineCity(t, 60, "")
			closeBindingGates(t, city)
			path := filepath.Join(city.cityPath, "city.toml")
			written, _ := os.ReadFile(path)
			pre := "# the operator's own layout\n[workspace]\nname = \"reverse-city\"\n"
			after := sha256Hex(written)
			if tc.edit != nil {
				if err := os.WriteFile(path, []byte(tc.edit(string(written))), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			current, _ := os.ReadFile(path)
			removed, err := config.RemoveStorageFromCityTOML(current)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256Hex([]byte(pre))
			if tc.badSum {
				sum = sha256Hex([]byte("other bytes"))
			}
			journal := fmt.Sprintf(`{"city_toml_before": %q, "city_toml_before_sha256": %q, "city_toml_after_sha256": %q}`, pre, sum, after)
			if err := os.WriteFile(filepath.Join(city.cityPath, ".gc", storageSplitJournalName), []byte(journal), 0o644); err != nil {
				t.Fatal(err)
			}
			if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
				t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
			}
			got, _ := os.ReadFile(path)
			restore := tc.edit == nil && !tc.badSum
			if (string(got) == pre) != restore {
				t.Fatalf("restored the pre-split bytes = %v, want %v:\n%s", string(got) == pre, restore, got)
			}
			if !restore && !bytes.Equal(got, removed) {
				t.Fatalf("[storage] was not removed in place, keeping every edit:\n%s", got)
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
// the release lane does once their blockers close (releaseCrossStoreGate).
func closeBindingGates(t *testing.T, city reverseEngineCity) {
	t.Helper()
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
	for _, b := range rigTestList(t, binding) {
		if isCrossStoreGate(b) && rigRowOpen(b) {
			releaseCrossStoreGate(t, binding, b)
		}
	}
}

// releaseCrossStoreGate closes gate as the release lane closes one whose
// blocker closed: one write carrying the status, the released marker and its
// reason, so it reads as closed by the controller
// (dispatch.IsControllerClosedCrossStoreGate). A plain close, with no marker
// and no controller reason, reads as an operator's.
func releaseCrossStoreGate(t *testing.T, store beads.Store, gate beads.Bead) {
	t.Helper()
	closed := "closed"
	if err := store.Update(gate.ID, beads.UpdateOpts{Status: &closed, Metadata: map[string]string{
		beadmeta.CrossStoreGateStateMetadataKey: dispatch.CrossStoreGateStateReleased,
		"close_reason":                          "blocker " + gate.Metadata[beadmeta.CrossStoreBlockerMetadataKey] + " closed",
	}}); err != nil {
		t.Fatal(err)
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
	want := got
	want.Metadata = beads.StringMap{}
	for k, v := range got.Metadata {
		if k != beadmeta.InfraReversedFromMetadataKey {
			want.Metadata[k] = v
		}
	}
	city.before[born.ID] = want
	city.assertReversedNotLossy(t)
}

// TestReverseToWorkGivesADrainsItemTheEdgeItsGateStoodFor: a gate the
// runtime mints in the binding after the cutover is a drain's
// (internal/dispatch/drain.go): owned by the drain control, which waits only
// on its item, and waited on by the item. --to-work gives the item back the
// edge the closed gate stood for, item -> blocker, and the control none.
func TestReverseToWorkGivesADrainsItemTheEdgeItsGateStoodFor(t *testing.T) {
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	blocker := ""
	for _, b := range city.rows {
		if !isInfra(b) {
			blocker = b.ID
			break
		}
	}
	if blocker == "" {
		t.Fatal("the fixture holds no work row for the drain to wait on")
	}
	control := createInBinding(t, city, beads.Bead{Title: "a drain control the binding minted", Type: "task"}, true)
	item := createInBinding(t, city, beads.Bead{Title: "its item", Type: "task"}, true)
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.DepAdd(control.ID, item.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	gate, err := binding.Create(dispatch.NewCrossStoreGate(beads.GraphStore{Store: binding}, control, blocker))
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.DepAdd(item.ID, gate.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	releaseCrossStoreGate(t, binding, gate)
	_ = closeBeadStoreHandle(binding)
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
	}
	if !hasEdge(t, city.source, item.ID, blocker) || !hasEdge(t, city.source, control.ID, item.ID) {
		t.Fatalf("the work store lacks %s -> %s or %s -> %s", item.ID, blocker, control.ID, item.ID)
	}
	if hasEdge(t, city.source, control.ID, blocker) {
		t.Fatalf("the work store holds %s -> %s, an edge the control never had", control.ID, blocker)
	}
	if _, err := city.source.Get(gate.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("the reverse carried the gate %s: %v", gate.ID, err)
	}
}

// TestReverseToWorkRefusesToHoldADrainItemAnOperatorReleased: an operator's
// close of a live drain's gate released its open item whatever the blocker
// does (dispatch.CrossStoreGateStateOverridden), whether the drain has marked
// the close yet or not. --to-work would land the item with its open blocker
// in the work store, where the edge the gate stood for holds it again, so it
// refuses, dry run included, naming the item, the blocker and the gate, and
// writes nothing. Once the blocker closes, the reverse restores the edge, and
// the item is ready.
func TestReverseToWorkRefusesToHoldADrainItemAnOperatorReleased(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(fmt.Sprintf("marked=%v", marked), func(t *testing.T) {
			city := newReverseEngineCity(t, 300, "")
			closeBindingGates(t, city)
			blocker := ""
			for _, b := range city.rows {
				if !isInfra(b) && rigRowOpen(b) {
					blocker = b.ID
					break
				}
			}
			if blocker == "" {
				t.Fatal("the fixture holds no open work row for the drain to wait on")
			}
			control := createInBinding(t, city, beads.Bead{Title: "a live drain's control the binding minted", Type: "task"}, false)
			item := createInBinding(t, city, beads.Bead{Title: "its item", Type: "task"}, false)
			binding, err := openInfraDestination(city.target)
			if err != nil {
				t.Fatal(err)
			}
			if err := binding.DepAdd(control.ID, item.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			gate, err := binding.Create(dispatch.NewCrossStoreGate(beads.GraphStore{Store: binding}, control, blocker))
			if err != nil {
				t.Fatal(err)
			}
			if err := binding.DepAdd(item.ID, gate.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			if err := binding.Close(gate.ID); err != nil {
				t.Fatal(err)
			}
			if marked {
				if err := binding.SetMetadata(gate.ID, beadmeta.CrossStoreGateStateMetadataKey, dispatch.CrossStoreGateStateOverridden); err != nil {
					t.Fatal(err)
				}
			}
			_ = closeBeadStoreHandle(binding)

			before := workStoreFingerprint(t, city.source)
			want := fmt.Sprintf("%s -[blocks]-> %s (gate %s)", item.ID, blocker, gate.ID)
			for _, dry := range []bool{true, false} {
				code, stdout, stderr := city.reverse(t, reverseOptions{DryRun: dry})
				if code == 0 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "an operator closed each gate") {
					t.Fatalf("--to-work (dry run %v) over an operator's close of the gate = %d, want a refusal naming %s: %s%s", dry, code, want, stdout, stderr)
				}
			}
			if workStoreFingerprint(t, city.source) != before || fileExists(storageReverseJournalPath(city.request.CityPath)) {
				t.Fatal("a refused reverse wrote to the work store or its journal")
			}

			if err := city.source.Close(blocker); err != nil {
				t.Fatal(err)
			}
			if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
				t.Fatalf("--to-work once the blocker closed = %d: %s%s", code, stdout, stderr)
			}
			if !hasEdge(t, city.source, item.ID, blocker) {
				t.Fatalf("the work store lacks %s -> %s", item.ID, blocker)
			}
			ready, err := city.source.Ready()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(ready, func(b beads.Bead) bool { return b.ID == item.ID }) {
				t.Fatalf("the item %s is not ready once its blocker closed", item.ID)
			}
		})
	}
}

// withNoCarriedRow deletes every binding row, but the closed cross-store
// gates when gates is set: the binding of a split nothing used, or of one
// whose coordination rows were purged, which holds no row a --to-work
// carries.
func withNoCarriedRow(t *testing.T, city reverseEngineCity, gates bool) {
	t.Helper()
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
	kept := 0
	for _, b := range rigTestList(t, binding) {
		if gates && isCrossStoreGate(b) && !rigRowOpen(b) {
			kept++
			continue
		}
		if err := binding.Delete(b.ID); err != nil {
			t.Fatal(err)
		}
	}
	if gates && kept == 0 {
		t.Fatal("the binding holds no closed gate to keep")
	}
}

// TestReverseToWorkOfABindingThatCarriesNoRow: a binding holding no row the
// reverse carries (none at all, or only closed gates) still gets its reverse
// manifest, empty, so the reverse finishes, in one run and after a kill at
// every boundary it passes, and leaves the work store as it was.
func TestReverseToWorkOfABindingThatCarriesNoRow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gates bool
	}{
		{"empty", false},
		{"only closed gates", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newCity := func(t *testing.T) (reverseEngineCity, string) {
				t.Helper()
				city := newReverseEngineCity(t, 120, "")
				closeBindingGates(t, city)
				withNoCarriedRow(t, city, tc.gates)
				return city, workStoreFingerprint(t, city.source)
			}
			assertReversed := func(t *testing.T, city reverseEngineCity, before string) {
				t.Helper()
				if after := workStoreFingerprint(t, city.source); after != before {
					t.Fatalf("the reverse changed the work store:\nbefore:\n%s\nafter:\n%s", before, after)
				}
				archived, _ := filepath.Glob(city.target.Root + ".reverted-*")
				if len(archived) != 1 {
					t.Fatalf("the binding root was archived as %v, want one *.reverted-<run>", archived)
				}
				manifest, recorded, err := readInfraCopyManifestRecordAt(filepath.Join(archived[0], infraReversedManifestName), "")
				if err != nil || !recorded || len(manifest.IDs) != 0 {
					t.Fatalf("the reverse manifest records %v (recorded %v, %v), want it recorded and empty", sortedMapKeys(manifest.IDs), recorded, err)
				}
				city.assertRevertedCityServesSingleStore(t)
			}
			var points []string
			city, before := newCity(t)
			prev := infraEngineCrashPoint
			infraEngineCrashPoint = func(p string) error {
				if p != reverseGuardedSeam {
					points = append(points, p)
				}
				return nil
			}
			code, stdout, stderr := city.reverse(t, reverseOptions{})
			infraEngineCrashPoint = prev
			if code != 0 {
				t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
			}
			assertReversed(t, city, before)
			for _, point := range points {
				t.Run(point, func(t *testing.T) {
					city, before := newCity(t)
					killReverseAt(t, city, point)
					if point != "reverse phase "+reversePhaseName(reverseComplete) {
						if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
							t.Fatalf("the run after a kill at %s = %d: %s%s", point, code, stdout, stderr)
						}
					}
					assertReversed(t, city, before)
				})
			}
		})
	}
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

// TestReverseRefusesABindingRootThatHoldsTheCity: R5 archives the binding
// root whole, so a binding whose path is the city's .gc or the city itself is
// refused at plan time, the dry run included, and the city's .gc, the work
// store and the binding are left as they were.
func TestReverseRefusesABindingRootThatHoldsTheCity(t *testing.T) {
	for _, bindingPath := range []string{".gc", "."} {
		t.Run("path "+bindingPath, func(t *testing.T) {
			city := newReverseEngineCityAt(t, 60, "", bindingPath)
			closeBindingGates(t, city)
			assertReverseRefusesTheArchive(t, city, "the city's .gc", filepath.Join(city.cityPath, ".gc", "beads.json"))
		})
	}
}

// TestReverseRefusesABindingRootInAStoreDirectory: a binding at the city's
// .beads, or inside it, is refused the same way, the dry run included. bd
// keeps the work store there and the managed Dolt server its data
// (.beads/dolt), so R5 would take the directory every row was just copied
// back into, or part of it, and report the reverse done.
func TestReverseRefusesABindingRootInAStoreDirectory(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"the store directory", ".beads", "and that would take the city's store directory .beads ("},
		{"the managed Dolt data", ".beads/dolt", "and that would take part of the city's store directory .beads ("},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newReverseEngineCityAt(t, 60, "", tc.path)
			closeBindingGates(t, city)
			data := filepath.Join(city.cityPath, ".beads", "dolt", "work", "sentinel")
			if err := os.MkdirAll(filepath.Dir(data), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(data, []byte("the managed Dolt server's data\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			assertReverseRefusesTheArchive(t, city, tc.want, data)
		})
	}
}

// assertReverseRefusesTheArchive runs a --to-work, dry run and real, over a
// binding root R5 cannot archive. Each must refuse with a reason saying want
// and write nothing: kept (a file the archive would take), the binding's
// marker and database stay where they are, the work store is unchanged, and
// there is no journal, reverting record or snapshot.
func assertReverseRefusesTheArchive(t *testing.T, city reverseEngineCity, want, kept string) {
	t.Helper()
	before := workStoreFingerprint(t, city.source)
	for _, opts := range []reverseOptions{{DryRun: true}, {}} {
		code, stdout, stderr := city.reverse(t, opts)
		if code == 0 || !strings.Contains(stderr, "REFUSED — 1 shape(s)") || !strings.Contains(stderr, want) {
			t.Fatalf("--to-work (dry run %v) with the binding root at %s = %d, want it refused saying %q: %s%s", opts.DryRun, city.target.Root, code, want, stdout, stderr)
		}
	}
	if archived, _ := filepath.Glob(city.target.Root + ".reverted-*"); len(archived) != 0 {
		t.Fatalf("a refused reverse archived the binding root as %v", archived)
	}
	for _, path := range []string{kept, city.target.MarkerPath(), city.target.Database} {
		if !fileExists(path) {
			t.Fatalf("a refused reverse moved %s", path)
		}
	}
	for _, path := range []string{storageReverseJournalPath(city.cityPath), filepath.Join(city.target.Root, infraRevertingRecordName), storageSnapshotDefaultDir(city.cityPath)} {
		if fileExists(path) {
			t.Fatalf("a refused reverse wrote %s", path)
		}
	}
	if after := workStoreFingerprint(t, city.source); after != before {
		t.Fatal("a refused reverse wrote to the work store")
	}
}

// TestReverseArchiveRefusals: a binding root R5 would archive with the city's
// .gc or a rig under it (the .gc itself, the city, a directory above either)
// is refused, naming what it holds, and so is one that is, holds or lies
// inside a store's directory (the city's .beads, a rig's .beads or .gc),
// naming what it would take of it. A root of the binding's own, beside them
// or inside the city's .gc, is not.
func TestReverseArchiveRefusals(t *testing.T) {
	parent := t.TempDir()
	cityPath := filepath.Join(parent, "city")
	rigPath := filepath.Join(parent, "rigs", "frontend")
	cityGC, cityBeads := filepath.Join(cityPath, ".gc"), filepath.Join(cityPath, ".beads")
	rigBeads, rigGC := filepath.Join(rigPath, ".beads"), filepath.Join(rigPath, ".gc")
	for _, dir := range []string{filepath.Join(cityGC, "store"), filepath.Join(cityBeads, "dolt", "work"), filepath.Join(rigBeads, "dolt"), rigGC} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "frontend", Path: rigPath, Prefix: "fe"}}}
	gcDir, rig := "the city's .gc ("+cityGC+")", "rig frontend ("+rigPath+")"
	beadsDir := "the city's store directory .beads (" + cityBeads + ")"
	rigBeadsDir := "rig frontend's store directory .beads (" + rigBeads + ")"
	rigGCDir := "rig frontend's store directory .gc (" + rigGC + ")"
	for _, tc := range []struct {
		root string
		want []string
	}{
		{filepath.Join(cityGC, "store"), nil},
		{filepath.Join(cityPath, ".gc-store"), nil},
		{filepath.Join(cityPath, ".beads-store"), nil},
		{filepath.Join(parent, "rigs-store"), nil},
		{filepath.Join(rigPath, "infra-store"), nil},
		{cityGC, []string{gcDir}},
		{cityPath, []string{gcDir, beadsDir}},
		{filepath.Join(parent, "rigs"), []string{rig}},
		{parent, []string{gcDir, beadsDir, rig}},
		{cityBeads, []string{beadsDir}},
		{filepath.Join(cityBeads, "dolt"), []string{"part of " + beadsDir}},
		{filepath.Join(cityBeads, "dolt", "work"), []string{"part of " + beadsDir}},
		{rigBeads, []string{rigBeadsDir}},
		{filepath.Join(rigBeads, "dolt", "fe"), []string{"part of " + rigBeadsDir}},
		{rigGC, []string{rigGCDir}},
		{filepath.Join(rigGC, "store"), []string{"part of " + rigGCDir}},
	} {
		refusals := reverseArchiveRefusals(cityPath, cfg, infraBindingTarget{Binding: "infra", Root: tc.root})
		if len(tc.want) == 0 {
			if len(refusals) != 0 {
				t.Fatalf("a binding root at %s refused: %v", tc.root, refusals)
			}
			continue
		}
		if len(refusals) != 1 || !strings.Contains(refusals[0].String(), "binding infra") || refusals[0].Remedy != reverseKeepSplitRemedy {
			t.Fatalf("a binding root at %s = %v, want one refusal naming the binding", tc.root, refusals)
		}
		want := fmt.Sprintf("R5 archives the binding root whole, renaming it %s.reverted-<run>, and that would take %s with it", tc.root, strings.Join(tc.want, " and "))
		if refusals[0].Reason != want {
			t.Fatalf("a binding root at %s refused with\n%s\nwant\n%s", tc.root, refusals[0].Reason, want)
		}
	}
}

// strandInWorkStore writes b into the work store as a raw bd writer does
// after the cutover, with no gate in its way, closed when closed is set: a
// row the binding does not serve.
func strandInWorkStore(t *testing.T, city reverseEngineCity, b beads.Bead, closed bool) beads.Bead {
	t.Helper()
	under, _, _ := unwrapBeadPolicyStore(city.source)
	created, err := under.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	if closed {
		if err := under.Close(created.ID); err != nil {
			t.Fatal(err)
		}
	}
	return created
}

// TestReverseRefusesTheStrandedRowsItWouldRevive: a stranded city serves
// degraded, its stranded rows invisible to their class readers. A --to-work
// makes the work store serve every class, so it refuses, dry run included,
// each open stranded row it would bring back to life, names the ways to
// settle them, and writes nothing. A row that is infrastructure class only
// because of metadata, and a closed stranded row, stay in the work store as
// they are, which the reverse reports; once the open rows are closed, it
// finishes and leaves all four as the writer left them.
func TestReverseRefusesTheStrandedRowsItWouldRevive(t *testing.T) {
	city := newReverseEngineCity(t, 120, "")
	closeBindingGates(t, city)
	mail := strandInWorkStore(t, city, beads.Bead{Title: "context cycle", Type: "message", Assignee: "worker"}, false)
	session := strandInWorkStore(t, city, beads.Bead{Title: "landed after the proof", Type: "session", Labels: []string{"gc:session"}}, false)
	read := strandInWorkStore(t, city, beads.Bead{Title: "already read", Type: "message", Assignee: "worker"}, true)
	work := strandInWorkStore(t, city, beads.Bead{Title: "patrol finding", Type: "task", Metadata: beads.StringMap{beadmeta.RootBeadIDMetadataKey: "gcg-622605"}}, false)
	if report := confirmInfraContainment(city.cityPath, city.target, "probe", io.Discard); !report.servesDegraded() || len(report.Stranded) != 4 {
		t.Fatalf("the city before the reverse = %v, stranded %v, want it served degraded over the four rows", report.Outcome, report.Stranded)
	}
	before := workStoreFingerprint(t, city.source)
	for _, opts := range []reverseOptions{{DryRun: true}, {}} {
		code, stdout, stderr := city.reverse(t, opts)
		if code == 0 || !strings.Contains(stderr, "REFUSED — 1 shape(s)") {
			t.Fatalf("--to-work (dry run %v) over open stranded rows = %d: %s%s", opts.DryRun, code, stdout, stderr)
		}
		for _, want := range []string{mail.ID + " (message, open)", session.ID + " (session, open)", "bd close <id>", "--purge-type message", storageRecoveryInstruction(), storageClearInstruction()} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("the refusal does not say %q: %s", want, stderr)
			}
		}
		for _, id := range []string{read.ID, work.ID} {
			if strings.Contains(stderr, id+" (") {
				t.Fatalf("the refusal names %s, which the reverse leaves as it is: %s", id, stderr)
			}
		}
		for _, want := range []string{"1 row(s) the city work store holds", work.ID, beadmeta.RootBeadIDMetadataKey, "1 closed row(s)", read.ID} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("the plan does not report %q: %s", want, stdout)
			}
		}
	}
	if after := workStoreFingerprint(t, city.source); after != before || fileExists(storageReverseJournalPath(city.cityPath)) {
		t.Fatal("a refused reverse wrote to the work store or its journal")
	}
	under, _, _ := unwrapBeadPolicyStore(city.source)
	for _, id := range []string{mail.ID, session.ID} {
		if err := under.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("--to-work once the open stranded rows are closed = %d: %s%s", code, stdout, stderr)
	}
	city.assertRevertedCityServesSingleStore(t)
	for id, want := range map[string]string{mail.ID: "closed", session.ID: "closed", read.ID: "closed", work.ID: "open"} {
		b, err := city.source.Get(id)
		if err != nil || b.Status != want || b.Metadata[beadmeta.InfraReversedFromMetadataKey] != "" {
			t.Fatalf("after the reverse the work store holds %s as %q (stamp %q, %v), want it %s as the writer left it", id, b.Status, b.Metadata[beadmeta.InfraReversedFromMetadataKey], err, want)
		}
	}
}

// TestReverseRefusesAStrandedIDTheBindingHoldsForAnotherBead: the work store
// minted again an id the copy carried, and the binding holds the original
// under it. Copying the binding's row back would put two beads under one id,
// so the reverse refuses it, closed as it is, with recover-stranded's per-id
// choices rather than a hand delete.
func TestReverseRefusesAStrandedIDTheBindingHoldsForAnotherBead(t *testing.T) {
	city := newReverseEngineCity(t, 120, "")
	closeBindingGates(t, city)
	var carried beads.Bead
	for _, id := range sortedMapKeys(city.before) {
		if b := city.before[id]; isInfra(b) && b.Type == "message" {
			carried = b
			break
		}
	}
	if carried.ID == "" {
		t.Fatal("no message row in the fixture")
	}
	under, _, _ := unwrapBeadPolicyStore(city.source)
	later := carried.CreatedAt.Add(time.Hour)
	if _, err := under.(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{ID: carried.ID, Title: "minted again", Type: "message", Status: "closed", CreatedAt: later, UpdatedAt: later}); err != nil {
		t.Fatal(err)
	}
	if report := confirmInfraContainment(city.cityPath, city.target, "probe", io.Discard); !slices.Equal(report.HeldDifferent, []string{carried.ID}) {
		t.Fatalf("the containment check = %v, held for a different bead %v, want %s", report.Outcome, report.HeldDifferent, carried.ID)
	}
	before := workStoreFingerprint(t, city.source)
	code, stdout, stderr := city.reverse(t, reverseOptions{DryRun: true})
	if code == 0 || !strings.Contains(stderr, "REFUSED — 1 shape(s)") || !strings.Contains(stderr, carried.ID+" (message, closed)") {
		t.Fatalf("--to-work --dry-run over a reused id = %d: %s%s", code, stdout, stderr)
	}
	for _, want := range []string{"--" + strandedReplaceBindingRowFlag, "--" + strandedDropWorkRowFlag, storageRecoveryInstruction()} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the refusal does not say %q: %s", want, stderr)
		}
	}
	if strings.Contains(stderr, "as another bead") {
		t.Fatalf("the refusal sends the reused id to a hand delete: %s", stderr)
	}
	if after := workStoreFingerprint(t, city.source); after != before {
		t.Fatal("a refused reverse wrote to the work store")
	}
}

// newRetainedReverseEngineCity is newReverseEngineCity migrated as gc 1.5.0
// did (copy, prove, mark, and no clear): the work store keeps its retained
// copies, and there is no cleared note.
func newRetainedReverseEngineCity(t *testing.T, rows int) reverseEngineCity {
	t.Helper()
	prev := clearRetainedInfraCopiesFn
	clearRetainedInfraCopiesFn = func(string, infraBindingTarget, io.Writer) (infraClearResult, error) { return infraClearResult{}, nil }
	defer func() { clearRetainedInfraCopiesFn = prev }()
	return newReverseEngineCity(t, rows, "")
}

// TestReverseRefusesRetainedCopiesOnceNamingTheClear: a city migrated as gc
// 1.5.0 did keeps its retained pre-migration copies, which boot refuses until
// the clear removes them. The reverse refuses them once, naming the clear,
// not once per row as an id held "as another bead", whose hand delete would
// skip the clear's backup and edge detach. After the clear it finishes
// without loss.
func TestReverseRefusesRetainedCopiesOnceNamingTheClear(t *testing.T) {
	city := newRetainedReverseEngineCity(t, 120)
	closeBindingGates(t, city)
	if _, err := storageBootGate(city.cityPath, city.request.Cfg, "gc start", nil, io.Discard); err == nil || !strings.Contains(err.Error(), storageClearInstruction()) {
		t.Fatalf("boot over retained copies = %v, want the refusal naming the clear", err)
	}
	before := workStoreFingerprint(t, city.source)
	for _, opts := range []reverseOptions{{DryRun: true}, {}} {
		code, stdout, stderr := city.reverse(t, opts)
		if code == 0 || !strings.Contains(stderr, "REFUSED — 1 shape(s)") || !strings.Contains(stderr, "retained pre-migration cop(ies)") || !strings.Contains(stderr, storageClearInstruction()) || strings.Contains(stderr, "as another bead") {
			t.Fatalf("--to-work (dry run %v) over retained copies = %d: %s%s", opts.DryRun, code, stdout, stderr)
		}
	}
	if after := workStoreFingerprint(t, city.source); after != before || fileExists(storageReverseJournalPath(city.cityPath)) {
		t.Fatal("a refused reverse wrote to the work store or its journal")
	}
	var said bytes.Buffer
	if report := migrateInfraClasses(t, city.cityPath, infraSplitConfig(".gc/store"), &said); report.Outcome != infraMigrationConverged {
		t.Fatalf("the clear = %v: %s", report.Outcome, said.String())
	}
	// The clear records on each work row the edges it dropped: those are the
	// rows the round trip compares the work rows with.
	city.afterForward = map[string]beads.Bead{}
	for _, b := range rigTestList(t, city.source) {
		city.afterForward[b.ID] = b
	}
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("--to-work after the clear = %d: %s%s", code, stdout, stderr)
	}
	city.assertReversedNotLossy(t)
	city.assertRevertedCityServesSingleStore(t)
}

// TestReverseWithNoCopyManifestSaysTheStrandCheckIsOff: a binding that
// converged before its copy manifest was recorded cannot tell a stranded row
// from one its GC collected. The reverse, the one rollback that keeps every
// binding row, does not refuse it: it says the check is off, and finishes.
func TestReverseWithNoCopyManifestSaysTheStrandCheckIsOff(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	if err := os.Remove(city.target.ManifestPath()); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := city.reverse(t, reverseOptions{})
	if code != 0 || !strings.Contains(stdout, "converged before "+city.target.ManifestPath()+" was recorded") {
		t.Fatalf("--to-work with no copy manifest = %d: %s%s", code, stdout, stderr)
	}
	city.assertReversedNotLossy(t)
	city.assertRevertedCityServesSingleStore(t)
}

// TestRevertAdviceNamesTheReverseFirst: a hand revert of the split (the
// classes pointed back at work, or [storage] deleted) is refused, and the
// refusal names the reverse, the rollback that keeps every bead written to
// the binding since the cutover, before the backup restore, which loses them.
// A re-point to another binding, and a served binding the reverse cannot copy
// from, are not told to run it.
func TestRevertAdviceNamesTheReverseFirst(t *testing.T) {
	reverse := storageReverseInstruction(reverseToWork, nil)
	work := infraBindingTarget{Binding: config.StorageWorkBinding}
	cleared := infraMigrationReport{Outcome: infraMigrationGenesisBlocked, Target: work, ServedNotePath: "/city/.gc/storage-infra-cleared.json", Cleared: &infraClearedNote{Binding: "infra", Backup: "/city/.gc/store/" + infraRetainedBackupName}}
	served := infraMigrationReport{Outcome: infraMigrationGenesisBlocked, Target: work, ServedNotePath: "/city/.gc/storage-served-binding.json", ServedBinding: "infra", ServedProvider: config.StorageProviderSQLiteBeads}
	repointed := served
	repointed.Target = infraBindingTarget{Binding: "elsewhere"}
	foreign := served
	foreign.ServedProvider = "bd"
	for _, tc := range []struct {
		name   string
		report infraMigrationReport
		want   bool
	}{
		{"a cleared city reverted", cleared, true},
		{"an uncleared city reverted", served, true},
		{"a re-point to another binding", repointed, false},
		{"a served binding the reverse cannot copy from", foreign, false},
	} {
		if advice := infraMigrationOperatorAdvice(tc.report, "gc start"); strings.Contains(advice, reverse) != tc.want {
			t.Fatalf("%s: the advice names %q = %v, want %v: %s", tc.name, reverse, !tc.want, tc.want, advice)
		}
	}
	advice := infraMigrationOperatorAdvice(cleared, "gc start")
	if restore := strings.Index(advice, "Restoring the work store from the backup"); restore < 0 || strings.Index(advice, reverse) > restore {
		t.Fatalf("the cleared city's advice does not name the reverse before the backup restore: %s", advice)
	}
}

// TestReverseHoldsEveryOtherStorageVerb: while a reverse is open, the forward
// migration, the rig import, the binding restore and recover-stranded (its
// repair, its purge and the purge's restore) refuse, naming it.
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
	for name, run := range map[string]func(io.Writer) int{
		"recover-stranded": func(w io.Writer) int {
			return doStorageRecoverStranded(context.Background(), city.request, strandedRecoveryOptions{}, &stdout, w)
		},
		"recover-stranded --dry-run": func(w io.Writer) int {
			return doStorageRecoverStranded(context.Background(), city.request, strandedRecoveryOptions{DryRun: true}, &stdout, w)
		},
		"recover-stranded --purge-type": func(w io.Writer) int {
			return doStoragePurgeStranded(context.Background(), city.request, strandedPurgeOptions{Type: "message", DryRun: true}, &stdout, w)
		},
		"recover-stranded --restore-purge": func(w io.Writer) int {
			return doStorageRestorePurge(context.Background(), city.request, filepath.Join(city.target.Root, "infra.purged.jsonl"), true, &stdout, w)
		},
	} {
		var errOut bytes.Buffer
		if code := run(&errOut); code == 0 || !strings.Contains(errOut.String(), "reverse migration") || !strings.Contains(errOut.String(), storageAbortReverseInstruction()) {
			t.Fatalf("%s during a reverse = %d: %s", name, code, errOut.String())
		}
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

// TestReverseResumeNamesTheAbortBeforeAHeldVerb: a row a raw writer strands
// in the work store while a reverse is open is refused when the reverse
// resumes, dry run included. Every storage verb the refusal's fixes run holds
// while the journal is open (recover-stranded writes neither the binding nor
// its manifest), so the refusal names --abort-reverse before them. Once the
// reverse is abandoned, its carry-on fix runs, and a new reverse brings the
// row back.
func TestReverseResumeNamesTheAbortBeforeAHeldVerb(t *testing.T) {
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
	mail := strandInWorkStore(t, city, beads.Bead{Title: "landed mid-reverse", Type: "message", Assignee: "worker"}, false)
	for _, opts := range []reverseOptions{{DryRun: true}, {}} {
		code, stdout, stderr := city.reverse(t, opts)
		abort, held := strings.Index(stderr, storageAbortReverseInstruction()), strings.Index(stderr, storageRecoveryInstruction())
		if code == 0 || !strings.Contains(stderr, mail.ID+" (message, open)") || abort < 0 || held < abort {
			t.Fatalf("the resume (dry run %v) over a row stranded mid-reverse = %d, want it refused naming the abort before recover-stranded: %s%s", opts.DryRun, code, stdout, stderr)
		}
	}
	bindingState := func() string {
		t.Helper()
		binding, err := openInfraBindingReadOnly(city.target)
		if err != nil {
			t.Fatal(err)
		}
		defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
		manifest, _, err := readInfraCopyManifestRecord(city.target)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s\nmanifest %v", workStoreFingerprint(t, binding), sortedMapKeys(manifest.IDs))
	}
	before := bindingState()
	var stdout, stderr bytes.Buffer
	if code := doStorageRecoverStranded(context.Background(), city.request, strandedRecoveryOptions{}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "reverse migration") {
		t.Fatalf("recover-stranded during the reverse = %d: %s%s", code, stdout.String(), stderr.String())
	}
	if bindingState() != before {
		t.Fatal("recover-stranded wrote the binding or its manifest while the reverse was open")
	}
	if code, out, errOut := city.abort(t); code != 0 {
		t.Fatalf("--abort-reverse = %d: %s%s", code, out, errOut)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRecoverStranded(context.Background(), city.request, strandedRecoveryOptions{}, &stdout, &stderr); code != 0 {
		t.Fatalf("recover-stranded once the reverse is abandoned = %d: %s%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageMigrate(context.Background(), city.request, &stdout, &stderr); code != 0 {
		t.Fatalf("the clear of the carried row's work-store copy = %d: %s%s", code, stdout.String(), stderr.String())
	}
	if code, out, errOut := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("a new reverse = %d: %s%s", code, out, errOut)
	}
	if got, err := city.source.Get(mail.ID); err != nil || got.Status != "open" || got.Metadata[beadmeta.InfraReversedFromMetadataKey] == "" {
		t.Fatalf("after the new reverse the work store holds %s as %q (stamp %q, %v), want it back, open and stamped", mail.ID, got.Status, got.Metadata[beadmeta.InfraReversedFromMetadataKey], err)
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
// dropped, is deleted and copied again, an edge whose payload the binding
// cleared is replaced, and the reverse ends equal to the binding as it is
// now.
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
	// An edge with a payload that does not touch from: the resume deletes
	// and copies from again, and that delete would take the edge with it.
	var cleared beads.Dep
	for _, d := range city.deps {
		_, carried := city.payloads[[2]string{d.IssueID, d.DependsOnID}]
		if carried && d.IssueID != from && d.DependsOnID != from && isInfra(city.before[d.IssueID]) && isInfra(city.before[d.DependsOnID]) && cleared.IssueID == "" {
			cleared = d
		}
	}
	if cleared.IssueID == "" {
		t.Fatal("the fixture holds no infrastructure edge with a payload")
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
	// A plain re-add clears the edge's payload in the binding; the work
	// store's copy of the edge still carries it.
	if err := binding.DepAdd(cleared.IssueID, cleared.DependsOnID, cleared.Type); err != nil {
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
	if payload, carried, err := city.source.(beads.DepMetadataReader).DepMetadata(cleared.IssueID, cleared.DependsOnID); err != nil || carried {
		t.Fatalf("the work store's %s -> %s carries (%q, %v), %v; want no payload, as the binding holds it", cleared.IssueID, cleared.DependsOnID, payload, carried, err)
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
			if name == "an invented row" {
				// A row under the stamp that the binding does not hold may be
				// the only copy: the rerun names it and deletes nothing, and
				// the operator removes it.
				code, _, stderr := city.reverse(t, reverseOptions{})
				if code == 0 || !strings.Contains(stderr, "may be the only copy") || !strings.Contains(stderr, "mc-invented-") {
					t.Fatalf("the rerun over an invented row = %d: %s", code, stderr)
				}
				for _, b := range rigTestList(t, city.source) {
					if strings.HasPrefix(b.ID, "mc-invented-") {
						under, _, _ := unwrapBeadPolicyStore(city.source)
						if err := under.Delete(b.ID); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
				t.Fatalf("the honest rerun = %d: %s%s", code, stdout, stderr)
			}
			city.assertReversedNotLossy(t)
		})
	}
}
