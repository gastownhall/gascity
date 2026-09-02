package sessionlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTailUsageReturnsEntriesInFileOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	writeTailJSONL(t, path, []map[string]any{
		{
			"type": "assistant",
			"uuid": "u1",
			"message": map[string]any{
				"role":  "assistant",
				"model": "claude-opus-4-7",
				"usage": map[string]any{
					"input_tokens":                100,
					"output_tokens":               50,
					"cache_read_input_tokens":     2000,
					"cache_creation_input_tokens": 800,
				},
			},
		},
		{
			"type":    "user",
			"uuid":    "user-1",
			"message": map[string]any{"role": "user", "content": "next"},
		},
		{
			"type": "assistant",
			"uuid": "u2",
			"message": map[string]any{
				"role":  "assistant",
				"model": "claude-opus-4-8",
				"usage": map[string]any{
					"input_tokens":                7,
					"output_tokens":               3,
					"cache_read_input_tokens":     11,
					"cache_creation_input_tokens": 5,
				},
			},
		},
	})

	usages, err := ExtractTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractTailUsage: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("ExtractTailUsage = %d entries, want 2: %+v", len(usages), usages)
	}
	want := []TailUsage{
		{EntryUUID: "u1", Model: "claude-opus-4-7", InputTokens: 100, OutputTokens: 50, CacheReadTokens: 2000, CacheCreationTokens: 800},
		{EntryUUID: "u2", Model: "claude-opus-4-8", InputTokens: 7, OutputTokens: 3, CacheReadTokens: 11, CacheCreationTokens: 5},
	}
	for i, w := range want {
		if usages[i] != w {
			t.Errorf("usages[%d] = %+v, want %+v", i, usages[i], w)
		}
	}
}

// TestExtractTailUsageCollapsesContentBlockEntriesByMessageID pins the real
// Claude transcript shape: one assistant JSONL entry is written PER CONTENT
// BLOCK of a single API response, each with a distinct entry uuid but the
// SAME message.id and an identical copy of message.usage. Counting each
// entry would systematically inflate token/cost metrics ~2x; the extractor
// must emit one TailUsage per message.id (last entry wins), with entries
// lacking a message.id standing alone.
func TestExtractTailUsageCollapsesContentBlockEntriesByMessageID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	usage := map[string]any{
		"input_tokens":                100,
		"output_tokens":               50,
		"cache_read_input_tokens":     2000,
		"cache_creation_input_tokens": 800,
	}
	writeTailJSONL(t, path, []map[string]any{
		// One API response split across three content-block entries:
		// identical message.id and identical usage on every copy.
		{
			"type": "assistant",
			"uuid": "u1",
			"message": map[string]any{
				"id": "msg_01AAA", "role": "assistant",
				"model": "claude-opus-4-7", "usage": usage,
			},
		},
		{
			"type": "assistant",
			"uuid": "u2",
			"message": map[string]any{
				"id": "msg_01AAA", "role": "assistant",
				"model": "claude-opus-4-7", "usage": usage,
			},
		},
		{
			"type": "assistant",
			"uuid": "u3",
			"message": map[string]any{
				"id": "msg_01AAA", "role": "assistant",
				"model": "claude-opus-4-7", "usage": usage,
			},
		},
		// A second, distinct invocation.
		{
			"type": "assistant",
			"uuid": "u4",
			"message": map[string]any{
				"id": "msg_01BBB", "role": "assistant",
				"model": "claude-opus-4-7",
				"usage": map[string]any{"input_tokens": 7, "output_tokens": 3},
			},
		},
		// An entry without message.id stands alone (identity = entry uuid).
		{
			"type": "assistant",
			"uuid": "u5",
			"message": map[string]any{
				"role": "assistant", "model": "claude-opus-4-7",
				"usage": map[string]any{"output_tokens": 12},
			},
		},
	})

	usages, err := ExtractTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractTailUsage: %v", err)
	}
	if len(usages) != 3 {
		t.Fatalf("ExtractTailUsage = %d entries, want 3 (content-block copies collapsed): %+v", len(usages), usages)
	}
	want := []TailUsage{
		{EntryUUID: "u3", MessageID: "msg_01AAA", Model: "claude-opus-4-7", InputTokens: 100, OutputTokens: 50, CacheReadTokens: 2000, CacheCreationTokens: 800},
		{EntryUUID: "u4", MessageID: "msg_01BBB", Model: "claude-opus-4-7", InputTokens: 7, OutputTokens: 3},
		{EntryUUID: "u5", Model: "claude-opus-4-7", OutputTokens: 12},
	}
	for i, w := range want {
		if usages[i] != w {
			t.Errorf("usages[%d] = %+v, want %+v", i, usages[i], w)
		}
	}
}

func TestExtractTailUsageSkipsEntriesWithoutUUIDOrUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	writeTailJSONL(t, path, []map[string]any{
		// Assistant entry without uuid — skipped.
		{
			"type": "assistant",
			"message": map[string]any{
				"role":  "assistant",
				"model": "claude-opus-4-7",
				"usage": map[string]any{"input_tokens": 100},
			},
		},
		// Assistant entry with uuid but zero usage — skipped.
		{
			"type": "assistant",
			"uuid": "zero-usage",
			"message": map[string]any{
				"role":  "assistant",
				"model": "claude-opus-4-7",
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		},
		// Assistant entry with uuid but no usage object — skipped.
		{
			"type": "assistant",
			"uuid": "no-usage",
			"message": map[string]any{
				"role":  "assistant",
				"model": "claude-opus-4-7",
			},
		},
		// Valid entry — kept.
		{
			"type": "assistant",
			"uuid": "u9",
			"message": map[string]any{
				"role":  "assistant",
				"model": "claude-opus-4-7",
				"usage": map[string]any{"output_tokens": 12},
			},
		},
	})

	usages, err := ExtractTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractTailUsage: %v", err)
	}
	if len(usages) != 1 {
		t.Fatalf("ExtractTailUsage = %d entries, want 1: %+v", len(usages), usages)
	}
	if usages[0].EntryUUID != "u9" || usages[0].OutputTokens != 12 {
		t.Errorf("usages[0] = %+v, want EntryUUID=u9 OutputTokens=12", usages[0])
	}
}

func TestExtractTailUsageToleratesMalformedLinesAndStringMessages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Mix a malformed line with a string-encoded message entry.
	content := `{"type":"assistant","uuid":"u1","message":{"model":"claude-opus-4-7","usage":{"input_tokens":10,"output_tokens":4}}}
this is not json at all
{"type":"assistant","uuid":"u2","message":"{\"model\":\"claude-opus-4-7\",\"usage\":{\"input_tokens\":20,\"output_tokens\":8}}"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usages, err := ExtractTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractTailUsage: %v", err)
	}
	if len(usages) != 2 {
		t.Fatalf("ExtractTailUsage = %d entries, want 2: %+v", len(usages), usages)
	}
	if usages[0].EntryUUID != "u1" || usages[0].InputTokens != 10 {
		t.Errorf("usages[0] = %+v, want u1 with input 10", usages[0])
	}
	if usages[1].EntryUUID != "u2" || usages[1].InputTokens != 20 || usages[1].OutputTokens != 8 {
		t.Errorf("usages[1] = %+v, want u2 with input 20, output 8", usages[1])
	}
}

func TestExtractTailUsageEmptyAndMissingFiles(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	usages, err := ExtractTailUsage(empty)
	if err != nil {
		t.Fatalf("ExtractTailUsage(empty): %v", err)
	}
	if len(usages) != 0 {
		t.Fatalf("ExtractTailUsage(empty) = %+v, want none", usages)
	}

	noUsage := filepath.Join(dir, "no-usage.jsonl")
	writeTailJSONL(t, noUsage, []map[string]any{
		{"type": "user", "uuid": "user-1", "message": map[string]any{"role": "user", "content": "hi"}},
	})
	usages, err = ExtractTailUsage(noUsage)
	if err != nil {
		t.Fatalf("ExtractTailUsage(no usage): %v", err)
	}
	if len(usages) != 0 {
		t.Fatalf("ExtractTailUsage(no usage) = %+v, want none", usages)
	}

	if _, err := ExtractTailUsage(filepath.Join(dir, "absent.jsonl")); err == nil {
		t.Fatal("ExtractTailUsage(missing file) = nil error, want error")
	}
}

func TestExtractTailUsageFromSearchPathsRejectsEscapedPath(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "session.jsonl")
	writeTailJSONL(t, outside, []map[string]any{{
		"type": "assistant",
		"uuid": "u1",
		"message": map[string]any{
			"model": "claude-opus-4-7",
			"usage": map[string]any{"input_tokens": 1},
		},
	}})

	if _, err := ExtractTailUsageFromSearchPaths([]string{root}, outside); err == nil {
		t.Fatal("ExtractTailUsageFromSearchPaths outside root = nil error, want rejection")
	}

	inside := filepath.Join(root, "session.jsonl")
	writeTailJSONL(t, inside, []map[string]any{{
		"type": "assistant",
		"uuid": "u1",
		"message": map[string]any{
			"model": "claude-opus-4-7",
			"usage": map[string]any{"input_tokens": 1},
		},
	}})
	usages, err := ExtractTailUsageFromSearchPaths([]string{root}, inside)
	if err != nil {
		t.Fatalf("ExtractTailUsageFromSearchPaths(inside root): %v", err)
	}
	if len(usages) != 1 || usages[0].EntryUUID != "u1" {
		t.Fatalf("ExtractTailUsageFromSearchPaths = %+v, want one u1 entry", usages)
	}
}

// buildUsageEntry returns one assistant entry whose padding makes the encoded
// line large enough that a handful of entries exceed the fixed tail window.
func buildUsageEntry(id string, padBytes int) map[string]any {
	return map[string]any{
		"type": "assistant",
		"uuid": "uuid-" + id,
		"message": map[string]any{
			"role":  "assistant",
			"id":    id,
			"model": "claude-opus-4-8",
			"usage": map[string]any{
				"input_tokens":                10,
				"output_tokens":               20,
				"cache_read_input_tokens":     30,
				"cache_creation_input_tokens": 40,
			},
			"content": strings.Repeat("x", padBytes),
		},
	}
}

// TestExtractTailUsageSinceReachesBackPastTheFixedWindow pins the defect that
// the fixed 64KB window drops every invocation that scrolled past it between
// two extractions. The transcript below is many windows long, so the fixed
// extractor cannot see the cursor entry or anything after it up to the last
// window; the cursor-aware extractor must return them.
func TestExtractTailUsageSinceReachesBackPastTheFixedWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	const entries = 60
	const pad = 20 * 1024 // ~20KB per entry: 60 entries ≈ 1.2MB, ~19 windows
	rows := make([]map[string]any, 0, entries)
	for i := range entries {
		rows = append(rows, buildUsageEntry(fmt.Sprintf("msg_%02d", i), pad))
	}
	writeTailJSONL(t, path, rows)

	// The cursor is the FIRST invocation: everything after it is owed.
	cursor := "msg_00"

	fixed, err := ExtractTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractTailUsage: %v", err)
	}
	if containsCursor(fixed, cursor) {
		t.Fatalf("test is not exercising the defect: fixed window still reaches the cursor (%d entries)", len(fixed))
	}

	got, err := ExtractTailUsageSince(path, cursor)
	if err != nil {
		t.Fatalf("ExtractTailUsageSince: %v", err)
	}
	if !containsCursor(got, cursor) {
		t.Fatalf("cursor %q not reached: got %d entries, want the window grown to include it", cursor, len(got))
	}
	if len(got) != entries {
		t.Errorf("got %d invocations, want all %d from the cursor onward", len(got), entries)
	}
	if len(got) <= len(fixed) {
		t.Errorf("cursor-aware scan returned %d entries, fixed window returned %d; want strictly more",
			len(got), len(fixed))
	}
}

// TestExtractTailUsageSinceEmptyCursorMatchesFixedWindow pins that a session
// with no prior extraction keeps the old bounded behavior.
func TestExtractTailUsageSinceEmptyCursorMatchesFixedWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	rows := make([]map[string]any, 0, 40)
	for i := range 40 {
		rows = append(rows, buildUsageEntry(fmt.Sprintf("msg_%02d", i), 20*1024))
	}
	writeTailJSONL(t, path, rows)

	fixed, err := ExtractTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractTailUsage: %v", err)
	}
	got, err := ExtractTailUsageSince(path, "")
	if err != nil {
		t.Fatalf("ExtractTailUsageSince: %v", err)
	}
	if len(got) != len(fixed) {
		t.Errorf("empty cursor returned %d entries, want the fixed-window %d", len(got), len(fixed))
	}
}

// TestExtractTailUsageSinceUnreachableCursorDegradesToBoundedWindow pins that a
// stale or absent cursor returns the widest scanned window rather than nothing.
func TestExtractTailUsageSinceUnreachableCursorDegradesToBoundedWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	rows := make([]map[string]any, 0, 30)
	for i := range 30 {
		rows = append(rows, buildUsageEntry(fmt.Sprintf("msg_%02d", i), 20*1024))
	}
	writeTailJSONL(t, path, rows)

	got, err := ExtractTailUsageSince(path, "msg_does_not_exist")
	if err != nil {
		t.Fatalf("ExtractTailUsageSince: %v", err)
	}
	// The whole file (~600KB) is well under the cap, so the window grows to
	// cover it and returns everything instead of failing.
	if len(got) != 30 {
		t.Errorf("got %d entries, want all 30 once the window covers the file", len(got))
	}
}
