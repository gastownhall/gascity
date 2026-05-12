package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestAcceptConfigDriftAcrossSessions_UpdatesStaleHash(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": "stale-hash-deadbeef",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	desired := map[string]TemplateParams{
		"worker": {
			Command:      "new-cmd",
			SessionName:  "worker",
			TemplateName: "worker",
		},
	}
	wantHash := runtime.CoreFingerprint(runtime.Config{Command: "new-cmd"})
	if wantHash == "stale-hash-deadbeef" {
		t.Fatalf("test setup: stale fixture coincidentally equals fresh hash %q", wantHash)
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, &stderr)
	if got != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", got, stderr.String())
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if updated.Metadata["started_config_hash"] != wantHash {
		t.Errorf("started_config_hash = %q, want %q", updated.Metadata["started_config_hash"], wantHash)
	}
}

// TestAcceptConfigDriftAcrossSessions_SkipsUnstartedSessions asserts that a
// session that has never recorded a started_config_hash (still in the
// startup window) is left alone. Stamping a hash on an unstarted session
// would interfere with the reconciler's first-start path; the reconciler
// already skips drift detection while started_config_hash is empty.
func TestAcceptConfigDriftAcrossSessions_SkipsUnstartedSessions(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"template":     "worker",
			// no started_config_hash — session hasn't reached started state yet
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	desired := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, &stderr)
	if got != 0 {
		t.Fatalf("updated = %d, want 0 for unstarted session (stderr=%s)", got, stderr.String())
	}

	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if _, present := unchanged.Metadata["started_config_hash"]; present {
		t.Errorf("started_config_hash unexpectedly written for unstarted session: %q", unchanged.Metadata["started_config_hash"])
	}
}

// TestAcceptConfigDriftAcrossSessions_SkipsOrphanedSessions asserts that a
// session whose name has no entry in the freshly-built desired state
// (orphaned by the config edit — e.g. an agent was removed) is left
// untouched. The orphan/suspended branch of the reconciler handles such
// sessions on the next tick; soft-reload only updates sessions still
// mapped to a configured agent.
func TestAcceptConfigDriftAcrossSessions_SkipsOrphanedSessions(t *testing.T) {
	store := beads.NewMemStore()
	const staleHash = "stale-orphan-hash"
	sessionBead, err := store.Create(beads.Bead{
		Title:  "removed-agent",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "removed-agent",
			"template":            "removed-agent",
			"started_config_hash": staleHash,
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	// Desired state has a different agent — the original session is orphaned.
	desired := map[string]TemplateParams{
		"surviving-agent": {Command: "cmd", SessionName: "surviving-agent", TemplateName: "surviving-agent"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, &stderr)
	if got != 0 {
		t.Fatalf("updated = %d, want 0 for orphaned session (stderr=%s)", got, stderr.String())
	}

	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if unchanged.Metadata["started_config_hash"] != staleHash {
		t.Errorf("started_config_hash changed for orphan = %q, want %q", unchanged.Metadata["started_config_hash"], staleHash)
	}
}

// TestAcceptConfigDriftAcrossSessions_LeavesNonDriftingSessionsAlone asserts
// that a session whose stored hash already matches the recomputed current
// hash is not rewritten — the function returns 0 and metadata is
// untouched. This keeps soft-reload a no-op for the common no-drift case.
func TestAcceptConfigDriftAcrossSessions_LeavesNonDriftingSessionsAlone(t *testing.T) {
	store := beads.NewMemStore()
	matchingHash := runtime.CoreFingerprint(runtime.Config{Command: "cmd"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": matchingHash,
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	desired := map[string]TemplateParams{
		"worker": {Command: "cmd", SessionName: "worker", TemplateName: "worker"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(store, desired, &stderr)
	if got != 0 {
		t.Fatalf("updated = %d, want 0 (no drift) — stderr=%s", got, stderr.String())
	}

	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if unchanged.Metadata["started_config_hash"] != matchingHash {
		t.Errorf("started_config_hash rewritten unexpectedly = %q, want %q", unchanged.Metadata["started_config_hash"], matchingHash)
	}
}
