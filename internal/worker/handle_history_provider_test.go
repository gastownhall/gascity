package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// historyProvider picks the string sessionlog dispatches transcript reads and
// tail-activity derivation on. A custom provider's NAME carries no family
// signal and may carry a misleading one ("kimi-k3-manifold" is a claude seat),
// so the raw provider_kind must win over the name. The worker_profile override
// stays on top, and a session without a kind keeps resolving by name.
func TestHistoryProviderPrefersProviderKindOverName(t *testing.T) {
	cases := []struct {
		name         string
		profile      Profile
		specProvider string
		info         sessionpkg.Info
		want         string
		wantFamily   string
	}{
		{
			name:         "custom zcode alias reads through the zcode family",
			specProvider: "glm53",
			info:         sessionpkg.Info{Provider: "glm53", ProviderKind: "zcode"},
			want:         "zcode",
			wantFamily:   "zcode",
		},
		{
			name:         "claude alias with kimi in its name reads through claude",
			specProvider: "kimi-k3-manifold",
			info:         sessionpkg.Info{Provider: "kimi-k3-manifold", ProviderKind: "claude"},
			want:         "claude",
			wantFamily:   "claude",
		},
		{
			name:         "bare provider without a kind keeps resolving by name",
			specProvider: "codex-mini",
			info:         sessionpkg.Info{Provider: "codex-mini"},
			want:         "codex-mini",
			wantFamily:   "codex",
		},
		{
			name:         "blank kind falls through to the name",
			specProvider: "codex-mini",
			info:         sessionpkg.Info{Provider: "codex-mini", ProviderKind: "  "},
			want:         "codex-mini",
			wantFamily:   "codex",
		},
		{
			name:         "empty info falls back to the spec provider",
			specProvider: "codex-mini",
			info:         sessionpkg.Info{},
			want:         "codex-mini",
			wantFamily:   "codex",
		},
		{
			name:         "worker profile still wins over the kind",
			profile:      ProfileClaudeTmuxCLI,
			specProvider: "claude",
			info:         sessionpkg.Info{Provider: "glm53", ProviderKind: "zcode"},
			want:         string(ProfileClaudeTmuxCLI),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &SessionHandle{session: SessionSpec{Profile: tc.profile, Provider: tc.specProvider}}
			got := h.historyProvider(tc.info)
			if got != tc.want {
				t.Fatalf("historyProvider(Provider=%q, ProviderKind=%q) = %q, want %q", tc.info.Provider, tc.info.ProviderKind, got, tc.want)
			}
			if tc.wantFamily != "" {
				if family := sessionlog.ProviderFamily(got); family != tc.wantFamily {
					t.Fatalf("ProviderFamily(%q) = %q, want %q", got, family, tc.wantFamily)
				}
			}
		})
	}
}

// Reproduces ga-0a1n5 at the worker boundary: a provider NAMED "glm53" whose
// kind is zcode must read its whole-file mirror through the zcode reader.
// Dispatching on the name fell through to the Claude JSONL reader and
// produced an empty transcript.
func TestSessionHandleReadsCustomZCodeProviderTranscriptByKind(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	handle := newCustomKindHandle(t, root, workDir, "glm53", "zcode", sessionpkg.ProviderResume{})
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mirror := filepath.Join(root, "sess_glm53.json")
	writeZCodeMirror(t, mirror, "sess_glm53", workDir,
		zcodeMirrorTurn{role: "user", text: "hello glm"},
		zcodeMirrorTurn{role: "assistant", text: "hello from glm through zcode"},
	)

	history, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history.Entries) != 2 {
		t.Fatalf("History().Entries = %d, want 2 mirrored turns", len(history.Entries))
	}
	if history.Entries[0].Actor != ActorUser || history.Entries[0].Text != "hello glm" {
		t.Fatalf("Entries[0] = %s %q, want user %q", history.Entries[0].Actor, history.Entries[0].Text, "hello glm")
	}
	if history.Entries[1].Actor != ActorAssistant || history.Entries[1].Text != "hello from glm through zcode" {
		t.Fatalf("Entries[1] = %s %q, want assistant reply", history.Entries[1].Actor, history.Entries[1].Text)
	}
	if got := history.Entries[0].Provenance.Provider; got != "zcode" {
		t.Fatalf("Provenance.Provider = %q, want the zcode family the mirror was read through", got)
	}

	transcript, err := handle.Transcript(context.Background(), TranscriptRequest{})
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if transcript.Provider != "zcode" {
		t.Fatalf("Transcript().Provider = %q, want zcode", transcript.Provider)
	}
	if got := len(transcript.Session.Messages); got != 2 {
		t.Fatalf("Transcript().Session.Messages = %d, want 2", got)
	}
}

// The inverse name collision: a claude seat NAMED "kimi-k3-manifold" must not
// be routed to the Kimi reader because of the "kimi" substring in its name.
func TestSessionHandleReadsClaudeKindTranscriptDespiteKimiName(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	handle := newCustomKindHandle(t, root, workDir, "kimi-k3-manifold", "claude", sessionpkg.ProviderResume{
		SessionIDFlag: "--session-id",
		ResumeFlag:    "--resume",
	})
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := handle.manager.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("Get(%q): %v", handle.sessionID, err)
	}
	if info.SessionKey == "" {
		t.Fatal("session key was not generated for the session-id provider")
	}
	if info.ProviderKind != "claude" {
		t.Fatalf("Info.ProviderKind = %q, want claude", info.ProviderKind)
	}

	slugDir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	writeWorkerTestJSONL(t, filepath.Join(slugDir, info.SessionKey+".jsonl"), []map[string]any{
		{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "hello claude"}},
		{"type": "assistant", "uuid": "a1", "parentUuid": "u1", "message": map[string]any{
			"role":        "assistant",
			"model":       "claude-opus-4-5-20251101",
			"stop_reason": "end_turn",
			"content":     "hello from a kimi-named claude seat",
		}},
	})

	history, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history.Entries) != 2 {
		t.Fatalf("History().Entries = %d, want 2 claude turns", len(history.Entries))
	}
	if history.Entries[1].Actor != ActorAssistant || history.Entries[1].Text != "hello from a kimi-named claude seat" {
		t.Fatalf("Entries[1] = %s %q, want the assistant reply", history.Entries[1].Actor, history.Entries[1].Text)
	}
	if got := history.Entries[0].Provenance.Provider; got != "claude" {
		t.Fatalf("Provenance.Provider = %q, want claude", got)
	}
}

// Side effect of dispatching on the kind: a zcode-kind seat now takes the
// derive-activity-from-history path (sessionlog.DerivesActivityFromHistory),
// which is the intended path for that family because this repo owns the mirror
// writer and it opens a turn with the user message and closes it with the
// reply. With the name-based dispatch PhaseBusy was unreachable for a custom
// zcode alias.
func TestSessionHandleStateDerivesBusyFromZCodeMirrorByKind(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	handle := newCustomKindHandle(t, root, workDir, "glm53", "zcode", sessionpkg.ProviderResume{})
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mirror := filepath.Join(root, "sess_glm53.json")

	writeZCodeMirror(t, mirror, "sess_glm53", workDir,
		zcodeMirrorTurn{role: "user", text: "start the build"},
	)
	state, err := handle.State(context.Background())
	if err != nil {
		t.Fatalf("State (open turn): %v", err)
	}
	if state.Phase != PhaseBusy {
		t.Fatalf("State().Phase with a trailing user message = %s, want %s", state.Phase, PhaseBusy)
	}

	writeZCodeMirror(t, mirror, "sess_glm53", workDir,
		zcodeMirrorTurn{role: "user", text: "start the build"},
		zcodeMirrorTurn{role: "assistant", text: "built"},
	)
	state, err = handle.State(context.Background())
	if err != nil {
		t.Fatalf("State (closed turn): %v", err)
	}
	if state.Phase != PhaseReady {
		t.Fatalf("State().Phase with a closed turn = %s, want %s", state.Phase, PhaseReady)
	}
}

// newCustomKindHandle builds a session-backed handle for a custom provider
// alias (no canonical worker profile) whose family is carried only by the
// provider_kind session metadata, mirroring how cmd/gc stamps resolved custom
// providers.
func newCustomKindHandle(t *testing.T, searchRoot, workDir, provider, kind string, resume sessionpkg.ProviderResume) *SessionHandle {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	// The stale-resume-key probe delay is production timing, not what these
	// tests own; skip it so a keyed (session-id) start does not sleep.
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithStaleKeyDetectionWaiter(
		func(context.Context, string) error { return nil },
	))
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchRoot},
		Session: SessionSpec{
			Template: "probe",
			Title:    "Probe",
			Command:  provider,
			WorkDir:  workDir,
			Provider: provider,
			Resume:   resume,
			Metadata: map[string]string{"provider_kind": kind},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	return handle
}

type zcodeMirrorTurn struct {
	role string
	text string
}

// writeZCodeMirror writes a zcode export mirror in the OpenCode {info, messages}
// shape the zcode adapter produces, with info.directory bound to workDir so
// discovery attributes it to the session.
func writeZCodeMirror(t *testing.T, path, sessionID, workDir string, turns ...zcodeMirrorTurn) {
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
