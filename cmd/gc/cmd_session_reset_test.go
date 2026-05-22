package main

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "session-a"
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                        identity,
			"template":                     "worker",
			"session_name":                 "s-gc-reset-cb-test",
			"state":                        "awake",
			namedSessionMetadataKey:        "true",
			namedSessionIdentityMetadata:   identity,
			sessionCircuitStateMetadata:    circuitOpen.String(),
			sessionCircuitRestartsMetadata: `["2026-04-10T12:00:00Z"]`,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	// Trip the breaker by recording enough restarts inside
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

	lis, err := startControllerSocket(
		cityDir,
		func() {},
		nil,
		nil,
		make(chan reloadRequest),
		make(chan convergenceRequest, 1),
		make(chan struct{}, 1),
		make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("startControllerSocket: %v", err)
	}
	defer lis.Close()                              //nolint:errcheck
	defer os.Remove(controllerSocketPath(cityDir)) //nolint:errcheck

	var stdout, stderr bytes.Buffer
	if code := cmdSessionReset([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionReset = %d, want 0; stderr=%s", code, stderr.String())
	}

	if cb.IsOpen(identity, now.Add(time.Minute)) {
		t.Fatalf("breaker still OPEN for %q after `gc session reset %s`", identity, identity)
	}
	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(session bead): %v", err)
	}
	if got := updated.Metadata[sessionCircuitStateMetadata]; got != "" {
		t.Fatalf("persisted circuit state = %q, want cleared", got)
	}
	if got := updated.Metadata[sessionCircuitRestartsMetadata]; got != "" {
		t.Fatalf("persisted restart history = %q, want cleared", got)
	}
}

func TestCmdSessionReset_RequestsFreshRestartWithController(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-reset-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "manual session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                      "sky",
			"template":                   "worker",
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
					t.Fatalf("controller commands = %v, want ping, poke, poke", gotCommands)
				}
				break
			}
			gotCommands = append(gotCommands, cmd)
		case <-deadline:
			t.Fatalf("timed out waiting for controller pokes, got %v", gotCommands)
		}
	}
	wantExact := []string{"ping\n", "poke\n", "poke\n"}
	for i, want := range wantExact {
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

func TestCmdSessionReset_ControllerClearFailureDoesNotQueueRestart(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-reset-clear-fail-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "generic named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                      "session-a",
			"template":                   "worker",
			"session_name":               "s-gc-reset-clear-fail",
			"state":                      "awake",
			"session_key":                "original-key",
			"started_config_hash":        "hash-before-reset",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "session-a",
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
			buf := make([]byte, 256)
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
			} else if strings.HasPrefix(cmd, "session-circuit-reset:") {
				reply = `{"outcome":"failed","error":"clear failed"}` + "\n"
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
	if code := cmdSessionReset([]string{"session-a"}, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdSessionReset = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `clearing session circuit breaker for "session-a": clear failed`) {
		t.Fatalf("stderr = %q, want controller clear failure", stderr.String())
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
				t.Fatalf("controller commands = %v, want ping, poke, reset", gotCommands)
			}
			gotCommands = append(gotCommands, cmd)
		case <-deadline:
			t.Fatalf("timed out waiting for controller commands, got %v", gotCommands)
		}
	}
	if gotCommands[0] != "ping\n" || gotCommands[1] != "poke\n" || !strings.HasPrefix(gotCommands[2], "session-circuit-reset:") {
		t.Fatalf("controller commands = %v, want ping, poke, reset", gotCommands)
	}

	reloaded, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(reload): %v", err)
	}
	got, err := reloaded.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got.Metadata["restart_requested"] == "true" {
		t.Fatalf("restart_requested = true, want no queued reset after controller clear failure")
	}
	if got.Metadata["continuation_reset_pending"] == "true" {
		t.Fatalf("continuation_reset_pending = true, want no queued reset after controller clear failure")
	}
}

func TestResetSessionCircuitBreakerOnControllerMalformedReply(t *testing.T) {
	cityDir := shortSocketTempDir(t, "gc-session-reset-malformed-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	sockPath := filepath.Join(cityDir, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	defer lis.Close() //nolint:errcheck

	errCh := make(chan error, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close() //nolint:errcheck
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			errCh <- scanner.Err()
			return
		}
		if _, err := conn.Write([]byte("not-json\n")); err != nil {
			errCh <- err
		}
	}()

	err = resetSessionCircuitBreakerOnController(cityDir, "session-id", "rig-a/session-a")
	if err == nil {
		t.Fatal("resetSessionCircuitBreakerOnController = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decoding session circuit reset reply") {
		t.Fatalf("error = %v, want decode context", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("controller socket: %v", err)
		}
	default:
	}
}

func writeGenericNamedSessionCityTOML(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	data := []byte(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "session-a"
provider = "codex"
start_command = "echo"

[[named_session]]
template = "session-a"
`)
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}
