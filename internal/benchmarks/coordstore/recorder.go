package coordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// OpSnapshot captures one operation histogram at a telemetry sample point.
type OpSnapshot struct {
	Samples   int   `json:"samples"`
	Errors    int   `json:"errors"`
	P50Nanos  int64 `json:"p50_nanos"`
	P95Nanos  int64 `json:"p95_nanos"`
	P99Nanos  int64 `json:"p99_nanos"`
	P999Nanos int64 `json:"p999_nanos"`
	MaxNanos  int64 `json:"max_nanos"`
}

// TelemetrySample is one JSONL row emitted by TimeSeriesRecorder.
type TelemetrySample struct {
	Timestamp      time.Time             `json:"timestamp"`
	HeapAllocBytes uint64                `json:"heap_alloc_bytes"`
	HeapInuseBytes uint64                `json:"heap_inuse_bytes"`
	RSSBytes       uint64                `json:"rss_bytes,omitempty"`
	Goroutines     int                   `json:"goroutines"`
	StoreSizeBytes int64                 `json:"store_size_bytes"`
	AdapterStats   map[string]int64      `json:"adapter_stats,omitempty"`
	Operations     map[string]OpSnapshot `json:"operations,omitempty"`
}

// TimeSeriesRecorderConfig configures a telemetry JSONL recorder.
type TimeSeriesRecorderConfig struct {
	Path              string
	DataDir           string
	SampleInterval    time.Duration
	Adapter           StoreAdapter
	HistogramSnapshot func() map[string]*OperationResult
}

// TimeSeriesRecorder appends telemetry samples to a JSONL file and fsyncs each
// sample.
type TimeSeriesRecorder struct {
	cfg TimeSeriesRecorderConfig

	mu     sync.Mutex
	file   *os.File
	stopCh chan struct{}
	doneCh chan struct{}
}

// NewTimeSeriesRecorder returns a recorder for cfg.
func NewTimeSeriesRecorder(cfg TimeSeriesRecorderConfig) *TimeSeriesRecorder {
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = time.Second
	}
	return &TimeSeriesRecorder{cfg: cfg}
}

// Start opens the JSONL file, writes an immediate sample, and starts periodic
// sampling until Stop is called or ctx is canceled.
func (r *TimeSeriesRecorder) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.file != nil {
		r.mu.Unlock()
		return nil
	}
	file, err := openRecorderFile(r.cfg.Path)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	r.file = file
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})
	if err := r.recordOnceLocked(ctx, file); err != nil {
		file.Close() //nolint:errcheck
		r.file = nil
		r.stopCh = nil
		r.doneCh = nil
		r.mu.Unlock()
		return err
	}
	stopCh := r.stopCh
	doneCh := r.doneCh
	interval := r.cfg.SampleInterval
	r.mu.Unlock()

	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = r.RecordOnce(ctx)
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			}
		}
	}()
	return nil
}

// Stop halts periodic sampling and closes the JSONL file.
func (r *TimeSeriesRecorder) Stop() error {
	r.mu.Lock()
	stopCh := r.stopCh
	doneCh := r.doneCh
	file := r.file
	r.stopCh = nil
	r.doneCh = nil
	r.file = nil
	r.mu.Unlock()

	if stopCh != nil {
		close(stopCh)
		<-doneCh
	}
	if file != nil {
		if err := file.Close(); err != nil {
			return fmt.Errorf("closing timeseries recorder: %w", err)
		}
	}
	return nil
}

// RecordOnce appends and fsyncs one telemetry sample.
func (r *TimeSeriesRecorder) RecordOnce(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	file := r.file
	closeAfter := false
	if file == nil {
		var err error
		file, err = openRecorderFile(r.cfg.Path)
		if err != nil {
			return err
		}
		closeAfter = true
	}
	if closeAfter {
		defer file.Close() //nolint:errcheck
	}
	return r.recordOnceLocked(ctx, file)
}

func (r *TimeSeriesRecorder) recordOnceLocked(ctx context.Context, file *os.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sample, err := r.sample(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(sample)
	if err != nil {
		return fmt.Errorf("marshaling telemetry sample: %w", err)
	}
	data = append(data, '\n')
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("writing telemetry sample: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("fsync telemetry sample: %w", err)
	}
	return nil
}

func (r *TimeSeriesRecorder) sample(ctx context.Context) (TelemetrySample, error) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	size, err := storeSizeBytes(r.cfg.DataDir)
	if err != nil {
		return TelemetrySample{}, err
	}
	sample := TelemetrySample{
		Timestamp:      time.Now().UTC(),
		HeapAllocBytes: ms.HeapAlloc,
		HeapInuseBytes: ms.HeapInuse,
		Goroutines:     runtime.NumGoroutine(),
		StoreSizeBytes: size,
	}
	if rss, ok := readRSSBytes(); ok {
		sample.RSSBytes = rss
	}
	if r.cfg.Adapter != nil {
		sample.AdapterStats = copyStats(r.cfg.Adapter.Stats(ctx))
	}
	if r.cfg.HistogramSnapshot != nil {
		sample.Operations = snapshotOperations(r.cfg.HistogramSnapshot())
	}
	return sample, nil
}

func openRecorderFile(path string) (*os.File, error) {
	if path == "" {
		return nil, fmt.Errorf("timeseries recorder path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating timeseries directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening timeseries file %s: %w", path, err)
	}
	return file, nil
}

func storeSizeBytes(root string) (int64, error) {
	if root == "" {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("walking store data dir %s: %w", root, err)
	}
	return total, nil
}

func copyStats(stats map[string]int64) map[string]int64 {
	if len(stats) == 0 {
		return nil
	}
	out := make(map[string]int64, len(stats))
	for k, v := range stats {
		out[k] = v
	}
	return out
}

func snapshotOperations(results map[string]*OperationResult) map[string]OpSnapshot {
	if len(results) == 0 {
		return nil
	}
	out := make(map[string]OpSnapshot, len(results))
	for name, res := range results {
		if res == nil {
			continue
		}
		op := OpSnapshot{Samples: res.Samples, Errors: res.Errors}
		if res.H != nil {
			op.P50Nanos = res.H.P50().Nanoseconds()
			op.P95Nanos = res.H.P95().Nanoseconds()
			op.P99Nanos = res.H.P99().Nanoseconds()
			op.P999Nanos = res.H.P999().Nanoseconds()
			op.MaxNanos = res.H.Max().Nanoseconds()
		}
		out[name] = op
	}
	return out
}
