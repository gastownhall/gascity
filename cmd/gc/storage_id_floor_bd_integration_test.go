//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestTheCityFloorHoldsOnARealBdStore runs the floor against real bd
// (embedded Dolt; no server, no port): bd mints ids, the clear removes them,
// the floor is raised past them, and every id bd mints afterwards, plain and
// wisp, has a longer hash suffix than any carried id, so none can be one. A
// second reservation writes nothing.
func TestTheCityFloorHoldsOnARealBdStore(t *testing.T) {
	bdPath, err := findPreferredBinary("bd", strings.TrimSpace(os.Getenv("GC_TEST_BD_BIN")))
	if err != nil {
		t.Skip("bd not found (set GC_TEST_BD_BIN or install bd)")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not found")
	}
	root := t.TempDir()
	home, dir := filepath.Join(root, "home"), filepath.Join(root, "ws")
	for _, d := range []string{home, dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeTestGitIdentity(home); err != nil {
		t.Fatal(err)
	}
	if err := writeTestDoltIdentity(home); err != nil {
		t.Fatal(err)
	}
	run := beads.ExecCommandRunnerWithEnvWithoutAmbientBeads(map[string]string{
		"HOME":               home,
		"XDG_CONFIG_HOME":    filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL":  filepath.Join(home, ".gitconfig"),
		"BD_BIN":             bdPath,
		"BEADS_DIR":          filepath.Join(dir, ".beads"),
		"BD_NON_INTERACTIVE": "1",
		"BD_BACKUP_ENABLED":  "false",
		"BD_EXPORT_AUTO":     "false",
		"BEADS_ACTOR":        "gc-test",
	})
	must := func(name string, args ...string) string {
		t.Helper()
		out, err := run(dir, name, args...)
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	must("git", "init", "-q", ".")
	must("bd", "init", "-p", "mc", "--quiet", "--skip-hooks")
	mint := func(n int) []string {
		var ids []string
		for i := 0; i < n; i++ {
			args := []string{"create", "--json", "minted", "-t", "task"}
			if i%2 == 1 {
				args = append(args, "--ephemeral")
			}
			ids = append(ids, parseCreatedBeadID(t, must("bd", args...)))
		}
		return ids
	}

	// The carried ids: minted at bd's default length, then cleared.
	carried := mint(40)
	for _, id := range carried {
		must("bd", "delete", "--force", id)
	}
	store := beads.NewBdStore(dir, run)
	stubCityHashConfig(t, cityHashConfig{Conf: store, Prefix: "mc"})
	got, err := reserveCityIDs(dir, carried)
	if err != nil {
		t.Fatal(err)
	}
	floor := cityIDFloor(carried, "mc")
	if got.Floor != floor || floor == 0 {
		t.Fatalf("reserved %+v, want the floor %d", got, floor)
	}
	if v, err := store.ConfigGet(bdMinHashLengthKey); err != nil || strings.TrimSpace(v) != strconv.Itoa(floor) {
		t.Fatalf("bd reads %s=%q (%v), want %d", bdMinHashLengthKey, v, err, floor)
	}

	was := map[string]bool{}
	for _, id := range carried {
		was[id] = true
	}
	for _, id := range mint(150) {
		if was[id] {
			t.Fatalf("bd minted the carried id %s again after the floor", id)
		}
		if n := len(rigIDHashSuffix(id)); n < floor {
			t.Fatalf("bd minted %s, a %d-character suffix under the floor %d", id, n, floor)
		}
	}

	again, err := reserveCityIDs(dir, carried)
	if err != nil || len(again.Changes) != 0 || again.Floor != floor {
		t.Fatalf("a second reservation = %+v (%v), want the floor already in force", again, err)
	}
}

// TestTheDefaultFloorOpenerReachesARealBdCity runs the floor through the
// production surface (defaultCityHashConfigOpener): a bd city initialized on
// disk, its prefix read from its own config, the bd runner gc builds for the
// scope. A second reservation writes nothing.
func TestTheDefaultFloorOpenerReachesARealBdCity(t *testing.T) {
	bdPath, err := findPreferredBinary("bd", strings.TrimSpace(os.Getenv("GC_TEST_BD_BIN")))
	if err != nil {
		t.Skip("bd not found (set GC_TEST_BD_BIN or install bd)")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not found")
	}
	root := t.TempDir()
	home, city := filepath.Join(root, "home"), filepath.Join(root, "city")
	for _, d := range []string{home, city} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeTestGitIdentity(home); err != nil {
		t.Fatal(err)
	}
	if err := writeTestDoltIdentity(home); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"HOME":               home,
		"XDG_CONFIG_HOME":    filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL":  filepath.Join(home, ".gitconfig"),
		"BD_BIN":             bdPath,
		"BEADS_DIR":          filepath.Join(city, ".beads"),
		"BD_NON_INTERACTIVE": "1",
		"BD_BACKUP_ENABLED":  "false",
		"BD_EXPORT_AUTO":     "false",
		"BEADS_ACTOR":        "gc-test",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	run := beads.ExecCommandRunnerWithEnvWithoutAmbientBeads(env)
	must := func(name string, args ...string) string {
		t.Helper()
		out, err := run(city, name, args...)
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	must("git", "init", "-q", ".")
	must("bd", "init", "-p", "mc", "--quiet", "--skip-hooks")
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"mc\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := cityHashConfigOpener
	cityHashConfigOpener = defaultCityHashConfigOpener
	t.Cleanup(func() { cityHashConfigOpener = prev })

	carried := []string{"mc-a1b2c", "mc-wisp-xy9", "mc-xsg-ddb51ee0b716"}
	got, err := reserveCityIDs(city, carried)
	if err != nil {
		t.Fatalf("the default surface's reservation: %v", err)
	}
	if got.Skipped != "" || got.Floor != 6 {
		t.Fatalf("reserved %+v, want the floor 6 set on the bd city", got)
	}
	if v := strings.TrimSpace(must("bd", "config", "get", bdMinHashLengthKey)); !strings.Contains(v, "6") {
		t.Fatalf("bd reads %s = %q, want 6", bdMinHashLengthKey, v)
	}
	again, err := reserveCityIDs(city, carried)
	if err != nil || len(again.Changes) != 0 || again.Floor != 6 {
		t.Fatalf("a second reservation = %+v (%v), want the floor already in force", again, err)
	}
	if id := parseCreatedBeadID(t, must("bd", "create", "--json", "minted", "-t", "task")); len(rigIDHashSuffix(id)) < 6 {
		t.Fatalf("bd minted %s under the floor 6", id)
	}
}
