package convergence

import "testing"

func exitCode(code int) *int { return &code }

func TestClassifyInfraBlind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		result     GateResult
		wantReason string
		wantBlind  bool
	}{
		{
			name:       "tempfail exit code",
			result:     GateResult{Outcome: GateFail, ExitCode: exitCode(GateInfraExitCode)},
			wantReason: "exit_75",
			wantBlind:  true,
		},
		{
			name:      "ordinary failure",
			result:    GateResult{Outcome: GateFail, ExitCode: exitCode(1), Stderr: "review verdict is reject"},
			wantBlind: false,
		},
		{
			// internal/beads/factory.go logs this and then falls back to bd,
			// so the read succeeds. A gate that prints it and fails has failed
			// for its own reason; re-running it burns the infra budget and
			// delays a real verdict.
			name:      "native store unavailable is a recovered path, not blindness",
			result:    GateResult{Outcome: GateFail, ExitCode: exitCode(1), Stderr: "2026/09/13 WARN native_store_unavailable scope=/city\n"},
			wantBlind: false,
		},
		{
			// "gc import install" is the remediation every pack-cache error
			// carries and cmd/gc/cmd_import.go's own error prefix; the typed
			// blindness it stood in for is "locked but not cached".
			name:      "gc import install failing is a verdict, not blindness",
			result:    GateResult{Outcome: GateFail, ExitCode: exitCode(1), Stderr: `gc import install: pack "core" failed checksum verification`},
			wantBlind: false,
		},
		{
			name:       "uncached pack import marker",
			result:     GateResult{Outcome: GateFail, ExitCode: exitCode(2), Stderr: `remote import x is locked but not cached at /c; run "gc import install"`},
			wantReason: "locked but not cached",
			wantBlind:  true,
		},
		{
			name:       "credential refusal marker",
			result:     GateResult{Outcome: GateFail, ExitCode: exitCode(1), Stderr: "Error 1045 (28000): Access denied for user 'root'@'localhost'"},
			wantReason: "error 1045",
			wantBlind:  true,
		},
		{
			name:      "passing gate is never blind",
			result:    GateResult{Outcome: GatePass, ExitCode: exitCode(0), Stderr: "Error 1045 (28000): Access denied for user 'root'@'localhost'"},
			wantBlind: false,
		},
		{
			name:      "already an infra outcome",
			result:    GateResult{Outcome: GateError, Stderr: "Error 1045 (28000): Access denied for user 'root'@'localhost'"},
			wantBlind: false,
		},
		{
			name:      "marker on stdout only",
			result:    GateResult{Outcome: GateFail, ExitCode: exitCode(1), Stdout: "Error 1045 (28000): Access denied for user 'root'@'localhost'"},
			wantBlind: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reason, blind := ClassifyInfraBlind(tc.result)
			if blind != tc.wantBlind {
				t.Fatalf("ClassifyInfraBlind blind = %v, want %v", blind, tc.wantBlind)
			}
			if reason != tc.wantReason {
				t.Fatalf("ClassifyInfraBlind reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestIsGateConfigError pins that only a completed run exiting 78 is a
// configuration error: the 75 protocol, ordinary verdicts, passes, and results
// that never produced an exit status are all something else.
func TestIsGateConfigError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		result GateResult
		want   bool
	}{
		{
			name:   "config exit code",
			result: GateResult{Outcome: GateFail, ExitCode: exitCode(GateConfigExitCode), Stderr: "python3 cannot import yaml"},
			want:   true,
		},
		{
			name:   "config exit code with no stderr",
			result: GateResult{Outcome: GateFail, ExitCode: exitCode(GateConfigExitCode)},
			want:   true,
		},
		{
			name:   "tempfail is an infrastructure outcome, not a config error",
			result: GateResult{Outcome: GateFail, ExitCode: exitCode(GateInfraExitCode)},
			want:   false,
		},
		{
			name:   "ordinary verdict",
			result: GateResult{Outcome: GateFail, ExitCode: exitCode(1), Stderr: "front matter missing required fields"},
			want:   false,
		},
		{
			name:   "passing gate is never a config error",
			result: GateResult{Outcome: GatePass, ExitCode: exitCode(0)},
			want:   false,
		},
		{
			name:   "gate error carries no exit status",
			result: GateResult{Outcome: GateError, Stderr: "fork/exec: permission denied"},
			want:   false,
		},
		{
			name:   "gate error with a stale 78 is still a gate error",
			result: GateResult{Outcome: GateError, ExitCode: exitCode(GateConfigExitCode)},
			want:   false,
		},
		{
			name:   "timeout is never a config error",
			result: GateResult{Outcome: GateTimeout, ExitCode: exitCode(GateConfigExitCode)},
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsGateConfigError(tc.result); got != tc.want {
				t.Fatalf("IsGateConfigError = %v, want %v", got, tc.want)
			}
		})
	}

	// The two protocols are disjoint: 78 must not also be reclassified as a
	// blind read, or the dispatcher would have two lanes claiming one result.
	if reason, blind := ClassifyInfraBlind(GateResult{Outcome: GateFail, ExitCode: exitCode(GateConfigExitCode)}); blind {
		t.Fatalf("ClassifyInfraBlind treated exit 78 as blind (reason %q)", reason)
	}
}
