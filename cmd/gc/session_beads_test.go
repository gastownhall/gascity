package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type countingMetadataStore struct {
	*beads.MemStore
	singleCalls int
	batchCalls  int
}

func newCountingMetadataStore() *countingMetadataStore {
	return &countingMetadataStore{MemStore: beads.NewMemStore()}
}

func (s *countingMetadataStore) SetMetadata(id, key, value string) error {
	s.singleCalls++
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *countingMetadataStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.batchCalls++
	return s.MemStore.SetMetadataBatch(id, kvs)
}

// allConfiguredDS builds configuredNames from a desiredState map.
func allConfiguredDS(ds map[string]TemplateParams) map[string]bool {
	m := make(map[string]bool, len(ds))
	for sn := range ds {
		m[sn] = true
	}
	return m
}

func allSessionBeads(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	all, err := store.ListByLabel(sessionBeadLabel, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	return all
}

func TestSyncSessionBeads_CreatesNewBeads(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "mayor", runtime.Config{Command: "claude"})

	ds := map[string]TemplateParams{
		"mayor": {TemplateName: "mayor", Command: "claude"},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}

	all := allSessionBeads(t, store)
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}

	b := all[0]
	if b.Type != sessionBeadType {
		t.Errorf("type = %q, want %q", b.Type, sessionBeadType)
	}
	if b.Metadata["session_name"] != "mayor" {
		t.Errorf("session_name = %q, want %q", b.Metadata["session_name"], "mayor")
	}
	if b.Metadata["state"] != "active" {
		t.Errorf("state = %q, want %q", b.Metadata["state"], "active")
	}
	if b.Metadata["generation"] != "1" {
		t.Errorf("generation = %q, want %q", b.Metadata["generation"], "1")
	}
	if b.Metadata["continuation_epoch"] != "1" {
		t.Errorf("continuation_epoch = %q, want %q", b.Metadata["continuation_epoch"], "1")
	}
	if b.Metadata["instance_token"] == "" {
		t.Error("instance_token is empty")
	}
}

func TestSyncSessionBeads_SetsManagedAlias(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "witness", Dir: "myrig"},
		},
	}

	ds := map[string]TemplateParams{
		"s-gc-123": {
			TemplateName: "myrig/witness",
			InstanceName: "myrig/witness",
			Alias:        "myrig/witness",
			Command:      "claude",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}
	if got := all[0].Metadata["alias"]; got != "myrig/witness" {
		t.Fatalf("alias = %q, want %q", got, "myrig/witness")
	}
}

func TestSyncSessionBeads_DoesNotCreateFallbackForConfiguredNamedConflict(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "witness", Dir: "myrig"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "witness", Dir: "myrig"},
		},
	}

	if _, err := store.Create(beads.Bead{
		Title:  "squatter",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "other-session",
			"alias":        "myrig/witness",
			"template":     "myrig/witness",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("creating conflicting bead: %v", err)
	}

	ds := map[string]TemplateParams{
		"myrig--witness": {
			TemplateName:            "myrig/witness",
			InstanceName:            "myrig/witness",
			Alias:                   "myrig/witness",
			Command:                 "claude",
			ConfiguredNamedIdentity: "myrig/witness",
			ConfiguredNamedMode:     "on_demand",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected only the conflicting bead to remain, got %d beads", len(all))
	}
	if got := all[0].Metadata["session_name"]; got != "other-session" {
		t.Fatalf("unexpected fallback bead created; first session_name = %q", got)
	}
	if got := all[0].Status; got == "closed" {
		t.Fatalf("conflicting bead was closed; metadata=%v", all[0].Metadata)
	}
	if got := all[0].Metadata["close_reason"]; got != "" {
		t.Fatalf("close_reason = %q, want empty for preserved conflict bead", got)
	}
	if !strings.Contains(stderr.String(), "alias \"myrig/witness\"") {
		t.Fatalf("stderr = %q, want alias conflict warning", stderr.String())
	}
	if !strings.Contains(stderr.String(), "blocks configured named session") {
		t.Fatalf("stderr = %q, want preserved-conflict diagnostic", stderr.String())
	}
}

func TestSyncSessionBeads_RetiresRemovedNamedSessionAndCreatesFreshOnReadd(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()

	cfgNamed := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "witness", Dir: "myrig"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "witness", Dir: "myrig"},
		},
	}
	cfgPlain := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "witness", Dir: "myrig"},
		},
	}

	ds := map[string]TemplateParams{
		"myrig--witness": {
			TemplateName:            "myrig/witness",
			InstanceName:            "myrig/witness",
			Alias:                   "myrig/witness",
			Command:                 "claude",
			ConfiguredNamedIdentity: "myrig/witness",
			ConfiguredNamedMode:     "on_demand",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), cfgNamed, clk, &stderr, false)

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads after create: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 bead after create, got %d", len(all))
	}
	originalID := all[0].ID
	assignedOpen, err := store.Create(beads.Bead{
		Title:    "open work owned by removed named session",
		Type:     "task",
		Assignee: originalID,
	})
	if err != nil {
		t.Fatalf("Create(assigned open work): %v", err)
	}
	assignedInProgress, err := store.Create(beads.Bead{
		Title:    "in-progress work owned by removed named session",
		Type:     "task",
		Assignee: originalID,
	})
	if err != nil {
		t.Fatalf("Create(assigned in-progress work): %v", err)
	}
	inProgressStatus := "in_progress"
	if err := store.Update(assignedInProgress.ID, beads.UpdateOpts{Status: &inProgressStatus}); err != nil {
		t.Fatalf("Update(%s, in_progress): %v", assignedInProgress.ID, err)
	}
	wait, err := store.Create(beads.Bead{
		Title:  "removed wait",
		Type:   session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel, "session:" + originalID},
		Metadata: map[string]string{
			"session_id": originalID,
			"state":      "open",
			"nudge_id":   "nudge-removed",
		},
	})
	if err != nil {
		t.Fatalf("Create(wait): %v", err)
	}
	fabric := extmsg.NewServices(store)
	ref := extmsg.ConversationRef{
		ScopeID:        "test-city",
		Provider:       "discord",
		AccountID:      "acct",
		ConversationID: "thread-removed",
		Kind:           extmsg.ConversationThread,
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	if _, err := fabric.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		SessionID:    originalID,
		Now:          clk.Now(),
	}); err != nil {
		t.Fatalf("Bind(original named session): %v", err)
	}

	clk.Advance(5 * time.Second)
	syncSessionBeads("", store, nil, sp, map[string]bool{}, cfgPlain, clk, &stderr, false)

	all, err = store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads after downgrade: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 bead after downgrade, got %d", len(all))
	}
	if got := all[0].Status; got != "archived" {
		t.Fatalf("status after removal = %q, want archived", got)
	}
	if got := all[0].Metadata[namedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session after removal = %q, want true historical marker", got)
	}
	if got := all[0].Metadata[namedSessionIdentityMetadata]; got != "myrig/witness" {
		t.Fatalf("configured_named_identity after removal = %q, want preserved identity", got)
	}
	if got := all[0].Metadata["alias"]; got != "" {
		t.Fatalf("alias after removal = %q, want cleared", got)
	}
	if got := all[0].Metadata["session_name"]; got != "" {
		t.Fatalf("session_name after removal = %q, want cleared", got)
	}
	for _, id := range []string{assignedOpen.ID, assignedInProgress.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Assignee != "" {
			t.Fatalf("work bead %s assignee = %q, want unclaimed after named session removal", id, got.Assignee)
		}
	}
	gotWait, err := store.Get(wait.ID)
	if err != nil {
		t.Fatalf("Get(wait): %v", err)
	}
	if gotWait.Status != "closed" || gotWait.Metadata["state"] != "canceled" {
		t.Fatalf("removed-session wait status/state = %q/%q, want closed/canceled", gotWait.Status, gotWait.Metadata["state"])
	}
	gotBinding, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation(after removal): %v", err)
	}
	if gotBinding != nil {
		t.Fatalf("binding after named session removal = %#v, want nil", gotBinding)
	}
	memberships, err := fabric.Transcript.ListMemberships(context.Background(), caller, ref)
	if err != nil {
		t.Fatalf("ListMemberships(after removal): %v", err)
	}
	if len(memberships) != 0 {
		t.Fatalf("memberships after named session removal = %#v, want none", memberships)
	}

	clk.Advance(5 * time.Second)
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), cfgNamed, clk, &stderr, false)

	all, err = store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads after re-adopt: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected only fresh open bead after re-add listing, got %d", len(all))
	}
	fresh := all[0]
	if fresh.ID == originalID {
		t.Fatalf("fresh bead ID = %q, want a new bead after archived removal", fresh.ID)
	}
	historical, err := store.Get(originalID)
	if err != nil {
		t.Fatalf("Get(original archived bead): %v", err)
	}
	if historical.Status == "open" {
		t.Fatalf("historical status = %q, want non-open", historical.Status)
	}
	if got := fresh.Metadata[namedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session after re-adopt = %q, want true", got)
	}
	if got := fresh.Metadata[namedSessionIdentityMetadata]; got != "myrig/witness" {
		t.Fatalf("configured_named_identity after re-adopt = %q, want myrig/witness", got)
	}
}

func TestSyncSessionBeads_AdoptsCanonicalSessionNameBeadIntoConfiguredNamedSession(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "witness", Dir: "myrig"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "witness", Dir: "myrig", Mode: "always"},
		},
	}

	if _, err := store.Create(beads.Bead{
		Title:  "witness",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "myrig--witness",
			"template":     "myrig/witness",
			"agent_name":   "myrig/witness",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("creating canonical-session-name bead: %v", err)
	}

	ds := map[string]TemplateParams{
		"myrig--witness": {
			TemplateName:            "myrig/witness",
			InstanceName:            "myrig/witness",
			Alias:                   "myrig/witness",
			Command:                 "claude",
			ConfiguredNamedIdentity: "myrig/witness",
			ConfiguredNamedMode:     "always",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads after adopt: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 bead after adopt, got %d", len(all))
	}
	if got := all[0].Metadata["alias"]; got != "myrig/witness" {
		t.Fatalf("alias after adopt = %q, want myrig/witness", got)
	}
	if got := all[0].Metadata[namedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session after adopt = %q, want true", got)
	}
	if got := all[0].Metadata[namedSessionIdentityMetadata]; got != "myrig/witness" {
		t.Fatalf("configured_named_identity after adopt = %q, want myrig/witness", got)
	}
	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestSyncSessionBeads_ReopensClosedConfiguredNamedSession(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "refinery", StartCommand: "true", MaxActiveSessions: intPtr(2)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "refinery", Mode: "on_demand"},
		},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "refinery")
	closed, err := store.Create(beads.Bead{
		Title:  "refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "refinery",
			"template":                   "refinery",
			"state":                      "suspended",
			"close_reason":               "suspended",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "refinery",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create closed canonical bead: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("close canonical bead: %v", err)
	}

	ds := map[string]TemplateParams{
		sessionName: {
			TemplateName:            "refinery",
			InstanceName:            "refinery",
			Alias:                   "refinery",
			Command:                 "true",
			ConfiguredNamedIdentity: "refinery",
			ConfiguredNamedMode:     "on_demand",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(all))
	}
	if all[0].ID != closed.ID {
		t.Fatalf("reopened bead ID = %q, want %q", all[0].ID, closed.ID)
	}
	if all[0].Status != "open" {
		t.Fatalf("status = %q, want open", all[0].Status)
	}
	if got := all[0].Metadata["close_reason"]; got != "" {
		t.Fatalf("close_reason = %q, want empty", got)
	}
	if got := all[0].Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
	if got := all[0].Metadata["session_name"]; got != sessionName {
		t.Fatalf("session_name = %q, want %q", got, sessionName)
	}
}

func TestSyncSessionBeads_UpdatesNamedModeForWizardMayor(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := config.WizardCity("test-city", "", "true")

	initial := buildDesiredState("test-city", cityPath, clk.Now(), &cfg, sp, store, io.Discard)
	if len(initial.State) == 0 {
		t.Fatal("initial desired state is empty, want canonical mayor session")
	}

	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, initial.State, sp, allConfiguredDS(initial.State), &cfg, clk, &stderr, false)

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing initial beads: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("initial session bead count = %d, want 1", len(all))
	}
	if got := all[0].Metadata[namedSessionModeMetadata]; got != "always" {
		t.Fatalf("initial configured_named_mode = %q, want always", got)
	}

	cfg.NamedSessions[0].Mode = "on_demand"
	updated := buildDesiredState("test-city", cityPath, clk.Now(), &cfg, sp, store, io.Discard)
	if len(updated.State) == 0 {
		t.Fatal("updated desired state is empty, want canonical mayor session")
	}

	syncSessionBeads(cityPath, store, updated.State, sp, allConfiguredDS(updated.State), &cfg, clk, &stderr, false)

	all, err = store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing updated beads: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("updated session bead count = %d, want 1", len(all))
	}
	if got := all[0].Metadata[namedSessionModeMetadata]; got != "on_demand" {
		t.Fatalf("updated configured_named_mode = %q, want on_demand", got)
	}
}

func TestSyncSessionBeads_DoesNotReopenConfiguredNamedSessionAcrossLiveConflict(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "refinery", StartCommand: "true", MaxActiveSessions: intPtr(2)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "refinery", Mode: "on_demand"},
		},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "refinery")
	closed, err := store.Create(beads.Bead{
		Title:  "refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "refinery",
			"template":                   "refinery",
			"state":                      "suspended",
			"close_reason":               "suspended",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "refinery",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create closed canonical bead: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("close canonical bead: %v", err)
	}
	blocker, err := store.Create(beads.Bead{
		Title:  "squatter",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "other-session",
			"alias":        "refinery",
			"template":     "refinery",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create live conflict bead: %v", err)
	}

	ds := map[string]TemplateParams{
		sessionName: {
			TemplateName:            "refinery",
			InstanceName:            "refinery",
			Alias:                   "refinery",
			Command:                 "true",
			ConfiguredNamedIdentity: "refinery",
			ConfiguredNamedMode:     "on_demand",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	reopened, err := store.Get(closed.ID)
	if err != nil {
		t.Fatalf("Get(closed): %v", err)
	}
	if reopened.Status != "closed" {
		t.Fatalf("historical bead status = %q, want closed", reopened.Status)
	}
	gotBlocker, err := store.Get(blocker.ID)
	if err != nil {
		t.Fatalf("Get(blocker): %v", err)
	}
	if gotBlocker.Status != "open" {
		t.Fatalf("blocker status = %q, want open", gotBlocker.Status)
	}

	open, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open session bead count = %d, want 1", len(open))
	}
	if open[0].ID != blocker.ID {
		t.Fatalf("open bead = %q, want blocker %q", open[0].ID, blocker.ID)
	}
	if !strings.Contains(stderr.String(), "unavailable during reopen") {
		t.Fatalf("stderr = %q, want reopen conflict diagnostic", stderr.String())
	}
}

func TestRetireDuplicateConfiguredNamedSessionBeads_DoesNotStopWinnerSharingSessionName(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "reviewer", StartCommand: "true"},
		},
		NamedSessions: []config.NamedSession{
			{Name: "mayor", Template: "reviewer", Mode: "on_demand"},
		},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "mayor")
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("start shared session %s: %v", sessionName, err)
	}
	loser, err := store.Create(beads.Bead{
		Title:  "mayor old",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"template":                   "reviewer",
			"generation":                 "1",
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create loser: %v", err)
	}
	winner, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"template":                   "reviewer",
			"generation":                 "2",
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create winner: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "owned work",
		Type:     "task",
		Status:   "open",
		Assignee: loser.ID,
	})
	if err != nil {
		t.Fatalf("create loser-owned work: %v", err)
	}
	wait, err := store.Create(beads.Bead{
		Title:  "loser wait",
		Type:   session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel, "session:" + loser.ID},
		Metadata: map[string]string{
			"session_id": loser.ID,
			"state":      "open",
			"nudge_id":   "nudge-loser",
		},
	})
	if err != nil {
		t.Fatalf("create loser wait: %v", err)
	}
	fabric := extmsg.NewServices(store)
	ref := extmsg.ConversationRef{
		ScopeID:        "test-city",
		Provider:       "discord",
		AccountID:      "acct",
		ConversationID: "thread-duplicate",
		Kind:           extmsg.ConversationThread,
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	if _, err := fabric.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		SessionID:    loser.ID,
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("bind loser session: %v", err)
	}
	openBeads := []beads.Bead{loser, winner}
	bySessionName := map[string]beads.Bead{sessionName: winner}
	indexBySessionName := map[string]int{sessionName: 1}

	retired := retireDuplicateConfiguredNamedSessionBeads(
		store, sp, cfg, "test-city", openBeads, bySessionName, indexBySessionName, time.Now().UTC(), io.Discard,
	)

	if !sp.IsRunning(sessionName) {
		t.Fatalf("shared runtime session %q was stopped while winner still owns it", sessionName)
	}
	if retired[0].Status != "archived" {
		t.Fatalf("loser status = %q, want archived", retired[0].Status)
	}
	if retired[1].Status != "open" {
		t.Fatalf("winner status = %q, want open", retired[1].Status)
	}
	updatedWork, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get loser-owned work: %v", err)
	}
	if updatedWork.Assignee != winner.ID {
		t.Fatalf("loser-owned work assignee = %q, want winner %q", updatedWork.Assignee, winner.ID)
	}
	updatedWait, err := store.Get(wait.ID)
	if err != nil {
		t.Fatalf("get loser wait: %v", err)
	}
	if updatedWait.Metadata["session_id"] != winner.ID {
		t.Fatalf("loser wait session_id = %q, want winner %q", updatedWait.Metadata["session_id"], winner.ID)
	}
	nudges, err := session.WaitNudgeIDs(store, winner.ID)
	if err != nil {
		t.Fatalf("WaitNudgeIDs(winner): %v", err)
	}
	if len(nudges) != 1 || nudges[0] != "nudge-loser" {
		t.Fatalf("winner wait nudges = %#v, want [nudge-loser]", nudges)
	}
	oldNudges, err := session.WaitNudgeIDs(store, loser.ID)
	if err != nil {
		t.Fatalf("WaitNudgeIDs(loser): %v", err)
	}
	if len(oldNudges) != 0 {
		t.Fatalf("loser wait nudges = %#v, want none after reassignment", oldNudges)
	}
	gotBinding, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation(after duplicate repair): %v", err)
	}
	if gotBinding == nil || gotBinding.SessionID != winner.ID {
		t.Fatalf("binding after duplicate repair = %#v, want winner %s", gotBinding, winner.ID)
	}
}

func TestSyncSessionBeads_PreservesConfiguredNamedSessionWithoutDesiredEntry(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "refinery", StartCommand: "true", MaxActiveSessions: intPtr(2)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "refinery", Mode: "on_demand"},
		},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "refinery")
	bead, err := store.Create(beads.Bead{
		Title:  "refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "refinery",
			"template":                   "refinery",
			"state":                      "stopped",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "refinery",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create canonical bead: %v", err)
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, nil, sp, map[string]bool{sessionName: true}, cfg, clk, &stderr, false)

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty", got.Metadata["close_reason"])
	}
}

func TestSyncSessionBeads_RecreatesDriftedNamedSessionRuntimeName(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "witness", Dir: "myrig"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "witness", Dir: "myrig"},
		},
	}
	identity := "myrig/witness"
	expectedName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, identity)
	oldName := "s-gc-old"

	if _, err := store.Create(beads.Bead{
		Title:  "myrig/witness",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               oldName,
			"alias":                      identity,
			"template":                   identity,
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
			namedSessionModeMetadata:     "on_demand",
		},
	}); err != nil {
		t.Fatalf("creating drifted canonical bead: %v", err)
	}
	if err := sp.Start(context.Background(), oldName, runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("starting drifted runtime: %v", err)
	}

	ds := map[string]TemplateParams{
		expectedName: {
			TemplateName:            identity,
			InstanceName:            identity,
			Alias:                   identity,
			Command:                 "claude",
			ConfiguredNamedIdentity: identity,
			ConfiguredNamedMode:     "on_demand",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	all := allSessionBeads(t, store)
	if len(all) != 2 {
		t.Fatalf("session bead count = %d, want 2", len(all))
	}
	var (
		closedOld beads.Bead
		openNew   beads.Bead
	)
	for _, b := range all {
		switch strings.TrimSpace(b.Metadata["session_name"]) {
		case oldName:
			closedOld = b
		case expectedName:
			openNew = b
		}
	}
	if closedOld.ID == "" {
		t.Fatalf("did not find closed drifted bead among %+v", all)
	}
	if closedOld.Status != "closed" || closedOld.Metadata["close_reason"] != "reconfigured" {
		t.Fatalf("drifted bead status=%q close_reason=%q, want closed/reconfigured", closedOld.Status, closedOld.Metadata["close_reason"])
	}
	if openNew.ID == "" {
		t.Fatalf("did not find recreated canonical bead with session_name %q", expectedName)
	}
	if got := openNew.Metadata["alias"]; got != identity {
		t.Fatalf("new bead alias = %q, want %q", got, identity)
	}
	if sp.IsRunning(oldName) {
		t.Fatalf("drifted runtime %q still running after reconcile", oldName)
	}
}

func TestSyncSessionBeads_KeepsDiscoveredPlainTemplateSessionOpen(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "helper", StartCommand: "echo"},
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper chat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":       "helper",
			"session_name":   "s-gc-plain",
			"state":          "active",
			"manual_session": "true",
		},
	})
	if err != nil {
		t.Fatalf("creating plain template bead: %v", err)
	}

	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, clk.Now(), store, io.Discard)
	desired := make(map[string]TemplateParams)
	discoverSessionBeads(bp, cfg, desired, io.Discard)
	if _, ok := desired["s-gc-plain"]; !ok {
		t.Fatalf("discoverSessionBeads() missing plain session, got keys: %v", mapKeys(desired))
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, desired, sp, configuredSessionNamesWithSnapshot(cfg, "", nil), cfg, clk, &stderr, false)

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("plain template session was closed unexpectedly: close_reason=%q stderr=%q", got.Metadata["close_reason"], stderr.String())
	}
	if got.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty", got.Metadata["close_reason"])
	}
}

func TestSyncSessionBeads_PreservesManualSessionExplicitAlias(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	sessionName := "s-gc-hal"
	if err := sp.Start(context.TODO(), sessionName, runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("start runtime session: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "helper", StartCommand: "echo"},
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "hal",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":       "helper",
			"session_name":   sessionName,
			"alias":          "hal",
			"state":          "active",
			"manual_session": "true",
		},
	})
	if err != nil {
		t.Fatalf("creating manual helper bead: %v", err)
	}

	bp := newAgentBuildParams("test-city", cityPath, cfg, sp, clk.Now(), store, io.Discard)
	desired := make(map[string]TemplateParams)
	discoverSessionBeads(bp, cfg, desired, io.Discard)

	tp, ok := desired[sessionName]
	if !ok {
		t.Fatalf("discoverSessionBeads() missing manual session, got keys: %v", mapKeys(desired))
	}
	if tp.Alias != "hal" {
		t.Fatalf("discovered alias = %q, want %q", tp.Alias, "hal")
	}

	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, desired, sp, configuredSessionNames(cfg, "test-city", store), cfg, clk, &stderr, false)

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	if got.Metadata["alias"] != "hal" {
		t.Fatalf("alias after sync = %q, want %q (stderr=%q)", got.Metadata["alias"], "hal", stderr.String())
	}

	resolvedID, err := resolveSessionIDWithConfig(cityPath, cfg, store, "hal")
	if err != nil {
		t.Fatalf("resolveSessionIDWithConfig(hal): %v", err)
	}
	if resolvedID != bead.ID {
		t.Fatalf("resolveSessionIDWithConfig(hal) = %q, want %q", resolvedID, bead.ID)
	}
}

func TestSyncSessionBeads_PreservesManagedAliasHistory(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()

	if _, err := store.Create(beads.Bead{
		Title:  "myrig/witness",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "s-gc-123",
			"template":     "myrig/witness",
			"alias":        "old-witness",
			"state":        "stopped",
		},
	}); err != nil {
		t.Fatalf("Create(seed): %v", err)
	}

	ds := map[string]TemplateParams{
		"s-gc-123": {
			TemplateName: "myrig/witness",
			InstanceName: "myrig/witness",
			Alias:        "myrig/witness",
			Command:      "claude",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}
	if got := all[0].Metadata["alias"]; got != "myrig/witness" {
		t.Fatalf("alias = %q, want %q", got, "myrig/witness")
	}
	if got := all[0].Metadata["alias_history"]; got != "old-witness" {
		t.Fatalf("alias_history = %q, want %q", got, "old-witness")
	}
}

func TestSyncSessionBeads_ClearsManagedAliasWhenRemoved(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "s-gc-123", runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := sp.SetMeta("s-gc-123", "GC_ALIAS", "old-witness"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "myrig/witness",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "s-gc-123",
			"template":     "myrig/witness",
			"alias":        "old-witness",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("Create(seed): %v", err)
	}

	ds := map[string]TemplateParams{
		"s-gc-123": {
			TemplateName: "myrig/witness",
			InstanceName: "myrig/witness",
			Command:      "claude",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}
	if got := all[0].Metadata["alias"]; got != "" {
		t.Fatalf("alias = %q, want empty", got)
	}
	if got := all[0].Metadata["alias_history"]; got != "old-witness" {
		t.Fatalf("alias_history = %q, want %q", got, "old-witness")
	}
	if got, err := sp.GetMeta("s-gc-123", "GC_ALIAS"); err != nil {
		t.Fatalf("GetMeta(GC_ALIAS): %v", err)
	} else if got != "" {
		t.Fatalf("GC_ALIAS = %q, want empty", got)
	}
}

func TestSyncSessionBeads_Idempotent(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "mayor", runtime.Config{Command: "claude"})

	ds := map[string]TemplateParams{
		"mayor": {TemplateName: "mayor", Command: "claude"},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Get the created bead's token and generation.
	all := allSessionBeads(t, store)
	token1 := all[0].Metadata["instance_token"]
	gen1 := all[0].Metadata["generation"]
	epoch1 := all[0].Metadata["continuation_epoch"]

	// Run again — should be idempotent.
	clk.Advance(5 * time.Second)
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all = allSessionBeads(t, store)
	if len(all) != 1 {
		t.Fatalf("expected 1 bead after re-sync, got %d", len(all))
	}

	// Token and generation should NOT change when config is unchanged.
	if all[0].Metadata["instance_token"] != token1 {
		t.Error("instance_token changed on idempotent re-sync")
	}
	if all[0].Metadata["generation"] != gen1 {
		t.Error("generation changed on idempotent re-sync")
	}
	if all[0].Metadata["continuation_epoch"] != epoch1 {
		t.Error("continuation_epoch changed on idempotent re-sync")
	}
}

func TestSyncSessionBeads_SyncsWakeMode(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()

	ds := map[string]TemplateParams{
		"mayor": {TemplateName: "mayor", Command: "claude", WakeMode: "fresh"},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if got := all[0].Metadata["wake_mode"]; got != "fresh" {
		t.Fatalf("wake_mode = %q, want fresh", got)
	}

	ds["mayor"] = TemplateParams{TemplateName: "mayor", Command: "claude"}
	clk.Advance(5 * time.Second)
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, err = store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads after clear: %v", err)
	}
	if got := all[0].Metadata["wake_mode"]; got != "" {
		t.Fatalf("wake_mode = %q, want empty after revert to resume", got)
	}
}

func TestSyncSessionBeads_BatchesExistingMetadataBackfill(t *testing.T) {
	store := newCountingMetadataStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()

	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"state":        "stopped",
		},
	})
	if err != nil {
		t.Fatalf("creating seed bead: %v", err)
	}

	ds := map[string]TemplateParams{
		"worker": {
			TemplateName: "worker",
			Command:      "true",
			WorkDir:      "/tmp/worktree",
			WakeMode:     "fresh",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	if store.batchCalls != 1 {
		t.Fatalf("batchCalls = %d, want 1", store.batchCalls)
	}
	if store.singleCalls != 0 {
		t.Fatalf("singleCalls = %d, want 0", store.singleCalls)
	}

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}
	b := all[0]
	if got := b.Metadata["template"]; got != "worker" {
		t.Fatalf("template = %q, want worker", got)
	}
	if got := b.Metadata["command"]; got != "true" {
		t.Fatalf("command = %q, want true", got)
	}
	if got := b.Metadata["work_dir"]; got != "/tmp/worktree" {
		t.Fatalf("work_dir = %q, want /tmp/worktree", got)
	}
	if got := b.Metadata["wake_mode"]; got != "fresh" {
		t.Fatalf("wake_mode = %q, want fresh", got)
	}
	if got := b.Metadata["synced_at"]; got == "" {
		t.Fatal("synced_at not set")
	}
}

func TestSyncSessionBeads_DoesNotRewriteReconcilerOwnedState(t *testing.T) {
	store := newCountingMetadataStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"template":           "worker",
			"state":              "awake",
			"wake_mode":          "resume",
			"command":            "true",
			"generation":         "1",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("creating seed bead: %v", err)
	}

	ds := map[string]TemplateParams{
		"worker": {
			TemplateName: "worker",
			Command:      "true",
			WakeMode:     "resume",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	if store.batchCalls != 0 {
		t.Fatalf("batchCalls = %d, want 0", store.batchCalls)
	}
	if store.singleCalls != 0 {
		t.Fatalf("singleCalls = %d, want 0", store.singleCalls)
	}

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}
	if got := all[0].Metadata["state"]; got != "awake" {
		t.Fatalf("state = %q, want awake", got)
	}
}

func TestSyncSessionBeads_ConfigDrift(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "mayor", runtime.Config{Command: "claude"})

	ds := map[string]TemplateParams{
		"mayor": {TemplateName: "mayor", Command: "claude"},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all := allSessionBeads(t, store)
	token1 := all[0].Metadata["instance_token"]

	// Change config — different command.
	ds["mayor"] = TemplateParams{TemplateName: "mayor", Command: "gemini"}
	clk.Advance(5 * time.Second)
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// syncSessionBeads no longer updates config_hash for existing beads.
	// The bead-driven reconciler (reconcileSessionBeads) detects drift by
	// comparing bead config_hash against the current desired config and
	// updates it only after successful restart.
	all, _ = store.ListByLabel(sessionBeadLabel, 0)
	if all[0].Metadata["generation"] != "1" {
		t.Errorf("generation = %q, want %q (should not change on sync)", all[0].Metadata["generation"], "1")
	}
	if all[0].Metadata["instance_token"] != token1 {
		t.Error("instance_token should NOT change on sync (drift handled by reconciler)")
	}
}

func TestSyncSessionBeads_OrphanDetection(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()

	// Create a bead for "old-agent".
	ds := map[string]TemplateParams{
		"old-agent": {TemplateName: "old-agent", Command: "claude"},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Now sync with a different agent list (old-agent removed from config too).
	ds2 := map[string]TemplateParams{
		"new-agent": {TemplateName: "new-agent", Command: "claude"},
	}
	clk.Advance(5 * time.Second)
	// configuredNames only has new-agent — old-agent is truly orphaned.
	syncSessionBeads("", store, ds2, sp, allConfiguredDS(ds2), nil, clk, &stderr, false)

	// old-agent's bead should be closed with reason "orphaned".
	all := allSessionBeads(t, store)
	var oldBead beads.Bead
	for _, b := range all {
		if b.Metadata["session_name"] == "old-agent" {
			oldBead = b
			break
		}
	}
	if oldBead.Status != "closed" {
		t.Errorf("old-agent status = %q, want %q", oldBead.Status, "closed")
	}
	if oldBead.Metadata["state"] != "orphaned" {
		t.Errorf("old-agent state = %q, want %q", oldBead.Metadata["state"], "orphaned")
	}
	if oldBead.Metadata["close_reason"] != "orphaned" {
		t.Errorf("old-agent close_reason = %q, want %q", oldBead.Metadata["close_reason"], "orphaned")
	}
	if oldBead.Metadata["closed_at"] == "" {
		t.Error("old-agent closed_at is empty")
	}
}

func TestSyncSessionBeads_NilStore(t *testing.T) {
	// Verify nil store does not panic.
	var stderr bytes.Buffer
	syncSessionBeads("", nil, nil, nil, nil, nil, &clock.Fake{}, &stderr, false)
	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestSyncSessionBeads_StoppedAgent(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake() // mayor NOT started → IsRunning returns false

	ds := map[string]TemplateParams{
		"mayor": {TemplateName: "mayor", Command: "claude"},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all := allSessionBeads(t, store)
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}
	if all[0].Metadata["state"] != "stopped" {
		t.Errorf("state = %q, want %q", all[0].Metadata["state"], "stopped")
	}
	if all[0].Metadata["pending_create_claim"] != "true" {
		t.Errorf("pending_create_claim = %q, want true", all[0].Metadata["pending_create_claim"])
	}
}

func TestSyncSessionBeads_ClosedBeadCreatesNew(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "mayor", runtime.Config{Command: "claude"})

	ds := map[string]TemplateParams{
		"mayor": {TemplateName: "mayor", Command: "claude"},
	}

	var stderr bytes.Buffer

	// First sync creates the bead.
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}

	// Close the bead to simulate a completed lifecycle.
	_ = store.Close(all[0].ID)

	// Re-sync should create a NEW bead, not reuse the closed one.
	clk.Advance(5 * time.Second)
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all = allSessionBeads(t, store)
	if len(all) != 2 {
		t.Fatalf("expected 2 beads (1 closed + 1 new), got %d", len(all))
	}

	// Find the open bead.
	var openBead beads.Bead
	for _, b := range all {
		if b.Status == "open" {
			openBead = b
			break
		}
	}
	if openBead.ID == "" {
		t.Fatal("no open bead found after re-sync")
	}
	if openBead.Metadata["state"] != "active" {
		t.Errorf("state = %q, want %q", openBead.Metadata["state"], "active")
	}
	if openBead.Metadata["generation"] != "1" {
		t.Errorf("generation = %q, want %q (fresh bead)", openBead.Metadata["generation"], "1")
	}
}

func TestSyncSessionBeads_PoolInstanceOrphaned(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "city-worker-1", runtime.Config{Command: "claude"})
	_ = sp.Start(context.TODO(), "city-worker-2", runtime.Config{Command: "claude"})

	ds := map[string]TemplateParams{
		"city-worker-1": {TemplateName: "worker", Command: "claude"},
		"city-worker-2": {TemplateName: "worker", Command: "claude"},
	}

	var stderr bytes.Buffer
	// configuredNames has the template name, not instance names.
	configuredNames := map[string]bool{"city-worker": true}
	syncSessionBeads("", store, ds, sp, configuredNames, nil, clk, &stderr, false)

	// Remove instances from runnable agents but keep template configured.
	clk.Advance(5 * time.Second)
	syncSessionBeads("", store, nil, sp, configuredNames, nil, clk, &stderr, false)

	// Pool instances are ephemeral (not user-configured), so they become
	// closed with reason "orphaned" when no longer running.
	all, _ := store.ListByLabel(sessionBeadLabel, 0)
	for _, b := range all {
		if b.Status != "closed" {
			t.Errorf("pool instance %s status = %q, want %q",
				b.Metadata["session_name"], b.Status, "closed")
		}
		if b.Metadata["state"] != "orphaned" {
			t.Errorf("pool instance %s state = %q, want %q",
				b.Metadata["session_name"], b.Metadata["state"], "orphaned")
		}
		if b.Metadata["close_reason"] != "orphaned" {
			t.Errorf("pool instance %s close_reason = %q, want %q",
				b.Metadata["session_name"], b.Metadata["close_reason"], "orphaned")
		}
	}
}

func TestSyncSessionBeads_ResumedAfterSuspension(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "worker", runtime.Config{Command: "claude"})

	ds := map[string]TemplateParams{
		"worker": {TemplateName: "worker", Command: "claude"},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Suspend the agent: remove from runnable but keep in configuredNames.
	clk.Advance(5 * time.Second)
	configuredNames := map[string]bool{"worker": true}
	syncSessionBeads("", store, nil, sp, configuredNames, nil, clk, &stderr, false)

	// Verify the bead is closed.
	all := allSessionBeads(t, store)
	if len(all) != 1 {
		t.Fatalf("expected 1 bead after suspension, got %d", len(all))
	}
	if all[0].Status != "closed" {
		t.Fatalf("bead status = %q, want %q", all[0].Status, "closed")
	}

	// Resume the agent: return it to the runnable set.
	clk.Advance(5 * time.Second)
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Should have 2 beads: 1 closed (old lifecycle) + 1 open (new lifecycle).
	all = allSessionBeads(t, store)
	if len(all) != 2 {
		t.Fatalf("expected 2 beads after resume, got %d", len(all))
	}

	var closedCount, openCount int
	for _, b := range all {
		switch b.Status {
		case "closed":
			closedCount++
		case "open":
			openCount++
			if b.Metadata["state"] != "active" {
				t.Errorf("resumed bead state = %q, want %q", b.Metadata["state"], "active")
			}
			if b.Metadata["generation"] != "1" {
				t.Errorf("resumed bead generation = %q, want %q (fresh lifecycle)", b.Metadata["generation"], "1")
			}
		}
	}
	if closedCount != 1 || openCount != 1 {
		t.Errorf("expected 1 closed + 1 open, got %d closed + %d open", closedCount, openCount)
	}
}

func TestSyncSessionBeads_StaleCloseMetadataCleared(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "worker", runtime.Config{Command: "claude"})

	ds := map[string]TemplateParams{
		"worker": {TemplateName: "worker", Command: "claude"},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Simulate a partially-failed closeBead: set close_reason on the
	// open bead as if setMeta("close_reason") succeeded but store.Close
	// failed. The bead stays open with stale terminal metadata.
	all, _ := store.ListByLabel(sessionBeadLabel, 0)
	_ = store.SetMetadata(all[0].ID, "close_reason", "orphaned")
	_ = store.SetMetadata(all[0].ID, "closed_at", "2026-03-07T12:00:05Z")

	// Agent resumes — sync should clear the stale close metadata.
	clk.Advance(5 * time.Second)
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, _ = store.ListByLabel(sessionBeadLabel, 0)
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}
	b := all[0]
	if b.Status != "open" {
		t.Errorf("status = %q, want %q", b.Status, "open")
	}
	if b.Metadata["state"] != "active" {
		t.Errorf("state = %q, want %q", b.Metadata["state"], "active")
	}
	if b.Metadata["close_reason"] != "" {
		t.Errorf("close_reason = %q, want empty (stale metadata not cleared)", b.Metadata["close_reason"])
	}
	if b.Metadata["closed_at"] != "" {
		t.Errorf("closed_at = %q, want empty (stale metadata not cleared)", b.Metadata["closed_at"])
	}
}

func TestSyncSessionBeads_SuspendedAgentNotOrphaned(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "mayor", runtime.Config{Command: "claude"})
	_ = sp.Start(context.TODO(), "worker", runtime.Config{Command: "claude"})

	ds := map[string]TemplateParams{
		"mayor":  {TemplateName: "mayor", Command: "claude"},
		"worker": {TemplateName: "worker", Command: "claude"},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Now "suspend" worker: remove from runnable agents but keep in configuredNames.
	dsOnlyMayor := map[string]TemplateParams{
		"mayor": {TemplateName: "mayor", Command: "claude"},
	}
	configuredNames := map[string]bool{
		"mayor":  true,
		"worker": true, // still configured, just suspended
	}
	clk.Advance(5 * time.Second)
	syncSessionBeads("", store, dsOnlyMayor, sp, configuredNames, nil, clk, &stderr, false)

	// Worker should be closed with reason "suspended", not "orphaned".
	all := allSessionBeads(t, store)
	var workerBead beads.Bead
	for _, b := range all {
		if b.Metadata["session_name"] == "worker" {
			workerBead = b
			break
		}
	}
	if workerBead.Status != "closed" {
		t.Errorf("worker status = %q, want %q", workerBead.Status, "closed")
	}
	if workerBead.Metadata["state"] != "suspended" {
		t.Errorf("worker state = %q, want %q", workerBead.Metadata["state"], "suspended")
	}
	if workerBead.Metadata["close_reason"] != "suspended" {
		t.Errorf("worker close_reason = %q, want %q", workerBead.Metadata["close_reason"], "suspended")
	}
}

func TestSyncSessionBeads_ReturnsIndex(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "mayor", runtime.Config{Command: "claude"})
	_ = sp.Start(context.TODO(), "worker", runtime.Config{Command: "claude"})

	ds := map[string]TemplateParams{
		"mayor":  {TemplateName: "mayor", Command: "claude"},
		"worker": {TemplateName: "worker", Command: "claude"},
	}

	var stderr bytes.Buffer
	idx := syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}

	// Index should contain both agents.
	if len(idx) != 2 {
		t.Fatalf("index length = %d, want 2", len(idx))
	}
	if idx["mayor"] == "" {
		t.Error("index missing mayor")
	}
	if idx["worker"] == "" {
		t.Error("index missing worker")
	}

	// Verify IDs match actual beads.
	all, _ := store.ListByLabel(sessionBeadLabel, 0)
	beadIDs := make(map[string]string)
	for _, b := range all {
		beadIDs[b.Metadata["session_name"]] = b.ID
	}
	if idx["mayor"] != beadIDs["mayor"] {
		t.Errorf("mayor ID = %q, want %q", idx["mayor"], beadIDs["mayor"])
	}
	if idx["worker"] != beadIDs["worker"] {
		t.Errorf("worker ID = %q, want %q", idx["worker"], beadIDs["worker"])
	}

	// Suspend worker — closed beads excluded from index.
	clk.Advance(5 * time.Second)
	cfgNames := map[string]bool{"mayor": true, "worker": true}
	dsOnlyMayor := map[string]TemplateParams{
		"mayor": {TemplateName: "mayor", Command: "claude"},
	}
	idx2 := syncSessionBeads("", store, dsOnlyMayor, sp, cfgNames, nil, clk, &stderr, false)

	if len(idx2) != 1 {
		t.Fatalf("after suspend, index length = %d, want 1", len(idx2))
	}
	if idx2["mayor"] == "" {
		t.Error("after suspend, index missing mayor")
	}
	if _, ok := idx2["worker"]; ok {
		t.Error("after suspend, index should not contain worker")
	}
}

// --- loadSessionBeads tests ---

func TestLoadSessionBeads_SingleBead(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := loadSessionBeads(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(result))
	}
	if result[0].Metadata["session_name"] != "worker" {
		t.Errorf("session_name = %q, want worker", result[0].Metadata["session_name"])
	}
}

func TestSyncSessionBeads_RepairsEmptyType(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), "mayor", runtime.Config{Command: "claude"})

	// Create a session bead, then corrupt its type to empty string.
	// MemStore defaults empty types to "task", so we create normally then
	// update to empty to simulate the corruption seen in production (BdStore
	// preserves empty types from the database).
	b, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "mayor",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	emptyType := ""
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &emptyType}); err != nil {
		t.Fatal(err)
	}

	ds := map[string]TemplateParams{
		"mayor": {TemplateName: "mayor", Command: "claude"},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// The bead's type should have been repaired to "session".
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != sessionBeadType {
		t.Errorf("type after repair = %q, want %q", got.Type, sessionBeadType)
	}
}

func TestLoadSessionBeads_NewTypeOnly(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := loadSessionBeads(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(result))
	}
}

func TestLoadSessionBeads_PoolOccupancy(t *testing.T) {
	store := beads.NewMemStore()

	// Three session beads for different pool slots.
	_, _ = store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"template":     "worker",
			"state":        "active",
			"pool_slot":    "1",
		},
	})
	_, _ = store.Create(beads.Bead{
		Title:  "worker-2",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-2",
			"template":     "worker",
			"state":        "active",
			"pool_slot":    "2",
		},
	})
	_, _ = store.Create(beads.Bead{
		Title:  "worker-3",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-3",
			"template":     "worker",
			"state":        "active",
			"pool_slot":    "3",
		},
	})

	result, err := loadSessionBeads(store)
	if err != nil {
		t.Fatal(err)
	}
	// All 3 should be returned.
	if len(result) != 3 {
		t.Fatalf("expected 3 beads for pool occupancy, got %d", len(result))
	}
}

func TestConfiguredSessionNames_IncludesForkSessions(t *testing.T) {
	store := beads.NewMemStore()

	// Create the primary session bead (managed, has agent_name).
	_, err := store.Create(beads.Bead{
		Title:  "overseer",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:overseer"},
		Metadata: map[string]string{
			"template":                   "overseer",
			"agent_name":                 "overseer",
			"session_name":               "s-primary",
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "overseer",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create a fork bead (no agent_name, from gc session new).
	_, err = store.Create(beads.Bead{
		Title:  "overseer fork",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:overseer"},
		Metadata: map[string]string{
			"template":     "overseer",
			"session_name": "s-fork-1",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents:        []config.Agent{{Name: "overseer"}},
		NamedSessions: []config.NamedSession{{Template: "overseer"}},
	}

	names := configuredSessionNames(cfg, "test", store)

	// Only the canonical configured session is controller-owned.
	if !names["s-primary"] {
		t.Errorf("configuredSessionNames missing primary session s-primary, got: %v", names)
	}
	if names["s-fork-1"] {
		t.Errorf("configuredSessionNames should not include fork session s-fork-1, got: %v", names)
	}
}

func TestConfiguredSessionNames_ExcludesClosedForks(t *testing.T) {
	store := beads.NewMemStore()

	// Primary bead.
	_, err := store.Create(beads.Bead{
		Title:  "overseer",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:overseer"},
		Metadata: map[string]string{
			"template":                   "overseer",
			"agent_name":                 "overseer",
			"session_name":               "s-primary",
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "overseer",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Closed fork bead — should NOT be in configured names.
	fork, err := store.Create(beads.Bead{
		Title:  "overseer old fork",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:overseer"},
		Metadata: map[string]string{
			"template":     "overseer",
			"session_name": "s-closed-fork",
			"state":        "closed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close(fork.ID)

	cfg := &config.City{
		Agents:        []config.Agent{{Name: "overseer"}},
		NamedSessions: []config.NamedSession{{Template: "overseer"}},
	}

	names := configuredSessionNames(cfg, "test", store)

	if !names["s-primary"] {
		t.Errorf("configuredSessionNames missing primary s-primary")
	}
	if names["s-closed-fork"] {
		t.Errorf("configuredSessionNames should NOT include closed fork s-closed-fork")
	}
}

func TestConfiguredSessionNames_DoesNotIncludePoolForks(t *testing.T) {
	store := beads.NewMemStore()

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)},
		},
	}

	// Create a pool instance bead that looks like a "fork" but is actually
	// a pool instance. Should NOT be in configured names (pool orphan detection
	// must still work).
	_, err := store.Create(beads.Bead{
		Title:  "worker-extra",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "s-worker-extra",
			"state":        "active",
			"pool_slot":    "5",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	names := configuredSessionNames(cfg, "test", store)

	// The pool base name should be in configured names.
	// But the excess pool instance should NOT be (it's a pool, not a singleton).
	if names["s-worker-extra"] {
		t.Errorf("configuredSessionNames should NOT include pool instance s-worker-extra")
	}
}

func TestSyncSessionBeads_OrphansLegacyPoolBaseSession(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "polecat",
			Dir:               "repo",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
		}},
	}

	legacySessionName := agent.SessionNameFor(
		config.EffectiveCityName(cfg, ""),
		cfg.Agents[0].QualifiedName(),
		cfg.Workspace.SessionTemplate,
	)

	legacy, err := store.Create(beads.Bead{
		Title:  "repo/polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:repo/polecat"},
		Metadata: map[string]string{
			"template":     "repo/polecat",
			"agent_name":   "repo/polecat",
			"session_name": legacySessionName,
			"alias":        "repo/polecat",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	desired := map[string]TemplateParams{
		"polecat-ci-1jb": {
			TemplateName: "repo/polecat",
			InstanceName: "repo/polecat/polecat-ci-1jb",
			Command:      "claude",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads(
		"",
		store,
		desired,
		sp,
		configuredSessionNames(cfg, "", store),
		cfg,
		clk,
		&stderr,
		false,
	)

	all := allSessionBeads(t, store)

	var (
		closedLegacy beads.Bead
		openPool     beads.Bead
	)
	for _, b := range all {
		switch b.Metadata["session_name"] {
		case legacySessionName:
			closedLegacy = b
		case "polecat-ci-1jb":
			openPool = b
		}
	}

	if closedLegacy.ID == "" {
		t.Fatalf("did not find legacy pool base bead in %+v", all)
	}
	if closedLegacy.ID != legacy.ID {
		t.Fatalf("legacy bead id = %q, want %q", closedLegacy.ID, legacy.ID)
	}
	if closedLegacy.Status != "closed" {
		t.Fatalf("legacy bead status = %q, want closed", closedLegacy.Status)
	}
	if closedLegacy.Metadata["close_reason"] != "orphaned" {
		t.Fatalf("legacy bead close_reason = %q, want orphaned", closedLegacy.Metadata["close_reason"])
	}
	if openPool.ID == "" {
		t.Fatalf("did not find new pool session bead in %+v", all)
	}
	if openPool.Metadata["alias"] != "" {
		t.Fatalf("new pool bead alias = %q, want empty", openPool.Metadata["alias"])
	}
}

func TestLoadSessionBeads_NilStore(t *testing.T) {
	result, err := loadSessionBeads(nil)
	if err != nil {
		t.Fatalf("nil store should not error: %v", err)
	}
	if result != nil {
		t.Errorf("nil store should return nil, got %v", result)
	}
}

func TestLoadSessionBeads_SkipsClosedBeads(t *testing.T) {
	store := beads.NewMemStore()

	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"state":        "active",
		},
	})
	_ = store.Close(b.ID)

	result, err := loadSessionBeads(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Errorf("expected 0 beads (closed), got %d", len(result))
	}
}

// TestFindClosedNamedSessionBead_ReopensOnRestart verifies that when a named
// session bead is closed (e.g., after gc stop), findClosedNamedSessionBead
// finds it by identity so the caller can reopen it. This preserves the bead
// ID for reference continuity (slings, convoys, messages). Supersedes PR #204
// which would have allowed name reuse by creating a new bead.
func TestFindClosedNamedSessionBead_ReopensOnRestart(t *testing.T) {
	store := beads.NewMemStore()

	// Create a named session bead with identity "mayor".
	b, err := store.Create(beads.Bead{
		Type:   "gc:session",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"session_name":               "mayor",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	originalID := b.ID

	// Close it (simulates gc stop).
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// findClosedNamedSessionBead should find it.
	found, ok := findClosedNamedSessionBead(store, "mayor")
	if !ok {
		t.Fatal("findClosedNamedSessionBead did not find closed mayor bead")
	}
	if found.ID != originalID {
		t.Errorf("found bead ID = %q, want %q (must reopen same bead)", found.ID, originalID)
	}

	// Reopen it (the caller's responsibility).
	open := "open"
	if err := store.Update(found.ID, beads.UpdateOpts{Status: &open}); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	// Verify the bead is open with the original ID.
	reopened, err := store.Get(originalID)
	if err != nil {
		t.Fatalf("Get reopened bead: %v", err)
	}
	if reopened.Status != "open" {
		t.Errorf("reopened status = %q, want %q", reopened.Status, "open")
	}
	if reopened.Metadata["session_name"] != "mayor" {
		t.Errorf("reopened session_name = %q, want %q", reopened.Metadata["session_name"], "mayor")
	}
}

func TestFindClosedNamedSessionBeadForSessionName_PrefersMatchingCanonicalCandidate(t *testing.T) {
	store := beads.NewMemStore()

	retired, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(retired): %v", err)
	}
	if err := store.Close(retired.ID); err != nil {
		t.Fatalf("Close(retired): %v", err)
	}

	canonical, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               "test-city--mayor",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}
	if err := store.Close(canonical.ID); err != nil {
		t.Fatalf("Close(canonical): %v", err)
	}

	found, ok := findClosedNamedSessionBeadForSessionName(store, "mayor", "test-city--mayor")
	if !ok {
		t.Fatal("findClosedNamedSessionBeadForSessionName did not find canonical mayor bead")
	}
	if found.ID != canonical.ID {
		t.Fatalf("found bead ID = %q, want canonical %q", found.ID, canonical.ID)
	}

	generic, ok := findClosedNamedSessionBead(store, "mayor")
	if !ok {
		t.Fatal("findClosedNamedSessionBead did not find closed mayor bead")
	}
	if generic.ID != canonical.ID {
		t.Fatalf("generic lookup bead ID = %q, want canonical %q", generic.ID, canonical.ID)
	}
}

func TestFindClosedNamedSessionBead_PrefersNewestClosedCanonical(t *testing.T) {
	store := beads.NewMemStore()

	older, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               "test-city--mayor",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(older): %v", err)
	}
	if err := store.Close(older.ID); err != nil {
		t.Fatalf("Close(older): %v", err)
	}

	newer, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               "test-city--mayor",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(newer): %v", err)
	}
	if err := store.Close(newer.ID); err != nil {
		t.Fatalf("Close(newer): %v", err)
	}

	found, ok := findClosedNamedSessionBead(store, "mayor")
	if !ok {
		t.Fatal("findClosedNamedSessionBead did not find closed mayor bead")
	}
	if found.ID != newer.ID {
		t.Fatalf("found bead ID = %q, want newest canonical %q", found.ID, newer.ID)
	}
}
