package usage

import (
	"context"
	"encoding/json"
	"reflect"
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
		RunID: "run-1", StepID: "bead-9", Worker: "s-bead-9", City: "demo",
		Kind:     KindModel,
		Upstream: "manifold", Model: "coder", Backing: "claude-opus-4-8", Provider: "anthropic",
		InputTokens: 100, OutputTokens: 200, CacheReadTokens: 50, CacheCreationTokens: 10,
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

func TestDiscardSink(t *testing.T) {
	if err := Discard.Record(context.Background(), Fact{Kind: KindModel}); err != nil {
		t.Fatalf("Discard.Record must never error: %v", err)
	}
}
