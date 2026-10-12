package acceptancehelpers

import (
	"fmt"
	"strings"
	"testing"
)

func TestRequireSwitchOnReadsTheEnvironment(t *testing.T) {
	const name = "GC_TEST_REQUIRE_SWITCH"
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: " ", want: false},
		{value: "0", want: false},
		{value: "1", want: true},
		{value: "true", want: true},
		{value: " 1 ", want: true},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(name, tc.value)
			if got := requireSwitchOn(name); got != tc.want {
				t.Fatalf("requireSwitchOn(%q=%q) = %t, want %t", name, tc.value, got, tc.want)
			}
		})
	}
}

// recordingSkipOrFailer records which of Skip and Fatalf skipOrFail chose.
type recordingSkipOrFailer struct {
	skipped string
	fatal   string
}

func (r *recordingSkipOrFailer) Helper() {}

func (r *recordingSkipOrFailer) Skip(args ...any) { r.skipped = fmt.Sprint(args...) }

func (r *recordingSkipOrFailer) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
}

// TestMissingPreconditionFailsWhereTheRowsMustRun pins the contract the
// proxied-native rows rely on (round4 missed low): under the require switch a
// missing precondition is a FAILURE, and only without it a skip. The required
// Beads / proxied-native acceptance job sets the switch, so an in-row skip can
// no longer turn that job green without running the row.
func TestMissingPreconditionFailsWhereTheRowsMustRun(t *testing.T) {
	const reason = "bd removed its record on SIGKILL"
	for _, tc := range []struct {
		value    string
		wantFail bool
	}{
		{value: "", wantFail: false},
		{value: "0", wantFail: false},
		{value: "1", wantFail: true},
	} {
		t.Run(EnvRequireTooling+"="+tc.value, func(t *testing.T) {
			t.Setenv(EnvRequireTooling, tc.value)
			var rec recordingSkipOrFailer
			missingPrecondition(&rec, "%s", reason)
			if tc.wantFail {
				if rec.fatal == "" || rec.skipped != "" {
					t.Fatalf("under %s=%q the row skipped (%q) instead of failing (%q)", EnvRequireTooling, tc.value, rec.skipped, rec.fatal)
				}
				if !strings.Contains(rec.fatal, reason) || !strings.Contains(rec.fatal, EnvRequireTooling) {
					t.Fatalf("the failure %q does not name the switch and the missing precondition", rec.fatal)
				}
				return
			}
			if rec.skipped != reason || rec.fatal != "" {
				t.Fatalf("without the switch the row failed (%q) instead of skipping (%q)", rec.fatal, rec.skipped)
			}
		})
	}
}
