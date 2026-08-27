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

// These tests pin the ga-f7v2ft.194 adjudication: a process the kernel has
// already killed cannot be a living session runtime, so it owes the sweep no
// proof at all.
//
// The window-31 shape (measured on the supervisor fleet): 522 host-wide
// zombies, many of them the supervisor's own orphaned tmux servers. A real
// zombie keeps a readable /proc/<pid>/status (Uid still ours, so
// processOwnedByUID passes) and a readable /proc/<pid>/stat, but its environ
// answers EACCES even to its owner — verified on the incident host:
//
//	STAT:    808784 (python3) Z 808679 ...
//	STATUS:  State: Z (zombie) / Uid: 1000 1000 1000 1000
//	ENVIRON: [Errno 13] Permission denied
//
// so every one of them landed in the residue. Orphaned zombies re-parent to
// init, and BOTH ga-lp5w6 scope proofs decline an init-exiting chain (that is
// the orphan shape), so the population stayed in-scope-undecidable forever:
// the sweep never regained completeness, acked drains escalated and parked,
// and parked rows held their pool seats.
//
// The exclusion is narrow. State Z (zombie) and X/x (dead) are kernel-proven
// terminal: the address space is gone and no code will ever run under that pid
// again. Nothing else is excluded — an unreadable process in ANY live state
// still fails the sweep closed, which is what
// TestScanWithRootSinceUnreadableSudoChildProofFailsClosed (unmodified) holds.

// writeFakeProcessStatComm writes a /proc/<pid>/stat fixture with an explicit
// comm and process state, so tests can model zombies and adversarial comm
// strings. It mirrors writeFakeProcessStat's field layout otherwise.
func writeFakeProcessStatComm(t *testing.T, dir string, pid, ppid int, startTicks uint64, comm, state string) {
	t.Helper()
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = state
	fields[1] = strconv.Itoa(ppid)
	fields[19] = strconv.FormatUint(startTicks, 10)
	stat := strconv.Itoa(pid) + " (" + comm + ") " + strings.Join(fields, " ")
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
}

// writeFakeZombie builds the incident's exact shape: an owned, orphaned
// (ppid 1), post-incarnation process whose environ is unreadable and whose
// stat reports state Z.
func writeFakeZombie(t *testing.T, root string, pid int, startTicks uint64) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create zombie fixture: %v", err)
	}
	writeUnreadableEnviron(t, dir)
	writeFakeProcessUID(t, dir, os.Geteuid())
	writeFakeProcessStatComm(t, dir, pid, 1, startTicks, "tmux: server", "Z")
	writeFakeProcessCgroup(t, dir, "/user.slice/user-1000.slice/user@1000.service/tmux-spawn-9866a319.scope")
}

// TestScanWithRootSinceExcludesKernelDeadProcessesFromCompleteness is the
// stall's resolution shape: the session's runtime is gone and the only owned
// processes left are zombies. The sweep must read COMPLETE so the drain-ack
// stop can finalize. The control proves the fixture is genuinely undecidable
// by every other axis: flip one zombie to a live state and the identical tree
// must fail closed again.
func TestScanWithRootSinceExcludesKernelDeadProcessesFromCompleteness(t *testing.T) {
	root := t.TempDir()
	boot := time.Unix(1_700_000_000, 0).UTC()
	writeFakeBootTime(t, root, boot)
	incarnation := boot.Add(10 * time.Second)

	// The session's own runtime is dead: no /proc entry survives it.
	for pid, startTicks := range map[int]uint64{910: 2000, 911: 2100, 912: 2200} {
		writeFakeZombie(t, root, pid, startTicks)
	}

	got, err := scanWithRootSince(root, "ga-target", incarnation)
	if err != nil {
		t.Fatalf("kernel-dead processes made the exact scan incomplete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("exact scan returned %d runtimes over a dead runtime, want 0", len(got))
	}

	// Control: the same unreadable, orphaned, post-incarnation process in a
	// LIVE state is undecidable and must still fail the session closed.
	// Without this the fixture would prove nothing about state Z.
	writeFakeProcessStatComm(t, filepath.Join(root, "912"), 912, 1, 2200, "tmux: server", "S")
	if _, err := scanWithRootSince(root, "ga-target", incarnation); err == nil {
		t.Fatal("a live unreadable orphan did not make the scan incomplete; the zombie fixture proves nothing")
	}
}

// TestScanResidueSummaryCountsExcludedKernelDeadProcesses pins the visibility
// half: excluded zombies never spam a line each, but a sweep that still has
// residue reports how many it dropped, so an operator reading one summary line
// can tell a quiet host from a host carrying 500 zombies.
func TestScanResidueSummaryCountsExcludedKernelDeadProcesses(t *testing.T) {
	root := t.TempDir()
	boot := time.Unix(1_700_000_000, 0).UTC()
	writeFakeBootTime(t, root, boot)

	writeFakeZombie(t, root, 920, 2000)
	writeFakeZombie(t, root, 921, 2100)
	for _, pid := range []int{922, 923} {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create undecidable fixture: %v", err)
		}
		writeUnreadableEnviron(t, dir)
		writeFakeProcessUID(t, dir, os.Geteuid())
		writeFakeProcessStatComm(t, dir, pid, 1, 2200, "agent", "S")
	}

	_, err := scanWithRootSince(root, "ga-target", boot.Add(10*time.Second))
	if err == nil {
		t.Fatal("live unreadable processes did not make the scan incomplete")
	}
	msg := err.Error()
	if strings.Contains(msg, "\n") {
		t.Fatalf("residue summary spans multiple lines, want one:\n%s", msg)
	}
	if !strings.Contains(msg, "2 processes could not be inspected") {
		t.Fatalf("residue summary %q does not count the 2 undecidable processes", msg)
	}
	if !strings.Contains(msg, "2 kernel-dead") {
		t.Fatalf("residue summary %q does not report the excluded zombies", msg)
	}
	// Scrub the fixture root first: t.TempDir() suffixes the directory with
	// random digits that can themselves spell a zombie's pid (CI drew
	// ...2784239216, whose "9216" contains "921").
	if scrubbed := strings.ReplaceAll(msg, root, "<root>"); strings.Contains(scrubbed, "920") || strings.Contains(scrubbed, "921") {
		t.Fatalf("residue summary %q names an excluded zombie; exclusions are counted, not itemized", msg)
	}
}

// TestReadProcessStatParsesStateThroughAdversarialComm pins the parse itself.
// comm is arbitrary attacker- or user-controlled bytes (a process can rename
// itself), and it can carry spaces, parens, and text shaped exactly like the
// fields that follow it. The only correct split is at the LAST ')'.
func TestReadProcessStatParsesStateThroughAdversarialComm(t *testing.T) {
	tests := []struct {
		name string
		comm string
	}{
		{name: "plain", comm: "tmux"},
		{name: "spaces", comm: "tmux: server"},
		{name: "open paren", comm: "we(ird"},
		{name: "close paren", comm: "weird)name"},
		{name: "balanced parens", comm: "(nested)"},
		{name: "mimics the stat tail", comm: "x) S 1 2 3"},
		{name: "mimics a whole record", comm: "9 (a) R 7 0 0"},
		{name: "paren storm", comm: "))((  )("},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			const (
				pid        = 931
				ppid       = 777
				startTicks = 4242
			)
			dir := filepath.Join(root, strconv.Itoa(pid))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			writeFakeProcessStatComm(t, dir, pid, ppid, startTicks, tt.comm, "Z")

			stat, exists, err := readProcessStat(root, pid)
			if err != nil || !exists {
				t.Fatalf("readProcessStat(comm=%q) = (exists=%t, %v), want a parsed record", tt.comm, exists, err)
			}
			if stat.PID != pid || stat.PPID != ppid || stat.StartTicks != startTicks {
				t.Fatalf("readProcessStat(comm=%q) = %+v, want pid/ppid/start %d/%d/%d", tt.comm, stat, pid, ppid, startTicks)
			}
			if !processStateIsKernelDead(stat.State) {
				t.Fatalf("readProcessStat(comm=%q) state = %q, want the zombie state parsed from after the last ')'", tt.comm, stat.State)
			}

			// Control: the identical record in a live state must not read as
			// kernel-dead, or the parse is picking up something other than the
			// state field.
			writeFakeProcessStatComm(t, dir, pid, ppid, startTicks, tt.comm, "R")
			live, _, err := readProcessStat(root, pid)
			if err != nil {
				t.Fatalf("readProcessStat(comm=%q, live): %v", tt.comm, err)
			}
			if processStateIsKernelDead(live.State) {
				t.Fatalf("readProcessStat(comm=%q) read a running process as kernel-dead (state %q)", tt.comm, live.State)
			}
		})
	}
}

// TestProcessStateIsKernelDeadCoversTerminalStatesOnly pins the state
// vocabulary: Z (zombie) and X/x (dead) are terminal; every live state — and
// an unparsed empty state — is not.
func TestProcessStateIsKernelDeadCoversTerminalStatesOnly(t *testing.T) {
	for _, state := range []string{"Z", "X", "x"} {
		if !processStateIsKernelDead(state) {
			t.Errorf("state %q not treated as kernel-dead", state)
		}
	}
	for _, state := range []string{"R", "S", "D", "T", "t", "I", "W", "K", "P", ""} {
		if processStateIsKernelDead(state) {
			t.Errorf("live state %q treated as kernel-dead", state)
		}
	}
}

// TestScanWithRootSinceInScopeWalksThroughAZombieLink pins the other half of
// the rule: excluding a zombie removes only its own candidacy as a living
// runtime. Its ppid is still valid, so it remains a usable LINK in another
// process's parent chain — here the pane-scope proof reaches the live spawner
// by walking THROUGH a dead intermediate.
func TestScanWithRootSinceInScopeWalksThroughAZombieLink(t *testing.T) {
	root := t.TempDir()
	boot := time.Unix(1_700_000_000, 0).UTC()
	writeFakeBootTime(t, root, boot)
	incarnation := boot.Add(10 * time.Second)
	scope := "/user.slice/user-1000.slice/session-1.scope/tmux-spawn-feed1.scope"

	const (
		spawnerPID  = 940
		paneRootPID = 941
		leafPID     = 942
	)

	spawnerDir := filepath.Join(root, strconv.Itoa(spawnerPID))
	if err := os.MkdirAll(spawnerDir, 0o755); err != nil {
		t.Fatalf("create spawner fixture: %v", err)
	}
	writeFakeProcessUID(t, spawnerDir, os.Geteuid())
	writeFakeProcessStatComm(t, spawnerDir, spawnerPID, 1, 500, "tmux: server", "S")
	writeFakeProcessCgroup(t, spawnerDir, scanScopeTestSpawnerCgroup)
	writeFakeProcessEnviron(t, spawnerDir, map[string]string{"GC_SESSION_ID": "ga-other"})

	// The intermediate pane root died and has not been reaped.
	paneRootDir := filepath.Join(root, strconv.Itoa(paneRootPID))
	if err := os.MkdirAll(paneRootDir, 0o755); err != nil {
		t.Fatalf("create pane-root fixture: %v", err)
	}
	writeUnreadableEnviron(t, paneRootDir)
	writeFakeProcessUID(t, paneRootDir, os.Geteuid())
	writeFakeProcessStatComm(t, paneRootDir, paneRootPID, spawnerPID, 2000, "bash", "Z")
	writeFakeProcessCgroup(t, paneRootDir, scope)

	leafDir := filepath.Join(root, strconv.Itoa(leafPID))
	if err := os.MkdirAll(leafDir, 0o755); err != nil {
		t.Fatalf("create leaf fixture: %v", err)
	}
	writeUnreadableEnviron(t, leafDir)
	writeFakeProcessUIDs(t, leafDir, os.Geteuid(), 0)
	writeFakeProcessStatComm(t, leafDir, leafPID, paneRootPID, 2100, "sudo", "S")
	writeFakeProcessCgroup(t, leafDir, scope)

	got, err := scanWithRootSinceInScope(root, "ga-target", incarnation, SessionScope{
		TmuxSessionProvenAbsent: true,
	})
	if err != nil {
		t.Fatalf("a zombie link broke the pane-scope chain walk: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("licensed exact scan returned %d runtimes, want 0", len(got))
	}
}

// TestScanWithRootSinceInScopeRefusesAZombieSpawnersEnviron is the fail-closed
// edge the exclusion must NOT open. Both scope proofs conclude from a
// spawner's environ that it does not carry the target session ID — but a dead
// process has no address space, so its environ reads empty (or EACCES) no
// matter what it once held. An empty read from a corpse is absence of
// evidence, never evidence of absence: the proof must decline.
func TestScanWithRootSinceInScopeRefusesAZombieSpawnersEnviron(t *testing.T) {
	scope := "/user.slice/user-1000.slice/session-1.scope/tmux-spawn-feed1.scope"
	boot := time.Unix(1_700_000_000, 0).UTC()
	incarnation := boot.Add(10 * time.Second)

	build := func(t *testing.T, spawnerState string) string {
		t.Helper()
		root := t.TempDir()
		writeFakeBootTime(t, root, boot)

		const (
			spawnerPID = 950
			leafPID    = 951
		)
		spawnerDir := filepath.Join(root, strconv.Itoa(spawnerPID))
		if err := os.MkdirAll(spawnerDir, 0o755); err != nil {
			t.Fatalf("create spawner fixture: %v", err)
		}
		writeFakeProcessUID(t, spawnerDir, os.Geteuid())
		writeFakeProcessStatComm(t, spawnerDir, spawnerPID, 1, 500, "tmux: server", spawnerState)
		writeFakeProcessCgroup(t, spawnerDir, scanScopeTestSpawnerCgroup)
		// A reaped process's environ reads empty — indistinguishable from a
		// live process that genuinely carries no session identity.
		writeFakeProcessEnviron(t, spawnerDir, map[string]string{})

		leafDir := filepath.Join(root, strconv.Itoa(leafPID))
		if err := os.MkdirAll(leafDir, 0o755); err != nil {
			t.Fatalf("create leaf fixture: %v", err)
		}
		writeUnreadableEnviron(t, leafDir)
		writeFakeProcessUIDs(t, leafDir, os.Geteuid(), 0)
		writeFakeProcessStatComm(t, leafDir, leafPID, spawnerPID, 2100, "sudo", "S")
		writeFakeProcessCgroup(t, leafDir, scope)
		return root
	}

	licensed := SessionScope{TmuxSessionProvenAbsent: true}
	if got, err := scanWithRootSinceInScope(build(t, "Z"), "ga-target", incarnation, licensed); err == nil {
		t.Fatalf("a dead spawner's empty environ proved the pane foreign = %v, want incomplete", got)
	}
	if _, err := scanWithRootSinceInScope(build(t, "S"), "ga-target", incarnation, licensed); err != nil {
		t.Fatalf("control: a LIVE spawner's readable ID-less environ must still prove the pane foreign: %v", err)
	}
}
