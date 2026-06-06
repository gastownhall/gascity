package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
	// Scrub inherited beads env (BEADS_DIR / BEADS_ACTOR /
	// GC_BEADS_SCOPE_ROOT) so the `bd config get` subprocess below
	// resolves to the empty .beads/ in the temp dir instead of an
	// outer gc city's beads database that a polecat/refinery worktree
	// passes down via the environment. Without this, bd finds the
	// outer city's store, reports all required types as present, and
	// the check returns StatusOK — defeating the test's assertion.
	// Same root cause class as ga-10qg / ga-766q; leak vector here is
	// BEADS_DIR into a bd subprocess.
	for _, key := range []string{"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT"} {
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
			got := mergeCustomTypes(tc.current, tc.required)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("mergeCustomTypes(%v, %v) = %v, want %v",
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
