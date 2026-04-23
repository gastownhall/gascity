package api

import (
	"encoding/json"
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
	s.storeHealthComputer = func() *StatusStoreHealth {
		calls++
		return want
	}

	now := time.Unix(1_000_000, 0)
	got := s.cachedStoreHealth(now)
	if got != want {
		t.Fatalf("cachedStoreHealth = %+v, want %+v", got, want)
	}
	if calls != 1 {
		t.Fatalf("computer called %d times, want 1", calls)
	}

	// Within TTL: no recomputation.
	got2 := s.cachedStoreHealth(now.Add(storeHealthCacheTTL - time.Second))
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
	s.storeHealthComputer = func() *StatusStoreHealth {
		calls++
		return &StatusStoreHealth{SizeBytes: int64(calls)}
	}

	now := time.Unix(1_000_000, 0)
	_ = s.cachedStoreHealth(now)
	later := now.Add(storeHealthCacheTTL + time.Second)
	got := s.cachedStoreHealth(later)
	if calls != 2 {
		t.Fatalf("computer calls = %d, want 2", calls)
	}
	if got.SizeBytes != 2 {
		t.Fatalf("refreshed entry SizeBytes = %d, want 2", got.SizeBytes)
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
	got := s.computeStoreHealth()
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

func TestComputeStoreHealthEmptyCityPath(t *testing.T) {
	state := &fakeState{cityPath: ""}
	s := &Server{state: state}
	if got := s.computeStoreHealth(); got != nil {
		t.Fatalf("computeStoreHealth = %+v, want nil for empty city path", got)
	}
}

func TestCountBeadStoreRowsNil(t *testing.T) {
	if got := countBeadStoreRows(nil); got != 0 {
		t.Fatalf("countBeadStoreRows(nil) = %d, want 0", got)
	}
}

func TestBuildStatusBodyIncludesStoreHealth(t *testing.T) {
	state := newFakeState(t)
	s := &Server{state: state}

	body := s.buildStatusBody()
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
