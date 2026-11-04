package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func bootstrapProviderCity(family string) *config.City {
	base := config.BasePrefixBuiltin + family
	extra := config.ProviderSpec{Base: &base}
	return &config.City{Providers: map[string]config.ProviderSpec{"alias": extra}}
}

func bootstrapResolvedProvider(t *testing.T, family string, extra config.ProviderSpec) *config.ResolvedProvider {
	t.Helper()
	base := config.BasePrefixBuiltin + family
	extra.Base = &base
	resolved, err := config.ResolveProvider(&config.Agent{Provider: "alias"}, nil, map[string]config.ProviderSpec{"alias": extra}, func(name string) (string, error) { return "/qualified/package/bin/" + name, nil })
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestBootstrapInputOriginFreshDeferredBuiltinUsesActualPreparedLaunch(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			rp := bootstrapResolvedProvider(t, family, config.ProviderSpec{})
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			city, work := t.TempDir(), t.TempDir()
			manager := session.NewManagerWithOptions(store, sp, session.WithCityPath(city))
			h, err := worker.NewSessionHandle(worker.SessionHandleConfig{Manager: manager, Session: worker.SessionSpec{
				Template: "worker", Title: "Fresh builtin", Provider: "alias", Command: rp.CommandString(), WorkDir: work,
				Hints:  runtime.Config{ProviderName: resolvedProviderLaunchFamily(rp)},
				Resume: session.ProviderResume{SessionIDFlag: rp.SessionIDFlag, ResumeFlag: rp.ResumeFlag, ResumeStyle: rp.ResumeStyle, ResumeCommand: rp.ResumeCommand},
			}})
			if err != nil {
				t.Fatal(err)
			}
			info, err := h.Create(context.Background(), worker.CreateModeDeferred)
			if err != nil {
				t.Fatal(err)
			}
			if info.Command == "" || info.StartedConfigHash != "" || info.Generation != "1" || info.ContinuationEpoch != "1" {
				t.Fatalf("unexpected actual fresh row: %+v", info)
			}
			tp := TemplateParams{
				TemplateName: "worker", SessionName: info.SessionName, Command: rp.CommandString(), WorkDir: work, ResolvedProvider: rp,
				Hints: agent.StartupHints{ProviderName: resolvedProviderLaunchFamily(rp)},
			}
			cfg := bootstrapProviderCity(family)
			prepared, _, err := buildPreparedStart(startCandidate{info: info, tp: tp}, cfg, store)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.cfg.BootstrapInputOrigin != runtime.BootstrapInputFreshBuiltin {
				t.Fatalf("actual deferred %s launch refused bootstrap: %+v", family, prepared.cfg)
			}
			if len(sp.SnapshotCalls()) != 0 {
				t.Fatalf("deferred launch preparation performed runtime actions: %+v", sp.SnapshotCalls())
			}
			// A prior-start marker is authoritative even if the transcript has
			// disappeared and deriveFirstStart would now call this a first start.
			info.StartedConfigHash = "prior-native-start"
			if got := freshBootstrapInputOrigin(tp, cfg, prepared.cfg, info, true, false, false, ""); got != "" {
				t.Fatalf("prior start reseeded credit: %q", got)
			}
		})
	}
}

func TestBootstrapInputOriginRejectsUnprovenLaunchAndStartupInput(t *testing.T) {
	base := TemplateParams{ResolvedProvider: bootstrapResolvedProvider(t, "codex", config.ProviderSpec{})}
	type state struct {
		tp                    TemplateParams
		city                  *config.City
		cfg                   runtime.Config
		info                  session.Info
		first, force, resumed bool
		parent                string
	}
	tests := map[string]func(*state){
		"no provider": func(s *state) { s.tp.ResolvedProvider = nil },
		"opaque command": func(s *state) {
			s.tp.ResolvedProvider = bootstrapResolvedProvider(t, "codex", config.ProviderSpec{Command: "opaque-launcher", ResumeCommand: "opaque-launcher resume {{.SessionKey}}"})
		},
		"raw argv input": func(s *state) {
			s.tp.ResolvedProvider = bootstrapResolvedProvider(t, "codex", config.ProviderSpec{Args: []string{"startup task"}})
		},
		"workspace command": func(s *state) { s.city.Workspace.StartCommand = "codex" },
		"missing catalog":   func(s *state) { s.city = nil },
		"ACP":               func(s *state) { s.tp.IsACP = true },
		"not first":         func(s *state) { s.first = false },
		"fresh reset":       func(s *state) { s.force = true },
		"resume":            func(s *state) { s.resumed = true },
		"fork":              func(s *state) { s.parent = "prior-native-conversation" },
		"prior started":     func(s *state) { s.info.StartedConfigHash = "started" },
		"later generation":  func(s *state) { s.info.Generation = "2" },
		"later epoch":       func(s *state) { s.info.ContinuationEpoch = "2" },
		"pending reset":     func(s *state) { s.info.ContinuationResetPending = "true" },
		"prompt":            func(s *state) { s.cfg.PromptSuffix = "'initial user content'" },
		"nudge":             func(s *state) { s.cfg.Nudge = "initial user content" },
		"setup keys":        func(s *state) { s.cfg.SessionSetup = []string{"tmux send-keys Enter"} },
		"setup script":      func(s *state) { s.cfg.SessionSetupScript = "/opaque/startup.sh" },
		"live keys":         func(s *state) { s.cfg.SessionLive = []string{"tmux send-keys Enter"} },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			s := state{tp: base, city: bootstrapProviderCity("codex"), first: true}
			change(&s)
			if got := freshBootstrapInputOrigin(s.tp, s.city, s.cfg, s.info, s.first, s.force, s.resumed, s.parent); got != "" {
				t.Fatalf("unproven launch granted credit: %q", got)
			}
		})
	}
}

func TestBootstrapInputOriginInitialMessageAssemblyCannotGrantCredit(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			rp := bootstrapResolvedProvider(t, family, config.ProviderSpec{})
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			work := t.TempDir()
			manager := session.NewManagerWithOptions(store, sp, session.WithCityPath(t.TempDir()))
			h, err := worker.NewSessionHandle(worker.SessionHandleConfig{Manager: manager, Session: worker.SessionSpec{Template: "worker", Title: "Initial input", Provider: family, Command: rp.CommandString(), WorkDir: work}})
			if err != nil {
				t.Fatal(err)
			}
			info, err := h.Create(context.Background(), worker.CreateModeDeferred)
			if err != nil {
				t.Fatal(err)
			}
			row, err := store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			row.Metadata["template_overrides"] = `{"initial_message":"initial user task"}`
			if err := store.SetMetadata(info.ID, "template_overrides", row.Metadata["template_overrides"]); err != nil {
				t.Fatal(err)
			}
			info.TemplateOverrides = row.Metadata["template_overrides"]
			tp := TemplateParams{TemplateName: "worker", SessionName: info.SessionName, Command: rp.CommandString(), WorkDir: work, ResolvedProvider: rp, Hints: agent.StartupHints{ProviderName: family}}
			cfg := bootstrapProviderCity(family)
			prepared, _, err := buildPreparedStart(startCandidate{info: info, tp: tp}, cfg, store)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.cfg.BootstrapInputOrigin != "" {
				t.Fatalf("initial message granted first-input credit: %+v", prepared.cfg)
			}
		})
	}
}

// This follows the actual deferred-create command baseline and template-option
// metadata path of a configured worker, then the real prepared launcher.
// Its automatically composed beacon is startup input even when the supplied
// model/effort/permission options themselves are only launch settings. Bare
// provider HTTP creates use a separate fresh-create seam, tested in internal/api.
func TestBootstrapInputOriginConfiguredWorkerBeaconRefusesCreditWithExactOptions(t *testing.T) {
	cases := []struct {
		family, model, permissionFlag, effortFlag string
	}{
		{"codex", "gpt-6.1-sol", "--dangerously-bypass-approvals-and-sandbox", "model_reasoning_effort"},
		{"claude", "claude-opus-5-5", "--dangerously-skip-permissions", "--effort"},
	}
	for _, tc := range cases {
		t.Run(tc.family, func(t *testing.T) {
			cfg := bootstrapProviderCity(tc.family)
			cityPath, work := t.TempDir(), t.TempDir()
			cfg.Workspace = config.Workspace{Name: "bootstrap-options", Provider: "alias"}
			cfgAgent := config.Agent{Name: "worker", Provider: "alias", Session: config.SessionTransportTmux, Dir: work}
			cfg.Agents = []config.Agent{cfgAgent}
			bp := &agentBuildParams{
				city: cfg, cityName: cfg.Workspace.Name, cityPath: cityPath,
				workspace: &cfg.Workspace, providers: cfg.Providers,
				lookPath: func(name string) (string, error) { return "/qualified/package/bin/" + name, nil },
				fs:       fsys.OSFS{}, rigs: []config.Rig{}, beaconTime: testBeaconTime,
				beadNames: map[string]string{}, stderr: io.Discard,
			}
			tp, err := resolveTemplate(bp, &cfgAgent, "worker", nil)
			if err != nil {
				t.Fatal(err)
			}
			rp := tp.ResolvedProvider
			options := map[string]string{"model": tc.model, "effort": "medium", "permission_mode": "unrestricted"}
			if _, err := config.ResolveExplicitOptions(rp.OptionsSchema, options); err != nil {
				t.Fatalf("real options were not accepted: %v", err)
			}
			if len(rp.Args) != 0 {
				t.Fatalf("recognized options became opaque argv: %v", rp.Args)
			}
			declaredCommand := rp.Command
			launch, err := config.BuildProviderTemplateLaunchCommand(cityPath, rp, config.SessionTransportTmux, config.ResolveInstallHooks(&cfgAgent, &cfg.Workspace), cfg.Providers)
			if err != nil {
				t.Fatal(err)
			}
			if launch.Command != tp.Command {
				t.Fatalf("API deferred baseline=%q controller baseline=%q", launch.Command, tp.Command)
			}
			data, err := json.Marshal(options)
			if err != nil {
				t.Fatal(err)
			}
			store, sp := beads.NewMemStore(), runtime.NewFake()
			manager := session.NewManagerWithOptions(store, sp, session.WithCityPath(cityPath))
			h, err := worker.NewSessionHandle(worker.SessionHandleConfig{Manager: manager, Session: worker.SessionSpec{
				Template: "worker", Title: "Actual public options", Provider: "alias", Transport: config.SessionTransportTmux,
				Command: launch.Command, WorkDir: work, Hints: tp.Hints.ToRuntimeConfig(),
				Resume:   session.ProviderResume{SessionIDFlag: rp.SessionIDFlag, ResumeFlag: rp.ResumeFlag, ResumeStyle: rp.ResumeStyle, ResumeCommand: rp.ResumeCommand},
				Metadata: map[string]string{"template_overrides": string(data)},
			}})
			if err != nil {
				t.Fatal(err)
			}
			info, err := h.Create(context.Background(), worker.CreateModeDeferred)
			if err != nil {
				t.Fatal(err)
			}
			if info.Command != launch.Command || info.TemplateOverrides != string(data) || info.StartedConfigHash != "" {
				t.Fatalf("actual deferred row lost baseline/options or appears previously started: %+v", info)
			}
			tp.SessionName = info.SessionName
			prepared, _, err := buildPreparedStart(startCandidate{info: info, tp: tp}, cfg, store)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.cfg.BootstrapInputOrigin != "" || (prepared.cfg.PromptSuffix == "" && prepared.cfg.Nudge == "") {
				t.Fatalf("configured %s worker beacon must remain startup input without fresh credit: origin=%q prompt=%t nudge=%t", tc.family, prepared.cfg.BootstrapInputOrigin, prepared.cfg.PromptSuffix != "", prepared.cfg.Nudge != "")
			}
			if tp.ResolvedProvider.Command != declaredCommand || len(tp.ResolvedProvider.Args) != 0 {
				t.Fatalf("schema options changed underlying command provenance: %+v", tp.ResolvedProvider)
			}
			for _, required := range []string{tc.model, "medium", tc.permissionFlag, tc.effortFlag} {
				if !strings.Contains(prepared.cfg.Command, required) {
					t.Fatalf("prepared command %q lost actual option %q", prepared.cfg.Command, required)
				}
			}
			if len(sp.SnapshotCalls()) != 0 {
				t.Fatal("deferred option preparation invoked a runtime")
			}
		})
	}
}
