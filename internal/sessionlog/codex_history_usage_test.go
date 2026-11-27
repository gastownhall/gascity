package sessionlog

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Structured history needs every Codex invocation's usage, not only those in
// the telemetry tail window: older turns keep their model and token counts.
func TestReadCodexFileDetachedUsageCoversWholeRollout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	first := codexTokenCountLine("2026-09-06T00:00:03Z", 150, 110, 10, 40, 0)
	padding := fmt.Sprintf(`{"timestamp":"2026-09-06T00:00:04Z","type":"event_msg","payload":{"type":"test_padding","text":%q}}`, strings.Repeat("x", 2*tailChunkSize))
	writeCodexUsageLines(t, path, []string{
		codexTurnContextLine("2026-09-06T00:00:01Z", "gpt-5.5"),
		first,
		padding,
		codexTurnContextLine("2026-09-06T00:00:05Z", "gpt-new"),
		// A duplicate cumulative snapshot after a model switch must keep the
		// first observation's timestamp and model.
		strings.Replace(first, "00:00:03Z", "00:00:06Z", 1),
		codexTokenCountLine("2026-09-06T00:00:07Z", 400, 200, 0, 50, 5),
		`{"timestamp":"2026-09-06T00:00:08Z","type":"event_msg","payload":{"type":"token_count","info":null}}`,
	})

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.DetachedUsage) != 2 {
		t.Fatalf("DetachedUsage = %+v, want 2 invocations", sess.DetachedUsage)
	}
	got := sess.DetachedUsage[0]
	if got.Model != "gpt-5.5" || got.EntryUUID != "2026-09-06T00:00:03Z" || got.InputTokens != 100 || got.CacheReadTokens != 10 || got.OutputTokens != 40 {
		t.Fatalf("first invocation = %+v", got)
	}
	wantStart, err := time.Parse(time.RFC3339, "2026-09-06T00:00:01Z")
	if err != nil {
		t.Fatal(err)
	}
	wantEnd, err := time.Parse(time.RFC3339, "2026-09-06T00:00:03Z")
	if err != nil {
		t.Fatal(err)
	}
	if !got.StartedAt.Equal(wantStart) || !got.Timestamp.Equal(wantEnd) {
		t.Fatalf("first span = %s .. %s, want %s .. %s (the later duplicate must not move it)", got.StartedAt, got.Timestamp, wantStart, wantEnd)
	}
	got = sess.DetachedUsage[1]
	if got.Model != "gpt-new" || got.InputTokens != 200 || got.OutputTokens != 50 || got.ReasoningTokens != 5 {
		t.Fatalf("second invocation = %+v", got)
	}
	wantStart, err = time.Parse(time.RFC3339, "2026-09-06T00:00:05Z")
	if err != nil {
		t.Fatal(err)
	}
	wantEnd, err = time.Parse(time.RFC3339, "2026-09-06T00:00:07Z")
	if err != nil {
		t.Fatal(err)
	}
	if !got.StartedAt.Equal(wantStart) || !got.Timestamp.Equal(wantEnd) {
		t.Fatalf("second span = %s .. %s, want %s .. %s", got.StartedAt, got.Timestamp, wantStart, wantEnd)
	}
}
