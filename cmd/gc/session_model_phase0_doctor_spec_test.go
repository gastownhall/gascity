package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Diagnostics
// - Doctor contract

func TestPhase0DoctorReportsClosedBeadOwner(t *testing.T) {
	cityPath, store := newPhase0DoctorCity(t)

	closed, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-closed",
			"template":     "worker",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close(%s): %v", closed.ID, err)
	}
	if _, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Title:    "stale owner",
		Assignee: closed.ID,
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "closed-bead-owner") {
		t.Fatalf("doctor output missing closed-bead-owner finding:\n%s", out)
	}
}

func TestPhase0DoctorReportsStaleRoutedConfig(t *testing.T) {
	cityPath, store := newPhase0DoctorCity(t)

	if _, err := store.Create(beads.Bead{
		Type:   "task",
		Status: "open",
		Title:  "stale route",
		Metadata: map[string]string{
			"gc.routed_to": "missing-config",
		},
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "stale-routed-config") {
		t.Fatalf("doctor output missing stale-routed-config finding:\n%s", out)
	}
}

func newPhase0DoctorCity(t *testing.T) (string, *beads.FileStore) {
	t.Helper()

	cityPath := t.TempDir()
	configText := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityPath, ".gc", "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	return cityPath, store
}
