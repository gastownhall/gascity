package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// agentTranscriptProvider picks the string transcript discovery and reads
// dispatch on for GET /agent/{name}/output and its stream. A custom provider's
// NAME carries no family signal ("glm53" is a zcode seat) or a misleading one
// ("kimi-k3-manifold" is a claude seat), so the session bead's family metadata
// must win, then the config chain's builtin ancestor, and only a name with no
// resolvable family falls through unchanged. PR #6144 (ga-0a1n5) gave the
// worker boundary's historyProvider a provider_kind rung for the same reason,
// but the ladders are deliberately not identical: historyProvider leads with
// the session Profile and falls back to the bead's recorded provider, while
// this helper takes no Profile rung and falls back to the config chain.
func TestAgentTranscriptProviderPrefersFamilyOverName(t *testing.T) {
	zcodeBase := "builtin:zcode"
	claudeBase := "builtin:claude"
	cfg := &config.City{Providers: map[string]config.ProviderSpec{
		"glm53":            {Base: &zcodeBase},
		"kimi-k3-manifold": {Base: &claudeBase},
		"claude-cmd-alias": {Command: "claude"},
		"test-agent":       {DisplayName: "Test Agent"},
	}}
	cases := []struct {
		name         string
		info         *session.Info
		providerName string
		want         string
	}{
		{
			name:         "bead builtin_ancestor wins over a config family and the name",
			info:         &session.Info{Provider: "glm53", ProviderKind: "zcode", BuiltinAncestor: "zcode"},
			providerName: "glm53",
			want:         "zcode",
		},
		{
			name:         "bead provider_kind wins when builtin_ancestor is absent",
			info:         &session.Info{Provider: "kimi-k3-manifold", ProviderKind: "claude"},
			providerName: "kimi-k3-manifold",
			want:         "claude",
		},
		{
			name:         "bead family beats a contradicting config chain",
			info:         &session.Info{Provider: "kimi-k3-manifold", ProviderKind: "zcode"},
			providerName: "kimi-k3-manifold",
			want:         "zcode",
		},
		{
			name:         "no bead: config base chain resolves the zcode family",
			providerName: "glm53",
			want:         "zcode",
		},
		{
			name:         "no bead: kimi-named claude descendant resolves to claude",
			providerName: "kimi-k3-manifold",
			want:         "claude",
		},
		{
			name:         "no bead: legacy command-matched alias inherits the builtin",
			providerName: "claude-cmd-alias",
			want:         "claude",
		},
		{
			name:         "bead without family metadata falls through to the config chain",
			info:         &session.Info{Provider: "glm53"},
			providerName: "glm53",
			want:         "zcode",
		},
		{
			name:         "blank bead kind falls through to the config chain",
			info:         &session.Info{Provider: "glm53", ProviderKind: "  ", BuiltinAncestor: " "},
			providerName: "glm53",
			want:         "zcode",
		},
		{
			name:         "bare builtin name keeps resolving to itself",
			providerName: "claude",
			want:         "claude",
		},
		{
			name:         "bare builtin with a bead and no stamped family stays itself",
			info:         &session.Info{Provider: "codex"},
			providerName: "codex",
			want:         "codex",
		},
		{
			name:         "fully custom provider with no family keeps its name",
			providerName: "test-agent",
			want:         "test-agent",
		},
		{
			name:         "unknown provider keeps its name",
			providerName: "claude/tmux-cli",
			want:         "claude/tmux-cli",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentTranscriptProvider(tc.info, tc.providerName, cfg)
			if got != tc.want {
				ancestor, kind := "<nil>", "<nil>"
				if tc.info != nil {
					ancestor, kind = tc.info.BuiltinAncestor, tc.info.ProviderKind
				}
				t.Fatalf("agentTranscriptProvider(BuiltinAncestor=%q, ProviderKind=%q, name=%q) = %q, want %q", ancestor, kind, tc.providerName, got, tc.want)
			}
		})
	}
}

// agentTranscriptProvider must not dereference a nil config when the server
// has no city config loaded.
func TestAgentTranscriptProviderNilConfigFallsBackToName(t *testing.T) {
	if got := agentTranscriptProvider(nil, "glm53", nil); got != "glm53" {
		t.Fatalf("agentTranscriptProvider(nil, glm53, nil) = %q, want glm53", got)
	}
	info := &session.Info{ProviderKind: "zcode"}
	if got := agentTranscriptProvider(info, "glm53", nil); got != "zcode" {
		t.Fatalf("agentTranscriptProvider(kind=zcode, glm53, nil) = %q, want zcode", got)
	}
}

// Reproduces ga-j72g7 at the HTTP boundary: an agent whose provider is NAMED
// "glm53" and whose session bead is stamped provider_kind=zcode must read its
// whole-file mirror through the zcode reader. The config entry deliberately
// carries no resolvable family (a non-builtin command, no base) so the bead is
// the only source of truth. Dispatching on the name fell through to the Claude
// slug discovery, found nothing, and the endpoint degraded to the peek
// fallback's 404.
func TestAgentOutputReadsCustomZCodeProviderTranscriptByFamily(t *testing.T) {
	fx := newZCodeAgentOutputFixture(t)

	h := newTestCityHandlerWith(t, fx.state, fx.srv)
	req := httptest.NewRequest("GET", cityURL(fx.state, "/agent/myrig/worker/output?tail=0"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp agentOutputResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "conversation" {
		t.Fatalf("Format = %q, want conversation (the zcode mirror should be read as a session log)", resp.Format)
	}
	if len(resp.Turns) != 2 {
		t.Fatalf("Turns = %+v, want the two mirrored zcode turns", resp.Turns)
	}
	if resp.Turns[0].Role != "user" || resp.Turns[0].Text != "hello glm" {
		t.Fatalf("Turns[0] = %+v, want user %q", resp.Turns[0], "hello glm")
	}
	if resp.Turns[1].Role != "assistant" || resp.Turns[1].Text != "hello from glm through zcode" {
		t.Fatalf("Turns[1] = %+v, want the assistant reply", resp.Turns[1])
	}
}

// The stream endpoint shares resolveAgentTranscript with the one-shot read, so
// the same custom zcode alias must stream its mirror rather than 404 as "not
// running".
func TestAgentOutputStreamReadsCustomZCodeProviderTranscriptByFamily(t *testing.T) {
	fx := newZCodeAgentOutputFixture(t)

	h := newTestCityHandlerWith(t, fx.state, fx.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", cityURL(fx.state, "/agent/myrig/worker/output/stream"), nil).WithContext(ctx)
	rec := newSyncResponseRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	body := waitForRecorderSubstring(t, rec, "hello from glm through zcode", 3*time.Second)
	cancel()
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, body)
	}
	if !strings.Contains(body, "event: turn") {
		t.Fatalf("stream body should carry an SSE turn event, got: %s", body)
	}
	if !strings.Contains(body, "hello from glm through zcode") {
		t.Fatalf("stream body should carry the mirrored zcode reply, got: %s", body)
	}
}

// The inverse name collision, resolved from config alone: a claude descendant
// NAMED "kimi-k3-manifold" with no session bead must discover and read its
// Claude JSONL through the Claude reader. Dispatching on the name routed it to
// the Kimi discovery, which found nothing.
func TestAgentOutputReadsClaudeFamilyTranscriptDespiteKimiName(t *testing.T) {
	state := newSessionFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}
	state.cfg.Agents[0].Provider = "kimi-k3-manifold"
	claudeBase := "builtin:claude"
	state.cfg.Providers = map[string]config.ProviderSpec{
		"kimi-k3-manifold": {Base: &claudeBase},
	}

	searchBase := t.TempDir()
	writeSessionJSONL(t, searchBase, rigDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello claude\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"hello from a kimi-named claude seat\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	h := newTestCityHandlerWith(t, state, srv)
	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output?tail=0"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp agentOutputResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "conversation" {
		t.Fatalf("Format = %q, want conversation", resp.Format)
	}
	if len(resp.Turns) != 2 || resp.Turns[1].Text != "hello from a kimi-named claude seat" {
		t.Fatalf("Turns = %+v, want the two Claude JSONL turns", resp.Turns)
	}
}

// A bare builtin provider name keeps working end to end: no bead family, no
// config chain, the name itself selects the reader.
func TestAgentOutputBareBuiltinProviderKeepsWorking(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}
	state.cfg.Agents[0].Provider = "claude"
	state.cfg.Providers = map[string]config.ProviderSpec{}

	searchBase := t.TempDir()
	writeSessionJSONL(t, searchBase, rigDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"bare\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"builtin\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	h := newTestCityHandlerWith(t, state, srv)
	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output?tail=0"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp agentOutputResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Turns) != 2 || resp.Turns[1].Text != "builtin" {
		t.Fatalf("Turns = %+v, want the two Claude turns", resp.Turns)
	}
}

type zcodeAgentOutputFixture struct {
	state *fakeState
	srv   *Server
}

// newZCodeAgentOutputFixture wires an agent whose provider is the custom alias
// "glm53", a session bead stamped with the zcode family (as cmd/gc stamps a
// resolved custom provider), and a zcode export mirror bound to the agent's
// work dir under the server's transcript search root.
func newZCodeAgentOutputFixture(t *testing.T) *zcodeAgentOutputFixture {
	t.Helper()
	state := newSessionFakeState(t)
	workDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: workDir}}
	state.cfg.Agents[0].Provider = "glm53"
	state.cfg.Providers = map[string]config.ProviderSpec{
		"glm53": {Command: "glm53-cli"},
	}

	searchRoot := t.TempDir()
	srv := newServerWithSearchPaths(state, searchRoot)

	mgr := session.NewManagerWithOptions(state.cityBeadStore, state.sp)
	sessionName := agentSessionName(state.CityName(), "myrig/worker", state.cfg.Workspace.SessionTemplate)
	if _, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		ExplicitName: sessionName,
		Template:     "myrig/worker",
		Title:        "Chat",
		Command:      "glm53-cli",
		WorkDir:      workDir,
		Provider:     "glm53",
		Resume:       session.ProviderResume{},
		Hints:        runtime.Config{},
		ExtraMeta: map[string]string{
			"session_origin":   "manual",
			"provider_kind":    "zcode",
			"builtin_ancestor": "zcode",
		},
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	writeZCodeMirrorForAPI(t, filepath.Join(searchRoot, "sess_glm53.json"), "sess_glm53", workDir,
		zcodeMirrorTurnForAPI{role: "user", text: "hello glm"},
		zcodeMirrorTurnForAPI{role: "assistant", text: "hello from glm through zcode"},
	)

	return &zcodeAgentOutputFixture{state: state, srv: srv}
}

type zcodeMirrorTurnForAPI struct {
	role string
	text string
}

// writeZCodeMirrorForAPI writes a zcode export mirror in the OpenCode
// {info, messages} shape the zcode adapter produces, with info.directory bound
// to workDir so discovery attributes it to the agent's session.
func writeZCodeMirrorForAPI(t *testing.T, path, sessionID, workDir string, turns ...zcodeMirrorTurnForAPI) {
	t.Helper()
	messages := make([]map[string]any, 0, len(turns))
	parent := ""
	for i, turn := range turns {
		id := fmt.Sprintf("msg_%d", i+1)
		messages = append(messages, map[string]any{
			"info": map[string]any{
				"id":        id,
				"sessionID": sessionID,
				"role":      turn.role,
				"parentID":  parent,
				"time":      map[string]any{"created": 1770000000000 + int64(i)*1000},
			},
			"parts": []map[string]any{{"id": "part_" + id, "type": "text", "text": turn.text}},
		})
		parent = id
	}
	doc := map[string]any{
		"info":     map[string]any{"id": sessionID, "directory": workDir},
		"messages": messages,
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal zcode mirror: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
