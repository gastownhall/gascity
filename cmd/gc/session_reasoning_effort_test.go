package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// codexEffortResolvedProvider builds a ResolvedProvider backed by the real
// builtin codex schema so opt_effort uses production flag args.
func codexEffortResolvedProvider() *config.ResolvedProvider {
	codex := config.BuiltinProviders()["codex"]
	return &config.ResolvedProvider{
		Name:              "codex",
		BuiltinAncestor:   "codex",
		Command:           codex.Command,
		OptionsSchema:     codex.OptionsSchema,
		EffectiveDefaults: config.ComputeEffectiveDefaults(codex.OptionsSchema, codex.OptionDefaults, nil),
	}
}

func claudeEffortResolvedProvider() *config.ResolvedProvider {
	claude := config.BuiltinProviders()["claude"]
	return &config.ResolvedProvider{
		Name:              "claude",
		BuiltinAncestor:   "claude",
		Command:           claude.Command,
		OptionsSchema:     claude.OptionsSchema,
		EffectiveDefaults: config.ComputeEffectiveDefaults(claude.OptionsSchema, claude.OptionDefaults, nil),
	}
}

// newOptionSessionWithWork creates a session bead plus an in-progress work bead
// assigned to that session carrying opt_<key> option metadata.
func newOptionSessionWithWork(t *testing.T, rp *config.ResolvedProvider, baseCommand string, options map[string]string) (startCandidate, *config.City, beads.Store) {
	t.Helper()
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"template":     "worker",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	workMeta := map[string]string{}
	for key, value := range options {
		workMeta[dispatchOptionMetadataKey(key)] = value
	}
	work, err := store.Create(beads.Bead{
		Title:    "do the work",
		Assignee: session.ID,
		Metadata: workMeta,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: ptrString("in_progress")}); err != nil {
		t.Fatalf("Update(work status): %v", err)
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{
		Command:          baseCommand,
		SessionName:      "worker",
		TemplateName:     "worker",
		ResolvedProvider: rp,
	}
	return startCandidate{session: &session, tp: tp, order: 0}, cfg, store
}

func TestBuildPreparedStart_CodexDispatchEffortOptionPresent(t *testing.T) {
	candidate, cfg, store := newOptionSessionWithWork(t, codexEffortResolvedProvider(), "codex", map[string]string{"effort": "high"})

	prepared, err := buildPreparedStart(candidate, cfg, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "-c model_reasoning_effort=high") {
		t.Fatalf("command %q should contain -c model_reasoning_effort=high", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "model_reasoning_effort=xhigh") {
		t.Fatalf("command %q dispatch effort=high should override the xhigh default", prepared.cfg.Command)
	}
}

func TestBuildPreparedStart_ProviderEffortOptionUsesProviderSchema(t *testing.T) {
	candidate, cfg, store := newOptionSessionWithWork(t, claudeEffortResolvedProvider(), "claude", map[string]string{"effort": "high"})

	prepared, err := buildPreparedStart(candidate, cfg, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--effort high") {
		t.Fatalf("command %q should contain claude --effort high", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "model_reasoning_effort") {
		t.Fatalf("command %q should not contain codex reasoning flags for claude", prepared.cfg.Command)
	}
}

func TestBuildPreparedStart_ExplicitEffortOverrideWinsOverDispatchOption(t *testing.T) {
	candidate, cfg, store := newOptionSessionWithWork(t, codexEffortResolvedProvider(), "codex", map[string]string{"effort": "high"})
	candidate.session.Metadata["template_overrides"] = `{"effort":"low"}`

	prepared, err := buildPreparedStart(candidate, cfg, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "-c model_reasoning_effort=low") {
		t.Fatalf("command %q should keep the explicit effort=low override", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "model_reasoning_effort=high") {
		t.Fatalf("command %q explicit override should beat the dispatch effort=high", prepared.cfg.Command)
	}
}

func ptrString(s string) *string { return &s }
