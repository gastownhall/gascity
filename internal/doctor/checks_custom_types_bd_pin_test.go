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
