package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultAntigravitySearchPaths(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	got := DefaultAntigravitySearchPaths()
	want := filepath.Join(tmpHome, ".gemini", "antigravity-cli", "brain")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("DefaultAntigravitySearchPaths() = %v, want [%q]", got, want)
	}
}

func TestFindAntigravitySessionFileByID(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	sessionID := "18e4eb9f-1b1d-4dbc-966b-c06e3646f3c4"
	brainRoot := filepath.Join(tmpHome, ".gemini", "antigravity-cli", "brain")
	logDir := filepath.Join(brainRoot, sessionID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir setup: %v", err)
	}

	transcriptPath := filepath.Join(logDir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("fake history\n"), 0o644); err != nil {
		t.Fatalf("write file setup: %v", err)
	}

	// Test found
	got := FindAntigravitySessionFileByID(nil, "/my/workspace", sessionID)
	if got != transcriptPath {
		t.Fatalf("FindAntigravitySessionFileByID() = %q, want %q", got, transcriptPath)
	}

	// Test missing
	gotMissing := FindAntigravitySessionFileByID(nil, "/my/workspace", "non-existent-uuid")
	if gotMissing != "" {
		t.Fatalf("FindAntigravitySessionFileByID() missing case = %q, want empty", gotMissing)
	}
}

func TestFindAntigravitySessionFileByIDRejectsTraversalSessionID(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "brain")
	workDir := "/tmp/gascity/phase1/antigravity"
	for _, sessionID := range []string{"../escape", `nested\escape`, "nested/escape"} {
		logDir := filepath.Join(base, sessionID, ".system_generated", "logs")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			t.Fatalf("mkdir escaped setup for %q: %v", sessionID, err)
		}
		transcriptPath := filepath.Join(logDir, "transcript.jsonl")
		if err := os.WriteFile(transcriptPath, []byte("escaped\n"), 0o644); err != nil {
			t.Fatalf("write escaped setup for %q: %v", sessionID, err)
		}
		if got := FindAntigravitySessionFileByID([]string{base}, workDir, sessionID); got != "" {
			t.Fatalf("FindAntigravitySessionFileByID(%q) = %q, want empty", sessionID, got)
		}
	}
}

func TestFindAntigravitySessionFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	cliDir := filepath.Join(tmpHome, ".gemini", "antigravity-cli")
	brainRoot := filepath.Join(cliDir, "brain")
	historyPath := filepath.Join(cliDir, "history.jsonl")

	if err := os.MkdirAll(brainRoot, 0o755); err != nil {
		t.Fatalf("mkdir setup: %v", err)
	}

	workDir := "/Users/kevmoo/github/gascity"
	session1 := "ee5e687f-a1aa-4fdf-8e37-36c30a9183c6"
	session2 := "18e4eb9f-1b1d-4dbc-966b-c06e3646f3c4"

	// Mock conversation transcripts on disk
	for _, sessID := range []string{session1, session2} {
		logDir := filepath.Join(brainRoot, sessID, ".system_generated", "logs")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			t.Fatalf("setup logs dir: %v", err)
		}
		tPath := filepath.Join(logDir, "transcript.jsonl")
		if err := os.WriteFile(tPath, []byte("fake history turns\n"), 0o644); err != nil {
			t.Fatalf("setup transcript: %v", err)
		}
	}

	// Mock historical log index mapping (session1 is older, session2 is latest)
	historyData := []AntigravityHistoryEntry{
		{
			Workspace:      workDir,
			ConversationID: session1,
			Timestamp:      1000,
		},
		{
			Workspace:      "/other/project",
			ConversationID: "other-id",
			Timestamp:      2000,
		},
		{
			Workspace:      workDir,
			ConversationID: session2,
			Timestamp:      3000, // Latest for our workspace!
		},
	}

	hfile, err := os.Create(historyPath)
	if err != nil {
		t.Fatalf("create history file: %v", err)
	}
	for _, entry := range historyData {
		b, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := hfile.Write(append(b, '\n')); err != nil {
			t.Fatalf("write history row: %v", err)
		}
	}
	if err := hfile.Close(); err != nil {
		t.Fatalf("close history file: %v", err)
	}

	// 1. Verify resolution for target workDir picks the latest session (session2)
	got := FindAntigravitySessionFile(nil, workDir)
	wantPath := filepath.Join(brainRoot, session2, ".system_generated", "logs", "transcript.jsonl")
	if got != wantPath {
		t.Fatalf("FindAntigravitySessionFile() latest = %q, want %q", got, wantPath)
	}

	// 2. Verify resolution for non-matching workdir returns empty
	gotMissing := FindAntigravitySessionFile(nil, "/nonexistent/workspace")
	if gotMissing != "" {
		t.Fatalf("FindAntigravitySessionFile() missing case = %q, want empty", gotMissing)
	}
}

func TestFindAntigravitySessionFileScansPastLargeHistoryRows(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	cliDir := filepath.Join(tmpHome, ".gemini", "antigravity-cli")
	brainRoot := filepath.Join(cliDir, "brain")
	historyPath := filepath.Join(cliDir, "history.jsonl")
	workDir := "/Users/kevmoo/github/gascity"
	sessionID := "18e4eb9f-1b1d-4dbc-966b-c06e3646f3c4"
	logDir := filepath.Join(brainRoot, sessionID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir setup: %v", err)
	}
	transcriptPath := filepath.Join(logDir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("fake history turns\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	entry := AntigravityHistoryEntry{Workspace: workDir, ConversationID: sessionID, Timestamp: 3000}
	row, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal history: %v", err)
	}
	longMalformedRow := append([]byte(`{"ignored":"`), make([]byte, 128*1024)...)
	for i := range longMalformedRow {
		if longMalformedRow[i] == 0 {
			longMalformedRow[i] = 'x'
		}
	}
	historyBody := make([]byte, 0, len(longMalformedRow)+len(row)+2)
	historyBody = append(historyBody, longMalformedRow...)
	historyBody = append(historyBody, '\n')
	historyBody = append(historyBody, row...)
	historyBody = append(historyBody, '\n')
	if err := os.WriteFile(historyPath, historyBody, 0o644); err != nil {
		t.Fatalf("write history: %v", err)
	}

	if got := FindAntigravitySessionFile(nil, workDir); got != transcriptPath {
		t.Fatalf("FindAntigravitySessionFile() = %q, want %q", got, transcriptPath)
	}
}

func TestReadAntigravityFileDerivesConversationSessionID(t *testing.T) {
	convID := "agy-conv-7f1c"
	logDir := filepath.Join(t.TempDir(), convID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir setup: %v", err)
	}
	path := filepath.Join(logDir, "transcript.jsonl")
	body := `{"step_index":0,"type":"USER_INPUT","created_at":"2026-04-04T09:00:00Z","content":"hello"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	sess, err := ReadAntigravityFile(path, 0)
	if err != nil {
		t.Fatalf("ReadAntigravityFile: %v", err)
	}
	if sess.ID != convID {
		t.Fatalf("session id = %q, want conversation directory %q", sess.ID, convID)
	}

	flatPath := filepath.Join(t.TempDir(), "rollout-123.jsonl")
	if err := os.WriteFile(flatPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write flat transcript: %v", err)
	}
	flat, err := ReadAntigravityFile(flatPath, 0)
	if err != nil {
		t.Fatalf("ReadAntigravityFile flat: %v", err)
	}
	if flat.ID != "rollout-123" {
		t.Fatalf("flat session id = %q, want file base %q", flat.ID, "rollout-123")
	}
}

func TestReadAntigravityFileCorrelatesMultipleToolResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := `{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"checking","tool_calls":[{"name":"Read","args":{"path":"a.txt"}},{"name":"Write","args":{"path":"b.txt"}}]}` + "\n" +
		`{"step_index":2,"type":"READ_FILE","created_at":"2026-04-04T09:00:02Z","content":"contents of a"}` + "\n" +
		`{"step_index":3,"type":"WRITE_FILE","created_at":"2026-04-04T09:00:03Z","content":"wrote b"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	sess, err := ReadAntigravityFileRaw(path, 0)
	if err != nil {
		t.Fatalf("ReadAntigravityFileRaw: %v", err)
	}
	if len(sess.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(sess.Messages))
	}
	assistantBlocks := sess.Messages[0].ContentBlocks()
	if len(assistantBlocks) != 3 {
		t.Fatalf("assistant blocks = %#v, want text plus two tool uses", assistantBlocks)
	}
	if assistantBlocks[1].Type != "tool_use" || assistantBlocks[1].ID != "call-1-0" {
		t.Fatalf("first tool_use = %#v, want call-1-0", assistantBlocks[1])
	}
	if assistantBlocks[2].Type != "tool_use" || assistantBlocks[2].ID != "call-1-1" {
		t.Fatalf("second tool_use = %#v, want call-1-1", assistantBlocks[2])
	}
	firstResult := sess.Messages[1].ContentBlocks()
	if len(firstResult) != 1 || firstResult[0].Type != "tool_result" || firstResult[0].ToolUseID != "call-1-0" {
		t.Fatalf("first result blocks = %#v, want tool_result call-1-0", firstResult)
	}
	secondResult := sess.Messages[2].ContentBlocks()
	if len(secondResult) != 1 || secondResult[0].Type != "tool_result" || secondResult[0].ToolUseID != "call-1-1" {
		t.Fatalf("second result blocks = %#v, want tool_result call-1-1", secondResult)
	}
}

func TestReadAntigravityFileCorrelatesExplicitOutOfOrderToolResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := `{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"checking","tool_calls":[{"id":"call-a","name":"Read","args":{"path":"a.txt"}},{"id":"call-b","name":"Write","args":{"path":"b.txt"}}]}` + "\n" +
		`{"step_index":2,"type":"WRITE_FILE","created_at":"2026-04-04T09:00:02Z","tool_call_id":"call-b","content":"wrote b"}` + "\n" +
		`{"step_index":3,"type":"READ_FILE","created_at":"2026-04-04T09:00:03Z","call_id":"call-a","content":"contents of a"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	sess, err := ReadAntigravityFileRaw(path, 0)
	if err != nil {
		t.Fatalf("ReadAntigravityFileRaw: %v", err)
	}
	if len(sess.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(sess.Messages))
	}
	firstResult := sess.Messages[1].ContentBlocks()
	if len(firstResult) != 1 || firstResult[0].Type != "tool_result" || firstResult[0].ToolUseID != "call-b" {
		t.Fatalf("first result blocks = %#v, want tool_result call-b", firstResult)
	}
	secondResult := sess.Messages[2].ContentBlocks()
	if len(secondResult) != 1 || secondResult[0].Type != "tool_result" || secondResult[0].ToolUseID != "call-a" {
		t.Fatalf("second result blocks = %#v, want tool_result call-a", secondResult)
	}
	if sess.Messages[1].ParentUUID != sess.Messages[0].UUID || sess.Messages[2].ParentUUID != sess.Messages[1].UUID {
		t.Fatalf("parent links = [%q, %q], want linear chain through %q then %q",
			sess.Messages[1].ParentUUID, sess.Messages[2].ParentUUID, sess.Messages[0].UUID, sess.Messages[1].UUID)
	}
}

func TestReadAntigravityFileTreatsUnmatchedExplicitResultIDsAsSystem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := `{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"checking","tool_calls":[{"id":"call-a","name":"Read","args":{"path":"a.txt"}}]}` + "\n" +
		`{"step_index":2,"type":"READ_FILE","created_at":"2026-04-04T09:00:02Z","tool_call_id":"call-a","content":"contents of a"}` + "\n" +
		`{"step_index":3,"type":"READ_FILE","created_at":"2026-04-04T09:00:03Z","tool_call_id":"call-a","content":"duplicate result"}` + "\n" +
		`{"step_index":4,"type":"READ_FILE","created_at":"2026-04-04T09:00:04Z","call_id":"call-unknown","content":"unknown result"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	sess, err := ReadAntigravityFileRaw(path, 0)
	if err != nil {
		t.Fatalf("ReadAntigravityFileRaw: %v", err)
	}
	if len(sess.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(sess.Messages))
	}
	if sess.Messages[1].Type != "result" || sess.Messages[1].ToolUseID != "call-a" {
		t.Fatalf("first result = type %q tool_use_id %q, want result call-a", sess.Messages[1].Type, sess.Messages[1].ToolUseID)
	}
	if sess.Messages[2].Type != "system" || sess.Messages[2].ToolUseID != "" {
		t.Fatalf("duplicate explicit result = type %q tool_use_id %q, want unmatched system entry", sess.Messages[2].Type, sess.Messages[2].ToolUseID)
	}
	if sess.Messages[3].Type != "system" || sess.Messages[3].ToolUseID != "" {
		t.Fatalf("unknown explicit result = type %q tool_use_id %q, want unmatched system entry", sess.Messages[3].Type, sess.Messages[3].ToolUseID)
	}
	if sess.OrphanedToolUseIDs != nil {
		t.Fatalf("OrphanedToolUseIDs = %#v, want nil after matched call-a", sess.OrphanedToolUseIDs)
	}
}

func TestReadAntigravityFileReportsOpenToolUseTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := `{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"checking","tool_calls":[{"id":"call-open","name":"Read","args":{"path":"README.md"}}]}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	sess, err := ReadAntigravityFileRaw(path, 0)
	if err != nil {
		t.Fatalf("ReadAntigravityFileRaw: %v", err)
	}
	if !sess.OrphanedToolUseIDs["call-open"] {
		t.Fatalf("OrphanedToolUseIDs = %#v, want call-open", sess.OrphanedToolUseIDs)
	}
}

func TestReadAntigravityFileNormalizesCompletedToolUseTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := `{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"checking","tool_calls":[{"id":"call-done","name":"Read","args":{"path":"README.md"}}]}` + "\n" +
		`{"step_index":2,"type":"READ_FILE","created_at":"2026-04-04T09:00:02Z","tool_call_id":"call-done","content":"file data"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	sess, err := ReadAntigravityFileRaw(path, 0)
	if err != nil {
		t.Fatalf("ReadAntigravityFileRaw: %v", err)
	}
	if sess.OrphanedToolUseIDs != nil {
		t.Fatalf("OrphanedToolUseIDs = %#v, want nil for completed tool use", sess.OrphanedToolUseIDs)
	}
}

func TestReadAntigravityFileNormalizesInteractions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := `{"step_index":0,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"approval needed","interactions":[{"request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"]}]}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	sess, err := ReadAntigravityFileRaw(path, 0)
	if err != nil {
		t.Fatalf("ReadAntigravityFileRaw: %v", err)
	}
	if len(sess.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(sess.Messages))
	}
	var found *ContentBlock
	for _, block := range sess.Messages[0].ContentBlocks() {
		block := block
		if block.Type == "interaction" {
			found = &block
			break
		}
	}
	if found == nil {
		t.Fatalf("no interaction block in normalized content: %s", sess.Messages[0].Message)
	}
	if found.RequestID != "approval-1" || found.Kind != "approval" || found.State != "pending" || found.Prompt != "Allow Read?" {
		t.Fatalf("interaction block = %+v, want approval-1/approval/pending/Allow Read?", found)
	}
}

func TestReadProviderFileAntigravityAppliesMessageIDCursors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := `{"step_index":0,"type":"USER_INPUT","created_at":"2026-04-04T09:00:00Z","content":"first"}` + "\n" +
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"second"}` + "\n" +
		`{"step_index":2,"type":"USER_INPUT","created_at":"2026-04-04T09:00:02Z","content":"third"}` + "\n" +
		`{"step_index":3,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:03Z","content":"fourth"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	newer, err := ReadProviderFileNewer("antigravity/tmux-cli", path, 0, "agy-1")
	if err != nil {
		t.Fatalf("ReadProviderFileNewer: %v", err)
	}
	if got := antigravityEntryIDs(newer.Messages); !reflect.DeepEqual(got, []string{"agy-2", "agy-3"}) {
		t.Fatalf("newer Antigravity message IDs = %v, want [agy-2 agy-3]", got)
	}

	older, err := ReadProviderFileOlder("antigravity/tmux-cli", path, 0, "agy-2")
	if err != nil {
		t.Fatalf("ReadProviderFileOlder: %v", err)
	}
	if got := antigravityEntryIDs(older.Messages); !reflect.DeepEqual(got, []string{"agy-0", "agy-1"}) {
		t.Fatalf("older Antigravity message IDs = %v, want [agy-0 agy-1]", got)
	}

	rawNewer, err := ReadProviderFileRawNewer("antigravity/tmux-cli", path, 0, "agy-2")
	if err != nil {
		t.Fatalf("ReadProviderFileRawNewer: %v", err)
	}
	if got := antigravityEntryIDs(rawNewer.Messages); !reflect.DeepEqual(got, []string{"agy-3"}) {
		t.Fatalf("raw newer Antigravity message IDs = %v, want [agy-3]", got)
	}

	rawOlder, err := ReadProviderFileRawOlder("antigravity/tmux-cli", path, 0, "agy-3")
	if err != nil {
		t.Fatalf("ReadProviderFileRawOlder: %v", err)
	}
	if got := antigravityEntryIDs(rawOlder.Messages); !reflect.DeepEqual(got, []string{"agy-0", "agy-1", "agy-2"}) {
		t.Fatalf("raw older Antigravity message IDs = %v, want [agy-0 agy-1 agy-2]", got)
	}
}

func TestFindAntigravitySessionFileHonorsSearchPaths(t *testing.T) {
	// Point HOME at an empty dir so only the configured search path can match.
	t.Setenv("HOME", t.TempDir())

	fixtureRoot := t.TempDir()
	brainRoot := filepath.Join(fixtureRoot, "brain")
	convID := "agy-fixture-conv"
	logDir := filepath.Join(brainRoot, convID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir setup: %v", err)
	}
	transcriptPath := filepath.Join(logDir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("turns\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	workDir := "/tmp/gascity/phase1/antigravity"
	entry := AntigravityHistoryEntry{Workspace: workDir, ConversationID: convID, Timestamp: 1770000000}
	row, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal history: %v", err)
	}
	// history.jsonl lives alongside the brain directory.
	if err := os.WriteFile(filepath.Join(fixtureRoot, "history.jsonl"), append(row, '\n'), 0o644); err != nil {
		t.Fatalf("write history: %v", err)
	}

	got := FindAntigravitySessionFile([]string{brainRoot}, workDir)
	if got != transcriptPath {
		t.Fatalf("FindAntigravitySessionFile() = %q, want %q", got, transcriptPath)
	}
}

func antigravityEntryIDs(entries []*Entry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.UUID)
	}
	return ids
}
