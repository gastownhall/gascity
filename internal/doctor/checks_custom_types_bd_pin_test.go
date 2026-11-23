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
	// The list gc v1.4.2 registered: it predates startup-health-episode.
	v142 := []string{
		"molecule", "convoy", "message", "event", "gate",
		"merge-request", "agent", "role", "rig", "session", "spec",
		"convergence", "step",
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

// TestCustomTypesCheckMissingTypeHintNamesTheExactFix: a store an older gc
// registered lacks a newer required type (startup-health-episode), and no boot
// path registers it on a store gc does not provision (an external Dolt server
// or an HTTP binding). doctor is the repair, so its failure must name the exact
// one-shot command, scoped to this check, and say it only adds types: an
// operator about to run --fix against a production store needs to know it
// touches nothing else.
func TestCustomTypesCheckMissingTypeHintNamesTheExactFix(t *testing.T) {
	dir := guardedTempDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	older := RequiredCustomTypes[:len(RequiredCustomTypes)-1]
	recorded := filepath.Join(dir, "bd-calls")
	pinned := writeCustomTypesStateBdStub(t, dir, recorded, strings.Join(older, ","), older)

	c := NewCustomTypesCheck(dir, "city", pinned)
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Fatalf("Run = %v %q, want an error", r.Status, r.Message)
	}
	for _, want := range []string{"gc doctor --fix --check custom-types:city", "additive"} {
		if !strings.Contains(r.FixHint, want) {
			t.Errorf("FixHint = %q, want it to contain %q", r.FixHint, want)
		}
	}
	if calls := readRecordedBdCalls(t, recorded); strings.Contains(calls, "config set") {
		t.Fatalf("Run wrote types.custom; calls:\n%s", calls)
	}
}

// TestCustomTypesCheckReadFailureHintNamesTheScopedFix: when bd cannot read the
// types.custom row or the custom_types table, the warning's hint names the same
// scoped repair as the missing-type failure, and says the repair waits on the
// read: Fix re-reads both and writes nothing while either fails, so an operator
// told to run it straight away would see it fail the same way.
func TestCustomTypesCheckReadFailureHintNamesTheScopedFix(t *testing.T) {
	for _, tc := range []struct {
		unread  string
		failing string
	}{
		{"the types.custom row", "config get"},
		{"the custom_types table", "types --json"},
	} {
		t.Run(tc.unread, func(t *testing.T) {
			dir := guardedTempDir(t)
			if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}
			recorded := filepath.Join(dir, "bd-calls")
			pinned := writeFailingReadBdStub(t, dir, recorded, tc.failing)

			c := NewCustomTypesCheck(dir, "city", pinned)
			r := c.Run(&CheckContext{CityPath: dir})
			if r.Status != StatusWarning {
				t.Fatalf("Run = %v %q, want a warning for %s bd could not read", r.Status, r.Message, tc.unread)
			}
			for _, want := range []string{"gc doctor --fix --check custom-types:city", "writes nothing while"} {
				if !strings.Contains(r.FixHint, want) {
					t.Errorf("FixHint = %q, want it to contain %q", r.FixHint, want)
				}
			}
			if calls := readRecordedBdCalls(t, recorded); strings.Contains(calls, "config set") {
				t.Fatalf("Run wrote types.custom; calls:\n%s", calls)
			}
		})
	}
}

// TestCustomTypesCheckHintQuotesTheCheckName: a rig's check name carries the
// rig name, which defaults to the rig directory's base name and so can hold a
// space. Both hints are commands an operator pastes, so the name is
// shell-quoted and the pasted command still selects this one check.
func TestCustomTypesCheckHintQuotesTheCheckName(t *testing.T) {
	dir := guardedTempDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(dir, "bd-calls")
	older := RequiredCustomTypes[:len(RequiredCustomTypes)-1]
	for name, bd := range map[string]string{
		"a missing type": writeCustomTypesStateBdStub(t, dir, recorded, strings.Join(older, ","), older),
		"a failed read":  writeFailingReadBdStub(t, dir, recorded, "types --json"),
	} {
		r := NewCustomTypesCheck(dir, "my rig", bd).Run(&CheckContext{CityPath: dir})
		if want := "gc doctor --fix --check 'custom-types:my rig'"; !strings.Contains(r.FixHint, want) {
			t.Errorf("%s: FixHint = %q, want it to contain %q", name, r.FixHint, want)
		}
	}
}

// writeFailingReadBdStub is a pinned bd whose failing read ("$1 $2") exits
// non-zero, while the other read answers with every required type, and which
// records every call.
func writeFailingReadBdStub(t *testing.T, dir, recorded, failing string) string {
	t.Helper()
	path := filepath.Join(dir, "failing-bd")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + recorded + "\n" +
		"case \"$1 $2\" in\n" +
		"  '" + failing + "') echo 'bd: store unreachable' >&2; exit 1 ;;\n" +
		"  'config get') echo '{\"value\":\"" + strings.Join(RequiredCustomTypes, ",") + "\"}' ;;\n" +
		"  'types --json') echo '{\"custom_types\":[\"" + strings.Join(RequiredCustomTypes, "\",\"") + "\"]}' ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
