//go:build acceptance_a && unix

package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// A user-level shared-server setting once put every managed server-mode bd
// call behind the host-wide gate. Hold that gate while crossing the real gc
// and bd CLI boundary; a gc-owned city and rig must still read their stores.
func TestBeadsManagedServerIgnoresUserLevelSharedServerGate(t *testing.T) {
	bdPath, doltPath := requireProxiedTooling(t)
	base := helpers.TempDir(t)
	sharedDir := filepath.Join(base, "shared-server")
	xdgConfig := filepath.Join(base, "xdg-config")
	if err := os.MkdirAll(filepath.Join(xdgConfig, "bd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdgConfig, "bd", "config.yaml"), []byte("dolt:\n  shared-server: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build the legacy managed-server topology without the polluted layer so
	// fixture setup cannot consume the same gate this test later holds.
	env := proxiedEnv(t, bdPath, doltPath)
	city := helpers.NewCity(t, env)
	if out, err := helpers.RunGC(env, "", "init", "--skip-provider-readiness", "--no-start", "--provider", "claude", city.Dir); err != nil {
		t.Fatalf("init fixture city: %v\n%s", err, out)
	}
	rigDir := createGitRig(t)
	t.Cleanup(func() {
		helpers.RunGC(env, city.Dir, "stop", city.Dir)         //nolint:errcheck // best effort after failed setup
		helpers.RunGC(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best effort after failed setup
		for _, root := range []string{city.Dir, rigDir} {
			if leaked := waitForNoDoltProcesses(t, root, 15*time.Second); len(leaked) > 0 {
				t.Errorf("processes under %s outlived the test:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
	})
	makeCityLookLegacyManaged(t, env, bdPath, city.Dir)
	if out, err := helpers.RunGC(env, city.Dir, "start", city.Dir); err != nil {
		t.Fatalf("start fixture city: %v\n%s", err, out)
	}
	if out, err := helpers.RunGC(env, city.Dir, "rig", "add", rigDir); err != nil {
		t.Fatalf("add fixture rig: %v\n%s", err, out)
	}
	if out, err := helpers.RunGC(env, city.Dir, "stop", city.Dir); err != nil {
		t.Fatalf("stop fixture city: %v\n%s", err, out)
	}
	if out, err := helpers.RunGC(env, "", "supervisor", "stop", "--wait"); err != nil {
		t.Fatalf("stop fixture supervisor: %v\n%s", err, out)
	}

	polluted := env.Clone().With("XDG_CONFIG_HOME", xdgConfig).
		With("BEADS_SHARED_SERVER_DIR", sharedDir).
		With("BD_DOLT_SHARED_SERVER", "")
	if out, err := helpers.RunGC(polluted, city.Dir, "start", city.Dir); err != nil {
		t.Fatalf("restart managed city under user-level shared-server config: %v\n%s", err, out)
	}

	for _, scope := range []struct{ label, root string }{{"city", city.Dir}, {"rig", rigDir}} {
		cfg, err := os.ReadFile(filepath.Join(scope.root, ".beads", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(cfg), "dolt.shared-server: false") && !strings.Contains(string(cfg), "shared-server: false") {
			t.Errorf("%s config.yaml did not pin shared-server off after gc start:\n%s", scope.label, cfg)
		}
	}

	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(sharedDir, "dolt.gate.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // temporary test lock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck // temporary test lock

	for _, scope := range []struct {
		label string
		args  []string
	}{
		{"city", []string{"bd", "list", "--limit", "1"}},
		{"rig", []string{"bd", "--rig", filepath.Base(rigDir), "list", "--limit", "1"}},
	} {
		out, runErr := helpers.RunGC(polluted, city.Dir, scope.args...)
		if runErr != nil {
			t.Errorf("gc bd list in %s failed under the shared-server gate: %v\n%s", scope.label, runErr, out)
		}
		if runErr == nil && strings.Contains(out, "maintenance operation is running") {
			t.Errorf("gc bd list in %s reached the shared-server gate:\n%s", scope.label, out)
		}
	}

	// The same user layer and lock must still affect a scope gc does not own.
	// Change the rig's endpoint marker after the positive reads, and remove
	// its managed-scope pin so the control exercises the user layer itself.
	configPath := filepath.Join(rigDir, ".beads", "config.yaml")
	cfg, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	controlCfg := strings.Replace(string(cfg), "gc.endpoint_origin: inherited_city", "gc.endpoint_origin: explicit", 1)
	if controlCfg == string(cfg) {
		t.Fatalf("rig had no inherited-city endpoint marker:\n%s", cfg)
	}
	controlCfg = strings.ReplaceAll(controlCfg, "dolt.shared-server: false\n", "")
	controlCfg = strings.ReplaceAll(controlCfg, "  shared-server: false\n", "")
	controlCfg += "dolt.host: 127.0.0.1\ndolt.port: 39999\n"
	if err := os.WriteFile(configPath, []byte(controlCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out, runErr := helpers.RunGC(polluted, city.Dir, "bd", "--rig", filepath.Base(rigDir), "list", "--limit", "1")
	if runErr == nil || !strings.Contains(out, "maintenance operation is running") {
		t.Errorf("explicit-endpoint rig did not follow the user-level shared-server gate: err=%v\n%s", runErr, out)
	}
}
