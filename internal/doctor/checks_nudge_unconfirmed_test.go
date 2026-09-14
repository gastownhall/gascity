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

// TestNudgeUnconfirmedCheckCountsSessionsNotArtifacts: a session holding both
// diagnostic files is one session with two artifacts. Counting details made
// the message report it as two sessions.
func TestNudgeUnconfirmedCheckCountsSessionsNotArtifacts(t *testing.T) {
	cityRoot := t.TempDir()
	sessionDir := filepath.Join(citylayout.SessionDiagnosticsDir(cityRoot), "gc-worker-3")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, filename := range []string{"nudge-unconfirmed.log", "startup-nudge-unconfirmed.log"} {
		if err := os.WriteFile(filepath.Join(sessionDir, filename), []byte("unconfirmed"), 0o644); err != nil {
			t.Fatalf("write %s: %v", filename, err)
		}
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", r.Status, r.Message)
	}
	if len(r.Details) != 2 {
		t.Fatalf("expected one detail per artifact, got %v", r.Details)
	}
	if !strings.Contains(r.Message, "1 session(s)") {
		t.Fatalf("message = %q, want it to name 1 session for one session holding two artifacts", r.Message)
	}
}

// TestNudgeUnconfirmedCheckReportsNonDirectoryEntries: a stray file directly
// under the sessions dir is malformed state, not clean state. It must be
// reported without suppressing the real diagnostics found alongside it.
func TestNudgeUnconfirmedCheckReportsNonDirectoryEntries(t *testing.T) {
	cityRoot := t.TempDir()
	sessionsDir := citylayout.SessionDiagnosticsDir(cityRoot)
	sessionDir := filepath.Join(sessionsDir, "gc-worker-4")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "nudge-unconfirmed.log"), []byte("unconfirmed"), 0o644); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "stray.txt"), []byte("not a session dir"), 0o644); err != nil {
		t.Fatalf("write stray entry: %v", err)
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", r.Status, r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "gc-worker-4") {
		t.Fatalf("a stray entry suppressed the real diagnostic; details = %v", r.Details)
	}
	if !strings.Contains(joined, "stray.txt") {
		t.Fatalf("expected details to name the non-directory entry, got %v", r.Details)
	}
}

// TestNudgeUnconfirmedCheckDoesNotReportCleanOnMalformedOnly: malformed state
// with no valid session dirs must not read as clean.
func TestNudgeUnconfirmedCheckDoesNotReportCleanOnMalformedOnly(t *testing.T) {
	cityRoot := t.TempDir()
	sessionsDir := citylayout.SessionDiagnosticsDir(cityRoot)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "stray.txt"), []byte("not a session dir"), 0o644); err != nil {
		t.Fatalf("write stray entry: %v", err)
	}

	r := NewNudgeUnconfirmedCheck().Run(&CheckContext{CityPath: cityRoot})
	if r.Status == StatusOK {
		t.Fatalf("malformed sessions dir reported clean: %s", r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "stray.txt") {
		t.Fatalf("expected details to name the non-directory entry, got %v", r.Details)
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
