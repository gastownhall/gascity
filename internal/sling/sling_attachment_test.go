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
			name: "routed_to only",
			bead: beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/polecat"}},
			want: []string{`warning: bead bd-1 already routed to "rig/polecat"`},
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
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := routedStateWarnings(tt.bead, "bd-1")
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("routedStateWarnings() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestCheckBeadStateWithOptions_RoutingConflict pins which routing states
// CheckBeadStateWithOptions reports as a hard Conflict: only a non-blank
// gc.routed_to that names neither the sling target nor the caller. A caller
// re-routing work its own identity already holds is not a conflict, an
// unresolved (empty) caller identity never matches a route, and assignee or
// pool: label state stays a warning.
func TestCheckBeadStateWithOptions_RoutingConflict(t *testing.T) {
	single := config.Agent{Name: "deacon", MaxActiveSessions: intPtr(1)}
	pool := config.Agent{Name: "worker", MaxActiveSessions: intPtr(3)}
	custom := config.Agent{Name: "deacon", SlingQuery: "label:special"}

	tests := []struct {
		name         string
		target       config.Agent
		bead         beads.Bead
		caller       string
		wantConflict string // substring of the conflict error; empty means none
		wantIdem     bool
	}{
		{
			name:         "third-party route conflicts",
			target:       single,
			bead:         beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/polecat"}},
			caller:       "rig/reviewer",
			wantConflict: `already routed to "rig/polecat"`,
		},
		{
			name:   "route held by the caller is not a conflict",
			target: single,
			bead:   beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/reviewer"}},
			caller: "rig/reviewer",
		},
		{
			name:         "unresolved caller fails closed",
			target:       single,
			bead:         beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/polecat"}},
			caller:       "",
			wantConflict: `already routed to "rig/polecat"`,
		},
		{
			name:   "blank routed_to is not a conflict",
			target: single,
			bead:   beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "  "}},
			caller: "",
		},
		{
			name:   "absent routed_to with an assignee stays a warning",
			target: single,
			bead:   beads.Bead{Assignee: "rig/polecat"},
			caller: "rig/reviewer",
		},
		{
			name:   "pool label without routed_to stays a warning",
			target: single,
			bead:   beads.Bead{Labels: []string{"pool:builders"}},
			caller: "rig/reviewer",
		},
		{
			name:     "route already at the target is idempotent, not a conflict",
			target:   single,
			bead:     beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: single.QualifiedName()}},
			caller:   "",
			wantIdem: true,
		},
		{
			name:   "custom sling query never reports a conflict",
			target: custom,
			bead:   beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/polecat"}},
			caller: "",
		},
		{
			name:         "pool target: third-party route conflicts",
			target:       pool,
			bead:         beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/polecat"}},
			caller:       "rig/reviewer",
			wantConflict: `already routed to "rig/polecat"`,
		},
		{
			name:   "pool target: route held by the caller is not a conflict",
			target: pool,
			bead:   beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/reviewer"}},
			caller: "rig/reviewer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.bead
			b.ID = "bd-1"
			store := beads.NewMemStoreFrom(0, []beads.Bead{b}, nil)
			result := CheckBeadStateWithOptions(store, "bd-1", tt.target, SlingDeps{CallerIdentity: tt.caller}, BeadCheckOptions{NoConvoy: true})
			if tt.wantConflict == "" {
				if result.Conflict != nil {
					t.Fatalf("Conflict = %v, want nil", result.Conflict)
				}
			} else {
				if result.Conflict == nil {
					t.Fatalf("Conflict = nil, want error containing %q", tt.wantConflict)
				}
				if got := result.Conflict.Error(); !strings.Contains(got, tt.wantConflict) || !strings.Contains(got, "--force") {
					t.Fatalf("Conflict = %q, want %q and a --force hint", got, tt.wantConflict)
				}
			}
			if result.Idempotent != tt.wantIdem {
				t.Fatalf("Idempotent = %v, want %v", result.Idempotent, tt.wantIdem)
			}
		})
	}
}
