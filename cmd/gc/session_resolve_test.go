package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestResolveSessionID_BeadID(t *testing.T) {
	store := beads.NewMemStore()
	// Create a real session bead so the direct lookup succeeds.
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})

	id, err := resolveSessionID(store, b.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_TemplateName(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:overseer"},
		Metadata: map[string]string{
			"template": "overseer",
		},
	})

	id, err := resolveSessionID(store, "overseer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_QualifiedName(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:myrig/worker"},
		Metadata: map[string]string{
			"template": "myrig/worker",
		},
	})

	// Resolve by bare name.
	id, err := resolveSessionID(store, "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}

	// Resolve by qualified name.
	id, err = resolveSessionID(store, "myrig/worker")
	if err != nil {
		t.Fatalf("unexpected error for qualified: %v", err)
	}
	if id != b.ID {
		t.Errorf("qualified: got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_Ambiguous(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template": "worker",
		},
	})
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template": "worker",
		},
	})

	_, err := resolveSessionID(store, "worker")
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention ambiguous, got: %v", err)
	}
}

func TestResolveSessionID_NotFound(t *testing.T) {
	store := beads.NewMemStore()
	_, err := resolveSessionID(store, "nonexistent")
	if err == nil {
		t.Fatal("expected not found error")
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Errorf("error should mention not found, got: %v", err)
	}
}

func TestResolveSessionID_SkipsClosedBeads(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template": "worker",
		},
	})
	_ = store.Close(b.ID)

	_, err := resolveSessionID(store, "worker")
	if err == nil {
		t.Fatal("expected not found for closed session")
	}
}

func TestResolveSessionIDAllowClosed_ResolvesClosedNamedSession(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
		},
	})
	_ = store.Close(b.ID)

	id, err := resolveSessionIDAllowClosed(store, "sky")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}
