package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/pricing"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// setupInvocationMetricsReader rebinds the lazy telemetry instruments to a
// manual-reader MeterProvider for the duration of the test. Mirrors
// telemetry/recorder_invocation_test.go.
func setupInvocationMetricsReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	telemetry.ResetInstrumentsForTest()
	t.Cleanup(telemetry.ResetInstrumentsForTest)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prevProvider := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prevProvider)
	})
	return reader
}

// newInvocationTelemetryHandle builds a started session handle whose
// transcript lives under a search-path root, plus the resolved transcript
// path the test should write usage entries to.
func newInvocationTelemetryHandle(t *testing.T) (*SessionHandle, *beads.MemStore, string) {
	t.Helper()
	searchBase := t.TempDir()
	workDir := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchBase},
		Session: SessionSpec{
			Profile:  ProfileClaudeTmuxCLI,
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
	info, err := manager.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("Get(%q): %v", handle.sessionID, err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	return handle, store, filepath.Join(slugDir, info.SessionKey+".jsonl")
}

func usageEntry(uuid, model string, input, output, cacheRead, cacheCreation int) map[string]any {
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

// usageEntryWithMessageID mirrors the real Claude transcript shape: one
// assistant entry per content block, each carrying the shared message.id and
// an identical copy of the response usage.
func usageEntryWithMessageID(uuid, messageID string, input, output, cacheRead, cacheCreation int) map[string]any {
	entry := usageEntry(uuid, "claude-opus-4-7", input, output, cacheRead, cacheCreation)
	entry["message"].(map[string]any)["id"] = messageID
	return entry
}

func collectInvocationMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var out metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return out
}

// invocationInt64Total sums the int64 counter datapoints for name and
// returns the attribute sets observed.
func invocationInt64Total(out metricdata.ResourceMetrics, name string) (int64, []map[attribute.Key]string) {
	var total int64
	var attrSets []map[attribute.Key]string
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
				attrs := make(map[attribute.Key]string)
				for _, kv := range dp.Attributes.ToSlice() {
					attrs[kv.Key] = kv.Value.AsString()
				}
				attrSets = append(attrSets, attrs)
			}
		}
	}
	return total, attrSets
}

// invocationFloat64Total sums the float64 counter datapoints for name and
// returns the number of datapoints seen.
func invocationFloat64Total(out metricdata.ResourceMetrics, name string) (float64, int) {
	var total float64
	count := 0
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[float64])
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

// invocationDatapointCount counts datapoints across the invocation
// instruments only (gc.agent.tokens.*, gc.agent.invocation.*). Other
// telemetry — e.g. the gc.agent.starts counter that handle.Start() emits
// when the general instruments happen to bind to the test reader — must not
// leak into absence assertions, or the tests become order-dependent.
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

func TestMessageRecordsInvocationTokensAndCost(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, _, transcriptPath := newInvocationTelemetryHandle(t)

	// Two completed invocations in the tail; with no persisted cursor only
	// the newest (u2) must be recorded — never the whole historical tail.
	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "claude-opus-4-7", 999, 999, 999, 999),
		usageEntry("u2", "claude-opus-4-7", 100, 50, 2000, 800),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	wantTokens := map[string]int64{
		"gc.agent.tokens.input":          100,
		"gc.agent.tokens.output":         50,
		"gc.agent.tokens.cache_read":     2000,
		"gc.agent.tokens.cache_creation": 800,
	}
	for name, want := range wantTokens {
		got, attrSets := invocationInt64Total(out, name)
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
		if len(attrSets) != 1 {
			t.Errorf("%s: %d datapoints, want 1", name, len(attrSets))
			continue
		}
		attrs := attrSets[0]
		if got := attrs["agent_name"]; got != "myrig/polecat-1" {
			t.Errorf("%s: agent_name = %q, want myrig/polecat-1", name, got)
		}
		if got := attrs["model"]; got != "claude-opus-4-7" {
			t.Errorf("%s: model = %q, want claude-opus-4-7", name, got)
		}
		if got := attrs["provider"]; got != "claude" {
			t.Errorf("%s: provider = %q, want claude", name, got)
		}
		if len(attrs) != 3 {
			t.Errorf("%s: unexpected attribute set %+v", name, attrs)
		}
	}

	wantCost, ok := pricing.BuildRegistry(nil, nil).Estimate("claude", "claude-opus-4-7", pricing.Usage{
		PromptTokens:        100,
		CompletionTokens:    50,
		CacheReadTokens:     2000,
		CacheCreationTokens: 800,
	})
	if !ok {
		t.Fatal("default pricing registry has no claude-opus-4-7 entry; fix the test fixture")
	}
	gotCost, costDPs := invocationFloat64Total(out, "gc.agent.invocation.cost_usd")
	if costDPs != 1 {
		t.Fatalf("gc.agent.invocation.cost_usd: %d datapoints, want 1", costDPs)
	}
	if gotCost != wantCost {
		t.Errorf("gc.agent.invocation.cost_usd = %v, want %v", gotCost, wantCost)
	}
}

func TestMessageAdvancesCursorAndSumsNewEntries(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, store, transcriptPath := newInvocationTelemetryHandle(t)

	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "claude-opus-4-7", 999, 999, 999, 999),
		usageEntry("u2", "claude-opus-4-7", 100, 50, 2000, 800),
	})
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "first"}); err != nil {
		t.Fatalf("Message(first): %v", err)
	}

	// Two more invocations complete; the next prompt op must record exactly
	// the new entries (u3+u4), not re-count u2.
	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "claude-opus-4-7", 999, 999, 999, 999),
		usageEntry("u2", "claude-opus-4-7", 100, 50, 2000, 800),
		usageEntry("u3", "claude-opus-4-7", 10, 5, 200, 80),
		usageEntry("u4", "claude-opus-4-7", 1, 2, 3, 4),
	})
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "second"}); err != nil {
		t.Fatalf("Message(second): %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	wantTokens := map[string]int64{
		"gc.agent.tokens.input":          100 + 10 + 1,
		"gc.agent.tokens.output":         50 + 5 + 2,
		"gc.agent.tokens.cache_read":     2000 + 200 + 3,
		"gc.agent.tokens.cache_creation": 800 + 80 + 4,
	}
	for name, want := range wantTokens {
		if got, _ := invocationInt64Total(out, name); got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}

	bead, err := store.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := bead.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "u4" {
		t.Fatalf("cursor metadata = %q, want u4", got)
	}

	// Third prompt op with no new entries must not change the totals.
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "third"}); err != nil {
		t.Fatalf("Message(third): %v", err)
	}
	out = collectInvocationMetrics(t, reader)
	for name, want := range wantTokens {
		if got, _ := invocationInt64Total(out, name); got != want {
			t.Errorf("after no-op message: %s = %d, want %d (double-counted)", name, got, want)
		}
	}
}

// TestMessageDoesNotRecountSplitContentBlockGroups pins single-counting of
// one API invocation whose content-block entries straddle prompt-operation
// boundaries: a prompt op can observe the first blocks of a response, and a
// later op the remaining blocks of the SAME message.id. The cursor must
// track the invocation identity, not the entry uuid, or the later op
// re-records the invocation it already counted.
func TestMessageDoesNotRecountSplitContentBlockGroups(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, store, transcriptPath := newInvocationTelemetryHandle(t)

	// First prompt op lands mid-write: two of msg_A's block entries exist.
	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntryWithMessageID("b1", "msg_A", 100, 50, 2000, 800),
		usageEntryWithMessageID("b2", "msg_A", 100, 50, 2000, 800),
	})
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "first"}); err != nil {
		t.Fatalf("Message(first): %v", err)
	}

	wantTokens := map[string]int64{
		"gc.agent.tokens.input":          100,
		"gc.agent.tokens.output":         50,
		"gc.agent.tokens.cache_read":     2000,
		"gc.agent.tokens.cache_creation": 800,
	}
	out := collectInvocationMetrics(t, reader)
	for name, want := range wantTokens {
		if got, _ := invocationInt64Total(out, name); got != want {
			t.Errorf("after split group: %s = %d, want %d (content blocks double-counted)", name, got, want)
		}
	}
	if _, costDPs := invocationFloat64Total(out, "gc.agent.invocation.cost_usd"); costDPs != 1 {
		t.Errorf("gc.agent.invocation.cost_usd: %d datapoints, want 1", costDPs)
	}

	// msg_A's final block lands after the cursor was persisted. The next
	// prompt op must NOT re-record msg_A.
	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntryWithMessageID("b1", "msg_A", 100, 50, 2000, 800),
		usageEntryWithMessageID("b2", "msg_A", 100, 50, 2000, 800),
		usageEntryWithMessageID("b3", "msg_A", 100, 50, 2000, 800),
	})
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "second"}); err != nil {
		t.Fatalf("Message(second): %v", err)
	}
	out = collectInvocationMetrics(t, reader)
	for name, want := range wantTokens {
		if got, _ := invocationInt64Total(out, name); got != want {
			t.Errorf("after late block of same message: %s = %d, want %d (invocation re-recorded across cursor boundary)", name, got, want)
		}
	}

	// A genuinely new invocation is still recorded, and the cursor advances
	// to its message identity.
	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntryWithMessageID("b1", "msg_A", 100, 50, 2000, 800),
		usageEntryWithMessageID("b2", "msg_A", 100, 50, 2000, 800),
		usageEntryWithMessageID("b3", "msg_A", 100, 50, 2000, 800),
		usageEntryWithMessageID("b4", "msg_B", 10, 5, 200, 80),
	})
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "third"}); err != nil {
		t.Fatalf("Message(third): %v", err)
	}
	out = collectInvocationMetrics(t, reader)
	wantAfterNew := map[string]int64{
		"gc.agent.tokens.input":          100 + 10,
		"gc.agent.tokens.output":         50 + 5,
		"gc.agent.tokens.cache_read":     2000 + 200,
		"gc.agent.tokens.cache_creation": 800 + 80,
	}
	for name, want := range wantAfterNew {
		if got, _ := invocationInt64Total(out, name); got != want {
			t.Errorf("after new invocation: %s = %d, want %d", name, got, want)
		}
	}

	bead, err := store.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := bead.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "msg_B" {
		t.Fatalf("cursor metadata = %q, want msg_B (message identity, not entry uuid)", got)
	}
}

func TestMessageSkipsCostForUnknownModel(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, _, transcriptPath := newInvocationTelemetryHandle(t)

	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "model-not-in-registry", 100, 50, 0, 0),
	})
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	if got, _ := invocationInt64Total(out, "gc.agent.tokens.input"); got != 100 {
		t.Errorf("gc.agent.tokens.input = %d, want 100", got)
	}
	if got, _ := invocationInt64Total(out, "gc.agent.tokens.output"); got != 50 {
		t.Errorf("gc.agent.tokens.output = %d, want 50", got)
	}
	if _, costDPs := invocationFloat64Total(out, "gc.agent.invocation.cost_usd"); costDPs != 0 {
		t.Errorf("gc.agent.invocation.cost_usd has %d datapoints for unknown model, want 0", costDPs)
	}
}

func TestInvocationTelemetrySuppressedContext(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, _, transcriptPath := newInvocationTelemetryHandle(t)

	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "claude-opus-4-7", 100, 50, 2000, 800),
	})
	ctx := WithoutOperationEvents(context.Background())
	if _, err := handle.Message(ctx, MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	if got := invocationDatapointCount(out); got != 0 {
		t.Fatalf("suppressed context emitted %d datapoints, want 0", got)
	}
}

func TestNudgeRecordsInvocationTokens(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, _, transcriptPath := newInvocationTelemetryHandle(t)

	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "claude-opus-4-7", 100, 50, 2000, 800),
	})
	if _, err := handle.Nudge(context.Background(), NudgeRequest{Text: "go"}); err != nil {
		t.Fatalf("Nudge: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	wantTokens := map[string]int64{
		"gc.agent.tokens.input":          100,
		"gc.agent.tokens.output":         50,
		"gc.agent.tokens.cache_read":     2000,
		"gc.agent.tokens.cache_creation": 800,
	}
	for name, want := range wantTokens {
		if got, _ := invocationInt64Total(out, name); got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
}

// TestNoLatencyMetricEmitted pins the documented deferral: no measured
// per-invocation latency source exists, and the wrapping operation's
// DurationMs is explicitly excluded by RecordInvocationLatency's contract.
func TestNoLatencyMetricEmitted(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, _, transcriptPath := newInvocationTelemetryHandle(t)

	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "claude-opus-4-7", 100, 50, 2000, 800),
	})
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gc.agent.invocation.latency_ms" {
				continue
			}
			if hist, ok := m.Data.(metricdata.Histogram[float64]); ok && len(hist.DataPoints) > 0 {
				t.Fatalf("gc.agent.invocation.latency_ms emitted %d datapoints; latency wiring is deferred — do not record wrapper-operation durations", len(hist.DataPoints))
			}
		}
	}
}

// TestInvocationTelemetrySkipsNonKeyedProviders pins the claude-family
// gate: non-claude provider families (codex here) must not trigger
// workdir-based discovery scans inside a prompt operation — those walk real
// date-tree session stores (multi-second on developer machines) and risk
// attaching to an unrelated transcript that merely shares the workdir.
func TestInvocationTelemetrySkipsNonKeyedProviders(t *testing.T) {
	reader := setupInvocationMetricsReader(t)

	searchBase := t.TempDir()
	workDir := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchBase},
		Session: SessionSpec{
			Profile:  ProfileCodexTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "codex",
			WorkDir:  workDir,
			Provider: "codex",
			Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A codex date-tree transcript discoverable by workdir scanning. If the
	// hook ever falls back to non-keyed discovery, this file is found and
	// its (claude-shaped) usage entry leaks into the metrics.
	dayDir := filepath.Join(searchBase, "2026", "06", "12")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dayDir, err)
	}
	writeWorkerTestJSONL(t, filepath.Join(dayDir, "rollout-test.jsonl"), []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"cwd": workDir}},
		usageEntry("cx1", "codex-model", 100, 50, 0, 0),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	if got := invocationDatapointCount(out); got != 0 {
		t.Fatalf("non-keyed provider emitted %d datapoints, want 0 (workdir-based discovery must not run)", got)
	}
}

func TestMessageWithoutTranscriptEmitsNothing(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, store, _ := newInvocationTelemetryHandle(t)

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	if got := invocationDatapointCount(out); got != 0 {
		t.Fatalf("no-transcript message emitted %d datapoints, want 0", got)
	}
	bead, err := store.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := bead.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "" {
		t.Fatalf("cursor metadata = %q, want empty", got)
	}
}
