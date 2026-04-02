package sessionlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- helpers ---

// writeJSONL writes lines to a temporary JSONL file and returns the path.
func writeJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-session.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test cleanup
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return path
}

// --- Entry tests ---

func TestIsCompactBoundary(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
		want  bool
	}{
		{"compact boundary", Entry{Type: "system", Subtype: "compact_boundary"}, true},
		{"system init", Entry{Type: "system", Subtype: "init"}, false},
		{"assistant", Entry{Type: "assistant"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.entry.IsCompactBoundary(); got != tt.want {
				t.Errorf("IsCompactBoundary() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestContentBlocks(t *testing.T) {
	// Assistant message with tool_use block.
	msg := `{"role":"assistant","content":[{"type":"tool_use","id":"tu_123","name":"Read","input":{"path":"/tmp/a"}}]}`
	e := &Entry{Message: json.RawMessage(msg)}
	blocks := e.ContentBlocks()
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].Type != "tool_use" {
		t.Errorf("block type = %q, want %q", blocks[0].Type, "tool_use")
	}
	if blocks[0].ID != "tu_123" {
		t.Errorf("block id = %q, want %q", blocks[0].ID, "tu_123")
	}
	if blocks[0].Name != "Read" {
		t.Errorf("block name = %q, want %q", blocks[0].Name, "Read")
	}
}

func TestContentBlocksPlainString(t *testing.T) {
	msg := `{"role":"user","content":"hello world"}`
	e := &Entry{Message: json.RawMessage(msg)}
	blocks := e.ContentBlocks()
	if blocks != nil {
		t.Errorf("expected nil blocks for plain string content, got %d", len(blocks))
	}
}

func TestContentBlocksEmpty(t *testing.T) {
	e := &Entry{}
	if blocks := e.ContentBlocks(); blocks != nil {
		t.Errorf("expected nil blocks for empty message, got %d", len(blocks))
	}
}

func TestTextContent(t *testing.T) {
	msg := `{"role":"user","content":"hello world"}`
	e := &Entry{Message: json.RawMessage(msg)}
	if got := e.TextContent(); got != "hello world" {
		t.Errorf("TextContent() = %q, want %q", got, "hello world")
	}
}

func TestTextContentArray(t *testing.T) {
	msg := `{"role":"assistant","content":[{"type":"text","text":"hi"}]}`
	e := &Entry{Message: json.RawMessage(msg)}
	if got := e.TextContent(); got != "" {
		t.Errorf("TextContent() should return empty for array content, got %q", got)
	}
}

// --- DAG tests ---

func TestBuildDagLinearConversation(t *testing.T) {
	entries := []*Entry{
		{UUID: "a", ParentUUID: "", Type: "user", Timestamp: mustTime("2025-01-01T00:00:00Z")},
		{UUID: "b", ParentUUID: "a", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:01Z")},
		{UUID: "c", ParentUUID: "b", Type: "user", Timestamp: mustTime("2025-01-01T00:00:02Z")},
		{UUID: "d", ParentUUID: "c", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:03Z")},
	}
	dag := BuildDag(entries)
	if len(dag.ActiveBranch) != 4 {
		t.Fatalf("got %d entries, want 4", len(dag.ActiveBranch))
	}
	// Should be root → tip order.
	if dag.ActiveBranch[0].UUID != "a" {
		t.Errorf("first = %q, want %q", dag.ActiveBranch[0].UUID, "a")
	}
	if dag.ActiveBranch[3].UUID != "d" {
		t.Errorf("last = %q, want %q", dag.ActiveBranch[3].UUID, "d")
	}
	if dag.HasBranches {
		t.Error("expected no branches in linear conversation")
	}
}

func TestBuildDagBranching(t *testing.T) {
	// Fork: a → b1 (older) and a → b2 (newer).
	entries := []*Entry{
		{UUID: "a", ParentUUID: "", Type: "user", Timestamp: mustTime("2025-01-01T00:00:00Z")},
		{UUID: "b1", ParentUUID: "a", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:01Z")},
		{UUID: "b2", ParentUUID: "a", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:02Z")},
	}
	dag := BuildDag(entries)
	if !dag.HasBranches {
		t.Error("expected HasBranches to be true")
	}
	// Active branch should follow the newer tip (b2).
	if len(dag.ActiveBranch) != 2 {
		t.Fatalf("got %d entries, want 2", len(dag.ActiveBranch))
	}
	if dag.ActiveBranch[1].UUID != "b2" {
		t.Errorf("tip = %q, want %q", dag.ActiveBranch[1].UUID, "b2")
	}
}

func TestBuildDagBranchingLongerWins(t *testing.T) {
	// Same timestamp on both tips, but one branch is longer.
	entries := []*Entry{
		{UUID: "a", ParentUUID: "", Type: "user", Timestamp: mustTime("2025-01-01T00:00:00Z")},
		{UUID: "b1", ParentUUID: "a", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:01Z")},
		{UUID: "c1", ParentUUID: "b1", Type: "user", Timestamp: mustTime("2025-01-01T00:00:02Z")},
		{UUID: "b2", ParentUUID: "a", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:02Z")},
	}
	dag := BuildDag(entries)
	// c1 branch is longer (3 nodes) vs b2 branch (2 nodes), same tip timestamp.
	if dag.ActiveBranch[len(dag.ActiveBranch)-1].UUID != "c1" {
		t.Errorf("tip = %q, want %q (longer branch)", dag.ActiveBranch[len(dag.ActiveBranch)-1].UUID, "c1")
	}
}

func TestBuildDagCompactBoundary(t *testing.T) {
	// Compaction: a → b, then compact_boundary c with logicalParentUuid=b, then d.
	entries := []*Entry{
		{UUID: "a", ParentUUID: "", Type: "user", Timestamp: mustTime("2025-01-01T00:00:00Z")},
		{UUID: "b", ParentUUID: "a", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:01Z")},
		{
			UUID: "c", ParentUUID: "", Type: "system", Subtype: "compact_boundary",
			LogicalParentUUID: "b", Timestamp: mustTime("2025-01-01T00:00:02Z"),
		},
		{UUID: "d", ParentUUID: "c", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:03Z")},
	}
	dag := BuildDag(entries)
	// Active branch should follow: a → b → c → d (via logicalParentUuid).
	if len(dag.ActiveBranch) != 4 {
		t.Fatalf("got %d entries, want 4", len(dag.ActiveBranch))
	}
	if dag.ActiveBranch[0].UUID != "a" {
		t.Errorf("first = %q, want %q", dag.ActiveBranch[0].UUID, "a")
	}
	if dag.ActiveBranch[3].UUID != "d" {
		t.Errorf("last = %q, want %q", dag.ActiveBranch[3].UUID, "d")
	}
	if dag.CompactionCount != 1 {
		t.Errorf("compaction count = %d, want 1", dag.CompactionCount)
	}
}

func TestBuildDagOrphanedToolUse(t *testing.T) {
	// tool_use with no matching tool_result anywhere.
	msg := `{"role":"assistant","content":[{"type":"tool_use","id":"tu_orphan","name":"Bash"}]}`
	entries := []*Entry{
		{UUID: "a", ParentUUID: "", Type: "user", Timestamp: mustTime("2025-01-01T00:00:00Z")},
		{
			UUID: "b", ParentUUID: "a", Type: "assistant", Message: json.RawMessage(msg),
			Timestamp: mustTime("2025-01-01T00:00:01Z"),
		},
	}
	dag := BuildDag(entries)
	if dag.OrphanedToolUseIDs == nil || !dag.OrphanedToolUseIDs["tu_orphan"] {
		t.Error("expected tu_orphan in OrphanedToolUseIDs")
	}
}

func TestBuildDagMatchedToolUse(t *testing.T) {
	// tool_use with matching tool_result — should NOT be orphaned.
	assistMsg := `{"role":"assistant","content":[{"type":"tool_use","id":"tu_match","name":"Read"}]}`
	resultMsg := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_match","content":"file data"}]}`
	entries := []*Entry{
		{UUID: "a", ParentUUID: "", Type: "user", Timestamp: mustTime("2025-01-01T00:00:00Z")},
		{
			UUID: "b", ParentUUID: "a", Type: "assistant", Message: json.RawMessage(assistMsg),
			Timestamp: mustTime("2025-01-01T00:00:01Z"),
		},
		{
			UUID: "c", ParentUUID: "b", Type: "result", Message: json.RawMessage(resultMsg),
			Timestamp: mustTime("2025-01-01T00:00:02Z"),
		},
	}
	dag := BuildDag(entries)
	if len(dag.OrphanedToolUseIDs) != 0 {
		t.Errorf("expected no orphaned tool uses, got %v", dag.OrphanedToolUseIDs)
	}
}

func TestBuildDagEmpty(t *testing.T) {
	dag := BuildDag(nil)
	if len(dag.ActiveBranch) != 0 {
		t.Errorf("expected empty active branch, got %d", len(dag.ActiveBranch))
	}
}

func TestBuildDagSkipsNoUUID(t *testing.T) {
	entries := []*Entry{
		{UUID: "", Type: "file-history-snapshot"},
		{UUID: "a", ParentUUID: "", Type: "user", Timestamp: mustTime("2025-01-01T00:00:00Z")},
	}
	dag := BuildDag(entries)
	if len(dag.ActiveBranch) != 1 {
		t.Fatalf("got %d entries, want 1 (skipping no-UUID)", len(dag.ActiveBranch))
	}
	if dag.ActiveBranch[0].UUID != "a" {
		t.Errorf("got %q, want %q", dag.ActiveBranch[0].UUID, "a")
	}
}

// --- parseFile tests ---

func TestParseFile(t *testing.T) {
	path := writeJSONL(t,
		`{"uuid":"a","parentUuid":"","type":"user","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"b","parentUuid":"a","type":"assistant","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	entries, err := parseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].UUID != "a" {
		t.Errorf("first uuid = %q, want %q", entries[0].UUID, "a")
	}
	// Raw should be preserved.
	if len(entries[0].Raw) == 0 {
		t.Error("expected Raw to be preserved")
	}
}

func TestParseFileSkipsMalformed(t *testing.T) {
	path := writeJSONL(t,
		`not json`,
		`{"uuid":"a","type":"user","timestamp":"2025-01-01T00:00:00Z"}`,
		``,
		`{"uuid":"b","type":"assistant","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	entries, err := parseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (skipping malformed and empty)", len(entries))
	}
}

func TestParseFileMissing(t *testing.T) {
	_, err := parseFile(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// --- ReadFile tests ---

func TestReadFileLinear(t *testing.T) {
	path := writeJSONL(t,
		`{"uuid":"a","parentUuid":"","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"b","parentUuid":"a","type":"assistant","message":{"role":"assistant","content":"hi"},"timestamp":"2025-01-01T00:00:01Z"}`,
	)
	sess, err := ReadFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(sess.Messages))
	}
	if sess.ID != "test-session" {
		t.Errorf("session id = %q, want %q", sess.ID, "test-session")
	}
}

func TestReadFileFiltersDisplayTypes(t *testing.T) {
	path := writeJSONL(t,
		`{"uuid":"a","parentUuid":"","type":"user","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"b","parentUuid":"a","type":"assistant","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"c","parentUuid":"b","type":"progress","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"d","parentUuid":"c","type":"result","timestamp":"2025-01-01T00:00:03Z"}`,
	)
	sess, err := ReadFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// progress should be filtered out; user, assistant, result kept.
	if len(sess.Messages) != 3 {
		t.Fatalf("got %d messages, want 3 (progress filtered out)", len(sess.Messages))
	}
	for _, m := range sess.Messages {
		if m.Type == "progress" {
			t.Error("progress type should be filtered out")
		}
	}
}

// --- Pagination tests ---

func TestSliceAtCompactBoundariesNoBoundaries(t *testing.T) {
	entries := makeEntries("a", "b", "c", "d")
	sliced, info := sliceAtCompactBoundaries(entries, 1, "")
	if len(sliced) != 4 {
		t.Fatalf("got %d, want all 4 (no boundaries to slice at)", len(sliced))
	}
	if info.HasOlderMessages {
		t.Error("should not have older messages")
	}
	if info.TotalCompactions != 0 {
		t.Errorf("total compactions = %d, want 0", info.TotalCompactions)
	}
}

func TestSliceAtCompactBoundariesOneBoundary(t *testing.T) {
	entries := []*Entry{
		{UUID: "a", Type: "user"},
		{UUID: "b", Type: "assistant"},
		{UUID: "cb1", Type: "system", Subtype: "compact_boundary"},
		{UUID: "c", Type: "user"},
		{UUID: "cb2", Type: "system", Subtype: "compact_boundary"},
		{UUID: "d", Type: "assistant"},
	}

	// tailCompactions=1 with 2 boundaries → slice from the last boundary.
	sliced, info := sliceAtCompactBoundaries(entries, 1, "")
	if len(sliced) != 2 {
		t.Fatalf("got %d, want 2 (from cb2 to end)", len(sliced))
	}
	if sliced[0].UUID != "cb2" {
		t.Errorf("first = %q, want %q", sliced[0].UUID, "cb2")
	}
	if !info.HasOlderMessages {
		t.Error("expected HasOlderMessages")
	}
	if info.TruncatedBeforeMessage != "cb2" {
		t.Errorf("truncated before = %q, want %q", info.TruncatedBeforeMessage, "cb2")
	}
	if info.TotalCompactions != 2 {
		t.Errorf("total compactions = %d, want 2", info.TotalCompactions)
	}
}

func TestSliceAtCompactBoundariesReturnsAllWhenFewer(t *testing.T) {
	entries := []*Entry{
		{UUID: "a", Type: "user"},
		{UUID: "cb", Type: "system", Subtype: "compact_boundary"},
		{UUID: "b", Type: "assistant"},
	}

	// 1 boundary, tailCompactions=1 → len(boundaries) <= tailCompactions → return all.
	sliced, info := sliceAtCompactBoundaries(entries, 1, "")
	if len(sliced) != 3 {
		t.Fatalf("got %d, want 3 (all entries returned when boundaries <= tailCompactions)", len(sliced))
	}
	if info.HasOlderMessages {
		t.Error("should not have older messages")
	}
	if info.TotalCompactions != 1 {
		t.Errorf("total compactions = %d, want 1", info.TotalCompactions)
	}
}

func TestSliceAtCompactBoundariesMultiple(t *testing.T) {
	entries := []*Entry{
		{UUID: "a", Type: "user"},
		{UUID: "cb1", Type: "system", Subtype: "compact_boundary"},
		{UUID: "b", Type: "assistant"},
		{UUID: "cb2", Type: "system", Subtype: "compact_boundary"},
		{UUID: "c", Type: "user"},
		{UUID: "cb3", Type: "system", Subtype: "compact_boundary"},
		{UUID: "d", Type: "assistant"},
	}

	// tailCompactions=2 → include from the 2nd-from-last boundary.
	sliced, info := sliceAtCompactBoundaries(entries, 2, "")
	if len(sliced) != 4 {
		t.Fatalf("got %d, want 4", len(sliced))
	}
	if sliced[0].UUID != "cb2" {
		t.Errorf("first = %q, want %q", sliced[0].UUID, "cb2")
	}
	if info.TotalCompactions != 3 {
		t.Errorf("total compactions = %d, want 3", info.TotalCompactions)
	}
}

func TestSliceAtCompactBoundariesBeforeCursor(t *testing.T) {
	entries := []*Entry{
		{UUID: "a", Type: "user"},
		{UUID: "cb1", Type: "system", Subtype: "compact_boundary"},
		{UUID: "b", Type: "assistant"},
		{UUID: "cb2", Type: "system", Subtype: "compact_boundary"},
		{UUID: "c", Type: "user"},
	}

	// Load older messages before "cb2".
	sliced, info := sliceAtCompactBoundaries(entries, 1, "cb2")
	// Working set is [a, cb1, b] — 1 boundary, tailCompactions=1 → return all.
	if len(sliced) != 3 {
		t.Fatalf("got %d, want 3 (all working set when boundaries <= tailCompactions)", len(sliced))
	}
	if sliced[0].UUID != "a" {
		t.Errorf("first = %q, want %q", sliced[0].UUID, "a")
	}
	if info.HasOlderMessages {
		t.Error("should not have older messages (only 1 boundary in working set)")
	}
}

func TestSliceAtCompactBoundariesBeforeCursorWithSlicing(t *testing.T) {
	entries := []*Entry{
		{UUID: "a", Type: "user"},
		{UUID: "cb1", Type: "system", Subtype: "compact_boundary"},
		{UUID: "b", Type: "assistant"},
		{UUID: "cb2", Type: "system", Subtype: "compact_boundary"},
		{UUID: "c", Type: "user"},
		{UUID: "cb3", Type: "system", Subtype: "compact_boundary"},
		{UUID: "d", Type: "assistant"},
	}

	// Load older before "cb3". Working set: [a, cb1, b, cb2, c].
	// 2 boundaries in working set, tailCompactions=1 → slice from cb2.
	sliced, info := sliceAtCompactBoundaries(entries, 1, "cb3")
	if len(sliced) != 2 {
		t.Fatalf("got %d, want 2", len(sliced))
	}
	if sliced[0].UUID != "cb2" {
		t.Errorf("first = %q, want %q", sliced[0].UUID, "cb2")
	}
	if !info.HasOlderMessages {
		t.Error("expected HasOlderMessages")
	}
}

// --- FindSessionFile tests ---

func TestFindSessionFile(t *testing.T) {
	base := t.TempDir()
	slug := ProjectSlug("/home/user/myproject")
	dir := filepath.Join(base, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create two session files; the newer one should be returned.
	older := filepath.Join(dir, "old-session.jsonl")
	newer := filepath.Join(dir, "new-session.jsonl")
	if err := os.WriteFile(older, []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ensure different mod times.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := FindSessionFile([]string{base}, "/home/user/myproject")
	if got != newer {
		t.Errorf("got %q, want %q", got, newer)
	}
}

func TestFindSessionFileNotFound(t *testing.T) {
	got := FindSessionFile([]string{t.TempDir()}, "/nonexistent/path")
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestProjectSlug(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/home/user/project", "-home-user-project"},
		{"/data/projects/gascity", "-data-projects-gascity"},
		{"/home/user/.hidden/dir", "-home-user--hidden-dir"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := ProjectSlug(tt.path); got != tt.want {
				t.Errorf("ProjectSlug(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// --- ReadFile with pagination ---

func TestReadFileWithPagination(t *testing.T) {
	// Need 2 compact boundaries so tailCompactions=1 triggers slicing.
	path := writeJSONL(t,
		`{"uuid":"a","parentUuid":"","type":"user","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"cb1","parentUuid":"a","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"b","parentUuid":"cb1","type":"assistant","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"cb2","parentUuid":"b","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:03Z"}`,
		`{"uuid":"c","parentUuid":"cb2","type":"user","timestamp":"2025-01-01T00:00:04Z"}`,
		`{"uuid":"d","parentUuid":"c","type":"assistant","timestamp":"2025-01-01T00:00:05Z"}`,
	)
	sess, err := ReadFile(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Pagination == nil {
		t.Fatal("expected pagination info")
	}
	if !sess.Pagination.HasOlderMessages {
		t.Error("expected HasOlderMessages")
	}
	// Should slice from cb2 onward. Display types in that range: system(cb2), user, assistant.
	if len(sess.Messages) == 0 {
		t.Fatal("expected messages")
	}
	if sess.Messages[0].UUID != "cb2" {
		t.Errorf("first message = %q, want %q", sess.Messages[0].UUID, "cb2")
	}
}

func TestReadFileOlder(t *testing.T) {
	path := writeJSONL(t,
		`{"uuid":"a","parentUuid":"","type":"user","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"b","parentUuid":"a","type":"assistant","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"cb1","parentUuid":"b","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"c","parentUuid":"cb1","type":"user","timestamp":"2025-01-01T00:00:03Z"}`,
		`{"uuid":"cb2","parentUuid":"c","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:04Z"}`,
		`{"uuid":"d","parentUuid":"cb2","type":"assistant","timestamp":"2025-01-01T00:00:05Z"}`,
	)
	sess, err := ReadFileOlder(path, 1, "cb2")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Pagination == nil {
		t.Fatal("expected pagination info")
	}
	// Should return messages before cb2, sliced at cb1.
	found := false
	for _, m := range sess.Messages {
		if m.UUID == "d" {
			t.Error("should not contain messages after cursor")
		}
		if m.UUID == "cb1" {
			found = true
		}
	}
	if !found {
		t.Error("expected cb1 in older messages")
	}
}

// --- Edge case tests (from review findings) ---

func TestSliceAtCompactBoundariesCursorAtFirstMessage(t *testing.T) {
	entries := []*Entry{
		{UUID: "a", Type: "user"},
		{UUID: "b", Type: "assistant"},
		{UUID: "c", Type: "user"},
	}
	// Cursor at first message → should return empty working set.
	sliced, info := sliceAtCompactBoundaries(entries, 1, "a")
	if len(sliced) != 0 {
		t.Fatalf("got %d, want 0 (cursor at first message = no older messages)", len(sliced))
	}
	if info.HasOlderMessages {
		t.Error("should not have older messages when working set is empty")
	}
}

func TestSliceAtCompactBoundariesTailCompactionsZero(t *testing.T) {
	entries := []*Entry{
		{UUID: "a", Type: "user"},
		{UUID: "cb", Type: "system", Subtype: "compact_boundary"},
		{UUID: "b", Type: "assistant"},
	}
	// tailCompactions=0 should return everything (no panic).
	sliced, info := sliceAtCompactBoundaries(entries, 0, "")
	if len(sliced) != 3 {
		t.Fatalf("got %d, want 3", len(sliced))
	}
	if info.HasOlderMessages {
		t.Error("should not have older messages with tailCompactions=0")
	}
}

func TestSliceAtCompactBoundariesTailZeroWithCursor(t *testing.T) {
	entries := []*Entry{
		{UUID: "a", Type: "user"},
		{UUID: "b", Type: "assistant"},
		{UUID: "c", Type: "user"},
	}
	// tailCompactions=0 with cursor should still respect the cursor.
	sliced, info := sliceAtCompactBoundaries(entries, 0, "b")
	if len(sliced) != 1 {
		t.Fatalf("got %d, want 1 (only messages before cursor 'b')", len(sliced))
	}
	if sliced[0].UUID != "a" {
		t.Errorf("got %q, want %q", sliced[0].UUID, "a")
	}
	if info.ReturnedMessageCount != 1 {
		t.Errorf("returned count = %d, want 1", info.ReturnedMessageCount)
	}
}

func TestBuildDagTopLevelToolResult(t *testing.T) {
	// tool_use with matching top-level tool_result entry (not nested in content blocks).
	assistMsg := `{"role":"assistant","content":[{"type":"tool_use","id":"tu_top","name":"Bash"}]}`
	entries := []*Entry{
		{UUID: "a", ParentUUID: "", Type: "user", Timestamp: mustTime("2025-01-01T00:00:00Z")},
		{
			UUID: "b", ParentUUID: "a", Type: "assistant", Message: json.RawMessage(assistMsg),
			Timestamp: mustTime("2025-01-01T00:00:01Z"),
		},
		{
			UUID: "c", ParentUUID: "b", Type: "result", ToolUseID: "tu_top",
			Timestamp: mustTime("2025-01-01T00:00:02Z"),
		},
	}
	dag := BuildDag(entries)
	if len(dag.OrphanedToolUseIDs) != 0 {
		t.Errorf("expected no orphaned tool uses (top-level ToolUseID should match), got %v", dag.OrphanedToolUseIDs)
	}
}

func TestBuildDagMissingParentNoFallback(t *testing.T) {
	// When a regular message's parentUuid is missing (not a compact boundary),
	// BuildDag should stop walking rather than splicing to an unrelated node.
	entries := []*Entry{
		{UUID: "a", ParentUUID: "", Type: "user", Timestamp: mustTime("2025-01-01T00:00:00Z")},
		{UUID: "b", ParentUUID: "a", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:01Z")},
		{UUID: "c", ParentUUID: "nonexistent", Type: "user", Timestamp: mustTime("2025-01-01T00:00:02Z")},
		{UUID: "d", ParentUUID: "c", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:03Z")},
	}
	dag := BuildDag(entries)
	// Active branch should be c → d (stops at c because "nonexistent" not found
	// and c is not a compact boundary, so no fallback).
	if len(dag.ActiveBranch) != 2 {
		t.Fatalf("got %d entries, want 2 (should not fallback to unrelated node)", len(dag.ActiveBranch))
	}
	if dag.ActiveBranch[0].UUID != "c" {
		t.Errorf("first = %q, want %q", dag.ActiveBranch[0].UUID, "c")
	}
	if dag.ActiveBranch[1].UUID != "d" {
		t.Errorf("last = %q, want %q", dag.ActiveBranch[1].UUID, "d")
	}
}

func TestBuildDagFallbackOnlyForCompactBoundary(t *testing.T) {
	// Compact boundary with missing logicalParentUuid SHOULD use fallback.
	entries := []*Entry{
		{UUID: "a", ParentUUID: "", Type: "user", Timestamp: mustTime("2025-01-01T00:00:00Z")},
		{UUID: "b", ParentUUID: "a", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:01Z")},
		{
			UUID: "c", ParentUUID: "", Type: "system", Subtype: "compact_boundary",
			LogicalParentUUID: "missing_uuid", Timestamp: mustTime("2025-01-01T00:00:02Z"),
		},
		{UUID: "d", ParentUUID: "c", Type: "assistant", Timestamp: mustTime("2025-01-01T00:00:03Z")},
	}
	dag := BuildDag(entries)
	// Active branch: a → b → c → d. c's logicalParentUuid is "missing_uuid"
	// which doesn't exist, so fallback finds b (highest lineIndex before c).
	if len(dag.ActiveBranch) != 4 {
		t.Fatalf("got %d entries, want 4 (compact boundary should use fallback)", len(dag.ActiveBranch))
	}
	if dag.ActiveBranch[0].UUID != "a" {
		t.Errorf("first = %q, want %q", dag.ActiveBranch[0].UUID, "a")
	}
}

// --- Codex session file tests ---

func TestFindCodexSessionFileIn(t *testing.T) {
	sessDir := t.TempDir()
	workDir := "/data/projects/myproject"

	// Create a date-organized session file with matching cwd.
	dayDir := filepath.Join(sessDir, "2026", "01", "25")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	matchFile := filepath.Join(dayDir, "rollout-2026-01-25T07-00-00-abc123.jsonl")
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":"%s"}}`, workDir)
	if err := os.WriteFile(matchFile, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := findCodexSessionFileIn(sessDir, workDir)
	if got != matchFile {
		t.Errorf("got %q, want %q", got, matchFile)
	}
}

func TestFindCodexSessionFileInNoMatch(t *testing.T) {
	sessDir := t.TempDir()

	// Create a session file with a different cwd.
	dayDir := filepath.Join(sessDir, "2026", "01", "25")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	noMatch := filepath.Join(dayDir, "rollout-abc.jsonl")
	if err := os.WriteFile(noMatch, []byte(`{"type":"session_meta","payload":{"cwd":"/other/project"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := findCodexSessionFileIn(sessDir, "/data/projects/myproject")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestFindCodexSessionFileInPicksNewest(t *testing.T) {
	sessDir := t.TempDir()
	workDir := "/data/projects/myproject"
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":"%s"}}`, workDir)

	// Create two matching sessions in different days.
	oldDay := filepath.Join(sessDir, "2026", "01", "20")
	newDay := filepath.Join(sessDir, "2026", "02", "15")
	for _, d := range []string{oldDay, newDay} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldFile := filepath.Join(oldDay, "rollout-old.jsonl")
	newFile := filepath.Join(newDay, "rollout-new.jsonl")
	for _, f := range []string{oldFile, newFile} {
		if err := os.WriteFile(f, []byte(meta+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := findCodexSessionFileIn(sessDir, workDir)
	// Should find the one in the newest date directory (2026/02/15).
	if got != newFile {
		t.Errorf("got %q, want %q (newest date dir)", got, newFile)
	}
}

func TestFindCodexSessionFileUsesObservedRoots(t *testing.T) {
	sessDir := t.TempDir()
	workDir := "/data/projects/myproject"
	dayDir := filepath.Join(sessDir, "2026", "03", "27")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	matchFile := filepath.Join(dayDir, "rollout-current.jsonl")
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":"%s"}}`, workDir)
	if err := os.WriteFile(matchFile, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := FindCodexSessionFile([]string{sessDir}, workDir)
	if got != matchFile {
		t.Errorf("got %q, want %q", got, matchFile)
	}
}

func TestCodexSessionCWD(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.jsonl")

	// Valid session_meta.
	if err := os.WriteFile(f, []byte(`{"type":"session_meta","payload":{"cwd":"/foo/bar"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := codexSessionCWD(f); got != "/foo/bar" {
		t.Errorf("got %q, want %q", got, "/foo/bar")
	}

	// Non-session_meta first line.
	if err := os.WriteFile(f, []byte(`{"type":"response_item","payload":{}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := codexSessionCWD(f); got != "" {
		t.Errorf("expected empty for non-session_meta, got %q", got)
	}

	// Missing file.
	if got := codexSessionCWD(filepath.Join(dir, "nope.jsonl")); got != "" {
		t.Errorf("expected empty for missing file, got %q", got)
	}
}

func TestFindSessionFileFallsBackToCodex(t *testing.T) {
	// No slug-based files exist and no Codex roots match, so resolution should
	// return empty.
	got := FindSessionFile([]string{t.TempDir()}, "/nonexistent/codex/project")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestFindGeminiSessionFileUsesObservedRoots(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tmp")
	workDir := "/data/projects/myproject"
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	projects := map[string]any{
		"projects": map[string]string{
			workDir: "myproject",
		},
	}
	data, err := json.Marshal(projects)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "projects.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(root, "myproject")
	if err := os.MkdirAll(filepath.Join(projectDir, "chats"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionFile := filepath.Join(projectDir, "chats", "session-2026-03-27T09-00-abc123.json")
	session := `{"sessionId":"g-123","projectHash":"p-hash","startTime":"2026-03-27T09:00:00Z","lastUpdated":"2026-03-27T09:05:00Z","messages":[]}`
	if err := os.WriteFile(sessionFile, []byte(session), 0o644); err != nil {
		t.Fatal(err)
	}

	got := FindGeminiSessionFile([]string{root}, workDir)
	if got != sessionFile {
		t.Errorf("got %q, want %q", got, sessionFile)
	}
}

func TestReadGeminiFileConvertsMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	content := `{
		"sessionId":"g-123",
		"projectHash":"project",
		"startTime":"2026-03-27T09:00:00Z",
		"lastUpdated":"2026-03-27T09:05:00Z",
		"messages":[
			{"id":"u1","timestamp":"2026-03-27T09:00:00Z","type":"user","content":[{"text":"Review this diff"}]},
			{"id":"a1","timestamp":"2026-03-27T09:00:10Z","type":"gemini","content":"Looks good","thoughts":[{"subject":"Scan","description":"Checking regressions"}],"toolCalls":[{"id":"tool-1","name":"grep_search","args":{"pattern":"TODO"},"result":[{"functionResponse":{"id":"tool-1","response":{"output":"Found 2 matches"}}}]}]},
			{"id":"i1","timestamp":"2026-03-27T09:00:20Z","type":"info","content":"Request canceled."}
		]
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := ReadGeminiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadGeminiFile: %v", err)
	}
	if got := len(sess.Messages); got != 3 {
		t.Fatalf("messages = %d, want 3", got)
	}
	if got := sess.Messages[0].Type; got != "user" {
		t.Fatalf("first type = %q, want user", got)
	}
	if got := sess.Messages[0].TextContent(); got != "Review this diff" {
		t.Fatalf("first text = %q, want %q", got, "Review this diff")
	}
	assistantBlocks := sess.Messages[1].ContentBlocks()
	if len(assistantBlocks) != 4 {
		t.Fatalf("assistant block count = %d, want 4", len(assistantBlocks))
	}
	if assistantBlocks[0].Type != "thinking" {
		t.Fatalf("assistant first block = %q, want thinking", assistantBlocks[0].Type)
	}
	if assistantBlocks[2].Type != "tool_use" || assistantBlocks[2].Name != "grep_search" {
		t.Fatalf("assistant tool block = %#v, want grep_search tool_use", assistantBlocks[2])
	}
	if assistantBlocks[3].Type != "tool_result" || assistantBlocks[3].ToolUseID != "tool-1" {
		t.Fatalf("assistant result block = %#v, want tool_result for tool-1", assistantBlocks[3])
	}
	if got := sess.Messages[2].Type; got != "system" {
		t.Fatalf("third type = %q, want system", got)
	}
}

// --- helpers ---

func makeEntries(uuids ...string) []*Entry {
	entries := make([]*Entry, len(uuids))
	for i, id := range uuids {
		entries[i] = &Entry{UUID: id, Type: "user"}
	}
	return entries
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
