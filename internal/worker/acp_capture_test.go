package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	acpTestHeader   = `{"gc_acp_capture":1,"ts":"2026-09-25T10:00:00Z","session_id":"gc-1","runtime_epoch":"1","pid":1}`
	acpTestPrompt   = `{"ts":"2026-09-25T10:00:01Z","dir":"out","msg":{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"acp-1","prompt":[{"type":"text","text":"build it"}]}}}`
	acpTestChunkA   = `{"ts":"2026-09-25T10:00:02Z","dir":"in","msg":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Built "}}}}}`
	acpTestChunkB   = `{"ts":"2026-09-25T10:00:03Z","dir":"in","msg":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"it."}}}}}`
	acpTestResponse = `{"ts":"2026-09-25T10:00:04Z","dir":"in","msg":{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn","usage":{"inputTokens":900,"outputTokens":40}}}}`
)

func writeACPTestCapture(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestFactorySearchPathsIncludeACPCaptureDir(t *testing.T) {
	city := t.TempDir()
	acpDir := citylayout.ACPTranscriptsDir(city)
	configured := t.TempDir()
	newAdapter := func(cfg FactoryConfig) SessionLogAdapter {
		t.Helper()
		cfg.Store = beads.NewMemStore()
		cfg.Provider = runtime.NewFake()
		f, err := NewFactory(cfg)
		if err != nil {
			t.Fatalf("NewFactory: %v", err)
		}
		return f.Adapter()
	}

	got := newAdapter(FactoryConfig{CityPath: city, SearchPaths: []string{configured}}).SearchPaths
	if !slices.Equal(got, []string{configured, acpDir}) {
		t.Errorf("search paths = %v, want the configured root plus %s", got, acpDir)
	}
	got = newAdapter(FactoryConfig{CityPath: city, SearchPaths: []string{configured, acpDir}}).SearchPaths
	if !slices.Equal(got, []string{configured, acpDir}) {
		t.Errorf("search paths = %v, want the capture dir once", got)
	}
	// With no configured roots, discovery falls back to the defaults; the
	// capture dir must join them, not replace them.
	got = newAdapter(FactoryConfig{CityPath: city}).SearchPaths
	if want := append(DefaultSearchPaths(), acpDir); !slices.Equal(got, want) {
		t.Errorf("search paths = %v, want defaults plus the capture dir %v", got, want)
	}
	got = newAdapter(FactoryConfig{SearchPaths: []string{configured}}).SearchPaths
	if !slices.Equal(got, []string{configured}) {
		t.Errorf("search paths without a city = %v, want only the configured root", got)
	}
}

func TestSessionHandleStateBusyFromACPCapture(t *testing.T) {
	city := t.TempDir()
	factory, err := NewFactory(FactoryConfig{
		Store:       beads.NewMemStore(),
		Provider:    runtime.NewFake(),
		CityPath:    city,
		SearchPaths: []string{t.TempDir()},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	// A pack-defined ACP provider: its name says nothing about the format.
	handle, err := factory.Session(SessionSpec{
		Template:  "probe",
		Title:     "Probe",
		Command:   "unreal-acp",
		WorkDir:   t.TempDir(),
		Provider:  "unreal-acp",
		Transport: "acp",
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	path, err := citylayout.ACPTranscriptPath(city, handle.sessionID, "1")
	if err != nil {
		t.Fatalf("ACPTranscriptPath: %v", err)
	}

	writeACPTestCapture(t, path, acpTestHeader, acpTestPrompt, acpTestChunkA)
	state, err := handle.State(context.Background())
	if err != nil {
		t.Fatalf("State (prompt in flight): %v", err)
	}
	if state.Phase != PhaseBusy {
		t.Fatalf("State().Phase with a prompt in flight = %s, want %s", state.Phase, PhaseBusy)
	}
	if handle.history != nil {
		t.Fatal("State() primed the history cache, want the tail-only busy probe")
	}

	writeACPTestCapture(t, path, acpTestHeader, acpTestPrompt, acpTestChunkA, acpTestChunkB, acpTestResponse)
	state, err = handle.State(context.Background())
	if err != nil {
		t.Fatalf("State (turn answered): %v", err)
	}
	if state.Phase != PhaseReady {
		t.Fatalf("State().Phase after the prompt response = %s, want %s", state.Phase, PhaseReady)
	}

	history, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if history.TranscriptStreamID != filepath.Clean(path) {
		t.Fatalf("History read %q, want the capture %q", history.TranscriptStreamID, path)
	}
}

func TestLoadHistoryFromACPCapture(t *testing.T) {
	city := t.TempDir()
	path, err := citylayout.ACPTranscriptPath(city, "gc-1", "1")
	if err != nil {
		t.Fatalf("ACPTranscriptPath: %v", err)
	}
	writeACPTestCapture(t, path, acpTestHeader, acpTestPrompt, acpTestChunkA, acpTestChunkB, acpTestResponse,
		`{"ts":"2026-09-25T10:01:00Z","dir":"out","msg":{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"acp-1","prompt":[{"type":"text","text":"test it"}]}}}`,
		`{"ts":"2026-09-25T10:01:01Z","dir":"in","msg":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Run tests","status":"pending"}}}}`,
		`{"ts":"2026-09-25T10:01:02Z","dir":"in","msg":{"jsonrpc":"2.0","id":0,"method":"session/request_permission","params":{"sessionId":"acp-1","toolCall":{"toolCallId":"t1","title":"Run tests"},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}}}`,
	)
	adapter := SessionLogAdapter{SearchPaths: []string{citylayout.ACPTranscriptsDir(city)}}

	// "codex" is the most format-opinionated family (tail parser and detached
	// usage); the capture path must override it.
	for _, provider := range []string{"unreal-acp", "codex"} {
		snapshot, err := adapter.LoadHistory(LoadRequest{Provider: provider, TranscriptPath: path, GCSessionID: "gc-1"})
		if err != nil {
			t.Fatalf("%s: LoadHistory: %v", provider, err)
		}
		var actors []string
		for _, e := range snapshot.Entries {
			actors = append(actors, string(e.Actor)+":"+e.Text)
		}
		want := []string{"user:build it", "assistant:Built it.", "user:test it", "assistant:", "assistant:"}
		if !slices.Equal(actors, want) {
			t.Fatalf("%s: entries = %q, want %q", provider, actors, want)
		}
		reply := snapshot.Entries[1]
		if reply.StopReason != "end_turn" || reply.Usage == nil || reply.Usage.InputTokens != 900 || reply.Usage.OutputTokens != 40 {
			t.Fatalf("%s: reply stop=%q usage=%+v, want end_turn with the turn's usage", provider, reply.StopReason, reply.Usage)
		}
		if snapshot.TailState.Activity != TailActivityInTurn {
			t.Fatalf("%s: tail activity = %s, want %s", provider, snapshot.TailState.Activity, TailActivityInTurn)
		}
		if !slices.Equal(snapshot.TailState.OpenToolUseIDs, []string{"t1"}) || len(snapshot.TailState.PendingInteractionIDs) != 1 {
			t.Fatalf("%s: tail state = %+v, want tool t1 open and one pending approval", provider, snapshot.TailState)
		}
		if snapshot.ProviderSessionID != "acp-1" {
			t.Fatalf("%s: provider session id = %q, want acp-1", provider, snapshot.ProviderSessionID)
		}

		activity, err := adapter.TailActivityForProvider(provider, path)
		if err != nil || activity != TailActivityInTurn {
			t.Fatalf("%s: TailActivityForProvider = %s, %v; want %s", provider, activity, err, TailActivityInTurn)
		}
		meta, err := adapter.TailMetaForProvider(provider, path)
		if err != nil || meta == nil || meta.Activity != "in-turn" {
			t.Fatalf("%s: TailMetaForProvider = %+v, %v; want in-turn", provider, meta, err)
		}
	}
}
