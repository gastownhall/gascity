package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
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
	// stopOnceSeenRunning, when set, stops the runtime right after the first
	// IsRunning that reads it running: a runtime that stops again between the
	// wait for it and the attach.
	stopOnceSeenRunning atomic.Bool
}

func (p *attachStartRecorder) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.cliStarts.Add(1)
	return p.Provider.Start(ctx, name, cfg)
}

func (p *attachStartRecorder) IsRunning(name string) bool {
	running := p.Provider.IsRunning(name)
	if running && p.stopOnceSeenRunning.CompareAndSwap(true, false) {
		_ = p.Stop(name)
	}
	return running
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

// writeAttachCityTOML rewrites the fixture city's city.toml (as
// writeGenericNamedSessionCityTOML writes it) with agentKeys, TOML lines,
// added to its one agent.
func writeAttachCityTOML(t *testing.T, agentKeys string) {
	t.Helper()
	data := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "session-a"
provider = "codex"
start_command = "echo"
` + agentKeys + `

[[named_session]]
template = "session-a"
`
	if err := os.WriteFile(filepath.Join(cityDirFromEnv(t), "city.toml"), []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

// latchIdleSleep puts session id to sleep idle under the sleep policy now in
// force (the idle latch), reading that policy through sp, the provider the
// attach reads it through.
func latchIdleSleep(t *testing.T, store beads.Store, id string, sp runtime.Provider) {
	t.Helper()
	cfg, err := loadCityConfig(cityDirFromEnv(t), io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	info, err := sessionFrontDoor(store).Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	policy := resolveSessionSleepPolicyInfo(info, cfg, sp)
	if !policy.enabled() || policy.Class == config.SessionSleepNonInteractive {
		t.Fatalf("precondition: an enabled interactive sleep policy, got enabled=%v class=%q", policy.enabled(), policy.Class)
	}
	setKillFixtureMetadata(t, store, id, map[string]string{
		"sleep_reason":             string(sessionpkg.SleepReasonIdle),
		"sleep_policy_fingerprint": policy.Fingerprint,
		"slept_at":                 time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	latched, err := sessionFrontDoor(store).Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if !configWakeSuppressedInfo(latched, policy, sp, clock.Real{}) {
		t.Fatal("precondition: the session must be idle-latched (configWakeSuppressedInfo = true)")
	}
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
// error naming the session and the state its row is in; nothing is started
// from the CLI process.
func TestCmdSessionAttachControllerWaitTimesOut(t *testing.T) {
	const sessionName = "s-gc-attach-timeout"
	_, bead, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateAsleep)
	startAttachFakeController(t, sessionName, inner, false)

	var stdout, stderr bytes.Buffer
	code := cmdSessionAttachWith([]string{killPokeSessionIdentity}, sessionAttachOpts{waitTimeout: time.Second}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdSessionAttach = 0, want non-zero; stdout=%s", stdout.String())
	}
	want := "session " + bead.ID + " did not start within 1s (state \"asleep\""
	if !strings.Contains(stderr.String(), want) || strings.Contains(stderr.String(), "creating state") {
		t.Fatalf("stderr = %q, want attach's timeout naming the session and its row's state (%q)", stderr.String(), want)
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

// TestCmdSessionAttachControllerStartsIdleLatchedSession: a session asleep
// under its idle latch (slept idle under the sleep policy still in force) is
// one the controller starts on the wake attach records, since the reconciler
// honors an explicit wake over the idle latch. So attach does not refuse it
// as one that will not start: it waits for the controller's start and
// attaches, starting nothing itself.
func TestCmdSessionAttachControllerStartsIdleLatchedSession(t *testing.T) {
	const sessionName = "s-gc-attach-idle"
	store, bead, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateAsleep)
	writeAttachCityTOML(t, `sleep_after_idle = "1m"`)
	latchIdleSleep(t, store, bead.ID, recorder)
	pokes := startAttachFakeController(t, sessionName, inner, true)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionAttachWith([]string{killPokeSessionIdentity}, sessionAttachOpts{waitTimeout: 10 * time.Second}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionAttach = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "will not start") {
		t.Fatalf("stderr = %q, want no will-not-start refusal: the controller starts an idle-latched session on an explicit wake", stderr.String())
	}
	if n := recorder.cliStarts.Load(); n != 0 {
		t.Fatalf("CLI-process runtime starts = %d, want 0: the controller starts the session", n)
	}
	if pokes.Load() == 0 {
		t.Fatal("controller was not poked")
	}
	if n := inner.CountCalls("Attach", sessionName); n != 1 {
		t.Fatalf("Attach calls = %d, want 1", n)
	}
}

// TestCmdSessionAttachControllerResumesAgentlessSessionHere: the controller
// starts only sessions a configured agent runs, so a session none runs (a
// provider session, whose template is its provider's name) is resumed in this
// process, as with no controller, with a note saying so. No wake is recorded
// and the controller is not poked.
func TestCmdSessionAttachControllerResumesAgentlessSessionHere(t *testing.T) {
	const sessionName = "s-gc-attach-provider"
	store, _, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateActive)
	const providerSessionName = "s-gc-attach-provider-session"
	provider, err := store.Create(beads.Bead{
		Title:  "provider session",
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"session_name": providerSessionName,
			"template":     "codex",
			"state":        string(sessionpkg.StateSuspended),
			"held_until":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"sleep_intent": "user-hold",
			"command":      "echo",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(provider session): %v", err)
	}
	pokes := startAttachFakeController(t, providerSessionName, inner, false)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionAttachWith([]string{provider.ID}, sessionAttachOpts{waitTimeout: time.Second}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionAttach(provider session) = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "the controller never starts it") {
		t.Fatalf("stderr = %q, want the note that the controller never starts the session", stderr.String())
	}
	if n := recorder.cliStarts.Load(); n != 1 || !inner.IsRunning(providerSessionName) {
		t.Fatalf("CLI-process runtime starts = %d running = %v, want the session resumed here", n, inner.IsRunning(providerSessionName))
	}
	if pokes.Load() != 0 {
		t.Fatalf("controller pokes = %d, want 0", pokes.Load())
	}
	if got := mustGetBead(t, store, provider.ID).Metadata["wake_request"]; got != "" {
		t.Fatalf("wake_request = %q, want no wake recorded", got)
	}
	if n := inner.CountCalls("Attach", providerSessionName); n != 1 {
		t.Fatalf("Attach calls = %d, want 1", n)
	}
}

// TestCmdSessionAttachControllerWillNotStartFails: a wake the controller will
// not act on (the session's agent is suspended) is recorded as gc session
// wake records it, and attach fails after the wake's reason, saying the
// controller will not start the session. Nothing starts and nothing attaches.
func TestCmdSessionAttachControllerWillNotStartFails(t *testing.T) {
	const sessionName = "s-gc-attach-refused"
	store, bead, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateAsleep)
	writeAttachCityTOML(t, "suspended = true")
	startAttachFakeController(t, sessionName, inner, false)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionAttachWith([]string{bead.ID}, sessionAttachOpts{waitTimeout: time.Second}, &stdout, &stderr); code == 0 {
		t.Fatalf("cmdSessionAttach = 0, want non-zero; stdout=%s", stdout.String())
	}
	got := stderr.String()
	if !strings.Contains(got, `agent "session-a" is suspended`) || !strings.Contains(got, "the controller will not start session "+bead.ID) {
		t.Fatalf("stderr = %q, want the wake's reason, then attach's refusal", got)
	}
	if wake := mustGetBead(t, store, bead.ID).Metadata["wake_request"]; wake != string(sessionpkg.WakeCauseExplicit) {
		t.Fatalf("wake_request = %q, want the recorded wake", wake)
	}
	if n := recorder.cliStarts.Load(); n != 0 || inner.IsRunning(sessionName) {
		t.Fatalf("CLI starts = %d running = %v, want nothing started", n, inner.IsRunning(sessionName))
	}
	if n := inner.CountCalls("Attach", sessionName); n != 0 {
		t.Fatalf("Attach calls = %d, want 0", n)
	}
}

// TestCmdSessionAttachControllerUnwokenSessionFails: a wake refused before
// anything is written (the row is in a state the controller skips) fails
// attach after the wake's reason; nothing is recorded, poked or started.
func TestCmdSessionAttachControllerUnwokenSessionFails(t *testing.T) {
	const sessionName = "s-gc-attach-unwoken"
	store, bead, recorder, inner := newAttachSession(t, sessionName, sessionpkg.State("hibernating"))
	pokes := startAttachFakeController(t, sessionName, inner, true)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionAttachWith([]string{bead.ID}, sessionAttachOpts{waitTimeout: time.Second}, &stdout, &stderr); code == 0 {
		t.Fatalf("cmdSessionAttach = 0, want non-zero; stdout=%s", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "cannot be woken") || !strings.Contains(got, "the controller will not start session "+bead.ID) {
		t.Fatalf("stderr = %q, want the wake's refusal, then attach's", got)
	}
	if wake := mustGetBead(t, store, bead.ID).Metadata["wake_request"]; wake != "" {
		t.Fatalf("wake_request = %q, want no wake recorded", wake)
	}
	if n := recorder.cliStarts.Load(); n != 0 || pokes.Load() != 0 || inner.IsRunning(sessionName) {
		t.Fatalf("CLI starts = %d pokes = %d running = %v, want nothing started or poked", n, pokes.Load(), inner.IsRunning(sessionName))
	}
}

// TestCmdSessionAttachControllerStoppedAgainBeforeAttachFails pins the
// runtime-only handle: a runtime the controller started that stops again
// between the wait for it and the attach fails the attach. Nothing starts it
// again from this process, as attaching through the session's own handle
// would.
func TestCmdSessionAttachControllerStoppedAgainBeforeAttachFails(t *testing.T) {
	const sessionName = "s-gc-attach-gone"
	_, _, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateAsleep)
	startAttachFakeController(t, sessionName, inner, true)
	recorder.stopOnceSeenRunning.Store(true)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionAttachWith([]string{killPokeSessionIdentity}, sessionAttachOpts{waitTimeout: 10 * time.Second}, &stdout, &stderr); code == 0 {
		t.Fatalf("cmdSessionAttach = 0, want non-zero; stdout=%s", stdout.String())
	}
	if recorder.stopOnceSeenRunning.Load() {
		t.Fatal("precondition: the runtime was never seen running, so it was never stopped before the attach")
	}
	if n := recorder.cliStarts.Load(); n != 0 || inner.IsRunning(sessionName) {
		t.Fatalf("CLI starts = %d running = %v, want nothing started in this process", n, inner.IsRunning(sessionName))
	}
	if n := inner.CountCalls("Attach", sessionName); n != 0 {
		t.Fatalf("Attach calls = %d, want 0", n)
	}
}

// TestCmdSessionAttachNonPositiveWaitTimeoutWaitsTheDefault: --wait-timeout
// of zero or less waits the default, as gc session new does, instead of
// failing at once after recording the wake.
func TestCmdSessionAttachNonPositiveWaitTimeoutWaitsTheDefault(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			const sessionName = "s-gc-attach-zero"
			_, _, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateAsleep)
			startAttachFakeController(t, sessionName, inner, true)

			var stdout, stderr bytes.Buffer
			if code := cmdSessionAttachWith([]string{killPokeSessionIdentity}, sessionAttachOpts{waitTimeout: timeout}, &stdout, &stderr); code != 0 {
				t.Fatalf("cmdSessionAttach(--wait-timeout %s) = %d; stdout=%s stderr=%s", timeout, code, stdout.String(), stderr.String())
			}
			if n := recorder.cliStarts.Load(); n != 0 {
				t.Fatalf("CLI-process runtime starts = %d, want 0", n)
			}
			if n := inner.CountCalls("Attach", sessionName); n != 1 {
				t.Fatalf("Attach calls = %d, want 1", n)
			}
		})
	}
}

// TestCmdSessionAttachNoStartMaterializesNoNamedSession: --no-start on a
// configured named session with no open session (never created, or closed)
// refuses it, with or without a controller: it creates, reopens, starts and
// wakes nothing, and pokes no controller.
func TestCmdSessionAttachNoStartMaterializesNoNamedSession(t *testing.T) {
	for _, tc := range []struct {
		name           string
		closed         bool
		withController bool
	}{
		{name: "unmaterialized/no-controller"},
		{name: "unmaterialized/controller", withController: true},
		{name: "closed/no-controller", closed: true},
		{name: "closed/controller", closed: true, withController: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const sessionName = "s-gc-attach-nostart-named"
			store, bead, recorder, inner := newAttachSession(t, sessionName, sessionpkg.StateAsleep)
			if tc.closed {
				if err := store.Close(bead.ID); err != nil {
					t.Fatalf("Close(%s): %v", bead.ID, err)
				}
			} else if err := store.Delete(bead.ID); err != nil {
				t.Fatalf("Delete(%s): %v", bead.ID, err)
			}
			var pokes *atomic.Int32
			if tc.withController {
				pokes = startAttachFakeController(t, sessionName, inner, true)
			}

			var stdout, stderr bytes.Buffer
			if code := cmdSessionAttachWith([]string{killPokeSessionIdentity}, sessionAttachOpts{noStart: true, waitTimeout: time.Second}, &stdout, &stderr); code == 0 {
				t.Fatalf("cmdSessionAttach(--no-start) = 0, want non-zero; stdout=%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), "--no-start") {
				t.Fatalf("stderr = %q, want the --no-start refusal", stderr.String())
			}
			open, err := loadSessionBeads(store)
			if err != nil {
				t.Fatalf("loadSessionBeads: %v", err)
			}
			if len(open) != 0 {
				t.Fatalf("open session beads = %d, want none created or reopened", len(open))
			}
			if n := recorder.cliStarts.Load(); n != 0 || inner.IsRunning(sessionName) {
				t.Fatalf("CLI starts = %d running = %v, want nothing started", n, inner.IsRunning(sessionName))
			}
			if pokes != nil && pokes.Load() != 0 {
				t.Fatalf("controller pokes = %d, want 0", pokes.Load())
			}
		})
	}
}
