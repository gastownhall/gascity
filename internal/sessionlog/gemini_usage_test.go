package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// geminiLegacyUsageFixture mirrors the real legacy single-JSON envelope
// (~/.gemini/tmp/<project>/chats/session-*.json): sessionId, projectHash,
// startTime, lastUpdated, messages[], kind. Token shape per gemini-cli
// ChatRecordingService.recordMessageTokens: {input, output, cached,
// thoughts, tool, total}.
const geminiLegacyUsageFixture = `{
  "sessionId": "14d7203e-8840-4c58-91f1-5ab32f11e898",
  "projectHash": "414e7f6f8996be8df658a05abdee1847e1072eb2b72d2e421a0e101a8a06b752",
  "startTime": "2026-03-19T01:40:49.149Z",
  "lastUpdated": "2026-03-19T01:41:30.000Z",
  "messages": [
    {
      "id": "525da806-1959-4f73-ae67-a512dead6004",
      "timestamp": "2026-03-19T01:40:49.149Z",
      "type": "user",
      "content": "hello"
    },
    {
      "id": "625da806-1959-4f73-ae67-a512dead6005",
      "timestamp": "2026-03-19T01:40:50.000Z",
      "type": "info",
      "content": "Existing command '/bug' was renamed to '/bug1' because it conflicts with built-in command."
    },
    {
      "id": "725da806-1959-4f73-ae67-a512dead6006",
      "timestamp": "2026-03-19T01:40:55.000Z",
      "type": "gemini",
      "content": "thinking about it",
      "model": "gemini-3-pro"
    },
    {
      "id": "825da806-1959-4f73-ae67-a512dead6007",
      "timestamp": "2026-03-19T01:41:00.000Z",
      "type": "gemini",
      "content": "first answer",
      "model": "gemini-3-pro",
      "tokens": {"input": 1200, "output": 80, "cached": 1000, "thoughts": 40, "tool": 30, "total": 1350}
    },
    {
      "id": "925da806-1959-4f73-ae67-a512dead6008",
      "timestamp": "2026-03-19T01:41:20.000Z",
      "type": "gemini",
      "content": "second answer",
      "model": "gemini-3-flash",
      "tokens": {"input": 10, "output": 5, "cached": 40, "thoughts": 0, "tool": 0, "total": 55}
    }
  ],
  "kind": "main"
}`

func TestExtractGeminiUsageLegacyJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-2026-03-19T01-40-14d7203e.json")
	if err := os.WriteFile(path, []byte(geminiLegacyUsageFixture), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usages, err := ExtractGeminiUsage(path)
	if err != nil {
		t.Fatalf("ExtractGeminiUsage: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usages, want 2 (non-token messages skipped): %+v", len(usages), usages)
	}

	first := usages[0]
	if first.EntryUUID != "825da806-1959-4f73-ae67-a512dead6007" || first.MessageID != first.EntryUUID {
		t.Errorf("first identity = (%q, %q), want message id for both", first.EntryUUID, first.MessageID)
	}
	if first.Model != "gemini-3-pro" {
		t.Errorf("first.Model = %q, want gemini-3-pro", first.Model)
	}
	if first.InputTokens != 1200+30-1000 {
		t.Errorf("first.InputTokens = %d, want %d (input + tool - cached)", first.InputTokens, 1200+30-1000)
	}
	if first.OutputTokens != 80+40 {
		t.Errorf("first.OutputTokens = %d, want %d (output + thoughts)", first.OutputTokens, 80+40)
	}
	if first.CacheReadTokens != 1000 {
		t.Errorf("first.CacheReadTokens = %d, want 1000", first.CacheReadTokens)
	}
	if first.CacheCreationTokens != 0 {
		t.Errorf("first.CacheCreationTokens = %d, want 0", first.CacheCreationTokens)
	}

	second := usages[1]
	if second.MessageID != "925da806-1959-4f73-ae67-a512dead6008" {
		t.Errorf("second.MessageID = %q, want the second token-bearing message id (order preserved)", second.MessageID)
	}
	if second.Model != "gemini-3-flash" {
		t.Errorf("second.Model = %q, want gemini-3-flash", second.Model)
	}
	if second.InputTokens != 0 {
		t.Errorf("second.InputTokens = %d, want 0 (clamped: cached exceeds input + tool)", second.InputTokens)
	}
	if second.CacheReadTokens != 40 {
		t.Errorf("second.CacheReadTokens = %d, want 40", second.CacheReadTokens)
	}
}

// geminiJSONLUsageLines mirrors the gemini-cli >=0.45 ChatRecordingService
// JSONL format: first line is the metadata object, message records are
// appended per push, the last gemini message is RE-appended once tokens
// arrive (last occurrence wins), and {"$set":...} metadata patches are
// interleaved.
var geminiJSONLUsageLines = []string{
	`{"sessionId":"24e7203e-8840-4c58-91f1-5ab32f11e899","projectHash":"414e7f6f8996be8df658a05abdee1847e1072eb2b72d2e421a0e101a8a06b752","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:00.000Z","kind":"main"}`,
	`{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"user","content":"hello"}`,
	`{"id":"b25da806-1959-4f73-ae67-a512dead6002","timestamp":"2026-06-10T10:00:05.000Z","type":"gemini","content":"answer","model":"gemini-3-pro","tokens":null}`,
	`{"id":"b25da806-1959-4f73-ae67-a512dead6002","timestamp":"2026-06-10T10:00:05.000Z","type":"gemini","content":"answer","model":"gemini-3-pro","tokens":{"input":900,"output":60,"cached":800,"thoughts":20,"tool":0,"total":980}}`,
	`{"$set":{"lastUpdated":"2026-06-10T10:00:06.000Z"}}`,
	`{"id":"c25da806-`, // torn trailing write tolerated
}

func TestExtractGeminiUsageJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-2026-06-10T10-00-24e7203e.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(geminiJSONLUsageLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usages, err := ExtractGeminiUsage(path)
	if err != nil {
		t.Fatalf("ExtractGeminiUsage: %v", err)
	}
	if len(usages) != 1 {
		t.Fatalf("got %d usages, want 1 (re-pushed message id collapses, $set and torn lines tolerated): %+v", len(usages), usages)
	}
	u := usages[0]
	if u.MessageID != "b25da806-1959-4f73-ae67-a512dead6002" || u.EntryUUID != u.MessageID {
		t.Errorf("identity = (%q, %q), want message id for both", u.EntryUUID, u.MessageID)
	}
	if u.Model != "gemini-3-pro" {
		t.Errorf("Model = %q, want gemini-3-pro", u.Model)
	}
	if u.InputTokens != 900-800 {
		t.Errorf("InputTokens = %d, want %d", u.InputTokens, 900-800)
	}
	if u.OutputTokens != 60+20 {
		t.Errorf("OutputTokens = %d, want %d", u.OutputTokens, 60+20)
	}
	if u.CacheReadTokens != 800 {
		t.Errorf("CacheReadTokens = %d, want 800", u.CacheReadTokens)
	}
}

// TestExtractGeminiUsageJSONLSetMessages pins $set replay parity in the
// usage extractor: usage carried INSIDE a non-nil $set.messages array (the
// compaction/resume rewrite) must be extracted, usage of messages dropped
// by the rewrite must not survive it, and post-$set appends count on top.
func TestExtractGeminiUsageJSONLSetMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-set-usage.jsonl")
	lines := []string{
		`{"sessionId":"74e7203e-8840-4c58-91f1-5ab32f11e804","projectHash":"hash","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:09.000Z","kind":"main"}`,
		// Pre-$set token-bearing message: scrubbed by the rewrite below.
		`{"id":"p25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"gemini","content":"scrubbed","model":"gemini-3-pro","tokens":{"input":5000,"output":900,"cached":4000,"thoughts":100,"tool":0,"total":6000}}`,
		`{"$set":{"messages":[` +
			`{"id":"u25da806-1959-4f73-ae67-a512dead6010","timestamp":"2026-06-10T10:00:02.000Z","type":"user","content":"compacted question"},` +
			`{"id":"g25da806-1959-4f73-ae67-a512dead6011","timestamp":"2026-06-10T10:00:03.000Z","type":"gemini","content":"compacted answer","model":"gemini-3-pro","tokens":{"input":900,"output":60,"cached":800,"thoughts":20,"tool":0,"total":980}}` +
			`]}}`,
		`{"id":"h25da806-1959-4f73-ae67-a512dead6012","timestamp":"2026-06-10T10:00:05.000Z","type":"gemini","content":"post-set answer","model":"gemini-3-flash","tokens":{"input":10,"output":5,"cached":0,"thoughts":0,"tool":0,"total":15}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usages, err := ExtractGeminiUsage(path)
	if err != nil {
		t.Fatalf("ExtractGeminiUsage: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usages, want 2 ($set-embedded + post-$set; pre-$set dropped): %+v", len(usages), usages)
	}
	first := usages[0]
	if first.MessageID != "g25da806-1959-4f73-ae67-a512dead6011" {
		t.Errorf("first.MessageID = %q, want the $set-embedded gemini message id", first.MessageID)
	}
	if first.InputTokens != 900-800 || first.OutputTokens != 60+20 || first.CacheReadTokens != 800 {
		t.Errorf("first usage = (%d,%d,%d), want (100,80,800)", first.InputTokens, first.OutputTokens, first.CacheReadTokens)
	}
	second := usages[1]
	if second.MessageID != "h25da806-1959-4f73-ae67-a512dead6012" {
		t.Errorf("second.MessageID = %q, want the post-$set message id", second.MessageID)
	}
	if second.Model != "gemini-3-flash" {
		t.Errorf("second.Model = %q, want gemini-3-flash", second.Model)
	}
}

// TestExtractGeminiUsageJSONLReappendLastWins pins last-occurrence-wins when
// one message id is re-appended with DIFFERENT non-null token values —
// gemini-cli re-pushes the final gemini message once its real tokens arrive,
// so a partial copy can precede the full copy under the same id. A first-wins
// regression would silently record the stale (partial) counts; the existing
// re-append fixture cannot catch it because its first copy carries tokens:null
// and is skipped before the dedup branch is reached.
func TestExtractGeminiUsageJSONLReappendLastWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-reappend.jsonl")
	lines := []string{
		`{"sessionId":"a4e7203e-8840-4c58-91f1-5ab32f11e807","projectHash":"hash","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:09.000Z","kind":"main"}`,
		`{"id":"r25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:05.000Z","type":"gemini","content":"partial","model":"gemini-3-pro","tokens":{"input":100,"output":10,"cached":0,"thoughts":0,"tool":0,"total":110}}`,
		`{"id":"r25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:06.000Z","type":"gemini","content":"final","model":"gemini-3-pro","tokens":{"input":900,"output":60,"cached":0,"thoughts":0,"tool":0,"total":960}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usages, err := ExtractGeminiUsage(path)
	if err != nil {
		t.Fatalf("ExtractGeminiUsage: %v", err)
	}
	if len(usages) != 1 {
		t.Fatalf("got %d usages, want 1 (one id collapses to a single entry): %+v", len(usages), usages)
	}
	u := usages[0]
	if u.InputTokens != 900 || u.OutputTokens != 60 {
		t.Errorf("usage = (%d,%d), want (900,60) — the LATER re-appended copy must win, not the first (100,10)", u.InputTokens, u.OutputTokens)
	}
}

// TestExtractGeminiUsageJSONLSetMessagesIDCollision pins the byMessageID reset
// on a $set.messages replay: gemini-cli compaction re-emits ORIGINAL message
// objects under their original ids, so a $set.messages array routinely carries
// an id that already appeared before the rewrite. The replay must reset the
// id index along with the usage slice; without the reset, the stale index
// would address the cleared slice and panic.
func TestExtractGeminiUsageJSONLSetMessagesIDCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-set-collision-usage.jsonl")
	lines := []string{
		`{"sessionId":"b4e7203e-8840-4c58-91f1-5ab32f11e808","projectHash":"hash","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:09.000Z","kind":"main"}`,
		// Pre-$set token-bearing message whose id is re-emitted inside the $set.
		`{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"gemini","content":"pre","model":"gemini-3-pro","tokens":{"input":5000,"output":900,"cached":4000,"thoughts":100,"tool":0,"total":6000}}`,
		`{"$set":{"messages":[` +
			`{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:02.000Z","type":"gemini","content":"replayed","model":"gemini-3-pro","tokens":{"input":900,"output":60,"cached":800,"thoughts":20,"tool":0,"total":980}},` +
			`{"id":"c25da806-1959-4f73-ae67-a512dead6003","timestamp":"2026-06-10T10:00:03.000Z","type":"gemini","content":"new","model":"gemini-3-flash","tokens":{"input":10,"output":5,"cached":0,"thoughts":0,"tool":0,"total":15}}` +
			`]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usages, err := ExtractGeminiUsage(path)
	if err != nil {
		t.Fatalf("ExtractGeminiUsage: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usages, want 2 (replayed colliding id + new id): %+v", len(usages), usages)
	}
	if usages[0].MessageID != "a25da806-1959-4f73-ae67-a512dead6001" || usages[0].InputTokens != 100 {
		t.Errorf("first = (%q,%d), want the REPLAYED colliding id with input 100, not the pre-$set 1000", usages[0].MessageID, usages[0].InputTokens)
	}
	if usages[1].MessageID != "c25da806-1959-4f73-ae67-a512dead6003" {
		t.Errorf("second.MessageID = %q, want the new $set-embedded id", usages[1].MessageID)
	}
}

// TestExtractGeminiUsageJSONLLargeFinalRecord pins whole-file extraction:
// gemini re-appends the ENTIRE message record on every update, so the final
// token-bearing record of tool-using turns routinely exceeds a 64KB tail
// window; a windowed read holds only its unparseable truncated suffix.
func TestExtractGeminiUsageJSONLLargeFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-large-usage.jsonl")
	bigContent := strings.Repeat("x", 80*1024) // exceeds any fixed 64KB tail window
	lines := []string{
		`{"sessionId":"84e7203e-8840-4c58-91f1-5ab32f11e805","projectHash":"hash","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:09.000Z","kind":"main"}`,
		`{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"user","content":"hello"}`,
		`{"id":"b25da806-1959-4f73-ae67-a512dead6002","timestamp":"2026-06-10T10:00:05.000Z","type":"gemini","content":"` + bigContent + `","model":"gemini-3-pro","tokens":{"input":900,"output":60,"cached":800,"thoughts":20,"tool":0,"total":980}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usages, err := ExtractGeminiUsage(path)
	if err != nil {
		t.Fatalf("ExtractGeminiUsage: %v", err)
	}
	if len(usages) != 1 {
		t.Fatalf("got %d usages, want 1 (a >64KB final record must still be extracted): %+v", len(usages), usages)
	}
	u := usages[0]
	if u.MessageID != "b25da806-1959-4f73-ae67-a512dead6002" {
		t.Errorf("MessageID = %q, want the large record's id", u.MessageID)
	}
	if u.InputTokens != 900-800 || u.OutputTokens != 60+20 || u.CacheReadTokens != 800 {
		t.Errorf("usage = (%d,%d,%d), want (100,80,800)", u.InputTokens, u.OutputTokens, u.CacheReadTokens)
	}
}

// TestGeminiJSONLOversizedLineTolerated pins the soft-stop on
// bufio.ErrTooLong: one oversized line (a whole-conversation $set patch
// grows with conversation size) must not fail the entire session — both
// readers return everything parsed before it with nil error, matching
// gemini-cli's own replay and the legacy path, which have no per-line cap.
func TestGeminiJSONLOversizedLineTolerated(t *testing.T) {
	prevCap := geminiScanBufferCap
	geminiScanBufferCap = 4 * 1024
	t.Cleanup(func() { geminiScanBufferCap = prevCap })

	path := filepath.Join(t.TempDir(), "session-oversized.jsonl")
	lines := []string{
		`{"sessionId":"94e7203e-8840-4c58-91f1-5ab32f11e806","projectHash":"hash","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:09.000Z","kind":"main"}`,
		`{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"user","content":"hello"}`,
		`{"id":"b25da806-1959-4f73-ae67-a512dead6002","timestamp":"2026-06-10T10:00:05.000Z","type":"gemini","content":"answer","model":"gemini-3-pro","tokens":{"input":900,"output":60,"cached":800,"thoughts":20,"tool":0,"total":980}}`,
		`{"$set":{"messages":[{"id":"x25da806-1959-4f73-ae67-a512dead6010","type":"user","content":"` + strings.Repeat("x", 8*1024) + `"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sess, err := ReadGeminiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadGeminiFile: %v (an oversized line must not fail the whole session)", err)
	}
	if len(sess.Messages) != 2 {
		var got []string
		for _, m := range sess.Messages {
			got = append(got, m.UUID)
		}
		t.Fatalf("messages = %v, want the 2 messages parsed before the oversized line", got)
	}

	usages, err := ExtractGeminiUsage(path)
	if err != nil {
		t.Fatalf("ExtractGeminiUsage: %v (an oversized line must not fail usage extraction)", err)
	}
	if len(usages) != 1 || usages[0].MessageID != "b25da806-1959-4f73-ae67-a512dead6002" {
		t.Fatalf("usages = %+v, want only the token-bearing message parsed before the oversized line", usages)
	}
}

func TestExtractGeminiUsageFromSearchPaths(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "proj", "chats", "session-test.json")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(inside, []byte(geminiLegacyUsageFixture), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usages, err := ExtractGeminiUsageFromSearchPaths([]string{root}, inside)
	if err != nil {
		t.Fatalf("ExtractGeminiUsageFromSearchPaths(inside root): %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usages, want 2", len(usages))
	}

	outside := filepath.Join(t.TempDir(), "session-out.json")
	if err := os.WriteFile(outside, []byte(geminiLegacyUsageFixture), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := ExtractGeminiUsageFromSearchPaths([]string{root}, outside); err == nil {
		t.Fatal("path outside all merged gemini roots must be rejected")
	}
}

func TestFindGeminiSessionFileAcceptsJSONL(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	projDir := filepath.Join(root, "proj")
	chatsDir := filepath.Join(projDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, ".project_root"), []byte(workDir+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(.project_root): %v", err)
	}

	oldJSON := filepath.Join(chatsDir, "session-old.json")
	newJSONL := filepath.Join(chatsDir, "session-new.jsonl")
	subagent := filepath.Join(chatsDir, "0199aaaa-bbbb-7000-8000-000000000001.jsonl")
	for _, p := range []string{oldJSON, newJSONL, subagent} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", p, err)
		}
	}
	base := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldJSON, base, base); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := os.Chtimes(newJSONL, base.Add(10*time.Minute), base.Add(10*time.Minute)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// Newest mtime of all, but lacks the session- prefix (subagent file):
	// must never win.
	if err := os.Chtimes(subagent, base.Add(20*time.Minute), base.Add(20*time.Minute)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	got := FindGeminiSessionFile([]string{root}, workDir)
	if got != newJSONL {
		t.Fatalf("FindGeminiSessionFile = %q, want %q (.jsonl sessions must be discoverable)", got, newJSONL)
	}
}

// TestReadGeminiFileJSONLRewind pins gemini-cli's rewind replay semantics
// (loadConversationRecord in the 0.45.2 bundle): a {"$rewindTo":"<id>"}
// record deletes the target message AND every later message; an unknown
// target clears the whole conversation. Messages appended after the rewind
// are kept.
func TestReadGeminiFileJSONLRewind(t *testing.T) {
	t.Run("known target truncates from the target onward", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session-rewind.jsonl")
		lines := []string{
			`{"sessionId":"34e7203e-8840-4c58-91f1-5ab32f11e800","projectHash":"hash","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:09.000Z","kind":"main"}`,
			`{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"user","content":"keep me"}`,
			`{"id":"b25da806-1959-4f73-ae67-a512dead6002","timestamp":"2026-06-10T10:00:02.000Z","type":"gemini","content":"kept answer","model":"gemini-3-pro"}`,
			`{"id":"c25da806-1959-4f73-ae67-a512dead6003","timestamp":"2026-06-10T10:00:03.000Z","type":"user","content":"rewound question"}`,
			`{"id":"d25da806-1959-4f73-ae67-a512dead6004","timestamp":"2026-06-10T10:00:04.000Z","type":"gemini","content":"rewound answer","model":"gemini-3-pro"}`,
			`{"$rewindTo":"c25da806-1959-4f73-ae67-a512dead6003"}`,
			`{"id":"e25da806-1959-4f73-ae67-a512dead6005","timestamp":"2026-06-10T10:00:08.000Z","type":"user","content":"after rewind"}`,
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		sess, err := ReadGeminiFile(path, 0)
		if err != nil {
			t.Fatalf("ReadGeminiFile: %v", err)
		}
		want := []string{
			"a25da806-1959-4f73-ae67-a512dead6001",
			"b25da806-1959-4f73-ae67-a512dead6002",
			"e25da806-1959-4f73-ae67-a512dead6005",
		}
		var got []string
		for _, m := range sess.Messages {
			got = append(got, m.UUID)
		}
		if len(got) != len(want) {
			t.Fatalf("messages = %v, want %v (rewind target and later messages deleted)", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("message %d = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("unknown target clears all prior messages", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session-rewind-unknown.jsonl")
		lines := []string{
			`{"sessionId":"44e7203e-8840-4c58-91f1-5ab32f11e801","projectHash":"hash","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:09.000Z","kind":"main"}`,
			`{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"user","content":"cleared"}`,
			`{"$rewindTo":"ffffffff-0000-0000-0000-000000000000"}`,
			`{"id":"b25da806-1959-4f73-ae67-a512dead6002","timestamp":"2026-06-10T10:00:05.000Z","type":"user","content":"survivor"}`,
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		sess, err := ReadGeminiFile(path, 0)
		if err != nil {
			t.Fatalf("ReadGeminiFile: %v", err)
		}
		if len(sess.Messages) != 1 || sess.Messages[0].UUID != "b25da806-1959-4f73-ae67-a512dead6002" {
			var got []string
			for _, m := range sess.Messages {
				got = append(got, m.UUID)
			}
			t.Fatalf("messages = %v, want only the post-rewind survivor", got)
		}
	})
}

// TestReadGeminiFileJSONLSetMessages pins gemini-cli 0.45.x's
// {"$set":{"messages":[...]}} replay semantics (updateMessagesFromHistory →
// loadConversationRecord): when $set.messages is a non-nil array, the
// accumulated conversation is CLEARED and replaced by the embedded messages
// in array order; records appended after the $set apply on top. Fired on
// compaction/context-scrub and on initialize() after resume, so skipping it
// renders stale pre-compaction messages.
func TestReadGeminiFileJSONLSetMessages(t *testing.T) {
	t.Run("non-nil messages array replaces accumulated state", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session-set.jsonl")
		lines := []string{
			`{"sessionId":"54e7203e-8840-4c58-91f1-5ab32f11e802","projectHash":"hash","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:09.000Z","kind":"main"}`,
			`{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"user","content":"pre-compaction question"}`,
			`{"id":"b25da806-1959-4f73-ae67-a512dead6002","timestamp":"2026-06-10T10:00:02.000Z","type":"gemini","content":"pre-compaction answer","model":"gemini-3-pro"}`,
			`{"$set":{"messages":[{"id":"x25da806-1959-4f73-ae67-a512dead6010","timestamp":"2026-06-10T10:00:03.000Z","type":"user","content":"compacted question"},{"id":"y25da806-1959-4f73-ae67-a512dead6011","timestamp":"2026-06-10T10:00:04.000Z","type":"gemini","content":"compacted answer","model":"gemini-3-pro"}]}}`,
			`{"id":"z25da806-1959-4f73-ae67-a512dead6012","timestamp":"2026-06-10T10:00:05.000Z","type":"user","content":"after compaction"}`,
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		sess, err := ReadGeminiFile(path, 0)
		if err != nil {
			t.Fatalf("ReadGeminiFile: %v", err)
		}
		want := []string{
			"x25da806-1959-4f73-ae67-a512dead6010",
			"y25da806-1959-4f73-ae67-a512dead6011",
			"z25da806-1959-4f73-ae67-a512dead6012",
		}
		var got []string
		for _, m := range sess.Messages {
			got = append(got, m.UUID)
		}
		if len(got) != len(want) {
			t.Fatalf("messages = %v, want %v ($set.messages must replace the accumulated conversation)", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("message %d = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("null or absent messages keeps skip behavior", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session-set-null.jsonl")
		lines := []string{
			`{"sessionId":"64e7203e-8840-4c58-91f1-5ab32f11e803","projectHash":"hash","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:09.000Z","kind":"main"}`,
			`{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"user","content":"kept"}`,
			`{"$set":{"messages":null}}`,
			`{"$set":{"lastUpdated":"2026-06-10T10:00:06.000Z"}}`,
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		sess, err := ReadGeminiFile(path, 0)
		if err != nil {
			t.Fatalf("ReadGeminiFile: %v", err)
		}
		if len(sess.Messages) != 1 || sess.Messages[0].UUID != "a25da806-1959-4f73-ae67-a512dead6001" {
			var got []string
			for _, m := range sess.Messages {
				got = append(got, m.UUID)
			}
			t.Fatalf("messages = %v, want only the original message ($set without a messages array must not clear)", got)
		}
	})

	t.Run("id-colliding messages array replays without stale index", func(t *testing.T) {
		// gemini-cli compaction re-emits ORIGINAL message objects under their
		// original ids, so a $set.messages array routinely carries an id seen
		// before the rewrite. The replay must reset byID with the slice;
		// without the reset, the stale index addresses the cleared slice and
		// panics (index out of range) instead of replaying.
		path := filepath.Join(t.TempDir(), "session-set-collision.jsonl")
		lines := []string{
			`{"sessionId":"d4e7203e-8840-4c58-91f1-5ab32f11e809","projectHash":"hash","startTime":"2026-06-10T10:00:00.000Z","lastUpdated":"2026-06-10T10:00:09.000Z","kind":"main"}`,
			`{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"user","content":"pre one"}`,
			`{"id":"b25da806-1959-4f73-ae67-a512dead6002","timestamp":"2026-06-10T10:00:02.000Z","type":"gemini","content":"pre two","model":"gemini-3-pro"}`,
			`{"$set":{"messages":[{"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:03.000Z","type":"user","content":"replayed one"},{"id":"c25da806-1959-4f73-ae67-a512dead6003","timestamp":"2026-06-10T10:00:04.000Z","type":"gemini","content":"replayed three","model":"gemini-3-pro"}]}}`,
			`{"id":"d25da806-1959-4f73-ae67-a512dead6004","timestamp":"2026-06-10T10:00:05.000Z","type":"user","content":"after compaction"}`,
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		sess, err := ReadGeminiFile(path, 0)
		if err != nil {
			t.Fatalf("ReadGeminiFile: %v", err)
		}
		want := []string{
			"a25da806-1959-4f73-ae67-a512dead6001",
			"c25da806-1959-4f73-ae67-a512dead6003",
			"d25da806-1959-4f73-ae67-a512dead6004",
		}
		var got []string
		for _, m := range sess.Messages {
			got = append(got, m.UUID)
		}
		if len(got) != len(want) {
			t.Fatalf("messages = %v, want %v (replayed colliding id + new id + post-$set append)", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("message %d = %q, want %q", i, got[i], want[i])
			}
		}
	})
}

func TestReadGeminiFileJSONL(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "session-legacy.json")
	jsonlPath := filepath.Join(dir, "session-jsonl.jsonl")

	legacy := `{
  "sessionId": "24e7203e-8840-4c58-91f1-5ab32f11e899",
  "projectHash": "hash",
  "startTime": "2026-06-10T10:00:00.000Z",
  "lastUpdated": "2026-06-10T10:00:06.000Z",
  "messages": [
    {"id":"a25da806-1959-4f73-ae67-a512dead6001","timestamp":"2026-06-10T10:00:01.000Z","type":"user","content":"hello"},
    {"id":"b25da806-1959-4f73-ae67-a512dead6002","timestamp":"2026-06-10T10:00:05.000Z","type":"gemini","content":"answer","model":"gemini-3-pro","tokens":{"input":900,"output":60,"cached":800,"thoughts":20,"tool":0,"total":980}}
  ],
  "kind": "main"
}`
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile(legacy): %v", err)
	}
	if err := os.WriteFile(jsonlPath, []byte(strings.Join(geminiJSONLUsageLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(jsonl): %v", err)
	}

	legacySess, err := ReadGeminiFile(legacyPath, 0)
	if err != nil {
		t.Fatalf("ReadGeminiFile(legacy): %v", err)
	}
	jsonlSess, err := ReadGeminiFile(jsonlPath, 0)
	if err != nil {
		t.Fatalf("ReadGeminiFile(jsonl): %v", err)
	}

	if jsonlSess.ID != "24e7203e-8840-4c58-91f1-5ab32f11e899" {
		t.Errorf("jsonl session ID = %q, want metadata sessionId", jsonlSess.ID)
	}
	if legacySess.ID != jsonlSess.ID {
		t.Errorf("session IDs differ: legacy %q vs jsonl %q", legacySess.ID, jsonlSess.ID)
	}
	if len(jsonlSess.Messages) != len(legacySess.Messages) {
		t.Fatalf("jsonl parsed %d messages, legacy %d — same conversation must normalize equivalently (re-pushed ids collapse, $set and torn lines skipped)",
			len(jsonlSess.Messages), len(legacySess.Messages))
	}
	for i := range legacySess.Messages {
		lm, jm := legacySess.Messages[i], jsonlSess.Messages[i]
		if lm.UUID != jm.UUID {
			t.Errorf("message %d UUID: legacy %q vs jsonl %q", i, lm.UUID, jm.UUID)
		}
		if lm.Type != jm.Type {
			t.Errorf("message %d Type: legacy %q vs jsonl %q", i, lm.Type, jm.Type)
		}
		if string(lm.Message) != string(jm.Message) {
			t.Errorf("message %d Message: legacy %s vs jsonl %s", i, lm.Message, jm.Message)
		}
	}
}
