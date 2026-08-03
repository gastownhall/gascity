package worker

import (
	"testing"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// TestNormalizeEntryCarriesModelStopReasonAndDerivedFlag covers three
// normalized-history invariants that previously had no direct assertion: model
// and stop_reason are extracted from the provider message, and the
// Provenance.Derived flag distinguishes provider-supplied identity from
// GC-synthesized identity.
func TestNormalizeEntryCarriesModelStopReasonAndDerivedFlag(t *testing.T) {
	message := mustMarshalStructuredToolTest(t, map[string]any{
		"role":        "assistant",
		"model":       "claude-sonnet-4-6",
		"stop_reason": "end_turn",
		"content": []map[string]any{
			{"type": "text", "text": "done"},
		},
	})

	withID := &sessionlog.Entry{UUID: "entry-1", Type: "assistant", Message: message}
	got := normalizeEntry("claude", "/tmp/x.jsonl", "sess-1", 0, withID)
	if got.Model != "claude-sonnet-4-6" {
		t.Fatalf("Model = %q, want claude-sonnet-4-6", got.Model)
	}
	if got.StopReason != "end_turn" {
		t.Fatalf("StopReason = %q, want end_turn", got.StopReason)
	}
	if got.Provenance.Derived {
		t.Fatal("Provenance.Derived = true, want false for an entry with a provider-supplied UUID")
	}

	// An entry without a UUID is given a synthesized ID and must be flagged as
	// derived so consumers can tell GC-minted identity from provider identity.
	noID := &sessionlog.Entry{Type: "assistant", Message: message}
	derived := normalizeEntry("claude", "/tmp/x.jsonl", "sess-1", 7, noID)
	if !derived.Provenance.Derived {
		t.Fatal("Provenance.Derived = false, want true for an entry without a UUID")
	}
	if derived.ID == "" {
		t.Fatal("derived entry must still receive a synthesized ID")
	}
}

// TestNormalizeEntrySynthesizedToolResultBlockIsDerived covers the block-level
// Derived flag: a tool_result entry that carries no content of its own still
// surfaces as a placeholder tool-result block, and that block must be marked
// Derived so consumers know it was synthesized by GC rather than read verbatim.
func TestNormalizeEntrySynthesizedToolResultBlockIsDerived(t *testing.T) {
	entry := &sessionlog.Entry{
		UUID:      "tr-1",
		Type:      "tool_result",
		ToolUseID: "call-1",
	}
	got := normalizeEntry("claude", "/tmp/x.jsonl", "sess-1", 0, entry)
	if len(got.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1 synthesized tool_result block; %+v", len(got.Blocks), got.Blocks)
	}
	block := got.Blocks[0]
	if block.Kind != BlockKindToolResult {
		t.Fatalf("block kind = %q, want tool_result", block.Kind)
	}
	if block.ToolUseID != "call-1" {
		t.Fatalf("block tool_use_id = %q, want call-1", block.ToolUseID)
	}
	if !block.Derived {
		t.Fatal("synthesized placeholder tool_result block must be marked Derived")
	}
}
