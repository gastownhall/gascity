package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	synctest.Test(t, func(t *testing.T) {
		var calls int
		want := &StatusStoreHealth{Path: "/c/.beads/dolt", SizeBytes: 123}
		s := &Server{}
		s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
			calls++
			return want, nil
		}

		got, err := s.cachedStoreHealth(context.Background(), time.Now())
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
		<-time.After(storeHealthCacheTTL - time.Second)
		got2, err := s.cachedStoreHealth(context.Background(), time.Now())
		if err != nil {
			t.Fatalf("second cachedStoreHealth: %v", err)
		}
		if got2 != want {
			t.Fatalf("second cachedStoreHealth = %+v, want %+v", got2, want)
		}
		if calls != 1 {
			t.Fatalf("computer called %d times within TTL, want 1", calls)
		}
	})
}

func TestCachedStoreHealthRefreshesAfterTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		s := &Server{}
		s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
			calls++
			return &StatusStoreHealth{SizeBytes: int64(calls)}, nil
		}

		if _, err := s.cachedStoreHealth(context.Background(), time.Now()); err != nil {
			t.Fatalf("initial cachedStoreHealth: %v", err)
		}
		<-time.After(storeHealthCacheTTL + time.Second)
		got, err := s.cachedStoreHealth(context.Background(), time.Now())
		if err != nil {
			t.Fatalf("refreshed cachedStoreHealth: %v", err)
		}
		if calls != 2 {
			t.Fatalf("computer calls = %d, want 2", calls)
		}
		if got.SizeBytes != 2 {
			t.Fatalf("refreshed entry SizeBytes = %d, want 2", got.SizeBytes)
		}
	})
}

func TestCachedStoreHealthConcurrentColdMissesCoalesce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const callers = 8

		want := &StatusStoreHealth{Path: "/c/.beads/dolt", SizeBytes: 123}
		releaseCompute := make(chan struct{})
		results := make(chan *StatusStoreHealth, callers)
		var calls atomic.Int32

		s := &Server{}
		s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
			calls.Add(1)
			<-releaseCompute
			return want, nil
		}

		for range callers {
			go func() {
				got, err := s.cachedStoreHealth(context.Background(), time.Now())
				if err != nil {
					t.Errorf("cachedStoreHealth: %v", err)
				}
				results <- got
			}()
		}

		// Every caller is now either the elected computer or waiting for that
		// same in-flight result. No wall-clock sleep is needed to prove overlap.
		synctest.Wait()
		computeCalls := calls.Load()

		close(releaseCompute)
		synctest.Wait()

		for i := range callers {
			if got := <-results; got != want {
				t.Errorf("caller %d got cachedStoreHealth = %p, want shared result %p", i, got, want)
			}
		}
		if computeCalls != 1 {
			t.Errorf("computer calls while %d cold misses overlapped = %d, want 1", callers, computeCalls)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("final computer calls after %d cold misses completed = %d, want 1", callers, got)
		}
	})
}

func TestCachedStoreHealthConcurrentExpiredMissesCoalesce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const callers = 8

		stale := &StatusStoreHealth{SizeBytes: 1}
		fresh := &StatusStoreHealth{SizeBytes: 2}
		releaseRefresh := make(chan struct{})
		results := make(chan *StatusStoreHealth, callers)
		var calls atomic.Int32

		s := &Server{}
		s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
			if calls.Add(1) == 1 {
				return stale, nil
			}
			<-releaseRefresh
			return fresh, nil
		}

		primed, err := s.cachedStoreHealth(context.Background(), time.Now())
		if err != nil {
			t.Fatalf("primed cachedStoreHealth: %v", err)
		}
		if primed != stale {
			t.Fatalf("primed cachedStoreHealth = %p, want stale entry %p", primed, stale)
		}
		<-time.After(storeHealthCacheTTL)

		for range callers {
			go func() {
				got, err := s.cachedStoreHealth(context.Background(), time.Now())
				if err != nil {
					t.Errorf("cachedStoreHealth: %v", err)
				}
				results <- got
			}()
		}

		synctest.Wait()
		computeCalls := calls.Load()

		close(releaseRefresh)
		synctest.Wait()

		for i := range callers {
			if got := <-results; got != fresh {
				t.Errorf("caller %d got cachedStoreHealth = %p, want refreshed result %p", i, got, fresh)
			}
		}
		if computeCalls != 2 {
			t.Errorf("computer calls across prime plus %d expired misses = %d, want 2", callers, computeCalls)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("final computer calls after %d expired misses completed = %d, want 2", callers, got)
		}
	})
}

func TestCachedStoreHealthRefreshSurvivesLeaderCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		want := &StatusStoreHealth{SizeBytes: 123}
		canceledResult := &StatusStoreHealth{SizeBytes: -1}
		computeStarted := make(chan struct{})
		releaseCompute := make(chan struct{})
		results := make(chan *StatusStoreHealth, 2)
		var calls atomic.Int32

		s := &Server{}
		s.storeHealthComputer = func(ctx context.Context) (*StatusStoreHealth, error) {
			calls.Add(1)
			close(computeStarted)
			<-releaseCompute
			if ctx.Err() != nil {
				return canceledResult, nil
			}
			return want, nil
		}

		leaderCtx, cancelLeader := context.WithCancel(context.Background())
		go func() {
			got, err := s.cachedStoreHealth(leaderCtx, time.Now())
			if err != nil {
				t.Errorf("cachedStoreHealth: %v", err)
			}
			results <- got
		}()
		<-computeStarted
		cancelLeader()

		go func() {
			got, err := s.cachedStoreHealth(context.Background(), time.Now())
			if err != nil {
				t.Errorf("cachedStoreHealth: %v", err)
			}
			results <- got
		}()
		synctest.Wait()

		close(releaseCompute)
		synctest.Wait()

		for i := range 2 {
			if got := <-results; got != want {
				t.Errorf("caller %d got cachedStoreHealth = %p, want request-independent result %p", i, got, want)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("computer calls with canceled leader and live waiter = %d, want 1", got)
		}
	})
}

func TestCachedStoreHealthTTLStartsAfterComputeCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		want := &StatusStoreHealth{Path: "/c/.beads/dolt", SizeBytes: 123}
		var calls atomic.Int32

		s := &Server{}
		s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
			calls.Add(1)
			// Advance virtual time past the TTL while the refresh is running.
			<-time.After(storeHealthCacheTTL + time.Second)
			return want, nil
		}

		first, err := s.cachedStoreHealth(context.Background(), time.Now())
		if err != nil {
			t.Fatalf("first cachedStoreHealth: %v", err)
		}
		second, err := s.cachedStoreHealth(context.Background(), time.Now())
		if err != nil {
			t.Fatalf("second cachedStoreHealth: %v", err)
		}

		if first != want || second != want {
			t.Fatalf("cached results = (%p, %p), want (%p, %p)", first, second, want, want)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("computer calls across immediate post-compute read = %d, want 1", got)
		}
	})
}

func TestCachedStoreHealthDoesNotHoldMutexDuringRefreshCompute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Server{}
		s.storeHealthComputer = func(context.Context) (*StatusStoreHealth, error) {
			locked := make(chan struct{})
			go func() {
				s.storeHealthMu.Lock()
				defer s.storeHealthMu.Unlock()
				close(locked)
			}()
			synctest.Wait()
			select {
			case <-locked:
			default:
				t.Error("cachedStoreHealth held storeHealthMu while running the refresh computer")
			}
			return &StatusStoreHealth{SizeBytes: 1}, nil
		}

		if _, err := s.cachedStoreHealth(context.Background(), time.Now()); err != nil {
			t.Fatalf("cachedStoreHealth: %v", err)
		}
	})
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

type storeHealthCounterStore struct {
	beads.Store
	count     int
	countErr  error
	gotQuery  *beads.ListQuery
	listCalls int
}

func (s *storeHealthCounterStore) Count(_ context.Context, query beads.ListQuery, _ ...string) (int, error) {
	s.gotQuery = &query
	return s.count, s.countErr
}

func (s *storeHealthCounterStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.Store.List(query)
}

func TestCountBeadStoreRowsPrefersCounterWithoutHydration(t *testing.T) {
	store := &storeHealthCounterStore{Store: beads.NewMemStore(), count: 41252}

	got, err := countBeadStoreRows(context.Background(), newFakeState(t), store)
	if err != nil {
		t.Fatalf("countBeadStoreRows: %v", err)
	}
	if got != 41252 {
		t.Fatalf("countBeadStoreRows = %d, want Counter result 41252", got)
	}
	if store.listCalls != 0 {
		t.Fatalf("List called %d times, want 0 (Counter path must not hydrate)", store.listCalls)
	}
	if store.gotQuery == nil || !store.gotQuery.IncludeClosed || !store.gotQuery.AllowScan {
		t.Fatalf("Count query = %+v, want AllowScan and IncludeClosed set", store.gotQuery)
	}
}

func TestCountBeadStoreRowsFallsBackWhenCountUnsupported(t *testing.T) {
	store := &storeHealthCounterStore{
		Store:    beads.NewMemStore(),
		countErr: fmt.Errorf("counting beads: %w", beads.ErrCountUnsupported),
	}
	if _, err := store.Create(beads.Bead{Title: "x"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := countBeadStoreRows(context.Background(), newFakeState(t), store)
	if err != nil {
		t.Fatalf("countBeadStoreRows: %v", err)
	}
	if got != 1 {
		t.Fatalf("countBeadStoreRows = %d, want 1 from List fallback", got)
	}
	if store.listCalls == 0 {
		t.Fatal("List never called, want hydrating fallback on ErrCountUnsupported")
	}
}

func TestCountBeadStoreRowsReturnsCounterError(t *testing.T) {
	wantErr := errors.New("store health count failed")
	store := &storeHealthCounterStore{Store: beads.NewMemStore(), countErr: wantErr}

	got, err := countBeadStoreRows(context.Background(), newFakeState(t), store)
	if got != 0 {
		t.Errorf("countBeadStoreRows = %d, want zero value on Counter error", got)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("countBeadStoreRows error = %v, want %v", err, wantErr)
	}
	if store.listCalls != 0 {
		t.Fatalf("List called %d times after non-unsupported Counter error, want 0", store.listCalls)
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
