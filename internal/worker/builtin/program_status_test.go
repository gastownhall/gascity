package builtin

import (
	"maps"
	"testing"
)

// Kills a declaration added without a verified capture, one dropped, or one
// whose app, relay or forced env drifts from what the capture was taken
// with. Each declared family was run under a scratch tmux socket against a
// local mock API, its pane output captured with pipe-pane -O (ga-f4ovjm.38).
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
func TestBuiltinProgramStatusDeclarations(t *testing.T) {
	type declaration struct {
		app   string
		relay bool
		env   map[string]string
	}
	want := map[string]declaration{
		// pi 1.4.0 reports only after a 7501 reply, which tmux never sends;
		// PI_PROGRAM_STATUS=1 forces reports on and skips the probe.
		"pi": {app: "pi", env: map[string]string{"PI_PROGRAM_STATUS": "1"}},
		// omp 18.14.1 reports on any TTY (terminal.programStatus defaults on).
		"omp": {app: "omp"},
		// grok 1.0.70 sends no probe inside tmux; REX_SESSION turns its
		// default auto mode on there (tmux DCS passthrough-wrapped reports).
		"grok": {app: "grok-build", env: map[string]string{"REX_SESSION": "1"}},
	}
	for name, spec := range BuiltinProviders() {
		w, declared := want[name]
		if !declared {
			if spec.ProgramStatusProtocol != "" || spec.ProgramStatusApp != "" || spec.ProgramStatusRelay || len(spec.ProgramStatusEnv) != 0 {
				t.Errorf("%s: unverified program-status declaration: protocol=%q app=%q relay=%t env=%v",
					name, spec.ProgramStatusProtocol, spec.ProgramStatusApp, spec.ProgramStatusRelay, spec.ProgramStatusEnv)
			}
			continue
		}
		if spec.ProgramStatusProtocol != programStatusOSC7501 {
			t.Errorf("%s: protocol = %q, want %q", name, spec.ProgramStatusProtocol, programStatusOSC7501)
		}
		if spec.ProgramStatusApp != w.app {
			t.Errorf("%s: app = %q, want %q", name, spec.ProgramStatusApp, w.app)
		}
		if spec.ProgramStatusRelay != w.relay {
			t.Errorf("%s: relay = %t, want %t", name, spec.ProgramStatusRelay, w.relay)
		}
		if !maps.Equal(spec.ProgramStatusEnv, w.env) {
			t.Errorf("%s: env = %v, want %v", name, spec.ProgramStatusEnv, w.env)
		}
		// The forced env must not also be launch env: profile Env is hashed
		// into the launch fingerprint and would relaunch the fleet.
		for k := range spec.ProgramStatusEnv {
			if _, dup := spec.Env[k]; dup {
				t.Errorf("%s: program-status env %q is also in the launch Env", name, k)
			}
		}
	}
	for name := range want {
		if _, ok := BuiltinProviders()[name]; !ok {
			t.Errorf("declared family %q is not a builtin provider", name)
		}
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
