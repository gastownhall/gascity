package censusgrowthcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testpolicy/resourcecensus"
)

func policyFile(expires string) string {
	return `version = 3

[[audit_baseline]]
scope = "all"
resource = "subprocess"
reported_calls = 1
reported_files = 1
owner_bead = "ga-owner"
invariant = "inv"
resource_owner = "owner"
migration_target = "P0"
expires = "` + expires + `"
`
}

func countsFile(calls int) string {
	return resourcecensus.RenderCounts([]resourcecensus.CountRecord{
		{Path: "a/a_test.go", Resource: resourcecensus.ResourceSubprocess, Calls: calls},
	})
}

// fixture writes the four files into a temp dir and returns their flag form.
type fixture struct {
	dir                    string
	baseCounts, headCounts string
	basePolicy, headPolicy string
}

func newFixture(t *testing.T, baseCalls, headCalls int, headExpires string) fixture {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return fixture{
		dir:        dir,
		baseCounts: write("base-counts.toml", countsFile(baseCalls)),
		headCounts: write("head-counts.toml", countsFile(headCalls)),
		basePolicy: write("base-policy.toml", policyFile("2026-12-31")),
		headPolicy: write("head-policy.toml", policyFile(headExpires)),
	}
}

func (f fixture) args(extra ...string) []string {
	return append(append([]string{}, extra...),
		"--base-counts", f.baseCounts, "--head-counts", f.headCounts,
		"--base-policy", f.basePolicy, "--head-policy", f.headPolicy)
}

func run(t *testing.T, args []string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRunEnforceApprovedMatrix(t *testing.T) {
	const same, later = "2026-12-31", "2027-06-30"
	cases := []struct {
		name      string
		baseCalls int
		headCalls int
		expires   string // head policy expires
		extra     []string
		wantZero  bool
		wantFlag  string // substring expected on stdout, "" for none
	}{
		{"report-only with growth", 1, 2, same, nil, true, "GROWTH"},
		{"report-only with a policy-row change", 1, 1, later, nil, true, "POLICY-ROW"},
		{"report-only ignores --approved", 1, 2, same, []string{"--approved"}, true, "GROWTH"},
		{"enforce with growth, not approved", 1, 2, same, []string{"--enforce"}, false, "GROWTH"},
		{"enforce with growth, approved", 1, 2, same, []string{"--enforce", "--approved"}, true, "GROWTH"},
		{"enforce with a policy-row change, not approved", 1, 1, later, []string{"--enforce"}, false, "POLICY-ROW"},
		{"enforce with a policy-row change, approved", 1, 1, later, []string{"--enforce", "--approved"}, true, "POLICY-ROW"},
		{"enforce with both flags, not approved", 1, 2, later, []string{"--enforce"}, false, "GROWTH"},
		{"enforce with no growth", 2, 1, same, []string{"--enforce"}, true, ""},
		{"enforce with nothing changed", 1, 1, same, []string{"--enforce"}, true, ""},
		{"approved alone with nothing changed", 1, 1, same, []string{"--approved"}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.baseCalls, tc.headCalls, tc.expires)
			code, stdout, stderr := run(t, f.args(tc.extra...))
			if tc.wantZero && code != 0 {
				t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if !tc.wantZero {
				if code == 0 || code == 2 {
					t.Fatalf("exit = %d, want a non-zero blocked status that is not the usage code 2\nstderr:\n%s", code, stderr)
				}
				if !strings.Contains(stderr, "--approved") {
					t.Errorf("a blocked run must say how to approve it; stderr:\n%s", stderr)
				}
			}
			if tc.wantFlag != "" && !strings.Contains(stdout, tc.wantFlag) {
				t.Errorf("stdout lacks %s:\n%s", tc.wantFlag, stdout)
			}
			if tc.wantFlag == "" && (strings.Contains(stdout, "GROWTH") || strings.Contains(stdout, "POLICY-ROW")) {
				t.Errorf("stdout carries an unexpected flag:\n%s", stdout)
			}
			if !strings.Contains(stdout, "census") && !strings.Contains(stdout, "Census") {
				t.Errorf("the report must go to stdout:\n%s", stdout)
			}
		})
	}
}

func TestRunBaseWithoutCountsFileIsNoDeltaAndExitsZero(t *testing.T) {
	f := newFixture(t, 1, 2, "2027-06-30")
	for _, name := range []string{"missing", "empty"} {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "missing":
				if err := os.Remove(f.baseCounts); err != nil {
					t.Fatal(err)
				}
			case "empty":
				if err := os.WriteFile(f.baseCounts, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// The base policy file is absent too: the base predates the ledger.
			if err := os.Remove(f.basePolicy); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			for _, extra := range [][]string{nil, {"--enforce"}} {
				code, stdout, stderr := run(t, f.args(extra...))
				if code != 0 {
					t.Fatalf("exit = %d with args %v, want 0\nstderr:\n%s", code, extra, stderr)
				}
				if !strings.Contains(stdout, "base predates the counts file: no delta available") {
					t.Errorf("stdout lacks the no-delta line:\n%s", stdout)
				}
			}
		})
	}
}

func TestRunHeadWithoutCountsFileIsAnError(t *testing.T) {
	f := newFixture(t, 1, 1, "2026-12-31")
	if err := os.Remove(f.headCounts); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--enforce", "--approved"}} {
		code, _, stderr := run(t, f.args(extra...))
		if code != 1 {
			t.Errorf("exit = %d with args %v, want 1", code, extra)
		}
		if !strings.Contains(stderr, resourcecensus.CountsPath) && !strings.Contains(stderr, "head") {
			t.Errorf("stderr should name the missing head file: %q", stderr)
		}
	}
}

func TestRunUsageErrorsExitTwo(t *testing.T) {
	f := newFixture(t, 1, 1, "2026-12-31")
	cases := map[string][]string{
		"no arguments":            nil,
		"a missing required flag": {"--base-counts", f.baseCounts, "--head-counts", f.headCounts, "--base-policy", f.basePolicy},
		"a positional argument":   append(f.args(), "extra"),
		"an unknown flag":         f.args("--surprise"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := run(t, args)
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if stdout != "" {
				t.Errorf("a usage error must not write a report: %q", stdout)
			}
			if stderr == "" {
				t.Error("a usage error must explain itself on stderr")
			}
		})
	}
}

func TestRunMalformedInputExitsOne(t *testing.T) {
	f := newFixture(t, 1, 1, "2026-12-31")
	if err := os.WriteFile(f.headPolicy, []byte("bogus = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, f.args())
	if code != 1 || stdout != "" || stderr == "" {
		t.Errorf("exit=%d stdout=%q stderr=%q, want 1, no report, an explanation", code, stdout, stderr)
	}
}
