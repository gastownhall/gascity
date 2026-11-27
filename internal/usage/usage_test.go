package usage

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestModelIdempotencyKeyDeterministicAndDistinct(t *testing.T) {
	a := ModelIdempotencyKey("run-1", "msg-abc")
	b := ModelIdempotencyKey("run-1", "msg-abc")
	if a != b {
		t.Fatalf("same inputs must yield same key: %q != %q", a, b)
	}
	if a == ModelIdempotencyKey("run-1", "msg-xyz") {
		t.Fatal("different message id must yield a different key")
	}
	if a == ModelIdempotencyKey("run-2", "msg-abc") {
		t.Fatal("different run must yield a different key")
	}
	// hex sha256 is 64 chars.
	if len(a) != 64 {
		t.Fatalf("expected 64-char hex key, got %d", len(a))
	}
}

func TestComputeIdempotencyKeyDeterministicAndDistinct(t *testing.T) {
	a := ComputeIdempotencyKey("run-1", "s-1", "epoch-1")
	if a != ComputeIdempotencyKey("run-1", "s-1", "epoch-1") {
		t.Fatal("same inputs must yield same key")
	}
	if a == ComputeIdempotencyKey("run-1", "s-1", "epoch-2") {
		t.Fatal("different awake epoch must yield a different key (no double-count across intervals)")
	}
}

func TestModelAndComputeKeysDoNotCollide(t *testing.T) {
	// A model key built from (run, reqID) must not equal a compute key built
	// from the same first two components plus an epoch.
	if ModelIdempotencyKey("run-1", "s-1") == ComputeIdempotencyKey("run-1", "s-1", "") {
		t.Fatal("model and compute keyspaces must not collide")
	}
}

func TestUsageFactJSONRoundTrip(t *testing.T) {
	in := Fact{
		RunID: "run-1", SessionID: "session-1", StepID: "bead-9", Worker: "s-bead-9", City: "demo",
		FormulaName: "my-formula",
		Kind:        KindModel,
		Upstream:    "manifold", Model: "coder", Backing: "claude-opus-4-8", Provider: "anthropic",
		InputTokens: 100, OutputTokens: 200, ReasoningTokens: 7, CacheReadTokens: 50, CacheCreationTokens: 10,
		UsageStart: 1_700_000_000_000, UsageEnd: 1_700_000_030_000,
		RuntimeGeneration: 4, RuntimeEpoch: 2, AccountRef: "acct-opaque",
		CostUSDEstimate: 0.0042, Unpriced: false,
		UpstreamReqID: "msg-abc", At: 1_700_000_000_000,
		IdempotencyKey: ModelIdempotencyKey("run-1", "msg-abc"),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Fact
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestUnpricedFactKeepsTokensZeroCost(t *testing.T) {
	// An unpriced model fact must retain its tokens while reporting no cost.
	f := Fact{Kind: KindModel, InputTokens: 123, Unpriced: true}
	if f.CostUSDEstimate != 0 {
		t.Fatal("unpriced fact must have zero cost estimate")
	}
	if f.InputTokens == 0 {
		t.Fatal("unpriced fact must keep its token counts")
	}
	b, _ := json.Marshal(f)
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["cost_usd_estimate"]; ok {
		t.Fatal("zero cost should be omitted from JSON (omitempty)")
	}
	if got["unpriced"] != true {
		t.Fatal("unpriced flag must be present so rollups can exclude it")
	}
}

func TestUsageFactOmitsUnknownSpanAndIdentity(t *testing.T) {
	b, err := json.Marshal(Fact{Kind: KindModel, InputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"reasoning_tokens", "usage_start", "usage_end",
		"runtime_generation", "runtime_epoch", "account_ref",
	} {
		if strings.Contains(string(b), key) {
			t.Errorf("zero %s must be omitted, got %s", key, b)
		}
	}
}

func TestTotalsIgnoreSpanAndReasoning(t *testing.T) {
	var got Totals
	got.Add(Fact{
		Kind:                KindModel,
		InputTokens:         10,
		OutputTokens:        4,
		CacheReadTokens:     3,
		CacheCreationTokens: 2,
		ReasoningTokens:     99,
		UsageStart:          1000,
		UsageEnd:            2000,
		RuntimeGeneration:   5,
		RuntimeEpoch:        6,
		AccountRef:          "acct",
	})
	want := Totals{
		Invocations:         1,
		InputTokens:         10,
		OutputTokens:        4,
		CacheReadTokens:     3,
		CacheCreationTokens: 2,
	}
	if got != want {
		t.Fatalf("Add folded span or reasoning into totals: got %+v, want %+v", got, want)
	}
}

func TestDiscardSink(t *testing.T) {
	if err := Discard.Record(context.Background(), Fact{Kind: KindModel}); err != nil {
		t.Fatalf("Discard.Record must never error: %v", err)
	}
}
