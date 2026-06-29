package runproj

import "testing"

// TestSemanticNodeIDForIterationStepRef is the regression test for the
// iteration-at-index-0 bug: a step ref that reduces to ["iteration", <int>] makes
// semanticIdFromStepRef return undefined in TS (plain semanticParts[-1]), so
// semanticNodeIdFor falls through to the bead id. The Go port previously returned
// a present-but-empty "" semantic id, corrupting grouping for that node.
func TestSemanticNodeIDForIterationStepRef(t *testing.T) {
	const root = "run-root"
	cases := []struct {
		name string
		bead runSnapshotBead
		want string
	}{
		{
			name: "iteration.N step ref falls through to bead id",
			bead: runSnapshotBead{id: "bead-x", stepRef: "iteration.5"},
			want: "bead-x",
		},
		{
			name: "iteration.N with runtime suffix still falls through to bead id",
			bead: runSnapshotBead{id: "bead-y", stepRef: "iteration.5.run.2"},
			want: "bead-y",
		},
		{
			name: "iteration.N with no bead id falls through to run-node",
			bead: runSnapshotBead{stepRef: "iteration.5"},
			want: "run-node",
		},
		{
			name: "trailing iteration keeps the preceding segment",
			bead: runSnapshotBead{id: "bead-z", stepRef: "preflight.iteration.3"},
			want: "preflight",
		},
		{
			name: "mid iteration keeps the last segment",
			bead: runSnapshotBead{id: "bead-w", stepRef: "review.iteration.3.apply"},
			want: "apply",
		},
		{
			name: "plain step ref uses the last segment",
			bead: runSnapshotBead{id: "bead-v", stepRef: "mol-adopt-pr-v2.preflight"},
			want: "preflight",
		},
		{
			name: "explicit gc.step_id wins over the ref",
			bead: runSnapshotBead{id: "bead-u", stepRef: "iteration.5", metadata: map[string]string{"gc.step_id": "real-step"}},
			want: "real-step",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := semanticNodeIDFor(tc.bead, root); got != tc.want {
				t.Errorf("semanticNodeIDFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsPositiveIntegerStr pins the JS isPositiveInteger semantics, including the
// float64 exact-representability boundary (parseInt yields a float64, so values
// beyond 2^53 only pass when they happen to be exactly representable).
func TestIsPositiveIntegerStr(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"12", true},
		{"0", false},
		{"012", false},
		{"-3", false},
		{"3.0", false},
		{"12abc", false},
		{"", false},
		{"9007199254740992", true},      // 2^53, exactly representable
		{"9007199254740993", false},     // 2^53+1, parseInt rounds → String != value
		{"9007199254740994", true},      // 2^53+2, exactly representable (even)
		{"99999999999999999999", false}, // overflows int64 → rejected (TS also rejects)
	}
	for _, tc := range cases {
		if got := isPositiveIntegerStr(tc.value); got != tc.want {
			t.Errorf("isPositiveIntegerStr(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
