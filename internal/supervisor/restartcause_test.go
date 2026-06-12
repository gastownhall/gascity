package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConsumePreviousExitCleanConsumesHandoffToken(t *testing.T) {
	home := t.TempDir()
	if err := WriteShutdownMarker(home); err != nil {
		t.Fatalf("WriteShutdownMarker: %v", err)
	}

	got, detail := ConsumePreviousExit(home, true)
	if got != PreviousExitClean || detail != nil {
		t.Fatalf("ConsumePreviousExit = %q, %v, want %q, nil", got, detail, PreviousExitClean)
	}
	if _, err := os.Stat(ShutdownMarkerPath(home)); !os.IsNotExist(err) {
		t.Fatalf("handoff token still present after consume (stat err = %v)", err)
	}

	// The token is single-use: a second start without a fresh clean
	// shutdown must not report clean again.
	if got, detail := ConsumePreviousExit(home, true); got != PreviousExitCrash || detail != nil {
		t.Fatalf("second ConsumePreviousExit = %q, %v, want %q, nil", got, detail, PreviousExitCrash)
	}
}

func TestConsumePreviousExitCrashWhenPriorInstanceLeftNoToken(t *testing.T) {
	home := t.TempDir()
	if got, detail := ConsumePreviousExit(home, true); got != PreviousExitCrash || detail != nil {
		t.Fatalf("ConsumePreviousExit = %q, %v, want %q, nil", got, detail, PreviousExitCrash)
	}
}

func TestConsumePreviousExitUnknownWithoutPriorInstanceEvidence(t *testing.T) {
	home := t.TempDir()
	if got, detail := ConsumePreviousExit(home, false); got != PreviousExitUnknown || detail != nil {
		t.Fatalf("ConsumePreviousExit = %q, %v, want %q, nil", got, detail, PreviousExitUnknown)
	}
}

func TestConsumePreviousExitSurfacesUnremovableTokenDetail(t *testing.T) {
	home := t.TempDir()
	// A non-empty directory at the token path makes os.Remove fail with
	// an error other than absence (ENOTEMPTY), modeling a token that
	// exists but cannot be removed.
	if err := os.MkdirAll(filepath.Join(ShutdownMarkerPath(home), "child"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, detail := ConsumePreviousExit(home, true)
	if got != PreviousExitUnknown {
		t.Fatalf("ConsumePreviousExit = %q, want %q", got, PreviousExitUnknown)
	}
	if detail == nil {
		t.Fatal("ConsumePreviousExit detail = nil, want removal error")
	}
}

func TestWriteShutdownMarkerCreatesHomeDir(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nested", ".gc")
	if err := WriteShutdownMarker(home); err != nil {
		t.Fatalf("WriteShutdownMarker: %v", err)
	}
	if _, err := os.Stat(ShutdownMarkerPath(home)); err != nil {
		t.Fatalf("stat handoff token: %v", err)
	}
}
