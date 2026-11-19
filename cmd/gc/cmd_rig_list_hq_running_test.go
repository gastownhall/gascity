package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// rigListHQRunningCity builds a supervisor-registered city and replaces the
// supervisor probes with stand-ins that count their calls, so a test can tell
// whether the rig list HQ row consulted the supervisor HTTP API.
func rigListHQRunningCity(t *testing.T) (cityPath string, supervisorAPICalls *atomic.Int64) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))
	t.Setenv("GC_NO_API", "1")
	t.Setenv("GC_DEBUG", "0")

	root := shortSocketTempDir(t, "gc-rigl-")
	cityPath = filepath.Join(root, "city")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(charCityBasic), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, charCityName); err != nil {
		t.Fatalf("register city: %v", err)
	}

	supervisorAPICalls = &atomic.Int64{}
	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4321 }
	supervisorCityRunningHook = func(string) (bool, string, bool) {
		supervisorAPICalls.Add(1)
		return true, "", true
	}
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})
	return cityPath, supervisorAPICalls
}

// runLocalRigListJSON runs the local route of `gc rig list --json`, as
// GC_NO_API=1 selects it, and returns the HQ row's running value.
func runLocalRigListJSON(t *testing.T, cityPath string) bool {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := routeRigList(cityPath, nil, "escape-hatch", true, &stdout, &stderr); code != 0 {
		t.Fatalf("routeRigList exit = %d, stderr=%q", code, stderr.String())
	}
	var doc RigListJSON
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal rig list: %v\n%s", err, stdout.String())
	}
	if len(doc.Rigs) == 0 || !doc.Rigs[0].HQ {
		t.Fatalf("rig list has no HQ row first:\n%s", stdout.String())
	}
	return doc.Rigs[0].Running
}

// GC_NO_API=1 is the escape hatch operators use when the supervisor API
// misbehaves, so the local route must not reach the supervisor over HTTP to
// fill the HQ row. The supervisor serves the city's controller socket while
// the city runs, so a ping on that socket answers the question locally.
func TestRigListLocalHQRunningAsksTheCitySocketNotTheSupervisorAPI(t *testing.T) {
	cityPath, supervisorAPICalls := rigListHQRunningCity(t)
	startFakeControllerSocket(t, cityPath, "4321\n")

	if !runLocalRigListJSON(t, cityPath) {
		t.Error("HQ running = false, want true: the city's controller socket answered ping")
	}
	if n := supervisorAPICalls.Load(); n != 0 {
		t.Errorf("local rig list asked the supervisor API %d time(s), want 0", n)
	}
}

func TestRigListLocalHQNotRunningWithoutTheCitySocket(t *testing.T) {
	cityPath, supervisorAPICalls := rigListHQRunningCity(t)

	if runLocalRigListJSON(t, cityPath) {
		t.Error("HQ running = true, want false: nothing answers on the city's controller socket")
	}
	if n := supervisorAPICalls.Load(); n != 0 {
		t.Errorf("local rig list asked the supervisor API %d time(s), want 0", n)
	}
}
