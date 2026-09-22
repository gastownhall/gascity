package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// bdVerbLog records the bd commands a fake beads.CommandRunner receives, so a
// test can assert on the VERIFICATION MECHANISM and not just the resulting
// verdict. A List-based scan and a single-bead Get can both settle on the
// same (false, nil) verdict for entirely different reasons — only the
// mechanism assertion (which bd verb ran) tells them apart.
type bdVerbLog struct {
	calls []string
}

func (l *bdVerbLog) record(name string, args ...string) {
	l.calls = append(l.calls, name+" "+strings.Join(args, " "))
}

func (l *bdVerbLog) sawShowFor(id string) bool {
	want := "bd show --json " + id
	for _, c := range l.calls {
		if c == want {
			return true
		}
	}
	return false
}

// newLiveGetFakeRunner answers exactly the bd verbs that
// liveWorkAssignmentAssigneeMatches's List-based (current) and Get-based
// (fixed) implementations can issue between them:
//
//   - "bd show --json <id>"  -- exact match, the Get path the fix must use.
//     Responds with rawJSON, the live bead's raw (unmapped) bd state.
//   - "bd list ..."          -- prefix match. Canned empty, mirroring bd's
//     own native status filter: `bd list --status=open` excludes a bead
//     whose RAW status is "blocked" or "deferred" even though Gas City's
//     mapped Status for both is also "open" (mapBdStatus collapses every
//     non-in_progress/closed raw status onto "open"). The exclusion is
//     real bd behavior, not a test simplification.
//   - "bd query ..."         -- prefix match, the ephemeral/wisp sub-call
//     TierMode:TierBoth also issues. Canned empty (no wisp matches).
//
// Any other command fails the test immediately rather than returning a zero
// value, so a change to bd's argv construction surfaces as a clear failure
// instead of a silently-wrong canned response.
func newLiveGetFakeRunner(t *testing.T, log *bdVerbLog, id, rawJSON string) beads.CommandRunner {
	t.Helper()
	showCmd := "bd show --json " + id
	return func(_, name string, args ...string) ([]byte, error) {
		log.record(name, args...)
		got := name + " " + strings.Join(args, " ")
		switch {
		case got == showCmd:
			return []byte(rawJSON), nil
		case strings.HasPrefix(got, "bd list "):
			return []byte(`[]`), nil
		case strings.HasPrefix(got, "bd query "):
			return []byte(`[]`), nil
		default:
			t.Fatalf("unexpected bd command: %q", got)
			return nil, nil
		}
	}
}

// TestLiveWorkAssignmentAssigneeMatches_RawStatusStillOpenButNativelyGated
// covers the architect-ruled protection for ga-8noaen: a WORK bead can carry
// a raw bd status of "blocked" or "deferred" while Gas City's mapped Status
// is still "open" (mapBdStatus/normalizedBdReadState collapse every raw
// status other than "in_progress"/"closed" onto "open"). A caller's
// OpenAssignedTo snapshot legitimately records expectedStatus=="open" for
// such a bead (OpenAssignedTo's own doc comment: "status selects the bead
// status (\"open\" / \"in_progress\")"), and releaseWorkAssignmentIfCurrent's
// tier-1 fast path only ever handles "in_progress", so an "open" snapshot
// always reaches this function as tier 2.
//
// wb.Status != expectedStatus does NOT catch this case, because both sides
// are "open". The native-blocked/deferred check is the only guard against
// treating a gated bead as though it were still the caller's plain open
// snapshot — and only a LIVE SINGLE-BEAD READ can see the gate: `bd list
// --status=open` excludes the bead by bd's own native filter (its raw
// status is not literally "open"), so a List-based scan reports the bead
// "not found" here too, by coincidence rather than by detecting the gate.
// Verification must call Get.
func TestLiveWorkAssignmentAssigneeMatches_RawStatusStillOpenButNativelyGated(t *testing.T) {
	const (
		id       = "gc-native-gate-1"
		assignee = "dead-session"
	)
	tests := []struct {
		name    string
		rawJSON string
	}{
		{
			name:    "natively blocked",
			rawJSON: `[{"id":"` + id + `","title":"t","status":"blocked","assignee":"` + assignee + `"}]`,
		},
		{
			name:    "indefinitely deferred",
			rawJSON: `[{"id":"` + id + `","title":"t","status":"deferred","assignee":"` + assignee + `"}]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := &bdVerbLog{}
			runner := newLiveGetFakeRunner(t, log, id, tc.rawJSON)
			store := beads.NewBdStore("/city", runner)

			matches, err := liveWorkAssignmentAssigneeMatches(store, id, "open", assignee)
			if err != nil {
				t.Fatalf("liveWorkAssignmentAssigneeMatches: %v", err)
			}
			if matches {
				t.Errorf("matches = true, want false — a natively gated bead must never be treated as a safe match for the caller's plain open snapshot")
			}
			if !log.sawShowFor(id) {
				t.Errorf("verification never issued `bd show --json %s` (calls: %v) — it must read the bead LIVE by id to see the native gate; a List-based scan reaches the same (false, nil) verdict only because bd's own `--status=open` filter happens to also exclude this bead, not because it detected the gate", id, log.calls)
			}
		})
	}
}
