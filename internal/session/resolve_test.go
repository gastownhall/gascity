package session_test

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestResolveSessionID_DirectLookup(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})

	id, err := session.ResolveSessionID(store, b.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_Alias(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "overseer",
		},
	})

	id, err := session.ResolveSessionID(store, "overseer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_DoesNotResolveTemplateName(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template": "myrig/worker",
		},
	})

	_, err := session.ResolveSessionID(store, "worker")
	if err == nil {
		t.Fatal("expected template name to stay unresolved")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestResolveSessionID_DoesNotResolveAgentName(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"agent_name": "myrig/worker",
		},
	})

	_, err := session.ResolveSessionID(store, "worker")
	if err == nil {
		t.Fatal("expected agent name to stay unresolved")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestResolveSessionID_SessionNameExactMatch(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-123",
			"agent_name":   "myrig/worker",
		},
	})

	id, err := session.ResolveSessionID(store, "s-gc-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_PrefersSessionNameOverAlias(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"session_name": "s-gc-1",
		},
	})
	named, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker",
		},
	})

	id, err := session.ResolveSessionID(store, "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != named.ID {
		t.Fatalf("got %q, want session-name match %q", id, named.ID)
	}
}

func TestResolveSessionID_HistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor,witness",
		},
	})

	id, err := session.ResolveSessionID(store, "mayor")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_PrefersCurrentAliasOverHistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	current, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "mayor",
		},
	})
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})

	id, err := session.ResolveSessionID(store, "mayor")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != current.ID {
		t.Fatalf("got %q, want live current alias %q", id, current.ID)
	}
}

func TestResolveSessionID_DoesNotResolveClosedSessionNameByDefault(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
			"template":     "worker",
		},
	})
	_ = store.Close(b.ID)

	_, err := session.ResolveSessionID(store, "sky")
	if err == nil {
		t.Fatal("expected closed named session to stay hidden from live resolver")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestResolveSessionIDAllowClosed_ResolvesClosedSessionName(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
			"template":     "worker",
		},
	})
	_ = store.Close(b.ID)

	id, err := session.ResolveSessionIDAllowClosed(store, "sky")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionIDAllowClosed_ResolvesClosedHistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})
	_ = store.Close(b.ID)

	id, err := session.ResolveSessionIDAllowClosed(store, "mayor")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionIDAllowClosed_ClosedExactBeatsLiveExactIdentifierMatch(t *testing.T) {
	store := beads.NewMemStore()
	closed, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker",
		},
	})
	_ = store.Close(closed.ID)
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template": "worker",
		},
	})

	id, err := session.ResolveSessionIDAllowClosed(store, "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != closed.ID {
		t.Fatalf("got %q, want closed exact-name session %q", id, closed.ID)
	}
}

func TestResolveSessionIDAllowClosed_ClosedExactBeatsLiveSuffixMatch(t *testing.T) {
	store := beads.NewMemStore()
	closed, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker",
		},
	})
	_ = store.Close(closed.ID)
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template": "myrig/worker",
		},
	})

	id, err := session.ResolveSessionIDAllowClosed(store, "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != closed.ID {
		t.Fatalf("got %q, want closed exact-name session %q", id, closed.ID)
	}
}

func TestResolveSessionID_AliasAmbiguous(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "worker",
		},
	})
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "worker",
		},
	})

	_, err := session.ResolveSessionID(store, "worker")
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !errors.Is(err, session.ErrAmbiguous) {
		t.Fatalf("expected ErrAmbiguous, got %v", err)
	}
}

func TestResolveSessionID_NotFound(t *testing.T) {
	store := beads.NewMemStore()
	_, err := session.ResolveSessionID(store, "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got: %v", err)
	}
}

func TestResolveSessionID_Ambiguous(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "worker",
		},
	})
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "worker",
		},
	})

	_, err := session.ResolveSessionID(store, "worker")
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !errors.Is(err, session.ErrAmbiguous) {
		t.Errorf("expected ErrAmbiguous, got: %v", err)
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

	_, err := session.ResolveSessionID(store, "worker")
	if err == nil {
		t.Fatal("expected not found for closed session")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got: %v", err)
	}
}
