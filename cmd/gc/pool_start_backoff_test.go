package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

var backoffTestNow = time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)

// TestStartFailureBackoff pins the schedule in one place: 10s doubling per
// consecutive failure, capped at 5m, and no backoff before the first failure.
func TestStartFailureBackoff(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{-1, 0},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 80 * time.Second},
		{5, 160 * time.Second},
		{6, 5 * time.Minute},
		{7, 5 * time.Minute},
		{40, 5 * time.Minute},
		{1000, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := startFailureBackoff(tc.failures); got != tc.want {
			t.Errorf("startFailureBackoff(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

func TestReadWorkStartFailureStateTreatsEmptyAndGarbageAsAbsent(t *testing.T) {
	state := readWorkStartFailureState(map[string]string{
		beadmeta.StartFailuresMetadataKey:     "",
		beadmeta.StartBackoffUntilMetadataKey: "not-a-time",
		beadmeta.ParkedAtMetadataKey:          "  ",
		beadmeta.ParkFailuresMetadataKey:      "x",
	})
	if state.Failures != 0 || !state.BackoffUntil.IsZero() || state.Parked() || state.ParkFailures != 0 {
		t.Fatalf("empty/garbage metadata must read as absent, got %+v", state)
	}
	state = readWorkStartFailureState(map[string]string{
		beadmeta.StartFailuresMetadataKey:     "3",
		beadmeta.StartFailedAtMetadataKey:     "2026-09-11T02:41:00Z",
		beadmeta.StartFailureMetadataKey:      "pre_start[0]: exit status 1",
		beadmeta.StartBackoffUntilMetadataKey: "2026-09-11T02:41:40Z",
		beadmeta.ParkedAtMetadataKey:          "2026-09-11T02:50:00Z",
		beadmeta.ParkReasonMetadataKey:        "boom",
		beadmeta.ParkFailuresMetadataKey:      "5",
		beadmeta.ParkMailedAtMetadataKey:      "2026-09-11T02:50:01Z",
	})
	if state.Failures != 3 || state.Failure != "pre_start[0]: exit status 1" || !state.Parked() || state.ParkFailures != 5 || state.ParkMailedAt.IsZero() {
		t.Fatalf("populated metadata must round-trip, got %+v", state)
	}
	if state.FailedAt.Format(time.RFC3339) != "2026-09-11T02:41:00Z" || state.BackoffUntil.Format(time.RFC3339) != "2026-09-11T02:41:40Z" {
		t.Fatalf("timestamps must parse, got %+v", state)
	}
}

// TestWorkStartFailurePatchBacksOffThenParks walks one bead through five
// consecutive failures under the default limit: failures 1..4 back off on the
// doubling schedule, failure 5 parks (the count folds into gc.park_failures,
// the backoff clock clears, the park is unmailed until the mail lands).
func TestWorkStartFailurePatchBacksOffThenParks(t *testing.T) {
	meta := map[string]string{}
	failure := "running pre_start: pre_start[0]: exit status 1; stderr: worker-worktree: ERROR several branches name bead gp-fh5f"
	wantBackoff := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second}
	for i, want := range wantBackoff {
		now := backoffTestNow.Add(time.Duration(i) * time.Minute)
		patch, parked := workStartFailurePatch(readWorkStartFailureState(meta), now, failure, 5)
		if parked {
			t.Fatalf("failure %d: parked before the limit", i+1)
		}
		applyMetadataPatch(meta, patch)
		state := readWorkStartFailureState(meta)
		if state.Failures != i+1 {
			t.Fatalf("failure %d: count = %d", i+1, state.Failures)
		}
		if !state.FailedAt.Equal(now) {
			t.Fatalf("failure %d: failed_at = %v, want %v", i+1, state.FailedAt, now)
		}
		if got := state.BackoffUntil.Sub(now); got != want {
			t.Fatalf("failure %d: backoff = %v, want %v", i+1, got, want)
		}
		if state.Failure != failure {
			t.Fatalf("failure %d: recorded failure = %q", i+1, state.Failure)
		}
		if state.Parked() {
			t.Fatalf("failure %d: parked", i+1)
		}
		if deferred, reason, _ := workStartDeferral(meta, now); !deferred || reason != workStartDeferralBackoff {
			t.Fatalf("failure %d: not deferred right after the failure: %v %q", i+1, deferred, reason)
		}
		if deferred, _, _ := workStartDeferral(meta, now.Add(want)); deferred {
			t.Fatalf("failure %d: still deferred once the backoff elapsed", i+1)
		}
	}
	now := backoffTestNow.Add(10 * time.Minute)
	patch, parked := workStartFailurePatch(readWorkStartFailureState(meta), now, failure, 5)
	if !parked {
		t.Fatal("failure 5 must park under limit 5")
	}
	applyMetadataPatch(meta, patch)
	state := readWorkStartFailureState(meta)
	if !state.Parked() || !state.ParkedAt.Equal(now) {
		t.Fatalf("park not recorded: %+v", state)
	}
	if state.ParkFailures != 5 {
		t.Fatalf("park_failures = %d, want 5", state.ParkFailures)
	}
	if state.ParkReason != failure {
		t.Fatalf("park_reason = %q", state.ParkReason)
	}
	if state.Failures != 0 || meta[beadmeta.StartFailuresMetadataKey] != "" {
		t.Fatalf("the count must fold into the park so the documented three-key unpark starts clean: %+v", state)
	}
	if !state.BackoffUntil.IsZero() || meta[beadmeta.StartBackoffUntilMetadataKey] != "" {
		t.Fatalf("a parked bead is deferred by the park, not the clock: %+v", state)
	}
	if !state.ParkMailedAt.IsZero() {
		t.Fatalf("a fresh park is unmailed: %+v", state)
	}
	if deferred, reason, _ := workStartDeferral(meta, now.Add(24*time.Hour)); !deferred || reason != workStartDeferralParked {
		t.Fatalf("a parked bead is deferred forever: %v %q", deferred, reason)
	}
	// The documented unpark: unset the three park keys. cmd/gc clears by empty
	// value (see clearedSessionAffinityMetadata), so the bd --unset-metadata
	// deletion and an empty value must read the same.
	for _, key := range []string{beadmeta.ParkReasonMetadataKey, beadmeta.ParkedAtMetadataKey, beadmeta.ParkFailuresMetadataKey} {
		meta[key] = ""
	}
	if deferred, _, _ := workStartDeferral(meta, now); deferred {
		t.Fatal("unparked bead must not be deferred")
	}
	// After the unpark the next failure starts the count over: one failure is a
	// backoff, not an instant re-park.
	patch, parked = workStartFailurePatch(readWorkStartFailureState(meta), now, failure, 5)
	if parked {
		t.Fatal("first failure after an unpark must back off, not re-park")
	}
	applyMetadataPatch(meta, patch)
	if state := readWorkStartFailureState(meta); state.Failures != 1 {
		t.Fatalf("count after unpark = %d, want 1", state.Failures)
	}
}

func TestWorkStartFailurePatchLimitZeroNeverParks(t *testing.T) {
	meta := map[string]string{}
	for i := 0; i < 50; i++ {
		patch, parked := workStartFailurePatch(readWorkStartFailureState(meta), backoffTestNow, "boom", 0)
		if parked {
			t.Fatalf("limit 0 parked on failure %d", i+1)
		}
		applyMetadataPatch(meta, patch)
	}
	state := readWorkStartFailureState(meta)
	if state.Failures != 50 || state.BackoffUntil.Sub(backoffTestNow) != 5*time.Minute {
		t.Fatalf("limit 0 must keep counting and capping the backoff: %+v", state)
	}
}

func TestWorkStartFailurePatchAlreadyParkedStaysParkedWithoutASecondPark(t *testing.T) {
	meta := map[string]string{}
	first, _ := workStartFailurePatch(readWorkStartFailureState(meta), backoffTestNow, "boom", 1)
	applyMetadataPatch(meta, first)
	meta[beadmeta.ParkMailedAtMetadataKey] = backoffTestNow.Format(time.RFC3339)
	later := backoffTestNow.Add(time.Hour)
	patch, parked := workStartFailurePatch(readWorkStartFailureState(meta), later, "boom again", 1)
	if parked {
		t.Fatal("a start that failed on an already-parked bead is not a second park (no second mail)")
	}
	applyMetadataPatch(meta, patch)
	state := readWorkStartFailureState(meta)
	if !state.ParkedAt.Equal(backoffTestNow) || state.ParkMailedAt.IsZero() {
		t.Fatalf("the original park and its mail stamp must survive: %+v", state)
	}
	if state.Failure != "boom again" || !state.FailedAt.Equal(later) {
		t.Fatalf("the last-failure record still updates: %+v", state)
	}
	if state.Failures != 0 || meta[beadmeta.StartFailuresMetadataKey] != "" {
		t.Fatalf("a failure on a parked bead does not count toward the next attempt (the three-key unpark starts from zero): %+v", state)
	}
}

func TestWorkStartFailureClearPatchTouchesOnlySetKeys(t *testing.T) {
	if got := workStartFailureClearPatch(map[string]string{}); len(got) != 0 {
		t.Fatalf("nothing set → nothing to clear, got %v", got)
	}
	meta := map[string]string{
		beadmeta.StartFailuresMetadataKey: "2",
		beadmeta.StartFailureMetadataKey:  "boom",
		beadmeta.ParkedAtMetadataKey:      "2026-09-11T02:50:00Z",
		beadmeta.RoutedToMetadataKey:      "packs/worker",
	}
	got := workStartFailureClearPatch(meta)
	want := map[string]string{
		beadmeta.StartFailuresMetadataKey: "",
		beadmeta.StartFailureMetadataKey:  "",
		beadmeta.ParkedAtMetadataKey:      "",
	}
	if len(got) != len(want) {
		t.Fatalf("clear patch = %v, want %v", got, want)
	}
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			t.Fatalf("clear patch = %v, want %v", got, want)
		}
	}
}

func TestWorkStartFailureLineIsOneBoundedLine(t *testing.T) {
	// rawStartError carries a provider's verbatim text: multi-line, trailing
	// newlines and all, which is what the one-line reducer exists for.
	err := rawStartError("running pre_start: pre_start[0]: exit status 1; stderr: line one\nworker-worktree: ERROR several branches name bead gp-fh5f\n\n")
	got := workStartFailureLine(err)
	if got != "worker-worktree: ERROR several branches name bead gp-fh5f" {
		t.Fatalf("want the last non-empty line, got %q", got)
	}
	long := errors.New(strings.Repeat("x", 2*workStartFailureLineLimit))
	if got := workStartFailureLine(long); len([]rune(got)) != workStartFailureLineLimit || !strings.HasSuffix(got, "…") {
		t.Fatalf("long line must be truncated to %d runes with an ellipsis, got %d runes", workStartFailureLineLimit, len([]rune(got)))
	}
	if got := workStartFailureLine(nil); got != "" {
		t.Fatalf("nil error → empty line, got %q", got)
	}
	if got := workStartFailureLine(rawStartError("  \n  \n")); got != "" {
		t.Fatalf("blank error → empty line, got %q", got)
	}
}

type rawStartError string

func (e rawStartError) Error() string { return string(e) }

func applyMetadataPatch(meta map[string]string, patch map[string]string) {
	for k, v := range patch {
		meta[k] = v
	}
}
