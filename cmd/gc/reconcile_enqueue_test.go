package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func drainSignal(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestLegacyEnqueueMapsKeysOntoLegacySignals(t *testing.T) {
	tests := []struct {
		name         string
		keys         []reconcilekey.Key
		wantPoke     bool
		wantDispatch bool
	}{
		{name: "no keys means allocator", wantPoke: true},
		{name: "allocator", keys: []reconcilekey.Key{reconcilekey.Allocator()}, wantPoke: true},
		{name: "session", keys: []reconcilekey.Key{reconcilekey.Session("gc-1")}, wantPoke: true},
		{name: "session by name", keys: []reconcilekey.Key{reconcilekey.SessionNamed("worker-1")}, wantPoke: true},
		{name: "zero key", keys: []reconcilekey.Key{{}}, wantPoke: true},
		{name: "control dispatch", keys: []reconcilekey.Key{reconcilekey.ControlDispatch()}, wantDispatch: true},
		{
			name:         "session and control dispatch",
			keys:         []reconcilekey.Key{reconcilekey.Session("gc-1"), reconcilekey.ControlDispatch()},
			wantPoke:     true,
			wantDispatch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pokeCh := make(chan struct{}, 1)
			dispatchCh := make(chan struct{}, 1)
			landed := legacyEnqueue(pokeCh, dispatchCh, tt.keys...)
			if got := drainSignal(pokeCh); got != tt.wantPoke {
				t.Fatalf("poke signaled = %v, want %v", got, tt.wantPoke)
			}
			if got := drainSignal(dispatchCh); got != tt.wantDispatch {
				t.Fatalf("control-dispatcher signaled = %v, want %v", got, tt.wantDispatch)
			}
			if !landed {
				t.Fatal("legacyEnqueue reported no signal landed on empty channels")
			}
		})
	}
}

func TestLegacyEnqueueCoalescesLikePoke(t *testing.T) {
	pokeCh := make(chan struct{}, 1)
	legacyEnqueue(pokeCh, nil, reconcilekey.Session("gc-1"), reconcilekey.Session("gc-2"), reconcilekey.Allocator())
	if landed := legacyEnqueue(pokeCh, nil, reconcilekey.Session("gc-3")); landed {
		t.Fatal("a second enqueue landed although a poke was already pending")
	}
	if len(pokeCh) != 1 {
		t.Fatalf("pending pokes = %d, want 1 (keys coalesce into one tick)", len(pokeCh))
	}
}

func TestLegacyEnqueueNilChannelsNeverBlock(t *testing.T) {
	if legacyEnqueue(nil, nil, reconcilekey.Session("gc-1"), reconcilekey.ControlDispatch()) {
		t.Fatal("legacyEnqueue reported a landed signal with nil channels")
	}
}

func TestControllerStateEnqueueUsesLegacySignals(t *testing.T) {
	cs := &controllerState{pokeCh: make(chan struct{}, 1), controlDispatcherCh: make(chan struct{}, 1)}

	cs.Enqueue(reconcilekey.Session("gc-1"))
	if !drainSignal(cs.pokeCh) || drainSignal(cs.controlDispatcherCh) {
		t.Fatal("session key should poke the reconciler only")
	}
	cs.Enqueue()
	if !drainSignal(cs.pokeCh) {
		t.Fatal("key-less Enqueue should poke the reconciler (allocator)")
	}
	cs.Enqueue(reconcilekey.ControlDispatch())
	if drainSignal(cs.pokeCh) || !drainSignal(cs.controlDispatcherCh) {
		t.Fatal("control-dispatch key should signal the control dispatcher only")
	}

	var nilState *controllerState
	nilState.Enqueue(reconcilekey.Allocator()) // must not panic
	(&controllerState{}).Enqueue(reconcilekey.Allocator())
}

// runControllerSocketCommand drives handleControllerConn with one command
// line and returns the reply.
func runControllerSocketCommand(t *testing.T, line string, pokeCh, dispatchCh chan struct{}) string {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck
	done := make(chan struct{})
	go func() {
		handleControllerConn(server, t.TempDir(), controllerHostingStandalone, func() {}, nil, nil, nil, nil, newLegacyWake(pokeCh, dispatchCh))
		close(done)
	}()
	if _, err := client.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write command: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatalf("read reply to %q: %v", line, err)
	}
	client.Close() //nolint:errcheck
	awaitClose(t, done, "handleControllerConn to exit")
	return reply
}

func TestControllerSocketPokeAcceptsOptionalKey(t *testing.T) {
	tests := []struct {
		name         string
		line         string
		wantPoke     bool
		wantDispatch bool
	}{
		{name: "legacy key-less poke", line: "poke", wantPoke: true},
		{name: "keyed session poke", line: "poke:" + reconcilekey.Session("gc-1").Encode(), wantPoke: true},
		{name: "keyed session-by-name poke", line: "poke:" + reconcilekey.SessionNamed("worker-1").Encode(), wantPoke: true},
		{name: "keyed allocator poke", line: "poke:" + reconcilekey.Allocator().Encode(), wantPoke: true},
		{name: "keyed control-dispatch poke", line: "poke:" + reconcilekey.ControlDispatch().Encode(), wantDispatch: true},
		{name: "malformed key degrades to allocator", line: "poke:{not json", wantPoke: true},
		{name: "empty key degrades to allocator", line: "poke:", wantPoke: true},
		{name: "legacy control-dispatcher", line: "control-dispatcher", wantDispatch: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pokeCh := make(chan struct{}, 1)
			dispatchCh := make(chan struct{}, 1)
			if reply := runControllerSocketCommand(t, tt.line, pokeCh, dispatchCh); reply != "ok\n" {
				t.Fatalf("reply = %q, want ok", reply)
			}
			if got := drainSignal(pokeCh); got != tt.wantPoke {
				t.Fatalf("poke signaled = %v, want %v", got, tt.wantPoke)
			}
			if got := drainSignal(dispatchCh); got != tt.wantDispatch {
				t.Fatalf("control-dispatcher signaled = %v, want %v", got, tt.wantDispatch)
			}
		})
	}
}

// fakeControllerSocket records every command line a fake socket receives.
type fakeControllerSocket struct {
	mu       sync.Mutex
	commands []string
}

func (f *fakeControllerSocket) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

// record wraps respond so every command line is recorded before it is
// answered; a client that got its reply (or EOF) sees the line recorded.
func (f *fakeControllerSocket) record(respond func(line string) string) func(string) string {
	return func(line string) string {
		f.mu.Lock()
		f.commands = append(f.commands, line)
		f.mu.Unlock()
		return respond(line)
	}
}

func startRecordingControllerSocket(t *testing.T, cityPath string, respond func(line string) string) *fakeControllerSocket {
	t.Helper()
	f := &fakeControllerSocket{}
	startFakeUnixSocket(t, controllerSocketPath(cityPath), f.record(respond))
	return f
}

// legacyControllerResponds acks the verbs an old controller knows and
// closes without replying on anything else, as an unknown verb does.
func legacyControllerResponds(line string) string {
	if line == "poke" || line == "control-dispatcher" {
		return "ok\n"
	}
	return ""
}

func keyedControllerResponds(line string) string {
	if strings.HasPrefix(line, pokeKeyedCommandPrefix) {
		return "ok\n"
	}
	return legacyControllerResponds(line)
}

// isolateSupervisorSocket points GC_HOME/XDG_RUNTIME_DIR at fresh dirs so
// supervisor fallbacks never reach a host supervisor. With no recorder
// listening there, a supervisor fallback fails, so a nil error from an
// enqueue proves the controller socket answered it.
func isolateSupervisorSocket(t *testing.T) {
	t.Helper()
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))
	t.Setenv("XDG_RUNTIME_DIR", shortSocketTempDir(t, "gc-run-"))
}

// supervisorReloadRecorder isolates the supervisor socket and records the
// commands it receives.
func supervisorReloadRecorder(t *testing.T) *fakeControllerSocket {
	t.Helper()
	isolateSupervisorSocket(t)
	f := &fakeControllerSocket{}
	startFakeUnixSocket(t, supervisorSocketPath(), f.record(func(string) string { return "" }))
	return f
}

func assertCommands(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%s commands = %q, want %q", what, got, want)
	}
}

func TestEnqueueControllerSendsKeyedPokeToKeyAwareController(t *testing.T) {
	isolateSupervisorSocket(t)
	cityPath := shortSocketTempDir(t, "gc-enq-")
	sock := startRecordingControllerSocket(t, cityPath, keyedControllerResponds)

	if err := enqueueController(cityPath, reconcilekey.Session("gc-1")); err != nil {
		t.Fatalf("enqueueController (a supervisor fallback would fail here): %v", err)
	}
	assertCommands(t, "controller", sock.seen(), []string{keyedPokeCommand(reconcilekey.Session("gc-1"))})
}

func TestEnqueueControllerFallsBackToLegacyPokeOnOldController(t *testing.T) {
	isolateSupervisorSocket(t)
	cityPath := shortSocketTempDir(t, "gc-enq-")
	sock := startRecordingControllerSocket(t, cityPath, legacyControllerResponds)

	if err := enqueueController(cityPath, reconcilekey.Session("gc-1")); err != nil {
		t.Fatalf("enqueueController against an old controller (a supervisor fallback would fail here): %v", err)
	}
	assertCommands(t, "controller", sock.seen(), []string{keyedPokeCommand(reconcilekey.Session("gc-1")), "poke"})
}

func TestEnqueueControllerKeepsLegacyVerbsForAllocatorAndControlDispatch(t *testing.T) {
	isolateSupervisorSocket(t)
	cityPath := shortSocketTempDir(t, "gc-enq-")
	sock := startRecordingControllerSocket(t, cityPath, legacyControllerResponds)

	if err := enqueueController(cityPath, reconcilekey.Allocator()); err != nil {
		t.Fatalf("enqueueController(allocator): %v", err)
	}
	if err := enqueueController(cityPath, reconcilekey.ControlDispatch()); err != nil {
		t.Fatalf("enqueueController(control dispatch): %v", err)
	}
	assertCommands(t, "controller", sock.seen(), []string{"poke", "control-dispatcher"})
}

// With no controller socket, a session enqueue dials the city socket once
// (the keyed attempt fails as unavailable, which is not retried) and then
// falls back to the supervisor exactly like pokeController does.
func TestEnqueueControllerFallsBackToSupervisorOnlyAfterSocketFails(t *testing.T) {
	sup := supervisorReloadRecorder(t)
	cityPath := shortSocketTempDir(t, "gc-enq-")

	if err := enqueueController(cityPath, reconcilekey.Session("gc-1")); err != nil {
		t.Fatalf("enqueueController with supervisor fallback: %v", err)
	}
	// pokeSupervisor does not wait for a reply, so the record is async.
	awaitCond(t, func() bool { return len(sup.seen()) == 1 }, "supervisor reload")
	assertCommands(t, "supervisor", sup.seen(), []string{"reload"})
}

func TestSendKeyedPokeRetriesOnlyForOldControllerSignature(t *testing.T) {
	timeoutErr := controllerCommandError{op: "reading response", err: os.ErrDeadlineExceeded, unresponsive: true}
	tests := []struct {
		name      string
		first     func() ([]byte, error)
		wantSends int
		wantErr   bool
	}{
		{name: "acked", first: func() ([]byte, error) { return []byte("ok"), nil }, wantSends: 1},
		{name: "old controller closed without reply", first: func() ([]byte, error) {
			return nil, controllerCommandError{op: "reading response", err: io.ErrUnexpectedEOF, unresponsive: true}
		}, wantSends: 2},
		{name: "non-ok reply", first: func() ([]byte, error) { return []byte("error: nope"), nil }, wantSends: 2},
		{name: "controller unavailable", first: func() ([]byte, error) {
			return nil, controllerCommandError{op: "connecting to controller", err: errors.New("refused"), unavailable: true}
		}, wantSends: 1, wantErr: true},
		{name: "read timeout", first: func() ([]byte, error) { return nil, timeoutErr }, wantSends: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sent []string
			err := sendKeyedPoke(reconcilekey.Session("gc-1"), func(command string) ([]byte, error) {
				sent = append(sent, command)
				if len(sent) == 1 {
					return tt.first()
				}
				return []byte("ok"), nil
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(sent) != tt.wantSends {
				t.Fatalf("sends = %q, want %d", sent, tt.wantSends)
			}
			if tt.wantSends == 2 && sent[1] != "poke" {
				t.Fatalf("retry = %q, want plain poke", sent[1])
			}
		})
	}
}

func TestSendKeyedPokeHungControllerCostsOneTimeout(t *testing.T) {
	cityPath := shortSocketTempDir(t, "gc-hung-")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sock := startRecordingControllerSocket(t, cityPath, func(string) string { <-release; return "" })

	start := time.Now()
	err := sendKeyedPoke(reconcilekey.Session("gc-1"), func(command string) ([]byte, error) {
		return sendControllerCommandWithTimeouts(cityPath, command, time.Second, time.Second, 300*time.Millisecond)
	})
	if !errors.Is(err, errControllerUnresponsive) {
		t.Fatalf("err = %v, want unresponsive (read timeout)", err)
	}
	// Two timeouts would take at least 600ms; the command list below is the
	// authoritative no-retry check, this bound only rejects a double wait.
	if elapsed := time.Since(start); elapsed >= 600*time.Millisecond {
		t.Fatalf("elapsed = %s, want a single 300ms timeout", elapsed)
	}
	assertCommands(t, "controller", sock.seen(), []string{keyedPokeCommand(reconcilekey.Session("gc-1"))})
}

func TestPokeControllerForRestartSendsKeyAndFallsBack(t *testing.T) {
	key := reconcilekey.SessionNamed("worker-1")

	newCity := shortSocketTempDir(t, "gc-rst-")
	newSock := startRecordingControllerSocket(t, newCity, keyedControllerResponds)
	if err := pokeControllerForRestart(newCity, key); err != nil {
		t.Fatalf("pokeControllerForRestart(new controller): %v", err)
	}
	assertCommands(t, "new controller", newSock.seen(), []string{keyedPokeCommand(key)})

	oldCity := shortSocketTempDir(t, "gc-rst-")
	oldSock := startRecordingControllerSocket(t, oldCity, legacyControllerResponds)
	if err := pokeControllerForRestart(oldCity, key); err != nil {
		t.Fatalf("pokeControllerForRestart(old controller): %v", err)
	}
	assertCommands(t, "old controller", oldSock.seen(), []string{keyedPokeCommand(key), "poke"})
}

// An explicit restart must report a missing controller instead of letting
// an unrelated supervisor answer for this city.
func TestPokeControllerForRestartWithoutControllerErrorsWithoutSupervisorFallback(t *testing.T) {
	// A live supervisor: falling back to it would succeed, so the error
	// below proves pokeControllerForRestart never did.
	supervisorReloadRecorder(t)
	cityPath := shortSocketTempDir(t, "gc-rst-")

	err := pokeControllerForRestart(cityPath, reconcilekey.SessionNamed("worker-1"))
	if err == nil {
		t.Fatal("pokeControllerForRestart with no controller succeeded, want error")
	}
	if !errors.Is(err, errControllerUnavailable) {
		t.Fatalf("err = %v, want controller unavailable", err)
	}
}

// captureWiredControllerStates records every city runtime whose controller
// state is installed while the test runs (controllerStateWiredHook).
func captureWiredControllerStates(t *testing.T) func() []*CityRuntime {
	t.Helper()
	var mu sync.Mutex
	var wired []*CityRuntime
	prev := controllerStateWiredHook
	controllerStateWiredHook = func(cr *CityRuntime) {
		mu.Lock()
		defer mu.Unlock()
		wired = append(wired, cr)
	}
	t.Cleanup(func() { controllerStateWiredHook = prev })
	return func() []*CityRuntime {
		mu.Lock()
		defer mu.Unlock()
		return append([]*CityRuntime(nil), wired...)
	}
}

// assertWakeSignalsWired checks each entry point's wiring: the API's wake
// signals exactly the channels the runtime's run loop selects on.
func assertWakeSignalsWired(t *testing.T, runtimes []*CityRuntime) {
	t.Helper()
	if len(runtimes) == 0 {
		t.Fatal("no controllerState was wired")
	}
	for _, cr := range runtimes {
		cs := cr.cs
		if cs == nil {
			t.Fatal("the runtime has no controller state")
		}
		if cs.wake == nil {
			t.Fatal("controllerState.wake is nil: API enqueues would reach no controller")
		}
		if cs.wake.pokeCh == nil {
			t.Fatal("controllerState.wake.pokeCh is nil: API enqueues would be dropped")
		}
		if cs.wake.controlDispatcherCh == nil {
			t.Fatal("controllerState.wake.controlDispatcherCh is nil: API control-dispatch enqueues would be dropped")
		}
		if cr.pokeCh != cs.wake.pokeCh {
			t.Fatal("the API wake's pokeCh is not the runtime's: API enqueues would wake no tick")
		}
		if cr.controlDispatcherCh != cs.wake.controlDispatcherCh {
			t.Fatal("the API wake's controlDispatcherCh is not the runtime's: API control-dispatch enqueues would wake no dispatcher tick")
		}
	}
}

func TestSupervisorStartOneCityWiresControllerStateWakeSignals(t *testing.T) {
	wired := captureWiredControllerStates(t)
	launchSupervisorWireCity(t, "")
	assertWakeSignalsWired(t, wired())
}

// launchSupervisorWireCity starts one city through the supervisor's
// reconcileCities, with daemon appended to its [daemon] section, and stops it
// when the test ends. It fails the test unless the city finished starting.
func launchSupervisorWireCity(t *testing.T, daemon string) {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())

	cityPath := shortSocketTempDir(t, "gc-wire-sup-")
	cleanupManagedDoltTestCity(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "wire-city"

[orders]
skip = ["beads-health", "cross-rig-deps", "gate-sweep", "jsonl-export", "reaper", "order-tracking-sweep", "orphan-sweep", "prune-branches", "spawn-storm-detect", "wisp-compact"]

[session]
provider = "fake"

[daemon]
shutdown_timeout = "100ms"
` + daemon
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeSpyScript(t, filepath.Join(t.TempDir(), "ops.log"))
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "wire-city"); err != nil {
		t.Fatal(err)
	}
	cr := newCityRegistry()
	var stdout, stderr lockedBuffer
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
	t.Cleanup(func() {
		if done := cr.CancelCity(canonicalTestPath(cityPath)); done != nil {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("city goroutine did not exit in time")
			}
		}
	})
	if !strings.Contains(stderr.String(), "Launching city") && !strings.Contains(stdout.String(), "Launching city") {
		t.Fatalf("city never finished starting (stderr: %s)", stderr.String())
	}
}
