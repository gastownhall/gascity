package convergence

import (
	"testing"
	"time"
)

func TestChildStats(t *testing.T) {
	const bead = "root-1"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// prefixedNonParseable has the convergence prefix but an iteration segment
	// that does not parse — it must count toward ClosedCount/CumulativeDur but
	// never win HighestClosed. This is the intentional filter drift the
	// consolidation preserves.
	prefixedNonParseable := IdempotencyKeyPrefix(bead) + "abc"

	tests := []struct {
		name string
		in   []BeadInfo
		want ChildStats
	}{
		{
			name: "empty",
			in:   nil,
			want: ChildStats{HighestClosedIter: -1, HighestOpenIter: -1},
		},
		{
			name: "unrelated keys ignored",
			in: []BeadInfo{
				{ID: "x", Status: "closed", IdempotencyKey: "unrelated-key"},
				{ID: "y", Status: "open", IdempotencyKey: "converge:other-bead:iter:1"},
			},
			want: ChildStats{HighestClosedIter: -1, HighestOpenIter: -1},
		},
		{
			name: "closed count and highest closed",
			in: []BeadInfo{
				{ID: "w1", Status: "closed", IdempotencyKey: IdempotencyKey(bead, 1)},
				{ID: "w3", Status: "closed", IdempotencyKey: IdempotencyKey(bead, 3)},
				{ID: "w2", Status: "closed", IdempotencyKey: IdempotencyKey(bead, 2)},
				{ID: "wo", Status: "in_progress", IdempotencyKey: IdempotencyKey(bead, 4)},
			},
			want: ChildStats{
				ClosedCount:        3,
				HighestClosed:      BeadInfo{ID: "w3", Status: "closed", IdempotencyKey: IdempotencyKey(bead, 3)},
				HighestClosedIter:  3,
				HighestClosedFound: true,
				HighestOpen:        BeadInfo{ID: "wo", Status: "in_progress", IdempotencyKey: IdempotencyKey(bead, 4)},
				HighestOpenIter:    4,
				HighestOpenFound:   true,
			},
		},
		{
			name: "closed without parseable iteration counts but does not win highest",
			in: []BeadInfo{
				{ID: "w1", Status: "closed", IdempotencyKey: IdempotencyKey(bead, 1)},
				{ID: "bad", Status: "closed", IdempotencyKey: prefixedNonParseable},
			},
			want: ChildStats{
				ClosedCount:        2,
				HighestClosed:      BeadInfo{ID: "w1", Status: "closed", IdempotencyKey: IdempotencyKey(bead, 1)},
				HighestClosedIter:  1,
				HighestClosedFound: true,
				HighestOpenIter:    -1,
			},
		},
		{
			name: "cumulative duration skips zero timestamps",
			in: []BeadInfo{
				{ID: "w1", Status: "closed", IdempotencyKey: IdempotencyKey(bead, 1), CreatedAt: base, ClosedAt: base.Add(2 * time.Second)},
				{ID: "w2", Status: "closed", IdempotencyKey: IdempotencyKey(bead, 2), CreatedAt: base}, // ClosedAt zero -> skipped
				{ID: "w3", Status: "closed", IdempotencyKey: IdempotencyKey(bead, 3), ClosedAt: base},  // CreatedAt zero -> skipped
			},
			want: ChildStats{
				ClosedCount:        3,
				CumulativeDur:      2 * time.Second,
				HighestClosed:      BeadInfo{ID: "w3", Status: "closed", IdempotencyKey: IdempotencyKey(bead, 3), ClosedAt: base},
				HighestClosedIter:  3,
				HighestClosedFound: true,
				HighestOpenIter:    -1,
			},
		},
		{
			name: "highest open picks max across open and in_progress",
			in: []BeadInfo{
				{ID: "o1", Status: "open", IdempotencyKey: IdempotencyKey(bead, 5)},
				{ID: "o2", Status: "in_progress", IdempotencyKey: IdempotencyKey(bead, 7)},
				{ID: "o3", Status: "open", IdempotencyKey: IdempotencyKey(bead, 6)},
			},
			want: ChildStats{
				HighestClosedIter: -1,
				HighestOpen:       BeadInfo{ID: "o2", Status: "in_progress", IdempotencyKey: IdempotencyKey(bead, 7)},
				HighestOpenIter:   7,
				HighestOpenFound:  true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := childStats(tt.in, bead)
			if got != tt.want {
				t.Errorf("childStats() =\n  %+v\nwant\n  %+v", got, tt.want)
			}
		})
	}
}
