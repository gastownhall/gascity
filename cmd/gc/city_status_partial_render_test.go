package main

import (
	"bytes"
	"strings"
	"testing"
)

// renderAgentsSnapshot builds a minimal snapshot carrying only the Agents
// block, which is all the two regressions below concern.
func renderAgentsSnapshot(t *testing.T, rows []cityStatusAgentRow, running, total int, partial bool) string {
	t.Helper()
	snapshot := cityStatusSnapshot{
		CityName: "testcity",
		CityPath: "/tmp/testcity",
		Agents:   rows,
		Partial:  partial,
	}
	snapshot.Summary.RunningAgents = running
	snapshot.Summary.TotalAgents = total
	var stdout bytes.Buffer
	renderCityStatusText(snapshot, newFakeDrainOps(), &stdout)
	return stdout.String()
}

// TestAgentSummaryLineDoesNotFoldUnknownIntoNotRunning covers defect 1 of
// gastownhall/gascity#4579: during partial status every non-running row
// renders "unknown  (partial status)", but the summary counted them as not
// running and printed "1/18 agents running" above eighteen rows saying
// otherwise. Non-partial output must be byte-identical to before the fix.
func TestAgentSummaryLineDoesNotFoldUnknownIntoNotRunning(t *testing.T) {
	tests := []struct {
		name    string
		running int
		total   int
		partial bool
		want    string
	}{
		{
			name:    "not partial renders the historical ratio unchanged",
			running: 1,
			total:   18,
			partial: false,
			want:    "1/18 agents running",
		},
		{
			name:    "not partial with all running unchanged",
			running: 3,
			total:   3,
			partial: false,
			want:    "3/3 agents running",
		},
		{
			name:    "partial reports unknown separately",
			running: 1,
			total:   18,
			partial: true,
			want:    "1 running, 17 unknown of 18 agents",
		},
		{
			name:    "partial with nothing unknown keeps the ratio",
			running: 3,
			total:   3,
			partial: true,
			want:    "3/3 agents running",
		},
		{
			name:    "partial with nothing running still names the unknowns",
			running: 0,
			total:   5,
			partial: true,
			want:    "0 running, 5 unknown of 5 agents",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := agentSummaryLine(tc.running, tc.total, tc.partial)
			if got != tc.want {
				t.Fatalf("agentSummaryLine(%d, %d, %v) = %q, want %q", tc.running, tc.total, tc.partial, got, tc.want)
			}
			if tc.partial && tc.total-tc.running > 0 && strings.Contains(got, "agents running") {
				t.Fatalf("summary %q counts unknown agents as not running during partial status", got)
			}
		})
	}
}

// TestAgentSummaryLineRenderedDuringPartialStatus is the end-to-end half of
// defect 1: the rendered Agents block must not carry a running/total ratio
// that contradicts the unknown rows printed directly above it.
func TestAgentSummaryLineRenderedDuringPartialStatus(t *testing.T) {
	rows := []cityStatusAgentRow{
		{Agent: StatusAgentJSON{Name: "alpha", QualifiedName: "alpha", Running: true}, SessionName: "alpha"},
		{Agent: StatusAgentJSON{Name: "bravo", QualifiedName: "bravo"}, SessionName: "bravo"},
		{Agent: StatusAgentJSON{Name: "charlie", QualifiedName: "charlie"}, SessionName: "charlie"},
	}
	out := renderAgentsSnapshot(t, rows, 1, 3, true)
	if strings.Count(out, "unknown  (partial status)") != 2 {
		t.Fatalf("stdout = %q, want two unknown rows", out)
	}
	if strings.Contains(out, "1/3 agents running") {
		t.Fatalf("stdout = %q, summary still folds unknown agents into not-running", out)
	}
	if !strings.Contains(out, "1 running, 2 unknown of 3 agents") {
		t.Fatalf("stdout = %q, want the unknown count reported separately", out)
	}
}

// TestAgentNameColumnKeepsGutter covers defect 2 of
// gastownhall/gascity#4579: a rig-qualified name at or past the fixed pad
// width ran straight into the status token
// ("tar-valon/core.control-dispatcherunknown  (partial status)").
func TestAgentNameColumnKeepsGutter(t *testing.T) {
	const longName = "tar-valon/core.control-dispatcher" // 33 chars, past the 24-wide pad

	tests := []struct {
		name     string
		expanded bool
	}{
		{name: "flat row", expanded: false},
		{name: "expanded row", expanded: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := []cityStatusAgentRow{
				{
					Agent:       StatusAgentJSON{Name: "core.control-dispatcher", QualifiedName: longName},
					SessionName: "core.control-dispatcher",
					Expanded:    tc.expanded,
				},
			}
			out := renderAgentsSnapshot(t, rows, 0, 1, true)
			if strings.Contains(out, "dispatcherunknown") {
				t.Fatalf("stdout = %q, long agent name overflows into the status column", out)
			}
			if !strings.Contains(out, longName+"  unknown  (partial status)") {
				t.Fatalf("stdout = %q, want a two-space gutter after the over-long agent name", out)
			}
		})
	}
}

// TestPadStatusNameMatchesFixedPadBelowGutter pins the no-change half of the
// defect-2 fix: names short enough to keep the minimum gutter must pad exactly
// as the old "%-*s" verb did.
func TestPadStatusNameMatchesFixedPadBelowGutter(t *testing.T) {
	tests := []struct {
		name  string
		width int
		want  string
	}{
		{name: "worker", width: 24, want: "worker" + strings.Repeat(" ", 18)},
		{name: strings.Repeat("a", 22), width: 24, want: strings.Repeat("a", 22) + "  "},
		{name: strings.Repeat("a", 23), width: 24, want: strings.Repeat("a", 23) + "  "},
		{name: strings.Repeat("a", 24), width: 24, want: strings.Repeat("a", 24) + "  "},
		{name: strings.Repeat("a", 40), width: 24, want: strings.Repeat("a", 40) + "  "},
	}
	for _, tc := range tests {
		got := padStatusName(tc.name, tc.width)
		if got != tc.want {
			t.Fatalf("padStatusName(%q, %d) = %q, want %q", tc.name, tc.width, got, tc.want)
		}
		if !strings.HasSuffix(got, strings.Repeat(" ", statusNameColumnGutter)) {
			t.Fatalf("padStatusName(%q, %d) = %q, want at least a %d-space gutter", tc.name, tc.width, got, statusNameColumnGutter)
		}
	}
}
