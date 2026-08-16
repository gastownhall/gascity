package storehealth

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
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

// recordingProvider wraps a Fake and counts List vs ListTail calls, so tests
// can assert which path LastMaintenance actually took rather than only
// inferring it from the result.
type recordingProvider struct {
	*events.Fake
	listCalls     int
	listTailCalls int
}

func (r *recordingProvider) List(filter events.Filter) ([]events.Event, error) {
	r.listCalls++
	return r.Fake.List(filter)
}

func (r *recordingProvider) ListTail(filter events.Filter, limit int) ([]events.Event, error) {
	r.listTailCalls++
	return r.Fake.ListTail(filter, limit)
}

// providerWithoutTail implements events.Provider but deliberately omits
// ListTail, so LastMaintenance must fall back to the unbounded List path
// rather than a type assertion panicking or silently returning nothing.
type providerWithoutTail struct {
	*events.Fake
}

func (p *providerWithoutTail) List(filter events.Filter) ([]events.Event, error) {
	return p.Fake.List(filter)
}

// TestLastMaintenanceUsesTailProviderFastPath is the regression for #4418:
// when the provider implements events.TailProvider, LastMaintenance must
// call ListTail (the bounded backward scan) instead of the unbounded List,
// which on a large event log with a rare Type filter costs a full-file scan
// for what is ultimately a cosmetic status field.
func TestLastMaintenanceUsesTailProviderFastPath(t *testing.T) {
	rp := &recordingProvider{Fake: events.NewFake()}
	payload, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 3})
	ts := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	rp.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: ts, Payload: payload})

	gotTs, gotStatus := LastMaintenance(rp)
	if !gotTs.Equal(ts) || gotStatus != "success" {
		t.Fatalf("LastMaintenance = (%v,%q), want (%v,success)", gotTs, gotStatus, ts)
	}
	if rp.listTailCalls == 0 {
		t.Fatalf("listTailCalls = 0, want > 0 (LastMaintenance should prefer the TailProvider fast path)")
	}
	if rp.listCalls != 0 {
		t.Fatalf("listCalls = %d, want 0 (fast path should not also fall back to List)", rp.listCalls)
	}
}

// TestLastMaintenanceFallsBackWithoutTailProvider guards the fallback: a
// provider that does not implement events.TailProvider (e.g. an exec-script
// provider) must still get a correct answer via the existing List path.
func TestLastMaintenanceFallsBackWithoutTailProvider(t *testing.T) {
	pwt := &providerWithoutTail{Fake: events.NewFake()}
	payload, _ := json.Marshal(events.StoreMaintenanceFailedPayload{Stage: "gc"})
	ts := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	pwt.Record(events.Event{Type: events.StoreMaintenanceFailed, Ts: ts, Payload: payload})

	gotTs, gotStatus := LastMaintenance(pwt)
	if !gotTs.Equal(ts) || gotStatus != "failed" {
		t.Fatalf("LastMaintenance = (%v,%q), want (%v,failed)", gotTs, gotStatus, ts)
	}
}

// writeArchivedEvents writes evts as a gzipped canonical events archive
// beside path, using the rotation naming convention
// (events.jsonl.archive-<ts>-seq-<first>-<last>.gz) that the archive-aware
// read path discovers by directory listing.
func writeArchivedEvents(t *testing.T, path string, evts []events.Event) {
	t.Helper()
	if len(evts) == 0 {
		t.Fatal("writeArchivedEvents: no events")
	}
	var raw bytes.Buffer
	for _, e := range evts {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(line)
		raw.WriteString("\n")
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz",
		evts[0].Ts.UTC().Format("20060102T150405Z"), evts[0].Seq, evts[len(evts)-1].Seq)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLastMaintenanceDoesNotReadRotatedArchives pins accepted, documented
// behavior rather than a bug: FileRecorder.ListTail scans the active
// events.jsonl only, so a maintenance event that has aged into a rotated
// .gz archive is reported as absent. The old unbounded List path did read
// archives; taking the archive-aware fall-through here (as
// fetchEventPageAscending does) would restore the full scan #4418 removed,
// and LastGCAt is display-only in every consumer. If this test starts
// failing because LastMaintenance grew a fall-through, that is a
// deliberate re-trade — update the comment on
// lastMaintenanceScanWindowBytes with it, do not just delete the test.
func TestLastMaintenanceDoesNotReadRotatedArchives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ts := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 3})
	if err != nil {
		t.Fatal(err)
	}

	// The only maintenance event lives in the archive; the active file
	// holds unrelated traffic, as it would after a rotation.
	writeArchivedEvents(t, path, []events.Event{
		{Seq: 1, Type: events.StoreMaintenanceDone, Ts: ts, Payload: payload},
	})

	rec, err := events.NewFileRecorder(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup
	rec.Record(events.Event{Type: "unrelated.event", Ts: ts.Add(time.Hour)})

	// Control: the archive-aware List path DOES see it, so an empty result
	// below is archive-blindness and not a broken fixture.
	viaList, err := rec.List(events.Filter{Type: events.StoreMaintenanceDone})
	if err != nil {
		t.Fatal(err)
	}
	if len(viaList) != 1 {
		t.Fatalf("List got %d events, want 1 (fixture must be readable via the archive-aware path)", len(viaList))
	}

	gotTs, gotStatus := LastMaintenance(rec)
	if !gotTs.IsZero() || gotStatus != "" {
		t.Fatalf("LastMaintenance = (%v,%q), want (zero,\"\") — the tail fast path reads the active file only", gotTs, gotStatus)
	}
}
