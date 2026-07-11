package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// optionSchemaProvider returns a ResolvedProvider with two OptionsSchema keys.
// Work beads request per-dispatch values for these keys via opt_<key> metadata.
func optionSchemaProvider() *config.ResolvedProvider {
	return &config.ResolvedProvider{
		Name:    "claude",
		Command: "claude",
		OptionsSchema: []config.ProviderOption{
			{
				Key:   "model",
				Label: "Model",
				Choices: []config.OptionChoice{
					{Value: "opus", FlagArgs: []string{"--model", "claude-opus-4-8"}},
					{Value: "sonnet", FlagArgs: []string{"--model", "claude-sonnet-4-6"}},
				},
			},
			{
				Key:   "effort",
				Label: "Effort",
				Choices: []config.OptionChoice{
					{Value: "low", FlagArgs: []string{"--effort", "low"}},
					{Value: "high", FlagArgs: []string{"--effort", "high"}},
				},
			},
		},
	}
}

// newOptionSessionCandidate builds an in-progress work bead assigned to a
// session. workOptions are written as opt_<key> metadata, matching the existing
// explicit option metadata convention used for session beads.
func newOptionSessionCandidate(t *testing.T, store beads.Store, workOptions, sessionOverrides map[string]string) startCandidate {
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
	for key, value := range workOptions {
		workMeta[dispatchOptionMetadataKey(key)] = value
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
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:     "worker",
			SessionName:      sessionName,
			Command:          "claude",
			ResolvedProvider: optionSchemaProvider(),
		},
	}
}

func storedSessionOverrides(t *testing.T, store beads.Store, id string) map[string]string {
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
	return parsed
}

func storedSessionMetadata(t *testing.T, store beads.Store, id string) map[string]string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	return b.Metadata
}

func TestBuildPreparedStart_ExplicitOverrideWinsPerKey(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newOptionSessionCandidate(
		t,
		store,
		map[string]string{"model": "opus", "effort": "high"},
		map[string]string{"model": "sonnet"},
	)

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--model claude-sonnet-4-6") {
		t.Fatalf("prepared command = %q, want explicit --model claude-sonnet-4-6", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "claude-opus-4-8") {
		t.Fatalf("prepared command = %q, work opt_model should not override explicit model", prepared.cfg.Command)
	}
	if !strings.Contains(prepared.cfg.Command, "--effort high") {
		t.Fatalf("prepared command = %q, want work opt_effort high", prepared.cfg.Command)
	}
	wantPersisted := map[string]string{"model": "sonnet"}
	if got := storedSessionOverrides(t, store, candidate.info.ID); !reflect.DeepEqual(got, wantPersisted) {
		t.Fatalf("persisted overrides = %v, want unchanged %v", got, wantPersisted)
	}
}

func TestResolveTaskOptionOverridesReadsAssignedWorkBead(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "active step",
		Type:     "task",
		Assignee: "worker-session",
		Metadata: map[string]string{
			"opt_model":  "sonnet",
			"opt_effort": "high",
		},
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark in_progress: %v", err)
	}

	want := map[string]string{"model": "sonnet", "effort": "high"}
	if got := resolveTaskOptionOverrides(store, optionSchemaProvider(), "worker-session"); !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveTaskOptionOverrides = %v, want %v", got, want)
	}
	open := "open"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &open}); err != nil {
		t.Fatalf("reopen work: %v", err)
	}
	if got := resolveTaskOptionOverrides(store, optionSchemaProvider(), "worker-session"); len(got) != 0 {
		t.Fatalf("resolveTaskOptionOverrides(open work) = %v, want empty", got)
	}
}

func TestResolveTaskOptionOverrides_InvalidValueIgnoredPerKey(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newOptionSessionCandidate(t, store, map[string]string{"model": "definitely-not-a-choice", "effort": "high"}, nil)

	want := map[string]string{"effort": "high"}
	if got := resolveTaskOptionOverrides(store, optionSchemaProvider(), taskWorkDirAssignees(candidate, &config.City{})...); !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveTaskOptionOverrides = %v, want %v", got, want)
	}
}

// TestBuildPreparedStartAppliesWorkBeadOptionsToCommand proves the end-to-end
// path: work bead opt_<key> metadata becomes provider CLI flags through
// OptionsSchema, without adding a dedicated field per option.
func TestBuildPreparedStartAppliesWorkBeadOptionsToCommand(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newOptionSessionCandidate(t, store, map[string]string{"model": "opus", "effort": "high"}, nil)

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--model claude-opus-4-8") {
		t.Fatalf("prepared command = %q, want --model claude-opus-4-8", prepared.cfg.Command)
	}
	if !strings.Contains(prepared.cfg.Command, "--effort high") {
		t.Fatalf("prepared command = %q, want --effort high", prepared.cfg.Command)
	}
	metadata := storedSessionMetadata(t, store, candidate.info.ID)
	if got := strings.TrimSpace(metadata["template_overrides"]); got != "" {
		t.Fatalf("template_overrides persisted from work options: %q", got)
	}
	if got := strings.TrimSpace(metadata["opt_model"]); got != "" {
		t.Fatalf("opt_model persisted on session from work option: %q", got)
	}
}

func TestBuildPreparedStartInitialMessageOnlyMatchesDriftHash(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newOptionSessionCandidate(t, store, nil, map[string]string{"initial_message": "hello"})
	resolved := claudeEffortResolvedProvider()
	defaultArgs := resolved.ResolveDefaultArgs()
	if len(defaultArgs) == 0 {
		t.Fatal("claude provider default args are empty")
	}
	candidate.tp.ResolvedProvider = resolved
	candidate.tp.Command = "claude " + shellquote.Join(defaultArgs) + " --settings /tmp/city/.gc/settings.json"

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	want := runtime.CoreFingerprint(sessionCoreConfigForHashInfo(candidate.tp, candidate.info))
	if prepared.coreHash != want {
		t.Fatalf("prepared coreHash = %s, want drift hash %s\nprepared command: %q\ndrift command:    %q",
			prepared.coreHash,
			want,
			prepared.cfg.Command,
			sessionCoreConfigForHashInfo(candidate.tp, candidate.info).Command)
	}
}
