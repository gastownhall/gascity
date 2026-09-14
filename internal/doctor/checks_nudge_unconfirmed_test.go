package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestNudgeUnconfirmedCheckOKWhenNoDiagnostics(t *testing.T) {
	cityRoot := t.TempDir()
	c := NewNudgeUnconfirmedCheck()
	r := c.Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s", r.Status, r.Message)
	}
}

func TestNudgeUnconfirmedCheckErrorsWhenDiagnosticsCannotBeRead(t *testing.T) {
	cityRoot := t.TempDir()
	sessionsDir := citylayout.SessionDiagnosticsDir(cityRoot)
	if err := os.MkdirAll(filepath.Dir(sessionsDir), 0o755); err != nil {
		t.Fatalf("mkdir runtime root: %v", err)
	}
	if err := os.WriteFile(sessionsDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write sessions path as file: %v", err)
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusError || r.Severity != SeverityAdvisory {
		t.Fatalf("unreadable diagnostics status = %v severity = %v, want advisory error: %s", r.Status, r.Severity, r.Message)
	}
	if !strings.Contains(r.Message, "cannot read session diagnostic directory") {
		t.Fatalf("unreadable diagnostics message = %q, want read failure", r.Message)
	}
}

func TestNudgeUnconfirmedCheckWarnsOnDiagnosticFile(t *testing.T) {
	cityRoot := t.TempDir()
	sessionDir := filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), "gc-worker-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "nudge-unconfirmed.log"), []byte("unconfirmed"), 0o644); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}

	c := NewNudgeUnconfirmedCheck()
	r := c.Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", r.Status, r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "gc-worker-1") || !strings.Contains(r.Details[0], "nudge-unconfirmed.log") {
		t.Fatalf("expected details to name session and file, got %v", r.Details)
	}
}

func TestNudgeUnconfirmedCheckWarnsOnStartupDiagnosticFile(t *testing.T) {
	cityRoot := t.TempDir()
	sessionDir := filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), "gc-worker-2")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "startup-nudge-unconfirmed.log"), []byte("unconfirmed"), 0o644); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}

	c := NewNudgeUnconfirmedCheck()
	r := c.Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", r.Status, r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "startup-nudge-unconfirmed.log") {
		t.Fatalf("expected details to name startup diagnostic file, got %v", r.Details)
	}
}

func TestNudgeUnconfirmedCheckCanFixIsFalse(t *testing.T) {
	c := NewNudgeUnconfirmedCheck()
	if c.CanFix() {
		t.Fatal("expected CanFix to be false")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("expected Fix to be a no-op, got %v", err)
	}
}
