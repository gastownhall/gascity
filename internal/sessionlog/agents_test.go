package sessionlog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile is a test helper that writes a file and fails the test on error.
func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeTestFile(%s): %v", path, err)
	}
}

// makeSessionDir creates the standard Claude session layout:
//
//	{dir}/{sessionID}.jsonl         (parent session)
//	{dir}/{sessionID}/subagents/    (agent files go here)
//
// Returns the parent session path and the subagents directory path.
func makeSessionDir(t *testing.T, dir, sessionID string) (parentPath, subagentsDir string) {
	t.Helper()
	parentPath = filepath.Join(dir, sessionID+".jsonl")
	writeTestFile(t, parentPath, `{"uuid":"u1","type":"user"}`+"\n")
	subagentsDir = filepath.Join(dir, sessionID, "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdirAll subagents: %v", err)
	}
	return parentPath, subagentsDir
}

func TestAgentDir(t *testing.T) {
	got := agentDir("/home/user/.claude/projects/slug/abc-123.jsonl")
	want := "/home/user/.claude/projects/slug/abc-123/subagents"
	if got != want {
		t.Errorf("agentDir = %q, want %q", got, want)
	}
}

func TestFindAgentFiles(t *testing.T) {
	dir := t.TempDir()
	parentPath, _ := makeSessionDir(t, dir, "session-abc")

	// No agent files yet.
	files, err := FindAgentFiles(parentPath)
	if err != nil {
		t.Fatalf("FindAgentFiles: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 agent files, got %d", len(files))
	}

	// Add some agent files and a non-agent file.
	subDir := filepath.Join(dir, "session-abc", "subagents")
	writeTestFile(t, filepath.Join(subDir, "agent-def.jsonl"), `{"uuid":"a1"}`+"\n")
	writeTestFile(t, filepath.Join(subDir, "agent-ghi.jsonl"), `{"uuid":"a2"}`+"\n")
	writeTestFile(t, filepath.Join(subDir, "other.jsonl"), `{"uuid":"o1"}`+"\n")

	files, err = FindAgentFiles(parentPath)
	if err != nil {
		t.Fatalf("FindAgentFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 agent files, got %d: %v", len(files), files)
	}
}

func TestFindAgentFiles_NoSubagentsDir(t *testing.T) {
	dir := t.TempDir()
	parentPath := filepath.Join(dir, "session-abc.jsonl")
	writeTestFile(t, parentPath, `{"uuid":"u1","type":"user"}`+"\n")

	// No subagents directory at all — should return empty, not error.
	files, err := FindAgentFiles(parentPath)
	if err != nil {
		t.Fatalf("FindAgentFiles: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 agent files, got %d", len(files))
	}
}

func TestFindAgentFiles_CrossSessionIsolation(t *testing.T) {
	dir := t.TempDir()

	// Create two sessions in the same slug directory.
	parentA, subA := makeSessionDir(t, dir, "session-aaa")
	_, subB := makeSessionDir(t, dir, "session-bbb")

	writeTestFile(t, filepath.Join(subA, "agent-a1.jsonl"), `{"uuid":"a1"}`+"\n")
	writeTestFile(t, filepath.Join(subB, "agent-b1.jsonl"), `{"uuid":"b1"}`+"\n")

	// Session A should only see its own agents.
	files, err := FindAgentFiles(parentA)
	if err != nil {
		t.Fatalf("FindAgentFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 agent file for session A, got %d: %v", len(files), files)
	}
	if filepath.Base(files[0]) != "agent-a1.jsonl" {
		t.Errorf("expected agent-a1.jsonl, got %s", filepath.Base(files[0]))
	}
}

func TestAgentIDFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/tmp/agent-abc123.jsonl", "abc123"},
		{"/tmp/agent-uuid-with-dashes.jsonl", "uuid-with-dashes"},
		{"/tmp/session-abc.jsonl", ""},
		{"/tmp/agent-.jsonl", ""},
	}
	for _, tt := range tests {
		got := agentIDFromPath(tt.path)
		if got != tt.want {
			t.Errorf("agentIDFromPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestExtractParentToolUseID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-test.jsonl")
	content := `{"uuid":"u1","type":"system","parentToolUseId":"toolu_abc123"}` + "\n" +
		`{"uuid":"u2","type":"user","message":{"role":"user","content":"hello"}}` + "\n"
	writeTestFile(t, path, content)

	got, err := extractParentToolUseID(path)
	if err != nil {
		t.Fatalf("extractParentToolUseID: %v", err)
	}
	if got != "toolu_abc123" {
		t.Errorf("extractParentToolUseID = %q, want %q", got, "toolu_abc123")
	}
}

func TestExtractParentToolUseID_Missing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-test.jsonl")
	writeTestFile(t, path, `{"uuid":"u1","type":"user"}`+"\n")

	got, err := extractParentToolUseID(path)
	if err != nil {
		t.Fatalf("extractParentToolUseID: %v", err)
	}
	if got != "" {
		t.Errorf("extractParentToolUseID = %q, want empty", got)
	}
}

func TestFindAgentMappings(t *testing.T) {
	dir := t.TempDir()
	parentPath, subDir := makeSessionDir(t, dir, "session-abc")

	writeTestFile(t, filepath.Join(subDir, "agent-def.jsonl"),
		`{"uuid":"a1","type":"system","parentToolUseId":"toolu_111"}`+"\n")
	writeTestFile(t, filepath.Join(subDir, "agent-ghi.jsonl"),
		`{"uuid":"a2","type":"system","parentToolUseId":"toolu_222"}`+"\n")

	mappings, err := FindAgentMappings(parentPath)
	if err != nil {
		t.Fatalf("FindAgentMappings: %v", err)
	}
	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(mappings))
	}

	found := make(map[string]string)
	for _, m := range mappings {
		found[m.AgentID] = m.ParentToolUseID
	}
	if found["def"] != "toolu_111" {
		t.Errorf("agent def: want toolu_111, got %q", found["def"])
	}
	if found["ghi"] != "toolu_222" {
		t.Errorf("agent ghi: want toolu_222, got %q", found["ghi"])
	}
}

func TestFindAgentMappings_PropagatesReadErrors(t *testing.T) {
	dir := t.TempDir()
	parentPath, subDir := makeSessionDir(t, dir, "session-abc")

	brokenPath := filepath.Join(subDir, "agent-broken.jsonl")
	writeTestFile(t, brokenPath, `{"uuid":"a1","type":"system"}`+"\n")
	if err := os.Chmod(brokenPath, 0); err != nil {
		t.Fatalf("chmod broken agent transcript: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(brokenPath, 0o644) })

	_, err := FindAgentMappings(parentPath)
	if err == nil {
		t.Fatal("expected error when reading a broken agent transcript")
	}
	if !strings.Contains(err.Error(), `reading agent "broken" mapping`) {
		t.Fatalf("FindAgentMappings error = %q, want agent context", err)
	}
	if !strings.Contains(err.Error(), "opening transcript") {
		t.Fatalf("FindAgentMappings error = %q, want open context", err)
	}
}

func TestExtractParentToolUseID_PropagatesScannerErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-test.jsonl")
	writeTestFile(t, path, strings.Repeat("x", agentMappingScannerMaxTokenSize+1)+"\n")

	_, err := extractParentToolUseID(path)
	if err == nil {
		t.Fatal("expected scanner error for oversized transcript line")
	}
	if !strings.Contains(err.Error(), "scanning transcript") {
		t.Fatalf("extractParentToolUseID error = %q, want scanner context", err)
	}
}

func TestReadAgentSession(t *testing.T) {
	dir := t.TempDir()
	parentPath, subDir := makeSessionDir(t, dir, "session-abc")

	agentContent := `{"uuid":"a1","type":"system","parentToolUseId":"toolu_111"}` + "\n" +
		`{"uuid":"a2","parentUuid":"a1","type":"user","message":{"role":"user","content":"do task"}}` + "\n" +
		`{"uuid":"a3","parentUuid":"a2","type":"assistant","message":{"role":"assistant","content":"done"}}` + "\n" +
		`{"uuid":"a4","parentUuid":"a3","type":"result","message":{"role":"result"}}` + "\n"
	writeTestFile(t, filepath.Join(subDir, "agent-myagent.jsonl"), agentContent)

	sess, err := ReadAgentSession(parentPath, "myagent")
	if err != nil {
		t.Fatalf("ReadAgentSession: %v", err)
	}
	if sess.Status != AgentStatusCompleted {
		t.Errorf("status = %q, want %q", sess.Status, AgentStatusCompleted)
	}
	if len(sess.Messages) != 4 {
		t.Errorf("messages = %d, want 4", len(sess.Messages))
	}
}

func TestReadAgentSession_Running(t *testing.T) {
	dir := t.TempDir()
	parentPath, subDir := makeSessionDir(t, dir, "session-abc")

	agentContent := `{"uuid":"a1","type":"system","parentToolUseId":"toolu_111"}` + "\n" +
		`{"uuid":"a2","parentUuid":"a1","type":"assistant","message":{"role":"assistant","content":"working..."}}` + "\n"
	writeTestFile(t, filepath.Join(subDir, "agent-running.jsonl"), agentContent)

	sess, err := ReadAgentSession(parentPath, "running")
	if err != nil {
		t.Fatalf("ReadAgentSession: %v", err)
	}
	if sess.Status != AgentStatusRunning {
		t.Errorf("status = %q, want %q", sess.Status, AgentStatusRunning)
	}
}

func TestReadAgentSession_Failed(t *testing.T) {
	dir := t.TempDir()
	parentPath, subDir := makeSessionDir(t, dir, "session-abc")

	agentContent := `{"uuid":"a1","type":"system","parentToolUseId":"toolu_111"}` + "\n" +
		`{"uuid":"a2","parentUuid":"a1","type":"result","message":{"is_error":true}}` + "\n"
	writeTestFile(t, filepath.Join(subDir, "agent-failed.jsonl"), agentContent)

	sess, err := ReadAgentSession(parentPath, "failed")
	if err != nil {
		t.Fatalf("ReadAgentSession: %v", err)
	}
	if sess.Status != AgentStatusFailed {
		t.Errorf("status = %q, want %q", sess.Status, AgentStatusFailed)
	}
}

func TestReadAgentSession_NotFound(t *testing.T) {
	dir := t.TempDir()
	parentPath, _ := makeSessionDir(t, dir, "session-abc")

	_, err := ReadAgentSession(parentPath, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("expected ErrAgentNotFound, got: %v", err)
	}
}

func TestValidateAgentID(t *testing.T) {
	tests := []struct {
		id      string
		wantErr bool
	}{
		{"abc123", false},
		{"uuid-with-dashes", false},
		{"", true},
		{"../etc/passwd", true},
		{"foo/bar", true},
		{"foo\\bar", true},
		{"..%2f..%2fetc", true}, // contains ".." literal
		{"valid-agent-id", false},
	}
	for _, tt := range tests {
		err := ValidateAgentID(tt.id)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateAgentID(%q) err=%v, wantErr=%v", tt.id, err, tt.wantErr)
		}
	}
}

func TestReadAgentSession_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	parentPath, _ := makeSessionDir(t, dir, "session-abc")

	traversalIDs := []string{
		"../../../etc/passwd",
		"foo/bar",
		"..\\windows\\system32",
	}
	for _, id := range traversalIDs {
		_, err := ReadAgentSession(parentPath, id)
		if err == nil {
			t.Errorf("ReadAgentSession with traversal ID %q should fail", id)
		}
	}
}

func TestReadAgentSession_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	parentPath, subDir := makeSessionDir(t, dir, "session-abc")

	// Write a corrupt agent file — parseFile skips malformed lines,
	// so this produces an empty transcript with "pending" status.
	writeTestFile(t, filepath.Join(subDir, "agent-corrupt.jsonl"), "not json at all\n")

	sess, err := ReadAgentSession(parentPath, "corrupt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.Status != AgentStatusPending {
		t.Errorf("status = %q, want %q (all lines were unparseable)", sess.Status, AgentStatusPending)
	}
}

func TestReadAgentSession_StatError(t *testing.T) {
	dir := t.TempDir()
	parentPath, subDir := makeSessionDir(t, dir, "session-abc")

	// Create agent file then make it unreadable by removing the subagents dir.
	writeTestFile(t, filepath.Join(subDir, "agent-perm.jsonl"), `{"uuid":"a1"}`+"\n")

	// Remove execute permission from subagents dir so Stat fails with permission error.
	if err := os.Chmod(subDir, 0o000); err != nil {
		t.Skipf("cannot change permissions: %v", err)
	}
	t.Cleanup(func() { os.Chmod(subDir, 0o755) }) //nolint:errcheck

	_, err := ReadAgentSession(parentPath, "perm")
	if err == nil {
		t.Fatal("expected error for permission-denied agent file")
	}
	// Should NOT be ErrAgentNotFound — it's a permission error.
	if errors.Is(err, ErrAgentNotFound) {
		t.Error("permission error should not be wrapped as ErrAgentNotFound")
	}
}

func TestInferAgentStatus(t *testing.T) {
	tests := []struct {
		name     string
		messages []*Entry
		want     AgentStatus
	}{
		{"empty", nil, AgentStatusPending},
		{"no result", []*Entry{{Type: "assistant"}}, AgentStatusRunning},
		{"completed", []*Entry{{Type: "assistant"}, {Type: "result", Message: []byte(`{}`)}}, AgentStatusCompleted},
		{"failed", []*Entry{{Type: "assistant"}, {Type: "result", Message: []byte(`{"is_error":true}`)}}, AgentStatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferAgentStatus(tt.messages)
			if got != tt.want {
				t.Errorf("inferAgentStatus = %q, want %q", got, tt.want)
			}
		})
	}
}
