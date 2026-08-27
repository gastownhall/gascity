package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestRevisionKnownAcceptsSignedRevisions pins the sign rule every fence site
// depends on. bd revisions are signed and about half of a city's rows carry a
// negative one; judging them by sign is what left the v59 journey's row stranded
// at `active` (ga-f7v2ft.140, fail-closed) and what made the pre-wake
// incarnation commit's CAS a no-op on the same rows (ga-f7v2ft.141, fail-open).
// The values are real revisions observed live in that journey.
func TestRevisionKnownAcceptsSignedRevisions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revision int64
		want     bool
	}{
		{"positive", 8834124395982504135, true},
		{"negative", -1655629893108404930, true},
		{"negative-observed-second", -763273861394134104, true},
		{"min-int64", -1 << 63, true},
		{"max-int64", 1<<63 - 1, true},
		{"unavailable", 0, false},
	} {
		if got := beads.RevisionKnown(tc.revision); got != tc.want {
			t.Errorf("RevisionKnown(%d) [%s] = %v, want %v", tc.revision, tc.name, got, tc.want)
		}
	}
}
