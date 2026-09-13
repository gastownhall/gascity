package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestRuntimeHandleExcludedFromInvocationTelemetry enforces the RuntimeHandle
// half of worker conformance requirement WC-USAGE-RECORD-001
// (workertest.RequirementInvocationUsageRecording): a prompt operation on a
// runtime-only handle records NO invocation telemetry (gc.agent.tokens.* /
// gc.agent.invocation.*). Runtime-only sessions have no transcript adapter,
// no session bead for the cursor, and no agent identity, so the exclusion is
// permanent (ga-tkvb31). The SessionHandle half of the same requirement — a
// prompt op DOES record gc.agent.tokens.* — is enforced by
// TestMessageRecordsInvocationTokensAndCost.
func TestRuntimeHandleExcludedFromInvocationTelemetry(t *testing.T) {
	reader := setupInvocationMetricsReader(t)

	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
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

	if _, err := handle.Message(ctx, MessageRequest{Text: "summarize the worker contract"}); err != nil {
		t.Fatalf("Message: %v", err)
	}
	if _, err := handle.Nudge(ctx, NudgeRequest{Text: "still there?"}); err != nil {
		t.Fatalf("Nudge: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	if got := invocationDatapointCount(out); got != 0 {
		t.Fatalf("runtime-only prompt ops recorded %d invocation telemetry datapoints, want 0 (RuntimeHandle is permanently excluded — ga-tkvb31)", got)
	}
}
