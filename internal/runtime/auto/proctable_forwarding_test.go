package auto

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// scannerlessProvider hides every optional interface of the wrapped provider,
// modeling a backend that cannot inspect a process table (hybrid, herdr,
// t3bridge, exec, k8s).
type scannerlessProvider struct {
	runtime.Provider
}

// brokenScanner returns a fixed partial result plus an error from its scan.
type brokenScanner struct {
	*runtime.Fake
	found []runtime.LiveRuntime
	err   error
}

func (b *brokenScanner) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	return append([]runtime.LiveRuntime(nil), b.found...), b.err
}

func startWithSessionID(t *testing.T, sp runtime.Provider, name, sessionID string) {
	t.Helper()
	if err := sp.Start(context.Background(), name, runtime.Config{
		Command: "agent",
		Env:     map[string]string{"GC_SESSION_ID": sessionID},
	}); err != nil {
		t.Fatalf("Start(%s): %v", name, err)
	}
}

func byPID(found []runtime.LiveRuntime) map[int]runtime.LiveRuntime {
	out := make(map[int]runtime.LiveRuntime, len(found))
	for _, r := range found {
		out[r.PID] = r
	}
	return out
}

// Without this forwarding, a city that routes any session to ACP loses orphan
// reaping for every session, because killExistingOrphans type-asserts the
// composite and the composite did not implement the scanner.
func TestProviderImplementsProcessTableScanner(t *testing.T) {
	var sp runtime.Provider = New(runtime.NewFake(), runtime.NewFake())
	if _, ok := sp.(runtime.ProcessTableScanner); !ok {
		t.Fatal("auto.Provider does not implement runtime.ProcessTableScanner")
	}
}

// Both backends scan the same host process table, so the same root appears in
// both results. A root either backend tracks is tracked; the tracking
// backend's provider name wins.
func TestFindRuntimesBySessionIDMergesByPIDAndORsTracking(t *testing.T) {
	def, acp := runtime.NewFake(), runtime.NewFake()
	// The ACP backend owns the live session; the default backend sees the same
	// root (Fake's pane root pid) but does not track it.
	startWithSessionID(t, acp, "acp-sess", "sid-a")
	def.OrphanedRuntimes["sid-a"] = runtime.LiveRuntime{SessionID: "sid-a", PID: 4242}
	// A genuinely untracked orphan both backends report.
	orphan := runtime.LiveRuntime{SessionID: "sid-a", PID: 777}
	def.ExtraRuntimes = []runtime.LiveRuntime{orphan}
	acp.ExtraRuntimes = []runtime.LiveRuntime{orphan}

	p := New(def, acp)
	found, err := p.FindRuntimesBySessionID("sid-a")
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found = %+v, want 2 roots merged by PID", found)
	}
	got := byPID(found)
	if r := got[4242]; !r.IsTracked || r.ProviderName != "acp-sess" {
		t.Errorf("pid 4242 = %+v, want tracked by the ACP backend as acp-sess", r)
	}
	if r := got[777]; r.IsTracked || r.ProviderName != "" {
		t.Errorf("pid 777 = %+v, want untracked", r)
	}
	if found[0].PID > found[1].PID {
		t.Errorf("found not ordered by PID: %+v", found)
	}
	if def.CountCalls("FindRuntimesBySessionID", "sid-a") != 1 || acp.CountCalls("FindRuntimesBySessionID", "sid-a") != 1 {
		t.Error("both backends must be scanned exactly once")
	}
}

// The mixed-city regression: a tmux/subprocess-tracked session must stay
// tracked even though the ACP backend, scanning the same /proc, does not know
// it.
func TestFindRuntimesBySessionIDKeepsDefaultBackendTracking(t *testing.T) {
	def, acp := runtime.NewFake(), runtime.NewFake()
	startWithSessionID(t, def, "tmux-sess", "sid-d")
	acp.OrphanedRuntimes["sid-d"] = runtime.LiveRuntime{SessionID: "sid-d", PID: 4242}

	found, err := New(def, acp).FindRuntimesBySessionID("sid-d")
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID: %v", err)
	}
	if len(found) != 1 || !found[0].IsTracked || found[0].ProviderName != "tmux-sess" {
		t.Fatalf("found = %+v, want one root tracked by the default backend", found)
	}
}

// Behind a scannerless default (hybrid, herdr, t3bridge, exec, k8s) the ACP
// scanner cannot tell a live default-hosted runtime carrying GC_SESSION_ID
// from an orphan, so forwarding it would reap live sessions. Nothing is
// surfaced and the ACP backend is not scanned.
func TestFindRuntimesBySessionIDScannerlessDefaultFindsNothing(t *testing.T) {
	acp := runtime.NewFake()
	acp.OrphanedRuntimes["sid-x"] = runtime.LiveRuntime{SessionID: "sid-x", PID: 900}
	p := New(&scannerlessProvider{Provider: runtime.NewFake()}, acp)

	found, err := p.FindRuntimesBySessionID("sid-x")
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found = %+v, want nothing behind a scannerless default", found)
	}
	if acp.CountCalls("FindRuntimesBySessionID", "sid-x") != 0 {
		t.Error("ACP backend scanned behind a scannerless default")
	}
}

func TestFindRuntimesBySessionIDNoScannerBackendsFindsNothing(t *testing.T) {
	p := New(&scannerlessProvider{Provider: runtime.NewFake()}, &scannerlessProvider{Provider: runtime.NewFake()})
	found, err := p.FindRuntimesBySessionID("sid")
	if err != nil || len(found) != 0 {
		t.Fatalf("FindRuntimesBySessionID = %+v, %v; want no roots and no error", found, err)
	}
}

// Best-effort contract: partial results from a failing backend are still
// merged, and each backend's error is labeled and joined.
func TestFindRuntimesBySessionIDJoinsBackendErrorsAndKeepsPartialResults(t *testing.T) {
	defErr := errors.New("tmux list running: server gone")
	acpErr := errors.New("reading environ: EIO")
	def := &brokenScanner{Fake: runtime.NewFake(), found: []runtime.LiveRuntime{{SessionID: "sid", PID: 10}}, err: defErr}
	acp := &brokenScanner{Fake: runtime.NewFake(), found: []runtime.LiveRuntime{{SessionID: "sid", PID: 11}}, err: acpErr}

	found, err := New(def, acp).FindRuntimesBySessionID("sid")
	if !errors.Is(err, defErr) || !errors.Is(err, acpErr) {
		t.Fatalf("err = %v, want both backend errors joined", err)
	}
	if !strings.Contains(err.Error(), "default backend") || !strings.Contains(err.Error(), "acp backend") {
		t.Errorf("err = %v, want backend labels", err)
	}
	if got := byPID(found); len(got) != 2 {
		t.Fatalf("found = %+v, want partial results from both backends", found)
	}
}

func TestTerminateRuntimeUsesDefaultBackendScanner(t *testing.T) {
	def, acp := runtime.NewFake(), runtime.NewFake()
	r := runtime.LiveRuntime{SessionID: "sid", PID: 321}
	if err := New(def, acp).TerminateRuntime(r); err != nil {
		t.Fatalf("TerminateRuntime: %v", err)
	}
	if def.CountCalls("TerminateRuntime", "sid") != 1 {
		t.Error("default backend TerminateRuntime not called")
	}
	if acp.CountCalls("TerminateRuntime", "sid") != 0 {
		t.Error("ACP backend TerminateRuntime called; want exactly one terminator")
	}
}

func TestTerminateRuntimeScannerlessDefaultErrors(t *testing.T) {
	acp := runtime.NewFake()
	r := runtime.LiveRuntime{SessionID: "sid", PID: 321}
	if err := New(&scannerlessProvider{Provider: runtime.NewFake()}, acp).TerminateRuntime(r); err == nil {
		t.Fatal("TerminateRuntime = nil, want an error behind a scannerless default")
	}
	if acp.CountCalls("TerminateRuntime", "sid") != 0 {
		t.Error("ACP backend TerminateRuntime called behind a scannerless default")
	}
}

func TestTerminateRuntimeWithoutScannerBackendsErrors(t *testing.T) {
	p := New(&scannerlessProvider{Provider: runtime.NewFake()}, &scannerlessProvider{Provider: runtime.NewFake()})
	if err := p.TerminateRuntime(runtime.LiveRuntime{SessionID: "sid", PID: 321}); err == nil {
		t.Fatal("TerminateRuntime = nil, want an error when no backend can terminate")
	}
}
