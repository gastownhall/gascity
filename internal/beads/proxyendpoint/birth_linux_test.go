//go:build linux

package proxyendpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCapturePlatformBirthAgreesWithProc pins the recompute against the two
// files it is derived from, using the test process itself as the subject.
//
// The subject has to be a real process: the whole value of the token is that it
// is byte-identical to one bd computed on this host, and a fixture-only test
// would pass while the real read returned a token no record could ever match.
func TestCapturePlatformBirthAgreesWithProc(t *testing.T) {
	pid := os.Getpid()
	got, err := capturePlatformBirth(pid)
	if err != nil {
		t.Skipf("this host cannot recompute a birth token: %v", err)
	}

	bootID, err := os.ReadFile(bootIDPath)
	if err != nil {
		t.Skipf("read boot id: %v", err)
	}
	stat, err := os.ReadFile(procStatPath(pid))
	if err != nil {
		t.Fatalf("read own proc stat: %v", err)
	}
	startTime, err := ParseProcStatStartTime(string(stat))
	if err != nil {
		t.Fatalf("parse own proc stat: %v", err)
	}
	if want := BirthToken(string(bootID), startTime); got != want {
		t.Fatalf("capturePlatformBirth = %q, want %q", got, want)
	}
	// Two reads of one live process must agree, or the token identifies nothing.
	again, err := capturePlatformBirth(pid)
	if err != nil || again != got {
		t.Fatalf("capturePlatformBirth is not stable: %q / %q (%v)", got, again, err)
	}
}

// TestCapturePlatformBirthUnavailableWhenTheBootIDIsNot pins that a host whose
// boot id cannot be read reports `unavailable` rather than a mismatch: the
// former keeps a live endpoint live on argv evidence, and the latter would
// retire it.
func TestCapturePlatformBirthUnavailableWhenTheBootIDIsNot(t *testing.T) {
	original := bootIDPath
	t.Cleanup(func() { bootIDPath = original })
	bootIDPath = filepath.Join(t.TempDir(), "absent-boot-id")

	_, err := capturePlatformBirth(os.Getpid())
	if !errors.Is(err, ErrBirthUnavailable) {
		t.Fatalf("capturePlatformBirth with no boot id = %v, want ErrBirthUnavailable", err)
	}
}

// TestCapturePlatformBirthOnADeadPID pins that a PID with no /proc entry is an
// ordinary read failure and NOT an unavailable host: the caller must not
// downgrade its evidence because one process went away.
func TestCapturePlatformBirthOnADeadPID(t *testing.T) {
	// PID 0 has no /proc/0 on Linux, so this needs no process of its own.
	_, err := capturePlatformBirth(0)
	if err == nil {
		t.Fatal("capturePlatformBirth(0) succeeded")
	}
	if errors.Is(err, ErrBirthUnavailable) {
		t.Fatalf("a missing /proc entry reported the host as unavailable: %v", err)
	}
}
