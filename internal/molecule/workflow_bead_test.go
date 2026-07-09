package molecule

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestWorkflowStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		bead beads.Bead
		want string
	}{
		// Direct ports of the api workflowStatus subtests so the body move is
		// oracle-checked against the pre-refactor behavior.
		{
			name: "open assigned is pending",
			bead: beads.Bead{
				Status:   "open",
				Assignee: "assigned-role",
				Metadata: map[string]string{"gc.routed_to": "routed-role"},
			},
			want: "pending",
		},
		{
			name: "in_progress unassigned is pending",
			bead: beads.Bead{Status: "in_progress"},
			want: "pending",
		},
		{
			name: "in_progress routed-only is pending",
			bead: beads.Bead{
				Status:   "in_progress",
				Metadata: map[string]string{"gc.routed_to": "routed-role"},
			},
			want: "pending",
		},
		{
			name: "closed skipped is skipped",
			bead: beads.Bead{
				Status:   "closed",
				Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped},
			},
			want: "skipped",
		},
		// Full coverage of the remaining switch arms.
		{
			name: "in_progress assigned is active",
			bead: beads.Bead{Status: "in_progress", Assignee: "worker-1"},
			want: "active",
		},
		{
			name: "closed plain is completed",
			bead: beads.Bead{Status: "closed"},
			want: "completed",
		},
		{
			name: "closed fail is failed",
			bead: beads.Bead{
				Status:   "closed",
				Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail},
			},
			want: "failed",
		},
		{
			name: "unknown status passes through",
			bead: beads.Bead{Status: "quarantined"},
			want: "quarantined",
		},
		{
			name: "unknown status with fail outcome is failed",
			bead: beads.Bead{
				Status:   "quarantined",
				Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail},
			},
			want: "failed",
		},
		{
			name: "unknown status with skipped outcome is skipped",
			bead: beads.Bead{
				Status:   "quarantined",
				Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped},
			},
			want: "skipped",
		},
		{
			name: "whitespace-padded status and outcome are trimmed",
			bead: beads.Bead{
				Status:   "  closed  ",
				Metadata: map[string]string{beadmeta.OutcomeMetadataKey: "  fail  "},
			},
			want: "failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WorkflowStatus(tc.bead); got != tc.want {
				t.Fatalf("WorkflowStatus(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestWorkflowKind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "gc.kind wins over Type",
			bead: beads.Bead{
				Type:     "task",
				Metadata: map[string]string{beadmeta.KindMetadataKey: "workflow"},
			},
			want: "workflow",
		},
		{
			name: "falls back to Type when gc.kind absent",
			bead: beads.Bead{Type: "task"},
			want: "task",
		},
		{
			name: "falls back to Type when gc.kind blank",
			bead: beads.Bead{
				Type:     "task",
				Metadata: map[string]string{beadmeta.KindMetadataKey: "   "},
			},
			want: "task",
		},
		{
			name: "trims padded gc.kind",
			bead: beads.Bead{
				Type:     "task",
				Metadata: map[string]string{beadmeta.KindMetadataKey: "  workflow  "},
			},
			want: "workflow",
		},
		{
			name: "trims padded Type fallback",
			bead: beads.Bead{Type: "  run  "},
			want: "run",
		},
		{
			name: "nil metadata is safe",
			bead: beads.Bead{Type: "run"},
			want: "run",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WorkflowKind(tc.bead); got != tc.want {
				t.Fatalf("WorkflowKind(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestWorkflowAttempt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		bead beads.Bead
		want int
	}{
		{
			name: "numeric attempt parses",
			bead: beads.Bead{Metadata: map[string]string{beadmeta.AttemptMetadataKey: "3"}},
			want: 3,
		},
		{
			name: "missing attempt is zero",
			bead: beads.Bead{},
			want: 0,
		},
		{
			name: "empty attempt is zero",
			bead: beads.Bead{Metadata: map[string]string{beadmeta.AttemptMetadataKey: ""}},
			want: 0,
		},
		{
			name: "non-numeric attempt is zero",
			bead: beads.Bead{Metadata: map[string]string{beadmeta.AttemptMetadataKey: "abc"}},
			want: 0,
		},
		{
			name: "padded numeric attempt is trimmed then parsed",
			bead: beads.Bead{Metadata: map[string]string{beadmeta.AttemptMetadataKey: "  7  "}},
			want: 7,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WorkflowAttempt(tc.bead); got != tc.want {
				t.Fatalf("WorkflowAttempt(%q) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestWorkflowBeadFromBead_AllFields(t *testing.T) {
	t.Parallel()

	b := beads.Bead{
		ID:       "step-1",
		Title:    "Do the thing",
		Status:   "in_progress",
		Assignee: "  worker-1  ",
		Type:     "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:          "  run  ",
			beadmeta.OutcomeMetadataKey:       "",
			beadmeta.AttemptMetadataKey:       "  2  ",
			beadmeta.StepRefMetadataKey:       "  iteration.1.review  ",
			beadmeta.LogicalBeadIDMetadataKey: "  logical-9  ",
			beadmeta.ScopeRefMetadataKey:      "  gascity  ",
		},
	}

	got := WorkflowBeadFromBead(b)
	want := WorkflowBead{
		ID:            "step-1",
		Title:         "Do the thing",
		Status:        "active",
		Kind:          "run",
		StepRef:       "iteration.1.review",
		Attempt:       2,
		LogicalBeadID: "logical-9",
		ScopeRef:      "gascity",
		Assignee:      "worker-1",
		Metadata:      b.Metadata,
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WorkflowBeadFromBead mismatch:\n got=%#v\nwant=%#v", got, want)
	}
}

func TestWorkflowBeadFromBead_MetadataClone(t *testing.T) {
	t.Parallel()

	src := map[string]string{beadmeta.KindMetadataKey: "workflow"}
	b := beads.Bead{ID: "root-1", Metadata: src}

	got := WorkflowBeadFromBead(b)
	if got.Metadata == nil {
		t.Fatal("expected cloned metadata, got nil")
	}
	// Mutating the source after projection must not change the clone.
	src[beadmeta.KindMetadataKey] = "mutated"
	if got.Metadata[beadmeta.KindMetadataKey] != "workflow" {
		t.Fatalf("clone not independent: got %q", got.Metadata[beadmeta.KindMetadataKey])
	}

	// nil source metadata projects to nil (not an empty map) so the wire keeps
	// emitting "metadata": null for nil-metadata beads.
	nilGot := WorkflowBeadFromBead(beads.Bead{ID: "root-2"})
	if nilGot.Metadata != nil {
		t.Fatalf("nil metadata projected to non-nil: %#v", nilGot.Metadata)
	}
}
