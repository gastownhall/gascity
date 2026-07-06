package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storehealth"
)

func TestCachedStoreHealthServesMemoized(t *testing.T) {
	var calls int
	want := &StatusStoreHealth{Path: "/c/.beads/dolt", SizeBytes: 123}
	s := &Server{}
	s.storeHealthComputer = func(context.Context) *StatusStoreHealth {
		calls++
		return want
	}

	now := time.Unix(1_000_000, 0)
	got := s.cachedStoreHealth(context.Background(), now)
	if got != want {
		t.Fatalf("cachedStoreHealth = %+v, want %+v", got, want)
	}
	if calls != 1 {
		t.Fatalf("computer called %d times, want 1", calls)
	}

	// Within TTL: no recomputation.
	got2 := s.cachedStoreHealth(context.Background(), now.Add(storeHealthCacheTTL-time.Second))
	if got2 != want {
		t.Fatalf("second cachedStoreHealth = %+v, want %+v", got2, want)
	}
	if calls != 1 {
		t.Fatalf("computer called %d times within TTL, want 1", calls)
	}
}

func TestCachedStoreHealthRefreshesAfterTTL(t *testing.T) {
	var calls int
	s := &Server{}
	s.storeHealthComputer = func(context.Context) *StatusStoreHealth {
		calls++
		return &StatusStoreHealth{SizeBytes: int64(calls)}
	}

	now := time.Unix(1_000_000, 0)
	_ = s.cachedStoreHealth(context.Background(), now)
	later := now.Add(storeHealthCacheTTL + time.Second)
	got := s.cachedStoreHealth(context.Background(), later)
	if calls != 2 {
		t.Fatalf("computer calls = %d, want 2", calls)
	}
	if got.SizeBytes != 2 {
		t.Fatalf("refreshed entry SizeBytes = %d, want 2", got.SizeBytes)
	}
}

func TestCachedStoreHealthDoesNotHoldMutexDuringRefreshCompute(t *testing.T) {
	s := &Server{}
	canLockDuringCompute := make(chan bool, 1)
	s.storeHealthComputer = func(context.Context) *StatusStoreHealth {
		locked := make(chan struct{})
		go func() {
			s.storeHealthMu.Lock()
			defer s.storeHealthMu.Unlock()
			close(locked)
		}()
		select {
		case <-locked:
			canLockDuringCompute <- true
		case <-time.After(100 * time.Millisecond):
			canLockDuringCompute <- false
		}
		return &StatusStoreHealth{SizeBytes: 1}
	}

	_ = s.cachedStoreHealth(context.Background(), time.Unix(1_000_000, 0))
	if !<-canLockDuringCompute {
		t.Fatal("cachedStoreHealth held storeHealthMu while running the refresh computer")
	}
}

func TestStatusStoreHealthFromDomainOmitsEmptyLastGC(t *testing.T) {
	h := storehealth.Health{Path: "/c/.beads/dolt"}
	out := statusStoreHealthFromDomain(h)
	if out.LastGCAt != "" || out.LastGCStatus != "" {
		t.Fatalf("LastGC fields = (%q,%q), want empty", out.LastGCAt, out.LastGCStatus)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "last_gc_at") {
		t.Errorf("JSON contains last_gc_at when zero: %s", data)
	}
}

func TestStatusStoreHealthFromDomainFormatsLastGC(t *testing.T) {
	ts := time.Date(2026, 4, 1, 3, 15, 30, 0, time.UTC)
	h := storehealth.Health{
		Path:         "/c/.beads/dolt",
		LastGCAt:     ts,
		LastGCStatus: "failed",
	}
	out := statusStoreHealthFromDomain(h)
	if out.LastGCAt != "2026-04-01T03:15:30Z" {
		t.Errorf("LastGCAt = %q, want 2026-04-01T03:15:30Z", out.LastGCAt)
	}
	if out.LastGCStatus != "failed" {
		t.Errorf("LastGCStatus = %q, want failed", out.LastGCStatus)
	}
}

func TestComputeStoreHealthServerIntegration(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	for i := 0; i < 5; i++ {
		if _, err := store.Create(beads.Bead{Title: "x"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	ep := events.NewFake()
	ts := time.Date(2026, 4, 8, 0, 0, 0, 0, time.UTC)
	payload, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 1})
	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: ts, Payload: payload})

	state := &fakeState{
		cityPath:      cityPath,
		eventProv:     ep,
		cityBeadStore: store,
	}
	s := &Server{state: state}
	got := s.computeStoreHealth(context.Background())
	if got == nil {
		t.Fatal("computeStoreHealth returned nil")
	}
	if got.LiveRows != 5 {
		t.Errorf("LiveRows = %d, want 5", got.LiveRows)
	}
	if got.ThresholdMB != 1.0 {
		t.Errorf("ThresholdMB = %v, want 1.0", got.ThresholdMB)
	}
	if got.LastGCAt != "2026-04-08T00:00:00Z" {
		t.Errorf("LastGCAt = %q, want 2026-04-08T00:00:00Z", got.LastGCAt)
	}
}

func TestComputeStoreHealthUsesDoltlitePathFromMetadata(t *testing.T) {
	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	state := &fakeState{
		cityPath:      cityPath,
		eventProv:     events.NewFake(),
		cityBeadStore: beads.NewMemStore(),
	}
	s := &Server{state: state}
	got := s.computeStoreHealth(context.Background())
	if got == nil {
		t.Fatal("computeStoreHealth returned nil")
	}
	if !strings.HasSuffix(got.Path, "/.beads/doltlite") {
		t.Fatalf("Path = %q, want .beads/doltlite suffix", got.Path)
	}
}

func TestComputeStoreHealthEmptyCityPath(t *testing.T) {
	state := &fakeState{cityPath: ""}
	s := &Server{state: state}
	if got := s.computeStoreHealth(context.Background()); got != nil {
		t.Fatalf("computeStoreHealth = %+v, want nil for empty city path", got)
	}
}

func TestCountBeadStoreRowsNil(t *testing.T) {
	if got := countBeadStoreRows(context.Background(), newFakeState(t), nil); got != 0 {
		t.Fatalf("countBeadStoreRows(nil) = %d, want 0", got)
	}
}

func TestCountBeadStoreRowsIncludesClosedBeads(t *testing.T) {
	store := beads.NewMemStore()
	open, err := store.Create(beads.Bead{Title: "open"})
	if err != nil {
		t.Fatalf("Create open: %v", err)
	}
	closed, err := store.Create(beads.Bead{Title: "closed"})
	if err != nil {
		t.Fatalf("Create closed: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := countBeadStoreRows(context.Background(), newFakeState(t), store); got != 2 {
		t.Fatalf("countBeadStoreRows = %d, want 2 including closed bead %s and open bead %s", got, closed.ID, open.ID)
	}
}

func TestBuildStatusBodyIncludesStoreHealth(t *testing.T) {
	state := newFakeState(t)
	s := &Server{state: state}

	body := s.buildStatusBody(context.Background(), false)
	if body.StoreHealth == nil {
		t.Fatal("StoreHealth = nil, want populated")
	}
	if body.StoreHealth.ThresholdMB != 1.0 {
		t.Errorf("ThresholdMB = %v, want 1.0", body.StoreHealth.ThresholdMB)
	}
	if !strings.HasSuffix(body.StoreHealth.Path, "/.beads/dolt") {
		t.Errorf("Path = %q, want .beads/dolt suffix", body.StoreHealth.Path)
	}
}

func TestBuildStatusBodyIncludesBeadsDiagnostic(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadsDiag = &beads.BeadsDiagnostic{
		Store:               "BdStore",
		NativeStoreEligible: false,
		PreflightGate:       "metadata_backend",
		PreflightReason:     "metadata backend=file; native store requires dolt",
	}
	s := &Server{state: state}

	body := s.buildStatusBody(context.Background(), false)
	if body.Beads == nil {
		t.Fatal("Beads = nil, want diagnostic")
	}
	if body.Beads.Store != "BdStore" {
		t.Fatalf("beads_store = %q, want BdStore", body.Beads.Store)
	}
	if body.Beads.NativeStoreEligible {
		t.Fatal("native_store_eligible = true, want false")
	}
	if body.Beads.PreflightGate != "metadata_backend" {
		t.Fatalf("preflight_gate = %q, want metadata_backend", body.Beads.PreflightGate)
	}
	if body.Beads.PreflightReason == "" {
		t.Fatal("preflight_reason = empty, want fallback reason")
	}
}
