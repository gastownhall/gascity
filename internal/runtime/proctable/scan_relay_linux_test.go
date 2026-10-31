//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeFakeCmdline writes pid's NUL-separated argv, as /proc does.
func writeFakeCmdline(t *testing.T, root string, pid int, argv ...string) {
	t.Helper()
	path := filepath.Join(root, strconv.Itoa(pid), "cmdline")
	if err := os.WriteFile(path, []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}
}

// buildFakeRelay builds a program-status relay process (pid, parent ppid)
// running /bin/sh -c in session env.
func buildFakeRelay(t *testing.T, root string, pid, ppid int, env map[string]string) {
	t.Helper()
	buildFakeProcUnder(t, root, pid, ppid, ProgramStatusCompanionName, env)
	writeFakeCmdline(t, root, pid, "/opt/gc/bin/"+ProgramStatusCompanionName, ProgramStatusRelayVerb, "--", "/bin/sh", "-c", "claude")
}

// The relay runs its program in a session and process group of its own, so
// a kill of the relay's group never reaches the program. The scan looks
// through the relay: the program is the root, attributed to the relay's
// tmux server exactly as the relay would have been.
func TestScanWithRootLooksThroughProgramStatusRelay(t *testing.T) {
	root := t.TempDir()
	const sessionID = "ga-relayed"
	env := map[string]string{"GC_SESSION_ID": sessionID}
	buildFakeProcUnder(t, root, 400, 1, "tmux: server", env)
	buildFakeRelay(t, root, 401, 400, env)
	buildFakeProcUnder(t, root, 402, 401, "claude", env)
	writeFakeCmdline(t, root, 402, "/bin/sh", "-c", "claude")
	buildFakeProcUnder(t, root, 403, 402, "node", env) // the program's own child: inside its envelope

	got, err := scanWithRoot(root, sessionID)
	if err != nil {
		t.Fatalf("scanWithRoot: %v", err)
	}
	if len(got) != 1 || got[0].PID != 402 {
		t.Fatalf("scanWithRoot = %+v, want only the relay's program pid 402", got)
	}
	if got[0].PPID != 400 || !got[0].ParentIsProviderInfrastructure {
		t.Fatalf("relayed root PPID=%d ParentIsProviderInfrastructure=%v, want the relay's tmux server 400 and true",
			got[0].PPID, got[0].ParentIsProviderInfrastructure)
	}

	SetScanRootForTesting(t, root)
	if IsScanRoot(401) {
		t.Error("IsScanRoot classified the relay as an agent root")
	}
	if !IsScanRoot(402) {
		t.Error("IsScanRoot must report the relay's program as the root")
	}
}

// An orphaned relay (its tmux server gone) leaves its program as the orphan
// the sweep must find: the program reads as reparented to init.
func TestOrphanCandidateScanLooksThroughOrphanedRelay(t *testing.T) {
	root := t.TempDir()
	writeFakeBootTime(t, root, time.Unix(1700000000, 0))
	env := map[string]string{"GC_SESSION_ID": "ga-orphan-relay"}
	buildFakeRelay(t, root, 501, 1, env)
	buildFakeProcUnder(t, root, 502, 501, "claude", env)
	// A relay still under its tmux server is no orphan.
	buildFakeProcUnder(t, root, 600, 1, "tmux: server", nil)
	buildFakeRelay(t, root, 601, 600, map[string]string{"GC_SESSION_ID": "ga-live-relay"})
	buildFakeProcUnder(t, root, 602, 601, "claude", map[string]string{"GC_SESSION_ID": "ga-live-relay"})

	got, err := scanOrphanCandidatesWithRoot(root, 0)
	if err != nil {
		t.Fatalf("scanOrphanCandidatesWithRoot: %v", err)
	}
	if len(got) != 1 || got[0].PID != 502 || got[0].PPID != 1 || got[0].ParentIsProviderInfrastructure {
		t.Fatalf("orphan candidates %+v, want only the orphaned relay's program 502 reading as reparented to init", got)
	}
}
