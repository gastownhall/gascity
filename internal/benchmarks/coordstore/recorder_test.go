package coordstore

import (
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
