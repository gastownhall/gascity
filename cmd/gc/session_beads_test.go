package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// allConfiguredDS builds configuredNames from a desiredState map.
func allConfiguredDS(ds map[string]TemplateParams) map[string]bool {
	m := make(map[string]bool, len(ds))
	for sn := range ds {
		m[sn] = true
	}
	return m
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
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

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
	if b.Metadata["instance_token"] == "" {
		t.Error("instance_token is empty")
	}
	if b.Metadata["config_hash"] == "" {
		t.Error("config_hash is empty")
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
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Get the created bead's token and generation.
	all, _ := store.ListByLabel(sessionBeadLabel, 0)
	token1 := all[0].Metadata["instance_token"]
	gen1 := all[0].Metadata["generation"]

	// Run again — should be idempotent.
	clk.Advance(5 * time.Second)
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, _ = store.ListByLabel(sessionBeadLabel, 0)
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
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, _ := store.ListByLabel(sessionBeadLabel, 0)
	token1 := all[0].Metadata["instance_token"]

	// Change config — different command.
	ds["mayor"] = TemplateParams{TemplateName: "mayor", Command: "gemini"}
	clk.Advance(5 * time.Second)
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

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
	// config_hash should still be the original hash (set at creation).
	origHash := runtime.CoreFingerprint(runtime.Config{Command: "claude"})
	if all[0].Metadata["config_hash"] != origHash {
		t.Errorf("config_hash = %q, want original %q", all[0].Metadata["config_hash"], origHash)
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
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Now sync with a different agent list (old-agent removed from config too).
	ds2 := map[string]TemplateParams{
		"new-agent": {TemplateName: "new-agent", Command: "claude"},
	}
	clk.Advance(5 * time.Second)
	// configuredNames only has new-agent — old-agent is truly orphaned.
	syncSessionBeads(store, ds2, sp, allConfiguredDS(ds2), nil, clk, &stderr, false)

	// old-agent's bead should be closed with reason "orphaned".
	all, _ := store.ListByLabel(sessionBeadLabel, 0)
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
	syncSessionBeads(nil, nil, nil, nil, nil, &clock.Fake{}, &stderr, false)
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
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}
	if all[0].Metadata["state"] != "stopped" {
		t.Errorf("state = %q, want %q", all[0].Metadata["state"], "stopped")
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
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(all) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(all))
	}

	// Close the bead to simulate a completed lifecycle.
	_ = store.Close(all[0].ID)

	// Re-sync should create a NEW bead, not reuse the closed one.
	clk.Advance(5 * time.Second)
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	all, _ = store.ListByLabel(sessionBeadLabel, 0)
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
	syncSessionBeads(store, ds, sp, configuredNames, nil, clk, &stderr, false)

	// Remove instances from runnable agents but keep template configured.
	clk.Advance(5 * time.Second)
	syncSessionBeads(store, nil, sp, configuredNames, nil, clk, &stderr, false)

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
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Suspend the agent: remove from runnable but keep in configuredNames.
	clk.Advance(5 * time.Second)
	configuredNames := map[string]bool{"worker": true}
	syncSessionBeads(store, nil, sp, configuredNames, nil, clk, &stderr, false)

	// Verify the bead is closed.
	all, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(all) != 1 {
		t.Fatalf("expected 1 bead after suspension, got %d", len(all))
	}
	if all[0].Status != "closed" {
		t.Fatalf("bead status = %q, want %q", all[0].Status, "closed")
	}

	// Resume the agent: return it to the runnable set.
	clk.Advance(5 * time.Second)
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Should have 2 beads: 1 closed (old lifecycle) + 1 open (new lifecycle).
	all, _ = store.ListByLabel(sessionBeadLabel, 0)
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
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Simulate a partially-failed closeBead: set close_reason on the
	// open bead as if setMeta("close_reason") succeeded but store.Close
	// failed. The bead stays open with stale terminal metadata.
	all, _ := store.ListByLabel(sessionBeadLabel, 0)
	_ = store.SetMetadata(all[0].ID, "close_reason", "orphaned")
	_ = store.SetMetadata(all[0].ID, "closed_at", "2026-03-07T12:00:05Z")

	// Agent resumes — sync should clear the stale close metadata.
	clk.Advance(5 * time.Second)
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

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
	syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

	// Now "suspend" worker: remove from runnable agents but keep in configuredNames.
	dsOnlyMayor := map[string]TemplateParams{
		"mayor": {TemplateName: "mayor", Command: "claude"},
	}
	configuredNames := map[string]bool{
		"mayor":  true,
		"worker": true, // still configured, just suspended
	}
	clk.Advance(5 * time.Second)
	syncSessionBeads(store, dsOnlyMayor, sp, configuredNames, nil, clk, &stderr, false)

	// Worker should be closed with reason "suspended", not "orphaned".
	all, _ := store.ListByLabel(sessionBeadLabel, 0)
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
	idx := syncSessionBeads(store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)

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
	idx2 := syncSessionBeads(store, dsOnlyMayor, sp, cfgNames, nil, clk, &stderr, false)

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

// --- loadSessionBeads tests (Phase 0a: dual bead type recognition) ---

func TestLoadSessionBeads_LegacyOnly(t *testing.T) {
	store := beads.NewMemStore()

	// Create a legacy bead.
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

func TestLoadSessionBeads_DeduplicatesBySessionName(t *testing.T) {
	store := beads.NewMemStore()

	// Create both a legacy and a new-type bead with the same session_name.
	_, _ = store.Create(beads.Bead{
		Title:  "worker-legacy",
		Type:   "agent_session",
		Labels: []string{legacySessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"state":        "active",
		},
	})
	_, _ = store.Create(beads.Bead{
		Title:  "worker-new",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"state":        "active",
		},
	})

	result, err := loadSessionBeads(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 bead (deduped), got %d", len(result))
	}
	// New-type bead takes precedence.
	if result[0].Title != "worker-new" {
		t.Errorf("expected new-type bead to win, got title=%q", result[0].Title)
	}
}

func TestLoadSessionBeads_LegacyStateMapping(t *testing.T) {
	tests := []struct {
		legacyState string
		wantState   string
		wantCount   int // expected number of beads returned
	}{
		{"stopped", "closed", 0},     // stopped → closed is terminal, filtered out
		{"orphaned", "suspended", 1}, // orphaned → suspended, kept
		{"active", "active", 1},
		{"suspended", "suspended", 1},
	}
	for _, tt := range tests {
		store := beads.NewMemStore() // fresh store per subtest
		_, _ = store.Create(beads.Bead{
			Title:  "worker",
			Type:   "agent_session",
			Labels: []string{legacySessionBeadLabel},
			Metadata: map[string]string{
				"session_name": "worker",
				"state":        tt.legacyState,
			},
		})

		result, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("state=%q: %v", tt.legacyState, err)
		}
		if len(result) != tt.wantCount {
			t.Fatalf("state=%q: expected %d bead(s), got %d", tt.legacyState, tt.wantCount, len(result))
		}
		if tt.wantCount > 0 && result[0].Metadata["state"] != tt.wantState {
			t.Errorf("state=%q: mapped to %q, want %q",
				tt.legacyState, result[0].Metadata["state"], tt.wantState)
		}
	}
}

func TestLoadSessionBeads_HybridPoolOccupancy(t *testing.T) {
	store := beads.NewMemStore()

	// Two legacy beads + one unified bead, all for different session names.
	_, _ = store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   "agent_session",
		Labels: []string{legacySessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"template":     "worker",
			"state":        "active",
			"pool_slot":    "1",
		},
	})
	_, _ = store.Create(beads.Bead{
		Title:  "worker-2",
		Type:   "agent_session",
		Labels: []string{legacySessionBeadLabel},
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
	// All 3 should be returned (no name collision).
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
			"template":     "overseer",
			"agent_name":   "overseer",
			"session_name": "s-primary",
			"state":        "active",
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
		Agents: []config.Agent{
			{Name: "overseer"},
		},
	}

	names := configuredSessionNames(cfg, "test", store)

	// Both the primary and fork session names must be in the configured set.
	if !names["s-primary"] {
		t.Errorf("configuredSessionNames missing primary session s-primary, got: %v", names)
	}
	if !names["s-fork-1"] {
		t.Errorf("configuredSessionNames missing fork session s-fork-1, got: %v", names)
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
			"template":     "overseer",
			"agent_name":   "overseer",
			"session_name": "s-primary",
			"state":        "active",
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
		Agents: []config.Agent{
			{Name: "overseer"},
		},
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
			{Name: "worker", Pool: &config.PoolConfig{Min: 1, Max: 3}},
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
