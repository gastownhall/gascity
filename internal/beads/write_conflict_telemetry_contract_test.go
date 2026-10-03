package beads

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/telemetry"
	beadslib "github.com/steveyegge/beads"
	"go.opentelemetry.io/otel"
	otellog "go.opentelemetry.io/otel/log"
	otellogglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// countWriteConflictSamples leaves instrument naming to the implementation.
// The public observation is one cumulative counter for the write outcome,
// with bounded labels and no bead ID or error text in metric attributes.
func countWriteConflictSamples(t *testing.T, reader *sdkmetric.ManualReader, wantOutcome string) int64 {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var count int64
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if !strings.HasPrefix(metric.Name, "gc.beads.") ||
				(!strings.Contains(metric.Name, "conflict") && !strings.Contains(metric.Name, "retry")) {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 counter", metric.Name, metric.Data)
			}
			for _, point := range sum.DataPoints {
				labels := map[string]string{}
				for _, kv := range point.Attributes.ToSlice() {
					labels[string(kv.Key)] = kv.Value.AsString()
					if strings.Contains(kv.Value.AsString(), "ga-session") || strings.Contains(kv.Value.AsString(), "1213") {
						t.Errorf("%s has unbounded attribute %s=%q", metric.Name, kv.Key, kv.Value.AsString())
					}
				}
				if labels["outcome"] == wantOutcome {
					if labels["operation"] == "" || labels["class"] == "" {
						t.Errorf("%s labels = %v, want operation and class", metric.Name, labels)
					}
					count += point.Value
				}
			}
		}
	}
	return count
}

func captureWriteConflictMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	telemetry.ResetInstrumentsForTest()
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		telemetry.ResetInstrumentsForTest()
		_ = provider.Shutdown(context.Background())
	})
	return reader
}

type writeConflictLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *writeConflictLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, record := range records {
		e.records = append(e.records, record.Clone())
	}
	return nil
}

func (*writeConflictLogExporter) Shutdown(context.Context) error   { return nil }
func (*writeConflictLogExporter) ForceFlush(context.Context) error { return nil }

func captureWriteConflictLogs(t *testing.T) *writeConflictLogExporter {
	t.Helper()
	exporter := &writeConflictLogExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	previous := otellogglobal.GetLoggerProvider()
	otellogglobal.SetLoggerProvider(provider)
	t.Cleanup(func() {
		otellogglobal.SetLoggerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	return exporter
}

func TestNativeDoltWriteConflictTelemetry(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "exhausted"
		if recovered {
			name = "recovered-final"
		}
		t.Run(name, func(t *testing.T) {
			reader := captureWriteConflictMetrics(t)
			logged := captureWriteConflictLogs(t)
			writes := 0
			storage := &nativeDoltStorageSpy{
				getIssue: func(context.Context, string) (*beadslib.Issue, error) {
					return &beadslib.Issue{ID: "ga-session", RowVersion: int64(writes + 1)}, nil
				},
				updateIssueChecked: func(context.Context, string, map[string]interface{}, string, beadslib.UpdateIssueOptions) error {
					writes++
					if recovered && writes == 3 {
						return nil
					}
					return beadslib.ErrVersionMismatch
				},
			}
			err := newNativeDoltStoreForTest(storage).SetMetadataBatch("ga-session", map[string]string{"state": "active"})
			if writes != 3 || (recovered && err != nil) || (!recovered && !IsCASRetriesExhausted(err)) {
				t.Fatalf("write result = (%d calls, %v), want third-attempt %s", writes, err, name)
			}
			if got := countWriteConflictSamples(t, reader, name); got != 1 {
				t.Errorf("%s counter = %d, want 1", name, got)
			}
			assertExhaustedWarning(t, logged, "ga-session", name == "exhausted")
		})
	}
}

func TestBdStoreWriteConflictTelemetry(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "exhausted"
		if recovered {
			name = "recovered-final"
		}
		t.Run(name, func(t *testing.T) {
			reader := captureWriteConflictMetrics(t)
			logged := captureWriteConflictLogs(t)
			calls := 0
			conflict := errors.New("Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction")
			runner := func(_, _ string, _ ...string) ([]byte, error) {
				calls++
				if recovered && calls == 3 {
					return []byte(`{"id":"ga-session"}`), nil
				}
				return nil, conflict
			}
			err := NewBdStore("/city", runner).SetMetadataBatch("ga-session", map[string]string{"state": "active"})
			if calls != 3 || (recovered && err != nil) || (!recovered && !errors.Is(err, conflict)) {
				t.Fatalf("write result = (%d calls, %v), want third-attempt %s", calls, err, name)
			}
			if got := countWriteConflictSamples(t, reader, name); got != 1 {
				t.Errorf("%s counter = %d, want 1", name, got)
			}
			assertExhaustedWarning(t, logged, "ga-session", name == "exhausted")
		})
	}
}

func assertExhaustedWarning(t *testing.T, exporter *writeConflictLogExporter, id string, want bool) {
	t.Helper()
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	var matching []sdklog.Record
	for _, record := range exporter.records {
		attrs := map[string]otellog.Value{}
		record.WalkAttributes(func(kv otellog.KeyValue) bool {
			attrs[kv.Key] = kv.Value
			return true
		})
		if record.Severity() == otellog.SeverityWarn &&
			attrs["bead_id"].AsString() == id &&
			attrs["operation"].AsString() != "" &&
			attrs["class"].AsString() != "" &&
			attrs["attempts"].AsInt64() == 3 {
			matching = append(matching, record)
		}
	}
	if got := len(matching); (want && got != 1) || (!want && got != 0) {
		wantCount := 0
		if want {
			wantCount = 1
		}
		t.Fatalf("structured exhaustion warnings = %d, want %d; records=%v", got, wantCount, exporter.records)
	}
}
