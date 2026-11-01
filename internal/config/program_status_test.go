package config

import (
	"maps"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	workerbuiltin "github.com/gastownhall/gascity/internal/worker/builtin"
)

func declaredBinding() ProgramStatusBinding {
	relay := true
	return ProgramStatusBinding{
		Protocol: ProgramStatusProtocolOSC7501,
		App:      "codex",
		Relay:    &relay,
		Env:      map[string]string{"PI_PROGRAM_STATUS": "1", "WITHHELD": ""},
	}
}

func TestProgramStatusArmDerivation(t *testing.T) {
	declared := declaredBinding()
	cases := []struct {
		name      string
		binding   ProgramStatusBinding
		mode      runtime.ProgramStatusMode
		acp       bool
		wantArm   runtime.ProgramStatusArm
		wantEnv   map[string]string
		wantArmed bool
	}{
		{name: "off disarms", binding: declared, mode: runtime.ProgramStatusOff},
		{name: "unset mode reads as off", binding: declared, mode: ""},
		{name: "undeclared provider", binding: ProgramStatusBinding{App: "codex", Env: map[string]string{"X": "1"}}, mode: runtime.ProgramStatusOn},
		{name: "explicitly undeclared", binding: ProgramStatusBinding{Protocol: ProgramStatusProtocolNone, App: "codex"}, mode: runtime.ProgramStatusOn},
		{name: "acp transport", binding: declared, mode: runtime.ProgramStatusOn, acp: true},
		{name: "unknown mode disarms", binding: declared, mode: "bogus"},
		{
			name: "shadow arms", binding: declared, mode: runtime.ProgramStatusShadow, wantArmed: true,
			wantArm: runtime.ProgramStatusArm{Capture: true, App: "codex", Relay: true},
			wantEnv: map[string]string{"PI_PROGRAM_STATUS": "1", "WITHHELD": ""},
		},
		{
			name: "on arms without relay", binding: ProgramStatusBinding{Protocol: ProgramStatusProtocolOSC7501}, mode: runtime.ProgramStatusOn, wantArmed: true,
			wantArm: runtime.ProgramStatusArm{Capture: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arm, env := tc.binding.Arm(tc.mode, tc.acp)
			if arm != tc.wantArm {
				t.Errorf("arm = %+v, want %+v", arm, tc.wantArm)
			}
			if !maps.Equal(env, tc.wantEnv) {
				t.Errorf("env = %v, want %v", env, tc.wantEnv)
			}
			if !tc.wantArmed && env != nil {
				t.Errorf("disarmed derivation returned non-nil env %v", env)
			}
		})
	}
}

func TestProgramStatusArmEnvIsACopy(t *testing.T) {
	b := declaredBinding()
	_, env := b.Arm(runtime.ProgramStatusOn, false)
	env["PI_PROGRAM_STATUS"] = "mutated"
	if b.Env["PI_PROGRAM_STATUS"] != "1" {
		t.Fatal("Arm returned the binding's own env map")
	}
}

func TestProgramStatusArmForUsesCitySwitchAndTransport(t *testing.T) {
	resolved := &ResolvedProvider{Name: "codex", ProgramStatus: declaredBinding()}
	on := &City{Session: SessionConfig{ProgramStatus: "on"}}
	if arm, _ := ProgramStatusArmFor(on, resolved, ""); !arm.Capture {
		t.Errorf("tmux transport under on: arm = %+v, want capture", arm)
	}
	if arm, env := ProgramStatusArmFor(on, resolved, SessionTransportACP); arm.Capture || env != nil {
		t.Errorf("acp transport: arm = %+v env = %v, want disarmed", arm, env)
	}
	if arm, env := ProgramStatusArmFor(&City{}, resolved, ""); arm.Capture || env != nil {
		t.Errorf("default switch: arm = %+v env = %v, want disarmed", arm, env)
	}
	if arm, env := ProgramStatusArmFor(nil, resolved, ""); arm.Capture || env != nil {
		t.Errorf("nil city: arm = %+v env = %v, want disarmed", arm, env)
	}
	if arm, env := ProgramStatusArmFor(on, nil, ""); arm.Capture || env != nil {
		t.Errorf("nil provider: arm = %+v env = %v, want disarmed", arm, env)
	}
}

func TestSessionProgramStatusModeDefaultsOff(t *testing.T) {
	cases := map[string]runtime.ProgramStatusMode{
		"":       runtime.ProgramStatusOff,
		"off":    runtime.ProgramStatusOff,
		"shadow": runtime.ProgramStatusShadow,
		"on":     runtime.ProgramStatusOn,
	}
	for raw, want := range cases {
		s := SessionConfig{ProgramStatus: raw}
		if got := s.ProgramStatusMode(); got != want {
			t.Errorf("ProgramStatusMode(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseProgramStatusSwitch(t *testing.T) {
	for _, mode := range []string{"off", "shadow", "on"} {
		cfg, err := Parse([]byte("[session]\nprogram_status = \"" + mode + "\"\n"))
		if err != nil {
			t.Fatalf("Parse(%s): %v", mode, err)
		}
		if got := string(cfg.Session.ProgramStatusMode()); got != mode {
			t.Errorf("mode = %q, want %q", got, mode)
		}
	}
	cfg, err := Parse([]byte("[workspace]\nname = \"c\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Session.ProgramStatusMode(); got != runtime.ProgramStatusOff {
		t.Errorf("default mode = %q, want off", got)
	}
}

func TestParseRejectsInvalidProgramStatus(t *testing.T) {
	cases := map[string]string{
		"switch typo":      "[session]\nprogram_status = \"shadwo\"\n",
		"switch padded":    "[session]\nprogram_status = \" on\"\n",
		"switch uppercase": "[session]\nprogram_status = \"ON\"\n",
		"unknown protocol": "[providers.p]\ncommand = \"p\"\n[providers.p.program_status]\nprotocol = \"osc7502\"\n",
		"bad app":          "[providers.p]\ncommand = \"p\"\n[providers.p.program_status]\nprotocol = \"osc7501\"\napp = \"has space\"\n",
		"app over 32":      "[providers.p]\ncommand = \"p\"\n[providers.p.program_status]\nprotocol = \"osc7501\"\napp = \"" + strings.Repeat("a", 33) + "\"\n",
		"bad env key":      "[providers.p]\ncommand = \"p\"\n[providers.p.program_status]\nprotocol = \"osc7501\"\nenv = { \"1BAD\" = \"x\" }\n",
		"empty env key":    "[providers.p]\ncommand = \"p\"\n[providers.p.program_status]\nprotocol = \"osc7501\"\nenv = { \"\" = \"x\" }\n",
		"reserved env key": "[providers.p]\ncommand = \"p\"\n[providers.p.program_status]\nprotocol = \"osc7501\"\nenv = { GC_TEMPLATE = \"x\" }\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Fatalf("Parse accepted invalid program_status config:\n%s", doc)
			} else if !strings.Contains(err.Error(), "program_status") {
				t.Errorf("error %q does not name program_status", err)
			}
		})
	}
}

// TestProgramStatusAppIsTheCodecAppName pins load-time app validation to the
// codec's app name, which the leaf checks again before arming: a binding
// loads exactly when its app can be armed, so none loads and then runs
// unarmed.
func TestProgramStatusAppIsTheCodecAppName(t *testing.T) {
	for _, app := range []string{"codex", "claude-code", "a.b_c+d-e", "+x", "-x", ".x", "_x", strings.Repeat("a", 32)} {
		if !programstatus.ValidApp(app) {
			t.Fatalf("fixture %q is not a codec app name", app)
		}
		if err := validateProgramStatusBinding(ProgramStatusBinding{Protocol: ProgramStatusProtocolOSC7501, App: app}); err != nil {
			t.Errorf("app %q: err = %v, want accepted", app, err)
		}
	}
	for _, app := range []string{strings.Repeat("a", 33), "has space", "a/b", "a#b", "a%b", "a:b", "é"} {
		if programstatus.ValidApp(app) {
			t.Fatalf("fixture %q is a codec app name", app)
		}
		if err := validateProgramStatusBinding(ProgramStatusBinding{Protocol: ProgramStatusProtocolOSC7501, App: app}); err == nil || !strings.Contains(err.Error(), "app") {
			t.Errorf("app %q: err = %v, want an app error", app, err)
		}
	}
}

// TestProgramStatusEnvRejectsReservedKeys pins the namespaces gc writes into a
// session env itself: the identity and city anchors (GC_*, GT_ROOT), the
// beads scope and bd pins (BEADS_*, BD_*), and with them every key the launch
// fingerprint hashes. A binding naming one would override it and move the
// fingerprint its env is documented to stay off.
func TestProgramStatusEnvRejectsReservedKeys(t *testing.T) {
	for _, key := range []string{"GC_TEMPLATE", "GC_CITY", "GC_CONTROLLER_TOKEN", "BEADS_DIR", "BD_BACKUP_ENABLED", "GT_ROOT"} {
		err := validateProgramStatusBinding(ProgramStatusBinding{Protocol: ProgramStatusProtocolOSC7501, Env: map[string]string{key: "x"}})
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("env key %s: err = %v, want a reserved-key error naming it", key, err)
		}
	}
	for _, key := range []string{"PI_PROGRAM_STATUS", "PI_GC_MODE", "GT_ROOT_HINT", "gc_lowercase"} {
		if err := validateProgramStatusBinding(ProgramStatusBinding{Protocol: ProgramStatusProtocolOSC7501, Env: map[string]string{key: "x"}}); err != nil {
			t.Errorf("env key %s: err = %v, want accepted", key, err)
		}
	}
}

func TestParseProviderProgramStatusBinding(t *testing.T) {
	cfg, err := Parse([]byte(`
[providers.pi]
command = "pi"

[providers.pi.program_status]
protocol = "osc7501"
app = "pi"
relay = true
env = { PI_PROGRAM_STATUS = "1" }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := cfg.Providers["pi"].ProgramStatus
	if got.Protocol != "osc7501" || got.App != "pi" || got.Relay == nil || !*got.Relay || got.Env["PI_PROGRAM_STATUS"] != "1" {
		t.Fatalf("binding = %+v", got)
	}
}

func TestMergeProviderOverBuiltinMergesProgramStatus(t *testing.T) {
	relayTrue, relayFalse := true, false
	base := ProviderSpec{ProgramStatus: ProgramStatusBinding{
		Protocol: ProgramStatusProtocolOSC7501,
		App:      "claude-code",
		Relay:    &relayTrue,
		Env:      map[string]string{"A": "1", "B": "2"},
	}}
	child := ProviderSpec{ProgramStatus: ProgramStatusBinding{
		Relay: &relayFalse,
		Env:   map[string]string{"B": "", "C": "3"},
	}}
	merged := MergeProviderOverBuiltin(base, child)
	ps := merged.ProgramStatus
	if ps.Protocol != ProgramStatusProtocolOSC7501 || ps.App != "claude-code" {
		t.Errorf("scalars not inherited: %+v", ps)
	}
	if ps.Relay == nil || *ps.Relay {
		t.Errorf("relay = %v, want explicit false override", ps.Relay)
	}
	want := map[string]string{"A": "1", "B": "", "C": "3"}
	if !maps.Equal(ps.Env, want) {
		t.Errorf("env = %v, want %v (per-key, later wins, empty verbatim)", ps.Env, want)
	}
	if base.ProgramStatus.Env["B"] != "2" {
		t.Error("merge mutated the base binding env")
	}

	undeclare := MergeProviderOverBuiltin(base, ProviderSpec{ProgramStatus: ProgramStatusBinding{Protocol: ProgramStatusProtocolNone}})
	if undeclare.ProgramStatus.Declared() {
		t.Error(`protocol = "none" must undeclare an inherited declaration`)
	}
}

func TestProviderChainInheritsProgramStatus(t *testing.T) {
	providers := map[string]ProviderSpec{
		"parent": {
			Command: "agent",
			ProgramStatus: ProgramStatusBinding{
				Protocol: ProgramStatusProtocolOSC7501,
				App:      "agent",
				Env:      map[string]string{"A": "1"},
			},
		},
		"child": {
			Base:          strPtr("provider:parent"),
			ProgramStatus: ProgramStatusBinding{Env: map[string]string{"B": "2"}},
		},
	}
	agent := &Agent{Name: "a", Provider: "child"}
	resolved, err := ResolveProvider(agent, nil, providers, func(string) (string, error) { return "/bin/agent", nil })
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	ps := resolved.ProgramStatus
	if !ps.Declared() || ps.App != "agent" {
		t.Errorf("chain lost the parent declaration: %+v", ps)
	}
	if !maps.Equal(ps.Env, map[string]string{"A": "1", "B": "2"}) {
		t.Errorf("env = %v, want merged parent+child", ps.Env)
	}
}

func TestWorkspaceStartCommandDisarmsProgramStatus(t *testing.T) {
	providers := map[string]ProviderSpec{"p": {Command: "agent", ProgramStatus: declaredBinding()}}
	agent := &Agent{Name: "a", Provider: "p"}
	ws := &Workspace{StartCommand: "some-other-program"}
	resolved, err := ResolveProvider(agent, ws, providers, func(string) (string, error) { return "/bin/agent", nil })
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if resolved.ProgramStatus.Declared() {
		t.Error("workspace start_command replaces the program; the provider's declaration must not apply")
	}
}

func TestDeepCopyProgramStatusBinding(t *testing.T) {
	spec := ProviderSpec{ProgramStatus: declaredBinding()}
	dup := deepCopyProviderSpec(spec)
	dup.ProgramStatus.Env["PI_PROGRAM_STATUS"] = "x"
	*dup.ProgramStatus.Relay = false
	if spec.ProgramStatus.Env["PI_PROGRAM_STATUS"] != "1" || !*spec.ProgramStatus.Relay {
		t.Error("deepCopyProviderSpec aliases the program_status binding")
	}

	r := ResolvedProvider{ProgramStatus: declaredBinding()}
	rdup := deepCopyResolvedProvider(r)
	rdup.ProgramStatus.Env["PI_PROGRAM_STATUS"] = "x"
	*rdup.ProgramStatus.Relay = false
	if r.ProgramStatus.Env["PI_PROGRAM_STATUS"] != "1" || !*r.ProgramStatus.Relay {
		t.Error("deepCopyResolvedProvider aliases the program_status binding")
	}
}

func TestComposeFragmentMergesProgramStatusPerField(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["frag.toml"]

[workspace]
name = "c"

[providers.p]
command = "agent"

[providers.p.program_status]
protocol = "osc7501"
app = "agent"
env = { A = "1" }
`)
	fs.Files["/city/frag.toml"] = []byte(`
[providers.p.program_status]
relay = true
env = { B = "2" }
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	ps := cfg.Providers["p"].ProgramStatus
	if ps.Protocol != ProgramStatusProtocolOSC7501 || ps.App != "agent" {
		t.Errorf("fragment dropped base scalars: %+v", ps)
	}
	if ps.Relay == nil || !*ps.Relay {
		t.Errorf("relay = %v, want fragment true", ps.Relay)
	}
	if !maps.Equal(ps.Env, map[string]string{"A": "1", "B": "2"}) {
		t.Errorf("env = %v, want per-key merge", ps.Env)
	}
}

// TestComposeSessionFragmentKeepsProgramStatus pins that a fragment defining
// other [session] keys keeps the root's program_status switch, while a
// fragment that sets the switch itself, to "off" or "" included, still wins.
func TestComposeSessionFragmentKeepsProgramStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		frag string
		want runtime.ProgramStatusMode
	}{
		{"other key only", "[session]\nsocket = \"frag-socket\"\n", runtime.ProgramStatusShadow},
		{"sets on", "[session]\nsocket = \"frag-socket\"\nprogram_status = \"on\"\n", runtime.ProgramStatusOn},
		{"sets off", "[session]\nsocket = \"frag-socket\"\nprogram_status = \"off\"\n", runtime.ProgramStatusOff},
		{"sets empty", "[session]\nsocket = \"frag-socket\"\nprogram_status = \"\"\n", runtime.ProgramStatusOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/city/city.toml"] = []byte("include = [\"frag.toml\"]\n[workspace]\nname = \"c\"\n[session]\nprogram_status = \"shadow\"\n")
			fs.Files["/city/frag.toml"] = []byte(tc.frag)
			cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
			if err != nil {
				t.Fatalf("LoadWithIncludes: %v", err)
			}
			if got := cfg.Session.ProgramStatusMode(); got != tc.want {
				t.Errorf("program_status = %q, want %q", got, tc.want)
			}
			if cfg.Session.Socket != "frag-socket" {
				t.Errorf("socket = %q, want the fragment's [session] socket", cfg.Session.Socket)
			}
		})
	}
}

func TestComposeRejectsInvalidProgramStatusFromFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte("include = [\"frag.toml\"]\n[workspace]\nname = \"c\"\n")
	fs.Files["/city/frag.toml"] = []byte("[session]\nprogram_status = \"maybe\"\n")
	if _, _, err := LoadWithIncludes(fs, "/city/city.toml"); err == nil || !strings.Contains(err.Error(), "program_status") {
		t.Fatalf("LoadWithIncludes err = %v, want program_status validation error", err)
	}
}

func TestProgramStatusKeysAreKnownTOMLKeys(t *testing.T) {
	known := map[string]bool{}
	for _, k := range knownTOMLKeys() {
		known[k] = true
	}
	for _, k := range []string{"program_status", "protocol", "app", "relay"} {
		if !known[k] {
			t.Errorf("knownTOMLKeys missing %q", k)
		}
	}
}

func TestProgramStatusBindingDecodesWithoutUndecodedKeys(t *testing.T) {
	var spec ProviderSpec
	md, err := toml.Decode("[program_status]\nprotocol = \"osc7501\"\napp = \"a\"\nrelay = false\nenv = { K = \"v\" }\n", &spec)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if und := md.Undecoded(); len(und) != 0 {
		t.Errorf("undecoded keys: %v", und)
	}
}

func TestProviderSpecFromWorkerCarriesProgramStatus(t *testing.T) {
	spec := providerSpecFromWorker(workerbuiltin.BuiltinProviderSpec{
		Command:               "agent",
		ProgramStatusProtocol: ProgramStatusProtocolOSC7501,
		ProgramStatusApp:      "agent",
		ProgramStatusRelay:    true,
		ProgramStatusEnv:      map[string]string{"K": "v"},
	})
	ps := spec.ProgramStatus
	if ps.Protocol != ProgramStatusProtocolOSC7501 || ps.App != "agent" || ps.Relay == nil || !*ps.Relay || ps.Env["K"] != "v" {
		t.Fatalf("binding = %+v", ps)
	}
	undeclared := providerSpecFromWorker(workerbuiltin.BuiltinProviderSpec{Command: "agent"})
	if undeclared.ProgramStatus.Relay != nil || undeclared.ProgramStatus.Env != nil || undeclared.ProgramStatus.Declared() {
		t.Fatalf("undeclared builtin produced binding %+v", undeclared.ProgramStatus)
	}
}
