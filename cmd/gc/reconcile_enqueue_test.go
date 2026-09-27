package main

import (
	"bufio"
	"bytes"
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

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
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
		handleControllerConn(server, t.TempDir(), controllerHostingStandalone, func() {}, nil, nil, nil, nil, pokeCh, dispatchCh)
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

// socketAction is how a fake controller socket answers one command.
type socketAction int

const (
	socketReplyOK socketAction = iota // ack with "ok"
	socketClose                       // close without replying (old controller, unknown verb)
	socketHang                        // hold the connection open, never reply
)

// fakeControllerSocket records every command line and answers per respond.
type fakeControllerSocket struct {
	mu       sync.Mutex
	commands []string
}

func (f *fakeControllerSocket) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func startRecordingControllerSocket(t *testing.T, cityPath string, respond func(line string) socketAction) *fakeControllerSocket {
	t.Helper()
	sockPath := controllerSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		_ = lis.Close()
		_ = os.Remove(sockPath)
	})
	f := &fakeControllerSocket{}
	go func() {
		for {
			conn, acceptErr := lis.Accept()
			if acceptErr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck // test cleanup
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				line, readErr := bufio.NewReader(conn).ReadString('\n')
				if readErr != nil {
					return
				}
				line = strings.TrimSuffix(line, "\n")
				f.mu.Lock()
				f.commands = append(f.commands, line)
				f.mu.Unlock()
				switch respond(line) {
				case socketReplyOK:
					_, _ = conn.Write([]byte("ok\n"))
				case socketHang:
					<-stop
				case socketClose:
				}
			}(conn)
		}
	}()
	return f
}

func legacyControllerResponds(line string) socketAction {
	if line == "poke" || line == "control-dispatcher" {
		return socketReplyOK
	}
	return socketClose
}

func keyedControllerResponds(line string) socketAction {
	if strings.HasPrefix(line, pokeKeyedCommandPrefix) {
		return socketReplyOK
	}
	return legacyControllerResponds(line)
}

func hungControllerResponds(string) socketAction { return socketHang }

// supervisorReloadRecorder listens on this test's supervisor socket and
// counts the commands it receives. It isolates GC_HOME/XDG_RUNTIME_DIR so
// fallbacks never reach a host supervisor.
func supervisorReloadRecorder(t *testing.T) *fakeControllerSocket {
	t.Helper()
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))
	t.Setenv("XDG_RUNTIME_DIR", shortSocketTempDir(t, "gc-run-"))
	sockPath := supervisorSocketPath()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	f := &fakeControllerSocket{}
	go func() {
		for {
			conn, acceptErr := lis.Accept()
			if acceptErr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck // test cleanup
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				line, _ := bufio.NewReader(conn).ReadString('\n')
				f.mu.Lock()
				f.commands = append(f.commands, strings.TrimSuffix(line, "\n"))
				f.mu.Unlock()
			}(conn)
		}
	}()
	return f
}

func assertCommands(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%s commands = %q, want %q", what, got, want)
	}
}

// eventually polls cond for up to 10s (socket handlers record asynchronously).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEnqueueControllerSendsKeyedPokeToKeyAwareController(t *testing.T) {
	sup := supervisorReloadRecorder(t)
	cityPath := shortSocketTempDir(t, "gc-enq-")
	sock := startRecordingControllerSocket(t, cityPath, keyedControllerResponds)

	if err := enqueueController(cityPath, reconcilekey.Session("gc-1")); err != nil {
		t.Fatalf("enqueueController: %v", err)
	}
	assertCommands(t, "controller", sock.seen(), []string{keyedPokeCommand(reconcilekey.Session("gc-1"))})
	time.Sleep(50 * time.Millisecond)
	assertCommands(t, "supervisor", sup.seen(), nil)
}

func TestEnqueueControllerFallsBackToLegacyPokeOnOldController(t *testing.T) {
	sup := supervisorReloadRecorder(t)
	cityPath := shortSocketTempDir(t, "gc-enq-")
	sock := startRecordingControllerSocket(t, cityPath, legacyControllerResponds)

	if err := enqueueController(cityPath, reconcilekey.Session("gc-1")); err != nil {
		t.Fatalf("enqueueController against an old controller: %v", err)
	}
	assertCommands(t, "controller", sock.seen(), []string{keyedPokeCommand(reconcilekey.Session("gc-1")), "poke"})
	time.Sleep(50 * time.Millisecond)
	assertCommands(t, "supervisor", sup.seen(), nil)
}

func TestEnqueueControllerKeepsLegacyVerbsForAllocatorAndControlDispatch(t *testing.T) {
	supervisorReloadRecorder(t)
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
	eventually(t, "supervisor reload", func() bool { return len(sup.seen()) == 1 })
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
	sock := startRecordingControllerSocket(t, cityPath, hungControllerResponds)

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
	sup := supervisorReloadRecorder(t)
	cityPath := shortSocketTempDir(t, "gc-rst-")

	err := pokeControllerForRestart(cityPath, reconcilekey.SessionNamed("worker-1"))
	if err == nil {
		t.Fatal("pokeControllerForRestart with no controller succeeded, want error")
	}
	if !errors.Is(err, errControllerUnavailable) {
		t.Fatalf("err = %v, want controller unavailable", err)
	}
	time.Sleep(50 * time.Millisecond)
	assertCommands(t, "supervisor", sup.seen(), nil)
}

// captureWiredControllerStates records every controllerState passed through
// wireControllerWakeSignals while the test runs.
func captureWiredControllerStates(t *testing.T) func() []*controllerState {
	t.Helper()
	var mu sync.Mutex
	var wired []*controllerState
	prev := controllerStateWiredHook
	controllerStateWiredHook = func(cs *controllerState) {
		mu.Lock()
		defer mu.Unlock()
		wired = append(wired, cs)
	}
	t.Cleanup(func() { controllerStateWiredHook = prev })
	return func() []*controllerState {
		mu.Lock()
		defer mu.Unlock()
		return append([]*controllerState(nil), wired...)
	}
}

func assertWakeSignalsWired(t *testing.T, states []*controllerState) {
	t.Helper()
	if len(states) == 0 {
		t.Fatal("no controllerState was wired")
	}
	for _, cs := range states {
		if cs.pokeCh == nil {
			t.Fatal("controllerState.pokeCh is nil: API enqueues would be dropped")
		}
		if cs.controlDispatcherCh == nil {
			t.Fatal("controllerState.controlDispatcherCh is nil: API control-dispatch enqueues would be dropped")
		}
	}
}

func TestRunControllerWiresControllerStateWakeSignals(t *testing.T) {
	wired := captureWiredControllerStates(t)
	sp := runtime.NewFake()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}
	dir := shortSocketTempDir(t, "gc-wire-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Beads:     config.BeadsConfig{Provider: "file"},
	}
	tomlPath := writeCityTOML(t, dir, "test")
	var stdout, stderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, nil, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &stdout, &stderr)
		close(done)
	}()
	t.Cleanup(func() {
		tryStopController(dir, &bytes.Buffer{})
		awaitClose(t, done, "controller to exit after stop")
	})
	waitForController(t, dir)
	// The socket comes up before the controller state is built.
	eventually(t, "controller state wiring", func() bool { return len(wired()) > 0 })
	assertWakeSignalsWired(t, wired())
}

func TestSupervisorStartOneCityWiresControllerStateWakeSignals(t *testing.T) {
	wired := captureWiredControllerStates(t)
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
`
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
	var stdout, stderr bytes.Buffer
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
	assertWakeSignalsWired(t, wired())
}
