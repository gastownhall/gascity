package workertest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	worker "github.com/gastownhall/gascity/internal/worker"
)

var conformanceFullTurnCaps = runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact, Lossless: true}

// turnConformanceSession is a started session handle on sp, keyed as a
// production Claude session is; transcript, when set, writes its
// conversation's transcript under its search path.
func turnConformanceSession(t *testing.T, sp runtime.Provider, script func(name string), transcript []string) *worker.SessionHandle {
	t.Helper()
	h, _ := turnConformanceSessionWithStore(t, sp, script, transcript)
	return h
}

func turnConformanceSessionWithStore(t *testing.T, sp runtime.Provider, script func(name string), transcript []string) (*worker.SessionHandle, beads.Store) {
	t.Helper()
	searchBase, workDir := t.TempDir(), t.TempDir()
	store := beads.NewMemStore()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()),
		// A keyed start would otherwise wait out the stale-key detection delay.
		sessionpkg.WithStaleKeyDetectionWaiter(func(context.Context, string) error { return nil }))
	handle, err := worker.NewSessionHandle(worker.SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchBase},
		Session: worker.SessionSpec{
			Profile:  worker.ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "claude",
			WorkDir:  workDir,
			Provider: "claude",
			Resume:   sessionpkg.ProviderResume{SessionIDFlag: "--session-id"},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(context.Background(), managerActor(t, manager, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sessions, err := store.List(beads.ListQuery{Label: sessionpkg.LabelSession})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("session beads = %d, %v; want 1", len(sessions), err)
	}
	info, err := manager.Get(sessions[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if script != nil {
		script(info.SessionName)
	}
	if transcript != nil {
		slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
		if err := os.MkdirAll(slugDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		body := ""
		for _, line := range transcript {
			body += line + "\n"
		}
		if err := os.WriteFile(filepath.Join(slugDir, info.SessionKey+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return handle, store
}

func turnConformanceRuntimeHandle(t *testing.T, sp runtime.Provider) *worker.RuntimeHandle {
	t.Helper()
	if err := sp.Start(context.Background(), "turn-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h, err := worker.NewRuntimeHandle(worker.RuntimeHandleConfig{Provider: sp, SessionName: "turn-worker", ProviderName: "claude"})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	return h
}

// TestTurnHandleConformance runs the TurnHandle contract against both handles
// and every source.
func TestTurnHandleConformance(t *testing.T) {
	claudeTurns := []string{
		`{"type":"user","isSidechain":false,"timestamp":"2026-10-13T05:00:00.123Z","message":{"role":"user","content":"a prompt"},"promptId":"7f3e9a10-1111-4aaa-8000-000000000001","origin":"user","promptSource":"prompt"}`,
		`{"type":"system","subtype":"turn_duration","isSidechain":false,"timestamp":"2026-10-13T05:00:05.456Z"}`,
		`{"type":"user","isSidechain":false,"timestamp":"2026-10-13T05:01:00.789Z","message":{"role":"user","content":"a prompt"},"promptId":"7f3e9a10-2222-4aaa-8000-000000000002","origin":"user","promptSource":"prompt"}`,
	}
	scriptedProtocol := func() (runtime.Provider, func(string)) {
		fake := runtime.NewFake()
		return runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: fake}), func(name string) {
			fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{name: {conformanceFullTurnCaps}}
			fake.TurnHistoryScripts = map[string][]runtime.TurnHistory{name: {{Current: runtime.TurnState{TurnID: "acp-1"}}}}
		}
	}
	cases := []struct {
		name   string
		handle func(t *testing.T) worker.TurnHandle
		source worker.TurnSource
	}{
		{"session handle, transcript", func(t *testing.T) worker.TurnHandle {
			return turnConformanceSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), nil, claudeTurns)
		}, worker.TurnSourceTranscript},
		{"session handle, protocol", func(t *testing.T) worker.TurnHandle {
			sp, script := scriptedProtocol()
			return turnConformanceSession(t, sp, script, nil)
		}, worker.TurnSourceProtocol},
		{"session handle, none", func(t *testing.T) worker.TurnHandle {
			return turnConformanceSession(t, runtime.NewFakeProfile(runtime.ProfileExec, runtime.FullFake{Fake: runtime.NewFake()}), nil, nil)
		}, worker.TurnSourceNone},
		{"session handle, unreadable transcript", func(t *testing.T) worker.TurnHandle {
			return turnConformanceSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), nil, append(claudeTurns[:1:1], `{"type":"user","times`))
		}, worker.TurnSourceTranscript},
		{"runtime handle, protocol", func(t *testing.T) worker.TurnHandle {
			sp, script := scriptedProtocol()
			h := turnConformanceRuntimeHandle(t, sp)
			script("turn-worker")
			return h
		}, worker.TurnSourceProtocol},
		{"runtime handle, none", func(t *testing.T) worker.TurnHandle {
			return turnConformanceRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}))
		}, worker.TurnSourceNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.handle(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			caps, err := h.TurnCapabilities(ctx)
			if err != nil || caps.Source != tc.source {
				t.Fatalf("TurnCapabilities = %+v, %v; want source %q", caps, err, tc.source)
			}
			if err := CheckTurnHandle(ctx, h); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A closed session's reads fail with a turn-read sentinel the contract names
// (runtime.ErrSessionNotFound), never an unclassified lookup error.
func TestTurnHandleConformanceClosedSession(t *testing.T) {
	h, store := turnConformanceSessionWithStore(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), nil, nil)
	sessions, err := store.List(beads.ListQuery{Label: sessionpkg.LabelSession})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("session beads = %d, %v; want 1", len(sessions), err)
	}
	if err := store.Close(sessions[0].ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := context.Background()
	hist, err := h.CurrentTurn(ctx)
	if err == nil {
		t.Fatal("CurrentTurn of a closed session succeeded")
	}
	if err := checkTurnRead(hist, err); err != nil {
		t.Fatalf("CurrentTurn of a closed session: %v", err)
	}
	if _, err := h.WatchTurns(ctx); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("WatchTurns of a closed session = %v, want ErrSessionNotFound", err)
	}
}
