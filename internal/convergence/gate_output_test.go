package convergence

import "testing"

// TestGateOutputFromMetadataAndCombinedOutput pins the read-side gate-output
// projection: full metadata populates every field; a nil map yields the zero
// value; HasOutput reflects stdout/stderr presence; and CombinedOutput matches
// the order-history detail handler's prior inline stdout/stderr join byte for
// byte.
func TestGateOutputFromMetadataAndCombinedOutput(t *testing.T) {
	full := map[string]string{
		FieldGateDurationMs: "1200",
		FieldGateExitCode:   "0",
		FieldGateStdout:     "out",
		FieldGateStderr:     "err",
	}
	g := GateOutputFromMetadata(full)
	if g.DurationMs != "1200" || g.ExitCode != "0" || g.Stdout != "out" || g.Stderr != "err" {
		t.Fatalf("GateOutputFromMetadata = %+v, want all four fields populated", g)
	}
	if !g.HasOutput() {
		t.Errorf("HasOutput = false, want true")
	}
	if got := g.CombinedOutput(); got != "out\nerr" {
		t.Errorf("CombinedOutput = %q, want %q", got, "out\nerr")
	}

	zero := GateOutputFromMetadata(nil)
	if zero != (GateOutput{}) {
		t.Errorf("GateOutputFromMetadata(nil) = %+v, want zero value", zero)
	}
	if zero.HasOutput() {
		t.Errorf("HasOutput(nil) = true, want false")
	}
	if got := zero.CombinedOutput(); got != "" {
		t.Errorf("CombinedOutput(nil) = %q, want empty", got)
	}

	cases := []struct {
		name          string
		stdout        string
		stderr        string
		wantHasOutput bool
		wantCombined  string
	}{
		{"both", "out", "err", true, "out\nerr"},
		{"stdout only", "out", "", true, "out"},
		{"stderr only", "", "err", true, "err"},
		{"neither", "", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := GateOutputFromMetadata(map[string]string{
				FieldGateStdout: tc.stdout,
				FieldGateStderr: tc.stderr,
			})
			if got.HasOutput() != tc.wantHasOutput {
				t.Errorf("HasOutput = %v, want %v", got.HasOutput(), tc.wantHasOutput)
			}
			if out := got.CombinedOutput(); out != tc.wantCombined {
				t.Errorf("CombinedOutput = %q, want %q", out, tc.wantCombined)
			}
		})
	}
}
