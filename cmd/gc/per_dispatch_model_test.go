package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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

func TestAssignedTaskOptionOverridesReadsAssignedWorkBead(t *testing.T) {
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
	if got, holds, err := assignedTaskOptionOverrides(store, optionSchemaProvider(), "worker-session"); err != nil || !holds || !reflect.DeepEqual(got, want) {
		t.Fatalf("assignedTaskOptionOverrides = (%v, %v, %v), want (%v, true, nil)", got, holds, err, want)
	}
	open := "open"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &open}); err != nil {
		t.Fatalf("reopen work: %v", err)
	}
	if got, holds, err := assignedTaskOptionOverrides(store, optionSchemaProvider(), "worker-session"); err != nil || holds || len(got) != 0 {
		t.Fatalf("assignedTaskOptionOverrides(open work) = (%v, %v, %v), want (empty, false, nil)", got, holds, err)
	}
}

func TestAssignedTaskOptionOverrides_InvalidValueIgnoredPerKey(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newOptionSessionCandidate(t, store, map[string]string{"model": "definitely-not-a-choice", "effort": "high"}, nil)

	want := map[string]string{"effort": "high"}
	if got, _, err := assignedTaskOptionOverrides(store, optionSchemaProvider(), taskWorkDirAssignees(candidate, &config.City{})...); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("assignedTaskOptionOverrides = (%v, %v), want (%v, nil)", got, err, want)
	}
}

func TestAssignedTaskOptionOverrides_ListErrorIsReported(t *testing.T) {
	store := beads.NewMemStore()
	assignInProgressWork(t, store, "worker-session", map[string]string{"effort": "high"})

	got, holds, err := assignedTaskOptionOverrides(&listErrorStore{Store: store}, optionSchemaProvider(), "worker-session")
	if err == nil || !strings.Contains(err.Error(), `"worker-session"`) {
		t.Fatalf("assignedTaskOptionOverrides(List error) err = %v, want an error naming the assignee", err)
	}
	if holds || got != nil {
		t.Fatalf("assignedTaskOptionOverrides(List error) = (%v, %v), want (nil, false)", got, holds)
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

// newTriggerOptionBead creates an open, unassigned routed task bead carrying
// triggerOptions as opt_<key> metadata: the demand a pool slot is spawned for.
func newTriggerOptionBead(t *testing.T, store beads.Store, id string, triggerOptions map[string]string) beads.Bead {
	t.Helper()
	meta := map[string]string{"gc.routed_to": "worker"}
	for key, value := range triggerOptions {
		meta[dispatchOptionMetadataKey(key)] = value
	}
	trigger, err := store.Create(beads.Bead{
		ID:       id,
		Title:    "routed step",
		Type:     "task",
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Create(trigger): %v", err)
	}
	return trigger
}

// newTriggerSessionCandidate creates a pool-style session bead whose
// gc.trigger_bead_id names triggerID (and gc.trigger_bead_store_ref names
// triggerStoreRef when set). No work bead is assigned to it.
func newTriggerSessionCandidate(t *testing.T, store beads.Store, triggerID string, sessionOverrides map[string]string, triggerStoreRef string) startCandidate {
	t.Helper()
	const sessionName = "worker"
	meta := map[string]string{
		"session_name":                    sessionName,
		"template":                        "worker",
		"state":                           "asleep",
		beadmeta.TriggerBeadIDMetadataKey: triggerID,
	}
	if triggerStoreRef != "" {
		meta[beadmeta.TriggerBeadStoreRefMetadataKey] = triggerStoreRef
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

// newTriggerOptionSessionCandidate builds a pool session spawned on unassigned
// demand: its trigger bead carries opt_<key> pins and nothing is claimed yet.
func newTriggerOptionSessionCandidate(t *testing.T, store beads.Store, triggerOptions, sessionOverrides map[string]string, triggerStoreRef string) (startCandidate, beads.Bead) {
	t.Helper()
	trigger := newTriggerOptionBead(t, store, "", triggerOptions)
	return newTriggerSessionCandidate(t, store, trigger.ID, sessionOverrides, triggerStoreRef), trigger
}

// assignInProgressWork creates an in-progress work bead assigned to assignee.
func assignInProgressWork(t *testing.T, store beads.Store, assignee string, workOptions map[string]string) {
	t.Helper()
	meta := map[string]string{}
	for key, value := range workOptions {
		meta[dispatchOptionMetadataKey(key)] = value
	}
	work, err := store.Create(beads.Bead{Title: "claimed step", Type: "task", Assignee: assignee, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
}

// useBuiltinClaude points the candidate at the builtin claude provider with
// the command its default args (including --effort max) produce.
func useBuiltinClaude(t *testing.T, candidate *startCandidate) {
	t.Helper()
	resolved := claudeEffortResolvedProvider()
	defaultArgs := resolved.ResolveDefaultArgs()
	if len(defaultArgs) == 0 {
		t.Fatal("claude provider default args are empty")
	}
	candidate.tp.ResolvedProvider = resolved
	candidate.tp.Command = "claude " + shellquote.Join(defaultArgs) + " --settings /tmp/city/.gc/settings.json"
	if !strings.Contains(candidate.tp.Command, "--effort max") {
		t.Fatalf("builtin claude base command = %q, want default --effort max", candidate.tp.Command)
	}
}

// TestBuildPreparedStartAppliesTriggerBeadOptionsBeforeClaim is gascity#5710:
// a pool session spawned on unassigned demand launches with its trigger bead's
// opt_* pins instead of the provider defaults.
func TestBuildPreparedStartAppliesTriggerBeadOptionsBeforeClaim(t *testing.T) {
	store := beads.NewMemStore()
	candidate, _ := newTriggerOptionSessionCandidate(t, store, map[string]string{"effort": "low", "model": "sonnet"}, nil, "")
	useBuiltinClaude(t, &candidate)

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--effort low") {
		t.Fatalf("prepared command = %q, want trigger opt_effort --effort low", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "--effort max") {
		t.Fatalf("prepared command = %q, provider default --effort max should be replaced", prepared.cfg.Command)
	}
	if !strings.Contains(prepared.cfg.Command, "--model claude-sonnet-5") {
		t.Fatalf("prepared command = %q, want trigger opt_model sonnet --model flag", prepared.cfg.Command)
	}
	metadata := storedSessionMetadata(t, store, candidate.info.ID)
	if got := strings.TrimSpace(metadata["template_overrides"]); got != "" {
		t.Fatalf("template_overrides persisted from trigger options: %q", got)
	}
	for _, key := range []string{"opt_effort", "opt_model"} {
		if got := strings.TrimSpace(metadata[key]); got != "" {
			t.Fatalf("%s persisted on session from trigger option: %q", key, got)
		}
	}
}

func TestBuildPreparedStart_AssignedWorkBeadOptionsBeatTriggerBead(t *testing.T) {
	store := beads.NewMemStore()
	candidate, _ := newTriggerOptionSessionCandidate(t, store, map[string]string{"effort": "low", "model": "opus"}, nil, "")
	assignInProgressWork(t, store, "worker", map[string]string{"effort": "high"})

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--effort high") {
		t.Fatalf("prepared command = %q, want claimed bead --effort high", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "claude-opus-4-8") {
		t.Fatalf("prepared command = %q, trigger opt_model must not mix into a claimed bead's options", prepared.cfg.Command)
	}
}

// TestBuildPreparedStart_ClaimedWorkWithoutOptionsSuppressesTriggerBead pins
// that claimed work beats the trigger on every leg the session can claim from,
// and that a claimed-work read that fails keeps the launch on provider defaults
// instead of reading as "holds nothing".
func TestBuildPreparedStart_ClaimedWorkWithoutOptionsSuppressesTriggerBead(t *testing.T) {
	reachable := func(sessionStore, rigStore beads.Store) claimedWorkProbe {
		return reachableClaimedWorkProbe("", &config.City{}, sessionStore, map[string]beads.Store{"frontend": rigStore})
	}
	failing := func(beads.Store, beads.Store) claimedWorkProbe {
		return func(sessionpkg.Info) (bool, error) { return false, errors.New("rig store unavailable") }
	}
	for _, tc := range []struct {
		name         string
		sessionClaim bool // in-progress work assigned to the session in the session store
		rigClaim     bool // in-progress work assigned to the session in a rig store
		listError    bool // the session store's List fails
		probe        func(sessionStore, rigStore beads.Store) claimedWorkProbe
		wantPins     bool
	}{
		{name: "session-store claim", sessionClaim: true},
		{name: "claimed-work list error", sessionClaim: true, listError: true},
		{name: "rig-store claim", rigClaim: true, probe: reachable},
		{name: "claimed-work probe error", probe: failing},
		{name: "no claimed work", probe: reachable, wantPins: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			rigStore := beads.NewMemStore()
			candidate, _ := newTriggerOptionSessionCandidate(t, store, map[string]string{"effort": "low"}, nil, "")
			if tc.sessionClaim {
				assignInProgressWork(t, store, "worker", nil)
			}
			if tc.rigClaim {
				assignInProgressWork(t, rigStore, "worker", nil)
			}
			var prepareStore beads.Store = store
			if tc.listError {
				prepareStore = &listErrorStore{Store: store}
			}
			var sources dispatchOptionSources
			if tc.probe != nil {
				sources.claimedWork = tc.probe(store, rigStore)
			}

			prepared, _, err := buildPreparedStartWithWorkDirResolver(candidate, "", &config.City{}, prepareStore, nil, sources)
			if err != nil {
				t.Fatalf("buildPreparedStartWithWorkDirResolver: %v", err)
			}
			if got := strings.Contains(prepared.cfg.Command, "--effort low"); got != tc.wantPins {
				t.Fatalf("prepared command = %q, trigger pins applied = %v, want %v", prepared.cfg.Command, got, tc.wantPins)
			}
		})
	}
}

// TestBuildPreparedStart_TriggerClaimedBySessionKeepsItsPins pins the restart
// of a seat that already claimed its rig-resident trigger: the cross-leg probe
// sees that claim, and the trigger's pins still apply because the claimed bead
// is the trigger itself.
func TestBuildPreparedStart_TriggerClaimedBySessionKeepsItsPins(t *testing.T) {
	sessionStore, rigStore, candidate := rigResidentTrigger(t)
	inProgress, assignee := "in_progress", "worker"
	if err := rigStore.Update(candidate.info.TriggerBeadID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claim trigger: %v", err)
	}
	probe := reachableClaimedWorkProbe("", &config.City{}, sessionStore, map[string]beads.Store{"frontend": rigStore})
	if claimed, err := probe(candidate.info); err != nil || !claimed {
		t.Fatalf("claimed-work probe = (%v, %v), want the rig-resident claim visible", claimed, err)
	}

	prepared, _, err := buildPreparedStartWithWorkDirResolver(candidate, "", &config.City{}, sessionStore, nil,
		dispatchOptionSources{trigger: rigStore.Get, claimedWork: probe})
	if err != nil {
		t.Fatalf("buildPreparedStartWithWorkDirResolver: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--effort low") || strings.Contains(prepared.cfg.Command, "--effort high") {
		t.Fatalf("prepared command = %q, want the claimed rig trigger's --effort low", prepared.cfg.Command)
	}
}

func TestBuildPreparedStart_TemplateOverrideBeatsTriggerBeadPerKey(t *testing.T) {
	store := beads.NewMemStore()
	candidate, _ := newTriggerOptionSessionCandidate(t, store,
		map[string]string{"model": "opus", "effort": "low"},
		map[string]string{"model": "sonnet"}, "")

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--model claude-sonnet-4-6") {
		t.Fatalf("prepared command = %q, want explicit --model claude-sonnet-4-6", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "claude-opus-4-8") {
		t.Fatalf("prepared command = %q, trigger opt_model should not override explicit model", prepared.cfg.Command)
	}
	if !strings.Contains(prepared.cfg.Command, "--effort low") {
		t.Fatalf("prepared command = %q, want trigger opt_effort low", prepared.cfg.Command)
	}
}

func TestResolveDispatchOptionOverrides_TriggerInvalidValueSkippedPerKey(t *testing.T) {
	store := beads.NewMemStore()
	candidate, _ := newTriggerOptionSessionCandidate(t, store,
		map[string]string{"model": "definitely-not-a-choice", "effort": "low"}, nil, "")

	want := map[string]string{"effort": "low"}
	if got := resolveDispatchOptionOverrides(candidate, "", &config.City{}, store, optionSchemaProvider(), dispatchOptionSources{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveDispatchOptionOverrides = %v, want %v", got, want)
	}
}

func TestResolveDispatchOptionOverrides_TriggerStepGate(t *testing.T) {
	want := map[string]string{"effort": "low"}
	tests := []struct {
		name     string
		status   string
		assignee string
		want     map[string]string
	}{
		{name: "closed", status: "closed", want: nil},
		{name: "claimed by another session", status: "in_progress", assignee: "other-session", want: nil},
		{name: "open assigned to this session", status: "open", assignee: "worker", want: want},
		{name: "open unassigned", status: "open", want: want},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			candidate, trigger := newTriggerOptionSessionCandidate(t, store, map[string]string{"effort": "low"}, nil, "")
			opts := beads.UpdateOpts{}
			if tc.assignee != "" {
				assignee := tc.assignee
				opts.Assignee = &assignee
			}
			if tc.status != "open" {
				status := tc.status
				opts.Status = &status
			}
			if err := store.Update(trigger.ID, opts); err != nil {
				t.Fatalf("Update(trigger): %v", err)
			}
			got := resolveDispatchOptionOverrides(candidate, "", &config.City{}, store, optionSchemaProvider(), dispatchOptionSources{})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolveDispatchOptionOverrides = %v, want %v", got, tc.want)
			}
		})
	}
}

// rigResidentTrigger seeds a session store and a separate rig store that both
// hold a bead with the same id; only the rig copy is the real trigger.
func rigResidentTrigger(t *testing.T) (sessionStore *beads.MemStore, rigStore *beads.MemStore, candidate startCandidate) {
	t.Helper()
	const triggerID = "fe-42"
	rigStore = &beads.MemStore{HonorExplicitIDs: true}
	newTriggerOptionBead(t, rigStore, triggerID, map[string]string{"effort": "low"})
	sessionStore = &beads.MemStore{HonorExplicitIDs: true}
	newTriggerOptionBead(t, sessionStore, triggerID, map[string]string{"effort": "high"})
	candidate = newTriggerSessionCandidate(t, sessionStore, triggerID, nil, "rig:frontend")
	return sessionStore, rigStore, candidate
}

func TestResolveDispatchOptionOverrides_UsesTriggerResolver(t *testing.T) {
	sessionStore, rigStore, candidate := rigResidentTrigger(t)

	want := map[string]string{"effort": "low"}
	if got := resolveDispatchOptionOverrides(candidate, "", &config.City{}, sessionStore, optionSchemaProvider(), dispatchOptionSources{trigger: rigStore.Get}); !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveDispatchOptionOverrides(resolver) = %v, want %v", got, want)
	}

	missing := func(string) (beads.Bead, error) { return beads.Bead{}, beads.ErrNotFound }
	if got := resolveDispatchOptionOverrides(candidate, "", &config.City{}, sessionStore, optionSchemaProvider(), dispatchOptionSources{trigger: missing}); got != nil {
		t.Fatalf("resolveDispatchOptionOverrides(resolver miss) = %v, want nil without session-store fallback", got)
	}
}

func TestResolveDispatchOptionOverrides_NoResolverSkipsForeignStoreStamp(t *testing.T) {
	sessionStore, _, candidate := rigResidentTrigger(t)
	if got := resolveDispatchOptionOverrides(candidate, "", &config.City{}, sessionStore, optionSchemaProvider(), dispatchOptionSources{}); got != nil {
		t.Fatalf("resolveDispatchOptionOverrides(rig stamp, no resolver) = %v, want nil", got)
	}

	for _, ref := range []string{"", "city"} {
		store := beads.NewMemStore()
		candidate, _ := newTriggerOptionSessionCandidate(t, store, map[string]string{"effort": "low"}, nil, ref)
		want := map[string]string{"effort": "low"}
		if got := resolveDispatchOptionOverrides(candidate, "", &config.City{}, store, optionSchemaProvider(), dispatchOptionSources{}); !reflect.DeepEqual(got, want) {
			t.Fatalf("resolveDispatchOptionOverrides(ref %q) = %v, want %v", ref, got, want)
		}
	}
}

func TestBuildPreparedStart_TriggerBeadOptionsDoNotChangeCoreHash(t *testing.T) {
	baselineStore := beads.NewMemStore()
	baseline, _ := newTriggerOptionSessionCandidate(t, baselineStore, nil, nil, "")
	useBuiltinClaude(t, &baseline)
	baselinePrepared, _, err := buildPreparedStart(baseline, &config.City{}, baselineStore)
	if err != nil {
		t.Fatalf("buildPreparedStart(baseline): %v", err)
	}

	store := beads.NewMemStore()
	candidate, _ := newTriggerOptionSessionCandidate(t, store, map[string]string{"effort": "low", "model": "sonnet"}, nil, "")
	useBuiltinClaude(t, &candidate)
	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--effort low") {
		t.Fatalf("prepared command = %q, want trigger --effort low", prepared.cfg.Command)
	}
	want := runtime.CoreFingerprint(sessionCoreConfigForHashInfo(candidate.tp, candidate.info))
	if prepared.coreHash != want {
		t.Fatalf("prepared coreHash = %s, want drift hash %s (trigger pins must not cause drift)", prepared.coreHash, want)
	}
	if prepared.launchHash != baselinePrepared.launchHash {
		t.Fatalf("prepared launchHash = %s, want trigger-free %s", prepared.launchHash, baselinePrepared.launchHash)
	}
}

func TestPrepareStartCandidateForCityThreadsTriggerBeadResolver(t *testing.T) {
	sessionStore, rigStore, candidate := rigResidentTrigger(t)
	prepared, err := prepareStartCandidateForCity(candidate, "", "", &config.City{}, nil, sessionStore,
		&clock.Fake{Time: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}, io.Discard, nil, dispatchOptionSources{trigger: rigStore.Get})
	if err != nil {
		t.Fatalf("prepareStartCandidateForCity: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--effort low") {
		t.Fatalf("prepared command = %q, want rig-resident trigger --effort low", prepared.cfg.Command)
	}
}

// TestExecutePlannedStartsAppliesRigResidentTriggerOptionsViaResolver covers the
// executePlannedStarts plumbing: withTriggerBeadResolver must reach the options
// lookup so a rig-resident trigger's opt_* pins reach the started command.
// Without the resolver the rig stamp is not read and the session-store decoy
// (effort=high) must not leak in either.
func TestExecutePlannedStartsAppliesRigResidentTriggerOptionsViaResolver(t *testing.T) {
	for _, tc := range []struct {
		name         string
		withResolver bool
		want         string
		forbidden    []string
	}{
		{name: "with resolver", withResolver: true, want: "--effort low", forbidden: []string{"--effort high"}},
		{name: "without resolver", withResolver: false, forbidden: []string{"--effort low", "--effort high"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionStore, rigStore, candidate := rigResidentTrigger(t)
			sp := runtime.NewFake()
			clk := &clock.Fake{Time: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}
			cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
			options := []startExecutionOption{
				withStartStabilityWaiter(immediateStartStabilityWaiter),
				withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
			}
			if tc.withResolver {
				options = append(options, withTriggerBeadResolver(rigStore.Get))
			}

			woken := executePlannedStartsTraced(
				context.Background(),
				[]startCandidate{candidate},
				cfg,
				map[string]TemplateParams{"worker": candidate.tp},
				sp,
				sessionStore,
				"",
				t.TempDir(),
				clk,
				events.Discard,
				5*time.Second,
				io.Discard,
				io.Discard,
				nil,
				options...,
			)
			if woken != 1 {
				t.Fatalf("woken = %d, want 1", woken)
			}
			var command string
			for _, call := range sp.Calls {
				if call.Method == "Start" && call.Name == "worker" {
					command = call.Config.Command
					break
				}
			}
			if command == "" {
				t.Fatalf("expected Start call for worker, calls=%#v", sp.Calls)
			}
			if tc.want != "" && !strings.Contains(command, tc.want) {
				t.Fatalf("Start command = %q, want %q", command, tc.want)
			}
			for _, bad := range tc.forbidden {
				if strings.Contains(command, bad) {
					t.Fatalf("Start command = %q, must not contain %q", command, bad)
				}
			}
		})
	}
}

// TestCityRuntimeBeadReconcileTick_AppliesTriggerPinsThroughResidencyReader pins
// the controller's install of the trigger resolver and the claimed-work probe.
// Each seat carries the rig demand-leg stamp its binder wrote, which the
// resolver-less fallback refuses to read through the session store, so only the
// tick's residency reader can supply the trigger's opt_* pins: from the rig that
// holds the trigger, or from the class binding that holds a graph-class step on
// a split city. A seat that already holds claimed work in a rig launches on
// defaults, because the tick's probe reads every serving leg, not just the
// session store. A suspended rig is not a serving leg: the probe sends it no
// read at all (suspension is quiescence), so a claim there does not suppress
// the trigger.
func TestCityRuntimeBeadReconcileTick_AppliesTriggerPinsThroughResidencyReader(t *testing.T) {
	for _, tc := range []struct {
		name              string
		triggerID         string
		split             bool
		rigClaim          bool
		suspendedRigClaim bool
		wantCommand       string
	}{
		{name: "rig-resident trigger", triggerID: "ra-1", wantCommand: "claude --effort low"},
		{name: "binding-resident graph-class trigger on a split city", triggerID: "gcg-1", split: true, wantCommand: "claude --effort low"},
		{name: "rig-resident claimed work suppresses the trigger", triggerID: "ra-1", rigClaim: true, wantCommand: "claude"},
		{name: "a suspended rig is not read for claimed work", triggerID: "ra-1", suspendedRigClaim: true, wantCommand: "claude --effort low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := beads.NewMemStore()
			rig := &beads.MemStore{HonorExplicitIDs: true}
			rigStores := map[string]beads.Store{"alpha": rig}
			suspendedRig := &readCountingStore{Store: beads.NewMemStore()}
			if tc.suspendedRigClaim {
				assignInProgressWork(t, suspendedRig.Store, "worker-1", nil)
				rigStores["bravo"] = suspendedRig
			}
			sessions, triggerStore := beads.Store(work), beads.Store(rig)
			var routes *storageRoutes
			if tc.split {
				binding := &beads.MemStore{HonorExplicitIDs: true}
				routes = splitRoutes(binding)
				sessions, triggerStore = binding, binding
			}
			trigger := newTriggerOptionBead(t, triggerStore, tc.triggerID, map[string]string{"effort": "low"})
			if tc.rigClaim {
				assignInProgressWork(t, rig, "worker-1", nil)
			}
			if _, err := sessions.Create(beads.Bead{
				Title:  "worker",
				Type:   sessionBeadType,
				Labels: []string{sessionBeadLabel, "agent:worker"},
				Metadata: map[string]string{
					"session_name":                          "worker-1",
					"template":                              "worker",
					"agent_name":                            "worker",
					"pool_slot":                             "1",
					poolManagedMetadataKey:                  boolMetadata(true),
					"state":                                 string(sessionpkg.StateStartPending),
					"pending_create_claim":                  "true",
					"generation":                            "1",
					beadmeta.TriggerBeadIDMetadataKey:       trigger.ID,
					beadmeta.TriggerBeadStoreRefMetadataKey: "rig:alpha",
				},
			}); err != nil {
				t.Fatalf("Create session bead: %v", err)
			}
			cfg := residencyTestConfig()
			// A city-scoped pool serves per-rig routed work, so its triggers and
			// claims can live in a rig store.
			cfg.Agents = []config.Agent{{Name: "worker", Scope: "city", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}
			cfg.Rigs[1].SuspendedOnStart = tc.suspendedRigClaim
			sp := runtime.NewFake()
			cr := &CityRuntime{
				cityPath:            t.TempDir(),
				cityName:            "test-city",
				cfg:                 cfg,
				sp:                  sp,
				standaloneCityStore: work,
				standaloneRigStores: rigStores,
				storageRoutes:       routes,
				sessionDrains:       newDrainTracker(),
				rec:                 events.Discard,
				stdout:              io.Discard,
				stderr:              io.Discard,
			}

			cr.beadReconcileTick(context.Background(), DesiredStateResult{
				State: map[string]TemplateParams{"worker-1": {
					TemplateName:     "worker",
					SessionName:      "worker-1",
					Command:          "claude",
					ResolvedProvider: optionSchemaProvider(),
				}},
				PoolDesiredCounts: map[string]int{"worker": 1},
			}, cr.loadSessionBeadSnapshot(), nil, true)
			if !cr.waitForAsyncStarts() {
				t.Fatal("async starts did not settle after the tick")
			}

			var starts []runtime.Call
			for _, call := range sp.SnapshotCalls() {
				if call.Method == "Start" {
					starts = append(starts, call)
				}
			}
			if len(starts) != 1 {
				t.Fatalf("runtime Start calls = %#v, want one start of the bound pool seat", starts)
			}
			if got := starts[0].Config.Command; got != tc.wantCommand {
				t.Fatalf("started command = %q, want %q", got, tc.wantCommand)
			}
			if got := suspendedRig.reads.Load(); got != 0 {
				t.Fatalf("suspended rig store reads = %d, want none", got)
			}
		})
	}
}

// readCountingStore counts the reads a store answers, so a test can pin that a
// scope gets none.
type readCountingStore struct {
	beads.Store
	reads atomic.Int32
}

func (s *readCountingStore) Get(id string) (beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.Get(id)
}

func (s *readCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.List(query)
}
