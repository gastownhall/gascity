package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

func createTranscriptBackedSession(t *testing.T, store beads.Store, sp *runtime.Fake, workDir string) session.Info {
	t.Helper()
	mgr := session.NewManager(store, sp)
	info, err := mgr.Create(
		context.Background(),
		"default",
		"Transcript Backed",
		"echo test",
		workDir,
		"test",
		nil,
		session.ProviderResume{},
		runtime.Config{},
	)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return info
}

func writeSessionAgentTranscriptFixture(t *testing.T, searchBase, workDir string, info session.Info) {
	t.Helper()

	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	parentPath := filepath.Join(slugDir, info.SessionKey+".jsonl")
	subagentsDir := filepath.Join(slugDir, info.SessionKey, "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	parentContent := `{"uuid":"u1","type":"user","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(parentPath, []byte(parentContent), 0o644); err != nil {
		t.Fatalf("write parent transcript: %v", err)
	}

	agentPath := filepath.Join(subagentsDir, "agent-helper.jsonl")
	agentContent := strings.Join([]string{
		`{"uuid":"a1","type":"system","parentToolUseId":"toolu_123"}`,
		`{"uuid":"a2","parentUuid":"a1","type":"assistant","message":{"role":"assistant","content":"working"}}`,
		`{"uuid":"a3","parentUuid":"a2","type":"result","message":{"role":"result"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatalf("write agent transcript: %v", err)
	}
}

type sessionAgentListTestResponse struct {
	Agents []struct {
		AgentID         string `json:"agent_id"`
		ParentToolUseID string `json:"parent_tool_use_id"`
	} `json:"agents"`
}

type sessionAgentGetTestResponse struct {
	Messages []map[string]any `json:"messages"`
	Status   string           `json:"status"`
}

func TestHandleSessionAgentList(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	searchBase := t.TempDir()
	srv.sessionLogSearchPaths = []string{searchBase}

	workDir := filepath.Join(t.TempDir(), "claude-project")
	info := createTranscriptBackedSession(t, fs.cityBeadStore, fs.sp, workDir)
	writeSessionAgentTranscriptFixture(t, searchBase, workDir, info)

	req := httptest.NewRequest(http.MethodGet, "/v0/session/"+info.ID+"/agents", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp sessionAgentListTestResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(resp.Agents))
	}
	if resp.Agents[0].AgentID != "helper" {
		t.Fatalf("Agents[0].AgentID = %q, want helper", resp.Agents[0].AgentID)
	}
	if resp.Agents[0].ParentToolUseID != "toolu_123" {
		t.Fatalf("Agents[0].ParentToolUseID = %q, want toolu_123", resp.Agents[0].ParentToolUseID)
	}
}

func TestHandleSessionAgentGet(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	searchBase := t.TempDir()
	srv.sessionLogSearchPaths = []string{searchBase}

	workDir := filepath.Join(t.TempDir(), "claude-project")
	info := createTranscriptBackedSession(t, fs.cityBeadStore, fs.sp, workDir)
	writeSessionAgentTranscriptFixture(t, searchBase, workDir, info)

	req := httptest.NewRequest(http.MethodGet, "/v0/session/"+info.ID+"/agents/helper", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp sessionAgentGetTestResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != string(sessionlog.AgentStatusCompleted) {
		t.Fatalf("Status = %q, want %q", resp.Status, sessionlog.AgentStatusCompleted)
	}
	if len(resp.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3", len(resp.Messages))
	}
	if got := resp.Messages[0]["parentToolUseId"]; got != "toolu_123" {
		t.Fatalf("Messages[0][parentToolUseId] = %#v, want toolu_123", got)
	}
}
