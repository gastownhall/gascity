package molecule

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func awaitingFinalizeRoot(kind, status string) beads.Bead {
	return beads.Bead{
		ID: "wf-root", Status: status, Assignee: "sess-1",
		Metadata: map[string]string{beadmeta.KindMetadataKey: kind},
	}
}

func awaitingFinalizeMember(id, status, kind string) beads.Bead {
	return beads.Bead{ID: id, Status: status, Metadata: map[string]string{
		beadmeta.KindMetadataKey:       kind,
		beadmeta.RootBeadIDMetadataKey: "wf-root",
	}}
}

func TestWorkflowAwaitingFinalize(t *testing.T) {
	closedStep := awaitingFinalizeMember("step-a", "closed", "task")
	openFinalizer := awaitingFinalizeMember("fin", "open", beadmeta.KindWorkflowFinalize)
	foreignOpenStep := beads.Bead{ID: "other-step", Status: "open", Metadata: map[string]string{
		beadmeta.KindMetadataKey: "task", beadmeta.RootBeadIDMetadataKey: "other-root",
	}}
	for _, tc := range []struct {
		name    string
		root    beads.Bead
		members []beads.Bead
		want    bool
	}{
		{
			"all closed", awaitingFinalizeRoot("workflow", "in_progress"),
			[]beads.Bead{closedStep, awaitingFinalizeMember("fin", "closed", beadmeta.KindWorkflowFinalize)},
			true,
		},
		{"finalizer open", awaitingFinalizeRoot("workflow", "in_progress"), []beads.Bead{closedStep, openFinalizer}, true},
		{
			"finalizer and scope open", awaitingFinalizeRoot("workflow", "in_progress"),
			[]beads.Bead{closedStep, openFinalizer, awaitingFinalizeMember("scope", "open", beadmeta.KindScope)},
			true,
		},
		{
			"finalizer and spec open", awaitingFinalizeRoot("workflow", "in_progress"),
			[]beads.Bead{closedStep, openFinalizer, awaitingFinalizeMember("spec", "open", beadmeta.KindSpec)},
			true,
		},
		{
			"padded kinds", awaitingFinalizeRoot(" workflow ", "in_progress"),
			[]beads.Bead{closedStep, awaitingFinalizeMember("fin", "open", " workflow-finalize ")},
			true,
		},
		{
			"root row and foreign rows ignored", awaitingFinalizeRoot("workflow", "in_progress"),
			[]beads.Bead{awaitingFinalizeRoot("workflow", "in_progress"), closedStep, openFinalizer, foreignOpenStep},
			true,
		},
		{
			"open step", awaitingFinalizeRoot("workflow", "in_progress"),
			[]beads.Bead{closedStep, awaitingFinalizeMember("step-b", "open", "task")},
			false,
		},
		{
			"in_progress step", awaitingFinalizeRoot("workflow", "in_progress"),
			[]beads.Bead{closedStep, awaitingFinalizeMember("step-b", "in_progress", "task")},
			false,
		},
		{
			"open retry control", awaitingFinalizeRoot("workflow", "in_progress"),
			[]beads.Bead{closedStep, awaitingFinalizeMember("retry", "open", beadmeta.KindRetry)},
			false,
		},
		{
			"open nested workflow", awaitingFinalizeRoot("workflow", "in_progress"),
			[]beads.Bead{closedStep, awaitingFinalizeMember("nested", "open", beadmeta.KindWorkflow)},
			false,
		},
		{"root only", awaitingFinalizeRoot("workflow", "in_progress"), nil, false},
		{
			"fresh", awaitingFinalizeRoot("workflow", "in_progress"),
			[]beads.Bead{awaitingFinalizeMember("step-a", "open", "task"), openFinalizer},
			false,
		},
		{
			"only topology closed", awaitingFinalizeRoot("workflow", "in_progress"),
			[]beads.Bead{awaitingFinalizeMember("scope", "closed", beadmeta.KindScope), openFinalizer},
			false,
		},
		{"closed root", awaitingFinalizeRoot("workflow", "closed"), []beads.Bead{closedStep, openFinalizer}, false},
		{"not a workflow root", awaitingFinalizeRoot("task", "in_progress"), []beads.Bead{closedStep, openFinalizer}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WorkflowAwaitingFinalize(tc.root, tc.members); got != tc.want {
				t.Fatalf("WorkflowAwaitingFinalize() = %v, want %v", got, tc.want)
			}
		})
	}
}
