package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worktree"
)

// lockedClock is a clock.Fake the demand pass's per-leg goroutines can use
// concurrently. A non-zero step advances it after every Now, so a segment
// with no clock reads inside it measures exactly one step, and a segment
// whose start was not reset measures more.
type lockedClock struct {
	mu   sync.Mutex
	fake clock.Fake
	step time.Duration
}

func newLockedClock(step time.Duration) *lockedClock {
	return &lockedClock{fake: clock.Fake{Time: time.Unix(1_700_000_000, 0)}, step: step}
}

func (c *lockedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.fake.Now()
	c.fake.Advance(c.step)
	return now
}

func (c *lockedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fake.Advance(d)
}

// clockAdvancingStore advances a clock inside each read by an amount fixed per
// read shape, so a trace record's duration proves the read was timed by the
// record that names it and by no other.
type clockAdvancingStore struct {
	beads.Store
	clock *lockedClock
}

// Per-call costs. Distinct values make a duration attributable to one shape:
// a record that timed the wrong call, or two calls, cannot land on its own
// value.
const (
	advanceListInProgress = 3 * time.Millisecond
	advanceListOpenLive   = 5 * time.Millisecond
	advanceListOpenCached = 7 * time.Millisecond
	advanceListOther      = 11 * time.Millisecond
	advanceReady          = 13 * time.Millisecond
	advanceMetadataWrite  = 100 * time.Millisecond
	advanceCreate         = 1000 * time.Millisecond
)

func (s *clockAdvancingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	switch {
	case query.Status == "in_progress":
		s.clock.Advance(advanceListInProgress)
	case query.Status == "open" && query.Live:
		s.clock.Advance(advanceListOpenLive)
	case query.Status == "open":
		s.clock.Advance(advanceListOpenCached)
	default:
		s.clock.Advance(advanceListOther)
	}
	return s.Store.List(query)
}

func (s *clockAdvancingStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.clock.Advance(advanceReady)
	return s.Store.Ready(query...)
}

// writeAdvancingStore advances a clock only on writes, so realization's
// segments can be told apart: a create is Phase B, a metadata write is a
// trigger binding.
type writeAdvancingStore struct {
	beads.Store
	clock *lockedClock
}

func (s *writeAdvancingStore) Create(b beads.Bead) (beads.Bead, error) {
	s.clock.Advance(advanceCreate)
	return s.Store.Create(b)
}

func (s *writeAdvancingStore) Update(id string, opts beads.UpdateOpts) error {
	s.clock.Advance(advanceMetadataWrite)
	return s.Store.Update(id, opts)
}

func (s *writeAdvancingStore) SetMetadata(id, key, value string) error {
	s.clock.Advance(advanceMetadataWrite)
	return s.Store.SetMetadata(id, key, value)
}

func (s *writeAdvancingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.clock.Advance(advanceMetadataWrite)
	return s.Store.SetMetadataBatch(id, kvs)
}

// demandTraceCycle begins a trace cycle whose demand durations come from c.
func demandTraceCycle(t *testing.T, cityDir string, c clock.Clock) (*SessionReconcilerTracer, *SessionReconcilerTraceCycle) {
	t.Helper()
	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.durationClock = c
	return tracer, cycle
}

// demandTraceRecords ends the cycle and reads back, through the durable
// segment files, every demand-snapshot operation record, keyed by
// operation_name.
func demandTraceRecords(t *testing.T, cityDir string, tracer *SessionReconcilerTracer, cycle *SessionReconcilerTraceCycle) map[string][]SessionReconcilerTraceRecord {
	t.Helper()
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	out := make(map[string][]SessionReconcilerTraceRecord)
	for _, r := range records {
		if r.RecordType != TraceRecordOperation || r.SiteCode != TraceSiteDemandSnapshot {
			continue
		}
		name := demandTraceString(r, "operation_name")
		out[name] = append(out[name], r)
	}
	return out
}

func demandTraceString(r SessionReconcilerTraceRecord, key string) string {
	v, _ := r.Fields[key].(string)
	return v
}

// demandTraceInt reads a numeric field as the JSON round trip decodes it, and
// fails on a missing one so a zero expectation cannot pass by absence.
func demandTraceInt(t *testing.T, r SessionReconcilerTraceRecord, key string) int {
	t.Helper()
	v, ok := r.Fields[key].(float64)
	if !ok {
		t.Fatalf("record %v: field %q = %#v, want a number", r.Fields, key, r.Fields[key])
	}
	return int(v)
}

type demandReadKey struct{ point, leg, op, tier string }

func (k demandReadKey) String() string {
	return fmt.Sprintf("%s|%s|%s|%s", k.point, k.leg, k.op, k.tier)
}

func readKeyOf(r SessionReconcilerTraceRecord) demandReadKey {
	return demandReadKey{
		point: demandTraceString(r, "point"),
		leg:   demandTraceString(r, "leg"),
		op:    demandTraceString(r, "op"),
		tier:  demandTraceString(r, "tier"),
	}
}

func storeReadsByKey(records []SessionReconcilerTraceRecord) map[demandReadKey][]SessionReconcilerTraceRecord {
	out := make(map[demandReadKey][]SessionReconcilerTraceRecord)
	for _, r := range records {
		out[readKeyOf(r)] = append(out[readKeyOf(r)], r)
	}
	return out
}

func sortedReadKeys[V any](m map[demandReadKey]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k.String())
	}
	sort.Strings(keys)
	return keys
}

// TestDemandReadTrace_RecordsEveryLegOpTierRows is the demand pass's read
// budget: the EXACT set of store reads one rebuild issues, each with its read
// point, store label, operation, tier, row count and own duration. An extra
// read (a second live Ready per store group) or a dropped one fails it.
//
// The city leg is a plain store. The rig leg is a primed CachingStore over a
// backing whose reads advance the clock, so its cached reads are in-memory
// (0ms) and its live reads cost exactly their shape; each rig read is timed
// alone on its own goroutine, and the plain city store advances nothing a rig
// record could absorb.
func TestDemandReadTrace_RecordsEveryLegOpTierRows(t *testing.T) {
	// The non-tick path passes no trace; the recorders must be nil-safe.
	recordDemandStoreRead(nil, demandStoreRead{}, time.Now(), 0, nil)
	var nilPass *demandPassTrace
	nilPass.read(demandStoreRead{}, nilPass.now(), 0, nil)

	cityDir := t.TempDir()
	cfg := warmCrossStoreCfg(t, cityDir)
	cfg.NamedSessions = []config.NamedSession{{Template: "worker", Dir: "gascity", Mode: "on_demand"}}
	clk := newLockedClock(0)
	cityStore := beads.NewMemStore()
	backing := &clockAdvancingStore{Store: beads.NewMemStore(), clock: clk}
	for i := 0; i < 2; i++ {
		if _, err := backing.Create(beads.Bead{
			Title:    fmt.Sprintf("routed %d", i),
			Type:     "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: warmWorkerTemplate},
		}); err != nil {
			t.Fatal(err)
		}
	}
	rigStore := beads.NewCachingStoreForTest(backing, nil)
	if err := rigStore.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	tracer, cycle := demandTraceCycle(t, cityDir, clk)
	sessionSnapshot, err := loadSessionBeadSnapshot(cityStore)
	if err != nil {
		t.Fatal(err)
	}
	buildDesiredStateWithSessionBeads("gc", cityDir, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": rigStore}, sessionSnapshot, cycle, io.Discard)

	reads := storeReadsByKey(demandTraceRecords(t, cityDir, tracer, cycle)["demand_snapshot.store_read"])

	const city, rig = "city:gc", "rig:gascity"
	type want struct {
		rows int           // -1: omitted (not a row set)
		dur  time.Duration // -1: not asserted (the plain city store is not clocked)
	}
	wants := map[demandReadKey]want{
		{demandReadPointSessionCensus, city, "list_sessions", demandReadTierCached}: {0, -1},
		{demandReadPointSessionCensus, rig, "list_sessions", demandReadTierCached}:  {0, 0},
		{demandReadPointAssigned, city, "list_in_progress", demandReadTierCached}:   {0, -1},
		{demandReadPointAssigned, rig, "list_in_progress", demandReadTierCached}:    {0, 0},
		{demandReadPointAssigned, city, "list_open", demandReadTierLive}:            {0, -1},
		{demandReadPointAssigned, rig, "list_open", demandReadTierLive}:             {2, advanceListOpenLive},
		{demandReadPointAssigned, city, "list_open", demandReadTierCached}:          {0, -1},
		{demandReadPointAssigned, rig, "list_open", demandReadTierCached}:           {2, 0},
		{demandReadPointAssigned, city, "closed_named_index", demandReadTierCached}: {-1, -1},
		{demandReadPointAssigned, city, "ready", demandReadTierLive}:                {0, -1},
		{demandReadPointAssigned, rig, "ready", demandReadTierLive}:                 {2, advanceReady},
		{demandReadPointUnassignedRouted, city, "list_open", demandReadTierLive}:    {0, -1},
		{demandReadPointUnassignedRouted, rig, "list_open", demandReadTierLive}:     {2, advanceListOpenLive},
		{demandReadPointDemand, city, "ready", demandReadTierCached}:                {0, -1},
		{demandReadPointDemand, rig, "ready", demandReadTierCached}:                 {2, -1},
		{demandReadPointDemand, rig, "ready", demandReadTierLive}:                   {2, advanceReady},
	}
	if got, exp := sortedReadKeys(reads), sortedReadKeys(wants); strings.Join(got, "\n") != strings.Join(exp, "\n") {
		t.Fatalf("store_read set:\n got %v\nwant %v", got, exp)
	}
	for key, w := range wants {
		got := reads[key]
		if len(got) != 1 {
			t.Errorf("store_read %s: %d records, want 1", key, len(got))
			continue
		}
		r := got[0]
		if r.OutcomeCode != TraceOutcomeComplete {
			t.Errorf("store_read %s: outcome %q, want %q", key, r.OutcomeCode, TraceOutcomeComplete)
		}
		if w.rows >= 0 {
			if rows := demandTraceInt(t, r, "rows"); rows != w.rows {
				t.Errorf("store_read %s: rows = %d, want %d", key, rows, w.rows)
			}
		} else if _, ok := r.Fields["rows"]; ok {
			t.Errorf("store_read %s: rows = %v, want omitted for a read that is not a row set", key, r.Fields["rows"])
		}
		if w.dur >= 0 && r.DurationMS != w.dur.Milliseconds() {
			t.Errorf("store_read %s: duration_ms = %d, want %d", key, r.DurationMS, w.dur.Milliseconds())
		}
	}
}

// TestDemandReadTrace_LegNamesTheStoreThatServedIt: on both topologies every
// store_read record's leg names the physical store that answered it. On a
// split city the "city" role is the sessions binding for the role-keyed reads
// (the closed named-session index, the demand group keyed "city") and the work
// ledger for the census legs, so a label taken from the role would give two
// stores one name. Each store is seeded with a distinct number of open rows,
// so a record's row count identifies the store that served it.
func TestDemandReadTrace_LegNamesTheStoreThatServedIt(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		e.cfg.NamedSessions = []config.NamedSession{{Template: splitEnvPoolAgent, Dir: splitEnvRigName, Mode: "on_demand"}}
		cityLabel := "city:" + e.cfg.Workspace.Name
		rigLabel := "rig:" + e.rigName
		classLabel := string(storeref.ClassRef(splitEnvInfraClasses()))

		// Rows each label must report on open-list and ready reads.
		wantRows := map[string]int{}
		seed := func(store beads.Store, n int, title string) {
			for i := 0; i < n; i++ {
				if _, err := store.Create(beads.Bead{Title: fmt.Sprintf("%s %d", title, i), Type: "task"}); err != nil {
					t.Fatal(err)
				}
			}
		}
		e.mintWispWith(t, wispOpts{title: "routed wisp A", routedTo: e.qualified})
		e.mintWispWith(t, wispOpts{title: "routed wisp B", routedTo: e.qualified})
		seed(e.rig, 4, "rig task")
		if e.split {
			seed(e.work, 1, "ledger task")
			wantRows[cityLabel], wantRows[classLabel], wantRows[rigLabel] = 1, 2, 4
		} else {
			wantRows[cityLabel], wantRows[rigLabel] = 2, 4
		}

		tracer, cycle := demandTraceCycle(t, e.cityPath, nil)
		buildDesiredStateWithSessionBeads("split-topology-city", e.cityPath, time.Now(), e.cfg, &localMockProvider{},
			e.sessionsStore(), e.rigStores, &sessionBeadSnapshot{}, cycle, io.Discard)
		reads := demandTraceRecords(t, e.cityPath, tracer, cycle)["demand_snapshot.store_read"]
		if len(reads) == 0 {
			t.Fatal("no store_read records")
		}

		sessionsLabel := cityLabel
		if e.split {
			sessionsLabel = classLabel
		}
		for _, r := range reads {
			key := readKeyOf(r)
			if _, known := wantRows[key.leg]; !known {
				t.Errorf("store_read %s: leg %q names no store of this city (want one of %v)", key, key.leg, wantRows)
				continue
			}
			switch key.op {
			case "list_open", "ready":
				if got := demandTraceInt(t, r, "rows"); got != wantRows[key.leg] {
					t.Errorf("store_read %s: rows = %d, but %q holds %d: the label names a different store than the one that answered", key, got, key.leg, wantRows[key.leg])
				}
			case "closed_named_index":
				// The index reads the store the pass was handed: the sessions
				// binding on a split city.
				if key.leg != sessionsLabel {
					t.Errorf("closed_named_index leg = %q, want %q (the store it reads)", key.leg, sessionsLabel)
				}
			}
		}
	})
}

// splitEnvInfraClasses is the class set splitEnvRoutes relocates.
func splitEnvInfraClasses() []coordclass.Class {
	var classes []coordclass.Class
	for _, c := range coordclass.Classes() {
		if c.IsInfrastructure() {
			classes = append(classes, c)
		}
	}
	return classes
}

// TestDemandReadTrace_FailedReadRecordsErrorAndOutcome: a failed leg read is
// recorded as failed with its error, bounded, so an outage is attributable to
// the leg without a subprocess stderr blowing up the record.
func TestDemandReadTrace_FailedReadRecordsErrorAndOutcome(t *testing.T) {
	cityDir := t.TempDir()
	tracer, cycle := demandTraceCycle(t, cityDir, nil)
	cfg := &config.City{Workspace: config.Workspace{Name: "gc"}}
	store := longErrorStore{Store: beads.NewMemStore()}
	collectOpenUnassignedRoutedWork("", cfg, store, nil, nil, io.Discard, newDemandPassTrace(cycle, cfg), nil)

	records := demandTraceRecords(t, cityDir, tracer, cycle)["demand_snapshot.store_read"]
	if len(records) != 1 {
		t.Fatalf("store_read records = %d, want 1: %v", len(records), records)
	}
	r := records[0]
	if r.OutcomeCode != TraceOutcomeFailed {
		t.Errorf("outcome = %q, want %q", r.OutcomeCode, TraceOutcomeFailed)
	}
	if msg := demandTraceString(r, "error"); len(msg) != demandTraceErrorMax || !strings.HasPrefix(msg, "list down") {
		t.Errorf("error = %d bytes %q, want the read's error cut to %d bytes", len(msg), msg, demandTraceErrorMax)
	}
	if key, want := readKeyOf(r), (demandReadKey{demandReadPointUnassignedRouted, "city:gc", "list_open", demandReadTierLive}); key != want {
		t.Errorf("read = %s, want %s", key, want)
	}
}

type longErrorStore struct{ beads.Store }

func (longErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list down: " + strings.Repeat("x", 4096))
}

// closedIndexStore answers the closed named-session index's by-type read with
// rows and a partial error, or with a hard error.
type closedIndexStore struct {
	*beads.MemStore
	hard bool
}

func (s *closedIndexStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(query)
	if !query.IncludeClosed || query.Type != session.BeadType {
		return rows, err
	}
	if s.hard {
		return nil, errors.New("closed index down")
	}
	return rows, &beads.PartialResultError{Op: "bd list", Err: errors.New("one leg down")}
}

// TestReadyAssignedWorkAssigneesClosedIndexFailsOpen pins the closed
// named-session index's degraded reads, and that their record carries the
// outcome. A hard failure leaves the index empty but every on_demand identity
// is still probed under its qualified name; a partial read keeps the rows it
// got, so a closed phantom still adds its runtime-name form.
func TestReadyAssignedWorkAssigneesClosedIndexFailsOpen(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city", SessionTemplate: "{{.City}}--{{.Agent}}"},
		NamedSessions: []config.NamedSession{{Template: "named-worker", Mode: "on_demand"}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	runtimeName := config.NamedSessionRuntimeName("test-city", cfg.Workspace, identity)
	if runtimeName == identity {
		t.Fatalf("fixture runtime name %q equals the identity; the test could not tell the two forms apart", runtimeName)
	}
	for _, tc := range []struct {
		name        string
		hard        bool
		outcome     TraceOutcomeCode
		wantRuntime bool
	}{
		{"hard failure", true, TraceOutcomeFailed, false},
		{"partial read", false, TraceOutcomePartial, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &closedIndexStore{MemStore: beads.NewMemStore(), hard: tc.hard}
			if _, err := store.Create(beads.Bead{
				Title:    "closed phantom",
				Type:     session.BeadType,
				Labels:   []string{session.LabelSession},
				Metadata: map[string]string{session.NamedSessionIdentityMetadata: identity},
			}); err != nil {
				t.Fatal(err)
			}
			rows, _ := store.MemStore.List(beads.ListQuery{Type: session.BeadType})
			if err := store.Close(rows[0].ID); err != nil {
				t.Fatal(err)
			}
			cityDir := t.TempDir()
			tracer, cycle := demandTraceCycle(t, cityDir, nil)

			got := readyAssignedWorkAssignees(cfg, store, nil, nil, newDemandPassTrace(cycle, cfg), demandReadPointAssigned, nil)

			has := func(v string) bool {
				for _, g := range got {
					if g == v {
						return true
					}
				}
				return false
			}
			if !has(identity) {
				t.Errorf("assignees = %v, want the on_demand identity %q still probed", got, identity)
			}
			if has(runtimeName) != tc.wantRuntime {
				t.Errorf("assignees = %v: runtime-name form %q present = %v, want %v", got, runtimeName, has(runtimeName), tc.wantRuntime)
			}
			records := demandTraceRecords(t, cityDir, tracer, cycle)["demand_snapshot.store_read"]
			if len(records) != 1 || records[0].OutcomeCode != tc.outcome {
				t.Fatalf("closed_named_index records = %v, want one with outcome %q", records, tc.outcome)
			}
		})
	}
}

// TestDemandReadTrace_FingerprintLabelsTheLegCanonically: the patrol reuse
// probe records its live Ready per routed-work leg under the leg's canonical
// label — the class ref on a split city, "city:<ws>" on a single-store one —
// not the spelling its hash and log line use.
func TestDemandReadTrace_FingerprintLabelsTheLegCanonically(t *testing.T) {
	t.Run("split", func(t *testing.T) {
		clk := newLockedClock(0)
		binding := &clockAdvancingStore{Store: beads.NewMemStore(), clock: clk}
		cr := bindingFingerprintRuntime(t, beads.NewMemStore(), binding)
		routedStepIn(t, binding.Store, "routed graph step")
		tracer, cycle := demandTraceCycle(t, cr.cityPath, clk)

		cr.readyDemandSnapshotFingerprint(cycle)

		records := demandTraceRecords(t, cr.cityPath, tracer, cycle)["demand_snapshot.store_read"]
		if len(records) != 1 {
			t.Fatalf("store_read records = %d, want 1: %v", len(records), records)
		}
		r := records[0]
		classRef := string(storeref.ClassRef([]coordclass.Class{
			coordclass.ClassGraph, coordclass.ClassSessions, coordclass.ClassMessaging, coordclass.ClassOrders, coordclass.ClassNudges,
		}))
		if got, want := readKeyOf(r), (demandReadKey{demandReadPointFingerprint, classRef, "ready", demandReadTierLive}); got != want {
			t.Errorf("fingerprint read = %s, want %s", got, want)
		}
		if rows := demandTraceInt(t, r, "rows"); rows != 1 {
			t.Errorf("rows = %d, want 1", rows)
		}
		if r.DurationMS != advanceReady.Milliseconds() {
			t.Errorf("duration_ms = %d, want %d", r.DurationMS, advanceReady.Milliseconds())
		}
	})
	t.Run("single store", func(t *testing.T) {
		cityPath := t.TempDir()
		cr := &CityRuntime{
			cityName: "test-city",
			cityPath: cityPath,
			cfg:      &config.City{Workspace: config.Workspace{Name: "test-city"}},
			cs: &controllerState{
				cityName:      "test-city",
				cityBeadStore: beads.NewMemStore(),
				eventProv:     events.NewFake(),
			},
			stderr: io.Discard,
		}
		before := cr.readyDemandSnapshotFingerprint(nil)
		tracer, cycle := demandTraceCycle(t, cityPath, nil)
		if traced := cr.readyDemandSnapshotFingerprint(cycle); traced != before {
			t.Fatalf("tracing changed the fingerprint: %q -> %q", before, traced)
		}
		records := demandTraceRecords(t, cityPath, tracer, cycle)["demand_snapshot.store_read"]
		if len(records) != 1 {
			t.Fatalf("store_read records = %d, want 1", len(records))
		}
		if got := demandTraceString(records[0], "leg"); got != "city:test-city" {
			t.Errorf("fingerprint leg = %q, want %q", got, "city:test-city")
		}
	})
}

// TestDemandSubPhaseTrace_SecondPoolComputeExcludesTheBuild: the pool
// recompute loadDemandSnapshot runs after the build records its own sub-phase
// with the pool count, and does not absorb the build's time.
func TestDemandSubPhaseTrace_SecondPoolComputeExcludesTheBuild(t *testing.T) {
	cityPath := t.TempDir()
	clk := newLockedClock(0)
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: cityPath,
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents: []config.Agent{{
				Name:              "worker",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(5),
			}},
		},
		cs: &controllerState{
			cityName:      "test-city",
			cityPath:      cityPath,
			cityBeadStore: beads.NewMemStore(),
			eventProv:     events.NewFake(),
		},
		stderr: io.Discard,
	}
	cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
		clk.Advance(advanceCreate)
		return DesiredStateResult{ScaleCheckCounts: map[string]int{"worker": 2}}
	}
	tracer, cycle := demandTraceCycle(t, cityPath, clk)

	snapshot := cr.loadDemandSnapshot(newSessionBeadSnapshot(nil), cycle, "poke", false)

	records := demandTraceRecords(t, cityPath, tracer, cycle)["demand_snapshot.second_pool_compute"]
	if len(records) != 1 {
		t.Fatalf("second_pool_compute records = %d, want 1", len(records))
	}
	r := records[0]
	if got, want := demandTraceInt(t, r, "pools"), len(snapshot.result.PoolDesiredCounts); got != want || want == 0 {
		t.Errorf("second_pool_compute pools = %d, want %d (non-zero)", got, want)
	}
	if r.DurationMS != 0 {
		t.Errorf("second_pool_compute duration_ms = %d, want 0: the build's time is not the recompute's", r.DurationMS)
	}
}

// TestBuildDesiredStateRecordsDemandSubPhases verifies the sub-phase operation
// records emitted inside buildDesiredStateWithSessionBeads (sr-5rz /
// gastownhall/gascity#2463, extended for the realization tail): the aggregate
// load_demand_snapshot tick phase regularly dominates the controller cycle,
// and these records are what make its internal split attributable from a
// trace instead of requiring an instrumented rebuild.
//
// The clock steps 1ms on every read, so a sub-phase with no timed work inside
// it measures exactly 1ms, and one whose start was not reset measures the
// segments before it too. The store advances the clock on writes, so a create
// (1000ms) and a trigger-binding metadata write (100ms) land in exactly one
// realize_pools field each.
func TestBuildDesiredStateRecordsDemandSubPhases(t *testing.T) {
	// The non-tick path passes no trace; the recorder must be nil-safe.
	recordDemandSubPhase(nil, "demand_snapshot.collect_open_session_beads", time.Now(), nil)

	cityDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
			{Name: "mayor", StartCommand: "true", MaxActiveSessions: intPtr(1)},
		},
		NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "always"}},
	}
	clk := newLockedClock(time.Millisecond)
	mem := beads.NewMemStore()
	for i := 0; i < 2; i++ {
		if _, err := mem.Create(beads.Bead{
			Title:    fmt.Sprintf("routed %d", i),
			Type:     "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A warm pool seat the realization reuses, beside the one it creates.
	if _, err := mem.Create(beads.Bead{
		Title:  "worker seat",
		Type:   sessionBeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"template":     "worker",
			"agent_name":   "worker-1",
			"pool_slot":    "1",
			"state":        "active",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatal(err)
	}
	store := &writeAdvancingStore{Store: mem, clock: clk}
	tracer, cycle := demandTraceCycle(t, cityDir, clk)
	sessionSnapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatal(err)
	}
	result := buildDesiredStateWithSessionBeads("gc", cityDir, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, sessionSnapshot, cycle, io.Discard)
	if len(result.State) != 3 {
		t.Fatalf("desired sessions = %d, want 3 (the reused seat, the created one, the named mayor)", len(result.State))
	}
	byName := demandTraceRecords(t, cityDir, tracer, cycle)
	one := func(name string) SessionReconcilerTraceRecord {
		t.Helper()
		got := byName["demand_snapshot."+name]
		if len(got) != 1 {
			t.Fatalf("%s: %d records, want exactly 1", name, len(got))
		}
		return got[0]
	}
	fields := func(name string, want map[string]int) SessionReconcilerTraceRecord {
		t.Helper()
		r := one(name)
		for key, w := range want {
			if got := demandTraceInt(t, r, key); got != w {
				t.Errorf("%s %s = %d, want %d", name, key, got, w)
			}
		}
		return r
	}

	for _, name := range []string{
		"collect_open_session_beads", "collect_assigned_work", "collect_unassigned_routed",
		"evaluate_pending_pools", "default_scale_demand", "named_session_demand",
	} {
		one(name)
	}
	// The pass's own writes: three and four steps timed back to back, each
	// empty here, so each measures one step.
	fields("assigned_repairs", map[string]int{"repair_ms": 1, "stamp_ms": 1, "canonicalize_ms": 1})
	fields("unassigned_repairs", map[string]int{"beads": 2, "repair_ms": 1, "canonicalize_ms": 1, "collapse_ms": 1, "dispatcher_repair_ms": 1})

	// Realization: one reuse and one create. The create's 1000ms and the
	// create path's own metadata write (100ms) are Phase B; the reused seat's
	// trigger binding is the one bind write (100ms). Each Phase C segment runs
	// once per item, a step each.
	realize := fields("realize_pools", map[string]int{
		"pools": 1, "requests": 2, "creates": 1, "create_errors": 0,
		"bind_attempts": 2, "bind_writes": 1, "bind_errors": 0, "worktree_verifies": 0, "realized": 2,
		"plan_ms": 1, "create_ms": 1101, "bind_ms": 102, "resolve_ms": 2, "side_effects_ms": 2,
	})
	// The whole record: 1200ms of writes plus one step for each of the 17
	// clock reads inside it. A start left at the previous boundary would also
	// count the compute segment.
	if realize.DurationMS != 1217 {
		t.Errorf("realize_pools duration_ms = %d, want 1217: its start must be reset at its own boundary", realize.DurationMS)
	}

	// Sub-phases with no timed work inside measure one step; a start that
	// was not reset would also count the segment before.
	for name, want := range map[string]map[string]int{
		"compute_pool_desired":      {"pools": 1},
		"named_session_materialize": {"specs": 1, "materialized": 1},
		"session_overlay":           {"base": 3, "desired": 3},
		"continuation_candidates":   {"candidates": 0},
	} {
		if r := fields(name, want); r.DurationMS != 1 {
			t.Errorf("%s duration_ms = %d, want 1: its start must be reset at its own boundary", name, r.DurationMS)
		}
	}
	if partial, _ := one("continuation_candidates").Fields["partial"].(bool); partial {
		t.Error("continuation_candidates partial = true, want false")
	}
}

// TestVerifiedPoolTriggerWorkDirCountsWorktreeVerifies: the realize_pools
// worktree_verifies counter counts the calls that reach worktree.Verify, and
// not requests the evidence guard refuses first.
func TestVerifiedPoolTriggerWorkDirCountsWorktreeVerifies(t *testing.T) {
	bp := &agentBuildParams{realizeProbe: &poolRealizeProbe{}}
	refused := SessionRequest{WorkBeadID: "gc-a", WorktreeError: "bad evidence"}
	_, _ = verifiedPoolTriggerWorkDir(bp, nil, "rig/pool", refused)
	if got := bp.realizeProbe.worktreeVerifies.Load(); got != 0 {
		t.Fatalf("worktree_verifies after a refused request = %d, want 0", got)
	}
	verified := SessionRequest{WorkBeadID: "gc-a", WorktreeSpec: &worktree.Spec{BeadID: "gc-a"}}
	_, _ = verifiedPoolTriggerWorkDir(bp, nil, "rig/pool", verified)
	if got := bp.realizeProbe.worktreeVerifies.Load(); got != 1 {
		t.Fatalf("worktree_verifies after one verified request = %d, want 1", got)
	}
}
