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
