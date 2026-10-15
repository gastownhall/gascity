package telemetry

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	otellogglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// resetInstruments resets the sync.Once so initInstruments re-runs against
// the current (noop) global MeterProvider during tests.
func resetInstruments(t *testing.T) {
	t.Helper()
	ResetInstrumentsForTest()
	t.Cleanup(ResetInstrumentsForTest)
}

// --- helper functions ---

func TestStatusStr(t *testing.T) {
	if got := statusStr(nil); got != "ok" {
		t.Errorf("statusStr(nil) = %q, want \"ok\"", got)
	}
	if got := statusStr(errors.New("boom")); got != "error" {
		t.Errorf("statusStr(err) = %q, want \"error\"", got)
	}
}

func TestTruncateOutput_Short(t *testing.T) {
	if got := truncateOutput("hello", 10); got != "hello" {
		t.Errorf("short string should not be truncated, got %q", got)
	}
}

func TestTruncateOutput_Exact(t *testing.T) {
	if got := truncateOutput("abcde", 5); got != "abcde" {
		t.Errorf("string at exact limit should not be truncated, got %q", got)
	}
}

func TestTruncateOutput_Long(t *testing.T) {
	got := truncateOutput("abcdefghij", 5)
	if got != "abcde…" {
		t.Errorf("truncateOutput = %q, want %q", got, "abcde…")
	}
}

func TestTruncateOutput_Empty(t *testing.T) {
	if got := truncateOutput("", 10); got != "" {
		t.Errorf("empty string changed: %q", got)
	}
}

func TestSeverity_Nil(t *testing.T) {
	if got := severity(nil); got != otellog.SeverityInfo {
		t.Errorf("severity(nil) = %v, want SeverityInfo", got)
	}
}

func TestSeverity_Error(t *testing.T) {
	if got := severity(errors.New("err")); got != otellog.SeverityError {
		t.Errorf("severity(err) = %v, want SeverityError", got)
	}
}

func TestErrKV_Nil(t *testing.T) {
	kv := errKV(nil)
	if kv.Value.AsString() != "" {
		t.Errorf("errKV(nil) value = %q, want empty", kv.Value.AsString())
	}
}

func TestErrKV_NonNil(t *testing.T) {
	kv := errKV(errors.New("test error"))
	if kv.Value.AsString() != "test error" {
		t.Errorf("errKV(err) value = %q, want %q", kv.Value.AsString(), "test error")
	}
}

// --- Record* functions (noop providers, must not panic) ---

func TestRecordAgentStart(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordAgentStart(ctx, "gc-test-agent1", "agent1", nil)
	RecordAgentStart(ctx, "gc-test-agent2", "agent2", errors.New("start error"))
}

func TestRecordAgentStop(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordAgentStop(ctx, "gc-test-session1", "gc-test-agent1", "orphan", nil)
	RecordAgentStop(ctx, "gc-test-session2", "gc-test-agent2", "drift", errors.New("stop error"))
}

func TestRecordAgentCrash(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordAgentCrash(ctx, "agent1", "some output")
	RecordAgentCrash(ctx, "agent2", "")
}

func TestRecordAgentQuarantine(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordAgentQuarantine(ctx, "agent1")
}

func TestRecordAgentIdleKill(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordAgentIdleKill(ctx, "agent1")
}

func TestRecordReconcileCycle(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordReconcileCycle(ctx, 3)
	RecordReconcileCycle(ctx, 0)
}

func TestRecordNudge(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordNudge(ctx, "agent1", nil)
	RecordNudge(ctx, "agent2", errors.New("nudge error"))
}

func TestRecordConfigReload(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordConfigReload(ctx, "abc123", "manual", "applied", 0, nil)
	RecordConfigReload(ctx, "", "watch", "failed", 1, errors.New("parse error"))
}

func TestRecordControllerLifecycle(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordControllerLifecycle(ctx, "started")
	RecordControllerLifecycle(ctx, "stopped")
}

func TestRecordSupervisorStarted(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordSupervisorStarted(ctx, "clean")
	RecordSupervisorStarted(ctx, "crash")
	RecordSupervisorStarted(ctx, "unknown")
}

func TestRecordBDCall(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordBDCall(ctx, []string{"list", "--all"}, 12.5, nil, []byte("output"), "")
	RecordBDCall(ctx, []string{"status"}, 3.0, errors.New("fail"), []byte(""), "stderr msg")
	RecordBDCall(ctx, nil, 0, nil, nil, "")
}

func TestRecordBDCall_TruncatesLongOutput(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	bigStdout := make([]byte, maxStdoutLog+100)
	bigStderr := string(make([]byte, maxStderrLog+100))
	RecordBDCall(ctx, []string{"cmd"}, 1.0, nil, bigStdout, bigStderr)
}

func TestSanitizeBDArgsRedactsSecretFlags(t *testing.T) {
	in := []string{
		"config", "set",
		"--token", "sk-secret",
		"--api-key=api-secret",
		"--password", "pw-secret",
		"--remote-password=remote-secret",
		"--bearer", "bearer-secret",
		"--not-token", "visible",
	}
	original := append([]string(nil), in...)

	got := sanitizeBDArgs(in)
	want := []string{
		"config", "set",
		"--token", "<redacted>",
		"--api-key=<redacted>",
		"--password", "<redacted>",
		"--remote-password=<redacted>",
		"--bearer", "<redacted>",
		"--not-token", "visible",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeBDArgs() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(in, original) {
		t.Fatalf("sanitizeBDArgs mutated input: got %#v, want %#v", in, original)
	}
}

func TestRecordBDCallSanitizesArgs(t *testing.T) {
	resetInstruments(t)
	exp := installRecordingLogExporter(t)

	RecordBDCall(context.Background(), []string{"config", "set", "--token", "sk-secret"}, 12.5, nil, nil, "")

	rec := exp.recordByBody("bd.call")
	if rec == nil {
		t.Fatal("RecordBDCall did not emit bd.call")
	}
	attrs := recordAttrs(*rec)
	if got := attrs["args"].AsString(); got != "config set --token <redacted>" {
		t.Fatalf("bd.call args = %q, want token redacted", got)
	}
}

func TestRecordBDSlowEmitsSanitizedWarnEvent(t *testing.T) {
	resetInstruments(t)
	exp := installRecordingLogExporter(t)

	RecordBDSlow(context.Background(), []string{"config", "set", "--token", "sk-secret"}, "/tmp/city", "test-agent-1")

	rec := exp.recordByBody("bd.slow")
	if rec == nil {
		t.Fatal("RecordBDSlow did not emit bd.slow")
	}
	if got := rec.Severity(); got != otellog.SeverityWarn {
		t.Fatalf("bd.slow severity = %v, want WARN", got)
	}
	attrs := recordAttrs(*rec)
	if got := logValueStringSlice(attrs["args"]); !reflect.DeepEqual(got, []string{"config", "set", "--token", "<redacted>"}) {
		t.Fatalf("bd.slow args = %#v, want token redacted", got)
	}
	if got := attrs["dir"].AsString(); got != "/tmp/city" {
		t.Fatalf("bd.slow dir = %q, want /tmp/city", got)
	}
	if got := attrs["agent_id"].AsString(); got != "test-agent-1" {
		t.Fatalf("bd.slow agent_id = %q, want test-agent-1", got)
	}
	if got := attrs["elapsed_ms"].AsInt64(); got != BDSlowThreshold.Milliseconds() {
		t.Fatalf("bd.slow elapsed_ms = %d, want %d", got, BDSlowThreshold.Milliseconds())
	}
	if got := attrs["threshold_ms"].AsInt64(); got != BDSlowThreshold.Milliseconds() {
		t.Fatalf("bd.slow threshold_ms = %d, want %d", got, BDSlowThreshold.Milliseconds())
	}
	if got := attrs["timestamp"].AsString(); got == "" {
		t.Fatal("bd.slow timestamp is empty")
	}
}

func TestRecordCacheScanLargeEmitsWarnEvent(t *testing.T) {
	resetInstruments(t)
	exp := installRecordingLogExporter(t)

	RecordCacheScanLarge(context.Background(), "test-rig", 3272, 2500, 2100*time.Millisecond)

	rec := exp.recordByBody("beads.cache.scan_large")
	if rec == nil {
		t.Fatal("RecordCacheScanLarge did not emit beads.cache.scan_large")
	}
	if got := rec.Severity(); got != otellog.SeverityWarn {
		t.Fatalf("beads.cache.scan_large severity = %v, want WARN", got)
	}
	attrs := recordAttrs(*rec)
	if got := attrs["rig"].AsString(); got != "test-rig" {
		t.Fatalf("beads.cache.scan_large rig = %q, want test-rig", got)
	}
	if got := attrs["bead_count"].AsInt64(); got != 3272 {
		t.Fatalf("beads.cache.scan_large bead_count = %d, want 3272", got)
	}
	if got := attrs["threshold"].AsInt64(); got != 2500 {
		t.Fatalf("beads.cache.scan_large threshold = %d, want 2500", got)
	}
	if got := attrs["elapsed_ms"].AsInt64(); got != 2100 {
		t.Fatalf("beads.cache.scan_large elapsed_ms = %d, want 2100", got)
	}
}

func TestRecordCacheScanLargeNormalizesEmptyRig(t *testing.T) {
	resetInstruments(t)
	exp := installRecordingLogExporter(t)

	RecordCacheScanLarge(context.Background(), "  ", 2600, 2500, 150*time.Millisecond)

	rec := exp.recordByBody("beads.cache.scan_large")
	if rec == nil {
		t.Fatal("RecordCacheScanLarge did not emit beads.cache.scan_large")
	}
	attrs := recordAttrs(*rec)
	if got := attrs["rig"].AsString(); got != "(no-prefix)" {
		t.Fatalf("beads.cache.scan_large rig = %q, want (no-prefix)", got)
	}
}

func TestRecordBeadStoreHealth(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordBeadStoreHealth(ctx, "test-city", true)
	RecordBeadStoreHealth(ctx, "test-city", false)
}

type recordingLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func installRecordingLogExporter(t *testing.T) *recordingLogExporter {
	t.Helper()
	exp := &recordingLogExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	prev := otellogglobal.GetLoggerProvider()
	otellogglobal.SetLoggerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otellogglobal.SetLoggerProvider(prev)
	})
	return exp
}

func (e *recordingLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range records {
		e.records = append(e.records, rec.Clone())
	}
	return nil
}

func (e *recordingLogExporter) Shutdown(context.Context) error {
	return nil
}

func (e *recordingLogExporter) ForceFlush(context.Context) error {
	return nil
}

func (e *recordingLogExporter) recordByBody(body string) *sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.records {
		if e.records[i].Body().AsString() == body {
			rec := e.records[i].Clone()
			return &rec
		}
	}
	return nil
}

func recordAttrs(rec sdklog.Record) map[string]attribute.Value {
	attrs := make(map[string]attribute.Value)
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	return attrs
}

func logValueStringSlice(value attribute.Value) []string {
	values := value.AsSlice()
	out := make([]string, 0, len(values))
	for _, item := range values {
		out = append(out, item.AsString())
	}
	return out
}

// TestRecordOpRecord pins gc.ops.record.total: one datapoint per (kind,
// surface, stage, result, unrecorded_reason), the reason empty unless the
// request ran unrecorded.
func TestRecordOpRecord(t *testing.T) {
	resetInstruments(t)
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	ctx := context.Background()
	RecordOpRecord(ctx, "message", "api", "admit", "recorded", "")
	RecordOpRecord(ctx, "message", "api", "admit", "recorded", "")
	RecordOpRecord(ctx, "wake", "cli", "admit", "unrecorded", "store_unavailable")

	var out metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := map[string]int64{}
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gc.ops.record.total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				var key []string
				for _, k := range []attribute.Key{"kind", "surface", "stage", "result", "unrecorded_reason"} {
					v, ok := dp.Attributes.Value(k)
					if !ok {
						t.Fatalf("datapoint %v lacks %s", dp.Attributes, k)
					}
					key = append(key, v.AsString())
				}
				if dp.Attributes.Len() != 5 {
					t.Fatalf("datapoint attributes %v, want exactly five", dp.Attributes)
				}
				got[strings.Join(key, "|")] = dp.Value
			}
		}
	}
	want := map[string]int64{
		"message|api|admit|recorded|":                 2,
		"wake|cli|admit|unrecorded|store_unavailable": 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gc.ops.record.total = %v, want %v", got, want)
	}
}

func TestRecordTurnInterrupt(t *testing.T) {
	resetInstruments(t)
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	exp := installRecordingLogExporter(t)

	ctx := context.Background()
	RecordTurnInterrupt(ctx, "s-1", "turn-1", "settled", "unfenced_interrupt", "delivered", nil)
	RecordTurnInterrupt(ctx, "s-2", "turn-2", "settled", "unfenced_interrupt", "delivered", nil)
	RecordTurnInterrupt(ctx, "s-3", "", "unavailable", "", "queued", errors.New("queue write failed"))

	var out metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := map[string]int64{}
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gc.session.turn_interrupts.total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				var key []string
				for _, k := range []attribute.Key{"outcome", "degradation", "message_disposition"} {
					v, ok := dp.Attributes.Value(k)
					if !ok {
						t.Fatalf("datapoint %v lacks %s", dp.Attributes, k)
					}
					key = append(key, v.AsString())
				}
				if dp.Attributes.Len() != 3 {
					t.Fatalf("datapoint attributes %v, want exactly three", dp.Attributes)
				}
				got[strings.Join(key, "|")] = dp.Value
			}
		}
	}
	want := map[string]int64{
		"settled|unfenced_interrupt|delivered": 2,
		"unavailable||queued":                  1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gc.session.turn_interrupts.total = %v, want %v", got, want)
	}

	exp.mu.Lock()
	var recs []sdklog.Record
	for _, rec := range exp.records {
		if rec.Body().AsString() == "session.turn_interrupt" {
			recs = append(recs, rec.Clone())
		}
	}
	exp.mu.Unlock()
	if len(recs) != 3 {
		t.Fatalf("session.turn_interrupt events = %d, want 3", len(recs))
	}
	first := recordAttrs(recs[0])
	for k, v := range map[string]string{"session": "s-1", "turn": "turn-1", "outcome": "settled", "degradation": "unfenced_interrupt", "message_disposition": "delivered", "error": ""} {
		if got := first[k].AsString(); got != v {
			t.Fatalf("first event %s = %q, want %q", k, got, v)
		}
	}
	if recs[0].Severity() != otellog.SeverityInfo {
		t.Fatalf("first event severity = %v, want INFO", recs[0].Severity())
	}
	last := recordAttrs(recs[2])
	for k, v := range map[string]string{"session": "s-3", "turn": "", "outcome": "unavailable", "message_disposition": "queued", "error": "queue write failed"} {
		if got := last[k].AsString(); got != v {
			t.Fatalf("last event %s = %q, want %q", k, got, v)
		}
	}
	if recs[2].Severity() != otellog.SeverityError {
		t.Fatalf("last event severity = %v, want ERROR", recs[2].Severity())
	}
}
