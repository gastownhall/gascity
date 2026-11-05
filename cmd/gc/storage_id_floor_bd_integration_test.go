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
	if err != nil || again.Before != floor || again.Floor != floor {
		t.Fatalf("a second reservation = %+v (%v), want the floor already in force", again, err)
	}
}
