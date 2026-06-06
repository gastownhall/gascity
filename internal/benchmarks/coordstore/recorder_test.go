package coordstore

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTimeSeriesRecorderRecordOnceWritesCompleteSample(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "store")
	if err := os.MkdirAll(filepath.Join(dataDir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "a.db"), []byte("abc"), 0o644); err != nil {
		t.Fatalf("write data file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "nested", "b.db"), []byte("defg"), 0o644); err != nil {
		t.Fatalf("write nested data file: %v", err)
	}

	hist := &Histogram{}
	hist.Add(10 * time.Millisecond)
	hist.Add(20 * time.Millisecond)
	recorder := NewTimeSeriesRecorder(TimeSeriesRecorderConfig{
		Path:           filepath.Join(dir, "timeseries.jsonl"),
		DataDir:        dataDir,
		SampleInterval: time.Millisecond,
		Adapter:        statsOnlyAdapter{stats: map[string]int64{"backend_records": 7}},
		HistogramSnapshot: func() map[string]*OperationResult {
			return map[string]*OperationResult{
				"Get": {Op: "Get", Samples: 2, Errors: 1, H: hist},
			}
		},
	})

	if err := recorder.RecordOnce(ctx); err != nil {
		t.Fatalf("RecordOnce: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "timeseries.jsonl"))
	if err != nil {
		t.Fatalf("read timeseries: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("line count = %d, want 1: %q", len(lines), data)
	}
	var sample TelemetrySample
	if err := json.Unmarshal([]byte(lines[0]), &sample); err != nil {
		t.Fatalf("decode sample: %v", err)
	}
	if sample.Timestamp.IsZero() {
		t.Fatalf("timestamp was not recorded")
	}
	if sample.HeapAllocBytes == 0 || sample.HeapInuseBytes == 0 {
		t.Fatalf("heap metrics missing: %#v", sample)
	}
	if sample.StoreSizeBytes != 7 {
		t.Fatalf("StoreSizeBytes = %d, want 7", sample.StoreSizeBytes)
	}
	if sample.AdapterStats["backend_records"] != 7 {
		t.Fatalf("AdapterStats = %#v", sample.AdapterStats)
	}
	gotOp := sample.Operations["Get"]
	if gotOp.Samples != 2 || gotOp.Errors != 1 || gotOp.P50Nanos == 0 || gotOp.P99Nanos == 0 || gotOp.MaxNanos == 0 {
		t.Fatalf("operation snapshot = %#v", gotOp)
	}
}

type statsOnlyAdapter struct {
	StoreAdapter
	stats map[string]int64
}

func (a statsOnlyAdapter) Stats(context.Context) map[string]int64 {
	return a.stats
}

func TestRecorderSampleIncludesHeapInuseDeltaBytes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ts.jsonl")

	recorder := NewTimeSeriesRecorder(TimeSeriesRecorderConfig{
		Path: path,
	})
	if err := recorder.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Force a second sample so delta has a chance to appear in the record.
	if err := recorder.RecordOnce(ctx); err != nil {
		t.Fatalf("RecordOnce: %v", err)
	}
	if err := recorder.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := raw["heap_inuse_delta_bytes"]; !ok {
			t.Fatalf("heap_inuse_delta_bytes absent from JSONL sample: %s", line)
		}
	}
}

func TestRecorderSampleLiveObjectCountPresentWhenAdapterReports(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ts.jsonl")

	recorder := NewTimeSeriesRecorder(TimeSeriesRecorderConfig{
		Path:    path,
		Adapter: statsOnlyAdapter{stats: map[string]int64{"live_objects": 42, "other": 9}},
	})
	if err := recorder.RecordOnce(ctx); err != nil {
		t.Fatalf("RecordOnce: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var sample TelemetrySample
	if err := json.Unmarshal(bytes.TrimSpace(data), &sample); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sample.LiveObjectCount != 42 {
		t.Errorf("LiveObjectCount = %d, want 42", sample.LiveObjectCount)
	}
	if sample.AdapterStats["other"] != 9 {
		t.Errorf("AdapterStats[other] = %d, want 9", sample.AdapterStats["other"])
	}
}

func TestRecorderSampleLiveObjectCountAbsentWhenAdapterDoesNotReport(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ts.jsonl")

	recorder := NewTimeSeriesRecorder(TimeSeriesRecorderConfig{
		Path:    path,
		Adapter: statsOnlyAdapter{stats: map[string]int64{"backend_records": 3}},
	})
	if err := recorder.RecordOnce(ctx); err != nil {
		t.Fatalf("RecordOnce: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["live_object_count"]; ok {
		t.Errorf("live_object_count present in JSONL when adapter does not report it")
	}
}
