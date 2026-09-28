package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The custom-types check and its --fix exec'd bare PATH `bd`, so a city that
// pins `[workspace.env] BD_BIN=/opt/beads-rc2/bd` had its store read — and, on
// --fix, written — through whatever binary PATH happened to hold: not the one
// that created the store, and for a proxied scope not the one that owns the
// proxy. Every other gc bd call resolves the pin first; this one did not, and
// the topology matrix cannot see it because TopologyEnv symlinks the run's bd
// onto PATH so pin and PATH never diverge.
func TestCustomTypesCheckRunsThePinnedBdBinary(t *testing.T) {
	dir := guardedTempDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	// PATH holds no bd at all, so anything the check runs other than the pin
	// fails to exec and the recorded arguments stay absent.
	t.Setenv("PATH", filepath.Join(dir, "empty-path"))

	recorded := filepath.Join(dir, "bd-calls")
	pinned := writeRecordingBdStub(t, dir, recorded)

	c := NewCustomTypesCheck(dir, "test", pinned)
	if r := c.Run(&CheckContext{CityPath: dir}); r.Status != StatusOK {
		t.Fatalf("Run status = %v (message=%q), want OK — the pinned bd answered", r.Status, r.Message)
	}
	calls := readRecordedBdCalls(t, recorded)
	for _, want := range []string{"config get --json types.custom", "types --json"} {
		if !strings.Contains(calls, want) {
			t.Errorf("pinned bd was not asked %q; calls:\n%s", want, calls)
		}
	}
}

// --fix writes types.custom, so it is the call that matters most: issued
// through an unrelated binary it reconfigures a store that binary does not own.
func TestCustomTypesCheckFixWritesThroughThePinnedBdBinary(t *testing.T) {
	dir := guardedTempDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(dir, "empty-path"))

	recorded := filepath.Join(dir, "bd-calls")
	pinned := writeRecordingBdStub(t, dir, recorded)

	c := NewCustomTypesCheck(dir, "test", pinned)
	c.missing = []string{"molecule"}
	if err := c.Fix(&CheckContext{CityPath: dir}); err != nil {
		t.Fatalf("Fix through the pinned bd: %v", err)
	}
	if calls := readRecordedBdCalls(t, recorded); !strings.Contains(calls, "config set types.custom") {
		t.Fatalf("pinned bd did not receive the types.custom write; calls:\n%s", calls)
	}
}

// writeRecordingBdStub installs a bd stand-in outside PATH that appends its
// arguments to a log and answers the two read verbs the check issues.
func writeRecordingBdStub(t *testing.T, dir, recorded string) string {
	t.Helper()
	path := filepath.Join(dir, "pinned-bd")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + recorded + "\n" +
		"case \"$1 $2\" in\n" +
		"  'config get') echo '{\"value\":\"" + strings.Join(RequiredCustomTypes, ",") + "\"}' ;;\n" +
		"  'types --json') echo '{\"custom_types\":[\"" + strings.Join(RequiredCustomTypes, "\",\"") + "\"]}' ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func readRecordedBdCalls(t *testing.T, recorded string) string {
	t.Helper()
	data, err := os.ReadFile(recorded)
	if err != nil {
		if os.IsNotExist(err) {
			return "<no bd was executed>"
		}
		t.Fatal(err)
	}
	return string(data)
}

// writeCustomTypesStateBdStub is a pinned bd that answers the config row with
// row and `types --json` with table, and records every call.
func writeCustomTypesStateBdStub(t *testing.T, dir, recorded, row string, table []string) string {
	t.Helper()
	path := filepath.Join(dir, "state-bd")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + recorded + "\n" +
		"case \"$1 $2\" in\n" +
		"  'config get') echo '{\"value\":\"" + row + "\"}' ;;\n" +
		"  'types --json') echo '{\"custom_types\":[\"" + strings.Join(table, "\",\"") + "\"]}' ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCustomTypesCheckFixPreservesTableOnlyTypes is the A3 guard for #6495.
// `bd config set types.custom` replaces the custom_types table wholesale, so a
// Fix that merged only the config row deleted every type the table alone
// carried -- the shape an upgraded legacy-managed store has once gc's start
// path rewrote the row. Fix must merge row ∪ table ∪ required.
func TestCustomTypesCheckFixPreservesTableOnlyTypes(t *testing.T) {
	dir := guardedTempDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	v142 := RequiredCustomTypes[:len(RequiredCustomTypes)-1] // everything but startup-health-episode
	if RequiredCustomTypes[len(RequiredCustomTypes)-1] != "startup-health-episode" {
		t.Fatalf("test assumes startup-health-episode is the last required type: %v", RequiredCustomTypes)
	}
	table := append(append([]string{}, v142...), "ops-extra")
	recorded := filepath.Join(dir, "bd-calls")
	pinned := writeCustomTypesStateBdStub(t, dir, recorded, strings.Join(RequiredCustomTypes, ","), table)

	c := NewCustomTypesCheck(dir, "test", pinned)
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError || !strings.Contains(r.Message, "startup-health-episode") {
		t.Fatalf("Run = %v %q, want an error naming the table-missing startup-health-episode", r.Status, r.Message)
	}
	if err := c.Fix(&CheckContext{CityPath: dir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	var set string
	for _, line := range strings.Split(readRecordedBdCalls(t, recorded), "\n") {
		if v, ok := strings.CutPrefix(line, "config set types.custom "); ok {
			set = v
		}
	}
	want := strings.Join(append(append([]string{}, RequiredCustomTypes...), "ops-extra"), ",")
	if set != want {
		t.Fatalf("Fix wrote types.custom = %q, want %q (table-only ops-extra must survive)", set, want)
	}
}

// TestCustomTypesCheckFixRefusesWithoutTheTable: a Fix that cannot read the
// custom_types table cannot prove its write is a superset, so it must fail
// rather than risk deleting table-only types.
func TestCustomTypesCheckFixRefusesWithoutTheTable(t *testing.T) {
	dir := guardedTempDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(dir, "bd-calls")
	path := filepath.Join(dir, "no-types-bd")
	script := "#!/bin/sh\necho \"$@\" >> " + recorded + "\n" +
		"case \"$1 $2\" in\n  'config get') echo '{\"value\":\"molecule\"}' ;;\n  'types --json') exit 1 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	c := NewCustomTypesCheck(dir, "test", path)
	c.missing = []string{"convoy"}
	if err := c.Fix(&CheckContext{CityPath: dir}); err == nil {
		t.Fatal("Fix succeeded without reading custom_types")
	}
	if calls := readRecordedBdCalls(t, recorded); strings.Contains(calls, "config set") {
		t.Fatalf("Fix wrote types.custom without reading custom_types; calls:\n%s", calls)
	}
}
