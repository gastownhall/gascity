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

	if got := ConsumePreviousExit(home, true); got != PreviousExitClean {
		t.Fatalf("ConsumePreviousExit = %q, want %q", got, PreviousExitClean)
	}
	if _, err := os.Stat(ShutdownMarkerPath(home)); !os.IsNotExist(err) {
		t.Fatalf("handoff token still present after consume (stat err = %v)", err)
	}

	// The token is single-use: a second start without a fresh clean
	// shutdown must not report clean again.
	if got := ConsumePreviousExit(home, true); got != PreviousExitCrash {
		t.Fatalf("second ConsumePreviousExit = %q, want %q", got, PreviousExitCrash)
	}
}

func TestConsumePreviousExitCrashWhenPriorInstanceLeftNoToken(t *testing.T) {
	home := t.TempDir()
	if got := ConsumePreviousExit(home, true); got != PreviousExitCrash {
		t.Fatalf("ConsumePreviousExit = %q, want %q", got, PreviousExitCrash)
	}
}

func TestConsumePreviousExitUnknownWithoutPriorInstanceEvidence(t *testing.T) {
	home := t.TempDir()
	if got := ConsumePreviousExit(home, false); got != PreviousExitUnknown {
		t.Fatalf("ConsumePreviousExit = %q, want %q", got, PreviousExitUnknown)
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
