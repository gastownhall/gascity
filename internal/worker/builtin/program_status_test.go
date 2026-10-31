package builtin

import (
	"maps"
	"testing"
)

// builtinProgramStatus is one builtin's flat program-status declaration.
type builtinProgramStatus struct {
	Protocol string
	App      string
	Relay    bool
	Env      map[string]string
}

// wantBuiltinProgramStatus is every builtin that declares OSC 7501 program
// status, each verified under gc's tmux against local mocks before it was
// declared (design 8.2): codex-cli 0.165.0 reports unprompted (tap only), and
// Claude Code 2.1.311 reports only once its support probe is answered, so it
// runs under the relay. Every other builtin is undeclared.
var wantBuiltinProgramStatus = map[string]builtinProgramStatus{
	"codex":  {Protocol: "osc7501", App: "codex"},
	"claude": {Protocol: "osc7501", App: "claude-code", Relay: true},
}

// Kills a declaration added, dropped or changed without its verification, and
// a family declared under another program's app name.
func TestBuiltinProgramStatusDeclarations(t *testing.T) {
	for name, spec := range BuiltinProviders() {
		got := builtinProgramStatus{
			Protocol: spec.ProgramStatusProtocol,
			App:      spec.ProgramStatusApp,
			Relay:    spec.ProgramStatusRelay,
			Env:      spec.ProgramStatusEnv,
		}
		want := wantBuiltinProgramStatus[name]
		if got.Protocol != want.Protocol || got.App != want.App || got.Relay != want.Relay || !maps.Equal(got.Env, want.Env) {
			t.Errorf("%s: program status = %+v, want %+v", name, got, want)
		}
	}
	for name := range wantBuiltinProgramStatus {
		if _, ok := BuiltinProviders()[name]; !ok {
			t.Errorf("declared builtin %q is not a builtin provider", name)
		}
	}
}

func TestCloneBuiltinProviderSpecClonesProgramStatusEnv(t *testing.T) {
	orig := BuiltinProviderSpec{ProgramStatusEnv: map[string]string{"K": "v"}}
	clone := cloneBuiltinProviderSpec(orig)
	clone.ProgramStatusEnv["K"] = "mutated"
	if orig.ProgramStatusEnv["K"] != "v" {
		t.Fatal("cloneBuiltinProviderSpec shares ProgramStatusEnv with its source")
	}
}
