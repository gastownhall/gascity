package sessionlog

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	grokTestSID      = "01a13e21-4c24-7a61-9780-c86d67f4a81b"
	grokTestOtherSID = "01a13e21-9999-7a61-9780-c86d67f4a000"
)

// grokUUIDv7At builds a canonical-layout UUIDv7 whose timestamp prefix encodes
// t, with a stable random tail (the finder only reads the prefix).
func grokUUIDv7At(t time.Time, tail string) string {
	hex := fmt.Sprintf("%012x", t.UnixMilli())
	return fmt.Sprintf("%s-%s-7a61-9780-%s", hex[:8], hex[8:12], tail)
}

func grokModelChangedLine(ts, sid, model string) string {
	return fmt.Sprintf(`{"ts":%q,"src":"shell","pid":1971723,"ver":"1.0.52","lvl":"info","sid":%q,"msg":"model changed","ctx":{"model":%q}}`, ts, sid, model)
}

func grokInferenceDoneLine(ts, sid string, loop, prompt, cached, completion, reasoning int) string {
	return fmt.Sprintf(`{"ts":%q,"src":"shell","pid":1971723,"ver":"1.0.52","lvl":"info","sid":%q,"msg":"shell.turn.inference_done","ctx":{"loop_index":%d,"model_elapsed_ms":8584,"elapsed_since_turn_start_ms":8614,"ttft_ms":5889,"itl_p50_ms":19,"attempts":1,"prompt_tokens":%d,"cached_prompt_tokens":%d,"completion_tokens":%d,"reasoning_tokens":%d,"tokens_per_sec":208.2}}`,
		ts, sid, loop, prompt, cached, completion, reasoning)
}

// grokTestLayout lays out <root>/sessions/<escaped workDir>/<sid>/ and returns
// the session dir plus the shared log path under <root>/logs/.
func grokTestLayout(t *testing.T, root, workDir, sid string) (sessionDir, logPath string) {
	t.Helper()
	sessionDir = filepath.Join(root, "sessions", url.PathEscape(filepath.Clean(workDir)), sid)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", sessionDir, err)
	}
	logPath = filepath.Join(root, "logs", "unified.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(logs): %v", err)
	}
	return sessionDir, logPath
}

func writeGrokSummary(t *testing.T, sessionDir, cwd string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"info": map[string]any{"cwd": cwd, "model": "grok-4.7"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "summary.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractGrokTailUsage(t *testing.T) {
	root := t.TempDir()
	sessionDir, logPath := grokTestLayout(t, root, "/work/dir", grokTestSID)
	writeCodexUsageLines(t, logPath, []string{
		grokModelChangedLine("2026-10-15T05:56:10.000Z", grokTestOtherSID, "grok-3"),
		grokModelChangedLine("2026-10-15T05:56:12.000Z", grokTestSID, "grok-4.7"),
		// Another sid's inference interleaved — must be skipped.
		grokInferenceDoneLine("2026-10-15T05:56:20.000Z", grokTestOtherSID, 1, 9999, 0, 999, 0),
		grokInferenceDoneLine("2026-10-15T05:56:24.218Z", grokTestSID, 1, 32807, 1664, 561, 361),
		// All-zero usage must be skipped.
		grokInferenceDoneLine("2026-10-15T05:56:30.000Z", grokTestSID, 2, 0, 0, 0, 0),
		`{"ts":"2026-10-15T05:56:31.000Z","sid":"` + grokTestSID + `","msg":"shell.turn.inference_done","ctx":"not-an-object"}`,
		grokInferenceDoneLine("2026-10-15T05:56:40.500Z", grokTestSID, 2, 34000, 34200, 120, 20),
		`{"ts":"2026-10-15T05:5`, // torn trailing line tolerated
	})

	usages, err := ExtractGrokTailUsage(sessionDir, "")
	if err != nil {
		t.Fatalf("ExtractGrokTailUsage: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usages, want 2: %+v", len(usages), usages)
	}
	first := usages[0]
	if first.MessageID != "grok:2026-10-15T05:56:24.218Z:1" {
		t.Errorf("first.MessageID = %q", first.MessageID)
	}
	if first.EntryUUID != "2026-10-15T05:56:24.218Z" {
		t.Errorf("first.EntryUUID = %q, want line ts", first.EntryUUID)
	}
	if first.Model != "grok-4.7" {
		t.Errorf("first.Model = %q, want grok-4.7 (this sid's model changed, not the other sid's)", first.Model)
	}
	if first.InputTokens != 32807-1664 {
		t.Errorf("first.InputTokens = %d, want %d (prompt - cached)", first.InputTokens, 32807-1664)
	}
	if first.CacheReadTokens != 1664 {
		t.Errorf("first.CacheReadTokens = %d, want 1664", first.CacheReadTokens)
	}
	if first.OutputTokens != 561 {
		t.Errorf("first.OutputTokens = %d, want 561 (reasoning is included, not added)", first.OutputTokens)
	}
	if first.ReasoningTokens != 361 {
		t.Errorf("first.ReasoningTokens = %d, want 361", first.ReasoningTokens)
	}
	if first.CacheCreationTokens != 0 {
		t.Errorf("first.CacheCreationTokens = %d, want 0", first.CacheCreationTokens)
	}
	if want := time.Date(2026, 10, 15, 5, 56, 24, 218000000, time.UTC); !first.Timestamp.Equal(want) {
		t.Errorf("first.Timestamp = %v, want %v", first.Timestamp, want)
	}
	second := usages[1]
	if second.InputTokens != 0 {
		t.Errorf("second.InputTokens = %d, want 0 (cached > prompt clamps at zero)", second.InputTokens)
	}
	if second.MessageID != "grok:2026-10-15T05:56:40.500Z:2" {
		t.Errorf("second.MessageID = %q", second.MessageID)
	}
}

func TestExtractGrokTailUsageLastModelChangeWins(t *testing.T) {
	root := t.TempDir()
	sessionDir, logPath := grokTestLayout(t, root, "/work/dir", grokTestSID)
	writeCodexUsageLines(t, logPath, []string{
		grokModelChangedLine("2026-10-15T05:56:12.000Z", grokTestSID, "grok-4.7"),
		grokInferenceDoneLine("2026-10-15T05:56:24.000Z", grokTestSID, 1, 100, 0, 10, 0),
		grokModelChangedLine("2026-10-15T05:57:00.000Z", grokTestSID, "grok-composer-2.5-fast"),
		grokInferenceDoneLine("2026-10-15T05:57:24.000Z", grokTestSID, 1, 200, 0, 20, 0),
	})
	usages, err := ExtractGrokTailUsage(sessionDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usages, want 2", len(usages))
	}
	if usages[0].Model != "grok-4.7" || usages[1].Model != "grok-composer-2.5-fast" {
		t.Errorf("models = %q, %q; want the model in effect at each inference", usages[0].Model, usages[1].Model)
	}
}

func TestExtractGrokTailUsageModelMissingIsEmpty(t *testing.T) {
	root := t.TempDir()
	sessionDir, logPath := grokTestLayout(t, root, "/work/dir", grokTestSID)
	writeCodexUsageLines(t, logPath, []string{
		grokModelChangedLine("2026-10-15T05:56:12.000Z", grokTestOtherSID, "grok-3"),
		grokInferenceDoneLine("2026-10-15T05:56:24.000Z", grokTestSID, 1, 100, 0, 10, 0),
	})
	usages, err := ExtractGrokTailUsage(sessionDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 1 {
		t.Fatalf("got %d usages, want 1", len(usages))
	}
	if usages[0].Model != "" {
		t.Errorf("Model = %q, want empty (no model changed line for this sid; unpriced rather than guessed)", usages[0].Model)
	}
}

// grokForeignTail is > tailChunkSize of another sid's lines, so a single
// fixed-window read would see none of ours.
func grokForeignTail(n int) []string {
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, grokInferenceDoneLine(fmt.Sprintf("2026-10-15T06:%02d:%02d.000Z", i/60%60, i%60), grokTestOtherSID, 1, 1000+i, 0, 10, 0))
	}
	return lines
}

func TestExtractGrokTailUsageGrowsPastForeignTail(t *testing.T) {
	root := t.TempDir()
	sessionDir, logPath := grokTestLayout(t, root, "/work/dir", grokTestSID)
	lines := []string{
		grokModelChangedLine("2026-10-15T05:56:12.000Z", grokTestSID, "grok-4.7"),
		grokInferenceDoneLine("2026-10-15T05:56:24.000Z", grokTestSID, 1, 100, 0, 10, 0),
		grokInferenceDoneLine("2026-10-15T05:56:44.000Z", grokTestSID, 1, 200, 0, 20, 0),
	}
	// ~350 bytes per line; 400 lines is well over the 64KB tail window.
	lines = append(lines, grokForeignTail(400)...)
	writeCodexUsageLines(t, logPath, lines)
	if st, err := os.Stat(logPath); err != nil || st.Size() <= tailChunkSize {
		t.Fatalf("fixture must exceed tailChunkSize: size=%d err=%v", st.Size(), err)
	}

	// Cold cursor: the window must grow until this sid's model changed line is
	// in view, so the session's inferences behind the foreign tail are found.
	usages, err := ExtractGrokTailUsage(sessionDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usages, want 2 behind a >64KB foreign tail: %+v", len(usages), usages)
	}
	if usages[0].Model != "grok-4.7" {
		t.Errorf("Model = %q, want grok-4.7", usages[0].Model)
	}
	// MessageIDs stay distinct even though loop_index repeats across turns.
	if usages[0].MessageID == usages[1].MessageID {
		t.Errorf("MessageID collision across turns with the same loop_index: %q", usages[0].MessageID)
	}

	// A capped scan returns the widest window rather than failing.
	capped, err := extractGrokTailUsage(logPath, grokTestSID, "", tailChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 0 {
		t.Errorf("cap at one window must not reach the session's lines; got %+v", capped)
	}
}

func TestExtractGrokTailUsageCursorStopsGrowth(t *testing.T) {
	root := t.TempDir()
	_, logPath := grokTestLayout(t, root, "/work/dir", grokTestSID)
	// No model changed line in the file at all: only the cursor (or EOF/cap)
	// can stop the growth loop.
	lines := []string{
		grokInferenceDoneLine("2026-10-15T05:56:24.000Z", grokTestSID, 1, 100, 0, 10, 0),
	}
	lines = append(lines, grokForeignTail(400)...)
	lines = append(lines, grokInferenceDoneLine("2026-10-15T07:00:00.000Z", grokTestSID, 1, 300, 0, 30, 0))
	lines = append(lines, grokForeignTail(400)...)
	writeCodexUsageLines(t, logPath, lines)

	cursor := "grok:2026-10-15T07:00:00.000Z:1"
	usages, err := extractGrokTailUsage(logPath, grokTestSID, cursor, maxUsageScanBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 1 || usages[0].MessageID != cursor {
		t.Fatalf("cursor in view must stop growth at the cursor's window; got %+v", usages)
	}
	// Without a cursor the loop runs to EOF (whole file in view) and sees both.
	all, err := extractGrokTailUsage(logPath, grokTestSID, "", maxUsageScanBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("whole-file scan: got %d usages, want 2", len(all))
	}
}

func TestExtractGrokTailUsageFromSearchPathsRejectsEscapedPath(t *testing.T) {
	root := t.TempDir()
	outsideRoot := t.TempDir()
	sessionDir, logPath := grokTestLayout(t, outsideRoot, "/work/dir", grokTestSID)
	writeCodexUsageLines(t, logPath, []string{
		grokModelChangedLine("2026-10-15T05:56:12.000Z", grokTestSID, "grok-4.7"),
		grokInferenceDoneLine("2026-10-15T05:56:24.000Z", grokTestSID, 1, 100, 0, 10, 0),
	})
	if _, err := ExtractGrokTailUsageFromSearchPaths([]string{root}, sessionDir, ""); err == nil {
		t.Fatal("session dir outside merged grok roots must be rejected")
	}

	inside, insideLog := grokTestLayout(t, root, "/work/dir", grokTestSID)
	writeCodexUsageLines(t, insideLog, []string{
		grokModelChangedLine("2026-10-15T05:56:12.000Z", grokTestSID, "grok-4.7"),
		grokInferenceDoneLine("2026-10-15T05:56:24.000Z", grokTestSID, 1, 100, 0, 10, 0),
	})
	usages, err := ExtractGrokTailUsageFromSearchPaths([]string{root}, inside, "")
	if err != nil {
		t.Fatalf("inside root: %v", err)
	}
	if len(usages) != 1 {
		t.Fatalf("got %d usages, want 1", len(usages))
	}

	// A shallow dir whose derived log would land above the root is refused too.
	shallow := filepath.Join(root, "sessions", "x")
	if err := os.MkdirAll(shallow, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractGrokTailUsageFromSearchPaths([]string{root}, shallow, ""); err == nil {
		t.Fatal("derived log path outside the root must be rejected")
	}
}

func TestFindGrokSessionDirNearScan(t *testing.T) {
	anchor := time.Date(2026, 10, 15, 5, 56, 0, 0, time.UTC)
	window := 10 * time.Minute

	t.Run("bad inputs are a clean no-op", func(t *testing.T) {
		root := t.TempDir()
		for name, args := range map[string]struct {
			workDir string
			anchor  time.Time
			window  time.Duration
		}{
			"empty workdir": {"", anchor, window},
			"zero anchor":   {"/work/dir", time.Time{}, window},
			"zero window":   {"/work/dir", anchor, 0},
		} {
			path, clean := FindGrokSessionDirNearScan([]string{root}, args.workDir, args.anchor, args.window)
			if path != "" || !clean {
				t.Errorf("%s: = (%q, %v), want (\"\", true)", name, path, clean)
			}
		}
	})

	t.Run("missing sessions dir is a clean miss", func(t *testing.T) {
		path, clean := FindGrokSessionDirNearScan([]string{t.TempDir()}, "/work/dir", anchor, window)
		if path != "" || !clean {
			t.Fatalf("= (%q, %v), want clean miss", path, clean)
		}
	})

	t.Run("in-window uuidv7 dir is found; out-of-window and non-v7 are not", func(t *testing.T) {
		root := t.TempDir()
		workDir := t.TempDir()
		want, _ := grokTestLayout(t, root, workDir, grokUUIDv7At(anchor.Add(5*time.Second), "c86d67f4a81b"))
		writeGrokSummary(t, want, workDir)
		grokTestLayout(t, root, workDir, grokUUIDv7At(anchor.Add(-2*time.Hour), "c86d67f4a000")) // out of window
		grokTestLayout(t, root, workDir, grokUUIDv7At(anchor.Add(window+2*time.Minute), "c86d67f4a001"))
		grokTestLayout(t, root, workDir, "not-a-uuid")
		grokTestLayout(t, root, workDir, strings.Replace(grokUUIDv7At(anchor, "c86d67f4a002"), "-7a61-", "-4a61-", 1)) // v4

		path, clean := FindGrokSessionDirNearScan([]string{root}, workDir, anchor, window)
		if path != want || !clean {
			t.Fatalf("= (%q, %v), want (%q, true)", path, clean, want)
		}
		if got := FindGrokSessionDirNear([]string{root}, workDir, anchor, window); got != want {
			t.Fatalf("FindGrokSessionDirNear = %q, want %q", got, want)
		}
	})

	t.Run("a minute before the anchor is still in window", func(t *testing.T) {
		root := t.TempDir()
		workDir := t.TempDir()
		want, _ := grokTestLayout(t, root, workDir, grokUUIDv7At(anchor.Add(-30*time.Second), "c86d67f4a81b"))
		path, _ := FindGrokSessionDirNearScan([]string{root}, workDir, anchor, window)
		if path != want {
			t.Fatalf("= %q, want %q", path, want)
		}
	})

	t.Run("two in-window dirs for one cwd are refused as ambiguous", func(t *testing.T) {
		root := t.TempDir()
		workDir := t.TempDir()
		grokTestLayout(t, root, workDir, grokUUIDv7At(anchor.Add(5*time.Second), "c86d67f4a81b"))
		grokTestLayout(t, root, workDir, grokUUIDv7At(anchor.Add(3*time.Minute), "c86d67f4a81c"))
		path, clean := FindGrokSessionDirNearScan([]string{root}, workDir, anchor, window)
		if path != "" || !clean {
			t.Fatalf("= (%q, %v), want a definitive (\"\", true) refusal", path, clean)
		}
	})

	t.Run("summary cwd naming another directory rejects the match", func(t *testing.T) {
		root := t.TempDir()
		workDir := t.TempDir()
		other := t.TempDir()
		dir, _ := grokTestLayout(t, root, workDir, grokUUIDv7At(anchor.Add(5*time.Second), "c86d67f4a81b"))
		writeGrokSummary(t, dir, other)
		path, clean := FindGrokSessionDirNearScan([]string{root}, workDir, anchor, window)
		if path != "" || !clean {
			t.Fatalf("= (%q, %v), want clean miss on cwd mismatch", path, clean)
		}
	})

	t.Run("a readdir fault clouds the scan", func(t *testing.T) {
		root := t.TempDir()
		workDir := t.TempDir()
		// The per-cwd sessions entry is a FILE, so ReadDir fails with ENOTDIR
		// — a non-ENOENT fault the caller must not mistake for a clean miss.
		sessions := filepath.Join(root, "sessions")
		if err := os.MkdirAll(sessions, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sessions, url.PathEscape(filepath.Clean(workDir))), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		path, clean := FindGrokSessionDirNearScan([]string{root}, workDir, anchor, window)
		if path != "" || clean {
			t.Fatalf("= (%q, %v), want (\"\", false) dirty miss", path, clean)
		}
	})
}

func TestUUIDv7CreationTime(t *testing.T) {
	want := time.Date(2026, 10, 15, 5, 56, 24, 218000000, time.UTC)
	got, ok := uuidV7CreationTime(grokUUIDv7At(want, "c86d67f4a81b"))
	if !ok || !got.Equal(want) {
		t.Fatalf("uuidV7CreationTime = (%v, %v), want (%v, true)", got, ok, want)
	}
	for _, bad := range []string{"", "01a13e21-4c24-4a61-9780-c86d67f4a81b", "01a13e21-4c24-7a61-9780-c86d67f4a81", "zzzzzzzz-zzzz-7a61-9780-c86d67f4a81b"} {
		if _, ok := uuidV7CreationTime(bad); ok {
			t.Errorf("uuidV7CreationTime(%q) accepted a non-v7 or malformed id", bad)
		}
	}
	// The codex helper is the same decoder.
	if ct, ok := codexUUIDv7CreationTime(grokUUIDv7At(want, "c86d67f4a81b")); !ok || !ct.Equal(want) {
		t.Fatalf("codexUUIDv7CreationTime diverged from uuidV7CreationTime: (%v, %v)", ct, ok)
	}
}
