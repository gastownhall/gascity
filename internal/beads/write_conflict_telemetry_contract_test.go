package beads

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/steveyegge/beads/issueops"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// countWriteConflictSamples checks the metadata write's counter and its bounded
// label set, which must not carry bead IDs or error text.
func countWriteConflictSamples(t *testing.T, reader *sdkmetric.ManualReader, wantOperation, wantOutcome string) int64 {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var count int64
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "gc.beads.write_conflicts.total" {
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
				}
				if len(labels) != 3 || labels["operation"] != wantOperation ||
					labels["class"] != "serialization" || labels["outcome"] != wantOutcome {
					t.Errorf("%s labels = %v, want only operation=%s class=serialization outcome=%s", metric.Name, labels, wantOperation, wantOutcome)
				}
				count += point.Value
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
	previous := otel.GetLoggerProvider()
	otel.SetLoggerProvider(provider)
	t.Cleanup(func() {
		otel.SetLoggerProvider(previous)
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
			conflict := errors.New("Error 1213 (40001): serialization conflict")
			store := newRetryCountingStore(t, retryCountingLifecycle{
				update: func(issueops.UpdateRequest) (issueops.UpdateResult, error) {
					writes++
					if recovered && writes == 3 {
						return issueops.UpdateResult{Changed: true}, nil
					}
					return issueops.UpdateResult{}, conflict
				},
			})
			err := store.SetMetadataBatch("gc-1", map[string]string{"state": "active"})
			if writes != 3 || (recovered && err != nil) || (!recovered && !errors.Is(err, conflict)) {
				t.Fatalf("write result = (%d calls, %v), want third-attempt %s", writes, err, name)
			}
			if got := countWriteConflictSamples(t, reader, "set_metadata_batch", name); got != 1 {
				t.Errorf("%s counter = %d, want 1", name, got)
			}
			assertExhaustedWarning(t, logged, "gc-1", "set_metadata_batch", name == "exhausted")
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
			if got := countWriteConflictSamples(t, reader, "update", name); got != 1 {
				t.Errorf("%s counter = %d, want 1", name, got)
			}
			assertExhaustedWarning(t, logged, "ga-session", "update", name == "exhausted")
		})
	}
}

// A recovered write reports the most recent retried conflict, while exhaustion
// reports the final error. Recovery before the budget stays silent.
func TestBdStoreRecoveredFinalClass(t *testing.T) {
	serialization := errors.New("Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction")
	catalog := errors.New("failed to prepare catalog")
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")
	for _, tt := range []struct {
		name    string
		laps    []error
		want    []writeConflictPoint
		warning map[string]string
	}{
		{
			name: "transient_catalog",
			laps: []error{catalog, catalog, nil},
			want: []writeConflictPoint{{operation: "update", class: "transient", outcome: "recovered-final", count: 1}},
		},
		{
			name: "mixed_serialization_then_busy",
			laps: []error{serialization, busy, nil},
			want: []writeConflictPoint{{operation: "update", class: "transient", outcome: "recovered-final", count: 1}},
		},
		{
			name: "mixed_busy_then_serialization",
			laps: []error{busy, serialization, nil},
			want: []writeConflictPoint{{operation: "update", class: "serialization", outcome: "recovered-final", count: 1}},
		},
		{
			name: "serialization_only",
			laps: []error{serialization, serialization, nil},
			want: []writeConflictPoint{{operation: "update", class: "serialization", outcome: "recovered-final", count: 1}},
		},
		{
			name:    "exhausted_uses_final_error",
			laps:    []error{serialization, serialization, catalog},
			want:    []writeConflictPoint{{operation: "update", class: "transient", outcome: "exhausted", count: 1}},
			warning: map[string]string{"operation": "update", "bead_id": "ga-session", "class": "transient", "attempts": "3"},
		},
		{
			name: "below_budget_stays_silent",
			laps: []error{catalog, nil},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := captureWriteConflictMetrics(t)
			logged := captureWriteConflictLogs(t)
			calls := 0
			runner := func(_, _ string, _ ...string) ([]byte, error) {
				calls++
				if calls > len(tt.laps) {
					t.Fatalf("write call %d exceeds scripted history of %d calls", calls, len(tt.laps))
				}
				if err := tt.laps[calls-1]; err != nil {
					return nil, err
				}
				return []byte(`{"id":"ga-session"}`), nil
			}
			err := NewBdStore("/city", runner).SetMetadataBatch("ga-session", map[string]string{"state": "active"})
			wantErr := tt.laps[len(tt.laps)-1]
			if calls != len(tt.laps) || !errors.Is(err, wantErr) {
				t.Fatalf("write result = (%d calls, %v), want (%d calls, %v)", calls, err, len(tt.laps), wantErr)
			}
			assertWriteConflictTelemetry(t, reader, logged, tt.want, tt.warning)
		})
	}
}

func assertExhaustedWarning(t *testing.T, exporter *writeConflictLogExporter, id, operation string, want bool) {
	t.Helper()
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	var matching []sdklog.Record
	for _, record := range exporter.records {
		attrs := map[string]attribute.Value{}
		record.WalkAttributes(func(kv attribute.KeyValue) bool {
			attrs[string(kv.Key)] = kv.Value
			return true
		})
		if record.Severity() != otellog.SeverityWarn {
			continue
		}
		if attrs["bead_id"].AsString() != id ||
			attrs["operation"].AsString() != operation ||
			attrs["class"].AsString() != "serialization" ||
			attrs["attempts"].AsInt64() != 3 {
			t.Errorf("exhaustion warning attributes = %v, want bead_id=%s operation=%s class=serialization attempts=3", attrs, id, operation)
		}
		matching = append(matching, record)
	}
	if got := len(matching); (want && got != 1) || (!want && got != 0) {
		wantCount := 0
		if want {
			wantCount = 1
		}
		t.Fatalf("structured exhaustion warnings = %d, want %d; records=%v", got, wantCount, exporter.records)
	}
}

// writeConflictPoint is one data point of gc.beads.write_conflicts.total.
type writeConflictPoint struct {
	operation, class, outcome string
	count                     int64
}

// writeConflictPoints returns the counter's data points in a stable order and
// fails the test for a point that carries any label besides operation, class
// and outcome: a bead ID or error text must never reach a label.
func writeConflictPoints(t *testing.T, reader *sdkmetric.ManualReader) []writeConflictPoint {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var points []writeConflictPoint
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "gc.beads.write_conflicts.total" {
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
				}
				if len(labels) != 3 {
					t.Errorf("%s labels = %v, want only operation, class and outcome", metric.Name, labels)
				}
				points = append(points, writeConflictPoint{
					operation: labels["operation"], class: labels["class"], outcome: labels["outcome"], count: point.Value,
				})
			}
		}
	}
	sort.Slice(points, func(i, j int) bool { return fmt.Sprint(points[i]) < fmt.Sprint(points[j]) })
	return points
}

// writeConflictWarnings returns the attributes of every warning the exporter
// received, one map per record.
func writeConflictWarnings(exporter *writeConflictLogExporter) []map[string]string {
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	var warnings []map[string]string
	for _, record := range exporter.records {
		if record.Severity() != otellog.SeverityWarn {
			continue
		}
		attrs := map[string]string{}
		record.WalkAttributes(func(kv attribute.KeyValue) bool {
			attrs[string(kv.Key)] = kv.Value.String()
			return true
		})
		warnings = append(warnings, attrs)
	}
	return warnings
}

// assertWriteConflictTelemetry checks everything a write left behind. want is
// the exact set of counter points, including each point's class (none for nil).
// warned is the one structured warning's attributes (none for nil).
func assertWriteConflictTelemetry(t *testing.T, reader *sdkmetric.ManualReader, logged *writeConflictLogExporter, want []writeConflictPoint, warned map[string]string) {
	t.Helper()
	got := writeConflictPoints(t, reader)
	match := len(got) == len(want)
	for i := 0; match && i < len(got); i++ {
		match = got[i].operation == want[i].operation && got[i].outcome == want[i].outcome &&
			got[i].count == want[i].count && got[i].class == want[i].class
	}
	if !match {
		t.Errorf("write-conflict counter = %v, want %v", got, want)
	}
	var wantWarnings []map[string]string
	if warned != nil {
		wantWarnings = []map[string]string{warned}
	}
	if gotWarnings := writeConflictWarnings(logged); fmt.Sprint(gotWarnings) != fmt.Sprint(wantWarnings) {
		t.Errorf("structured warnings = %v, want %v", gotWarnings, wantWarnings)
	}
}

// recordWriteBudget reports only a write that ran to the end of its attempt
// budget: silence below it (the last lap before the budget included), recovered
// or exhausted at it, exhausted only for an error the retry loop itself would
// have retried, and classed cas, serialization or transient by the error (or,
// for a write that recovered on its last attempt, by the last retried error).
func TestRecordWriteBudgetDecisions(t *testing.T) {
	const budget = 3
	serialization := errors.New("Error 1213 (40001): serialization failure")
	cas := &CASRetriesExhaustedError{ID: "ga-1", Key: "state", Attempts: budget}
	for _, tt := range []struct {
		name      string
		attempts  int
		err       error
		retryable bool
		retried   error  // the last retried lap's error, for a recovered write
		outcome   string // empty: nothing is recorded
		class     string // empty: not asserted
	}{
		{"first attempt succeeded", 1, nil, false, nil, "", ""},
		{"recovered before the last attempt", budget - 1, nil, false, serialization, "", ""},
		{"recovered on the last attempt after serialization", budget, nil, false, serialization, "recovered-final", "serialization"},
		{"recovered on the last attempt after a transient error", budget, nil, false, errors.New("database is locked"), "recovered-final", "transient"},
		{"recovered on the last attempt after a lost CAS round", budget, nil, false, &PreconditionFailedError{Current: 2}, "recovered-final", "cas"},
		{"recovered on the last attempt with no retried error", budget, nil, false, nil, "recovered-final", "transient"},
		{"conflict before the last attempt", budget - 1, serialization, true, nil, "", ""},
		{"exhausted by a serialization conflict", budget, serialization, true, nil, "exhausted", "serialization"},
		{"exhausted by CAS retries", budget, cas, true, nil, "exhausted", "cas"},
		{"exhausted by another retryable error", budget, errors.New("failed to prepare catalog"), true, nil, "exhausted", "transient"},
		{"exhaustion ignores the retried error", budget, serialization, true, errors.New("database is locked"), "exhausted", "serialization"},
		{"permanent failure on the last attempt", budget, errors.New("permission denied"), false, nil, "", ""},
		{"the retry loop's verdict decides, not the error text", budget, serialization, false, nil, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := captureWriteConflictMetrics(t)
			logged := captureWriteConflictLogs(t)
			recordWriteBudget("update", "ga-1", tt.attempts, budget, tt.err, tt.retryable, tt.retried)
			points := writeConflictPoints(t, reader)
			warnings := writeConflictWarnings(logged)
			if tt.outcome == "" {
				if len(points) != 0 || len(warnings) != 0 {
					t.Fatalf("recorded %v and warned %v, want silence", points, warnings)
				}
				return
			}
			if len(points) != 1 || points[0].operation != "update" || points[0].outcome != tt.outcome || points[0].count != 1 ||
				(tt.class != "" && points[0].class != tt.class) {
				t.Fatalf("points = %v, want one %q point for operation update (class %q)", points, tt.outcome, tt.class)
			}
			if tt.outcome != "exhausted" {
				if len(warnings) != 0 {
					t.Fatalf("a recovered write warned: %v", warnings)
				}
				return
			}
			want := []map[string]string{{"operation": "update", "bead_id": "ga-1", "class": tt.class, "attempts": fmt.Sprint(tt.attempts)}}
			if fmt.Sprint(warnings) != fmt.Sprint(want) {
				t.Fatalf("warnings = %v, want %v", warnings, want)
			}
		})
	}
}

// bdWriteTarget names the verb and the first bead of every argv shape the
// BdStore builds, skipping flags and the values of the flags that take one.
func TestBdWriteTarget(t *testing.T) {
	for _, tt := range []struct {
		name          string
		args          []string
		operation, id string
	}{
		{"no argv", nil, "", ""},
		{"update names the bead after --json", []string{"update", "--json", "ga-1", "--set-metadata", "k=v"}, "update", "ga-1"},
		{"claim", []string{"update", "ga-1", "--claim", "--json"}, "update", "ga-1"},
		{"close skips the --reason value", []string{"close", "--force", "--json", "--reason", "done with ga-9", "ga-1", "ga-2"}, "close", "ga-1"},
		{"close without a reason names its first bead", []string{"close", "--force", "--json", "ga-1", "ga-2"}, "close", "ga-1"},
		{"short reason flag", []string{"close", "-r", "done", "ga-1"}, "close", "ga-1"},
		{"flag values are not beads", []string{"update", "--status", "open", "--assignee", "alice", "--add-label", "x", "--remove-label", "y", "--set-metadata", "k=v", "ga-3"}, "update", "ga-3"},
		{"reclaim names its --id", []string{"reclaim", "--id", "ga-4", "--json"}, "reclaim", "ga-4"},
		{"dangling --id", []string{"reclaim", "--id"}, "reclaim", ""},
		{"delete", []string{"delete", "--force", "--json", "ga-5"}, "delete", "ga-5"},
		{"reopen", []string{"reopen", "--json", "ga-6"}, "reopen", "ga-6"},
		{"dep add names the issue, not the sub-command", []string{"dep", "add", "ga-7", "ga-8", "--type", "blocks"}, "dep", "ga-7"},
		{"dep remove", []string{"dep", "remove", "ga-7", "ga-8"}, "dep", "ga-7"},
		{"bare dep", []string{"dep"}, "dep", ""},
		{"a verb that takes no bead", []string{"update"}, "update", ""},
		{"only flags", []string{"close", "--json"}, "close", ""},
		{"create never names a bead", []string{"create", "--json", "a title"}, "create", ""},
		{"import names a file, not a bead", []string{"import", "beads.jsonl"}, "import", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if operation, id := bdWriteTarget(tt.args); operation != tt.operation || id != tt.id {
				t.Fatalf("bdWriteTarget(%q) = (%q, %q), want (%q, %q)", tt.args, operation, id, tt.operation, tt.id)
			}
		})
	}
}

// A permanent failure on the last lap, after two conflicts, is not budget
// exhaustion: the retry loop gave up on it, so neither store reports it.
func TestWriteConflictTelemetryIgnoresPermanentFailureOnLastAttempt(t *testing.T) {
	conflict := errors.New("Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction")
	permanent := errors.New("permission denied")
	t.Run("native", func(t *testing.T) {
		reader := captureWriteConflictMetrics(t)
		logged := captureWriteConflictLogs(t)
		writes := 0
		store := newRetryCountingStore(t, retryCountingLifecycle{
			update: func(issueops.UpdateRequest) (issueops.UpdateResult, error) {
				writes++
				if writes == nativeWriteAttempts {
					return issueops.UpdateResult{}, permanent
				}
				return issueops.UpdateResult{}, conflict
			},
		})
		err := store.SetMetadataBatch("gc-1", map[string]string{"state": "active"})
		if writes != nativeWriteAttempts || !errors.Is(err, permanent) {
			t.Fatalf("write result = (%d calls, %v), want %d calls ending in the permanent error", writes, err, nativeWriteAttempts)
		}
		assertWriteConflictTelemetry(t, reader, logged, nil, nil)
	})
	t.Run("bd", func(t *testing.T) {
		reader := captureWriteConflictMetrics(t)
		logged := captureWriteConflictLogs(t)
		calls := 0
		runner := func(_, _ string, _ ...string) ([]byte, error) {
			calls++
			if calls == bdTransientWriteAttempts {
				return nil, permanent
			}
			return nil, conflict
		}
		err := NewBdStore("/city", runner).SetMetadataBatch("ga-session", map[string]string{"state": "active"})
		if calls != bdTransientWriteAttempts || !errors.Is(err, permanent) {
			t.Fatalf("write result = (%d calls, %v), want %d calls ending in the permanent error", calls, err, bdTransientWriteAttempts)
		}
		assertWriteConflictTelemetry(t, reader, logged, nil, nil)
	})
}

// The BdStore's fenced writes report their own budget under <verb>_if_match.
func TestBdStoreConditionalWriteTelemetry(t *testing.T) {
	disableConditionalWriteSleep(t)
	conflict := errors.New("Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction")
	for _, recovered := range []bool{false, true} {
		name := "exhausted"
		if recovered {
			name = "recovered-final"
		}
		t.Run(name, func(t *testing.T) {
			reader := captureWriteConflictMetrics(t)
			logged := captureWriteConflictLogs(t)
			w := &scriptedBd{id: "ga-1", revision: 1, status: "open"}
			w.writeHook = func(w *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
				if recovered && w.writeCalls == conditionalWriteMaxAttempts {
					return nil, nil, false
				}
				return nil, conflict, true
			}
			title := "renamed"
			err := NewBdStore("/city", w.runner).UpdateIfMatch("ga-1", 1, UpdateOpts{Title: &title})
			if w.writeCalls != conditionalWriteMaxAttempts || (recovered && err != nil) || (!recovered && err == nil) {
				t.Fatalf("write result = (%d calls, %v), want %d calls ending %s", w.writeCalls, err, conditionalWriteMaxAttempts, name)
			}
			want := []writeConflictPoint{{operation: "update_if_match", class: "serialization", outcome: name, count: 1}}
			var warned map[string]string
			if !recovered {
				warned = map[string]string{"operation": "update_if_match", "bead_id": "ga-1", "class": "serialization", "attempts": fmt.Sprint(conditionalWriteMaxAttempts)}
			}
			assertWriteConflictTelemetry(t, reader, logged, want, warned)
		})
	}
	for _, tt := range []struct {
		name    string
		laps    []error
		outcome string
		warning map[string]string
	}{
		{
			name:    "recovered-final after a catalog error",
			laps:    []error{errors.New("failed to prepare catalog"), errors.New("failed to prepare catalog"), nil},
			outcome: "recovered-final",
		},
		{
			name:    "exhausted after serialization laps",
			laps:    []error{conflict, conflict, errors.New("failed to prepare catalog")},
			outcome: "exhausted",
			warning: map[string]string{"operation": "update_if_match", "bead_id": "ga-1", "class": "transient", "attempts": "3"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := captureWriteConflictMetrics(t)
			logged := captureWriteConflictLogs(t)
			w := &scriptedBd{id: "ga-1", revision: 1, status: "open"}
			w.writeHook = func(w *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
				if w.writeCalls > len(tt.laps) {
					t.Fatalf("write call %d exceeds scripted history of %d calls", w.writeCalls, len(tt.laps))
				}
				err := tt.laps[w.writeCalls-1]
				return nil, err, err != nil
			}
			title := "renamed"
			err := NewBdStore("/city", w.runner).UpdateIfMatch("ga-1", 1, UpdateOpts{Title: &title})
			wantErr := tt.laps[len(tt.laps)-1]
			if w.writeCalls != len(tt.laps) || !errors.Is(err, wantErr) {
				t.Fatalf("write result = (%d calls, %v), want (%d calls, %v)", w.writeCalls, err, len(tt.laps), wantErr)
			}
			want := []writeConflictPoint{{operation: "update_if_match", class: "transient", outcome: tt.outcome, count: 1}}
			assertWriteConflictTelemetry(t, reader, logged, want, tt.warning)
		})
	}
}

// The metadata compare-and-set emulation reports exhaustion as class cas under
// set_metadata_cas, once: its inner fenced writes only meet preconditions.
func TestBdStoreCASEmulationTelemetry(t *testing.T) {
	disableConditionalWriteSleep(t)
	for _, won := range []bool{false, true} {
		name := "exhausted"
		if won {
			name = "recovered-final"
		}
		t.Run(name, func(t *testing.T) {
			reader := captureWriteConflictMetrics(t)
			logged := captureWriteConflictLogs(t)
			w := &scriptedBd{id: "ga-1", revision: 1, status: "open", metadata: map[string]string{"k": "start"}}
			w.writeHook = func(w *scriptedBd, _ string, _ int64) ([]byte, error, bool) {
				if won && w.writeCalls == casEmulationMaxAttempts {
					return nil, nil, false
				}
				w.revision++ // an unrelated write moves the revision under the lap
				return w.preconditionBodyLocked(0), errors.New("exit status 1"), true
			}
			ok, err := NewBdStore("/city", w.runner).CompareAndSetMetadataKey("ga-1", "k", "start", "won")
			if won && (!ok || err != nil) || !won && (ok || !IsCASRetriesExhausted(err)) {
				t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want %s", ok, err, name)
			}
			want := []writeConflictPoint{{operation: "set_metadata_cas", class: "cas", outcome: name, count: 1}}
			var warned map[string]string
			if !won {
				warned = map[string]string{"operation": "set_metadata_cas", "bead_id": "ga-1", "class": "cas", "attempts": fmt.Sprint(casEmulationMaxAttempts)}
			}
			assertWriteConflictTelemetry(t, reader, logged, want, warned)
		})
	}
}

// The native fenced writers report exhaustion under their own operation labels.
func TestNativeDoltConditionalWriteTelemetryOperations(t *testing.T) {
	status := "in_progress"
	for _, tt := range []struct {
		name, operation string
		write           func(*NativeDoltStore) error
	}{
		{"UpdateIfMatch", "update_if_match", func(s *NativeDoltStore) error { return s.UpdateIfMatch("gc-1", 7, UpdateOpts{Status: &status}) }},
		{"CloseIfMatch", "close_if_match", func(s *NativeDoltStore) error { return s.CloseIfMatch("gc-1", 7) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := captureWriteConflictMetrics(t)
			logged := captureWriteConflictLogs(t)
			store := newRetryCountingStore(t, retryCountingLifecycle{
				update: func(issueops.UpdateRequest) (issueops.UpdateResult, error) {
					return issueops.UpdateResult{}, errors.New(serializationConflictErr)
				},
				close: func(issueops.CloseRequest) (issueops.CloseResult, error) {
					return issueops.CloseResult{}, errors.New(serializationConflictErr)
				},
			})
			if err := tt.write(store); !isNativeDoltSerializationConflict(err) {
				t.Fatalf("%s = %v, want the serialization conflict after the budget", tt.name, err)
			}
			want := []writeConflictPoint{{operation: tt.operation, class: "serialization", outcome: "exhausted", count: 1}}
			warned := map[string]string{"operation": tt.operation, "bead_id": "gc-1", "class": "serialization", "attempts": fmt.Sprint(nativeWriteAttempts)}
			assertWriteConflictTelemetry(t, reader, logged, want, warned)
		})
	}
}
