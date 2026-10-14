package main

import (
	"os"
	"path/filepath"
	"testing"

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
