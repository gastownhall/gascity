//go:build windows

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// Windows cannot read another process's argv (pidutil.Cmdline reports
// ErrUnsupported rather than reading the PEB across a process boundary), so the
// argv-discrimination tests in submit_poller_identity_unix_test.go have nothing
// to compare here.
//
// What Windows must still guarantee is the half of the contract that decides
// whether a duplicate singleton gets started: a live recorded PID that cannot
// be identified is an error, never a quiet "no poller running". These tests pin
// that, so the gap left by the missing argv read stays visible instead of
// passing as a clean negative.

func TestExistingSessionSubmitPollerPIDReportsUnidentifiableLivePID(t *testing.T) {
	cityPath := t.TempDir()
	pidPath := sessionSubmitPollerPIDPath(cityPath, "s-test", "session-id")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	running, err := existingSessionSubmitPollerPID(pidPath, cityPath, "s-test", "session-id")
	if running {
		t.Fatal("existingSessionSubmitPollerPID = true for a PID whose argv cannot be read")
	}
	if !errors.Is(err, pidutil.ErrUnsupported) {
		t.Fatalf("existingSessionSubmitPollerPID error = %v, want ErrUnsupported; a live but unidentifiable PID must not read as a clean negative", err)
	}
}

// TestEnsureSessionSubmitPollerRefusesWhenRecordedPIDIsUnidentifiable is the
// consequence that matters: the gate must refuse rather than overwrite the
// pidfile and run a second poller delivering the same nudges.
func TestEnsureSessionSubmitPollerRefusesWhenRecordedPIDIsUnidentifiable(t *testing.T) {
	cityPath := t.TempDir()
	pidPath := sessionSubmitPollerPIDPath(cityPath, "s-test", "agent")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	recorded := fmt.Sprintf("%d\n", os.Getpid())
	if err := os.WriteFile(pidPath, []byte(recorded), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := ensureSessionSubmitPoller(cityPath, "agent", "s-test")
	if err == nil {
		t.Fatal("ensureSessionSubmitPoller = nil with an unidentifiable live PID recorded; a duplicate poller may have started")
	}
	if !strings.Contains(err.Error(), "checking for an existing nudge poller") {
		t.Fatalf("ensureSessionSubmitPoller error = %v, want the existing-poller check to block the spawn", err)
	}

	current, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatalf("ReadFile(pid): %v", readErr)
	}
	if string(current) != recorded {
		t.Fatalf("poller pid file = %q, want %q unchanged; the refused spawn must not claim the lease", string(current), recorded)
	}
}

// TestExistingSessionSubmitPollerPIDOnDeadPIDIsACleanNegative bounds the rule
// above: once the process is genuinely gone there is nothing to identify, so
// the answer is a plain negative and a replacement may start.
func TestExistingSessionSubmitPollerPIDOnDeadPIDIsACleanNegative(t *testing.T) {
	cityPath := t.TempDir()
	pidPath := sessionSubmitPollerPIDPath(cityPath, "s-test", "session-id")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// A PID that cannot be live: process IDs are positive and this one is
	// reserved, so no identity read is attempted at all.
	if err := os.WriteFile(pidPath, []byte("999999999\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	running, err := existingSessionSubmitPollerPID(pidPath, cityPath, "s-test", "session-id")
	if err != nil {
		t.Fatalf("existingSessionSubmitPollerPID: %v", err)
	}
	if running {
		t.Fatal("existingSessionSubmitPollerPID = true for a PID that is not alive")
	}
}
