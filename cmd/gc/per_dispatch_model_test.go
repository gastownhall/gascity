package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// modelSchemaProvider returns a ResolvedProvider that exposes a "model" option
// with two choices, matching the shape of the builtin claude/codex schemas.
func modelSchemaProvider() *config.ResolvedProvider {
	return &config.ResolvedProvider{
		Name:    "claude",
		Command: "claude",
		OptionsSchema: []config.ProviderOption{{
			Key:   "model",
			Label: "Model",
			Choices: []config.OptionChoice{
				{Value: "opus", FlagArgs: []string{"--model", "claude-opus-4-8"}},
				{Value: "sonnet", FlagArgs: []string{"--model", "claude-sonnet-4-6"}},
			},
		}},
	}
}

// newModelSessionCandidate builds an in-progress work bead carrying gc.model
// assigned to a session bead, plus the matching startCandidate. The work bead's
// assignee is the session_name so taskWorkDirAssignees resolves it.
func newModelSessionCandidate(t *testing.T, store beads.Store, gcModel string, sessionOverrides map[string]string) startCandidate {
	t.Helper()
	const sessionName = "worker"
	meta := map[string]string{
		"session_name": sessionName,
		"template":     "worker",
		"state":        "asleep",
	}
	if len(sessionOverrides) > 0 {
		raw, err := json.Marshal(sessionOverrides)
		if err != nil {
			t.Fatalf("Marshal(template_overrides): %v", err)
		}
		meta["template_overrides"] = string(raw)
	}
	session, err := store.Create(beads.Bead{
		Title:    sessionName,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	workMeta := map[string]string{}
	if gcModel != "" {
		workMeta["gc.model"] = gcModel
	}
	work, err := store.Create(beads.Bead{
		Title:    "do the work",
		Type:     "task",
		Assignee: sessionName,
		Metadata: workMeta,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	return startCandidate{
		session: &session,
		tp: TemplateParams{
			TemplateName:     "worker",
			SessionName:      sessionName,
			Command:          "claude",
			ResolvedProvider: modelSchemaProvider(),
		},
	}
}

func sessionModelOverride(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	var parsed map[string]string
	if raw := strings.TrimSpace(b.Metadata["template_overrides"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("unmarshal template_overrides: %v", err)
		}
	}
	return parsed["model"]
}

// setAssignedWorkModel updates the gc.model metadata on the in-progress work
// bead(s) assigned to assignee, simulating a later dispatch advising a
// different (or, with gcModel=="", no) model on the same session.
func setAssignedWorkModel(t *testing.T, store beads.Store, assignee, gcModel string) {
	t.Helper()
	assigned, err := store.List(beads.ListQuery{
		Assignee: assignee,
		Status:   "in_progress",
		Live:     true,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		t.Fatalf("List(assigned work): %v", err)
	}
	if len(assigned) == 0 {
		t.Fatalf("no in-progress work assigned to %q", assignee)
	}
	for _, b := range assigned {
		if err := store.SetMetadata(b.ID, "gc.model", gcModel); err != nil {
			t.Fatalf("SetMetadata(gc.model): %v", err)
		}
	}
}

func TestMaybeApplyPerDispatchModelOverride_SwitchesModelAcrossDispatches(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newModelSessionCandidate(t, store, "opus", nil)

	// First dispatch stamps opus.
	maybeApplyPerDispatchModelOverride(candidate, &config.City{}, store)
	if got := sessionModelOverride(t, store, candidate.session.ID); got != "opus" {
		t.Fatalf("after first dispatch model = %q, want %q", got, "opus")
	}

	// A later dispatch on the same session advises sonnet; the auto-stamped
	// model must flip rather than leak the prior choice forward.
	setAssignedWorkModel(t, store, "worker", "sonnet")
	maybeApplyPerDispatchModelOverride(candidate, &config.City{}, store)
	if got := sessionModelOverride(t, store, candidate.session.ID); got != "sonnet" {
		t.Fatalf("after second dispatch model = %q, want %q", got, "sonnet")
	}
	if got := candidate.session.Metadata[perDispatchModelSourceKey]; got != "sonnet" {
		t.Fatalf("provenance = %q, want %q", got, "sonnet")
	}
}

func TestMaybeApplyPerDispatchModelOverride_ClearsWhenNextDispatchHasNoModel(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newModelSessionCandidate(t, store, "opus", nil)

	// First dispatch stamps opus.
	maybeApplyPerDispatchModelOverride(candidate, &config.City{}, store)
	if got := sessionModelOverride(t, store, candidate.session.ID); got != "opus" {
		t.Fatalf("after first dispatch model = %q, want %q", got, "opus")
	}

	// A later dispatch advises no model; the auto-stamped override must be
	// cleared so the agent falls back to its configured default.
	setAssignedWorkModel(t, store, "worker", "")
	maybeApplyPerDispatchModelOverride(candidate, &config.City{}, store)
	if got := sessionModelOverride(t, store, candidate.session.ID); got != "" {
		t.Fatalf("after clearing dispatch model = %q, want empty", got)
	}
	if got := candidate.session.Metadata[perDispatchModelSourceKey]; got != "" {
		t.Fatalf("provenance = %q, want cleared", got)
	}
}

func TestMaybeApplyPerDispatchModelOverride_ExplicitOverrideStillWinsAfterRouting(t *testing.T) {
	store := beads.NewMemStore()
	// Session carries a genuine explicit override (no provenance marker) and a
	// work bead advises a different model.
	candidate := newModelSessionCandidate(t, store, "opus", map[string]string{"model": "sonnet"})

	maybeApplyPerDispatchModelOverride(candidate, &config.City{}, store)

	if got := sessionModelOverride(t, store, candidate.session.ID); got != "sonnet" {
		t.Fatalf("model = %q, want explicit %q (routing must not replace it)", got, "sonnet")
	}
	if _, ok := candidate.session.Metadata[perDispatchModelSourceKey]; ok {
		t.Fatalf("provenance marker set on explicit override: %q", candidate.session.Metadata[perDispatchModelSourceKey])
	}
}

func TestWorkRoutingModel(t *testing.T) {
	if got := WorkRoutingModel(beads.Bead{}); got != "" {
		t.Fatalf("WorkRoutingModel(empty) = %q, want empty", got)
	}
	if got := WorkRoutingModel(beads.Bead{Metadata: map[string]string{"gc.model": "  opus  "}}); got != "opus" {
		t.Fatalf("WorkRoutingModel = %q, want trimmed %q", got, "opus")
	}
}

func TestResolveTaskModelReadsAssignedWorkBead(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "active step",
		Type:     "task",
		Assignee: "worker-session",
		Metadata: map[string]string{"gc.model": "sonnet"},
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark in_progress: %v", err)
	}

	if got := resolveTaskModel(store, "worker-session"); got != "sonnet" {
		t.Fatalf("resolveTaskModel = %q, want %q", got, "sonnet")
	}
	// Open (not in_progress) work is ignored, matching resolveTaskWorkDir.
	open := "open"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &open}); err != nil {
		t.Fatalf("reopen work: %v", err)
	}
	if got := resolveTaskModel(store, "worker-session"); got != "" {
		t.Fatalf("resolveTaskModel(open work) = %q, want empty", got)
	}
}

func TestMaybeApplyPerDispatchModelOverride_StampsWorkBeadModel(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newModelSessionCandidate(t, store, "opus", nil)

	maybeApplyPerDispatchModelOverride(candidate, &config.City{}, store)

	// Persisted on the bead so config-drift hashing sees the same override.
	if got := sessionModelOverride(t, store, candidate.session.ID); got != "opus" {
		t.Fatalf("persisted template_overrides model = %q, want %q", got, "opus")
	}
	// Mirrored in memory for the in-flight start.
	if got := candidate.session.Metadata["template_overrides"]; !strings.Contains(got, `"model":"opus"`) {
		t.Fatalf("in-memory template_overrides = %q, want model:opus", got)
	}
}

func TestMaybeApplyPerDispatchModelOverride_ExplicitOverrideWins(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newModelSessionCandidate(t, store, "opus", map[string]string{"model": "sonnet"})

	maybeApplyPerDispatchModelOverride(candidate, &config.City{}, store)

	if got := sessionModelOverride(t, store, candidate.session.ID); got != "sonnet" {
		t.Fatalf("model = %q, want explicit %q (routing metadata must not win)", got, "sonnet")
	}
}

func TestMaybeApplyPerDispatchModelOverride_NoWorkModelIsNoOp(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newModelSessionCandidate(t, store, "", nil)

	maybeApplyPerDispatchModelOverride(candidate, &config.City{}, store)

	if _, ok := candidate.session.Metadata["template_overrides"]; ok {
		t.Fatalf("template_overrides set despite no gc.model: %q", candidate.session.Metadata["template_overrides"])
	}
}

func TestMaybeApplyPerDispatchModelOverride_InvalidModelIgnored(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newModelSessionCandidate(t, store, "definitely-not-a-choice", nil)

	maybeApplyPerDispatchModelOverride(candidate, &config.City{}, store)

	if got, ok := candidate.session.Metadata["template_overrides"]; ok {
		t.Fatalf("invalid gc.model was applied: %q", got)
	}
}

// TestBuildPreparedStartAppliesWorkBeadModelToCommand proves the end-to-end
// seam: a work bead's gc.model becomes a --model flag on the launch command.
func TestBuildPreparedStartAppliesWorkBeadModelToCommand(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newModelSessionCandidate(t, store, "opus", nil)

	prepared, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--model claude-opus-4-8") {
		t.Fatalf("prepared command = %q, want --model claude-opus-4-8", prepared.cfg.Command)
	}
}
