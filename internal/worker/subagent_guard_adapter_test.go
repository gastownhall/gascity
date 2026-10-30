package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// adapterBackedTranscript drives the guard through the real SessionLogAdapter
// from a file on disk, the way a live SessionHandle does. Tests that inject
// RawMessages bypass the read path entirely and cannot observe DAG pruning.
type adapterBackedTranscript struct {
	mappings []AgentMapping
	path     string
}

func (a adapterBackedTranscript) AgentMappings(context.Context) ([]AgentMapping, error) {
	return a.mappings, nil
}

// Transcript is the DAG-pruned read the guard used before it read records.
func (a adapterBackedTranscript) Transcript(_ context.Context, req TranscriptRequest) (*TranscriptResult, error) {
	req.TranscriptPath = a.path
	req.Provider = "claude"
	return SessionLogAdapter{}.ReadTranscript(req)
}

func (a adapterBackedTranscript) TranscriptRecords(context.Context) ([]json.RawMessage, error) {
	return SessionLogAdapter{}.TranscriptRecords(a.path)
}

func fixtureOnDisk(t *testing.T, fixture string) string {
	t.Helper()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

// A completion recorded only as a queue-operation carries no uuid, so the
// active-branch walk drops it. Reading through the DAG therefore reports a
// finished subagent as live and refuses a kill that should proceed.
func TestInFlightBackgroundSubagents_QueueOperationCompletionThroughRealAdapter(t *testing.T) {
	path := fixtureOnDisk(t, "testdata/subagent_guard_queue_only_completion.jsonl")
	mappings := []AgentMapping{{AgentID: "queueonly", ParentToolUseID: "toolu_queue_only"}}

	live, err := InFlightBackgroundSubagents(context.Background(), adapterBackedTranscript{mappings: mappings, path: path})
	if err != nil {
		t.Fatalf("InFlightBackgroundSubagents: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live = %#v, want none: the queue-operation completion must be visible to the guard", live)
	}
}

// A spawn recorded before a compaction boundary falls off the active branch
// once the post-compaction turns form the tip. Reading the pruned view hides
// the spawn entirely, so the guard sees nothing to protect and a kill destroys
// live background work — the failure this guard exists to prevent.
func TestInFlightBackgroundSubagents_PreCompactionSpawnThroughRealAdapter(t *testing.T) {
	path := fixtureOnDisk(t, "testdata/subagent_guard_precompaction_spawn.jsonl")
	mappings := []AgentMapping{{AgentID: "precompact", ParentToolUseID: "toolu_precompact"}}
	transcript := adapterBackedTranscript{mappings: mappings, path: path}

	// Contrast: the DAG-pruned read drops the pre-compaction spawn.
	pruned, err := transcript.Transcript(context.Background(), TranscriptRequest{Raw: true})
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	records, err := transcript.TranscriptRecords(context.Background())
	if err != nil {
		t.Fatalf("TranscriptRecords: %v", err)
	}
	if len(pruned.RawMessages) >= len(records) {
		t.Fatalf("pruned=%d records=%d: fixture no longer exercises DAG pruning", len(pruned.RawMessages), len(records))
	}

	live, err := InFlightBackgroundSubagents(context.Background(), transcript)
	if err != nil {
		t.Fatalf("InFlightBackgroundSubagents: %v", err)
	}
	if len(live) != 1 || live[0].AgentID != "precompact" {
		t.Fatalf("live = %#v, want the pre-compaction spawn reported live", live)
	}
}
