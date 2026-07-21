package storehealth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// Compile-time checks: spyTailProvider must implement both interfaces.
var (
	_ events.Provider     = (*spyTailProvider)(nil)
	_ events.TailProvider = (*spyTailProvider)(nil)
)

func TestStorePath(t *testing.T) {
	got := StorePath("/tmp/citysvc")
	want := filepath.Join("/tmp/citysvc", ".beads", "dolt")
	if got != want {
		t.Fatalf("StorePath = %q, want %q", got, want)
	}
}

func TestStorePath_DoltliteMetadata(t *testing.T) {
	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := StorePath(cityPath)
	want := filepath.Join(cityPath, ".beads", "doltlite")
	if got != want {
		t.Fatalf("StorePath = %q, want %q", got, want)
	}
}

func TestComputeWarningHighRatio(t *testing.T) {
	// 11.2 GB (decimal) / 221 rows = ~50.68 MB/row, warning.
	const size = 11_200_000_000
	h := Compute("/c", size, 221, true, time.Time{}, "")
	if !h.Warning {
		t.Fatalf("Warning = false, want true for size=%d rows=221", size)
	}
	if h.RatioMB < 50 || h.RatioMB > 51 {
		t.Fatalf("RatioMB = %v, want ~50.7", h.RatioMB)
	}
	if h.ThresholdMB != DefaultThresholdMB {
		t.Fatalf("ThresholdMB = %v, want %v", h.ThresholdMB, DefaultThresholdMB)
	}
	if h.Path != "/c/.beads/dolt" {
		t.Fatalf("Path = %q, want /c/.beads/dolt", h.Path)
	}
}

func TestComputeNoWarningLowRatio(t *testing.T) {
	// 50 MB / 221 rows = ~0.23 MB/row, no warning.
	const size = 50_000_000
	h := Compute("/c", size, 221, true, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true, want false for size=%d rows=221", size)
	}
	if h.RatioMB > 0.5 {
		t.Fatalf("RatioMB = %v, want < 0.5", h.RatioMB)
	}
}

func TestComputeZeroRetainedRowsDoesNotWarnForBookkeepingBytes(t *testing.T) {
	// The denominator is retained rows (open and closed). A genuinely empty
	// store can still contain bookkeeping files, which alone are not unhealthy.
	h := Compute("/c", 1, 0, true, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true, want false for bookkeeping bytes with zero retained rows")
	}
}

func TestComputeZeroEverything(t *testing.T) {
	h := Compute("/c", 0, 0, true, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true, want false for all-zero inputs")
	}
}

// TestComputeUnmeasuredRowsNeverWarns: a row count that failed or
// timed out must never be treated as a real zero. Even with a large
// sizeBytes that would trip the ratio warning if 0 retained rows were real,
// rowsMeasured=false must suppress the warning entirely — there is nothing
// to compute a ratio against.
func TestComputeUnmeasuredRowsNeverWarns(t *testing.T) {
	const size = 11_200_000_000 // would warn at 221 real rows (see TestComputeWarningHighRatio)
	h := Compute("/c", size, 0, false, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true, want false when rows are unmeasured (RowsMeasured=false)")
	}
	if h.RatioMB != 0 {
		t.Fatalf("RatioMB = %v, want 0 when rows are unmeasured", h.RatioMB)
	}
	if h.RowsMeasured {
		t.Fatalf("RowsMeasured = true, want false")
	}
}

// TestComputeUnmeasuredIsDistinguishableFromRealZero pins the actual
// deliverable: two Health values with identical LiveRows=0 but different
// RowsMeasured must be distinguishable by callers, so a failed measurement
// can never render byte-identically to a genuinely empty, healthy store.
func TestComputeUnmeasuredIsDistinguishableFromRealZero(t *testing.T) {
	measured := Compute("/c", 1, 0, true, time.Time{}, "")
	unmeasured := Compute("/c", 1, 0, false, time.Time{}, "")
	if measured.RowsMeasured == unmeasured.RowsMeasured {
		t.Fatalf("RowsMeasured did not distinguish a real zero-row count from an unmeasured one")
	}
	if !measured.RowsMeasured {
		t.Fatalf("measured.RowsMeasured = false, want true")
	}
	if unmeasured.RowsMeasured {
		t.Fatalf("unmeasured.RowsMeasured = true, want false")
	}
}

func TestComputeBoundary(t *testing.T) {
	// Exactly at the threshold: size = 1M * rows should NOT warn
	// (the inequality is strict ">", not ">=").
	// rows is large enough that the ratio threshold size clears
	// MinWarnSizeBytes, so this exercises the ratio boundary alone,
	// not the absolute-size floor (see TestComputeSmallStoreFloor).
	const rows = 2000
	h := Compute("/c", int64(DefaultThresholdMB*bytesPerMB)*int64(rows), rows, true, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true at exact threshold, want false")
	}
	h = Compute("/c", int64(DefaultThresholdMB*bytesPerMB)*int64(rows)+1, rows, true, time.Time{}, "")
	if !h.Warning {
		t.Fatalf("Warning = false one byte over threshold, want true")
	}
}

// TestComputeSmallStoreFloorSuppressesFalsePositive is the regression for
// #3374: a young/small city with only a handful of live rows still carries
// Dolt's own baseline footprint (oldgen archives, system tables) well into
// the hundreds of MB, which permanently trips a pure MB-per-row ratio with
// nothing for maintenance to reclaim — gc dolt compact's own commit-count
// gate correctly finds nothing to do, but the warning could never clear.
// Reproduces the reported numbers exactly: 343 MB at 7 live rows (~49
// MB/row, far above the 1.0 MB/row ratio threshold) must not warn, since
// the total size is still well under the absolute floor.
func TestComputeSmallStoreFloorSuppressesFalsePositive(t *testing.T) {
	const size = 343_000_000
	h := Compute("/c", size, 7, true, time.Time{}, "")
	if h.Warning {
		t.Fatalf("Warning = true, want false (343MB/7 rows is below the absolute floor despite a high ratio)")
	}
	if h.RatioMB < 48 || h.RatioMB > 50 {
		t.Fatalf("RatioMB = %v, want ~49 (the ratio itself is still reported for diagnostics)", h.RatioMB)
	}
}

// TestComputeLargeStoreStillWarnsAboveFloor guards the fix's scope: the
// floor only suppresses the false positive on genuinely small stores — the
// real pathology the ratio check exists to catch (production case: ~11GB
// at ~64 rows) must still warn once both the ratio AND the absolute floor
// are exceeded.
func TestComputeLargeStoreStillWarnsAboveFloor(t *testing.T) {
	const size = 11_200_000_000
	h := Compute("/c", size, 221, true, time.Time{}, "")
	if !h.Warning {
		t.Fatalf("Warning = false, want true (11.2GB/221 rows is well above both the ratio threshold and the absolute floor)")
	}
}

func TestComputeCarriesLastGC(t *testing.T) {
	ts := time.Date(2026, 4, 1, 3, 0, 0, 0, time.UTC)
	h := Compute("/c", 1, 1, true, ts, "success")
	if !h.LastGCAt.Equal(ts) {
		t.Fatalf("LastGCAt = %v, want %v", h.LastGCAt, ts)
	}
	if h.LastGCStatus != "success" {
		t.Fatalf("LastGCStatus = %q, want success", h.LastGCStatus)
	}
}

func TestWalkSizeMissingPath(t *testing.T) {
	got := WalkSize(filepath.Join(t.TempDir(), "nonexistent"))
	if got != 0 {
		t.Fatalf("WalkSize(missing) = %d, want 0", got)
	}
}

func TestWalkSizeSumsFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel string, size int) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	mustWrite("a.bin", 100)
	mustWrite("sub/b.bin", 250)
	mustWrite("sub/deeper/c.bin", 17)
	got := WalkSize(dir)
	if got != 367 {
		t.Fatalf("WalkSize = %d, want 367", got)
	}
}

func TestLastMaintenanceNilProvider(t *testing.T) {
	ts, status := LastMaintenance(nil)
	if !ts.IsZero() || status != "" {
		t.Fatalf("LastMaintenance(nil) = (%v,%q), want (zero,\"\")", ts, status)
	}
}

func TestLastMaintenanceReturnsLatestAcrossTypes(t *testing.T) {
	ep := events.NewFake()
	older := time.Date(2026, 4, 1, 3, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 4, 8, 3, 0, 0, 0, time.UTC)

	payloadDone, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 1})
	payloadFail, _ := json.Marshal(events.StoreMaintenanceFailedPayload{Stage: "gc"})

	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: older, Payload: payloadDone})
	ep.Record(events.Event{Type: events.StoreMaintenanceFailed, Ts: newer, Payload: payloadFail})

	ts, status := LastMaintenance(ep)
	if !ts.Equal(newer) {
		t.Fatalf("ts = %v, want %v", ts, newer)
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
}

func TestLastMaintenanceOnlyDoneEvents(t *testing.T) {
	ep := events.NewFake()
	t1 := time.Date(2026, 4, 1, 3, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 4, 8, 3, 0, 0, 0, time.UTC)
	payload, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 2})
	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: t1, Payload: payload})
	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: t2, Payload: payload})

	ts, status := LastMaintenance(ep)
	if !ts.Equal(t2) {
		t.Fatalf("ts = %v, want %v", ts, t2)
	}
	if status != "success" {
		t.Fatalf("status = %q, want success", status)
	}
}

func TestLastMaintenanceNoEvents(t *testing.T) {
	ep := events.NewFake()
	ts, status := LastMaintenance(ep)
	if !ts.IsZero() || status != "" {
		t.Fatalf("LastMaintenance(empty) = (%v,%q), want (zero,\"\")", ts, status)
	}
}

// tailCallRecord captures a single ListTail invocation for inspection.
type tailCallRecord struct {
	filter events.Filter
	limit  int
}

// spyTailProvider is a Provider+TailProvider that records every ListTail call
// and delegates to an inner Fake for actual event results.
type spyTailProvider struct {
	inner *events.Fake
	calls []tailCallRecord
}

func (s *spyTailProvider) Record(e events.Event)                        { s.inner.Record(e) }
func (s *spyTailProvider) List(f events.Filter) ([]events.Event, error) { return s.inner.List(f) }

func (s *spyTailProvider) ListTail(f events.Filter, limit int) ([]events.Event, error) {
	s.calls = append(s.calls, tailCallRecord{filter: f, limit: limit})
	return s.inner.ListTail(f, limit)
}

func (s *spyTailProvider) LatestSeq() (uint64, error) { return s.inner.LatestSeq() }
func (s *spyTailProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	return s.inner.Watch(ctx, afterSeq)
}
func (s *spyTailProvider) Close() error { return s.inner.Close() }

// TestLastMaintenanceUsesWindowedTailScan verifies that LastMaintenance
// delegates to ListTail (not List) with the correct WindowBytes bound.
// Without the hasTail branch, List would be called instead and this test
// fails. Without the WindowBytes value, the bound is silently absent.
func TestLastMaintenanceUsesWindowedTailScan(t *testing.T) {
	spy := &spyTailProvider{inner: events.NewFake()}
	LastMaintenance(spy)

	if len(spy.calls) != 2 {
		t.Fatalf("expected 2 ListTail calls (one per maintenance type), got %d", len(spy.calls))
	}
	for i, call := range spy.calls {
		if call.filter.WindowBytes != lastMaintenanceScanWindow {
			t.Errorf("call[%d]: WindowBytes=%d, want %d", i, call.filter.WindowBytes, lastMaintenanceScanWindow)
		}
		if call.limit != 1 {
			t.Errorf("call[%d]: limit=%d, want 1", i, call.limit)
		}
	}
}

// TestLastMaintenanceBoundedScan_EventBeforeWindow verifies end-to-end that
// when the bounded tail scan is used, maintenance events placed before the
// scan window are NOT returned — they are treated as unknown.
//
// Failing case without the fix: ReadFilteredTail walks to byte 0, finds the
// maintenance event, and LastMaintenance returns a non-zero timestamp.
// With the fix: the walk stops at the window floor and returns (zero, "").
func TestLastMaintenanceBoundedScan_EventBeforeWindow(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")

	// Write a maintenance-done event at the very start of the file.
	maintenanceEvent := events.Event{
		Seq:  1,
		Ts:   time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		Type: events.StoreMaintenanceDone,
	}
	payload, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 10})
	maintenanceEvent.Payload = payload
	lineBytes, err := json.Marshal(maintenanceEvent)
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(append(lineBytes, '\n')) //nolint:errcheck

	// Fill with noise events until the file exceeds lastMaintenanceScanWindow,
	// pushing the maintenance event before the window floor.
	noiseBase, _ := json.Marshal(events.Event{Seq: 2, Ts: time.Date(2026, 4, 1, 12, 0, 1, 0, time.UTC), Type: "gc.session.started"})
	noiseBase = append(noiseBase, '\n')
	noiseLine := noiseBase
	written := int64(len(lineBytes) + 1)
	for written <= lastMaintenanceScanWindow+int64(len(noiseLine))*10 {
		f.Write(noiseLine) //nolint:errcheck
		written += int64(len(noiseLine))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ep, err := events.NewFileRecorder(eventsPath, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close() //nolint:errcheck

	ts, status := LastMaintenance(ep)
	if !ts.IsZero() || status != "" {
		t.Errorf("LastMaintenance returned (%v, %q): maintenance event before window should be treated as unknown", ts, status)
	}
}
