package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fanoutTestCity builds a proxied city with two proxied rigs, a and b, whose
// provider script records every scope it is invoked for and refuses the one
// named by refuse ("city", "a", "b", or "" for none) the way bd refuses a
// proxy record it cannot verify without --force.
type fanoutCity struct {
	path    string
	rigA    string
	rigB    string
	logPath string
}

func newFanoutTestCity(t *testing.T, refuse string) fanoutCity {
	t.Helper()
	city := fanoutCity{path: t.TempDir()}
	city.rigA = filepath.Join(city.path, "rigs", "a")
	city.rigB = filepath.Join(city.path, "rigs", "b")
	city.logPath = filepath.Join(city.path, "provider-ops")
	for _, dir := range []string{city.rigA, city.rigB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	refused := map[string]string{"city": city.path, "a": city.rigA, "b": city.rigB}[refuse]
	script := filepath.Join(city.path, "provider.sh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$BEADS_DIR\" >> \"" + city.logPath + "\"\n"
	if refused != "" {
		body += "if [ \"$BEADS_DIR\" = \"" + filepath.Join(normalizePathForCompare(refused), ".beads") + "\" ]; then\n" +
			"  echo 'proxy process identity is unverifiable' >&2\n  exit 1\nfi\n"
	}
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	cityTOML := "[workspace]\nname = \"t\"\n[beads]\nprovider = \"exec:" + script + "\"\n" +
		"[[rigs]]\nname = \"a\"\npath = \"" + city.rigA + "\"\n" +
		"[[rigs]]\nname = \"b\"\npath = \"" + city.rigB + "\"\n"
	if err := os.WriteFile(filepath.Join(city.path, "city.toml"), []byte(cityTOML), 0o644); err != nil { //nolint:gosec // fixture config
		t.Fatal(err)
	}
	for _, scope := range []string{city.path, city.rigA, city.rigB} {
		writeScopeBeadsMetadata(t, scope, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`)
		if err := os.MkdirAll(filepath.Join(scope, ".beads", "dolt"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return city
}

func beadsDirsFor(roots ...string) []string {
	dirs := make([]string, 0, len(roots))
	for _, root := range roots {
		dirs = append(dirs, filepath.Join(normalizePathForCompare(root), ".beads"))
	}
	return dirs
}

// A retiring op must attempt every provider-owned scope. Returning at the
// first failure left the proxy and Dolt child of every later scope resident,
// and rerunning `gc stop` repeated the same short-circuit because the failing
// scope is still visited first. Health reports the same way: one bad scope
// must not hide the state of the others.
func TestProviderOwnedRetiringOpVisitsEveryScopeAndAggregatesFailures(t *testing.T) {
	for _, op := range []string{"stop", "shutdown", "health"} {
		t.Run(op, func(t *testing.T) {
			city := newFanoutTestCity(t, "a")
			err := runProviderOwnedScopesLifecycleOp(city.path, op)
			if err == nil {
				t.Fatalf("%s hid a failing scope", op)
			}
			if !strings.Contains(err.Error(), city.rigA) {
				t.Errorf("%s error does not name the failing scope: %v", op, err)
			}
			data, readErr := os.ReadFile(city.logPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := beadsDirsFor(city.path, city.rigA, city.rigB)
			if got := strings.Fields(string(data)); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s fan-out = %#v, want every scope %#v", op, got, want)
			}
		})
	}
}

// A starting op keeps its fail-fast contract: initializing a rig under a city
// scope that just refused is not a recovery, it is a second owner.
func TestProviderOwnedStartOpStillStopsAtTheFirstFailure(t *testing.T) {
	city := newFanoutTestCity(t, "city")
	if err := runProviderOwnedScopesLifecycleOp(city.path, "start"); err == nil {
		t.Fatal("start reported success for a failing scope")
	}
	data, err := os.ReadFile(city.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(string(data)), beadsDirsFor(city.path); !reflect.DeepEqual(got, want) {
		t.Fatalf("start fan-out = %#v, want %#v", got, want)
	}
}
