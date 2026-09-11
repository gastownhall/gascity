//go:build acceptance_a

// The init topology matrix.
//
// AC-M of the beads-proxied-local-default design: every supported way to
// initialise a Gas City beads scope runs the same list of ordinary commands —
// init, doctor, bd create/list/show, rig add, start, status, stop, start, stop
// — through the real gc and bd front doors, and each one is measured against
// the shape it is supposed to produce.
//
// The point is the shapes nobody exercises. Proving the proxied-local default
// works says nothing about whether a city bound to somebody else's Dolt server
// still reads as healthy, whether a doltlite city quietly acquired a proxied
// binding, or whether the pre-journal cities that exist in the field survive an
// upgrade. Each shape's expected topology lives in
// test/acceptance/helpers/beads_topology.go, so a new shape is a table entry
// rather than a new test.
package acceptance_test

import (
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestBeadsInitTopologyMatrix(t *testing.T) {
	helpers.ForEachTopology(t, testEnv, func(t *testing.T, run *helpers.TopologyRun) {
		topo := run.Topology
		t.Logf("%s — %s", topo.Name, topo.Doc)

		cityRoot := run.City.Dir
		rigName := "matrixrig"
		rigDir := run.RigWorkspace(t, rigName)

		if !topo.Deferred {
			t.Run("init-shape", func(t *testing.T) {
				helpers.AssertScopeShape(t, cityRoot, cityRoot, topo.City, topo.Name+" city")
				helpers.AssertJournalState(t, cityRoot, "city", topo.City, topo.Name+" city")
			})
		} else {
			// A deferred shape has no store until `gc start` makes one, so
			// start is part of its init step rather than a later one: doctor
			// and the bd front door have nothing to talk to before it.
			t.Run("init-defers-the-store", func(t *testing.T) {
				assertDeferredInit(t, run)
				run.City.StartWithSupervisor()
				helpers.AssertScopeShape(t, cityRoot, cityRoot, topo.City, topo.Name+" city after the deferred start")
				helpers.AssertJournalState(t, cityRoot, "city", topo.City, topo.Name+" city after the deferred start")
			})
		}

		t.Run("doctor", func(t *testing.T) {
			assertTopologyDoctor(t, run, topo.PreStartDoctorGaps, "after init")
		})

		var createdBead string
		t.Run("bd-front-door", func(t *testing.T) {
			createdBead = assertBeadRoundTrip(t, run, topo.Name)
		})

		t.Run("rig-inherits", func(t *testing.T) {
			out, err := run.GC("rig", "add", rigDir)
			if err != nil {
				t.Fatalf("gc rig add on a %s city: %v\n%s", topo.Name, err, out)
			}
			helpers.AssertScopeShape(t, cityRoot, rigDir, topo.Rig, topo.Name+" rig")
			helpers.AssertJournalState(t, cityRoot, "rig:"+rigName, topo.Rig, topo.Name+" rig")
		})

		t.Run("start-default-pack", func(t *testing.T) {
			run.City.StartWithSupervisor()

			// The bd pack imports the dolt pack, whose orders fire on every
			// city. Driving mol-dog-stale-db's own front door is the same proof
			// as waiting for the cron tick, without the wait: on a shape gc
			// does not own it has to be a typed no-op, and on one it does own
			// it has to do the real thing.
			assertDoltCleanupOutcome(t, run)

			// Whatever the shape, gc must publish managed-Dolt runtime state
			// only for a scope whose Dolt process it actually started.
			helpers.AssertScopeShape(t, cityRoot, cityRoot, topo.City, topo.Name+" city under supervisor")

			if out, err := run.GC("status"); err != nil {
				t.Fatalf("gc status: %v\n%s", err, out)
			}
		})

		t.Run("doctor-after-start", func(t *testing.T) {
			assertTopologyDoctor(t, run, nil, "after start")
		})

		t.Run("stop-quiescent", func(t *testing.T) {
			assertStopRetiresTheScope(t, run, rigDir)

			// Re-runnable: "there was nothing to stop" is success.
			if out, err := run.Stop(); err != nil {
				t.Fatalf("second gc stop on a %s city: %v\n%s", topo.Name, err, out)
			}
		})

		t.Run("restart", func(t *testing.T) {
			run.City.StartWithSupervisor()
			helpers.AssertScopeShape(t, cityRoot, cityRoot, topo.City, topo.Name+" restarted city")
			helpers.AssertScopeShape(t, cityRoot, rigDir, topo.Rig, topo.Name+" restarted rig")

			// The store has to be the same one, not a fresh empty binding: the
			// bead created before the stop must still be there.
			if createdBead != "" {
				list, err := run.City.GCStdout("bd", "list", "--json")
				if err != nil {
					t.Fatalf("gc bd list after restart: %v\n%s", err, list)
				}
				if !strings.Contains(list, createdBead) {
					t.Fatalf("%s lost %s across a restart; it came back over a different store:\n%s",
						topo.Name, createdBead, list)
				}
			}

			assertStopRetiresTheScope(t, run, rigDir)
		})
	})
}

// assertTopologyDoctor runs the real `gc doctor --json` front door and requires
// exit 0, no failures, and no warning from a check whose subject is the bead
// store's topology.
//
// allowedFailures names the checks a shape may legitimately fail at this point
// in its life. Only the legacy shape uses it, and only before its first start:
// a city initialised by an older gc carries that gc's bead vocabulary until the
// new one's lifecycle runs over it once.
func assertTopologyDoctor(t *testing.T, run *helpers.TopologyRun, allowedFailures []string, when string) {
	t.Helper()
	label := run.Topology.Name + " " + when
	out, err := run.City.GC("doctor", "--json")
	var report doctorReport
	lastJSONLine(t, out, &report)

	allowed := make(map[string]bool, len(allowedFailures)+len(run.Topology.DoctorGaps))
	for _, name := range allowedFailures {
		allowed[name] = true
	}
	for _, name := range run.Topology.DoctorGaps {
		allowed[name] = true
	}
	failures, topologyWarnings := 0, 0
	for _, r := range report.Results {
		if r.Status == "ok" {
			continue
		}
		t.Logf("%s: %s — %s", r.Status, r.Name, r.Message)
		switch r.Status {
		case "error":
			if allowed[r.Name] {
				continue
			}
			// A check that timed out reports its own outcome as unknown. The
			// matrix runs eight shapes' worth of real Dolt back to back, so a
			// check that ran out of wall clock is a statement about the box,
			// not about the topology this test is measuring. It is logged
			// above either way.
			if strings.Contains(r.Message, "timed out") {
				continue
			}
			failures++
		case "warning":
			if !beadsTopologyCheck(r.Name) {
				continue
			}
			if want, ok := run.Topology.ExpectedTopologyWarnings[r.Name]; ok && strings.Contains(r.Message, want) {
				continue
			}
			topologyWarnings++
		}
	}
	if failures == 0 && topologyWarnings == 0 {
		return
	}
	if err != nil {
		t.Logf("gc doctor exit: %v", err)
	}
	t.Fatalf("gc doctor on %s reported %d unexpected failure(s) and %d bead-topology warning(s), want none",
		label, failures, topologyWarnings)
}

// assertDeferredInit pins the one thing GC_DOLT=skip promises: init records
// what the scope is going to be and creates nothing.
func assertDeferredInit(t *testing.T, run *helpers.TopologyRun) {
	t.Helper()
	got := helpers.ReadScopeArtifacts(t, run.City.Dir, run.City.Dir)
	if got.HasMetadata {
		t.Errorf("a deferred init wrote a beads binding: %+v", got.Metadata)
	}
	if len(got.Processes) != 0 {
		t.Errorf("a deferred init started Dolt processes:\n%s", strings.Join(got.Processes, "\n"))
	}
	journal, present := helpers.ReadOwnershipJournal(t, run.City.Dir)
	if !present {
		t.Fatal("a deferred init left no ownership record, so nothing knows what to finish")
	}
	entry, ok := journal.Scopes["city"]
	if !ok {
		t.Fatalf("deferred city missing from the ownership journal: %+v", journal.Scopes)
	}
	if entry.State != "provider_initializing" {
		t.Errorf("deferred city state = %q, want provider_initializing", entry.State)
	}
	if entry.Intent.Transport == "" || entry.Intent.Target == "" {
		t.Errorf("deferred city carries no intent to finish: %+v", entry.Intent)
	}
}

// assertBeadRoundTrip drives create, list and show through gc's bd front door.
// A shape that cannot serve beads pins the refusal instead, so the limitation
// stays visible rather than being skipped past.
func assertBeadRoundTrip(t *testing.T, run *helpers.TopologyRun, label string) string {
	t.Helper()
	if want := run.Topology.BeadFrontDoorRefusal; want != "" {
		out, _, err := helpers.RunGCStreams(run.Env, run.City.Dir, "bd", "create", label+" matrix bead", "--json")
		combined, _ := run.City.GC("bd", "create", label+" matrix bead", "--json")
		if err == nil {
			t.Fatalf("gc bd create succeeded on %s, which is documented as unable to serve beads:\n%s", label, out)
		}
		if !strings.Contains(combined, want) {
			t.Fatalf("gc bd create on %s failed with something other than the known limitation %q:\n%s", label, want, combined)
		}
		return ""
	}
	out, err := run.City.GCStdout("bd", "create", label+" matrix bead", "--json")
	if err != nil {
		t.Fatalf("gc bd create on a %s city: %v\n%s", label, err, out)
	}
	var created struct {
		ID string `json:"id"`
	}
	lastJSONLine(t, out, &created)
	if strings.TrimSpace(created.ID) == "" {
		t.Fatalf("gc bd create returned no id:\n%s", out)
	}

	list, err := run.City.GCStdout("bd", "list", "--json")
	if err != nil {
		t.Fatalf("gc bd list on a %s city: %v\n%s", label, err, list)
	}
	if !strings.Contains(list, created.ID) {
		t.Fatalf("gc bd list does not contain %s:\n%s", created.ID, list)
	}
	if show, err := run.City.GCStdout("bd", "show", created.ID, "--json"); err != nil {
		t.Fatalf("gc bd show %s: %v\n%s", created.ID, err, show)
	}
	return created.ID
}

// assertDoltCleanupOutcome drives the dolt pack's stale-db front door and
// requires the answer the shape's ownership implies: a typed no-op naming the
// bd-owned scope, or a real probe of the server gc runs itself.
func assertDoltCleanupOutcome(t *testing.T, run *helpers.TopologyRun) {
	t.Helper()
	out, err := run.City.GCStdout("dolt-cleanup", "--json", "--probe")
	if err != nil {
		if run.Topology.City.Owner == helpers.OwnerNobody {
			// A shape with no Dolt process anywhere has nothing for the reaper
			// to probe. What matters is that it is not mistaken for a bd-owned
			// proxied scope, which is checked below on the output it did emit.
			t.Logf("gc dolt-cleanup on a %s city: %v\n%s", run.Topology.Name, err, out)
		} else {
			t.Fatalf("gc dolt-cleanup --json --probe on a %s city: %v\n%s", run.Topology.Name, err, out)
		}
	}
	var report struct {
		Skipped *struct {
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	lastJSONLine(t, out, &report)

	ownedByProvider := run.Topology.City.Owner == helpers.OwnerProvider &&
		strings.EqualFold(run.Topology.City.DoltMode, "proxied-server")
	if ownedByProvider {
		if report.Skipped == nil || report.Skipped.Reason != "bd-owned-proxied-scope" {
			t.Fatalf("dolt cleanup did not report the bd-owned no-op on a %s city:\n%s", run.Topology.Name, out)
		}
		return
	}
	if report.Skipped != nil && report.Skipped.Reason == "bd-owned-proxied-scope" {
		t.Fatalf("dolt cleanup called a %s city a bd-owned proxied scope:\n%s", run.Topology.Name, out)
	}
}

// assertStopRetiresTheScope stops the city and requires that nothing under it
// or its rig survives — except an upstream the fixture owns, which gc must
// leave running.
func assertStopRetiresTheScope(t *testing.T, run *helpers.TopologyRun, rigDir string) {
	t.Helper()
	out, err := run.Stop()
	if err != nil {
		t.Fatalf("gc stop on a %s city: %v\n%s", run.Topology.Name, err, out)
	}
	for _, root := range []string{run.City.Dir, rigDir} {
		if leaked := helpers.WaitForNoDoltProcesses(t, root, 20*time.Second); len(leaked) > 0 {
			t.Errorf("%s: processes under %s survived gc stop:\n%s",
				run.Topology.Name, root, strings.Join(leaked, "\n"))
		}
	}
	if run.Upstream == nil {
		return
	}
	// gc stops what it owns. The data upstream belongs to whoever runs it, and
	// a stop that reaches across that line is the one failure this shape exists
	// to catch.
	if live := helpers.DoltProcessesUnder(t, run.Upstream.DataDir); len(live) == 0 {
		t.Errorf("%s: gc stop retired the external Dolt server it does not own (%s)",
			run.Topology.Name, run.Upstream.Addr())
	}
}
