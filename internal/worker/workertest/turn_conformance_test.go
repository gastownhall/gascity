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
	return turnConformanceSessionAged(t, sp, script, transcript, 0)
}

// turnConformanceSessionAged is turnConformanceSessionWithStore with the
// transcript's modification time set age in the past, so a read sees a
// transcript that stopped growing that long ago (the idle overlay's quiet
// window) without the test sleeping.
func turnConformanceSessionAged(t *testing.T, sp runtime.Provider, script func(name string), transcript []string, age time.Duration) (*worker.SessionHandle, beads.Store) {
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
		path := filepath.Join(slugDir, info.SessionKey+".jsonl")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if age > 0 {
			at := time.Now().Add(-age)
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
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

// A transcript whose turn ended failed on a provider refusal keeps the
// contract, and every read reports the failure as the transcript recorded
// it: the source's class and status, classed a rate limit, and its text.
func TestTurnHandleConformanceFailedTurn(t *testing.T) {
	const refusal = "API Error: Request rejected (429)"
	h := turnConformanceSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), nil, []string{
		`{"type":"user","isSidechain":false,"timestamp":"2026-10-13T05:00:00.123Z","message":{"role":"user","content":"a prompt"},"promptId":"7f3e9a10-1111-4aaa-8000-000000000001","origin":"user","promptSource":"prompt"}`,
		`{"type":"assistant","isSidechain":false,"timestamp":"2026-10-13T05:00:01.000Z","isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":429,"message":{"role":"assistant","content":[{"type":"text","text":"` + refusal + `"}]}}`,
		`{"type":"system","subtype":"turn_duration","isSidechain":false,"timestamp":"2026-10-13T05:00:01.010Z"}`,
		`{"type":"user","isSidechain":false,"timestamp":"2026-10-13T05:01:00.789Z","message":{"role":"user","content":"a prompt"},"promptId":"7f3e9a10-2222-4aaa-8000-000000000002","origin":"user","promptSource":"prompt"}`,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := CheckTurnHandle(ctx, h); err != nil {
		t.Fatal(err)
	}
	want := runtime.TurnFailure{Code: "rate_limit", HTTPStatus: 429, RateLimited: true, Message: refusal}
	for name, read := range map[string]func(context.Context) (runtime.TurnHistory, error){"CurrentTurn": h.CurrentTurn, "FreshTurn": h.FreshTurn} {
		hist, err := read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(hist.Recent) != 1 || hist.Recent[0].Status != runtime.TurnStatusFailed || hist.Recent[0].Failure != want {
			t.Fatalf("%s recent = %+v, want one failed turn with failure %+v", name, hist.Recent, want)
		}
	}
}

// A transcript whose newest turn ended failed on a provider error, with no
// turn after it, reads as that turn failed and no turn open, whatever the
// error: the work ladder takes up a step whose seat waits on a person only on
// such a reading (the turn the person's answer started died), so a failed
// turn must never read as a turn still open or as one completed.
func TestTurnHandleConformanceNewestTurnFailed(t *testing.T) {
	const prompt = `{"type":"user","isSidechain":false,"timestamp":"2026-10-13T05:00:00.123Z","message":{"role":"user","content":"a prompt"},"promptId":"7f3e9a10-1111-4aaa-8000-000000000001","origin":"user","promptSource":"prompt"}`
	const ended = `{"type":"system","subtype":"turn_duration","isSidechain":false,"timestamp":"2026-10-13T05:00:01.010Z"}`
	for name, apiError := range map[string]string{
		"a rate limit": `{"type":"assistant","isSidechain":false,"timestamp":"2026-10-13T05:00:01.000Z","isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":429,"message":{"role":"assistant","content":[{"type":"text","text":"API Error: Request rejected (429)"}]}}`,
		"an overload":  `{"type":"assistant","isSidechain":false,"timestamp":"2026-10-13T05:00:01.000Z","isApiErrorMessage":true,"error":"server_error","apiErrorStatus":529,"message":{"role":"assistant","content":[{"type":"text","text":"API Error: overloaded"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := turnConformanceSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), nil, []string{prompt, apiError, ended})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := CheckTurnHandle(ctx, h); err != nil {
				t.Fatal(err)
			}
			for read, f := range map[string]func(context.Context) (runtime.TurnHistory, error){"CurrentTurn": h.CurrentTurn, "FreshTurn": h.FreshTurn} {
				hist, err := f(ctx)
				if err != nil {
					t.Fatalf("%s: %v", read, err)
				}
				if hist.Current.Active() || len(hist.Recent) != 1 || hist.Recent[0].Status != runtime.TurnStatusFailed {
					t.Fatalf("%s = %+v, want no turn open and the newest ended turn failed", read, hist)
				}
			}
		})
	}
}

// A Claude CLI that stops at a provider error, idle or dead, writes no
// turn_duration: its transcript leaves the turn open after the error. Once
// the transcript has been quiet past the overlay's window and the session
// reads idle, every read ends that turn failed on the error, with no turn
// open, as a recorded end does: the work ladder takes up a step waiting on a
// person on that reading, never on a turn still open.
func TestTurnHandleConformanceNewestTurnStoppedAtAnErrorFailed(t *testing.T) {
	const prompt = `{"type":"user","isSidechain":false,"timestamp":"2026-10-13T05:00:00.123Z","message":{"role":"user","content":"a prompt"},"promptId":"7f3e9a10-1111-4aaa-8000-000000000001","origin":"user","promptSource":"prompt"}`
	const apiError = `{"type":"assistant","isSidechain":false,"timestamp":"2026-10-13T05:00:01.000Z","isApiErrorMessage":true,"error":"server_error","apiErrorStatus":529,"message":{"role":"assistant","content":[{"type":"text","text":"API Error: overloaded"}]}}`
	// The transcript stopped growing a minute ago: past the idle overlay's
	// quiet window, so the overlay may end the open turn.
	h, _ := turnConformanceSessionAged(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), nil, []string{prompt, apiError}, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := CheckTurnHandle(ctx, h); err != nil {
		t.Fatal(err)
	}
	for read, f := range map[string]func(context.Context) (runtime.TurnHistory, error){"CurrentTurn": h.CurrentTurn, "FreshTurn": h.FreshTurn} {
		hist, err := f(ctx)
		if err != nil {
			t.Fatalf("%s: %v", read, err)
		}
		if hist.Current.Active() || len(hist.Recent) != 1 || hist.Recent[0].Status != runtime.TurnStatusFailed || !hist.Recent[0].Inferred {
			t.Fatalf("%s = %+v, want no turn open and the newest turn ended failed, inferred", read, hist)
		}
	}
}

// checkTurnRead rejects a failure on a turn that did not fail, a rate limit
// with neither the source's class nor HTTP 429 (read from the text), and a
// spent quota that is not a rate limit.
func TestCheckTurnReadFailures(t *testing.T) {
	const id = "7f3e9a10-1111-4aaa-8000-000000000001"
	tests := []struct {
		name    string
		status  runtime.TurnStatus
		failure runtime.TurnFailure
		ok      bool
	}{
		{"failed with its class", runtime.TurnStatusFailed, runtime.TurnFailure{Code: "rate_limit", RateLimited: true, Message: "m"}, true},
		{"failed with 429", runtime.TurnStatusFailed, runtime.TurnFailure{HTTPStatus: 429, RateLimited: true}, true},
		{"failed, text only", runtime.TurnStatusFailed, runtime.TurnFailure{Message: "API Error"}, true},
		{"failed, no failure recorded", runtime.TurnStatusFailed, runtime.TurnFailure{}, true},
		{"completed, no failure", runtime.TurnStatusCompleted, runtime.TurnFailure{}, true},
		{"completed with a failure", runtime.TurnStatusCompleted, runtime.TurnFailure{Message: "API Error"}, false},
		{"closed by a newer turn with a failure", "", runtime.TurnFailure{Code: "server_error"}, false},
		{"rate limit read from the text", runtime.TurnStatusFailed, runtime.TurnFailure{RateLimited: true, HTTPStatus: 503, Message: "429"}, false},
		{"a spent quota, the source's rate-limit class", runtime.TurnStatusFailed, runtime.TurnFailure{Code: "usage_limit_exceeded", RateLimited: true, Quota: true}, true},
		{"a spent quota that is no rate limit", runtime.TurnStatusFailed, runtime.TurnFailure{Code: "usage_limit_exceeded", Quota: true}, false},
	}
	for _, tc := range tests {
		hist := runtime.TurnHistory{ObservedAt: time.Now(), Recent: []runtime.EndedTurn{{TurnID: id, Status: tc.status, Failure: tc.failure}}}
		if err := checkTurnRead(hist, nil); (err == nil) != tc.ok {
			t.Errorf("%s: checkTurnRead = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
