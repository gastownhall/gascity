package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRebaseSessionConfigHashes_UpdatesDriftedHash(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	// Desired state with a NEW command (simulates config change after reload).
	desiredState := map[string]TemplateParams{
		"worker": {
			Command:      "new-cmd",
			SessionName:  "worker",
			TemplateName: "worker",
		},
	}

	// Session bead has the OLD config hash.
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	oldLive := runtime.LiveFingerprint(runtime.Config{Command: "old-cmd"})
	b, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": oldHash,
			"started_live_hash":   oldLive,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", n, stderr.String())
	}

	// Verify the stored hash now matches the new config.
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := runtime.CoreFingerprint(runtime.Config{Command: "new-cmd"})
	if got.Metadata["started_config_hash"] != wantHash {
		t.Errorf("started_config_hash = %q, want %q", got.Metadata["started_config_hash"], wantHash)
	}
	if got.Metadata["core_hash_breakdown"] == "" {
		t.Error("core_hash_breakdown should be set after rebase")
	}
}

func TestRebaseSessionConfigHashes_SkipsSessionWithoutHash(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	desiredState := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	// Session in startup window — no started_config_hash yet.
	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"template":     "worker",
			"state":        "creating",
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 0 {
		t.Errorf("updated = %d, want 0 for sessions without stored hash", n)
	}
}

func TestRebaseSessionConfigHashes_SkipsOrphanedSession(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Empty desired state — session is orphaned.
	desiredState := map[string]TemplateParams{}

	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": "old-hash",
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 0 {
		t.Errorf("updated = %d, want 0 for orphaned sessions", n)
	}
}

func TestRebaseSessionConfigHashes_NoUpdateWhenHashMatches(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	desiredState := map[string]TemplateParams{
		"worker": {Command: "test-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	// Session hash already matches the desired config.
	currentHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	currentLive := runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"})
	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": currentHash,
			"started_live_hash":   currentLive,
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 0 {
		t.Errorf("updated = %d, want 0 when hashes already match", n)
	}
	if stdout.Len() > 0 {
		t.Errorf("expected no stdout output, got %q", stdout.String())
	}
}

func TestRebaseSessionConfigHashes_NilStoreReturnsZero(t *testing.T) {
	n := rebaseSessionConfigHashes(nil, nil, nil, nil, nil, nil)
	if n != 0 {
		t.Errorf("updated = %d, want 0 for nil store", n)
	}
}

func TestRebaseSessionConfigHashes_MultipleSessions(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{
		{Name: "worker"},
		{Name: "mayor"},
	}}

	oldWorkerHash := runtime.CoreFingerprint(runtime.Config{Command: "old-worker-cmd"})
	oldMayorHash := runtime.CoreFingerprint(runtime.Config{Command: "old-mayor-cmd"})

	desiredState := map[string]TemplateParams{
		"worker-1": {Command: "new-worker-cmd", SessionName: "worker-1", TemplateName: "worker"},
		"mayor-1":  {Command: "new-mayor-cmd", SessionName: "mayor-1", TemplateName: "mayor"},
	}

	w, _ := store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker-1",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": oldWorkerHash,
		},
	})
	m, _ := store.Create(beads.Bead{
		Title:  "mayor-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "mayor-1",
			"template":            "mayor",
			"state":               "active",
			"started_config_hash": oldMayorHash,
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{w, m})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 2 {
		t.Fatalf("updated = %d, want 2 (stderr=%s)", n, stderr.String())
	}

	gotW, _ := store.Get(w.ID)
	wantWorkerHash := runtime.CoreFingerprint(runtime.Config{Command: "new-worker-cmd"})
	if gotW.Metadata["started_config_hash"] != wantWorkerHash {
		t.Errorf("worker hash = %q, want %q", gotW.Metadata["started_config_hash"], wantWorkerHash)
	}

	gotM, _ := store.Get(m.ID)
	wantMayorHash := runtime.CoreFingerprint(runtime.Config{Command: "new-mayor-cmd"})
	if gotM.Metadata["started_config_hash"] != wantMayorHash {
		t.Errorf("mayor hash = %q, want %q", gotM.Metadata["started_config_hash"], wantMayorHash)
	}
}

// TestRebaseSessionConfigHashes_ReconciledAfterRebaseShowsNoDrift verifies
// the end-to-end scenario: after rebase, the reconciler sees no config drift.
func TestRebaseSessionConfigHashes_ReconciledAfterRebaseShowsNoDrift(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

	// Set up desired state with NEW config (simulating post-reload state).
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")

	// Session started with OLD config hash.
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": oldHash,
	})

	// Without rebase, reconciler would detect drift.
	// Rebase first.
	snapshot := newSessionBeadSnapshot([]beads.Bead{session})
	var stdout, stderr bytes.Buffer
	n := rebaseSessionConfigHashes(env.store, env.cfg, env.desiredState, snapshot, &stdout, &stderr)
	if n != 1 {
		t.Fatalf("rebase updated = %d, want 1 (stderr=%s)", n, stderr.String())
	}

	// Reload the session from store (rebase updated metadata).
	updated, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Reconcile — should NOT initiate a drain.
	env.reconcile([]beads.Bead{updated})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain after rebase, got reason=%q", ds.reason)
	}
}

func TestRebaseSessionConfigHashes_SkipsEmptySessionName(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	desiredState := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	// Session with blank session_name but non-empty hash — should be skipped.
	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": "some-hash",
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 0 {
		t.Errorf("updated = %d, want 0 for empty session_name", n)
	}
}

func TestRebaseSessionConfigHashes_TemplateFallback(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	// TemplateName is empty in desired state — function falls back to
	// normalizedSessionTemplate which reads the bead's "template" metadata.
	desiredState := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: ""},
	}

	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": oldHash,
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", n, stderr.String())
	}
}

func TestRebaseSessionConfigHashes_SkipsWhenBothTemplatesEmpty(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	// TemplateName is empty AND bead has no "template" metadata —
	// normalizedSessionTemplate returns "" so the session is skipped.
	desiredState := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: ""},
	}

	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"state":               "active",
			"started_config_hash": "old-hash",
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 0 {
		t.Errorf("updated = %d, want 0 when both template paths are empty", n)
	}
}

func TestRebaseSessionConfigHashes_SkipsUnknownTemplate(t *testing.T) {
	store := beads.NewMemStore()
	// Config has "worker" but bead's template is "unknown" — findAgentByTemplate returns nil.
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	desiredState := map[string]TemplateParams{
		"rogue": {Command: "new-cmd", SessionName: "rogue", TemplateName: "unknown"},
	}

	b, _ := store.Create(beads.Bead{
		Title:  "rogue",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "rogue",
			"template":            "unknown",
			"state":               "active",
			"started_config_hash": "old-hash",
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 0 {
		t.Errorf("updated = %d, want 0 for template not in agent config", n)
	}
}

func TestRebaseSessionConfigHashes_TemplateOverridesModifyHash(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	schema := []config.ProviderOption{{
		Key:   "model",
		Label: "Model",
		Type:  "select",
		Choices: []config.OptionChoice{
			{Value: "fast", Label: "Fast", FlagArgs: []string{"--model", "fast"}},
			{Value: "smart", Label: "Smart", FlagArgs: []string{"--model", "smart"}},
		},
	}}

	desiredState := map[string]TemplateParams{
		"worker": {
			Command:      "claude --model fast",
			SessionName:  "worker",
			TemplateName: "worker",
			ResolvedProvider: &config.ResolvedProvider{
				OptionsSchema:     schema,
				EffectiveDefaults: map[string]string{"model": "fast"},
			},
		},
	}

	// Session was started with "fast" model. template_overrides selects "smart".
	// The rebase must apply the override to compute the correct hash.
	overrides := map[string]string{"model": "smart"}
	overridesJSON, _ := json.Marshal(overrides)

	baseCfg := templateParamsToConfig(desiredState["worker"])
	oldHash := runtime.CoreFingerprint(baseCfg)

	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": oldHash,
			"template_overrides":  string(overridesJSON),
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", n, stderr.String())
	}

	got, _ := store.Get(b.ID)
	// The hash should differ from the base (non-overridden) hash because the
	// override changed the command via replaceSchemaFlags.
	if got.Metadata["started_config_hash"] == oldHash {
		t.Error("hash should change when template_overrides modify the command")
	}
}

func TestRebaseSessionConfigHashes_TemplateOverridesSkipsInitialMessage(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	schema := []config.ProviderOption{{
		Key:   "model",
		Label: "Model",
		Type:  "select",
		Choices: []config.OptionChoice{
			{Value: "fast", Label: "Fast", FlagArgs: []string{"--model", "fast"}},
		},
	}}

	desiredState := map[string]TemplateParams{
		"worker": {
			Command:      "claude --model fast",
			SessionName:  "worker",
			TemplateName: "worker",
			ResolvedProvider: &config.ResolvedProvider{
				OptionsSchema:     schema,
				EffectiveDefaults: map[string]string{"model": "fast"},
			},
		},
	}

	// Override contains only initial_message which is filtered out.
	// No effective schema change → hash should match the base config.
	overrides := map[string]string{"initial_message": "hello", "model": "fast"}
	overridesJSON, _ := json.Marshal(overrides)

	baseCfg := templateParamsToConfig(desiredState["worker"])
	// Compute the hash the rebase function would produce (with overrides applied
	// but initial_message filtered): effective options are model=fast (same as default).
	extra, _ := config.ResolveExplicitOptions(schema, map[string]string{"model": "fast"})
	expectedCfg := baseCfg
	if len(extra) > 0 {
		expectedCfg.Command = config.ReplaceSchemaFlags(baseCfg.Command, schema, extra)
	}
	expectedHash := runtime.CoreFingerprint(expectedCfg)

	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": "different-old-hash",
			"template_overrides":  string(overridesJSON),
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", n, stderr.String())
	}

	got, _ := store.Get(b.ID)
	if got.Metadata["started_config_hash"] != expectedHash {
		t.Errorf("hash = %q, want %q", got.Metadata["started_config_hash"], expectedHash)
	}
}

func TestRebaseSessionConfigHashes_TemplateOverridesIgnoredWithoutProvider(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	// No ResolvedProvider — template_overrides should be ignored entirely.
	desiredState := map[string]TemplateParams{
		"worker": {
			Command:      "new-cmd",
			SessionName:  "worker",
			TemplateName: "worker",
		},
	}

	overrides := map[string]string{"model": "smart"}
	overridesJSON, _ := json.Marshal(overrides)

	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": oldHash,
			"template_overrides":  string(overridesJSON),
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", n, stderr.String())
	}

	// Hash should match the base config (no override processing).
	got, _ := store.Get(b.ID)
	wantHash := runtime.CoreFingerprint(runtime.Config{Command: "new-cmd"})
	if got.Metadata["started_config_hash"] != wantHash {
		t.Errorf("hash = %q, want %q (base config, no overrides)", got.Metadata["started_config_hash"], wantHash)
	}
}

func TestRebaseSessionConfigHashes_TemplateOverridesInvalidJSON(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	desiredState := map[string]TemplateParams{
		"worker": {
			Command:      "new-cmd",
			SessionName:  "worker",
			TemplateName: "worker",
			ResolvedProvider: &config.ResolvedProvider{
				OptionsSchema: []config.ProviderOption{{Key: "model"}},
			},
		},
	}

	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": oldHash,
			"template_overrides":  "{invalid json",
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 1 {
		t.Fatalf("updated = %d, want 1 (invalid JSON ignored, base hash used)", n)
	}

	// Hash should match the base config since invalid JSON is silently ignored.
	got, _ := store.Get(b.ID)
	wantHash := runtime.CoreFingerprint(runtime.Config{Command: "new-cmd"})
	if got.Metadata["started_config_hash"] != wantHash {
		t.Errorf("hash = %q, want %q", got.Metadata["started_config_hash"], wantHash)
	}
}

func TestRebaseSessionConfigHashes_LiveHashOnlyDiffers(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	desiredState := map[string]TemplateParams{
		"worker": {Command: "test-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	// Core hash already matches, but live hash differs.
	currentCoreHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	currentLiveHash := runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"})
	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": currentCoreHash,
			"started_live_hash":   "stale-live-hash",
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 1 {
		t.Fatalf("updated = %d, want 1 for live-only hash drift (stderr=%s)", n, stderr.String())
	}

	got, _ := store.Get(b.ID)
	if got.Metadata["started_live_hash"] != currentLiveHash {
		t.Errorf("started_live_hash = %q, want %q", got.Metadata["started_live_hash"], currentLiveHash)
	}
	// Core hash should NOT have been updated (it already matched).
	if got.Metadata["started_config_hash"] != currentCoreHash {
		t.Errorf("started_config_hash changed unexpectedly: %q → %q", currentCoreHash, got.Metadata["started_config_hash"])
	}
	if got.Metadata["core_hash_breakdown"] != "" {
		t.Error("core_hash_breakdown should not be set when core hash matches")
	}
}

func TestRebaseSessionConfigHashes_LiveHashEmptySkipped(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	desiredState := map[string]TemplateParams{
		"worker": {Command: "test-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	// Core hash matches and live hash is empty — no update needed.
	currentCoreHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	b, _ := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": currentCoreHash,
		},
	})
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(store, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 0 {
		t.Errorf("updated = %d, want 0 when core matches and live hash is empty", n)
	}
}

func TestRebaseSessionConfigHashes_SetMetadataBatchError(t *testing.T) {
	// Use unavailableStore to simulate a store write failure.
	// We need a store that can hold data but fails on SetMetadataBatch.
	// Build a real store for setup, then wrap with a failing store.
	realStore := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}

	desiredState := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	b, _ := realStore.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"state":               "active",
			"started_config_hash": oldHash,
		},
	})
	// Use the snapshot from the real store but pass the failing store for writes.
	snapshot := newSessionBeadSnapshot([]beads.Bead{b})
	failStore := unavailableStore{err: errors.New("disk full")}
	var stdout, stderr bytes.Buffer

	n := rebaseSessionConfigHashes(failStore, cfg, desiredState, snapshot, &stdout, &stderr)
	if n != 0 {
		t.Errorf("updated = %d, want 0 when store write fails", n)
	}
	if !strings.Contains(stderr.String(), "disk full") {
		t.Errorf("stderr = %q, want error message mentioning 'disk full'", stderr.String())
	}
	if stdout.Len() > 0 {
		t.Errorf("stdout should be empty on failure, got %q", stdout.String())
	}
}

func TestRebaseSessionConfigHashes_NilSessionBeadsReturnsZero(t *testing.T) {
	store := beads.NewMemStore()
	n := rebaseSessionConfigHashes(store, nil, nil, nil, nil, nil)
	if n != 0 {
		t.Errorf("updated = %d, want 0 for nil sessionBeads", n)
	}
}
