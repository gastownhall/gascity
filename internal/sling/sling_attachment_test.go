package sling

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestRoutedStateWarnings(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want []string
		// wantConflict, when non-empty, must be a substring of the returned
		// conflict error. When empty, the conflict must be nil.
		wantConflict string
	}{
		{
			name: "clean bead has no warnings",
			bead: beads.Bead{},
			want: nil,
		},
		{
			name: "assignee only",
			bead: beads.Bead{Assignee: "rig/polecat"},
			want: []string{`warning: bead bd-1 already assigned to "rig/polecat"`},
		},
		{
			name:         "routed_to only",
			bead:         beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/polecat"}},
			want:         []string{`warning: bead bd-1 already routed to "rig/polecat"`},
			wantConflict: `bd-1 already routed to "rig/polecat"`,
		},
		{
			name: "blank routed_to metadata is ignored",
			bead: beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "  "}},
			want: nil,
		},
		{
			name: "pool label only",
			bead: beads.Bead{Labels: []string{"pool:builders"}},
			want: []string{`warning: bead bd-1 already has pool label "pool:builders"`},
		},
		{
			name: "non-pool labels are ignored",
			bead: beads.Bead{Labels: []string{"kind:task", "priority:high"}},
			want: nil,
		},
		{
			name: "all three states, ordered assignee then routed_to then labels",
			bead: beads.Bead{
				Assignee: "rig/polecat",
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/deacon"},
				Labels:   []string{"kind:task", "pool:builders", "pool:reviewers"},
			},
			want: []string{
				`warning: bead bd-1 already assigned to "rig/polecat"`,
				`warning: bead bd-1 already routed to "rig/deacon"`,
				`warning: bead bd-1 already has pool label "pool:builders"`,
				`warning: bead bd-1 already has pool label "pool:reviewers"`,
			},
			wantConflict: `bd-1 already routed to "rig/deacon"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotWarnings, gotConflict := routedStateWarnings(tt.bead, "bd-1")
			if !reflect.DeepEqual(gotWarnings, tt.want) {
				t.Fatalf("routedStateWarnings() warnings = %#v, want %#v", gotWarnings, tt.want)
			}
			if tt.wantConflict == "" {
				if gotConflict != nil {
					t.Fatalf("routedStateWarnings() conflict = %v, want nil", gotConflict)
				}
				return
			}
			if gotConflict == nil {
				t.Fatalf("routedStateWarnings() conflict = nil, want error containing %q", tt.wantConflict)
			}
			if !strings.Contains(gotConflict.Error(), tt.wantConflict) {
				t.Fatalf("routedStateWarnings() conflict = %q, want substring %q", gotConflict.Error(), tt.wantConflict)
			}
		})
	}
}

// TestCheckBeadStateWithOptions_RoutingConflict pins the higher-level
// behavior CheckBeadStateWithOptions must expose so gc sling can hard-refuse
// a conflicting gc.routed_to overwrite: a bead already routed to a different
// target reports Conflict; a bead already routed to the checked target is
// idempotent, not a conflict; custom sling-query agents never see a
// conflict (they keep the existing unconditional warn-and-proceed
// behavior); and an assignee alone (no gc.routed_to metadata) is not a
// routing conflict.
func TestCheckBeadStateWithOptions_RoutingConflict(t *testing.T) {
	single := config.Agent{Name: "deacon", MaxActiveSessions: intPtr(1)}

	t.Run("routed to a different target reports conflict", func(t *testing.T) {
		b := beads.Bead{
			ID:       "bd-1",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/polecat"},
		}
		store := beads.NewMemStoreFrom(0, []beads.Bead{b}, nil)
		result := CheckBeadStateWithOptions(store, "bd-1", single, SlingDeps{}, BeadCheckOptions{})
		if result.Conflict == nil {
			t.Fatal("expected a routing conflict, got nil")
		}
	})

	t.Run("already routed to the checked target is idempotent, not a conflict", func(t *testing.T) {
		b := beads.Bead{
			ID:       "bd-1",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: single.QualifiedName()},
		}
		store := beads.NewMemStoreFrom(0, []beads.Bead{b}, nil)
		// NoConvoy isolates this check from the pre-existing convoy-recovery
		// path (resolveConvoyRecovery), which is orthogonal to routing
		// conflict detection and would otherwise report Idempotent=false
		// here simply because the fixture has no tracking convoy bead.
		result := CheckBeadStateWithOptions(store, "bd-1", single, SlingDeps{}, BeadCheckOptions{NoConvoy: true})
		if result.Conflict != nil {
			t.Fatalf("expected no conflict for an idempotent route, got %v", result.Conflict)
		}
		if !result.Idempotent {
			t.Fatal("expected Idempotent=true when routed_to already matches the target")
		}
	})

	t.Run("custom sling query agents are never reported as conflicting", func(t *testing.T) {
		custom := config.Agent{
			Name:       "deacon",
			SlingQuery: "label:special",
		}
		b := beads.Bead{
			ID:       "bd-1",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/polecat"},
		}
		store := beads.NewMemStoreFrom(0, []beads.Bead{b}, nil)
		result := CheckBeadStateWithOptions(store, "bd-1", custom, SlingDeps{}, BeadCheckOptions{})
		if result.Conflict != nil {
			t.Fatalf("custom sling query agents must not report routing conflicts, got %v", result.Conflict)
		}
	})

	t.Run("assignee without routed_to metadata is not a routing conflict", func(t *testing.T) {
		b := beads.Bead{
			ID:       "bd-1",
			Assignee: "rig/polecat",
		}
		store := beads.NewMemStoreFrom(0, []beads.Bead{b}, nil)
		result := CheckBeadStateWithOptions(store, "bd-1", single, SlingDeps{}, BeadCheckOptions{})
		if result.Conflict != nil {
			t.Fatalf("expected no conflict when gc.routed_to is unset, got %v", result.Conflict)
		}
	})
}
