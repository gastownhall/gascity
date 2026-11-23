package config

import (
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	workerbuiltin "github.com/gastownhall/gascity/internal/worker/builtin"
)

// Kills a declaration that drops either decision opt-in, carries one onto a
// binding that does not declare the protocol, or reads an explicit false as
// true.
func TestProgramStatusDecisionOptInsReachTheArm(t *testing.T) {
	declared := ProgramStatusBinding{Protocol: ProgramStatusProtocolOSC7501, DecideStartup: boolPtr(true), DecideSettle: boolPtr(true)}
	want := runtime.ProgramStatusArm{Capture: true, DecideStartup: true, DecideSettle: true}
	if arm, _ := declared.Arm(runtime.ProgramStatusOn, false); arm != want {
		t.Errorf("Arm(on) = %+v, want %+v", arm, want)
	}
	if arm, _ := declared.Launch(runtime.ProgramStatusOff, false); arm != want {
		t.Errorf("Launch(off) = %+v, want the switch-independent %+v", arm, want)
	}
	explicitFalse := ProgramStatusBinding{Protocol: ProgramStatusProtocolOSC7501, DecideStartup: boolPtr(false), DecideSettle: boolPtr(false)}
	if arm, _ := explicitFalse.Arm(runtime.ProgramStatusOn, false); arm != (runtime.ProgramStatusArm{Capture: true}) {
		t.Errorf("explicit false: arm = %+v, want capture alone", arm)
	}
	undeclared := ProgramStatusBinding{DecideStartup: boolPtr(true), DecideSettle: boolPtr(true)}
	if arm, _ := undeclared.Arm(runtime.ProgramStatusOn, false); arm != (runtime.ProgramStatusArm{}) {
		t.Errorf("opt-ins without a declaration: arm = %+v, want nothing", arm)
	}
	if arm, _ := declared.Arm(runtime.ProgramStatusOn, true); arm != (runtime.ProgramStatusArm{}) {
		t.Errorf("acp transport: arm = %+v, want nothing", arm)
	}
}

func TestParseProviderProgramStatusDecisionOptIns(t *testing.T) {
	cfg, err := Parse([]byte(`
[providers.p]
command = "agent"

[providers.p.program_status]
protocol = "osc7501"
decide_startup = true
decide_settle = false
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := cfg.Providers["p"].ProgramStatus
	if got.DecideStartup == nil || !*got.DecideStartup || got.DecideSettle == nil || *got.DecideSettle {
		t.Fatalf("binding = %+v, want decide_startup true and decide_settle explicitly false", got)
	}
	var spec ProviderSpec
	md, err := toml.Decode("[program_status]\ndecide_startup = true\ndecide_settle = true\n", &spec)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if und := md.Undecoded(); len(und) != 0 {
		t.Errorf("undecoded keys: %v", und)
	}
	known := map[string]bool{}
	for _, k := range knownTOMLKeys() {
		known[k] = true
	}
	for _, k := range []string{"decide_startup", "decide_settle"} {
		if !known[k] {
			t.Errorf("knownTOMLKeys missing %q", k)
		}
	}
}

// Kills a merge that drops an inherited opt-in or ignores a child's explicit
// false, for each decision on its own.
func TestMergeProviderOverBuiltinMergesDecisionOptIns(t *testing.T) {
	base := ProviderSpec{ProgramStatus: ProgramStatusBinding{Protocol: ProgramStatusProtocolOSC7501, DecideStartup: boolPtr(true), DecideSettle: boolPtr(true)}}
	inherited := MergeProviderOverBuiltin(base, ProviderSpec{}).ProgramStatus
	if inherited.DecideStartup == nil || !*inherited.DecideStartup || inherited.DecideSettle == nil || !*inherited.DecideSettle {
		t.Errorf("inherited = %+v, want both opt-ins from the base", inherited)
	}
	child := ProviderSpec{ProgramStatus: ProgramStatusBinding{DecideSettle: boolPtr(false)}}
	merged := MergeProviderOverBuiltin(base, child).ProgramStatus
	if merged.DecideStartup == nil || !*merged.DecideStartup {
		t.Errorf("decide_startup = %v, want the base's true", merged.DecideStartup)
	}
	if merged.DecideSettle == nil || *merged.DecideSettle {
		t.Errorf("decide_settle = %v, want the child's explicit false", merged.DecideSettle)
	}
	child = ProviderSpec{ProgramStatus: ProgramStatusBinding{DecideStartup: boolPtr(false)}}
	if got := MergeProviderOverBuiltin(base, child).ProgramStatus.DecideStartup; got == nil || *got {
		t.Errorf("decide_startup = %v, want the child's explicit false", got)
	}
}

func TestDeepCopyProgramStatusDecisionOptIns(t *testing.T) {
	binding := func() ProgramStatusBinding {
		return ProgramStatusBinding{Protocol: ProgramStatusProtocolOSC7501, DecideStartup: boolPtr(true), DecideSettle: boolPtr(true)}
	}
	spec := ProviderSpec{ProgramStatus: binding()}
	dup := deepCopyProviderSpec(spec)
	*dup.ProgramStatus.DecideStartup = false
	*dup.ProgramStatus.DecideSettle = false
	if !*spec.ProgramStatus.DecideStartup || !*spec.ProgramStatus.DecideSettle {
		t.Error("deepCopyProviderSpec aliases the decision opt-ins")
	}
	r := ResolvedProvider{ProgramStatus: binding()}
	rdup := deepCopyResolvedProvider(r)
	*rdup.ProgramStatus.DecideStartup = false
	*rdup.ProgramStatus.DecideSettle = false
	if !*r.ProgramStatus.DecideStartup || !*r.ProgramStatus.DecideSettle {
		t.Error("deepCopyResolvedProvider aliases the decision opt-ins")
	}
}

func TestComposeFragmentMergesDecisionOptIns(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["frag.toml"]

[workspace]
name = "c"

[providers.p]
command = "agent"

[providers.p.program_status]
protocol = "osc7501"
decide_startup = true
`)
	fs.Files["/city/frag.toml"] = []byte(`
[providers.p.program_status]
decide_settle = true
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	ps := cfg.Providers["p"].ProgramStatus
	if ps.DecideStartup == nil || !*ps.DecideStartup {
		t.Errorf("decide_startup = %v, want the base's true kept", ps.DecideStartup)
	}
	if ps.DecideSettle == nil || !*ps.DecideSettle {
		t.Errorf("decide_settle = %v, want the fragment's true", ps.DecideSettle)
	}
}

// Kills a builtin lift that drops either opt-in, or invents one for a
// builtin that does not declare it.
func TestProviderSpecFromWorkerCarriesDecisionOptIns(t *testing.T) {
	spec := providerSpecFromWorker(workerbuiltin.BuiltinProviderSpec{
		Command:                    "agent",
		ProgramStatusProtocol:      ProgramStatusProtocolOSC7501,
		ProgramStatusDecideStartup: true,
		ProgramStatusDecideSettle:  true,
	})
	ps := spec.ProgramStatus
	if ps.DecideStartup == nil || !*ps.DecideStartup || ps.DecideSettle == nil || !*ps.DecideSettle {
		t.Fatalf("binding = %+v, want both opt-ins", ps)
	}
	undecided := providerSpecFromWorker(workerbuiltin.BuiltinProviderSpec{Command: "agent", ProgramStatusProtocol: ProgramStatusProtocolOSC7501}).ProgramStatus
	if undecided.DecideStartup != nil || undecided.DecideSettle != nil {
		t.Fatalf("binding = %+v, want both opt-ins unset so a city layer inherits", undecided)
	}
}

// Kills a builtin catalog whose resolved declarations opt a family into a
// record-driven decision without its evidence: of the declaring families,
// only claude and codex decide the startup ladder with the record, and only
// claude the interrupt settle (no shadow recorded a codex settle); every
// other family and decision decides as under off.
func TestBuiltinDecisionOptInsResolveOnlyForMeasuredFamilies(t *testing.T) {
	measuredStartup := map[string]bool{"claude": true, "codex": true}
	measuredSettle := map[string]bool{"claude": true}
	for name, spec := range BuiltinProviders() {
		ps := spec.ProgramStatus
		if !ps.Declared() {
			continue
		}
		arm, _ := ps.Arm(runtime.ProgramStatusOn, false)
		if arm.DecideStartup != measuredStartup[name] || arm.DecideSettle != measuredSettle[name] {
			t.Errorf("%s: arm = %+v, want decide_startup=%v, decide_settle=%v", name, arm, measuredStartup[name], measuredSettle[name])
		}
	}
}
