//go:build acceptance_a

// Adopting a bd store bound to a loopback Dolt server gc does not run.
//
// `bd init --server --external` against a dolt sql-server an operator runs on
// this machine records a binding spelled exactly like gc's own managed server:
// 127.0.0.1 and a port. gc used to classify every loopback binding as its own,
// so `gc rig add --adopt` ran gc's init op over the store and failed, and a
// city on that server could not restart once gc had stamped its endpoint keys
// into bd's config.yaml. A loopback binding is gc's only when gc's runtime
// record for the city names its port; neither city here has one.
//
// Each test drives the real gc, bd and dolt through the front doors: adopt,
// the bd front door into the rig, stop and start, remove and re-adopt, and
// doctor. bd's own files must stay byte-identical throughout.
package acceptance_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

const (
	loopbackAdoptRigName  = "adopted"
	loopbackAdoptPrefix   = "hv"
	loopbackAdoptDatabase = "hv"
	loopbackAdoptCityDB   = "hq"
	loopbackAdoptBdInit   = 3 * time.Minute

	// loopbackAdoptTypesCheck is the adopted rig's custom-types doctor check.
	loopbackAdoptTypesCheck = "custom-types:" + loopbackAdoptRigName
)

// loopbackAdoptStoreFiles are the bd-owned files under a store's .beads that gc
// must leave as found.
var loopbackAdoptStoreFiles = []string{"metadata.json", "config.yaml", loopbackAdoptPortFile}

// loopbackAdoptPortFile is bd's own port mirror. bd itself writes it, naming
// the server it is bound to, the first time it opens a server-mode store, so it
// may appear; what it may never do is name anything but bd's binding.
const loopbackAdoptPortFile = "dolt-server.port"

// loopbackServerAdoption is one city plus an operator-run dolt sql-server on
// loopback and a rig workspace bd bound to that server.
type loopbackServerAdoption struct {
	run      *helpers.TopologyRun
	rigDir   string
	snapshot map[string][]byte
}

// newLoopbackServerAdoption starts the server, initialises the city with
// initArgs, and runs `bd init --server --external` in the rig workspace.
func newLoopbackServerAdoption(t *testing.T, name string, initArgs func(up *helpers.ExternalDolt) []string) *loopbackServerAdoption {
	t.Helper()
	bdPath, doltPath := helpers.RequireTopologyTooling(t)
	root := helpers.TempDir(t)
	env := helpers.TopologyEnv(t, testEnv, root, bdPath, doltPath)
	up := helpers.StartExternalDolt(t, env, filepath.Join(root, "upstream"), loopbackAdoptCityDB)

	run := &helpers.TopologyRun{
		Topology: helpers.BeadsTopology{Name: name},
		Env:      env,
		Root:     root,
		Upstream: up,
	}
	run.City = helpers.NewCityAt(t, env, filepath.Join(root, "city"))
	// Registered after StartExternalDolt's own cleanup, so it runs first: the
	// city lets go of the server before the server stops.
	t.Cleanup(func() {
		helpers.RunGC(env, run.City.Dir, "stop", run.City.Dir)       //nolint:errcheck // best effort
		helpers.RunGC(env, "", "supervisor", "stop", "--wait")       //nolint:errcheck // best effort
		helpers.RunGC(env, run.City.Dir, "stop", run.City.Dir)       //nolint:errcheck // best effort
		helpers.RunGC(env, run.City.Dir, "unregister", run.City.Dir) //nolint:errcheck // best effort
		up.Stop()
		if leaked := helpers.WaitForNoDoltProcesses(t, root, 20*time.Second); len(leaked) > 0 {
			t.Errorf("processes under %s outlived the test:\n%s", root, strings.Join(leaked, "\n"))
		}
	})

	args := []string{"init", "--skip-provider-readiness", "--no-start", "--provider", "claude"}
	if initArgs != nil {
		args = append(args, initArgs(up)...)
	}
	args = append(args, run.City.Dir)
	if out, err := helpers.RunGC(env, "", args...); err != nil {
		t.Fatalf("gc init: %v\n%s", err, out)
	}

	rigDir := run.RigWorkspace(t, loopbackAdoptRigName)
	ctx, cancel := context.WithTimeout(context.Background(), loopbackAdoptBdInit)
	defer cancel()
	initCmd := exec.CommandContext(ctx, bdPath, "init", "--server", "--external", //nolint:gosec // resolved test binary
		"--server-host", up.Host, "--server-port", up.Port,
		"--database", loopbackAdoptDatabase, "-p", loopbackAdoptPrefix,
		"--skip-hooks", "--skip-agents", "--quiet", "--non-interactive", rigDir)
	initCmd.Dir = rigDir
	initCmd.Env = env.Clone().With("BEADS_DIR", filepath.Join(rigDir, ".beads")).ToolList()
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init --server --external on %s: %v\n%s", up.Addr(), err, out)
	}

	a := &loopbackServerAdoption{run: run, rigDir: rigDir}
	a.snapshot = a.readStoreFiles(t)
	if _, ok := a.snapshot["metadata.json"]; !ok {
		t.Fatalf("bd init wrote no metadata.json under %s", rigDir)
	}
	return a
}

// readStoreFiles returns the bd-owned files that exist under the rig's .beads.
func (a *loopbackServerAdoption) readStoreFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range loopbackAdoptStoreFiles {
		data, err := os.ReadFile(filepath.Join(a.rigDir, ".beads", name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		files[name] = data
	}
	return files
}

// assertStoreFilesAsFound requires bd's files to be byte-identical to what
// bd init wrote, and none to have appeared or vanished, except bd's own port
// mirror, which bd may write but only with the port of its binding, and which
// is then held to the same rule.
func (a *loopbackServerAdoption) assertStoreFilesAsFound(t *testing.T, when string) {
	t.Helper()
	got := a.readStoreFiles(t)
	for _, name := range loopbackAdoptStoreFiles {
		want, hadIt := a.snapshot[name]
		have, hasIt := got[name]
		switch {
		case hadIt && !hasIt:
			t.Errorf("%s: .beads/%s was removed", when, name)
		case !hadIt && hasIt && name == loopbackAdoptPortFile:
			if port := strings.TrimSpace(string(have)); port != a.run.Upstream.Port {
				t.Errorf("%s: .beads/%s names port %q, want bd's binding %s", when, name, port, a.run.Upstream.Port)
			}
			// From here on it is bd's file as found: gc must neither rewrite
			// nor remove it.
			a.snapshot[name] = have
		case !hadIt && hasIt:
			t.Errorf("%s: .beads/%s was written:\n%s", when, name, have)
		case hadIt && !bytes.Equal(want, have):
			t.Errorf("%s: .beads/%s changed:\n--- before\n%s\n--- after\n%s", when, name, want, have)
		}
	}
}

func (a *loopbackServerAdoption) adopt(t *testing.T, when string) {
	t.Helper()
	out, err := a.run.GC("rig", "add", "--adopt", "--prefix", loopbackAdoptPrefix, a.rigDir)
	if err != nil {
		t.Fatalf("%s: gc rig add --adopt of a store bound to a loopback server gc does not run: %v\n%s", when, err, out)
	}
	a.assertStoreFilesAsFound(t, when)
}

// createRigBead writes through the bd front door into the adopted rig and
// returns the bead's id.
func (a *loopbackServerAdoption) createRigBead(t *testing.T) string {
	t.Helper()
	out, err := a.run.City.GCStdout("bd", "--rig", loopbackAdoptRigName, "create", "loopback adopt", "--json")
	if err != nil {
		t.Fatalf("gc bd --rig %s create: %v\n%s", loopbackAdoptRigName, err, out)
	}
	var created struct {
		ID string `json:"id"`
	}
	lastJSONLine(t, out, &created)
	if !strings.HasPrefix(created.ID, loopbackAdoptPrefix+"-") {
		t.Fatalf("gc bd --rig %s create returned id %q, want prefix %s-:\n%s", loopbackAdoptRigName, created.ID, loopbackAdoptPrefix, out)
	}
	a.assertRigListsBead(t, created.ID, "after create")
	return created.ID
}

func (a *loopbackServerAdoption) assertRigListsBead(t *testing.T, id, when string) {
	t.Helper()
	list, err := a.run.City.GCStdout("bd", "--rig", loopbackAdoptRigName, "list", "--json")
	if err != nil {
		t.Fatalf("%s: gc bd --rig %s list: %v\n%s", when, loopbackAdoptRigName, err, list)
	}
	if !strings.Contains(list, id) {
		t.Fatalf("%s: gc bd --rig %s list does not contain %s:\n%s", when, loopbackAdoptRigName, id, list)
	}
}

// registerRigTypes runs the one-shot repair doctor names for the rig's
// custom-types check. gc writes no bead types into a store it does not serve,
// on adopt or on start, so the operator registers them, through bd at the
// store's own binding, which leaves bd's files as found.
func (a *loopbackServerAdoption) registerRigTypes(t *testing.T) {
	t.Helper()
	if out, err := a.run.GC("doctor", "--fix", "--check", loopbackAdoptTypesCheck); err != nil {
		t.Fatalf("gc doctor --fix --check %s: %v\n%s", loopbackAdoptTypesCheck, err, out)
	}
	a.assertStoreFilesAsFound(t, "after gc doctor --fix")
}

// assertDoctorReportsNoRigFailure requires that no doctor check about the
// adopted rig is an error: neither a check named for the rig nor the
// city-wide dolt-drift check, which names a rig only in its details and must
// say nothing about one gc does not serve.
func (a *loopbackServerAdoption) assertDoctorReportsNoRigFailure(t *testing.T) {
	t.Helper()
	out, _ := a.run.GC("doctor", "--json")
	var report doctorReport
	lastJSONLine(t, out, &report)
	sawDrift := false
	for _, r := range report.Results {
		if r.Name == "dolt-drift" {
			sawDrift = true
			details := strings.Join(r.Details, "\n")
			if r.Status == "error" || strings.Contains(details, strconv.Quote(loopbackAdoptRigName)) {
				t.Errorf("gc doctor: dolt-drift = %s: %s\n%s", r.Status, r.Message, details)
			}
			continue
		}
		if r.Status == "error" && strings.Contains(r.Name, loopbackAdoptRigName) {
			t.Errorf("gc doctor: %s = error: %s", r.Name, r.Message)
		}
	}
	if !sawDrift {
		t.Errorf("gc doctor reported no dolt-drift result:\n%s", out)
	}
}

// exercise is the shared sequence: adopt, write and read through the bd front
// door, stop and start, remove and re-adopt, then doctor's custom-types repair
// and a doctor run.
func (a *loopbackServerAdoption) exercise(t *testing.T) {
	t.Helper()
	a.adopt(t, "adopt")
	id := a.createRigBead(t)

	a.run.Start(t)
	a.assertStoreFilesAsFound(t, "after gc start")
	if out, err := a.run.Stop(); err != nil {
		t.Fatalf("gc stop: %v\n%s", err, out)
	}
	a.run.Start(t)
	a.assertStoreFilesAsFound(t, "after gc stop and start")
	a.assertRigListsBead(t, id, "after restart")

	if out, err := a.run.GC("rig", "remove", loopbackAdoptRigName); err != nil {
		t.Fatalf("gc rig remove %s: %v\n%s", loopbackAdoptRigName, err, out)
	}
	a.adopt(t, "re-adopt")
	a.assertRigListsBead(t, id, "after re-adopt")
	a.registerRigTypes(t)
	a.assertDoctorReportsNoRigFailure(t)
}

// TestBeadsAdoptLoopbackServerStoreDirectExternalCity adopts the store into a
// city bound directly to the same server, which gc does not run either.
func TestBeadsAdoptLoopbackServerStoreDirectExternalCity(t *testing.T) {
	a := newLoopbackServerAdoption(t, "loopback-adopt-direct-external-city", func(up *helpers.ExternalDolt) []string {
		return []string{
			"--beads-transport", "direct", "--beads-target", "external",
			"--dolt-host", up.Host, "--dolt-port", up.Port, "--dolt-database", loopbackAdoptCityDB,
		}
	})
	a.exercise(t)

	cityDir := a.run.City.Dir
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); err == nil {
		t.Error("gc wrote managed-Dolt runtime state for a city bound to a server it does not run")
	}
	if procs := helpers.DoltProcessesUnder(t, cityDir); len(procs) > 0 {
		t.Errorf("gc started a Dolt process under the city:\n%s", strings.Join(procs, "\n"))
	}
}

// TestBeadsAdoptLoopbackServerStoreProxiedCity adopts the store into a default
// (proxied-local) city, which runs no Dolt server of its own.
func TestBeadsAdoptLoopbackServerStoreProxiedCity(t *testing.T) {
	a := newLoopbackServerAdoption(t, "loopback-adopt-proxied-city", nil)
	a.exercise(t)
}
