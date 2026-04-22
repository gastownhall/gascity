package main

import (
	"bytes"
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
