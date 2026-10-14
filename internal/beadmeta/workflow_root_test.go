package beadmeta

import "testing"

// TestIsExpandedWorkflowRoot pins the rule as kind AND marker. The marker
// alone is not enough: retry and ralph attempt roots carry it with
// gc.kind=task and are real, claimable work.
func TestIsExpandedWorkflowRoot(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{
			name: "workflow root with marker",
			meta: map[string]string{KindMetadataKey: KindWorkflow, WorkflowExpandedMetadataKey: "true"},
			want: true,
		},
		{
			name: "root-only workflow root without marker",
			meta: map[string]string{KindMetadataKey: KindWorkflow},
			want: false,
		},
		{
			name: "marked attempt root with kind task",
			meta: map[string]string{KindMetadataKey: KindTask, WorkflowExpandedMetadataKey: "true"},
			want: false,
		},
		{
			name: "marked scope root",
			meta: map[string]string{KindMetadataKey: KindScope, WorkflowExpandedMetadataKey: "true"},
			want: false,
		},
		{
			name: "workflow root with marker false",
			meta: map[string]string{KindMetadataKey: KindWorkflow, WorkflowExpandedMetadataKey: "false"},
			want: false,
		},
		{
			// Values are trimmed, matching how the claim and demand call sites
			// read metadata.
			name: "workflow root with padded kind and marker",
			meta: map[string]string{KindMetadataKey: " " + KindWorkflow + " ", WorkflowExpandedMetadataKey: " true "},
			want: true,
		},
		{
			name: "nil metadata",
			meta: nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsExpandedWorkflowRoot(tt.meta); got != tt.want {
				t.Errorf("IsExpandedWorkflowRoot(%v) = %v, want %v", tt.meta, got, tt.want)
			}
		})
	}
}
