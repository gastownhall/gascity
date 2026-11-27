//go:build unix

package pidutil

// Both depend on the ps fallback: one reads this process's own argv through
// it, the other stages a hung ps on PATH. Windows enumerates children from a
// Toolhelp snapshot and cannot read argv at all.
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCmdlineReturnsOwnArgv(t *testing.T) {
	argv, err := Cmdline(os.Getpid())
	if err != nil {
		t.Fatalf("Cmdline(%d): %v", os.Getpid(), err)
	}
	if len(argv) == 0 || !strings.Contains(filepath.Base(argv[0]), "pidutil") {
		t.Fatalf("Cmdline(%d) = %v, want test binary argv", os.Getpid(), argv)
	}
}

// TestChildPIDsReturnsErrorWhenPSHangs is a RED test for ga-gxmz9n's binding
// constraint: when enumeration cannot complete, ChildPIDs must report an
// error rather than silently returning an empty (falsely "no children")
// result — otherwise a leak-detection caller cannot tell "checked, found
// none" apart from "never actually checked". Mirrors
// TestPSReportsZombieReturnsWhenPSHangs's PATH-shadowing technique.
func TestChildPIDsReturnsErrorWhenPSHangs(t *testing.T) {
	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "ps")
	if err := os.WriteFile(psPath, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	t.Setenv("GC_PIDUTIL_PS_TIMEOUT", "1s")

	// Target a pid with no /proc entry so the fast path reports "cannot
	// answer" and the ps fallback — the path under test — actually runs.
	// A live pid would be answered from /proc and never reach ps.
	start := time.Now()
	pids, err := ChildPIDs(1 << 30)
	if err == nil {
		t.Fatalf("ChildPIDs with a hanging ps: got pids=%v err=nil, want a non-nil error", pids)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ChildPIDs took %s, want bounded timeout", elapsed)
	}
}
