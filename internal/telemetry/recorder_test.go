package telemetry

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	otellogglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// resetInstruments resets the sync.Once so initInstruments re-runs against
// the current (noop) global MeterProvider during tests.
func resetInstruments(t *testing.T) {
	t.Helper()
	instOnce = sync.Once{}
	t.Cleanup(func() { instOnce = sync.Once{} })
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

	RecordAgentStop(ctx, "gc-test-agent1", "orphan", nil)
	RecordAgentStop(ctx, "gc-test-agent2", "drift", errors.New("stop error"))
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

	RecordReconcileCycle(ctx, 3, 1, 2)
	RecordReconcileCycle(ctx, 0, 0, 0)
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

func recordAttrs(rec sdklog.Record) map[string]otellog.Value {
	attrs := make(map[string]otellog.Value)
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		attrs[kv.Key] = kv.Value
		return true
	})
	return attrs
}

func logValueStringSlice(value otellog.Value) []string {
	values := value.AsSlice()
	out := make([]string, 0, len(values))
	for _, item := range values {
		out = append(out, item.AsString())
	}
	return out
}
