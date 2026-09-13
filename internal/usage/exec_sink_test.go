package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSinkScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sink.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecSinkRecordSuccessFeedsFactJSON(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.jsonl")
	s := NewExecSink(writeSinkScript(t, "cat >> "+out))
	if err := s.Record(context.Background(), Fact{Kind: KindModel, RunID: "r1", InputTokens: 5}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"run_id":"r1"`) {
		t.Fatalf("script did not receive the fact JSON on stdin: %q", data)
	}
}

func TestExecSinkRecordSurfacesScriptFailure(t *testing.T) {
	s := NewExecSink(writeSinkScript(t, "echo boom >&2; exit 3"))
	err := s.Record(context.Background(), Fact{Kind: KindCompute, RunID: "r1"})
	if err == nil {
		t.Fatal("a failing script must surface an error, never a silent drop")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should include the script stderr, got %v", err)
	}
}

// TestExecSinkRecordHangingScriptTimesOutPromptly is the regression for the
// adopt-pr review finding that a hung exec: usage sink could block the
// controller reconcile tick and worker op-finish hot paths indefinitely. The
// per-fact timeout must bound Record even when the caller passes a long-lived
// context, so a hung script returns a timeout error instead of stalling.
func TestExecSinkRecordHangingScriptTimesOutPromptly(t *testing.T) {
	s := NewExecSinkWithTimeout(writeSinkScript(t, "sleep 30"), 100*time.Millisecond)
	start := time.Now()
	err := s.Record(context.Background(), Fact{Kind: KindModel, RunID: "r1"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a hung script must surface a timeout error, not block forever")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error should report a timeout, got %v", err)
	}
	// The real bound is ~100ms; allow generous slack for CI scheduling without
	// tolerating an indefinite block.
	if elapsed > 5*time.Second {
		t.Fatalf("Record did not return promptly under a hung script: took %s", elapsed)
	}
}
