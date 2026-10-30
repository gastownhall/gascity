//go:build acceptance_a

// Doctor must never start a server. On bd 1.3.1 `bd backup status` starts a
// stopped proxied scope's proxy and Dolt child (it dials the store to size the
// database), and a gc-owned scope's zero idle timeout keeps them up, so the
// proxied-backup-coverage check has to leave a stopped city alone. The positive
// control proves the process scan sees exactly what bd would start.
//
// The check is run in-process against the real city rather than through the
// whole `gc doctor`: other doctor checks read the store through bd, and on a
// proxied scope any bd read restarts the proxy (BEADS_DOLT_AUTO_START is inert
// there, tracked separately), which would hide what this check does. Needs
// GC_ACCEPTANCE_BD_BIN pointing at a bd that supports proxied backup (beads
// hotfix/1.3.1+); a bd that refuses it skips.
package acceptance_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestDoctorProxiedBackupCoverageLeavesAStoppedCityStopped(t *testing.T) {
	bdPath, doltPath := helpers.RequireTopologyTooling(t)
	// Skip before building a city when bd predates proxied backup (beads
	// hotfix/1.3.1; its release candidates count): the positive control needs
	// `bd backup status` to be the thing that starts a stopped store.
	helpers.RequireBDAtLeast(t, bdPath, "v1.3.1-0", "proxied `bd backup`")
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
	stopCity := func(when string) {
		t.Helper()
		if out, err := run.Stop(); err != nil {
			t.Fatalf("gc stop %s: %v\n%s", when, err, out)
		}
		if leaked := helpers.WaitForNoDoltProcesses(t, run.Root, 30*time.Second); len(leaked) != 0 {
			t.Fatalf("processes survived gc stop %s:\n%s", when, strings.Join(leaked, "\n"))
		}
	}
	stopCity("after init")

	// Positive control: bd itself wakes the stopped store.
	if out, err := run.GC("bd", "backup", "status", "--json"); err != nil {
		if strings.Contains(out, "proxy.backup.unsupported") {
			t.Skipf("bd at %s refuses proxied backup; the control needs one that supports it", bdPath)
		}
		t.Fatalf("control gc bd backup status: %v\n%s", err, out)
	}
	if started := helpers.DoltProcessesUnder(t, run.Root); len(started) == 0 {
		t.Fatal("control: bd backup status started nothing, so the scan cannot prove doctor started nothing")
	}
	stopCity("after the control")

	check := doctor.NewProxiedBackupCoverageCheckForConfig(run.City.Dir, nil, errors.New("scopes from disk"),
		func(string) string { return bdPath })
	if check == nil {
		t.Fatal("no proxied-backup-coverage check for a proxied city")
	}
	result := check.Run(&doctor.CheckContext{CityPath: run.City.Dir})
	if !strings.Contains(result.Message, "not checked: store not running") {
		t.Errorf("proxied-backup-coverage on a stopped city = %v %q, want not checked", result.Status, result.Message)
	}
	if started := helpers.DoltProcessesUnder(t, run.Root); len(started) != 0 {
		t.Fatalf("proxied-backup-coverage started %d process(es) for a stopped city:\n%s", len(started), strings.Join(started, "\n"))
	}
}
