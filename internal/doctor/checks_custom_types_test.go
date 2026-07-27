package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestCustomTypesCheck_NoBeadsDir(t *testing.T) {
	dir := t.TempDir()
	c := NewCustomTypesCheck(dir, "test")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (no .beads dir)", r.Status)
	}
}

func TestCustomTypesCheck_MissingTypes(t *testing.T) {
	// Scrub inherited beads env so the `bd config get` subprocess below
	// resolves to the empty .beads/ in the temp dir instead of an outer
	// gc city's beads database. Without this, bd can reach a live dolt
	// server (via GC_BEADS=bd + BEADS_DOLT_SERVER_PORT), reports all
	// required types as present, and the check returns StatusOK —
	// defeating the assertion. Clearing GC_BEADS and the dolt connection
	// vars prevents bd from connecting even if testenv.init() has not
	// yet added them to its LeakVectorVars scrub list.
	for _, key := range []string{
		"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT",
		"GC_BEADS", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST",
	} {
		t.Setenv(key, "")
	}

	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	c := NewCustomTypesCheck(dir, "test")
	// This will fail because bd isn't initialized in the temp dir.
	// The check should report a warning (can't read config).
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status == StatusOK {
		t.Fatal("expected non-OK status when bd config fails")
	}
	if !c.CanFix() {
		t.Fatal("CanFix should return true")
	}
}

// TestCustomTypesCheck_TableDrift proves detect+heal of the bug this bead
// fixes: config.yaml's types.custom CSV can list a type (e.g. "step") that
// the normalized custom_types TABLE doesn't have a row for. bd's create
// validation reads the TABLE, not the CSV, so a store in this state rejects
// `bd create --type step ...` with "invalid issue type: step" even though
// `bd config get types.custom` reports the type present. This drift happens
// on stores an older bd wrote (or where the table row was dropped some
// other way) — bd itself keeps CSV and table in sync on `bd config set`,
// but nothing previously re-ran that set for existing stores.
//
// The test manufactures the drift directly (delete the table row via the
// dolt CLI) rather than depending on an old bd binary, then asserts Run
// catches it — even though the CSV alone is complete — and Fix heals it by
// re-running `bd config set types.custom <merged>`, which reinserts the
// missing table row as a side effect of bd's own set-path.
func TestCustomTypesCheck_TableDrift(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd binary not on PATH")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}

	// Scrub inherited beads env so the bd subprocesses below resolve to the
	// throwaway store in the temp dir instead of an outer gc city's beads
	// database. See TestCustomTypesCheck_MissingTypes for why each var
	// matters.
	for _, key := range []string{
		"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT",
		"GC_BEADS", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST",
	} {
		t.Setenv(key, "")
	}

	dir := t.TempDir()

	runBD := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("bd", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	runBD("init", "--non-interactive", "-p", "tst", "--skip-hooks", "--skip-agents")
	runBD("config", "set", "types.custom", strings.Join(RequiredCustomTypes, ","))

	// Locate the embedded dolt DB directory the same way production code
	// does (internal/beads.(*BdStore).embeddedDoltDir), rather than
	// hand-deriving the sanitized database name from the prefix.
	metadataPath := filepath.Join(dir, ".beads", "metadata.json")
	dbName, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, metadataPath)
	if err != nil || !ok {
		t.Fatalf("ReadDoltDatabase(%s): ok=%v err=%v", metadataPath, ok, err)
	}
	doltDir := filepath.Join(dir, ".beads", "embeddeddolt", dbName)

	// Manufacture table drift: delete the "step" row directly from the
	// custom_types table while leaving config.yaml's CSV untouched.
	deleteCmd := exec.Command("dolt", "sql", "-q", "delete from custom_types where name='step'")
	deleteCmd.Dir = doltDir
	if out, err := deleteCmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt sql delete: %v\n%s", err, out)
	}

	c := NewCustomTypesCheck(dir, "test")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Fatalf("Run status = %v, want StatusError (table drift); message=%q", r.Status, r.Message)
	}
	if len(c.missing) != 0 {
		t.Fatalf("c.missing = %v, want empty — the CSV is complete, only the table is drifted", c.missing)
	}
	if !slices.Contains(c.tableMissing, "step") {
		t.Fatalf("c.tableMissing = %v, want it to contain %q", c.tableMissing, "step")
	}

	if err := c.Fix(&CheckContext{CityPath: dir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	c2 := NewCustomTypesCheck(dir, "test")
	r2 := c2.Run(&CheckContext{CityPath: dir})
	if r2.Status != StatusOK {
		t.Fatalf("after Fix, Run status = %v, want StatusOK; message=%q", r2.Status, r2.Message)
	}

	out := runBD("create", "--type", "step", "drift healed check")
	if !strings.Contains(out, "Created issue") {
		t.Fatalf("bd create --type step failed after Fix, table still drifted: %s", out)
	}
}

func TestCustomTypesCheck_RequiredTypesIncludeSpec(t *testing.T) {
	found := false
	for _, typ := range RequiredCustomTypes {
		if typ == "spec" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("RequiredCustomTypes must include 'spec'")
	}
}

// TestCustomTypesCheck_RequiredTypesIncludeConvergence verifies that
// "convergence" is in the required list. gc's convergence handler
// (internal/convergence/create.go) creates beads with Type="convergence"
// on every `gc converge create` call; if the type isn't registered in
// bd's types.custom, every convergence loop fails at creation with
// "invalid issue type: convergence".
func TestCustomTypesCheck_RequiredTypesIncludeConvergence(t *testing.T) {
	found := false
	for _, typ := range RequiredCustomTypes {
		if typ == "convergence" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("RequiredCustomTypes must include 'convergence' — gc's convergence handler requires this type")
	}
}

// TestMergeCustomTypes exercises the merge/dedup/preservation logic that
// backs CustomTypesCheck.Fix(). The regression it guards against is
// `--fix` overwriting user-defined types (which was the pre-PR behavior
// and still the failure mode if the merge is ever reverted).
func TestMergeCustomTypes(t *testing.T) {
	cases := []struct {
		name     string
		current  []string
		required []string
		want     []string
	}{
		{
			name:     "empty current gets required only",
			current:  nil,
			required: []string{"a", "b"},
			want:     []string{"a", "b"},
		},
		{
			name:     "preserves extra user types and appends missing required",
			current:  []string{"custom-foo", "molecule"},
			required: []string{"molecule", "spec", "convergence"},
			want:     []string{"custom-foo", "molecule", "spec", "convergence"},
		},
		{
			name:     "dedupes duplicates in current",
			current:  []string{"a", "a", "b", "a"},
			required: []string{"c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "drops empty and whitespace-only entries",
			current:  []string{"a", "", "  ", "b"},
			required: []string{"c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "trims whitespace around entries",
			current:  []string{" a ", "b\t"},
			required: []string{"a", "c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "dedupes when required entry already in current",
			current:  []string{"a", "b", "c"},
			required: []string{"b", "c", "d"},
			want:     []string{"a", "b", "c", "d"},
		},
		{
			name:     "preserves order of current entries",
			current:  []string{"z", "y", "x"},
			required: []string{"a"},
			want:     []string{"z", "y", "x", "a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contract.MergeCustomTypes(tc.current, tc.required)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("contract.MergeCustomTypes(%v, %v) = %v, want %v",
					tc.current, tc.required, got, tc.want)
			}
		})
	}
}

// TestParseCustomTypesJSON guards against the regression where
// `bd config get types.custom` on a store with an unset key returns
// "types.custom (not set)" and the old parser would persist that
// string as a fake custom type when Fix() merges. Switching to
// --json (+ this parser) eliminates the sentinel.
func TestParseCustomTypesJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:  "unset key returns nil",
			input: `{"key":"types.custom","value":""}`,
			want:  nil,
		},
		{
			name:  "whitespace-only value returns nil",
			input: `{"key":"types.custom","value":"   "}`,
			want:  nil,
		},
		{
			name:  "populated value splits on comma",
			input: `{"key":"types.custom","value":"molecule,spec,convergence"}`,
			want:  []string{"molecule", "spec", "convergence"},
		},
		{
			name:    "malformed JSON errors",
			input:   `not json`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCustomTypesJSON([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (result=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseCustomTypesJSON(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestTypesNotIn exercises the set-difference helper shared by the CSV
// completeness check and the custom_types table drift check.
func TestTypesNotIn(t *testing.T) {
	cases := []struct {
		name string
		want []string
		have []string
		out  []string
	}{
		{
			name: "nothing missing",
			want: []string{"a", "b"},
			have: []string{"a", "b", "c"},
			out:  nil,
		},
		{
			name: "some missing, preserves want order",
			want: []string{"a", "b", "c"},
			have: []string{"b"},
			out:  []string{"a", "c"},
		},
		{
			name: "everything missing when have is empty",
			want: []string{"a", "b"},
			have: nil,
			out:  []string{"a", "b"},
		},
		{
			name: "trims whitespace before comparing",
			want: []string{"a"},
			have: []string{" a "},
			out:  nil,
		},
		{
			name: "empty want yields nil regardless of have",
			want: nil,
			have: []string{"a"},
			out:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := typesNotIn(tc.want, tc.have)
			if !reflect.DeepEqual(got, tc.out) {
				t.Errorf("typesNotIn(%v, %v) = %v, want %v", tc.want, tc.have, got, tc.out)
			}
		})
	}
}

func TestCustomTypesCheck_RequiredTypesComplete(t *testing.T) {
	expected := map[string]bool{
		"molecule": true, "convoy": true, "message": true,
		"event": true, "gate": true, "merge-request": true,
		"agent": true, "role": true, "rig": true,
		"session": true, "spec": true, "convergence": true,
		"step": true,
	}
	for _, typ := range RequiredCustomTypes {
		if !expected[typ] {
			t.Errorf("unexpected required type: %q", typ)
		}
		delete(expected, typ)
	}
	for typ := range expected {
		t.Errorf("missing required type: %q", typ)
	}
}
