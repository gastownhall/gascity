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
			name:       "native store unavailable marker",
			result:     GateResult{Outcome: GateFail, ExitCode: exitCode(1), Stderr: "2026/09/13 WARN native_store_unavailable scope=/city\n"},
			wantReason: "native_store_unavailable",
			wantBlind:  true,
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
			result:    GateResult{Outcome: GatePass, ExitCode: exitCode(0), Stderr: "WARN native_store_unavailable"},
			wantBlind: false,
		},
		{
			name:      "already an infra outcome",
			result:    GateResult{Outcome: GateError, Stderr: "native_store_unavailable"},
			wantBlind: false,
		},
		{
			name:      "marker on stdout only",
			result:    GateResult{Outcome: GateFail, ExitCode: exitCode(1), Stdout: "native_store_unavailable"},
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
