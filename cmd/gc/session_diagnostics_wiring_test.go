package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestTmuxConfigRuntimeDirReachesDoctorSessionDiagnostics runs the real
// production wiring end to end: the tmux provider config as gc actually builds
// it, through the path the diagnostic writers compose from its RuntimeDir, into
// the directory gc doctor's nudge-unconfirmed check actually scans.
//
// This is the regression test for dr-6siig HIGH finding 1. Both sides had unit
// tests before, and both passed, because each hardcoded its own assumption
// about where the directory is; the writers wrote to <city>/.gc/sessions and
// the check scanned <city>/.gc/runtime/sessions, so a real unconfirmed nudge
// produced a file on disk and a green doctor. Nothing that asserts one side
// against its own assumption can catch that -- only composing the production
// configuration can, which is what this does.
func TestTmuxConfigRuntimeDirReachesDoctorSessionDiagnostics(t *testing.T) {
	city := t.TempDir()

	// Exactly what gc builds for a live session.
	cfg := tmuxConfigFromSession(config.SessionConfig{}, "test-city", city)
	if cfg.RuntimeDir == "" {
		t.Fatal("production tmux config carries no RuntimeDir; diagnostic capture would be disabled fleet-wide")
	}

	// The directory the writers compose from that RuntimeDir, for one session.
	sessionDir := filepath.Join(citylayout.SessionDiagnosticsDirForRuntimeDir(cfg.RuntimeDir), "gc-worker-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	artifact := filepath.Join(sessionDir, "nudge-unconfirmed.log")
	if err := os.WriteFile(artifact, []byte("session: gc-worker-1\n"), 0o600); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}

	// The check must see it. A StatusOK here is the false green.
	r := doctor.NewNudgeUnconfirmedCheck().Run(&doctor.CheckContext{CityPath: city})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("doctor did not see a diagnostic written through the production runtime dir:\n"+
			"  artifact  = %s\n  status    = %v (%s)\n"+
			"the writers and the check are resolving different directories again", artifact, r.Status, r.Message)
	}
}

// TestDoctorNudgeUnconfirmedCheckSkipsClosedSessions wires the check the way
// buildDoctorChecks does -- live sessions read from the session store -- and
// asserts a diagnostic left by a closed session is not counted while a live
// session's is. Diagnostics are never cleared, so counting closed sessions
// makes the warning grow with the city's age.
func TestDoctorNudgeUnconfirmedCheckSkipsClosedSessions(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	oldOpen := openSessionProviderStore
	t.Cleanup(func() { openSessionProviderStore = oldOpen })
	openSessionProviderStore = func(string) (beads.Store, error) { return store, nil }

	for _, name := range []string{"gc-live-1", "gc-closed-1"} {
		b, err := store.Create(beads.Bead{
			Title:    name,
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel},
			Metadata: map[string]string{"session_name": name, "template": "worker", "state": "awake"},
		})
		if err != nil {
			t.Fatalf("create session bead %s: %v", name, err)
		}
		if name == "gc-closed-1" {
			if err := store.Close(b.ID); err != nil {
				t.Fatalf("close session bead %s: %v", name, err)
			}
		}
		dir := filepath.Join(citylayout.SessionDiagnosticsDir(city), name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "startup-nudge-unconfirmed.log"), []byte("session: "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write diagnostic: %v", err)
		}
	}

	check := doctor.NewNudgeUnconfirmedCheck(doctor.WithLiveSessionNames(doctorLiveSessionNames(city, nil)))
	r := check.Run(&doctor.CheckContext{CityPath: city})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s), want a warning for the live session", r.Status, r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "gc-live-1") {
		t.Fatalf("details = %v, want only gc-live-1", r.Details)
	}
	if !strings.Contains(r.Message, "1 closed session(s) skipped") {
		t.Fatalf("message = %q, want it to report the skipped closed session", r.Message)
	}
}
