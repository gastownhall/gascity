package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
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

func TestRecordBeadStoreHealth(t *testing.T) {
	resetInstruments(t)
	ctx := context.Background()

	RecordBeadStoreHealth(ctx, "test-city", true)
	RecordBeadStoreHealth(ctx, "test-city", false)
}
