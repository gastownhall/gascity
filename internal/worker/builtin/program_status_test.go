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
// declared (design 8.2). Every other builtin is undeclared.
//
// Families left undeclared, and why (re-verify before declaring one):
//   - opencode, cerebras, groq: opencode 1.x (npm opencode-ai, the default
//     install) emits nothing; 2.x (@opencode/cli) reports ungated as app
//     "opencode" but rejects the --model flag these profiles render.
//   - kimi: Kimi Code 2.4+ honors PI_PROGRAM_STATUS=1 (app "kimi-code") but
//     rejects this profile's --no-thinking; the kimi it does launch has no
//     OSC 7501.
//   - antigravity: agy 1.3.6+ reports with AGY_CLI_OSC7501=on, but its turn
//     reports need a Google sign-in, so turn start/end were not observed.
//   - amp: probe-gated with no override; its turns need Amp's hosted
//     user-actor service, so nothing past the probe was observed under the
//     relay.
var wantBuiltinProgramStatus = map[string]builtinProgramStatus{
	// codex-cli 0.165.0 reports unprompted (tap only).
	"codex": {Protocol: "osc7501", App: "codex"},
	// Claude Code 2.1.311 reports only once its support probe is answered,
	// so it runs under the relay.
	"claude": {Protocol: "osc7501", App: "claude-code", Relay: true},
	// pi 1.4.0 reports only after a 7501 reply, which tmux never sends;
	// PI_PROGRAM_STATUS=1 forces reports on and skips the probe.
	"pi": {Protocol: "osc7501", App: "pi", Env: map[string]string{"PI_PROGRAM_STATUS": "1"}},
	// omp 18.14.1 reports on any TTY (terminal.programStatus defaults on).
	"omp": {Protocol: "osc7501", App: "omp"},
	// grok 1.0.70 sends no probe inside tmux; REX_SESSION turns its default
	// auto mode on there (tmux DCS passthrough-wrapped reports).
	"grok": {Protocol: "osc7501", App: "grok-build", Env: map[string]string{"REX_SESSION": "1"}},
}

// Kills a declaration added, dropped or changed without its verification, a
// family declared under another program's app name, and a forced env that
// is also launch env.
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
		// The forced env must not also be launch env: profile Env is hashed
		// into the launch fingerprint and would relaunch the fleet.
		for k := range spec.ProgramStatusEnv {
			if _, dup := spec.Env[k]; dup {
				t.Errorf("%s: program-status env %q is also in the launch Env", name, k)
			}
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

// Kills a BuiltinProviders that hands out the catalog's own program-status
// env map.
func TestBuiltinProvidersCloneProgramStatusEnv(t *testing.T) {
	a := BuiltinProviders()
	a["pi"].ProgramStatusEnv["PI_PROGRAM_STATUS"] = "mutated"
	if got := BuiltinProviders()["pi"].ProgramStatusEnv["PI_PROGRAM_STATUS"]; got != "1" {
		t.Fatalf("pi PI_PROGRAM_STATUS = %q after mutating a returned copy, want 1", got)
	}
}
