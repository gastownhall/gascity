package main

import (
	"bytes"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// moveUnderFakeTmuxServer rewrites pid's parent in the fake procfs root to a
// fake "tmux: server" (serverPID, adopted by init) carrying env, as a server
// an agent founded in its box does.
func moveUnderFakeTmuxServer(t *testing.T, root string, pid, serverPID int, env []string) {
	t.Helper()
	statPath := filepath.Join(root, strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		t.Fatal(err)
	}
	stat := string(data)
	end := strings.LastIndexByte(stat, ')')
	fields := strings.Fields(stat[end+1:])
	fields[1] = strconv.Itoa(serverPID)
	if err := os.WriteFile(statPath, []byte(stat[:end+1]+" "+strings.Join(fields, " ")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, strconv.Itoa(serverPID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uid := strconv.Itoa(os.Geteuid())
	for name, content := range map[string]string{
		"environ": strings.Join(env, "\x00") + "\x00",
		"stat":    strconv.Itoa(serverPID) + " (tmux: server) S 1" + strings.Repeat(" 0", 17) + " 100" + strings.Repeat(" 0", 29),
		"comm":    "tmux: server\n",
		"cmdline": "tmux\x00",
		"status":  "Name:\ttmux: server\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Kills a drain-ack escalation that force-terminates a pane of a tmux server
// an agent founded inside a host-sharing exec box: the server carries the
// session's GC_SESSION_ID, so the shared scan reads the pane as the
// session's root, tracked under the box's name with a tmux parent, which is
// exactly the shape the escalation's positive fence accepts for the tmux
// runtime. The exec scanner never reports a process under a tmux server, so
// the escalation selects nothing of an exec pack, while the same root as the
// tmux runtime's scan reports it is still a target (its rule is unchanged).
func TestDrainAckEscalationNeverTargetsInBoxTmuxPaneOfExecPack(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("reads /proc snapshots, which only linux has")
	}
	cityPath := t.TempDir()
	sp := sessionexec.NewSeamBackedForCity(sweepGoldenPack(t, true), "drain-city", cityPath)
	pane := startSweepLeftover(t, sp, "seat", "gc-seat", cityPath, false)
	root := runtimetest.SnapshotProcRoot(t, pane.PID)
	const serverPID = 4194000
	moveUnderFakeTmuxServer(t, root, pane.PID, serverPID, []string{"GC_SESSION_ID=gc-seat", "GC_CITY_PATH=" + cityPath})
	proctable.SetScanRootForTesting(t, root)

	scanner, ok := runtime.AsProcessTableScanner(sp)
	if !ok {
		t.Fatal("the host-sharing golden pack has no process-table scanner")
	}
	found, err := scanner.FindRuntimesBySessionID("gc-seat")
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID: %v", err)
	}
	var stderr bytes.Buffer
	if targets, _ := drainAckForceTerminationTargets(found, "gc-seat", cityPath, "seat", 0, &stderr); len(targets) != 0 {
		t.Fatalf("escalation targets %+v of an exec pack, want none: the pane under the in-box tmux server is not the seat's runtime; stderr=%q", targets, stderr.String())
	}

	// Control: the tmux runtime's scan reports the same pane as a tmux-parented
	// root, and once its provider binds it to the seat, the escalation's rule
	// still targets it.
	tmuxFound, err := proctable.ScanBySessionID("gc-seat")
	if err != nil || len(tmuxFound) != 1 || tmuxFound[0].PID != pane.PID || !tmuxFound[0].ParentIsProviderInfrastructure {
		t.Fatalf("ScanBySessionID = %+v, %v; want the pane pid %d with a tmux parent", tmuxFound, err, pane.PID)
	}
	tmuxFound[0].IsTracked, tmuxFound[0].ProviderName = true, "seat"
	if targets, _ := drainAckForceTerminationTargets(tmuxFound, "gc-seat", cityPath, "seat", 0, &stderr); len(targets) != 1 {
		t.Fatalf("escalation targets %+v of the tmux runtime's pane, want it: the tmux rule is unchanged; stderr=%q", targets, stderr.String())
	}
}
