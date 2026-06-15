package worker

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/usage"
)

func TestModelUsageFactFromPayload(t *testing.T) {
	// No token data → no fact (the state until #3442 wires token capture).
	if _, ok := modelUsageFactFromPayload(operationEventPayload{OpID: "op1", RunID: "r1"}); ok {
		t.Fatal("no token usage must yield ok=false")
	}

	up := true
	p := operationEventPayload{
		OpID:             "op2",
		RunID:            "run-x",
		BeadID:           "bead-9",
		SessionName:      "s-x",
		Model:            "opus",
		Provider:         "anthropic",
		PromptTokens:     100,
		CompletionTokens: 50,
		CacheReadTokens:  10,
		CostUSDEstimate:  0.02,
		Unpriced:         &up,
		FinishedAt:       time.Unix(1, 0),
	}
	f, ok := modelUsageFactFromPayload(p)
	if !ok {
		t.Fatal("token usage present must yield a fact")
	}
	if f.Kind != usage.KindModel {
		t.Fatalf("kind = %q", f.Kind)
	}
	if f.RunID != "run-x" || f.StepID != "bead-9" || f.Worker != "s-x" {
		t.Fatalf("identity wrong: %+v", f)
	}
	if f.InputTokens != 100 || f.OutputTokens != 50 || f.CacheReadTokens != 10 {
		t.Fatalf("tokens wrong: %+v", f)
	}
	if f.Model != "opus" || f.Provider != "anthropic" || f.CostUSDEstimate != 0.02 {
		t.Fatalf("model/cost wrong: %+v", f)
	}
	if !f.Unpriced {
		t.Fatal("unpriced flag must propagate")
	}
	if f.IdempotencyKey == "" || f.UpstreamReqID != "op2" {
		t.Fatalf("key/req wrong: %+v", f)
	}
}
