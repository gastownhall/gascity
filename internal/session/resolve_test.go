package session_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

type listCountingStore struct {
	beads.Store
	listCalls []beads.ListQuery
}

func (s *listCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls = append(s.listCalls, q)
	return s.Store.List(q)
}

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

type noBroadSessionListStore struct {
	*beads.MemStore
	t *testing.T
}

func (s *noBroadSessionListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == session.LabelSession && len(query.Metadata) == 0 {
		s.t.Fatalf("session resolution used broad session label scan: %+v", query)
	}
	return s.MemStore.List(query)
}

func TestResolveSessionID_UsesTargetedAliasLookup(t *testing.T) {
	store := &noBroadSessionListStore{MemStore: beads.NewMemStore(), t: t}
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
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionIDAllowClosed_UsesTargetedSessionNameLookup(t *testing.T) {
	store := &noBroadSessionListStore{MemStore: beads.NewMemStore(), t: t}
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
		},
	})
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}

	id, err := session.ResolveSessionIDAllowClosed(store, "sky")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
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

func TestResolveSessionID_SessionNameExactMatchAcceptsTypeOnlySessionBead(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type: session.BeadType,
		Metadata: map[string]string{
			"session_name": "s-gc-legacy",
		},
	})

	id, err := session.ResolveSessionID(store, "s-gc-legacy")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_AliasExactMatchAcceptsTypeOnlySessionBead(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type: session.BeadType,
		Metadata: map[string]string{
			"alias": "legacy",
		},
	})

	id, err := session.ResolveSessionID(store, "legacy")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_TrimsMetadataIdentifier(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker",
		},
	})

	id, err := session.ResolveSessionID(store, " worker ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
}

func TestResolveSessionID_WhitespaceOnlyIdentifierDoesNotList(t *testing.T) {
	store := &listCountingStore{Store: beads.NewMemStore()}

	_, err := session.ResolveSessionID(store, "   ")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("ResolveSessionID(whitespace) = %v, want ErrSessionNotFound", err)
	}
	if len(store.listCalls) != 0 {
		t.Fatalf("List calls = %d, want 0 for empty trimmed metadata identifier", len(store.listCalls))
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

func TestResolveSessionID_PrefersSessionNameOverDualAliasSessionNameBead(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"session_name": "worker",
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
		t.Fatalf("got %q, want session-name-only match %q", id, named.ID)
	}
}

func TestResolveSessionID_DualAliasSessionNameBeadWinsWhenNoOtherSessionNameMatch(t *testing.T) {
	store := beads.NewMemStore()
	dual, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"session_name": "worker",
		},
	})
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias": "worker",
		},
	})

	id, err := session.ResolveSessionID(store, "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != dual.ID {
		t.Fatalf("got %q, want dual session-name match %q", id, dual.ID)
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

func TestResolveSessionIDAllowClosed_OpenHitStaysCacheServed(t *testing.T) {
	backing := &listCountingStore{Store: beads.NewMemStore()}
	b, _ := backing.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
		},
	})
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	backing.listCalls = nil

	id, err := session.ResolveSessionIDAllowClosed(cache, "sky")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != b.ID {
		t.Fatalf("got %q, want %q", id, b.ID)
	}
	if len(backing.listCalls) != 0 {
		t.Fatalf("backing List calls = %d, want 0 for cached open match: %+v", len(backing.listCalls), backing.listCalls)
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

func TestResolveSessionID_BoundedListCalls(t *testing.T) {
	inner := beads.NewMemStore()
	for i := 0; i < 200; i++ {
		_, _ = inner.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": fmt.Sprintf("worker-%d", i),
			},
		})
	}
	target, _ := inner.Create(beads.Bead{
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": "mayor"},
	})
	store := &listCountingStore{Store: inner}
	id, err := session.ResolveSessionID(store, "mayor")
	if err != nil || id != target.ID {
		t.Fatalf("resolve failed: id=%q err=%v", id, err)
	}
	if len(store.listCalls) == 0 {
		t.Fatalf("expected at least one List call")
	}
	if len(store.listCalls) != 2 {
		t.Fatalf("List calls = %d, want 2", len(store.listCalls))
	}
	for i, q := range store.listCalls {
		if len(q.Metadata) == 0 {
			t.Fatalf("List call #%d has no metadata filter (would scan all beads): %+v", i, q)
		}
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
