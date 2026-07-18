package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storehealth"
)

type storeHealthListErrorStore struct {
	beads.Store
	err error
}

func (s *storeHealthListErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.AllowScan && query.IncludeClosed {
		return nil, s.err
	}
	return s.Store.List(query)
}

func TestCachedStoreHealthServesMemoized(t *testing.T) {
	var calls int
	want := &StatusStoreHealth{Path: "/c/.beads/dolt", SizeBytes: 123}
	s := &Server{}
	s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
		calls++
		return want, nil
	}

	now := time.Unix(1_000_000, 0)
	got, err := s.cachedStoreHealth(context.Background(), now)
	if err != nil {
		t.Fatalf("cachedStoreHealth: %v", err)
	}
	if got != want {
		t.Fatalf("cachedStoreHealth = %+v, want %+v", got, want)
	}
	if calls != 1 {
		t.Fatalf("computer called %d times, want 1", calls)
	}

	// Within TTL: no recomputation.
	got2, err := s.cachedStoreHealth(context.Background(), now.Add(storeHealthCacheTTL-time.Second))
	if err != nil {
		t.Fatalf("second cachedStoreHealth: %v", err)
	}
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
	s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
		calls++
		return &StatusStoreHealth{SizeBytes: int64(calls)}, nil
	}

	now := time.Unix(1_000_000, 0)
	if _, err := s.cachedStoreHealth(context.Background(), now); err != nil {
		t.Fatalf("initial cachedStoreHealth: %v", err)
	}
	later := now.Add(storeHealthCacheTTL + time.Second)
	got, err := s.cachedStoreHealth(context.Background(), later)
	if err != nil {
		t.Fatalf("refreshed cachedStoreHealth: %v", err)
	}
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
	s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
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
		return &StatusStoreHealth{SizeBytes: 1}, nil
	}

	if _, err := s.cachedStoreHealth(context.Background(), time.Unix(1_000_000, 0)); err != nil {
		t.Fatalf("cachedStoreHealth: %v", err)
	}
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
	got, err := s.computeStoreHealth(context.Background())
	if err != nil {
		t.Fatalf("computeStoreHealth: %v", err)
	}
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
	got, err := s.computeStoreHealth(context.Background())
	if err != nil {
		t.Fatalf("computeStoreHealth: %v", err)
	}
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
	got, err := s.computeStoreHealth(context.Background())
	if err != nil {
		t.Fatalf("computeStoreHealth: %v", err)
	}
	if got != nil {
		t.Fatalf("computeStoreHealth = %+v, want nil for empty city path", got)
	}
}

func TestCountBeadStoreRowsReturnsUnavailableForNilStore(t *testing.T) {
	got, err := countBeadStoreRows(context.Background(), newFakeState(t), nil)
	if got != 0 {
		t.Errorf("countBeadStoreRows(nil) = %d, want zero value when unavailable", got)
	}
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("countBeadStoreRows(nil) error = %v, want unavailable error", err)
	}
}

func TestCountBeadStoreRowsReturnsScanError(t *testing.T) {
	wantErr := errors.New("store health row scan failed")
	store := &storeHealthListErrorStore{Store: beads.NewMemStore(), err: wantErr}

	got, err := countBeadStoreRows(context.Background(), newFakeState(t), store)
	if got != 0 {
		t.Errorf("countBeadStoreRows rows = %d, want zero value when unavailable", got)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("countBeadStoreRows error = %v, want %v", err, wantErr)
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
	got, err := countBeadStoreRows(context.Background(), newFakeState(t), store)
	if err != nil {
		t.Fatalf("countBeadStoreRows: %v", err)
	}
	if got != 2 {
		t.Fatalf("countBeadStoreRows = %d, want 2 including closed bead %s and open bead %s", got, closed.ID, open.ID)
	}
}

func TestComputeStoreHealthReturnsRowCountError(t *testing.T) {
	wantErr := errors.New("store health row scan failed")
	state := newFakeState(t)
	state.cityBeadStore = &storeHealthListErrorStore{Store: beads.NewMemStore(), err: wantErr}
	s := &Server{state: state}

	got, err := s.computeStoreHealth(context.Background())
	if got != nil {
		t.Errorf("computeStoreHealth = %+v, want nil when row count is unavailable", got)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("computeStoreHealth error = %v, want %v", err, wantErr)
	}
}

func TestBuildStatusBodyIncludesStoreHealth(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
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

func TestBuildStatusBodyOmitsUnavailableStoreHealthAndReportsPartialError(t *testing.T) {
	wantErr := errors.New("store health row scan failed")
	state := newFakeState(t)
	s := &Server{state: state}
	s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
		return nil, wantErr
	}

	body := s.buildStatusBody(context.Background(), false)
	if body.StoreHealth != nil {
		t.Errorf("StoreHealth = %+v, want omitted when unavailable", body.StoreHealth)
	}
	if !body.Partial {
		t.Error("Partial = false, want true when store health is unavailable")
	}
	wantPartialError := "store health: " + wantErr.Error()
	if len(body.PartialErrors) != 1 || body.PartialErrors[0] != wantPartialError {
		t.Fatalf("PartialErrors = %q, want [%q]", body.PartialErrors, wantPartialError)
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
