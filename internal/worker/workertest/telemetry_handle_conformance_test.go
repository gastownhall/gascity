package workertest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/telemetry"
	worker "github.com/gastownhall/gascity/internal/worker"
)

// telemetryHandleProfile labels the WC-USAGE-RECORD-001 result. Unlike the
// transcript catalog, the handle-recording requirement is a single global
// handle-behavior contract rather than a per-fixture-profile rule, so it
// carries a synthetic profile id instead of one of the Phase1Profiles ids.
const telemetryHandleProfile ProfileID = "worker-handle"

// TestTelemetryHandleConformance emits WC-USAGE-RECORD-001
// (RequirementInvocationUsageRecording) through the SuiteReporter artifact
// path. The handle-recording requirement is registered by
// TelemetryHandleCatalog as the conformance contract of record, but its
// SessionHandle-positive and RuntimeHandle-negative behavior is enforced by
// worker-package handle tests that cannot reach this reporter (the workertest
// package imports worker, so worker cannot import workertest). Without this
// suite the GC_WORKER_REPORT_DIR artifacts record WC-TX-USAGE-001 and
// WC-USAGE-COST-001 while silently omitting the handle-recording requirement.
//
// The check drives both halves of the contract through the exported worker
// API: a transcript-backed SessionHandle prompt op must record
// gc.agent.tokens.*, and a runtime-only RuntimeHandle prompt op must record
// nothing (ga-tkvb31).
func TestTelemetryHandleConformance(t *testing.T) {
	reporter := NewSuiteReporter(t, "telemetry-handle", map[string]string{
		"tier": "worker-core",
	})
	reporter.Require(t, invocationUsageRecordingResult(t))
}

// invocationUsageRecordingResult evaluates WC-USAGE-RECORD-001 end to end and
// returns the conformance result. Setup failures fail the test loudly (they
// are harness problems, not contract violations); only an unexpected recorded
// datapoint count produces a failing result.
func invocationUsageRecordingResult(t *testing.T) Result {
	t.Helper()

	sessionInput := sessionHandleRecordedInputTokens(t)
	runtimeDatapoints := runtimeHandleInvocationDatapoints(t)

	evidence := map[string]string{
		"session_handle_input_tokens": strconv.FormatInt(sessionInput, 10),
		"runtime_handle_datapoints":   strconv.Itoa(runtimeDatapoints),
	}

	const wantSessionInput int64 = 100
	if sessionInput != wantSessionInput {
		return Fail(telemetryHandleProfile, RequirementInvocationUsageRecording,
			fmt.Sprintf("SessionHandle prompt op recorded gc.agent.tokens.input=%d, want %d", sessionInput, wantSessionInput)).
			WithEvidence(evidence)
	}
	if runtimeDatapoints != 0 {
		return Fail(telemetryHandleProfile, RequirementInvocationUsageRecording,
			fmt.Sprintf("RuntimeHandle prompt op recorded %d invocation telemetry datapoints, want 0 (RuntimeHandle is permanently excluded — ga-tkvb31)", runtimeDatapoints)).
			WithEvidence(evidence)
	}
	return Pass(telemetryHandleProfile, RequirementInvocationUsageRecording,
		"SessionHandle prompt op recorded gc.agent.tokens.*; RuntimeHandle prompt op recorded nothing").
		WithEvidence(evidence)
}

// sessionHandleRecordedInputTokens drives a transcript-backed SessionHandle
// prompt op and returns the recorded gc.agent.tokens.input total.
func sessionHandleRecordedInputTokens(t *testing.T) int64 {
	t.Helper()

	reader := newInvocationMetricsReader(t)

	searchBase := t.TempDir()
	workDir := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)

	handle, err := worker.NewSessionHandle(worker.SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchBase},
		Session: worker.SessionSpec{
			Profile:  worker.ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "claude",
			WorkDir:  workDir,
			Provider: "claude",
			Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	transcriptPath := claudeTranscriptPath(t, store, manager, searchBase, workDir)
	writeUsageJSONL(t, transcriptPath, []map[string]any{
		claudeUsageEntry("u1", "claude-opus-4-7", 100, 50, 2000, 800),
	})

	if _, err := handle.Message(context.Background(), worker.MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	total, _ := invocationInt64Total(collectInvocationMetrics(t, reader), "gc.agent.tokens.input")
	return total
}

// runtimeHandleInvocationDatapoints drives a runtime-only RuntimeHandle prompt
// op and returns the number of invocation telemetry datapoints it recorded.
func runtimeHandleInvocationDatapoints(t *testing.T) int {
	t.Helper()

	reader := newInvocationMetricsReader(t)

	sp := runtime.NewFake()
	factory, err := worker.NewFactory(worker.FactoryConfig{
		Provider: sp,
		Recorder: events.NewFake(),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	const sessionName = "runtime-only-telemetry-probe"
	handle, err := factory.RuntimeHandle(sessionName, "claude", "tmux-cli", []string{"claude"})
	if err != nil {
		t.Fatalf("RuntimeHandle: %v", err)
	}

	ctx := context.Background()
	if err := sp.Start(ctx, sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := handle.Message(ctx, worker.MessageRequest{Text: "summarize the worker contract"}); err != nil {
		t.Fatalf("Message: %v", err)
	}
	if _, err := handle.Nudge(ctx, worker.NudgeRequest{Text: "still there?"}); err != nil {
		t.Fatalf("Nudge: %v", err)
	}

	return invocationDatapointCount(collectInvocationMetrics(t, reader))
}

// claudeTranscriptPath resolves the provider-native claude transcript path the
// started session handle will discover, mirroring the worker package's
// session-key + project-slug layout via the canonical ProjectSlug helper.
func claudeTranscriptPath(t *testing.T, store *beads.MemStore, manager *sessionpkg.Manager, searchBase, workDir string) string {
	t.Helper()

	sessions, err := store.List(beads.ListQuery{Label: sessionpkg.LabelSession})
	if err != nil {
		t.Fatalf("List sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("started session beads = %d, want 1 (fresh store, single handle)", len(sessions))
	}
	info, err := manager.Get(sessions[0].ID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", sessions[0].ID, err)
	}

	// A fresh session has no provider resume key yet, so the keyed transcript
	// lookup misses and discovery falls back to the unambiguous claude slug-dir
	// scan. Mirror the worker package's harness, which writes the transcript at
	// <slug>/<SessionKey>.jsonl (an empty key yields the scanned .jsonl file).
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	return filepath.Join(slugDir, info.SessionKey+".jsonl")
}

// claudeUsageEntry mirrors one assistant transcript entry carrying response
// token usage in the claude transcript shape.
func claudeUsageEntry(uuid, model string, input, output, cacheRead, cacheCreation int) map[string]any {
	return map[string]any{
		"type": "assistant",
		"uuid": uuid,
		"message": map[string]any{
			"role":  "assistant",
			"model": model,
			"usage": map[string]any{
				"input_tokens":                input,
				"output_tokens":               output,
				"cache_read_input_tokens":     cacheRead,
				"cache_creation_input_tokens": cacheCreation,
			},
		},
	}
}

// writeUsageJSONL writes each entry as one JSON line to path.
func writeUsageJSONL(t *testing.T, path string, lines []map[string]any) {
	t.Helper()

	var b strings.Builder
	writer := bufio.NewWriter(&b)
	encoder := json.NewEncoder(writer)
	for _, line := range lines {
		if err := encoder.Encode(line); err != nil {
			t.Fatalf("encode usage line: %v", err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("flush usage lines: %v", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

// newInvocationMetricsReader rebinds the lazy telemetry instruments to a
// manual-reader MeterProvider for the duration of the test, mirroring the
// worker package's invocation telemetry test harness.
func newInvocationMetricsReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()

	telemetry.ResetInstrumentsForTest()
	t.Cleanup(telemetry.ResetInstrumentsForTest)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
	})
	return reader
}

// collectInvocationMetrics collects the current metric snapshot.
func collectInvocationMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()

	var out metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return out
}

// invocationInt64Total sums the int64 counter datapoints for name and returns
// the number of datapoints observed.
func invocationInt64Total(out metricdata.ResourceMetrics, name string) (int64, int) {
	var total int64
	count := 0
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
				count++
			}
		}
	}
	return total, count
}

// invocationDatapointCount counts datapoints across the invocation instruments
// only (gc.agent.tokens.*, gc.agent.invocation.*), so unrelated telemetry such
// as gc.agent.starts cannot leak into absence assertions.
func invocationDatapointCount(out metricdata.ResourceMetrics) int {
	count := 0
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			if !strings.HasPrefix(m.Name, "gc.agent.tokens.") &&
				!strings.HasPrefix(m.Name, "gc.agent.invocation.") {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				count += len(data.DataPoints)
			case metricdata.Sum[float64]:
				count += len(data.DataPoints)
			case metricdata.Histogram[float64]:
				count += len(data.DataPoints)
			}
		}
	}
	return count
}
