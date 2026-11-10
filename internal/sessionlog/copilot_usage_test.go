package sessionlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copilotUsageRecordLine is one session.usage_record event shaped like the
// ones GitHub Copilot CLI 1.0.109 writes (fields trimmed to the ones read).
func copilotUsageRecordLine(id, apiCallID, model string, input, output, cacheRead, cacheWrite, reasoning int, ts string) string {
	return fmt.Sprintf(`{"type":"session.usage_record","data":{"usage":{"model":%q,"responseModel":"x","provider":{"id":"copilot","kind":"copilot","label":"GitHub Copilot"},"inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"reasoningTokens":%d,"cost":15.0,"initiator":"agent","interactionType":"conversation-agent","isByok":false,"apiCallId":%q}},"id":%q,"timestamp":%q,"parentId":"p"}`,
		model, input, output, cacheRead, cacheWrite, reasoning, apiCallID, id, ts)
}

func copilotStartLine(cwd string, started time.Time) string {
	return fmt.Sprintf(`{"type":"session.start","data":{"sessionId":"s","version":1,"producer":"copilot-agent","copilotVersion":"1.0.109","startTime":%q,"selectedModel":"claude-opus-5.5","context":{"cwd":%q}},"id":"start","timestamp":%q,"parentId":null}`,
		started.UTC().Format(time.RFC3339Nano), cwd, started.UTC().Format(time.RFC3339Nano))
}

func writeCopilotEvents(t *testing.T, root, sessionID string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(root, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractCopilotTailUsage(t *testing.T) {
	root := t.TempDir()
	path := writeCopilotEvents(t, root, "sess-1",
		copilotStartLine("/w", time.Date(2026, 11, 10, 21, 21, 51, 0, time.UTC)),
		`{"type":"user.message","data":{"content":"hi"},"id":"u1","timestamp":"2026-11-10T21:22:00Z"}`,
		// Real record values from a Copilot CLI 1.0.109 session.
		copilotUsageRecordLine("ev-1", "msg_1", "claude-opus-5.5", 33701, 134, 0, 33697, 0, "2026-11-10T21:22:22.735Z"),
		`{"type":"assistant.message","data":{"messageId":"m1","model":"claude-opus-5.5","apiCallId":"msg_1"},"id":"a1","timestamp":"2026-11-10T21:22:22.738Z"}`,
		copilotUsageRecordLine("ev-2", "msg_2", "claude-opus-5.5", 42683, 334, 34745, 7936, 78, "2026-11-10T21:23:09.632Z"),
		// All-zero usage is skipped.
		copilotUsageRecordLine("ev-3", "msg_3", "claude-opus-5.5", 0, 0, 0, 0, 0, "2026-11-10T21:23:10Z"),
		`not json`,
	)

	got, err := ExtractCopilotTailUsage(path, "")
	if err != nil {
		t.Fatalf("ExtractCopilotTailUsage: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d usages, want 2: %+v", len(got), got)
	}
	first := got[0]
	if first.MessageID != "msg_1" || first.EntryUUID != "ev-1" || first.Model != "claude-opus-5.5" {
		t.Errorf("identity = %q/%q/%q, want msg_1/ev-1/claude-opus-5.5", first.MessageID, first.EntryUUID, first.Model)
	}
	// inputTokens is the whole prompt: 33701 - 0 read - 33697 written = 4 uncached.
	if first.InputTokens != 4 || first.CacheCreationTokens != 33697 || first.CacheReadTokens != 0 || first.OutputTokens != 134 {
		t.Errorf("first tokens = %+v, want input 4, cache write 33697, read 0, output 134", first)
	}
	if want := time.Date(2026, 11, 10, 21, 22, 22, 735000000, time.UTC); !first.Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want %v", first.Timestamp, want)
	}
	second := got[1]
	if second.InputTokens != 2 || second.CacheReadTokens != 34745 || second.CacheCreationTokens != 7936 || second.OutputTokens != 334 || second.ReasoningTokens != 78 {
		t.Errorf("second tokens = %+v, want input 2, read 34745, write 7936, output 334, reasoning 78", second)
	}
	// Copilot's "cost" is a premium-request multiplier, not dollars.
	for _, u := range got {
		if u.HasReportedCost || u.ReportedCostUSD != 0 {
			t.Errorf("%s carried a reported cost %v; Copilot's cost field is not USD", u.MessageID, u.ReportedCostUSD)
		}
	}
}

func TestExtractCopilotTailUsageGrowsWindowToCursor(t *testing.T) {
	root := t.TempDir()
	lines := []string{copilotStartLine("/w", time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC))}
	lines = append(lines, copilotUsageRecordLine("ev-cursor", "msg_cursor", "m", 10, 1, 0, 0, 0, "2026-11-10T21:00:01Z"))
	// Push the cursor out of the fixed 64KB tail.
	filler := `{"type":"tool.execution_complete","data":{"result":"` + strings.Repeat("x", 4096) + `"},"id":"f","timestamp":"2026-11-10T21:00:02Z"}`
	for i := 0; i < 40; i++ {
		lines = append(lines, filler)
	}
	lines = append(lines, copilotUsageRecordLine("ev-new", "msg_new", "m", 20, 2, 0, 0, 0, "2026-11-10T21:00:03Z"))
	path := writeCopilotEvents(t, root, "sess-grow", lines...)

	got, err := ExtractCopilotTailUsage(path, "msg_cursor")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].MessageID != "msg_cursor" || got[1].MessageID != "msg_new" {
		t.Fatalf("got %+v, want the cursor record and the newer one", got)
	}
}

// TestExtractCopilotTailUsageFirstReadReadsWholeLog pins the first-read rule:
// with no cursor, a call written more than 64KB before the end of the log is
// still returned, because a Copilot log holds only its own session.
func TestExtractCopilotTailUsageFirstReadReadsWholeLog(t *testing.T) {
	root := t.TempDir()
	lines := []string{copilotStartLine("/w", time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC))}
	lines = append(lines, copilotUsageRecordLine("ev-early", "msg_early", "m", 10, 1, 0, 0, 0, "2026-11-10T21:00:01Z"))
	filler := `{"type":"tool.execution_complete","data":{"result":"` + strings.Repeat("x", 4096) + `"},"id":"f","timestamp":"2026-11-10T21:00:02Z"}`
	for i := 0; i < 40; i++ {
		lines = append(lines, filler)
	}
	lines = append(lines, copilotUsageRecordLine("ev-late", "msg_late", "m", 20, 2, 0, 0, 0, "2026-11-10T21:00:03Z"))
	path := writeCopilotEvents(t, root, "sess-first", lines...)

	got, err := ExtractCopilotTailUsage(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].MessageID != "msg_early" || got[1].MessageID != "msg_late" {
		t.Fatalf("got %+v, want both calls on the first read", got)
	}
}

func TestExtractCopilotTailUsageFromSearchPathsRejectsOutsidePath(t *testing.T) {
	root := t.TempDir()
	outside := writeCopilotEvents(t, t.TempDir(), "sess-x",
		copilotUsageRecordLine("ev-1", "msg_1", "m", 10, 1, 0, 0, 0, "2026-11-10T21:00:01Z"))
	if _, err := ExtractCopilotTailUsageFromSearchPaths([]string{root}, outside, ""); err == nil {
		t.Fatal("want an error for a path outside the Copilot search roots")
	}
}

func TestFindCopilotSessionFileNearScan(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	wake := time.Date(2026, 11, 10, 21, 21, 44, 0, time.UTC)

	// An earlier session of the same pool seat: same cwd, started an hour before the wake.
	writeCopilotEvents(t, root, "seat-earlier", copilotStartLine(workDir, wake.Add(-time.Hour)))
	// This wake's session.
	want := writeCopilotEvents(t, root, "seat-now", copilotStartLine(workDir, wake.Add(7*time.Second)))
	// Another agent started in the same window, different cwd.
	writeCopilotEvents(t, root, "other-agent", copilotStartLine(otherDir, wake.Add(3*time.Second)))
	// mtime gate must not hide files written in the window.
	for _, id := range []string{"seat-earlier", "seat-now", "other-agent"} {
		p := filepath.Join(root, id, "events.jsonl")
		if err := os.Chtimes(p, wake.Add(time.Minute), wake.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	got, clean := FindCopilotSessionFileNearScan([]string{root}, workDir, wake, 10*time.Minute)
	if got != want || !clean {
		t.Fatalf("FindCopilotSessionFileNearScan = %q, %v; want %q, true", got, clean, want)
	}

	// A second in-window session for the same work dir is ambiguous: refuse.
	writeCopilotEvents(t, root, "seat-dup", copilotStartLine(workDir, wake.Add(20*time.Second)))
	if got, clean := FindCopilotSessionFileNearScan([]string{root}, workDir, wake, 10*time.Minute); got != "" || !clean {
		t.Fatalf("ambiguous scan = %q, %v; want \"\", true", got, clean)
	}

	// Bad inputs are a clean no-op.
	if got, clean := FindCopilotSessionFileNearScan([]string{root}, "", wake, time.Minute); got != "" || !clean {
		t.Fatalf("empty workdir = %q, %v", got, clean)
	}
}
