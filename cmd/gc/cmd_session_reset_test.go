package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionReset_ClearsCircuitBreaker verifies that running
// `gc session reset <identity>` clears a tripped session circuit breaker
// for the matching named session, so the supervisor will respawn the
// session on the next tick. This is the operator-facing remediation path
// the breaker's ERROR log message points at.
func TestCmdSessionReset_ClearsCircuitBreaker(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-reset-cb-")
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "mayor"
	if _, err := store.Create(beads.Bead{
		Title:  "named mayor",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:mayor"},
		Metadata: map[string]string{
			"alias":                      identity,
			"template":                   "mayor",
			"session_name":               "s-gc-reset-cb-test",
			"state":                      "awake",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	}); err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	// Stand up a fake controller socket so cmdSessionReset's pokeController
	// + cityUsesManagedReconciler check both succeed.
	sockPath := filepath.Join(cityDir, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	defer lis.Close() //nolint:errcheck
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 64)
			n, _ := conn.Read(buf)
			reply := "ok\n"
			if string(buf[:n]) == "ping\n" {
				reply = "123\n"
			}
			conn.Write([]byte(reply)) //nolint:errcheck
			conn.Close()              //nolint:errcheck
		}
	}()

	// Trip the breaker for "mayor" by recording enough restarts inside
	// the rolling window with no progress events.
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{
		Window:      30 * time.Minute,
		MaxRestarts: 3,
	})
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()
	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		cb.RecordRestart(identity, now.Add(time.Duration(i)*time.Second))
	}
	if !cb.IsOpen(identity, now.Add(time.Minute)) {
		t.Fatalf("precondition: expected breaker OPEN for %q after 4 restarts", identity)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionReset([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionReset = %d, want 0; stderr=%s", code, stderr.String())
	}

	if cb.IsOpen(identity, now.Add(time.Minute)) {
		t.Fatalf("breaker still OPEN for %q after `gc session reset %s`", identity, identity)
	}
}

func TestCmdSessionReset_RequestsFreshRestartWithController(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-reset-")
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "manual mayor",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:mayor"},
		Metadata: map[string]string{
			"alias":                      "sky",
			"template":                   "mayor",
			"session_name":               "s-gc-reset-test",
			"state":                      "awake",
			"session_key":                "original-key",
			"started_config_hash":        "hash-before-reset",
			"continuation_reset_pending": "",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	sockPath := filepath.Join(cityDir, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	defer lis.Close() //nolint:errcheck

	commands := make(chan string, 3)
	errCh := make(chan error, 1)
	go func() {
		defer close(commands)
		for i := 0; i < 3; i++ {
			conn, err := lis.Accept()
			if err != nil {
				errCh <- err
				return
			}
			buf := make([]byte, 64)
			n, err := conn.Read(buf)
			if err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			cmd := string(buf[:n])
			commands <- cmd
			reply := "ok\n"
			if cmd == "ping\n" {
				reply = "123\n"
			}
			if _, err := conn.Write([]byte(reply)); err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			conn.Close() //nolint:errcheck
		}
	}()

	var stdout, stderr bytes.Buffer
	if code := cmdSessionReset([]string{"sky"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionReset(controller) = %d, want 0; stderr=%s", code, stderr.String())
	}

	gotCommands := make([]string, 0, 3)
	deadline := time.After(2 * time.Second)
	for len(gotCommands) < 3 {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("controller socket: %v", err)
			}
		case cmd, ok := <-commands:
			if !ok {
				if len(gotCommands) != 3 {
					t.Fatalf("controller commands = %v, want ping plus 2 pokes", gotCommands)
				}
				break
			}
			gotCommands = append(gotCommands, cmd)
		case <-deadline:
			t.Fatalf("timed out waiting for controller pokes, got %v", gotCommands)
		}
	}
	wantCommands := []string{"ping\n", "poke\n", "poke\n"}
	for i, want := range wantCommands {
		if gotCommands[i] != want {
			t.Fatalf("controller command %d = %q, want %q", i, gotCommands[i], want)
		}
	}

	reloaded, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(reload): %v", err)
	}
	got, err := reloaded.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got.Metadata["restart_requested"] != "true" {
		t.Fatalf("restart_requested = %q, want true", got.Metadata["restart_requested"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["session_key"] != "original-key" {
		t.Fatalf("session_key = %q, want original key preserved until reconcile", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "hash-before-reset" {
		t.Fatalf("started_config_hash = %q, want original hash preserved until reconcile", got.Metadata["started_config_hash"])
	}
}
