package graphroute

import (
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestUnroutedMemberPatch(t *testing.T) {
	pool := GraphRouteBinding{QualifiedName: "worker", MetadataOnly: true}
	oneShotPool := GraphRouteBinding{QualifiedName: "worker", MetadataOnly: true, IndependentSteps: true}
	rigPool := GraphRouteBinding{QualifiedName: "myrig/worker", MetadataOnly: true}
	solo := GraphRouteBinding{QualifiedName: "solo", SessionName: "test-city--solo"}

	tests := []struct {
		name    string
		bead    beads.Bead
		binding GraphRouteBinding
		want    map[string]string
	}{
		{
			name:    "pool step",
			bead:    beads.Bead{ID: "s1", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: ""}},
			binding: pool,
			want:    map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
		},
		{
			name:    "single-session target stamps its session name",
			bead:    beads.Bead{ID: "s1", Status: "open", Metadata: map[string]string{}},
			binding: solo,
			want:    map[string]string{beadmeta.RoutedToMetadataKey: "solo", beadmeta.SessionNameMetadataKey: "test-city--solo"},
		},
		{
			name: "one-shot pool keeps a formula-declared continuation group",
			bead: beads.Bead{ID: "s1", Status: "open", Metadata: map[string]string{
				beadmeta.ContinuationGroupMetadataKey: "batch",
			}},
			binding: oneShotPool,
			want: map[string]string{
				beadmeta.RoutedToMetadataKey:        "worker",
				beadmeta.SessionAffinityMetadataKey: "require",
			},
		},
		{
			name: "drain group on a pool gets only the route",
			bead: beads.Bead{ID: "s1", Status: "open", Metadata: map[string]string{
				beadmeta.ContinuationGroupMetadataKey: "drain:cv-1",
			}},
			binding: pool,
			want:    map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
		},
		{
			name: "drain group on a one-shot pool gets only the route",
			bead: beads.Bead{ID: "s1", Status: "open", Metadata: map[string]string{
				beadmeta.ContinuationGroupMetadataKey: "drain:cv-1",
			}},
			binding: oneShotPool,
			want:    map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
		},
		{
			name: "control with an empty execution route on a city pool",
			bead: beads.Bead{ID: "c1", Status: "open", Metadata: map[string]string{
				beadmeta.KindMetadataKey:     beadmeta.KindCheck,
				beadmeta.RoutedToMetadataKey: config.ControlDispatcherAgentName,
			}},
			binding: pool,
			want:    map[string]string{beadmeta.ExecutionRoutedToMetadataKey: "worker"},
		},
		{
			name: "control on a rig pool carries the rig execution context",
			bead: beads.Bead{ID: "c1", Status: "open", Metadata: map[string]string{
				beadmeta.KindMetadataKey:     beadmeta.KindCheck,
				beadmeta.RoutedToMetadataKey: "myrig/" + config.ControlDispatcherAgentName,
			}},
			binding: rigPool,
			want: map[string]string{
				beadmeta.ExecutionRoutedToMetadataKey:   "myrig/worker",
				beadmeta.ExecutionRigContextMetadataKey: "myrig",
			},
		},
		{
			name: "control with an execution route",
			bead: beads.Bead{ID: "c1", Status: "open", Metadata: map[string]string{
				beadmeta.KindMetadataKey:              beadmeta.KindCheck,
				beadmeta.RoutedToMetadataKey:          config.ControlDispatcherAgentName,
				beadmeta.ExecutionRoutedToMetadataKey: "other",
			}},
			binding: pool,
		},
		{
			name:    "scope latch",
			bead:    beads.Bead{ID: "t1", Status: "open", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScope}},
			binding: pool,
		},
		{
			name:    "spec sidecar",
			bead:    beads.Bead{ID: "t1", Status: "open", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindSpec}},
			binding: pool,
		},
		{
			name:    "nested workflow root",
			bead:    beads.Bead{ID: "t1", Status: "open", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
			binding: pool,
		},
		{
			name:    "closed step",
			bead:    beads.Bead{ID: "s1", Status: "closed", Metadata: map[string]string{}},
			binding: pool,
		},
		{
			name:    "assigned step",
			bead:    beads.Bead{ID: "s1", Status: "in_progress", Assignee: "someone", Metadata: map[string]string{}},
			binding: pool,
		},
		{
			name:    "direct session binding",
			bead:    beads.Bead{ID: "s1", Status: "open", Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gc-7"}},
			binding: pool,
		},
		{
			name:    "explicitly routed step",
			bead:    beads.Bead{ID: "s1", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "reviewer"}},
			binding: pool,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := maps.Clone(tc.bead.Metadata)
			got, err := UnroutedMemberPatch(tc.bead, tc.binding)
			if err != nil {
				t.Fatalf("UnroutedMemberPatch: %v", err)
			}
			if !maps.Equal(got, tc.want) {
				t.Fatalf("patch = %v, want %v", got, tc.want)
			}
			if !maps.Equal(tc.bead.Metadata, before) {
				t.Fatalf("UnroutedMemberPatch mutated the bead: %v, was %v", tc.bead.Metadata, before)
			}
		})
	}
}
