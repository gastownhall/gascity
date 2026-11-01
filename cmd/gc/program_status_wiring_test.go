package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/processenv"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

const programStatusTestEnvKey = "PROGRAM_STATUS_TEST_FLAG"

func programStatusTestBinding() config.ProgramStatusBinding {
	relay := true
	return config.ProgramStatusBinding{
		Protocol: config.ProgramStatusProtocolOSC7501,
		App:      "agent",
		Relay:    &relay,
		Env:      map[string]string{programStatusTestEnvKey: "1"},
	}
}

var programStatusTestArm = runtime.ProgramStatusArm{Capture: true, App: "agent", Relay: true}

// newProgramStatusTestCity returns a city directory shared by every resolution
// in one test, so the city-anchored env (GC_CITY, GC_CITY_PATH) is identical
// across the configs a test compares.
func newProgramStatusTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	return cityPath
}

// programStatusTestProvider is an ACP-capable provider declaring binding.
func programStatusTestProvider(binding config.ProgramStatusBinding) config.ProviderSpec {
	supportsACP := true
	return config.ProviderSpec{
		Command:       "echo",
		PromptMode:    "none",
		SupportsACP:   &supportsACP,
		ProgramStatus: binding,
	}
}

// programStatusTemplateParams resolves provider through the reconciler/CLI
// create path under the given switch.
func programStatusTemplateParams(t *testing.T, cityPath, mode string, provider config.ProviderSpec, agentSession string) TemplateParams {
	t.Helper()
	params := upstreamTestParams(t, &config.City{Session: config.SessionConfig{ProgramStatus: mode}})
	params.cityPath = cityPath
	params.providers = map[string]config.ProviderSpec{"test": provider}
	agent := &config.Agent{Name: "runner", Session: agentSession}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	return tp
}

// programStatusTemplateConfig resolves a declared provider through the
// reconciler/CLI create path under the given switch and returns its
// runtime.Config.
func programStatusTemplateConfig(t *testing.T, cityPath, mode string, binding config.ProgramStatusBinding, agentSession string) runtime.Config {
	t.Helper()
	return templateParamsToConfig(programStatusTemplateParams(t, cityPath, mode, programStatusTestProvider(binding), agentSession))
}

func TestProgramStatusEnvDoesNotMoveLaunchFingerprint(t *testing.T) {
	cityPath := newProgramStatusTestCity(t)
	off := programStatusTemplateConfig(t, cityPath, "off", programStatusTestBinding(), "")
	for _, mode := range []string{"shadow", "on"} {
		armed := programStatusTemplateConfig(t, cityPath, mode, programStatusTestBinding(), "")
		if got := armed.Env[programStatusTestEnvKey]; got != "1" {
			t.Fatalf("%s: Env[%s] = %q, want the binding's forced env", mode, programStatusTestEnvKey, got)
		}
		if _, ok := armed.OperatorEnv[programStatusTestEnvKey]; ok {
			t.Errorf("%s: program-status env leaked into OperatorEnv (hashed launch identity)", mode)
		}
		if armed.ProgramStatus != programStatusTestArm {
			t.Errorf("%s: ProgramStatus = %+v, want %+v", mode, armed.ProgramStatus, programStatusTestArm)
		}
		for name, fp := range map[string]func(runtime.Config) string{
			"ConfigFingerprint":             runtime.ConfigFingerprint,
			"CoreFingerprint":               runtime.CoreFingerprint,
			"LiveFingerprint":               runtime.LiveFingerprint,
			"PrePolicyProvisionFingerprint": runtime.PrePolicyProvisionFingerprint,
			"PrePolicyLaunchFingerprint":    runtime.PrePolicyLaunchFingerprint,
		} {
			if fp(off) != fp(armed) {
				t.Errorf("%s: %s moved when program status armed", mode, name)
			}
		}
	}
}

func TestProgramStatusEnvSkippedOnACPTransport(t *testing.T) {
	cityPath := newProgramStatusTestCity(t)
	cfg := programStatusTemplateConfig(t, cityPath, "on", programStatusTestBinding(), config.SessionTransportACP)
	if _, ok := cfg.Env[programStatusTestEnvKey]; ok {
		t.Errorf("ACP session received program-status env %q", programStatusTestEnvKey)
	}
	if cfg.ProgramStatus != (runtime.ProgramStatusArm{}) {
		t.Errorf("ACP session ProgramStatus = %+v, want disarmed", cfg.ProgramStatus)
	}
}

// TestProgramStatusRecordedACPLaunchIsDisarmed pins the recorded-ACP transport
// swap (templateParamsForSession): a session recorded on ACP whose template now
// resolves the CLI transport starts on its ACP launch disarmed, and a key the
// binding forces keeps the value the template's other layers resolved (here
// the provider's), neither the forced value nor deleted. Both builders the
// swap feeds are checked: the start path and the drift hash.
func TestProgramStatusRecordedACPLaunchIsDisarmed(t *testing.T) {
	cityPath := newProgramStatusTestCity(t)
	provider := programStatusTestProvider(programStatusTestBinding())
	provider.Env = map[string]string{programStatusTestEnvKey: "provider"}
	tp := programStatusTemplateParams(t, cityPath, "on", provider, "")
	if tp.IsACP || tp.ACPLaunch == nil {
		t.Fatalf("template IsACP = %v, ACPLaunch = %v; want a CLI template that keeps an ACP launch", tp.IsACP, tp.ACPLaunch)
	}

	// Control: the CLI launch, and the drift hash of a session that records
	// no transport, are armed with the forced env.
	for name, cfg := range map[string]runtime.Config{
		"templateParamsToConfig":       templateParamsToConfig(tp),
		"sessionCoreConfigForHashInfo": sessionCoreConfigForHashInfo(tp, session.Info{}),
	} {
		if cfg.ProgramStatus != programStatusTestArm {
			t.Errorf("%s: CLI launch ProgramStatus = %+v, want %+v", name, cfg.ProgramStatus, programStatusTestArm)
		}
		if got := cfg.Env[programStatusTestEnvKey]; got != "1" {
			t.Errorf("%s: CLI launch Env[%s] = %q, want the binding's forced %q", name, programStatusTestEnvKey, got, "1")
		}
	}

	recorded := session.Info{Transport: config.SessionTransportACP}
	acp, swapped := templateParamsForSession(tp, recorded)
	if !swapped {
		t.Fatal("templateParamsForSession kept the CLI launch for a session recorded on ACP")
	}
	for name, cfg := range map[string]runtime.Config{
		"templateParamsToConfig":       templateParamsToConfig(acp),
		"sessionCoreConfigForHashInfo": sessionCoreConfigForHashInfo(tp, recorded),
	} {
		if cfg.Command != tp.ACPLaunch.Command {
			t.Fatalf("%s: Command = %q, want the ACP launch %q", name, cfg.Command, tp.ACPLaunch.Command)
		}
		if cfg.ProgramStatus != (runtime.ProgramStatusArm{}) {
			t.Errorf("%s: recorded-ACP launch ProgramStatus = %+v, want disarmed", name, cfg.ProgramStatus)
		}
		if got := cfg.Env[programStatusTestEnvKey]; got != "provider" {
			t.Errorf("%s: recorded-ACP launch Env[%s] = %q, want the provider's %q", name, programStatusTestEnvKey, got, "provider")
		}
	}
}

func TestProgramStatusOffInjectsNothing(t *testing.T) {
	cityPath := newProgramStatusTestCity(t)
	declared := programStatusTemplateConfig(t, cityPath, "off", programStatusTestBinding(), "")
	undeclared := programStatusTemplateConfig(t, cityPath, "off", config.ProgramStatusBinding{}, "")
	unset := programStatusTemplateConfig(t, cityPath, "", programStatusTestBinding(), "")
	if !reflect.DeepEqual(declared, undeclared) {
		t.Errorf("off: a declared binding changed runtime.Config\ndeclared:   %+v\nundeclared: %+v", declared, undeclared)
	}
	if !reflect.DeepEqual(unset, undeclared) {
		t.Errorf("unset switch: a declared binding changed runtime.Config")
	}
	if _, ok := declared.Env[programStatusTestEnvKey]; ok {
		t.Errorf("off injected %s", programStatusTestEnvKey)
	}
}

// TestProgramStatusEnvYieldsToControllerOnlyRepin pins resolveTemplate's
// managed pins as the last writers over a binding's forced env. Load rejects
// such keys in a configured binding; built-in profiles are not validated, so
// the template launch re-pins them after the forced env.
func TestProgramStatusEnvYieldsToControllerOnlyRepin(t *testing.T) {
	cityPath := newProgramStatusTestCity(t)
	var key, pinned string
	for k, v := range processenv.ControllerOnlyEnvOverlay() {
		key, pinned = k, v
		break
	}
	if key == "" {
		t.Fatal("processenv.ControllerOnlyEnvOverlay is empty; nothing pins the re-pin order")
	}
	binding := programStatusTestBinding()
	binding.Env[key] = "from-binding"
	binding.Env[execenv.UsageMetricsDisableEnv] = "from-binding"
	cfg := programStatusTemplateConfig(t, cityPath, "on", binding, "")
	if got := cfg.Env[key]; got != pinned {
		t.Errorf("Env[%s] = %q, want the controller-only re-pin %q to stay the last writer", key, got, pinned)
	}
	if got := cfg.Env[execenv.UsageMetricsDisableEnv]; got != execenv.UsageMetricsDisableValue {
		t.Errorf("Env[%s] = %q, want the usage-metrics opt-out %q to stay the last writer", execenv.UsageMetricsDisableEnv, got, execenv.UsageMetricsDisableValue)
	}
}

// TestEveryRuntimeConfigSiteAppliesProgramStatusArm pins the cmd/gc
// runtime.Config construction sites: the reconciler/CLI create path
// (templateParamsToConfig), the unmanaged worker create path, and worker
// resume. internal/api pins its own sites.
func TestEveryRuntimeConfigSiteAppliesProgramStatusArm(t *testing.T) {
	newCity := func(mode string) *config.City {
		return &config.City{
			Session: config.SessionConfig{ProgramStatus: mode},
			Agents:  []config.Agent{{Name: "worker", Provider: "stub"}},
			Providers: map[string]config.ProviderSpec{"stub": {
				Command:       "/bin/echo",
				ProgramStatus: programStatusTestBinding(),
			}},
		}
	}
	resolve := func(t *testing.T, cfg *config.City) *config.ResolvedProvider {
		t.Helper()
		resolved, err := config.ResolveProvider(&cfg.Agents[0], &cfg.Workspace, cfg.Providers, func(string) (string, error) { return "/bin/echo", nil })
		if err != nil {
			t.Fatalf("ResolveProvider: %v", err)
		}
		return resolved
	}

	t.Run("template create", func(t *testing.T) {
		cityPath := newProgramStatusTestCity(t)
		if got := programStatusTemplateConfig(t, cityPath, "on", programStatusTestBinding(), "").ProgramStatus; got != programStatusTestArm {
			t.Errorf("ProgramStatus = %+v, want %+v", got, programStatusTestArm)
		}
	})

	for _, tc := range []struct {
		mode    string
		wantArm runtime.ProgramStatusArm
		wantEnv bool
	}{
		{"on", programStatusTestArm, true},
		{"off", runtime.ProgramStatusArm{}, false},
	} {
		t.Run("worker create "+tc.mode, func(t *testing.T) {
			cityDir := t.TempDir()
			cfg := newCity(tc.mode)
			resolved := resolve(t, cfg)
			sessionCfg := worker.ResolvedSessionConfig{Runtime: worker.ResolvedRuntime{Hints: workerSessionCreateHints(resolved)}}
			applyWorkerCreateRuntimeInputs(&sessionCfg, cfg, cityDir, "worker", "", resolved)
			if got := sessionCfg.Runtime.Hints.ProgramStatus; got != tc.wantArm {
				t.Errorf("Hints.ProgramStatus = %+v, want %+v", got, tc.wantArm)
			}
			_, inSession := sessionCfg.Runtime.SessionEnv[programStatusTestEnvKey]
			_, inHints := sessionCfg.Runtime.Hints.Env[programStatusTestEnvKey]
			if inSession != tc.wantEnv || inHints != tc.wantEnv {
				t.Errorf("program-status env present: SessionEnv=%v Hints.Env=%v, want %v", inSession, inHints, tc.wantEnv)
			}
		})

		t.Run("worker resume "+tc.mode, func(t *testing.T) {
			cityDir := t.TempDir()
			rt, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, newCity(tc.mode), session.Info{
				Template: "worker",
				WorkDir:  cityDir,
			}, "", nil)
			if err != nil {
				t.Fatalf("resolvedWorkerRuntimeWithConfigAndMetadata: %v", err)
			}
			if rt == nil {
				t.Fatal("resolvedWorkerRuntimeWithConfigAndMetadata() = nil")
			}
			if got := rt.Hints.ProgramStatus; got != tc.wantArm {
				t.Errorf("Hints.ProgramStatus = %+v, want %+v", got, tc.wantArm)
			}
			if _, ok := rt.SessionEnv[programStatusTestEnvKey]; ok != tc.wantEnv {
				t.Errorf("SessionEnv has program-status env = %v, want %v", ok, tc.wantEnv)
			}
		})
	}

	t.Run("worker create acp", func(t *testing.T) {
		cfg := newCity("on")
		resolved := resolve(t, cfg)
		sessionCfg := worker.ResolvedSessionConfig{Runtime: worker.ResolvedRuntime{Hints: workerSessionCreateHints(resolved)}}
		applyWorkerCreateRuntimeInputs(&sessionCfg, cfg, t.TempDir(), "worker", config.SessionTransportACP, resolved)
		if got := sessionCfg.Runtime.Hints.ProgramStatus; got != (runtime.ProgramStatusArm{}) {
			t.Errorf("ACP Hints.ProgramStatus = %+v, want disarmed", got)
		}
		if _, ok := sessionCfg.Runtime.SessionEnv[programStatusTestEnvKey]; ok {
			t.Error("ACP create received program-status env")
		}
	})
}

// builtinProgramStatusTemplateConfig resolves an agent on a city provider
// "seat" that inherits family's builtin (base = "builtin:<family>", its own
// wrapper command, as maintainer-city declares its seats) through the
// reconciler/CLI create path under the given switch, and returns its
// runtime.Config. binding is the seat's own [providers.seat.program_status].
func builtinProgramStatusTemplateConfig(t *testing.T, cityPath, mode, family string, binding config.ProgramStatusBinding) runtime.Config {
	t.Helper()
	params := upstreamTestParams(t, &config.City{Session: config.SessionConfig{ProgramStatus: mode}})
	params.cityPath = cityPath
	params.workspace = &config.Workspace{}
	base := "builtin:" + family
	params.providers = map[string]config.ProviderSpec{"seat": {
		Base:          &base,
		Command:       "/bin/echo",
		ResumeCommand: "/bin/echo resume {{.SessionKey}}",
		ProgramStatus: binding,
	}}
	agent := &config.Agent{Name: "runner", Provider: "seat"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(%s): %v", base, err)
	}
	return templateParamsToConfig(tp)
}

// The builtin codex and claude declarations (ga-f4ovjm.18) reach a city
// provider that inherits them, yet change nothing while the switch is off or
// unset: the runtime.Config is the one the explicitly undeclared provider
// resolves to. Under shadow and on they arm capture (claude under the relay)
// without moving any launch fingerprint, and protocol = "none" undeclares.
func TestBuiltinProgramStatusDeclarationsInertWhenOff(t *testing.T) {
	cityPath := newProgramStatusTestCity(t)
	undeclare := config.ProgramStatusBinding{Protocol: config.ProgramStatusProtocolNone}
	for family, wantArm := range map[string]runtime.ProgramStatusArm{
		"codex":  {Capture: true, App: "codex"},
		"claude": {Capture: true, App: "claude-code", Relay: true},
	} {
		t.Run(family, func(t *testing.T) {
			undeclared := builtinProgramStatusTemplateConfig(t, cityPath, "off", family, undeclare)
			for _, mode := range []string{"", "off"} {
				got := builtinProgramStatusTemplateConfig(t, cityPath, mode, family, config.ProgramStatusBinding{})
				if !reflect.DeepEqual(got, undeclared) {
					t.Errorf("switch %q: the builtin declaration changed runtime.Config\ndeclared:   %+v\nundeclared: %+v", mode, got, undeclared)
				}
			}
			for _, mode := range []string{"shadow", "on"} {
				armed := builtinProgramStatusTemplateConfig(t, cityPath, mode, family, config.ProgramStatusBinding{})
				if armed.ProgramStatus != wantArm {
					t.Errorf("%s: ProgramStatus = %+v, want %+v", mode, armed.ProgramStatus, wantArm)
				}
				if runtime.CoreFingerprint(armed) != runtime.CoreFingerprint(undeclared) ||
					runtime.PrePolicyLaunchFingerprint(armed) != runtime.PrePolicyLaunchFingerprint(undeclared) {
					t.Errorf("%s: arming the builtin declaration moved the launch fingerprint", mode)
				}
				if got := builtinProgramStatusTemplateConfig(t, cityPath, mode, family, undeclare).ProgramStatus; got != (runtime.ProgramStatusArm{}) {
					t.Errorf("%s: protocol = \"none\" left ProgramStatus = %+v, want disarmed", mode, got)
				}
			}
		})
	}
}

// Kills a tmux provider that reads under another switch than the city's:
// every process building one (controller, CLI, API) passes the [session]
// program_status switch to the leaf, and an unset switch reads off.
func TestTmuxConfigFromSessionCarriesProgramStatusSwitch(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want runtime.ProgramStatusMode
	}{
		{"", runtime.ProgramStatusOff},
		{"off", runtime.ProgramStatusOff},
		{"shadow", runtime.ProgramStatusShadow},
		{"on", runtime.ProgramStatusOn},
	} {
		cfg := tmuxConfigFromSession(config.SessionConfig{ProgramStatus: tc.set}, "city", "/tmp/city-a")
		if cfg.ProgramStatus != tc.want {
			t.Errorf("program_status %q: tmux Config.ProgramStatus = %q, want %q", tc.set, cfg.ProgramStatus, tc.want)
		}
	}
}
