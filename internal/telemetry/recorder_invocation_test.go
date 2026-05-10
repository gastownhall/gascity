package telemetry

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func resetInvocationInstruments(t *testing.T) {
	t.Helper()
	invInstOnce = sync.Once{}
	t.Cleanup(func() { invInstOnce = sync.Once{} })
}

func TestInvocationLabels_OTelAttributes(t *testing.T) {
	labels := InvocationLabels{
		AgentName: "rig/polecat-1",
		Model:     "claude-opus-4-7",
		Provider:  "claude",
	}
	attrs := labels.toOTel()
	if len(attrs) != 3 {
		t.Fatalf("toOTel() = %d attrs, want 3", len(attrs))
	}
	want := map[attribute.Key]string{
		"agent_name": "rig/polecat-1",
		"model":      "claude-opus-4-7",
		"provider":   "claude",
	}
	for _, a := range attrs {
		expected, ok := want[a.Key]
		if !ok {
			t.Errorf("unexpected attr key: %s", a.Key)
			continue
		}
		if got := a.Value.AsString(); got != expected {
			t.Errorf("attr %s: got %q want %q", a.Key, got, expected)
		}
	}
}

// TestRecordInvocationTokensNoPanicOnZeros verifies the helpers no-op
// gracefully when nothing to record. Mirrors the pattern used by the
// existing Record* tests.
func TestRecordInvocationTokensNoPanicOnZeros(t *testing.T) {
	resetInvocationInstruments(t)
	ctx := context.Background()
	RecordInvocationTokens(ctx, InvocationLabels{
		AgentName: "x", Model: "y", Provider: "z",
	}, 0, 0, 0, 0)
}

func TestRecordInvocationTokensSmokeTest(t *testing.T) {
	resetInvocationInstruments(t)
	ctx := context.Background()
	RecordInvocationTokens(ctx, InvocationLabels{
		AgentName: "rig/polecat-1", Model: "claude-opus-4-7", Provider: "claude",
	}, 100, 50, 2000, 800)
}

func TestRecordInvocationLatencyIgnoresNonPositive(t *testing.T) {
	resetInvocationInstruments(t)
	ctx := context.Background()
	// Should not panic — recorder ignores values <= 0.
	RecordInvocationLatency(ctx, InvocationLabels{
		AgentName: "x", Model: "y", Provider: "z",
	}, 0)
	RecordInvocationLatency(ctx, InvocationLabels{
		AgentName: "x", Model: "y", Provider: "z",
	}, -1)
}

func TestRecordInvocationCostEstimateIgnoresNonPositive(t *testing.T) {
	resetInvocationInstruments(t)
	ctx := context.Background()
	RecordInvocationCostEstimate(ctx, InvocationLabels{
		AgentName: "x", Model: "y", Provider: "z",
	}, 0)
	RecordInvocationCostEstimate(ctx, InvocationLabels{
		AgentName: "x", Model: "y", Provider: "z",
	}, -0.5)
}

// TestInvocationInstrumentsActuallyRegisterValues uses a manual SDK
// MeterProvider to confirm the instruments emit observations with the
// correct names and attribute set. The no-op-provider tests above guard
// against panics; this one guards against silent registration failures.
func TestInvocationInstrumentsActuallyRegisterValues(t *testing.T) {
	resetInvocationInstruments(t)

	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	prevProvider := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prevProvider)
	})

	ctx := context.Background()
	labels := InvocationLabels{
		AgentName: "rig/polecat-1",
		Model:     "claude-opus-4-7",
		Provider:  "claude",
	}
	RecordInvocationTokens(ctx, labels, 100, 50, 2000, 800)
	RecordInvocationLatency(ctx, labels, 1234.5)
	RecordInvocationCostEstimate(ctx, labels, 0.0123)

	var out metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &out); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	want := map[string]bool{
		"gc.agent.tokens.input":          false,
		"gc.agent.tokens.output":         false,
		"gc.agent.tokens.cache_read":     false,
		"gc.agent.tokens.cache_creation": false,
		"gc.agent.invocation.latency_ms": false,
		"gc.agent.invocation.cost_usd":   false,
	}
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			if _, ok := want[m.Name]; ok {
				want[m.Name] = true
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("instrument %q not registered/observed", name)
		}
	}
}

// TestInvocationInstrumentsCarryExpectedAttributes confirms the
// {agent_name, model, provider} tag set is the only set on every
// instrument — no leaked bead_id or prompt_sha would explode cardinality.
func TestInvocationInstrumentsCarryExpectedAttributes(t *testing.T) {
	resetInvocationInstruments(t)

	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	prevProvider := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prevProvider)
	})

	ctx := context.Background()
	labels := InvocationLabels{
		AgentName: "agentA", Model: "modelB", Provider: "providerC",
	}
	RecordInvocationTokens(ctx, labels, 100, 50, 2000, 800)

	var out metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &out); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	checked := 0
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				keys := make(map[attribute.Key]string)
				for _, kv := range dp.Attributes.ToSlice() {
					keys[kv.Key] = kv.Value.AsString()
				}
				if got := keys["agent_name"]; got != "agentA" {
					t.Errorf("%s: agent_name = %q", m.Name, got)
				}
				if got := keys["model"]; got != "modelB" {
					t.Errorf("%s: model = %q", m.Name, got)
				}
				if got := keys["provider"]; got != "providerC" {
					t.Errorf("%s: provider = %q", m.Name, got)
				}
				if len(keys) != 3 {
					t.Errorf("%s: unexpected attributes: %+v", m.Name, keys)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no token-counter datapoints inspected; verify SDK setup")
	}
}
