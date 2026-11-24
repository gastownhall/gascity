package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/session"
)

// syncControllerResponds answers session-sync with reply and every other
// verb the way a current controller does.
func syncControllerResponds(reply string) func(string) string {
	return func(line string) string {
		switch {
		case line == "ping":
			return "123\n"
		case strings.HasPrefix(line, sessionSyncCommandPrefix):
			return reply
		default:
			return keyedControllerResponds(line)
		}
	}
}

// blockingSyncResponds never answers session-sync, so the client times out.
func blockingSyncResponds(t *testing.T) func(string) string {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(line string) string {
		if strings.HasPrefix(line, sessionSyncCommandPrefix) {
			<-release
			return ""
		}
		return syncControllerResponds(`{"outcome":"ok"}` + "\n")(line)
	}
}

// legacySyncResponds is a controller that predates session-sync but knows
// the keyed poke: it closes on session-sync without replying.
func legacySyncResponds(line string) string {
	if line == "ping" {
		return "123\n"
	}
	return keyedControllerResponds(line)
}

// mustSessionSyncCommand renders the session-sync command the client sends
// for sessionID.
func mustSessionSyncCommand(t *testing.T, sessionID string) string {
	t.Helper()
	command, err := sessionSyncCommand(sessionID)
	if err != nil {
		t.Fatalf("rendering session-sync command: %v", err)
	}
	return command
}

func shortSessionSyncTimeout(t *testing.T) {
	t.Helper()
	prev := sessionSyncReadTimeout
	sessionSyncReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { sessionSyncReadTimeout = prev })
}

func TestSyncSessionOnControllerOutcomes(t *testing.T) {
	syncCmd := mustSessionSyncCommand(t, "gc-1")
	keyed := keyedPokeCommand(reconcilekey.Session("gc-1"))
	tests := []struct {
		name         string
		respond      func(t *testing.T) func(string) string
		wantErr      error
		wantText     string
		wantCommands []string
	}{
		{
			name: "ok",
			respond: func(*testing.T) func(string) string {
				return syncControllerResponds(`{"outcome":"ok","generation":"4"}` + "\n")
			},
			wantCommands: []string{syncCmd},
		},
		{
			name:         "missing",
			respond:      func(*testing.T) func(string) string { return syncControllerResponds(`{"outcome":"missing"}` + "\n") },
			wantErr:      errSessionSyncMissing,
			wantCommands: []string{syncCmd},
		},
		{
			name: "three busy replies",
			respond: func(*testing.T) func(string) string {
				return syncControllerResponds(`{"outcome":"busy","error":"cache yielding"}` + "\n")
			},
			wantText:     "busy: cache yielding",
			wantCommands: []string{syncCmd, syncCmd, syncCmd},
		},
		{
			name: "failed",
			respond: func(*testing.T) func(string) string {
				return syncControllerResponds(`{"outcome":"failed","error":"backing read failed"}` + "\n")
			},
			wantText:     "failed: backing read failed",
			wantCommands: []string{syncCmd},
		},
		{
			name: "unknown outcome",
			respond: func(*testing.T) func(string) string {
				return syncControllerResponds(`{"outcome":"stalled"}` + "\n")
			},
			wantText:     `unknown outcome "stalled"`,
			wantCommands: []string{syncCmd},
		},
		{
			name:         "timeout",
			respond:      blockingSyncResponds,
			wantErr:      errControllerUnresponsive,
			wantCommands: []string{syncCmd},
		},
		{
			name:         "older controller gets the keyed poke",
			respond:      func(*testing.T) func(string) string { return legacySyncResponds },
			wantErr:      errSessionSyncUnsupported,
			wantCommands: []string{syncCmd, keyed},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shortSessionSyncTimeout(t)
			cityPath := shortSocketTempDir(t, "gc-sync-")
			sock := startRecordingControllerSocket(t, cityPath, tt.respond(t))

			err := syncSessionOnController(cityPath, "gc-1")
			switch {
			case tt.wantErr == nil && tt.wantText == "":
				if err != nil {
					t.Fatalf("syncSessionOnController = %v, want nil", err)
				}
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("syncSessionOnController = %v, want %v", err, tt.wantErr)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), tt.wantText) {
					t.Fatalf("syncSessionOnController = %v, want error containing %q", err, tt.wantText)
				}
			}
			assertCommands(t, "controller", sock.seen(), tt.wantCommands)
		})
	}
}

func TestSyncSessionOnControllerRefusesOnAWorkerSiteWithoutDialing(t *testing.T) {
	resetControlPlaneForTest(t)
	dials := countSocketDials(t)
	cityDir := t.TempDir()
	makeWorkerSite(t, cityDir)

	if err := syncSessionOnController(cityDir, "gc-1"); !errors.Is(err, errWorkerSiteNoController) {
		t.Fatalf("syncSessionOnController on a worker site = %v, want errWorkerSiteNoController", err)
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("socket dials = %d, want 0", n)
	}
}

// newResetSyncCity writes a city with one unnamed, awake session row and
// returns the city directory and the row's ID.
func newResetSyncCity(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	cityDir := shortSocketTempDir(t, "gc-reset-sync-")
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
			"alias":               "sky",
			"template":            "worker",
			"session_name":        "s-gc-reset-sync",
			"state":               "awake",
			"session_key":         "original-key",
			"started_config_hash": "hash-before-reset",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}
	return cityDir, bead.ID
}

func TestCmdSessionResetReportsAnUnacknowledgedReset(t *testing.T) {
	tests := []struct {
		name       string
		respond    func(t *testing.T) func(string) string
		reason     string
		syncs      int
		keyedPoke  bool
		jsonOutput bool
	}{
		{
			name: "three busy replies",
			respond: func(*testing.T) func(string) string {
				return syncControllerResponds(`{"outcome":"busy","error":"cache yielding"}` + "\n")
			},
			reason: "busy: cache yielding",
			syncs:  3,
		},
		{
			name: "failed",
			respond: func(*testing.T) func(string) string {
				return syncControllerResponds(`{"outcome":"failed","error":"backing read failed"}` + "\n")
			},
			reason:     "failed: backing read failed",
			syncs:      1,
			jsonOutput: true,
		},
		{
			name:    "timeout",
			respond: blockingSyncResponds,
			reason:  "i/o timeout",
			syncs:   1,
		},
		{
			name:      "older controller",
			respond:   func(*testing.T) func(string) string { return legacySyncResponds },
			reason:    errSessionSyncUnsupported.Error(),
			syncs:     1,
			keyedPoke: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shortSessionSyncTimeout(t)
			cityDir, id := newResetSyncCity(t)
			sock := startRecordingControllerSocket(t, cityDir, tt.respond(t))

			var stdout, stderr bytes.Buffer
			if code := cmdSessionReset([]string{"sky"}, "", &stdout, &stderr, tt.jsonOutput); code == 0 {
				t.Fatalf("cmdSessionReset = 0, want non-zero; stdout=%s", stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want no success text or JSON", stdout.String())
			}
			msg := stderr.String()
			for _, want := range []string{
				"reset is durably recorded on " + id,
				"controller did not confirm",
				tt.reason,
				"gc session kill " + id,
			} {
				if !strings.Contains(msg, want) {
					t.Fatalf("stderr = %q, want it to contain %q", msg, want)
				}
			}

			syncs, keyed := 0, 0
			for _, line := range sock.seen() {
				switch {
				case line == mustSessionSyncCommand(t, id):
					syncs++
				case line == keyedPokeCommand(reconcilekey.Session(id)):
					keyed++
				}
			}
			if syncs != tt.syncs {
				t.Fatalf("session-sync commands = %d, want %d; seen %v", syncs, tt.syncs, sock.seen())
			}
			if wantKeyed := map[bool]int{true: 1, false: 0}[tt.keyedPoke]; keyed != wantKeyed {
				t.Fatalf("keyed pokes = %d, want %d; seen %v", keyed, wantKeyed, sock.seen())
			}

			reloaded, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatalf("openCityStoreAt(reload): %v", err)
			}
			got, err := reloaded.Get(id)
			if err != nil {
				t.Fatalf("Get(%s): %v", id, err)
			}
			if got.Metadata["restart_requested"] != "true" || got.Metadata["continuation_reset_pending"] != "true" {
				t.Fatalf("markers = %q/%q, want the reset durably recorded", got.Metadata["restart_requested"], got.Metadata["continuation_reset_pending"])
			}
		})
	}
}
