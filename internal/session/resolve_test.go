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

func TestResolveSessionIDByExactID_OnlyAcceptsSessionBeads(t *testing.T) {
	store := beads.NewMemStore()
	task, _ := store.Create(beads.Bead{
		Type:   "task",
		Labels: []string{"other"},
	})

	_, err := session.ResolveSessionIDByExactID(store, task.ID)
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("ResolveSessionIDByExactID(task) = %v, want ErrSessionNotFound", err)
	}
}

func TestResolveSessionIDByExactID_RepairsEmptyTypeSessionBead(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	emptyType := ""
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &emptyType}); err != nil {
		t.Fatal(err)
	}

	id, err := session.ResolveSessionIDByExactID(store, b.ID)
	if err != nil {
		t.Fatalf("ResolveSessionIDByExactID() error = %v", err)
	}
	if id != b.ID {
		t.Fatalf("ResolveSessionIDByExactID() = %q, want %q", id, b.ID)
	}
	stored, _ := store.Get(b.ID)
	if stored.Type != session.BeadType {
		t.Fatalf("stored type = %q, want %q", stored.Type, session.BeadType)
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

func TestResolveSessionID_DoesNotResolveExactQualifiedTemplate(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template": "myrig/worker",
			"state":    "creating",
		},
	})

	_, err := session.ResolveSessionID(store, "myrig/worker")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("ResolveSessionID(exact template) = %v, want ErrSessionNotFound", err)
	}
}

func TestResolveSessionID_DoesNotResolveTemplateBasename(t *testing.T) {
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
		t.Fatal("expected agent name to stay unresolved")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestResolveSessionID_DoesNotResolveExactAgentName(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "agent:myrig/worker-1"},
		Metadata: map[string]string{
			"template":     "myrig/worker",
			"agent_name":   "myrig/worker-1",
			"session_name": "s-gc-123",
			"state":        "awake",
		},
	})

	_, err := session.ResolveSessionID(store, "myrig/worker-1")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("ResolveSessionID(exact agent_name) = %v, want ErrSessionNotFound", err)
	}
}

func TestResolveSessionID_DoesNotResolveExactTemplateWithOpenCandidate(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":       "gascity/claude",
			"session_name":   "s-old",
			"state":          "asleep",
			"sleep_reason":   "drained",
			"manual_session": "true",
		},
	})
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":             "gascity/claude",
			"session_name":         "s-new",
			"state":                "creating",
			"pending_create_claim": "true",
			"manual_session":       "true",
		},
	})

	_, err := session.ResolveSessionID(store, "gascity/claude")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("ResolveSessionID(exact template) = %v, want ErrSessionNotFound", err)
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

func TestResolveSessionID_DoesNotResolveHistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor,witness",
		},
	})

	_, err := session.ResolveSessionID(store, "mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("ResolveSessionID(historical alias) = %v, want ErrSessionNotFound", err)
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

func TestResolveSessionIDAllowClosed_DoesNotResolveClosedHistoricalAlias(t *testing.T) {
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

	_, err := session.ResolveSessionIDAllowClosed(store, "mayor")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("ResolveSessionIDAllowClosed(historical alias) = %v, want ErrSessionNotFound", err)
	}
}

func TestResolveSessionIDAllowClosed_DoesNotUseLiveTemplateOverClosedSessionName(t *testing.T) {
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
		t.Fatalf("got %q, want closed session-name match %q", id, closed.ID)
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

func TestResolveSessionID_RepairsEmptyTypeDirectLookup(t *testing.T) {
	store := beads.NewMemStore()
	// Create a session bead then corrupt its type to empty.
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "mayor",
		},
	})
	emptyType := ""
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &emptyType}); err != nil {
		t.Fatal(err)
	}

	// Direct lookup by bead ID should repair and resolve.
	id, err := session.ResolveSessionID(store, b.ID)
	if err != nil {
		t.Fatalf("expected resolution to succeed, got: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}

	// Verify the store was repaired.
	stored, _ := store.Get(b.ID)
	if stored.Type != session.BeadType {
		t.Errorf("stored type = %q, want %q", stored.Type, session.BeadType)
	}
}

func TestResolveSessionID_RepairsEmptyTypeAliasLookup(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "overseer",
		},
	})
	emptyType := ""
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &emptyType}); err != nil {
		t.Fatal(err)
	}

	// Alias lookup should still resolve via the gc:session label.
	id, err := session.ResolveSessionID(store, "overseer")
	if err != nil {
		t.Fatalf("expected resolution to succeed, got: %v", err)
	}
	if id != b.ID {
		t.Errorf("got %q, want %q", id, b.ID)
	}

	// Verify the store was repaired.
	stored, _ := store.Get(b.ID)
	if stored.Type != session.BeadType {
		t.Errorf("stored type = %q, want %q", stored.Type, session.BeadType)
	}
}

func TestResolveSessionID_SkipsEmptyTypeWithoutLabel(t *testing.T) {
	store := beads.NewMemStore()
	// A bead with empty type and no gc:session label should not be treated
	// as a session bead.
	b, _ := store.Create(beads.Bead{
		Type:   "task",
		Labels: []string{"other"},
		Metadata: map[string]string{
			"session_name": "mayor",
		},
	})
	emptyType := ""
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &emptyType}); err != nil {
		t.Fatal(err)
	}

	_, err := session.ResolveSessionID(store, b.ID)
	if err == nil {
		t.Fatal("expected not found for non-session bead with empty type")
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got: %v", err)
	}
}

func TestIsSessionBeadOrRepairable(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want bool
	}{
		{
			name: "normal session bead",
			bead: beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}},
			want: true,
		},
		{
			name: "empty type with session label",
			bead: beads.Bead{Type: "", Labels: []string{session.LabelSession}},
			want: true,
		},
		{
			name: "empty type without session label",
			bead: beads.Bead{Type: "", Labels: []string{"other"}},
			want: false,
		},
		{
			name: "wrong type with session label",
			bead: beads.Bead{Type: "task", Labels: []string{session.LabelSession}},
			want: false,
		},
		{
			name: "empty type with no labels",
			bead: beads.Bead{Type: ""},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := session.IsSessionBeadOrRepairable(tt.bead); got != tt.want {
				t.Errorf("IsSessionBeadOrRepairable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRepairEmptyType(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	emptyType := ""
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &emptyType}); err != nil {
		t.Fatal(err)
	}
	// Re-read so the local copy has the empty type.
	b, _ = store.Get(b.ID)

	session.RepairEmptyType(store, &b)

	// In-memory bead should be repaired.
	if b.Type != session.BeadType {
		t.Errorf("in-memory type = %q, want %q", b.Type, session.BeadType)
	}
	// Store should be repaired.
	stored, _ := store.Get(b.ID)
	if stored.Type != session.BeadType {
		t.Errorf("stored type = %q, want %q", stored.Type, session.BeadType)
	}
}

func TestRepairEmptyType_NoopForNonEmpty(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})

	// Should be a no-op when type is already set.
	session.RepairEmptyType(store, &b)
	if b.Type != session.BeadType {
		t.Errorf("type = %q, want %q", b.Type, session.BeadType)
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
