//go:build unix

package pidutil

// These exercise the ps fallback and stage their conditions by shadowing ps on
// PATH. Windows never consults ps -- GetProcessTimes answers directly and
// always -- so the stub would change nothing and the 'unreadable identity'
// case cannot be staged this way there.
import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestPSStartTimeReturnsIdentity covers the new fallback's success path.
// ps -o lstart= works on linux too, so this runs on every platform — without
// it, no CI job ever executes a successful psStartTime.
func TestPSStartTimeReturnsIdentity(t *testing.T) {
	got, err := psStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("psStartTime(self) on %s: %v", runtime.GOOS, err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatalf("psStartTime(self) on %s returned an empty identity", runtime.GOOS)
	}
}

// TestAliveWithStartTime_UnreadableIdentityKeepsAliveAnswer pins the deliberately
// CONSERVATIVE direction, which is the opposite of the reaper's. Here a missing
// signal must not invent a death: reporting a live process dead would let a
// caller start a second copy alongside it. So an unreadable identity keeps the
// Alive answer, exactly as the pre-existing doc comment promises.
func TestAliveWithStartTime_UnreadableIdentityKeepsAliveAnswer(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("on linux /proc answers directly, so a ps stub cannot make the identity unreadable")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	if !AliveWithStartTime(os.Getpid(), "some-captured-identity") {
		t.Fatal("AliveWithStartTime = false when the identity is unreadable; a live process must not be reported dead")
	}
}

// TestPSStartTimeIsBounded mirrors the other ps probes in this package: callers
// sit in a post-SIGKILL reap loop, so a hung ps must not stall them.
func TestPSStartTimeIsBounded(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	t.Setenv("GC_PIDUTIL_PS_TIMEOUT", "1s")

	start := time.Now()
	_, _ = psStartTime(os.Getpid())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("psStartTime took %s, want a bounded timeout", elapsed)
	}
}
