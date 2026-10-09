package main

import "testing"

func TestRewriteBdReadyInternalArgs(t *testing.T) {
	const excludeType = "--exclude-type=" + sessionBeadType
	const excludeLabel = "--exclude-label=" + labelOrderTracking

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "bare ready gains both exclusions",
			args: []string{"ready"},
			want: []string{"ready", excludeType, excludeLabel},
		},
		{
			name: "ready --claim gains both exclusions",
			args: []string{"ready", "--claim", "--json"},
			want: []string{"ready", "--claim", "--json", excludeType, excludeLabel},
		},
		{
			name: "ready with a caller exclude-type keeps it and adds ours",
			args: []string{"ready", "--json", "--exclude-type=epic"},
			want: []string{"ready", "--json", "--exclude-type=epic", excludeType, excludeLabel},
		},
		{
			name: "ready with value flags separated from their values",
			args: []string{"ready", "--assignee", "worker-1", "--limit", "5"},
			want: []string{"ready", "--assignee", "worker-1", "--limit", "5", excludeType, excludeLabel},
		},
		{
			name: "global flags before the verb are skipped",
			args: []string{"--json", "ready"},
			want: []string{"--json", "ready", excludeType, excludeLabel},
		},
		{
			name: "list --ready gains both exclusions",
			args: []string{"list", "--ready", "--json"},
			want: []string{"list", "--ready", "--json", excludeType, excludeLabel},
		},
		{
			name: "explicit --type session keeps the type half",
			args: []string{"ready", "--type", "session"},
			want: []string{"ready", "--type", "session", excludeLabel},
		},
		{
			name: "explicit -t=session keeps the type half",
			args: []string{"ready", "-t=session"},
			want: []string{"ready", "-t=session", excludeLabel},
		},
		{
			name: "explicit --label order-tracking keeps the label half",
			args: []string{"ready", "--label", "order-tracking"},
			want: []string{"ready", "--label", "order-tracking", excludeType},
		},
		{
			name: "explicit --label-any naming the label keeps the label half",
			args: []string{"ready", "--label-any=exec,order-tracking"},
			want: []string{"ready", "--label-any=exec,order-tracking", excludeType},
		},
		{
			name: "caller already excludes session: not duplicated",
			args: []string{"ready", "--exclude-type", "epic,session"},
			want: []string{"ready", "--exclude-type", "epic,session", excludeLabel},
		},
		{
			name: "caller already excludes the label: not duplicated",
			args: []string{"ready", "--exclude-label=order-tracking"},
			want: []string{"ready", "--exclude-label=order-tracking", excludeType},
		},
		{
			name: "list --ready --skip-labels drops only the label half",
			args: []string{"list", "--ready", "--skip-labels"},
			want: []string{"list", "--ready", "--skip-labels", excludeType},
		},
		{
			name: "plain list is untouched",
			args: []string{"list", "--type=session"},
			want: []string{"list", "--type=session"},
		},
		{
			name: "show is untouched",
			args: []string{"show", "de-d0iq"},
			want: []string{"show", "de-d0iq"},
		},
		{
			name: "create is untouched",
			args: []string{"create", "ready", "--type=task"},
			want: []string{"create", "ready", "--type=task"},
		},
		{
			name: "unknown flag fails open",
			args: []string{"ready", "--frobnicate", "x"},
			want: []string{"ready", "--frobnicate", "x"},
		},
		{
			name: "dangling value flag fails open",
			args: []string{"ready", "--type"},
			want: []string{"ready", "--type"},
		},
		{
			name: "no verb is untouched",
			args: []string{"--json"},
			want: []string{"--json"},
		},
		{
			name: "empty argv is untouched",
			args: nil,
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := append([]string(nil), tc.args...)
			got := rewriteBdReadyInternalArgs(input)
			if !equalArgs(got, tc.want) {
				t.Fatalf("rewriteBdReadyInternalArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
			if !equalArgs(input, tc.args) {
				t.Fatalf("rewriteBdReadyInternalArgs mutated its input: got %v, want %v", input, tc.args)
			}
		})
	}
}

// TestRewriteBdReadyInternalArgsTracksWriters pins the exclusions to the
// constants the bead writers use, so a renamed session type or order-tracking
// label cannot leave the ready filter excluding a value nothing writes.
func TestRewriteBdReadyInternalArgsTracksWriters(t *testing.T) {
	got := rewriteBdReadyInternalArgs([]string{"ready"})
	if !containsArg(got, "--exclude-type="+sessionBeadType) {
		t.Fatalf("args = %v, want --exclude-type=%s (session_beads.go sessionBeadType)", got, sessionBeadType)
	}
	if !containsArg(got, "--exclude-label="+labelOrderTracking) {
		t.Fatalf("args = %v, want --exclude-label=%s (order_dispatch.go labelOrderTracking)", got, labelOrderTracking)
	}
}
