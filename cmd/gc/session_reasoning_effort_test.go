package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// codexReasoningResolvedProvider builds a ResolvedProvider backed by the real
// builtin codex schema so the effort option (low/medium/high/xhigh ->
// `-c model_reasoning_effort=<v>`) matches production exactly.
func codexReasoningResolvedProvider() *config.ResolvedProvider {
	codex := config.BuiltinProviders()["codex"]
	return &config.ResolvedProvider{
		Name:              "codex",
		BuiltinAncestor:   "codex",
		Command:           codex.Command,
		OptionsSchema:     codex.OptionsSchema,
		EffectiveDefaults: config.ComputeEffectiveDefaults(codex.OptionsSchema, codex.OptionDefaults, nil),
	}
}

func claudeReasoningResolvedProvider() *config.ResolvedProvider {
	claude := config.BuiltinProviders()["claude"]
	return &config.ResolvedProvider{
		Name:              "claude",
		BuiltinAncestor:   "claude",
		Command:           claude.Command,
		OptionsSchema:     claude.OptionsSchema,
		EffectiveDefaults: config.ComputeEffectiveDefaults(claude.OptionsSchema, claude.OptionDefaults, nil),
	}
}

// newReasoningSessionWithWork creates a session bead plus an in-progress work
// bead assigned to that session carrying the given gc.reasoning value, and
// returns a start candidate plus the store wired for buildPreparedStart.
func newReasoningSessionWithWork(t *testing.T, rp *config.ResolvedProvider, baseCommand, reasoning string) (startCandidate, *config.City, beads.Store) {
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
	if reasoning != "" {
		workMeta[dispatchReasoningMetadataKey] = reasoning
	}
	work, err := store.Create(beads.Bead{
		Title:    "do the work",
		Assignee: session.ID,
		Metadata: workMeta,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	// MemStore.Create forces status=open; transition to in_progress so it is
	// discovered by the same query resolveTaskWorkDir uses in production.
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

func TestBuildPreparedStart_CodexReasoningEffortPresent(t *testing.T) {
	candidate, cfg, store := newReasoningSessionWithWork(t, codexReasoningResolvedProvider(), "codex", "high")

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

func TestBuildPreparedStart_CodexReasoningEffortAbsent(t *testing.T) {
	// No gc.reasoning on the work bead: the launcher must not synthesize a
	// reasoning flag. The base command stays untouched (the codex default
	// effort is only applied by the launch-command builder, not here).
	candidate, cfg, store := newReasoningSessionWithWork(t, codexReasoningResolvedProvider(), "codex", "")

	prepared, err := buildPreparedStart(candidate, cfg, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if strings.Contains(prepared.cfg.Command, "model_reasoning_effort") {
		t.Fatalf("command %q should not contain a reasoning flag when gc.reasoning is absent", prepared.cfg.Command)
	}
}

func TestBuildPreparedStart_CodexReasoningEffortInvalidSkipped(t *testing.T) {
	// An out-of-range value is skipped (logged) rather than crashing or being
	// injected verbatim.
	candidate, cfg, store := newReasoningSessionWithWork(t, codexReasoningResolvedProvider(), "codex", "ludicrous")

	prepared, err := buildPreparedStart(candidate, cfg, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if strings.Contains(prepared.cfg.Command, "model_reasoning_effort") {
		t.Fatalf("command %q should not contain a reasoning flag for an invalid effort", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "ludicrous") {
		t.Fatalf("command %q should never echo an invalid effort value", prepared.cfg.Command)
	}
}

func TestBuildPreparedStart_NonCodexProviderNeverAddsReasoning(t *testing.T) {
	// A valid-looking effort on a non-codex provider (claude has no reasoning
	// option) must never produce a model_reasoning_effort flag.
	candidate, cfg, store := newReasoningSessionWithWork(t, claudeReasoningResolvedProvider(), "claude", "high")

	prepared, err := buildPreparedStart(candidate, cfg, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if strings.Contains(prepared.cfg.Command, "model_reasoning_effort") {
		t.Fatalf("command %q should never contain model_reasoning_effort for a non-codex provider", prepared.cfg.Command)
	}
	// claude has its own unrelated `--effort` option; gc.reasoning must not
	// leak into it either. The base command must be untouched.
	if strings.Contains(prepared.cfg.Command, "--effort") {
		t.Fatalf("command %q should not inject claude --effort from gc.reasoning", prepared.cfg.Command)
	}
}

func TestBuildPreparedStart_ExplicitEffortOverrideWinsOverDispatch(t *testing.T) {
	// A session-level template_overrides effort is more specific than the
	// per-dispatch gc.reasoning default and must win.
	candidate, cfg, store := newReasoningSessionWithWork(t, codexReasoningResolvedProvider(), "codex", "high")
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

func TestResolveDispatchReasoningEffort_NonCodexReturnsEmpty(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "w",
		Assignee: "agent-x",
		Metadata: map[string]string{dispatchReasoningMetadataKey: "high"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: ptrString("in_progress")}); err != nil {
		t.Fatalf("Update(status): %v", err)
	}
	if got := resolveDispatchReasoningEffort(store, claudeReasoningResolvedProvider(), "agent-x"); got != "" {
		t.Fatalf("resolveDispatchReasoningEffort(non-codex) = %q, want empty", got)
	}
	if got := resolveDispatchReasoningEffort(store, codexReasoningResolvedProvider(), "agent-x"); got != "high" {
		t.Fatalf("resolveDispatchReasoningEffort(codex) = %q, want high", got)
	}
}
