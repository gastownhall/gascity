package tokenrender

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/connect"
	"github.com/gastownhall/gascity/internal/session"
)

func sessionBead(id, status string, meta map[string]string) beads.Bead {
	return beads.Bead{ID: id, Status: status, Metadata: meta}
}

// TestEligibleTokenRowCoversEveryBaseState pins DES-CN D6 over every
// session.BaseState constant: each fixture is first checked to project to
// its state, so a change to the projection fails here rather than silently
// moving a state across the eligibility line.
func TestEligibleTokenRowCoversEveryBaseState(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		meta     map[string]string
		base     session.BaseState
		eligible bool
	}{
		{"new", "open", map[string]string{}, session.BaseStateNone, true},
		{"creating", "open", map[string]string{"state": "creating"}, session.BaseStateCreating, true},
		{"start-pending", "open", map[string]string{"state": "start-pending"}, session.BaseStateStartPending, true},
		{"active", "open", map[string]string{"state": "active"}, session.BaseStateActive, true},
		{"awake", "open", map[string]string{"state": "awake"}, session.BaseStateActive, true},
		{"draining", "open", map[string]string{"state": "draining"}, session.BaseStateDraining, true},
		{"closing", "open", map[string]string{"state": "closing"}, session.BaseStateClosing, true},
		{"orphaned", "open", map[string]string{"state": "orphaned"}, session.BaseStateOrphaned, true},
		{"asleep", "open", map[string]string{"state": "asleep"}, session.BaseStateAsleep, false},
		{"asleep drained through sleep_reason", "open", map[string]string{"state": "asleep", "sleep_reason": "drained"}, session.BaseStateDrained, false},
		{"drained", "open", map[string]string{"state": "drained"}, session.BaseStateDrained, false},
		{"suspended", "open", map[string]string{"state": "suspended"}, session.BaseStateSuspended, false},
		{"archived", "open", map[string]string{"state": "archived"}, session.BaseStateArchived, false},
		{"failed-create", "open", map[string]string{"state": "failed-create"}, session.BaseStateFailedCreate, false},
		{"stopped", "open", map[string]string{"state": "stopped"}, session.BaseStateStopped, false},
		{"quarantined", "open", map[string]string{"state": "quarantined"}, session.BaseStateQuarantined, false},
		{"closed through the state", "open", map[string]string{"state": "closed"}, session.BaseStateClosed, false},
		{"closed through the status", "closed", map[string]string{"state": "active"}, session.BaseStateClosed, false},
		{"unknown state", "open", map[string]string{"state": "some-future-state"}, session.BaseState("some-future-state"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := session.ProjectLifecycle(session.LifecycleInputFromMetadata(tc.status, tc.meta)).BaseState
			if got != tc.base {
				t.Fatalf("fixture projects to %q, want %q", got, tc.base)
			}
			row, ok := EligibleTokenRow(sessionBead("mc-abc12", tc.status, tc.meta))
			if ok != tc.eligible {
				t.Fatalf("eligible = %v, want %v", ok, tc.eligible)
			}
			if ok && row != (connect.TokenRow{SessionID: "mc-abc12", Generation: 1}) {
				t.Fatalf("row = %+v", row)
			}
		})
	}
}

// TestEligibleTokenRowNormalizesGenerationAsTheStartPath pins the rule
// session.Manager applies when it starts a runtime (internal/session/chat.go:
// strconv.Atoi, then session.DefaultGeneration on an error or a result at
// most 0).
func TestEligibleTokenRowNormalizesGenerationAsTheStartPath(t *testing.T) {
	cases := []struct {
		raw     *string
		want    int
		comment string
	}{
		{nil, session.DefaultGeneration, "missing"},
		{ptr("0"), session.DefaultGeneration, "zero"},
		{ptr("-3"), session.DefaultGeneration, "negative"},
		{ptr("abc"), session.DefaultGeneration, "unparsable"},
		{ptr(" 7"), session.DefaultGeneration, "untrimmed (Atoi refuses it)"},
		{ptr("99999999999999999999"), session.DefaultGeneration, "overflowing (Atoi returns the maximum int with its error)"},
		{ptr("7"), 7, "positive"},
		{ptr("130"), 130, "multi-digit"},
	}
	for _, tc := range cases {
		meta := map[string]string{"state": "active"}
		if tc.raw != nil {
			meta["generation"] = *tc.raw
		}
		row, ok := EligibleTokenRow(sessionBead("s-1", "open", meta))
		if !ok || row.Generation != tc.want {
			t.Errorf("%s: row = %+v, %v; want generation %d", tc.comment, row, ok, tc.want)
		}
	}
}

// TestEligibleTokenRowPassesInvalidSessionIDsThrough pins that eligibility is
// D6's lifecycle rule alone: a row whose id no bearer can carry reaches the
// renderer, which drops and counts it (Report.DroppedRows), so gc doctor sees
// it instead of a source dropping it silently.
func TestEligibleTokenRowPassesInvalidSessionIDsThrough(t *testing.T) {
	for _, id := range []string{"", "-lead", "has space", "a/b", "é"} {
		row, ok := EligibleTokenRow(sessionBead(id, "open", map[string]string{"state": "active"}))
		if !ok || row.SessionID != id || row.Generation != 1 {
			t.Errorf("id %q = %+v, %v; want it passed through", id, row, ok)
		}
		if _, ok := EligibleTokenRow(sessionBead(id, "closed", nil)); ok {
			t.Errorf("closed id %q eligible", id)
		}
	}
}

func ptr(s string) *string { return &s }
