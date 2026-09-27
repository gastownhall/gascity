package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
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

// fakeControllerSocket records every command line and answers "ok" to the
// ones accepted; anything else gets the old controller's treatment for an
// unknown verb (close without a reply).
type fakeControllerSocket struct {
	mu       sync.Mutex
	commands []string
}

func (f *fakeControllerSocket) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func startRecordingControllerSocket(t *testing.T, cityPath string, accept func(line string) bool) *fakeControllerSocket {
	t.Helper()
	sockPath := controllerSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
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
				if accept(line) {
					_, _ = conn.Write([]byte("ok\n"))
				}
			}(conn)
		}
	}()
	return f
}

func legacyControllerAccepts(line string) bool {
	return line == "poke" || line == "control-dispatcher"
}

func keyedControllerAccepts(line string) bool {
	return legacyControllerAccepts(line) || strings.HasPrefix(line, pokeKeyedCommandPrefix)
}

func TestEnqueueControllerSendsKeyedPokeToKeyAwareController(t *testing.T) {
	cityPath := shortSocketTempDir(t, "gc-enq-")
	sock := startRecordingControllerSocket(t, cityPath, keyedControllerAccepts)

	if err := enqueueController(cityPath, reconcilekey.Session("gc-1")); err != nil {
		t.Fatalf("enqueueController: %v", err)
	}
	want := []string{"poke:" + reconcilekey.Session("gc-1").Encode()}
	if got := sock.seen(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestEnqueueControllerFallsBackToLegacyPokeOnOldController(t *testing.T) {
	cityPath := shortSocketTempDir(t, "gc-enq-")
	sock := startRecordingControllerSocket(t, cityPath, legacyControllerAccepts)

	if err := enqueueController(cityPath, reconcilekey.Session("gc-1")); err != nil {
		t.Fatalf("enqueueController against an old controller: %v", err)
	}
	want := []string{"poke:" + reconcilekey.Session("gc-1").Encode(), "poke"}
	if got := sock.seen(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestEnqueueControllerKeepsLegacyVerbsForAllocatorAndControlDispatch(t *testing.T) {
	cityPath := shortSocketTempDir(t, "gc-enq-")
	sock := startRecordingControllerSocket(t, cityPath, legacyControllerAccepts)

	if err := enqueueController(cityPath, reconcilekey.Allocator()); err != nil {
		t.Fatalf("enqueueController(allocator): %v", err)
	}
	if err := enqueueController(cityPath, reconcilekey.ControlDispatch()); err != nil {
		t.Fatalf("enqueueController(control dispatch): %v", err)
	}
	want := []string{"poke", "control-dispatcher"}
	if got := sock.seen(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestPokeControllerForRestartSendsKeyAndFallsBack(t *testing.T) {
	key := reconcilekey.SessionNamed("worker-1")

	newCity := shortSocketTempDir(t, "gc-rst-")
	newSock := startRecordingControllerSocket(t, newCity, keyedControllerAccepts)
	if err := pokeControllerForRestart(newCity, key); err != nil {
		t.Fatalf("pokeControllerForRestart(new controller): %v", err)
	}
	if got, want := newSock.seen(), []string{"poke:" + key.Encode()}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("new controller commands = %q, want %q", got, want)
	}

	oldCity := shortSocketTempDir(t, "gc-rst-")
	oldSock := startRecordingControllerSocket(t, oldCity, legacyControllerAccepts)
	if err := pokeControllerForRestart(oldCity, key); err != nil {
		t.Fatalf("pokeControllerForRestart(old controller): %v", err)
	}
	if got, want := oldSock.seen(), []string{"poke:" + key.Encode(), "poke"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("old controller commands = %q, want %q", got, want)
	}
}
