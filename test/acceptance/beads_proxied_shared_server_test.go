//go:build acceptance_a

// F9 acceptance: a user-level bd `dolt.shared-server: true` must not relocate
// gc-owned proxied stores into bd's host-wide shared server.
//
// bd resolves dolt.shared-server through layered config, and the user-level
// layers (~/.config/bd/config.yaml, the legacy ~/.beads/config.yaml) sit under
// every scope that does not decide for itself. On the proxied default a
// user-level `true` used to root the city's and every rig's proxy and Dolt child
// in ~/.beads/shared-server — silently, and shared by every city on the host, so
// two cities' `hq` stores became one database.
//
// bd never sees the real HOME here (the harness re-homes every bd under the
// Env's tool home), so the user-level config is supplied through
// XDG_CONFIG_HOME — bd's os.UserConfigDir() layer — and BEADS_SHARED_SERVER_DIR
// points the shared server at a test directory, so a regression lands there
// and never in the operator's ~/.beads/shared-server. The harness also pins
// BD_DOLT_SHARED_SERVER=false by default; this test clears it to "" (which bd
// reads as unset) so the user-level layer is what decides.
package acceptance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestBeadsProxiedIgnoresUserLevelSharedServer(t *testing.T) {
	bdPath, doltPath := requireProxiedTooling(t)

	base := helpers.TempDir(t)
	xdgConfig := filepath.Join(base, "xdg-config")
	sharedDir := filepath.Join(base, "shared-server")
	if err := os.MkdirAll(filepath.Join(xdgConfig, "bd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdgConfig, "bd", "config.yaml"), []byte("dolt:\n  shared-server: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := proxiedEnv(t, bdPath, doltPath).
		With("XDG_CONFIG_HOME", xdgConfig).
		With("BEADS_SHARED_SERVER_DIR", sharedDir).
		With("BD_DOLT_SHARED_SERVER", "")

	city := helpers.NewCity(t, env)
	cityRoot := city.Dir
	rigDir := createGitRig(t)
	t.Cleanup(func() {
		helpers.RunGC(env, cityRoot, "stop", cityRoot)         //nolint:errcheck
		helpers.RunGC(env, "", "supervisor", "stop", "--wait") //nolint:errcheck
		for _, root := range []string{cityRoot, rigDir, sharedDir} {
			if leaked := waitForNoDoltProcesses(t, root, 15*time.Second); len(leaked) > 0 {
				t.Errorf("processes under %s outlived the test:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
	})

	city.Init("claude")
	city.RigAdd(rigDir, "")

	for _, scope := range []struct{ label, root string }{{"city", cityRoot}, {"rig", rigDir}} {
		bead, err := helpers.RunGC(env, scope.root, "bd", "create", "f9 "+scope.label+" bead", "--json")
		if err != nil {
			t.Fatalf("gc bd create in %s: %v\n%s", scope.label, err, bead)
		}

		// The store is the scope's own: bd-owned proxy under <scope>/.beads/dolt.
		assertProxiedScope(t, scope.root, scope.label)
		var sidecar proxiedSidecar
		readJSONFile(t, filepath.Join(scope.root, ".beads", "proxied_server_client_info.json"), &sidecar)
		if sidecar.RootPath != "" && strings.HasPrefix(sidecar.RootPath, sharedDir) {
			t.Errorf("%s proxy is rooted in the shared server: %s", scope.label, sidecar.RootPath)
		}

		// The pin a raw bd (an agent's shell) resolves is in the scope config.
		cfg, err := os.ReadFile(filepath.Join(scope.root, ".beads", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(cfg), "shared-server: false") {
			t.Errorf("%s config.yaml does not pin dolt.shared-server off:\n%s", scope.label, cfg)
		}

		// A raw bd run the way an agent shell runs it — no gc projection, just
		// the scope and the user-level config — still reads the scope's store.
		raw := exec.Command(bdPath, "list", "--json") //nolint:gosec // resolved test binary
		raw.Dir = scope.root
		raw.Env = env.Clone().With("BEADS_DIR", filepath.Join(scope.root, ".beads")).ToolList()
		out, err := raw.CombinedOutput()
		if err != nil {
			t.Fatalf("raw bd list in %s: %v\n%s", scope.label, err, out)
		}
		if !strings.Contains(string(out), "f9 "+scope.label+" bead") {
			t.Errorf("raw bd list in %s does not see the scope's bead (store relocated?):\n%s", scope.label, out)
		}
	}

	if entries, err := os.ReadDir(filepath.Join(sharedDir, "dolt")); err == nil && len(entries) > 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("bd's shared server received this city's stores: %s", strings.Join(names, ", "))
	}

	assertDoctorGreen(t, city, "a proxied city under a user-level shared-server: true")
}
