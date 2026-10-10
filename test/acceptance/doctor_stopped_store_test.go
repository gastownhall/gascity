//go:build acceptance_a

// Doctor must never start a server. On a stopped bd-owned proxied city, any bd
// read of the store starts its proxy and Dolt child (BEADS_DOLT_AUTO_START does
// not apply on the proxied path), and a gc-owned scope's zero idle timeout keeps
// them up for good. Before the store gate a full `gc doctor` did exactly that:
// the session provider read the city store as the checks were built, the
// bead-store preflight then found it "reachable", and ~20 store-reading checks
// followed. This runs the real `gc doctor` and `gc doctor --fix` against a real
// stopped city with a rig and asserts no proxy or Dolt process appears.
package acceptance_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestDoctorLeavesAStoppedProxiedCityStopped(t *testing.T) {
	bdPath, doltPath := helpers.RequireTopologyTooling(t)
	var topo helpers.BeadsTopology
	for _, candidate := range helpers.BeadsTopologies() {
		if candidate.Name == "M1-proxied-local" {
			topo = candidate
		}
	}
	if topo.Name == "" {
		t.Fatal("no M1-proxied-local topology")
	}
	run := helpers.StartTopology(t, testEnv, topo, bdPath, doltPath)
	rigDir := run.RigWorkspace(t, "stoppedrig")
	if out, err := run.GC("rig", "add", rigDir); err != nil {
		t.Fatalf("gc rig add: %v\n%s", err, out)
	}
	if out, err := run.Stop(); err != nil {
		t.Fatalf("gc stop: %v\n%s", err, out)
	}
	if leaked := helpers.WaitForNoDoltProcesses(t, run.Root, 30*time.Second); len(leaked) != 0 {
		t.Fatalf("processes survived gc stop:\n%s", strings.Join(leaked, "\n"))
	}

	for _, args := range [][]string{
		{"doctor", "--json", "--check-timeout", topologyDoctorCheckTimeout},
		{"doctor", "--fix", "--json", "--check-timeout", topologyDoctorCheckTimeout},
	} {
		label := "gc " + strings.Join(args[:len(args)-3], " ")
		out, _ := run.City.GC(args...)
		var report doctorReport
		lastJSONLine(t, out, &report)
		notChecked := map[string]bool{}
		for _, r := range report.Results {
			if strings.Contains(r.Message, doctor.StoreNotRunningMessage) {
				notChecked[r.Name] = true
			}
		}
		for _, name := range []string{"agent-sessions", "beads-store", "custom-types:city", "custom-types:stoppedrig"} {
			if !notChecked[name] {
				t.Errorf("%s: %s is not reported as %q", label, name, doctor.StoreNotRunningMessage)
			}
		}
		// A leaked proxy stays up (idle timeout never), so a short window is
		// enough to see one that doctor started.
		if started := helpers.WaitForDoltProcesses(t, run.Root, 2*time.Second); len(started) != 0 {
			t.Fatalf("%s started %d process(es) on a stopped city:\n%s", label, len(started), strings.Join(started, "\n"))
		}
	}
}
