package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// resolveCodexBuiltinTemplate resolves a codex agent through the production
// template path against a city that declares `[providers.codex] base =
// "builtin:codex"` and nothing else, the shape a stock city uses.
func resolveCodexBuiltinTemplate(t *testing.T, optionDefaults map[string]string) TemplateParams {
	t.Helper()

	params := &agentBuildParams{
		cityName:   "codex-city",
		cityPath:   t.TempDir(),
		workspace:  &config.Workspace{Provider: "codex"},
		providers:  builtinProviderAliasesForTest("codex"),
		lookPath:   func(name string) (string, error) { return filepath.Join("/usr/bin", name), nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agentCfg := &config.Agent{
		Name:           "worker",
		Provider:       "codex",
		Session:        "tmux",
		WorkDir:        filepath.Join(".gc", "agents", "codex-default-model"),
		OptionDefaults: optionDefaults,
	}
	tp, err := resolveTemplate(params, agentCfg, agentCfg.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(codex): %v", err)
	}
	return tp
}

// A builtin:codex session with no model or effort chosen anywhere must leave
// both to the user's own Codex configuration (~/.codex/config.toml). Forcing
// a catalog model broke every turn for ChatGPT-account logins whose plan does
// not serve that model.
func TestResolveTemplate_CodexBuiltinWithoutChoiceDefersModelAndEffortToCodexConfig(t *testing.T) {
	tp := resolveCodexBuiltinTemplate(t, nil)

	fields := strings.Fields(tp.Command)
	for _, tok := range fields {
		if tok == "--model" || tok == "-m" {
			t.Fatalf("command %q passes a model flag without an explicit model choice", tp.Command)
		}
	}
	if strings.Contains(tp.Command, "model_reasoning_effort") {
		t.Fatalf("command %q passes a reasoning effort without an explicit effort choice", tp.Command)
	}
	if !strings.Contains(tp.Command, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("command %q lost the permission-mode default", tp.Command)
	}
	if rp := tp.ResolvedProvider; rp != nil {
		if v, ok := rp.EffectiveDefaults["model"]; ok && v != "" {
			t.Fatalf("EffectiveDefaults[model] = %q, want unset", v)
		}
		if v, ok := rp.EffectiveDefaults["effort"]; ok && v != "" {
			t.Fatalf("EffectiveDefaults[effort] = %q, want unset", v)
		}
	}
}

func TestResolveTemplate_CodexBuiltinExplicitModelAndEffortStillEmitFlags(t *testing.T) {
	tp := resolveCodexBuiltinTemplate(t, map[string]string{
		"model":  "gpt-6.1-sol",
		"effort": "high",
	})

	if !strings.Contains(tp.Command, "--model gpt-6.1-sol") {
		t.Fatalf("command %q should carry the explicit --model gpt-6.1-sol", tp.Command)
	}
	if !strings.Contains(tp.Command, "-c model_reasoning_effort=high") {
		t.Fatalf("command %q should carry the explicit effort", tp.Command)
	}
	if n := strings.Count(tp.Command, "--model"); n != 1 {
		t.Fatalf("command %q has %d --model flags, want 1", tp.Command, n)
	}
}

func TestBuildPreparedStart_CodexWithoutChoiceOmitsModelAndEffort(t *testing.T) {
	candidate, cfg, store := newOptionSessionWithWork(t, codexEffortResolvedProvider(), "codex", nil)

	prepared, _, err := buildPreparedStart(candidate, cfg, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	for _, tok := range strings.Fields(prepared.cfg.Command) {
		if tok == "--model" || tok == "-m" {
			t.Fatalf("command %q passes a model flag without an explicit model choice", prepared.cfg.Command)
		}
	}
	if strings.Contains(prepared.cfg.Command, "model_reasoning_effort") {
		t.Fatalf("command %q passes a reasoning effort without an explicit effort choice", prepared.cfg.Command)
	}
}

func TestBuildPreparedStart_CodexDispatchModelOptionPresent(t *testing.T) {
	candidate, cfg, store := newOptionSessionWithWork(t, codexEffortResolvedProvider(), "codex", map[string]string{"model": "gpt-6.1-sol"})

	prepared, _, err := buildPreparedStart(candidate, cfg, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--model gpt-6.1-sol") {
		t.Fatalf("command %q should carry the dispatched --model gpt-6.1-sol", prepared.cfg.Command)
	}
}
