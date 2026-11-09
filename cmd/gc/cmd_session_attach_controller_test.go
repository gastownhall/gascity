package main

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// attachStartRecorder is the CLI process's session provider in the attach
// tests: it counts the runtime starts made from this process, so a test can
// tell them from the starts the fake controller makes on the inner Fake.
type attachStartRecorder struct {
	runtime.Provider
	cliStarts atomic.Int32
}

func (p *attachStartRecorder) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.cliStarts.Add(1)
	return p.Provider.Start(ctx, name, cfg)
}

// ObserveLivenessWithError reads the inner Fake fresh, as tmux does.
func (p *attachStartRecorder) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	return runtime.FullFake{Fake: p.Provider.(*runtime.Fake)}.ObserveLivenessWithError(name, processNames)
}

// newAttachSession stands up a named session whose runtime is stopped, in
// state, with the CLI's provider wrapped in an attachStartRecorder. It returns
// the store, the session bead, the recorder and the inner Fake (the runtime
// a controller would start).
func newAttachSession(t *testing.T, sessionName string, state sessionpkg.State) (beads.Store, beads.Bead, *attachStartRecorder, *runtime.Fake) {
	t.Helper()
	store, bead, _ := newKillPokeSession(t, sessionName)
	oldBuild := buildSessionProviderByName
	innerProvider, err := oldBuild(nil, "", config.SessionConfig{}, "", "")
	if err != nil {
		t.Fatalf("building fixture provider: %v", err)
	}
	inner := innerProvider.(*runtime.Fake)
	recorder := &attachStartRecorder{Provider: inner}
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return recorder, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	if err := inner.Stop(sessionName); err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{
		"state":      string(state),
		"template":   killPokeSessionIdentity,
		"agent_name": killPokeSessionIdentity,
		"command":    "echo",
	}
	if state == sessionpkg.StateSuspended {
		meta["held_until"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		meta["sleep_intent"] = "user-hold"
	}
	setKillFixtureMetadata(t, store, bead.ID, meta)
	return store, bead, recorder, inner
}

// startAttachFakeController serves the city's controller socket: it answers
// ping, and, when startOnPoke, starts sessionName on inner (the controller's
// own start, outside the CLI process) when poked. It returns the poke count.
func startAttachFakeController(t *testing.T, sessionName string, inner *runtime.Fake, startOnPoke bool) *atomic.Int32 {
	t.Helper()
	cityDir := cityDirFromEnv(t)
	pokes := &atomic.Int32{}
	startFakeUnixSocket(t, controllerSocketPath(cityDir), func(line string) string {
		switch {
		case line == "ping":
			return "4242\n"
		case strings.HasPrefix(line, "poke"):
			pokes.Add(1)
			if startOnPoke && !inner.IsRunning(sessionName) {
				_ = inner.Start(context.Background(), sessionName, runtime.Config{Command: "echo"})
			}
			return "ok\n"
		}
		return ""
	})
	return pokes
}

func cityDirFromEnv(t *testing.T) string {
	t.Helper()
	cityDir, err := resolveCity()
	if err != nil {
		t.Fatalf("resolveCity: %v", err)
	}
	return cityDir
}

// TestCmdSessionAttachControllerStartsStoppedSession: with a controller
// running, attach to an asleep or suspended session asks the controller to
// start it (a recorded wake and a poke), waits for it to run, and attaches;
// the CLI process starts no runtime, so the viewer owns no runtime's lifetime.
func TestCmdSessionAttachControllerStartsStoppedSession(t *testing.T) {
	for _, state := range []sessionpkg.State{sessionpkg.StateAsleep, sessionpkg.StateSuspended} {
		t.Run(string(state), func(t *testing.T) {
			const sessionName = "s-gc-attach-ctl"
			store, bead, recorder, inner := newAttachSession(t, sessionName, state)
			pokes := startAttachFakeController(t, sessionName, inner, true)

			var stdout, stderr bytes.Buffer
			code := cmdSessionAttachWith([]string{killPokeSessionIdentity}, sessionAttachOpts{waitTimeout: 10 * time.Second}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cmdSessionAttach = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if n := recorder.cliStarts.Load(); n != 0 {
				t.Fatalf("CLI-process runtime starts = %d, want 0: the controller starts the session", n)
			}
			if pokes.Load() == 0 {
				t.Fatal("controller was not poked")
			}
			if !inner.IsRunning(sessionName) {
				t.Fatal("session not running after attach")
			}
			if n := inner.CountCalls("Attach", sessionName); n != 1 {
				t.Fatalf("Attach calls = %d, want 1", n)
			}
			if got := mustGetBead(t, store, bead.ID).Metadata["wake_request"]; got == "" {
				t.Fatalf("wake_request empty, want the wake recorded for the controller; meta=%v", mustGetBead(t, store, bead.ID).Metadata)
			}
		})
	}
}

// TestCmdSessionAttachControllerWaitTimesOut: a controller that never starts
// the session leaves attach waiting until its timeout, then failing with an
// error naming the session; nothing is started from the CLI process.
func TestCmdSessionAttachControllerWaitTimesOut(t *testing.T) {
	const sessionName = "s-gc-attach-timeout"
	_, _, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateAsleep)
	startAttachFakeController(t, sessionName, inner, false)

	var stdout, stderr bytes.Buffer
	code := cmdSessionAttachWith([]string{killPokeSessionIdentity}, sessionAttachOpts{waitTimeout: time.Second}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdSessionAttach = 0, want non-zero; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "did not start within 1s") || !strings.Contains(stderr.String(), sessionName) {
		t.Fatalf("stderr = %q, want a timeout naming the session", stderr.String())
	}
	if n := recorder.cliStarts.Load(); n != 0 || inner.IsRunning(sessionName) {
		t.Fatalf("CLI starts = %d running = %v, want nothing started", n, inner.IsRunning(sessionName))
	}
	if n := inner.CountCalls("Attach", sessionName); n != 0 {
		t.Fatalf("Attach calls = %d, want 0", n)
	}
}

// TestCmdSessionAttachNoStartRefusesStoppedSession: --no-start on a session
// that is not running exits non-zero, records no wake and starts nothing,
// with or without a controller.
func TestCmdSessionAttachNoStartRefusesStoppedSession(t *testing.T) {
	for _, withController := range []bool{false, true} {
		name := "no-controller"
		if withController {
			name = "controller"
		}
		t.Run(name, func(t *testing.T) {
			const sessionName = "s-gc-attach-nostart"
			store, bead, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateAsleep)
			var pokes *atomic.Int32
			if withController {
				pokes = startAttachFakeController(t, sessionName, inner, true)
			}

			var stdout, stderr bytes.Buffer
			code := cmdSessionAttachWith([]string{killPokeSessionIdentity}, sessionAttachOpts{noStart: true, waitTimeout: time.Second}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("cmdSessionAttach(--no-start) = 0, want non-zero; stdout=%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), "is not running") || !strings.Contains(stderr.String(), "--no-start") {
				t.Fatalf("stderr = %q, want the not-running refusal", stderr.String())
			}
			if n := recorder.cliStarts.Load(); n != 0 || inner.IsRunning(sessionName) {
				t.Fatalf("CLI starts = %d running = %v, want nothing started", n, inner.IsRunning(sessionName))
			}
			if pokes != nil && pokes.Load() != 0 {
				t.Fatalf("controller pokes = %d, want 0", pokes.Load())
			}
			if got := mustGetBead(t, store, bead.ID).Metadata["wake_request"]; got != "" {
				t.Fatalf("wake_request = %q, want no wake recorded", got)
			}
		})
	}
}

// TestCmdSessionAttachNoStartAttachesRunningSession: --no-start on a running
// session attaches to it and starts nothing.
func TestCmdSessionAttachNoStartAttachesRunningSession(t *testing.T) {
	const sessionName = "s-gc-attach-nostart-live"
	_, _, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateActive)
	if err := inner.Start(context.Background(), sessionName, runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionAttachWith([]string{killPokeSessionIdentity}, sessionAttachOpts{noStart: true, waitTimeout: time.Second}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionAttach(--no-start, running) = %d; stderr=%s", code, stderr.String())
	}
	if n := recorder.cliStarts.Load(); n != 0 {
		t.Fatalf("CLI starts = %d, want 0", n)
	}
	if n := inner.CountCalls("Attach", sessionName); n != 1 {
		t.Fatalf("Attach calls = %d, want 1", n)
	}
}

// TestCmdSessionAttachRunningSessionWithControllerAttaches: a running session
// attaches as before while a controller runs: no wake, no poke, no start.
func TestCmdSessionAttachRunningSessionWithControllerAttaches(t *testing.T) {
	const sessionName = "s-gc-attach-live"
	_, _, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateActive)
	if err := inner.Start(context.Background(), sessionName, runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	pokes := startAttachFakeController(t, sessionName, inner, true)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionAttachWith([]string{killPokeSessionIdentity}, sessionAttachOpts{waitTimeout: time.Second}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionAttach(running) = %d; stderr=%s", code, stderr.String())
	}
	if n := recorder.cliStarts.Load(); n != 0 || pokes.Load() != 0 {
		t.Fatalf("CLI starts = %d pokes = %d, want 0 and 0", n, pokes.Load())
	}
	if n := inner.CountCalls("Attach", sessionName); n != 1 {
		t.Fatalf("Attach calls = %d, want 1", n)
	}
}
