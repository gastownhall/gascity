package main

import (
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

func TestSessionWake_StateTransitionsAndMetadata(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name            string
		metadata        map[string]string
		wantState       string
		wantSleepReason string
	}{
		{
			name: "suspended becomes asleep",
			metadata: map[string]string{
				"template":     "worker",
				"state":        "suspended",
				"held_until":   future,
				"sleep_reason": "user-hold",
			},
			wantState:       "asleep",
			wantSleepReason: "",
		},
		{
			name: "drained becomes asleep",
			metadata: map[string]string{
				"template":     "worker",
				"state":        "drained",
				"sleep_reason": "drained",
			},
			wantState:       "asleep",
			wantSleepReason: "",
		},
		{
			name: "creating clears quarantine but stays creating",
			metadata: map[string]string{
				"template":          "worker",
				"state":             "creating",
				"quarantined_until": future,
				"sleep_reason":      "quarantine",
				"wake_attempts":     "5",
			},
			wantState:       "creating",
			wantSleepReason: "",
		},
		{
			name: "active stays active",
			metadata: map[string]string{
				"template":     "worker",
				"state":        "active",
				"sleep_reason": "idle",
			},
			wantState:       "active",
			wantSleepReason: "idle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{
				Type:     session.BeadType,
				Labels:   []string{session.LabelSession},
				Metadata: tt.metadata,
			})
			if err != nil {
				t.Fatalf("store.Create(): %v", err)
			}

			if _, err := session.WakeSession(store, b, time.Now()); err != nil {
				t.Fatalf("WakeSession: %v", err)
			}

			updated, err := store.Get(b.ID)
			if err != nil {
				t.Fatalf("store.Get(%s): %v", b.ID, err)
			}
			if got := updated.Metadata["state"]; got != tt.wantState {
				t.Fatalf("state = %q, want %q", got, tt.wantState)
			}
			if got := updated.Metadata["sleep_reason"]; got != tt.wantSleepReason {
				t.Fatalf("sleep_reason = %q, want %q", got, tt.wantSleepReason)
			}
			if got := updated.Metadata["held_until"]; got != "" {
				t.Fatalf("held_until = %q, want empty", got)
			}
			if got := updated.Metadata["quarantined_until"]; got != "" {
				t.Fatalf("quarantined_until = %q, want empty", got)
			}
			if got := updated.Metadata["wait_hold"]; got != "" {
				t.Fatalf("wait_hold = %q, want empty", got)
			}
			if got := updated.Metadata["sleep_intent"]; got != "" {
				t.Fatalf("sleep_intent = %q, want empty", got)
			}
			if got := updated.Metadata["wake_attempts"]; got != "0" {
				t.Fatalf("wake_attempts = %q, want 0", got)
			}
			if got := updated.Metadata["churn_count"]; got != "0" {
				t.Fatalf("churn_count = %q, want 0", got)
			}
		})
	}
}

func TestCmdSessionWake_PokesManagedControllerAndMovesSuspendedToAsleep(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	rootDir, err := os.MkdirTemp("", "gcw-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rootDir) })
	cityDir := filepath.Join(rootDir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", cityDir, err)
	}
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig(%q): %v", cityDir, err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	sessionID, err := resolveSessionIDMaterializingNamed(cityDir, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	if err := store.SetMetadataBatch(sessionID, map[string]string{
		"state":        "suspended",
		"held_until":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"sleep_reason": "user-hold",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(%s): %v", sessionID, err)
	}

	sockPath := filepath.Join(cityDir, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	defer lis.Close() //nolint:errcheck

	commands := make(chan string, 2)
	errCh := make(chan error, 1)
	go func() {
		defer close(commands)
		for range 2 {
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
	if code := cmdSessionWake([]string{"mayor"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionWake() = %d, want 0; stderr=%s", code, stderr.String())
	}

	gotCommands := make([]string, 0, 2)
	deadline := time.After(2 * time.Second)
	for len(gotCommands) < 2 {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("controller socket: %v", err)
			}
		case cmd, ok := <-commands:
			if !ok {
				t.Fatalf("controller commands = %v, want ping plus poke", gotCommands)
			}
			gotCommands = append(gotCommands, cmd)
		case <-deadline:
			t.Fatalf("timed out waiting for controller commands, got %v", gotCommands)
		}
	}
	wantCommands := []string{"ping\n", "poke\n"}
	for i, want := range wantCommands {
		if gotCommands[i] != want {
			t.Fatalf("controller command %d = %q, want %q", i, gotCommands[i], want)
		}
	}

	freshStore, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	updated, err := freshStore.Get(sessionID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", sessionID, err)
	}
	if got := updated.Metadata["state"]; got != "asleep" {
		t.Fatalf("state = %q, want asleep", got)
	}
	if got := updated.Metadata["held_until"]; got != "" {
		t.Fatalf("held_until = %q, want empty", got)
	}
	if got := updated.Metadata["sleep_reason"]; got != "" {
		t.Fatalf("sleep_reason = %q, want empty", got)
	}
}

func TestCmdSessionWake_RejectsArchivedHistoricalSessionID(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	rootDir, err := os.MkdirTemp("", "gcw-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rootDir) })
	cityDir := filepath.Join(rootDir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", cityDir, err)
	}
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig(%q): %v", cityDir, err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	sessionID, err := resolveSessionIDMaterializingNamed(cityDir, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	if err := store.SetMetadataBatch(sessionID, map[string]string{
		"state":               "archived",
		"continuity_eligible": "false",
		"alias":               "",
		"session_name":        "",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(%s): %v", sessionID, err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionWake([]string{sessionID}, &stdout, &stderr); code == 0 {
		t.Fatalf("cmdSessionWake() = %d, want rejection; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestCmdSessionWake_RequestsStartForContinuityEligibleArchivedSessionID(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	rootDir, err := os.MkdirTemp("", "gcw-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rootDir) })
	cityDir := filepath.Join(rootDir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", cityDir, err)
	}
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig(%q): %v", cityDir, err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	sessionID, err := resolveSessionIDMaterializingNamed(cityDir, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor): %v", err)
	}
	if err := store.SetMetadataBatch(sessionID, map[string]string{
		"state":               "archived",
		"continuity_eligible": "true",
		"archived_at":         time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("SetMetadataBatch(%s): %v", sessionID, err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionWake([]string{sessionID}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionWake() = %d, want success; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "wake requested") {
		t.Fatalf("stdout = %q, want wake requested message", stdout.String())
	}

	freshStore, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	updated, err := freshStore.Get(sessionID)
	if err != nil {
		t.Fatalf("Get(%s): %v", sessionID, err)
	}
	if got := updated.Metadata["state"]; got != "creating" {
		t.Fatalf("state = %q, want creating", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
}
