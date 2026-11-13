package main

import (
	"bytes"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

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
	writeFakeProcEntry(t, root, serverPID, 1, "tmux: server", []string{"GC_SESSION_ID=gc-seat", "GC_CITY_PATH=" + cityPath})
	runtimetest.SetSnapshotParent(t, root, pane.PID, serverPID)
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

// Pins the stated exec-pack outcome of the escalation: the wedged seat's own
// box program, tracked under the seat's name while the pack lists its box,
// is never a force-termination target, whether its parent is the pack's
// holder or init, since the escalation targets only a root whose parent is
// tmux infrastructure. The escalation therefore reports nothing attributable
// for an exec seat, and the provider's stop, already tried, is all it gets.
func TestDrainAckEscalationNeverTargetsAnExecSeatsOwnProgram(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("reads /proc snapshots, which only linux has")
	}
	cityPath := t.TempDir()
	sp := sessionexec.NewSeamBackedForCity(sweepGoldenPack(t, true), "drain-city", cityPath)
	program := startSweepLeftover(t, sp, "seat", "gc-seat", cityPath, false)
	scanner, ok := runtime.AsProcessTableScanner(sp)
	if !ok {
		t.Fatal("the host-sharing golden pack has no process-table scanner")
	}
	const holderPID = 4194100
	for _, tc := range []struct {
		name   string
		parent int
	}{
		{"under the pack's holder", holderPID},
		{"adopted by init", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := runtimetest.SnapshotProcRoot(t, program.PID)
			writeFakeProcEntry(t, root, holderPID, 1, "holder", []string{"PATH=/usr/bin"})
			runtimetest.SetSnapshotParent(t, root, program.PID, tc.parent)
			proctable.SetScanRootForTesting(t, root)
			found, err := scanner.FindRuntimesBySessionID("gc-seat")
			if err != nil || len(found) != 1 || found[0].PID != program.PID || !found[0].IsTracked || found[0].ProviderName != "seat" {
				t.Fatalf("FindRuntimesBySessionID = %+v, %v; want the seat's program pid %d tracked as seat, or the case proves nothing", found, err, program.PID)
			}
			var stderr bytes.Buffer
			if targets, _ := drainAckForceTerminationTargets(found, "gc-seat", cityPath, "seat", 0, &stderr); len(targets) != 0 {
				t.Fatalf("escalation targets %+v, want none: an exec seat's own program is never force-terminated by the escalation; stderr=%q", targets, stderr.String())
			}
		})
	}
}

// boxTmuxGoldenPack is sweepGoldenPack's host-filesystem variant declaring
// proc.exec too: a pack whose agent runs in its box's tmux session main. Its
// `exec` still runs nothing, so no test drives a host tmux.
func boxTmuxGoldenPack(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.HostFSBodies()
	bodies["exec"] = `cat >/dev/null`
	bodies["protocol"] = `echo '{"version":0,"capabilities":["observe.v1","report-attachment","report-activity","conformance-faults","city-scoped","server-lifecycle","fs.host","proc.exec"],"locality":"local"}'`
	script := filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// The other half of the exec rule: a pack whose agent runs in its box's
// tmux session main (proc.exec without carrier.ops) is scanned as the tmux
// runtime is, so a wedged seat's agent, a pane of the box's tmux server
// carrying the session's GC_SESSION_ID and tracked under the seat's name
// while the pack lists the box, is the escalation's target, as the tmux
// runtime's pane is.
func TestDrainAckEscalationTargetsTheAgentPaneOfAnExecPackRunningItInTmux(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("reads /proc snapshots, which only linux has")
	}
	cityPath := t.TempDir()
	sp := sessionexec.NewSeamBackedForCity(boxTmuxGoldenPack(t), "drain-city", cityPath)
	agent := startSweepLeftover(t, sp, "seat", "gc-seat", cityPath, false)
	root := runtimetest.SnapshotProcRoot(t, agent.PID)
	const serverPID = 4194000
	writeFakeProcEntry(t, root, serverPID, 1, "tmux: server", []string{"GC_SESSION_ID=gc-seat", "GC_CITY_PATH=" + cityPath})
	runtimetest.SetSnapshotParent(t, root, agent.PID, serverPID)
	proctable.SetScanRootForTesting(t, root)

	scanner, ok := runtime.AsProcessTableScanner(sp)
	if !ok {
		t.Fatal("the host-sharing golden pack has no process-table scanner")
	}
	found, err := scanner.FindRuntimesBySessionID("gc-seat")
	if err != nil || len(found) != 1 || found[0].PID != agent.PID || !found[0].IsTracked || found[0].ProviderName != "seat" || !found[0].ParentIsProviderInfrastructure {
		t.Fatalf("FindRuntimesBySessionID = %+v, %v; want the seat's agent pid %d, tracked as seat with a tmux parent", found, err, agent.PID)
	}
	var stderr bytes.Buffer
	if targets, _ := drainAckForceTerminationTargets(found, "gc-seat", cityPath, "seat", 0, &stderr); len(targets) != 1 || targets[0].PID != agent.PID {
		t.Fatalf("escalation targets %+v, want the seat's agent pane %d; stderr=%q", targets, agent.PID, stderr.String())
	}
}
