package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// splitReadyCity is an unsplit city whose work store is a real SQLite store
// shaped like maintainer-city's (newMCShapedInfraCity), opened on the lane
// diag names, with no controller live, the provider up, no session leg to
// census, and the window's provider start and stop counted.
func splitReadyCity(t *testing.T, rows int, diag beads.BeadsDiagnostic) (storageOperatorRequest, beads.Store, *migrationWindowProvider) {
	t.Helper()
	city := newMCShapedInfraCity(t, rows)
	source := city.sqliteSource(t)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(cityPath string) (beads.Store, error) {
		recordMigrationLane(cityPath, diag)
		return source, nil
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
	stubSplitPreflightLegs(t, nil)
	stubInfraControllerPing(t, 0)
	provider := &migrationWindowProvider{}
	stubMigrationWindowStartStop(t, provider)
	stubMigrationProviderDown(t, false)
	request := storageTestRequest(t, &config.City{})
	request.TimeBudget = storageDefaultTimeBudget
	return request, source, provider
}

func stubSplitPreflightLegs(t *testing.T, legs []v2SessionLeg) {
	t.Helper()
	prev := splitPreflightSessionLegs
	splitPreflightSessionLegs = func(string, *config.City) []v2SessionLeg { return legs }
	t.Cleanup(func() { splitPreflightSessionLegs = prev })
}

func stubMigrationProviderDown(t *testing.T, down bool) {
	t.Helper()
	prev := migrationWindowProviderDown
	migrationWindowProviderDown = func(string) (bool, error) { return down, nil }
	t.Cleanup(func() { migrationWindowProviderDown = prev })
}

func runSplitPreflight(t *testing.T, request storageOperatorRequest) (int, splitPreflightSummary, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, summary := doStorageSplitPreflight(request, &stdout, &stderr)
	return code, summary, stdout.String() + stderr.String()
}

var nativeDoltLane = beads.BeadsDiagnostic{Store: beads.BeadsStoreNameNativeDoltStore}

// TestSplitEstimateConstantsArePinned pins every rate and size the estimates
// use to the measurement it came from. A change here changes every estimate
// the command prints, so it has to change this test and its source note too.
func TestSplitEstimateConstantsArePinned(t *testing.T) {
	near := func(got, want time.Duration, tolerance float64) bool {
		diff := float64(got - want)
		return diff <= tolerance*float64(want) && -diff <= tolerance*float64(want)
	}
	for _, tc := range []struct {
		name      string
		got, want time.Duration
		tolerance float64
	}{
		// BenchmarkInfraMigrateMCShaped, 40,000 rows and 18,662 edges (#8799).
		{"plan 1.1 s at 40k", 40000 * splitPlanPerRow, 1100 * time.Millisecond, 0},
		{"rows 6.2 s at 40k", 40000 * splitCopyPerRow, 6200 * time.Millisecond, 0},
		{"edges 0.5 s at 18,662", 18662 * splitCopyPerEdge, 500 * time.Millisecond, 0.01},
		{"verify 2.0 s at 40k", 40000 * splitVerifyPerRow, 2 * time.Second, 0},
		{"SQLite clear 21.3 s at 40k", 40000 * splitSQLiteClearPerRow, 21300 * time.Millisecond, 0.002},
		// Native Dolt (#8799, a dolt sql-server, 300 rows and edges).
		{"Dolt DepRemoveBatch", splitDoltBatchedPerEdge, 62 * time.Millisecond, 0},
		{"Dolt DepRemove", splitDoltPerEdge, 129 * time.Millisecond, 0},
		{"Dolt DepList 7.4 + DeleteBatch 4.6", splitDoltClearPerRow, 7400*time.Microsecond + 4600*time.Microsecond, 0},
		{"WAL growth is S5-E's floor", time.Duration(storageSnapshotWALGrowthFloor), 64 << 20, 0},
	} {
		if !near(tc.got, tc.want, tc.tolerance) {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if splitBindingPerJSONByte != 6 || splitSnapshotPerJSONByte != 2 {
		t.Errorf("disk factors = %d, %d; want 6 (binding root, measured 4.9) and 2 (snapshot JSONL, measured 1.3)", splitBindingPerJSONByte, splitSnapshotPerJSONByte)
	}
}

// TestSplitClearLaneFor maps each lane an open reports to its rates.
func TestSplitClearLaneFor(t *testing.T) {
	proxied := beads.BeadsDiagnostic{Store: beads.BeadsStoreNameNativeDoltStore, Proxied: &beads.ProxiedDiagnostic{}}
	for _, tc := range []struct {
		name    string
		diag    beads.BeadsDiagnostic
		known   bool
		perRow  time.Duration
		perEdge time.Duration
		floor   bool
	}{
		{"unknown", beads.BeadsDiagnostic{}, false, splitDoltClearPerRow, splitDoltPerEdge, true},
		{"native Dolt", nativeDoltLane, true, splitDoltClearPerRow, splitDoltBatchedPerEdge, false},
		{"proxied", proxied, true, splitDoltClearPerRow, splitDoltPerEdge, false},
		{"bd CLI", beads.BeadsDiagnostic{Store: beads.BeadsStoreNameBdStore}, true, splitDoltClearPerRow, splitDoltPerEdge, true},
		{"exec", beads.BeadsDiagnostic{Store: beads.BeadsStoreNameExecStore}, true, splitDoltClearPerRow, splitDoltPerEdge, true},
		{"file", beads.BeadsDiagnostic{Store: beads.BeadsStoreNameFileStore}, true, splitSQLiteClearPerRow, 0, false},
		{"library backend", beads.BeadsDiagnostic{Store: beads.BeadsStoreNameLibraryBackendStore}, true, splitSQLiteClearPerRow, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lane := splitClearLaneFor(tc.diag, tc.known)
			if lane.perRow != tc.perRow || lane.perEdge != tc.perEdge || lane.floor != tc.floor || lane.basis == "" {
				t.Errorf("lane = %+v, want per row %v, per edge %v, floor %t, a basis", lane, tc.perRow, tc.perEdge, tc.floor)
			}
		})
	}
}

// TestEstimateSplitTime pins the arithmetic, term by term.
func TestEstimateSplitTime(t *testing.T) {
	native := splitClearLaneFor(nativeDoltLane, true)
	sqlite := splitClearLaneFor(beads.BeadsDiagnostic{Store: beads.BeadsStoreNameLibraryBackendStore}, true)
	for _, tc := range []struct {
		name string
		in   splitEstimateInput
		want splitTimeEstimate
	}{
		{"nothing", splitEstimateInput{lane: native}, splitTimeEstimate{}},
		{
			"rows only",
			splitEstimateInput{rows: 1000, clearRows: 1000, lane: native},
			splitTimeEstimate{copy: 232500 * time.Microsecond, clear: 12 * time.Second},
		},
		{
			"gates are copied rows",
			splitEstimateInput{gates: 10, lane: native},
			splitTimeEstimate{copy: 2325 * time.Microsecond},
		},
		{
			"edges",
			splitEstimateInput{copyEdges: 500, clearEdges: 600, lane: native},
			splitTimeEstimate{copy: 13500 * time.Microsecond, clear: 37200 * time.Millisecond},
		},
		{
			"rigs",
			splitEstimateInput{rigRows: 100, rigCopyEdges: 40, rigClearRows: 120, rigClearEdges: 70, lane: native},
			splitTimeEstimate{rigs: 23250*time.Microsecond + 1080*time.Microsecond + 1440*time.Millisecond + 4340*time.Millisecond},
		},
		{
			"SQLite clear has no per-edge term",
			splitEstimateInput{clearRows: 40000, clearEdges: 18662, lane: sqlite},
			splitTimeEstimate{clear: 21320 * time.Millisecond},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimateSplitTime(tc.in); got != tc.want {
				t.Errorf("estimateSplitTime = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSplitBudgetResumes pins how many resumes a budget costs: every run of
// the same command spends the whole budget again.
func TestSplitBudgetResumes(t *testing.T) {
	const b = 30 * time.Minute
	for _, tc := range []struct {
		work, budget time.Duration
		want         int
	}{
		{time.Hour, 0, 0},
		{time.Hour, -time.Second, 0},
		{-3 * b, b, 0},
		{0, b, 0},
		{1, b, 0},
		{1, 1, 0},
		{2, 1, 1},
		{b - 1, b, 0},
		{b, b, 0},
		{b + 1, b, 1},
		{2 * b, b, 1},
		{2*b + 1, b, 2},
		{25 * time.Minute, 10 * time.Minute, 2},
	} {
		if got := splitBudgetResumes(tc.work, tc.budget); got != tc.want {
			t.Errorf("splitBudgetResumes(%v, %v) = %d, want %d", tc.work, tc.budget, got, tc.want)
		}
	}
}

// TestEstimateSplitDisk pins the disk arithmetic for each exact snapshot.
func TestEstimateSplitDisk(t *testing.T) {
	const wal = int64(storageSnapshotWALGrowthFloor)
	for _, tc := range []struct {
		exact          string
		snapshot, want int64
	}{
		{snapshotMethodDoltTag, 2000, 2000 + 6000 + wal},
		{snapshotMethodNone, 2000, 2000 + 6000 + wal},
		{snapshotMethodBdExport, 4000, 4000 + 6000 + wal},
		{snapshotMethodFileCopy, 4000, 4000 + 6000 + wal},
	} {
		d := estimateSplitDisk(1000, 1000, tc.exact)
		if d.snapshot != tc.snapshot || d.binding != 6000 || d.wal != wal || d.total() != tc.want || d.exact != tc.exact {
			t.Errorf("%s: estimateSplitDisk = %+v (total %d), want snapshot %d, binding 6000, total %d", tc.exact, d, d.total(), tc.snapshot, tc.want)
		}
	}
}

// TestSplitEstimateInputCountsWhatTheClearAndTheRigsMove counts each edge
// touching a moved row once, the gates as rows and edges, and the rigs'
// moves, gates, retained rows and records.
func TestSplitEstimateInputCountsWhatTheClearAndTheRigsMove(t *testing.T) {
	dep := func(from, to string) beads.Dep { return beads.Dep{IssueID: from, DependsOnID: to, Type: "blocks"} }
	plan := infraCopyPlan{
		rows: []beads.Bead{{ID: "a"}, {ID: "b"}},
		down: map[string][]beads.Dep{"a": {dep("a", "b"), dep("a", "w1")}, "b": {dep("b", "external:x")}},
		up:   map[string][]beads.Dep{"b": {dep("a", "b"), dep("w2", "b")}},
		boundary: crossBoundaryResult{
			Copy:  []beads.Dep{dep("a", "b"), dep("b", "external:x")},
			Gates: []rigImportGate{{}},
		},
	}
	rig := &rigImportPlan{
		Move:     []beads.Bead{{ID: "r1"}, {ID: "r2"}},
		Retained: []beads.Bead{{ID: "r3"}},
		Copy:     []beads.Dep{dep("r1", "r2")},
		Gates:    []rigImportGate{{}, {}},
		Records:  map[string][]rigCrossEdgeRecord{"r1": {{}, {}}, "r2": {{}}},
	}
	found := preflightFindings{plan: plan, rigs: []rigCensusRow{{Rig: "alpha", Plan: rig}, {Rig: "beta"}}}
	got := splitEstimateInputFor(found, splitClearLane{name: "x"})
	want := splitEstimateInput{
		rows: 2, gates: 1, copyEdges: 3, clearRows: 2, clearEdges: 4,
		rigRows: 4, rigCopyEdges: 3, rigClearRows: 3, rigClearEdges: 4,
		lane: splitClearLane{name: "x"},
	}
	if got != want {
		t.Errorf("splitEstimateInputFor = %+v, want %+v", got, want)
	}
}

// TestSplitPreflightRehearsesARealSQLiteCity is the happy path on a real
// SQLite work store: every check the migration makes, the reconciler, the
// lane, and the estimates, which say they are estimates and are computed
// from the counts read.
func TestSplitPreflightRehearsesARealSQLiteCity(t *testing.T) {
	request, source, provider := splitReadyCity(t, 300, nativeDoltLane)
	code, summary, out := runSplitPreflight(t, request)
	if code != 0 || !summary.ready() {
		t.Fatalf("exit %d, summary %+v:\n%s", code, summary, out)
	}
	for _, want := range []string{
		"split [storage] (hypothetical; nothing is written)",
		"[ok]    config",
		"[ok]    reconciler     after the split: v2",
		"[ok]    v2 sessions",
		"would copy 300 infrastructure bead(s)",
		"[ok]    holds",
		"the work store opens on NativeDoltStore in the window (native Dolt)",
		"ESTIMATE, not a measurement",
		"based on: the row and edge counts read above, read-only",
		"62 ms an edge",
		"[ok]    time budget    ESTIMATE",
		"[ok]    disk           ESTIMATE",
		"Ready. The figures marked ESTIMATE are estimates",
		storageClearInstruction(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, rigImportAllInstruction()) {
		t.Errorf("a city with no rig was told to import its rigs:\n%s", out)
	}
	if summary.Reconciler != "v2 (default: session_reconciler is unset)" {
		t.Errorf("reconciler = %q", summary.Reconciler)
	}
	est := summary.Estimate
	if est == nil {
		t.Fatal("no estimate")
	}
	// The counts are what the store holds: every infrastructure row, and each
	// edge touching one, once.
	rows, _, err := readInfraSnapshotRows(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	infra := map[string]bool{}
	for _, b := range rows {
		infra[b.ID] = true
	}
	all, err := source.List(beads.ListQuery{IncludeClosed: true, AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	touching := map[beads.DepPair]bool{}
	for _, b := range all {
		deps, err := source.DepList(b.ID, "down")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range deps {
			if infra[b.ID] || infra[d.DependsOnID] {
				touching[beads.DepPair{IssueID: b.ID, DependsOnID: d.DependsOnID}] = true
			}
		}
	}
	if est.input.rows != len(rows) || est.input.clearRows != len(rows) || est.input.clearEdges != len(touching) {
		t.Errorf("counted %d rows, %d cleared, %d clear edges; the store holds %d infrastructure rows and %d edges touching them", est.input.rows, est.input.clearRows, est.input.clearEdges, len(rows), len(touching))
	}
	if want := estimateSplitTime(est.input); est.time != want || !strings.Contains(out, "clear "+splitDuration(want.clear)) {
		t.Errorf("printed estimate is not the arithmetic over the counts: %+v, want %+v:\n%s", est.time, want, out)
	}
	if provider.starts != 0 || provider.stops != 0 {
		t.Errorf("the rehearsal started %d and stopped %d provider(s); it must touch none", provider.starts, provider.stops)
	}
	if activeMigrationWindow(request.CityPath) != nil {
		t.Error("the rehearsal left its lane window registered")
	}
}

// TestSplitPreflightWritesNothing is the read-only guard: the city tree is
// byte-identical after a rehearsal and after the storage-split doctor check,
// no event is published, and no window, lock or record is left behind.
func TestSplitPreflightWritesNothing(t *testing.T) {
	request, _, _ := splitReadyCity(t, 120, nativeDoltLane)
	before := treeFingerprint(t, request.CityPath)
	if code, summary, out := runSplitPreflight(t, request); code != 0 {
		t.Fatalf("exit %d (%+v):\n%s", code, summary, out)
	}
	newStorageSplitCheck(request.Cfg, request.CityPath).Run(nil)
	if after := treeFingerprint(t, request.CityPath); !equalStrings(before, after) {
		t.Errorf("the rehearsal wrote into the city:\n before %v\n after  %v", before, after)
	}
	// The control: the fingerprint sees a write into the city, so its
	// silence above is evidence.
	defer func() {
		var log bytes.Buffer
		migrateInfraClasses(t, request.CityPath, splitHypotheticalConfig(request.Cfg), &log)
		if equalStrings(before, treeFingerprint(t, request.CityPath)) {
			t.Error("a migration into the same city left the fingerprint unchanged, so it proves nothing")
		}
	}()
	for _, path := range []string{
		filepath.Join(request.CityPath, ".gc", "events.jsonl"),
		filepath.Join(request.CityPath, config.DefaultSQLiteStoragePath),
		migrationWindowRecordPath(request.CityPath),
		minGCVersionPath(request.CityPath),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the rehearsal left %s (stat err %v)", path, err)
		}
	}
}

// TestSplitPreflightStartsNothingWhenTheProviderIsDown: the migration's window
// starts a stopped provider; the rehearsal reports that and reads nothing.
func TestSplitPreflightStartsNothingWhenTheProviderIsDown(t *testing.T) {
	request, _, provider := splitReadyCity(t, 10, nativeDoltLane)
	stubMigrationProviderDown(t, true)
	opened := false
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		opened = true
		return nil, errors.New("opened")
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
	code, summary, out := runSplitPreflight(t, request)
	if code == 0 || summary.ready() || !strings.Contains(summary.Unread, "is down") {
		t.Fatalf("a city whose provider is down cleared: exit %d, %+v:\n%s", code, summary, out)
	}
	if opened || provider.starts != 0 || provider.stops != 0 {
		t.Errorf("opened the work store: %t; provider starts %d, stops %d", opened, provider.starts, provider.stops)
	}
	if !strings.Contains(out, "the split starts it in its maintenance window") || !strings.Contains(out, "did not run, because this command starts nothing") {
		t.Errorf("the report does not say why nothing was read:\n%s", out)
	}
}

// TestSplitStoreReadGate: an open window owns the provider, so the guided
// split's own rehearsal reads inside it even with the provider down.
func TestSplitStoreReadGate(t *testing.T) {
	city := t.TempDir()
	stubMigrationProviderDown(t, true)
	if why := splitStoreReadGate(city, &config.City{}); !strings.Contains(why, "is down") {
		t.Errorf("gate with the provider down = %q", why)
	}
	closeWindow := openPreflightLaneWindow(city, &config.City{})
	defer closeWindow()
	if why := splitStoreReadGate(city, &config.City{}); why != "" {
		t.Errorf("gate inside a window = %q, want none", why)
	}
	stubMigrationProviderDown(t, false)
	if why := splitStoreReadGate(t.TempDir(), &config.City{}); why != "" {
		t.Errorf("gate with the provider up = %q, want none", why)
	}
}

// TestSplitPreflightRefusals maps each refusal the split would hit to a
// blocking check and a non-zero exit: the authored config, a served note,
// the C11 census, an S5-D hold, an S5-B lane gate, and the disk.
func TestSplitPreflightRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, request *storageOperatorRequest, source beads.Store)
		block string
		want  string
	}{
		{
			name: "authored storage",
			setup: func(_ *testing.T, request *storageOperatorRequest, _ beads.Store) {
				request.Cfg = infraSplitConfig(".gc/store")
			},
			block: "config", want: "already authors [storage]",
		},
		{
			name: "a cleared work store's note",
			setup: func(t *testing.T, request *storageOperatorRequest, _ beads.Store) {
				if err := writeInfraClearedNote(request.CityPath, infraClearedNote{Binding: "infra", Complete: true}); err != nil {
					t.Fatal(err)
				}
			},
			block: "config",
		},
		{
			name: "v2 boot census",
			setup: func(t *testing.T, request *storageOperatorRequest, _ beads.Store) {
				request.Cfg.Agents = append(request.Cfg.Agents, allocPoolAgent("worker", 3))
				stubSplitPreflightLegs(t, []v2SessionLeg{{ref: "city", store: censusStore(poolRow("gc-a0", "worker", 1, "archived"))}})
			},
			block: "v2 sessions", want: "v2's boot census refuses",
		},
		{
			name: "an unreadable census leg",
			setup: func(t *testing.T, _ *storageOperatorRequest, _ beads.Store) {
				stubSplitPreflightLegs(t, []v2SessionLeg{{ref: "city", notChecked: "open failed"}})
			},
			block: "v2 sessions", want: "could not be read",
		},
		{
			name: "a waits-for onto open work (S5-D hold)",
			setup: func(t *testing.T, _ *storageOperatorRequest, source beads.Store) {
				work := mustCreateInfraBead(t, source, beads.Bead{Title: "open work", Type: "task"})
				waiting := mustCreateInfraBead(t, source, beads.Bead{Title: "a session waiting on it", Type: "session"})
				if err := source.DepAdd(waiting.ID, work.ID, "waits-for"); err != nil {
					t.Fatal(err)
				}
			},
			block: "migration", want: "[BLOCK] holds",
		},
		{
			name: "the bd CLI lane under hooks (S5-B gate)",
			setup: func(t *testing.T, _ *storageOperatorRequest, source beads.Store) {
				hooked := beads.BeadsDiagnostic{Store: beads.BeadsStoreNameBdStore, PreflightGate: beads.BeadsGateBdHooks, PreflightReason: "hooks"}
				blind := &payloadUnanswerableStore{Store: source}
				prev := openInfraMigrationSource
				openInfraMigrationSource = func(cityPath string) (beads.Store, error) {
					recordMigrationLane(cityPath, hooked)
					return blind, nil
				}
				t.Cleanup(func() { openInfraMigrationSource = prev })
			},
			block: "migration", want: "Executable bd hooks",
		},
		{
			name: "a disk the split would not fit",
			setup: func(t *testing.T, _ *storageOperatorRequest, _ beads.Store) {
				prev := doltContainerFreeBytesFunc
				doltContainerFreeBytesFunc = func(string) (int64, error) { return 1 << 20, nil }
				t.Cleanup(func() { doltContainerFreeBytesFunc = prev })
			},
			block: "disk", want: "[BLOCK] disk",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, source, _ := splitReadyCity(t, 40, nativeDoltLane)
			tc.setup(t, &request, source)
			code, summary, out := runSplitPreflight(t, request)
			if code != 1 || !slices.Contains(summary.Blocks, tc.block) {
				t.Fatalf("exit %d, blocks %v, want %q:\n%s", code, summary.Blocks, tc.block, out)
			}
			if !strings.Contains(out, tc.want) || !strings.Contains(out, "the split would refuse") {
				t.Errorf("the report lacks %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestSplitPreflightCountsTheResumesABudgetCosts: a budget under the
// estimate is a note naming the pauses, not a refusal, and the count is the
// arithmetic over the estimate.
func TestSplitPreflightCountsTheResumesABudgetCosts(t *testing.T) {
	request, _, _ := splitReadyCity(t, 120, nativeDoltLane)
	request.TimeBudget = time.Second
	code, summary, out := runSplitPreflight(t, request)
	if code != 0 || summary.Estimate == nil {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	est := summary.Estimate
	want := splitBudgetResumes(est.time.copy+est.time.clear, time.Second)
	if want == 0 || est.resumes != want {
		t.Fatalf("resumes = %d, want %d (a non-zero count, or the fixture proves nothing)", est.resumes, want)
	}
	if !strings.Contains(out, "[note]  time budget") || !strings.Contains(out, "need "+strconv.Itoa(want)+" resume(s)") {
		t.Errorf("the report does not name the resumes:\n%s", out)
	}
}

// TestSplitDiskEstimateCoversARealBinding migrates the rehearsed city into a
// real binding and holds the binding estimate between the size it leaves and
// twice that.
func TestSplitDiskEstimateCoversARealBinding(t *testing.T) {
	request, _, _ := splitReadyCity(t, 1000, nativeDoltLane)
	_, summary, out := runSplitPreflight(t, request)
	if summary.Estimate == nil {
		t.Fatalf("no estimate:\n%s", out)
	}
	cfg := splitHypotheticalConfig(request.Cfg)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, request.CityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("migrating: %s: %s", got.Outcome, log.String())
	}
	var actual int64
	root := filepath.Join(request.CityPath, config.DefaultSQLiteStoragePath)
	if err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			actual += info.Size()
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if est := summary.Estimate.disk.binding; est < actual || est > 2*actual {
		t.Errorf("binding estimate %d against %d written: want within [1x, 2x]", est, actual)
	}
}

// TestStoragePreflightSplitFlags: --split and --time-budget are on the
// command, and a budget without --split is refused before anything runs.
func TestStoragePreflightSplitFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStorageCmd(&stdout, &stderr)
	cmd.SetArgs([]string{storagePreflightVerb, "--" + storageTimeBudgetFlag, "1m"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(stderr.String(), "needs --split") {
		t.Errorf("--time-budget without --split: err %v, stderr %q", err, stderr.String())
	}
	preflight, _, err := newStorageCmd(io.Discard, io.Discard).Find([]string{storagePreflightVerb})
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{storagePreflightSplitFlag, storageTimeBudgetFlag} {
		if preflight.Flags().Lookup(flag) == nil {
			t.Errorf("gc storage preflight has no --%s", flag)
		}
	}
}

// payloadUnanswerableStore is a work store that cannot report edge payloads,
// as the bd CLI lane cannot.
type payloadUnanswerableStore struct {
	beads.Store
}
