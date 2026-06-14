package worker

import (
	"context"
	"crypto/md5" //nolint:gosec // Kimi keys its session store by workdir MD5.
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// invocationCostTotal sums the gc.agent.invocation.cost_usd counter
// datapoints and returns the number of datapoints seen.
func invocationCostTotal(out metricdata.ResourceMetrics) (float64, int) {
	var total float64
	count := 0
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gc.agent.invocation.cost_usd" {
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
	gotCost, costDPs := invocationCostTotal(out)
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
	if _, costDPs := invocationCostTotal(out); costDPs != 1 {
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
	if _, costDPs := invocationCostTotal(out); costDPs != 0 {
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

// newFamilyTelemetryHandle builds a started session handle for an arbitrary
// provider family, returning the handle, the bead store, the search-path
// root, and the session workdir. Family-specific transcript fixtures are
// laid out by the callers.
func newFamilyTelemetryHandle(t *testing.T, profile Profile, provider, command string, reg *pricing.Registry) (*SessionHandle, *beads.MemStore, string, string) {
	t.Helper()
	searchBase := t.TempDir()
	workDir := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchBase},
		Pricing:     reg,
		Session: SessionSpec{
			Profile:  profile,
			Template: "probe",
			Title:    "Probe",
			Command:  command,
			WorkDir:  workDir,
			Provider: provider,
			Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return handle, store, searchBase, workDir
}

// codexWorkerSessionMeta mirrors the first line of a real codex rollout.
func codexWorkerSessionMeta(cwd string) map[string]any {
	return map[string]any{
		"timestamp": "2026-06-12T10:00:00.000Z",
		"type":      "session_meta",
		"payload": map[string]any{
			"id": "019d9845-4273-7ee3-a7d7-15b71ec6f096", "cwd": cwd,
			"originator": "codex-tui", "cli_version": "0.121.0",
			"source": "cli", "model_provider": "openai",
		},
	}
}

func codexWorkerTurnContext() map[string]any {
	return map[string]any{
		"timestamp": "2026-06-12T10:00:01.000Z",
		"type":      "turn_context",
		"payload":   map[string]any{"turn_id": "t-1", "model": "gpt-5.5", "approval_policy": "never"},
	}
}

// codexWorkerTokenCount mirrors a real token_count event: cumulative
// total_token_usage plus per-call last_token_usage.
func codexWorkerTokenCount(ts string, total, lastInput, lastCached, lastOutput int) map[string]any {
	usage := func(input, cached, output, totalTokens int) map[string]any {
		return map[string]any{
			"input_tokens": input, "cached_input_tokens": cached,
			"output_tokens": output, "reasoning_output_tokens": 0,
			"total_tokens": totalTokens,
		}
	}
	return map[string]any{
		"timestamp": ts,
		"type":      "event_msg",
		"payload": map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"total_token_usage": usage(total-lastOutput, lastCached, lastOutput, total),
				"last_token_usage":  usage(lastInput, lastCached, lastOutput, lastInput+lastOutput),
			},
		},
	}
}

// codexWorkerRolloutPath returns the rollout path the codex CLI would create
// at ts: local-date day dir, local-time filename timestamp.
func codexWorkerRolloutPath(t *testing.T, searchBase string, ts time.Time) string {
	t.Helper()
	return codexWorkerRolloutPathWithID(t, searchBase, ts, "019d9845-aaaa-7000-8000-000000000001")
}

// codexWorkerRolloutPathWithID is codexWorkerRolloutPath with an explicit
// session uuid suffix, for tests keying discovery off the bead session_key.
func codexWorkerRolloutPathWithID(t *testing.T, searchBase string, ts time.Time, uuid string) string {
	t.Helper()
	local := ts.In(time.Local)
	dir := filepath.Join(searchBase, local.Format("2006"), local.Format("01"), local.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	return filepath.Join(dir, "rollout-"+local.Format("2006-01-02T15-04-05")+"-"+uuid+".jsonl")
}

func TestMessageRecordsCodexInvocationTokens(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, _, searchBase, workDir := newFamilyTelemetryHandle(t, ProfileCodexTmuxCLI, "codex", "codex", nil)

	// Two completed API calls in the rollout; with no persisted cursor only
	// the newest must be recorded.
	writeWorkerTestJSONL(t, codexWorkerRolloutPath(t, searchBase, time.Now()), []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-12T10:00:05.000Z", 15917, 15562, 10624, 355),
		codexWorkerTokenCount("2026-06-12T10:00:10.000Z", 34114, 17888, 15232, 309),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	wantTokens := map[string]int64{
		"gc.agent.tokens.input":      17888 - 15232,
		"gc.agent.tokens.output":     309,
		"gc.agent.tokens.cache_read": 15232,
	}
	for name, want := range wantTokens {
		got, attrSets := invocationInt64Total(out, name)
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
		if len(attrSets) != 1 {
			t.Errorf("%s: %d datapoint attribute sets, want 1", name, len(attrSets))
			continue
		}
		attrs := attrSets[0]
		if got := attrs["agent_name"]; got != "myrig/polecat-1" {
			t.Errorf("%s: agent_name = %q, want myrig/polecat-1", name, got)
		}
		if got := attrs["model"]; got != "gpt-5.5" {
			t.Errorf("%s: model = %q, want gpt-5.5 (from turn_context)", name, got)
		}
		if got := attrs["provider"]; got != "codex" {
			t.Errorf("%s: provider = %q, want codex", name, got)
		}
	}
	if got, _ := invocationInt64Total(out, "gc.agent.tokens.cache_creation"); got != 0 {
		t.Errorf("gc.agent.tokens.cache_creation = %d, want 0 (codex reports no cache writes)", got)
	}
	// No codex entries ship in the default pricing registry: tokens flow,
	// cost is skipped — never zero-filled.
	if _, costDPs := invocationCostTotal(out); costDPs != 0 {
		t.Errorf("gc.agent.invocation.cost_usd: %d datapoints, want 0 without configured codex pricing", costDPs)
	}
}

// TestMessageRecordsCodexTokensForResumedSession pins the default codex
// continuation path: gc re-wakes codex sessions with `codex resume
// <session_key>` and the CLI APPENDS to the ORIGINAL rollout, whose filename
// timestamp is the FIRST start — far outside any later wake's discovery
// window. With session_key on the bead, discovery must resolve the rollout
// by its uuid suffix instead of the wake-anchored window, or every resumed
// codex session silently records zero tokens forever.
func TestMessageRecordsCodexTokensForResumedSession(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, store, searchBase, workDir := newFamilyTelemetryHandle(t, ProfileCodexTmuxCLI, "codex", "codex", nil)

	const sessionUUID = "019d9845-bbbb-7000-8000-000000000042"
	// Rollout created ~3h ago (well outside the 10m window plus tolerance),
	// then appended to by the resumed run.
	writeWorkerTestJSONL(t, codexWorkerRolloutPathWithID(t, searchBase, time.Now().Add(-3*time.Hour), sessionUUID), []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-12T10:00:05.000Z", 15917, 15562, 10624, 355),
		codexWorkerTokenCount("2026-06-12T10:00:10.000Z", 34114, 17888, 15232, 309),
	})
	// The codex hook captured the provider session id, and the reconciler
	// recorded the wake that resumed it.
	if err := store.SetMetadata(handle.sessionID, "session_key", sessionUUID); err != nil {
		t.Fatalf("SetMetadata(session_key): %v", err)
	}
	if err := store.SetMetadata(handle.sessionID, "last_woke_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("SetMetadata(last_woke_at): %v", err)
	}

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	wantTokens := map[string]int64{
		"gc.agent.tokens.input":      17888 - 15232,
		"gc.agent.tokens.output":     309,
		"gc.agent.tokens.cache_read": 15232,
	}
	for name, want := range wantTokens {
		if got, _ := invocationInt64Total(out, name); got != want {
			t.Errorf("%s = %d, want %d (resumed-session rollout must be found by session_key)", name, got, want)
		}
	}

	bead, err := store.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := bead.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "total:34114" {
		t.Fatalf("cursor metadata = %q, want total:34114 (cursor must advance on the keyed path)", got)
	}
}

// TestCodexKeyedMissDoesNotFallBackToWindow pins the no-fallback rule: when
// the bead carries a session_key but no rollout with that uuid suffix
// exists, a window-eligible rollout belonging to a DIFFERENT session must
// not be attributed — that would silently bill a foreign session's tokens.
func TestCodexKeyedMissDoesNotFallBackToWindow(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, store, searchBase, workDir := newFamilyTelemetryHandle(t, ProfileCodexTmuxCLI, "codex", "codex", nil)

	// In-window, cwd-matching rollout — but its uuid suffix is not the
	// session_key, so the keyed lookup misses and must stay missed.
	writeWorkerTestJSONL(t, codexWorkerRolloutPath(t, searchBase, time.Now()), []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-12T10:00:10.000Z", 34114, 17888, 15232, 309),
	})
	if err := store.SetMetadata(handle.sessionID, "session_key", "019d9845-cccc-7000-8000-000000000777"); err != nil {
		t.Fatalf("SetMetadata(session_key): %v", err)
	}
	if err := store.SetMetadata(handle.sessionID, "last_woke_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("SetMetadata(last_woke_at): %v", err)
	}

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	if got := invocationDatapointCount(out); got != 0 {
		t.Fatalf("keyed miss emitted %d datapoints, want 0 (no window fallback on the identity path)", got)
	}
}

// TestMessageRecordsCodexTokensFreshWakeWithoutSessionKey pins the un-keyed
// branch with a present last_woke_at: before the codex hook captures the
// session_key, discovery still runs the wake-anchored window search.
func TestMessageRecordsCodexTokensFreshWakeWithoutSessionKey(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, store, searchBase, workDir := newFamilyTelemetryHandle(t, ProfileCodexTmuxCLI, "codex", "codex", nil)

	writeWorkerTestJSONL(t, codexWorkerRolloutPath(t, searchBase, time.Now()), []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-12T10:00:10.000Z", 34114, 17888, 15232, 309),
	})
	if err := store.SetMetadata(handle.sessionID, "last_woke_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("SetMetadata(last_woke_at): %v", err)
	}

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	wantTokens := map[string]int64{
		"gc.agent.tokens.input":      17888 - 15232,
		"gc.agent.tokens.output":     309,
		"gc.agent.tokens.cache_read": 15232,
	}
	for name, want := range wantTokens {
		if got, _ := invocationInt64Total(out, name); got != want {
			t.Errorf("%s = %d, want %d (window search anchored at last_woke_at)", name, got, want)
		}
	}
}

// TestMessageRecordsCodexTokensBehindSymlinkedRoot pins the aimux-managed
// multi-account layout end to end: the rollout lives outside the search root
// and is reachable only through a symlinked extra root (mirroring
// ~/.codex/sessions/aimux-<acct> -> ~/.aimux/codex/<acct>/sessions).
// Discovery must hand the extractor a symlink-LEXICAL path that search-path
// validation accepts; an EvalSymlinks-resolved path is rejected there and
// the session silently records zero tokens forever.
func TestMessageRecordsCodexTokensBehindSymlinkedRoot(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, _, searchBase, workDir := newFamilyTelemetryHandle(t, ProfileCodexTmuxCLI, "codex", "codex", nil)

	target := t.TempDir() // account session store outside the search root
	if err := os.Symlink(target, filepath.Join(searchBase, "aimux-acct")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	writeWorkerTestJSONL(t, codexWorkerRolloutPath(t, target, time.Now()), []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-12T10:00:10.000Z", 34114, 17888, 15232, 309),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	wantTokens := map[string]int64{
		"gc.agent.tokens.input":      17888 - 15232,
		"gc.agent.tokens.output":     309,
		"gc.agent.tokens.cache_read": 15232,
	}
	for name, want := range wantTokens {
		if got, _ := invocationInt64Total(out, name); got != want {
			t.Errorf("%s = %d, want %d (rollout behind symlinked extra root)", name, got, want)
		}
	}
}

func TestCodexCostFlowsWithConfiguredPricing(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	reg := pricing.BuildRegistry([]pricing.ModelPricing{{
		Provider: "codex",
		Model:    "gpt-5.5",
		Tier: pricing.Tier{
			PromptUSDPer1M:     2.00,
			CompletionUSDPer1M: 8.00,
			CacheReadUSDPer1M:  0.20,
		},
	}}, nil)
	handle, _, searchBase, workDir := newFamilyTelemetryHandle(t, ProfileCodexTmuxCLI, "codex", "codex", reg)

	writeWorkerTestJSONL(t, codexWorkerRolloutPath(t, searchBase, time.Now()), []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-12T10:00:10.000Z", 34114, 17888, 15232, 309),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	wantCost, ok := reg.Estimate("codex", "gpt-5.5", pricing.Usage{
		PromptTokens:     17888 - 15232,
		CompletionTokens: 309,
		CacheReadTokens:  15232,
	})
	if !ok {
		t.Fatal("configured registry has no codex:gpt-5.5 entry; fix the test fixture")
	}
	out := collectInvocationMetrics(t, reader)
	gotCost, costDPs := invocationCostTotal(out)
	if costDPs != 1 {
		t.Fatalf("gc.agent.invocation.cost_usd: %d datapoints, want 1", costDPs)
	}
	if gotCost != wantCost {
		t.Errorf("gc.agent.invocation.cost_usd = %v, want %v", gotCost, wantCost)
	}
}

func TestMessageAdvancesCodexCursor(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, store, searchBase, workDir := newFamilyTelemetryHandle(t, ProfileCodexTmuxCLI, "codex", "codex", nil)

	rollout := codexWorkerRolloutPath(t, searchBase, time.Now())
	writeWorkerTestJSONL(t, rollout, []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-12T10:00:05.000Z", 15917, 15562, 10624, 355),
		codexWorkerTokenCount("2026-06-12T10:00:10.000Z", 34114, 17888, 15232, 309),
	})
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "first"}); err != nil {
		t.Fatalf("Message(first): %v", err)
	}

	// One more API call completes; the next prompt op must record exactly
	// the delta entry.
	writeWorkerTestJSONL(t, rollout, []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-12T10:00:05.000Z", 15917, 15562, 10624, 355),
		codexWorkerTokenCount("2026-06-12T10:00:10.000Z", 34114, 17888, 15232, 309),
		codexWorkerTokenCount("2026-06-12T10:00:20.000Z", 56066, 21683, 17792, 269),
	})
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "second"}); err != nil {
		t.Fatalf("Message(second): %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	wantTokens := map[string]int64{
		"gc.agent.tokens.input":      (17888 - 15232) + (21683 - 17792),
		"gc.agent.tokens.output":     309 + 269,
		"gc.agent.tokens.cache_read": 15232 + 17792,
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
	if got := bead.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "total:56066" {
		t.Fatalf("cursor metadata = %q, want total:56066 (cumulative-total identity)", got)
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

// TestCodexTelemetrySkipsOutOfWindowRollouts behaviorally pins the bounded
// codex discovery: a rollout whose filename timestamp falls far outside the
// wake-anchor window must not be found even though its embedded cwd matches
// the session workdir — an unbounded date-tree walk would have found it.
func TestCodexTelemetrySkipsOutOfWindowRollouts(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, _, searchBase, workDir := newFamilyTelemetryHandle(t, ProfileCodexTmuxCLI, "codex", "codex", nil)

	writeWorkerTestJSONL(t, codexWorkerRolloutPath(t, searchBase, time.Now().Add(-72*time.Hour)), []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-09T10:00:10.000Z", 34114, 17888, 15232, 309),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	if got := invocationDatapointCount(out); got != 0 {
		t.Fatalf("out-of-window rollout emitted %d datapoints, want 0 (discovery must stay anchor-bounded)", got)
	}
}

// TestInvocationTelemetrySkipsUnsupportedFamilies pins the remaining
// family gate: provider families without a registered usage extractor (kimi
// here) are skipped before any transcript discovery runs, even when a
// workdir-discoverable transcript with claude-shaped usage entries exists.
func TestInvocationTelemetrySkipsUnsupportedFamilies(t *testing.T) {
	reader := setupInvocationMetricsReader(t)
	handle, _, searchBase, workDir := newFamilyTelemetryHandle(t, ProfileKimiTmuxCLI, "kimi", "kimi", nil)

	// A kimi context transcript discoverable by workdir hashing. If the gate
	// ever admits kimi with generic claude-style wiring, this usage entry
	// leaks into the metrics.
	workHash := md5.Sum([]byte(filepath.Clean(workDir)))
	sessDir := filepath.Join(searchBase, hex.EncodeToString(workHash[:]), "sess-1")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", sessDir, err)
	}
	writeWorkerTestJSONL(t, filepath.Join(sessDir, "context.jsonl"), []map[string]any{
		usageEntry("k1", "kimi-model", 100, 50, 0, 0),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	if got := invocationDatapointCount(out); got != 0 {
		t.Fatalf("unsupported family emitted %d datapoints, want 0 (no discovery may run)", got)
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
