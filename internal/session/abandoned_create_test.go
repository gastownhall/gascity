package session

import (
	"testing"
	"time"
)

var abandonedCreateNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// createInfoOpts describes one in-flight-create row. Ages are measured back
// from abandonedCreateNow; a zero age leaves the field unset.
type createInfoOpts struct {
	state     string
	claim     bool
	startedAt time.Duration // pending_create_started_at age; 0 = unset
	createdAt time.Duration // CreatedAt age; 0 = zero time
	lastWoke  time.Duration // last_woke_at age; 0 = empty
}

func createInfo(o createInfoOpts) Info {
	info := Info{
		ID:                 "gc-1",
		Template:           "worker",
		MetadataState:      o.state,
		PendingCreateClaim: o.claim,
	}
	if o.startedAt != 0 {
		info.PendingCreateStartedAt = abandonedCreateNow.Add(-o.startedAt).Format(time.RFC3339)
	}
	if o.createdAt != 0 {
		info.CreatedAt = abandonedCreateNow.Add(-o.createdAt)
	}
	if o.lastWoke != 0 {
		info.LastWokeAt = abandonedCreateNow.Add(-o.lastWoke).Format(time.RFC3339)
	}
	return info
}

const longAgo = 62 * time.Hour

func TestAbandonedCreateConstants(t *testing.T) {
	if StaleCreatingTimeout != time.Minute {
		t.Errorf("StaleCreatingTimeout = %s, want 1m", StaleCreatingTimeout)
	}
	if NeverStartedCreateLease != 10*time.Minute {
		t.Errorf("NeverStartedCreateLease = %s, want 10m", NeverStartedCreateLease)
	}
	if StaleKeyDetectDelay != 2*time.Second {
		t.Errorf("StaleKeyDetectDelay = %s, want 2s", StaleKeyDetectDelay)
	}
}

func TestDetectAbandonedCreate(t *testing.T) {
	const st = time.Minute // startupTimeout; in-flight window = 1m + 2s + 5s = 67s
	tests := []struct {
		name string
		info Info
		st   time.Duration
		want bool
	}{
		// Rows A-F from the architect ruling (state=creating, 62h old).
		{"A no claim, never started", createInfo(createInfoOpts{state: "creating", startedAt: longAgo, createdAt: longAgo}), st, true},
		{"B claim, never started", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, createdAt: longAgo}), st, true},
		{"C claim, started then wedged", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, createdAt: longAgo, lastWoke: longAgo}), st, true},
		{"E no claim, started", createInfo(createInfoOpts{state: "creating", startedAt: longAgo, createdAt: longAgo, lastWoke: longAgo}), st, true},
		{"F claim, start in flight", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, createdAt: longAgo, lastWoke: 20 * time.Second}), st, false},
		{"C as start-pending", createInfo(createInfoOpts{state: "start-pending", claim: true, startedAt: longAgo, lastWoke: longAgo}), st, true},
		{"raw state is trimmed", createInfo(createInfoOpts{state: " creating ", startedAt: longAgo}), st, true},

		// Not in flight at all.
		{"active", createInfo(createInfoOpts{state: "active", startedAt: longAgo}), st, false},
		{"asleep", createInfo(createInfoOpts{state: "asleep", startedAt: longAgo}), st, false},
		{"suspended", createInfo(createInfoOpts{state: "suspended", startedAt: longAgo}), st, false},
		{"empty state", createInfo(createInfoOpts{startedAt: longAgo}), st, false},
		{"closed", func() Info {
			i := createInfo(createInfoOpts{state: "creating", startedAt: longAgo})
			i.Closed = true
			return i
		}(), st, false},

		// Staleness boundary: stale AT anchor+1m (inclusive).
		{"stale exactly at 1m", createInfo(createInfoOpts{state: "creating", startedAt: time.Minute}), st, true},
		{"not stale 1s before 1m", createInfo(createInfoOpts{state: "creating", startedAt: time.Minute - time.Second}), st, false},
		{"stale 1s after 1m", createInfo(createInfoOpts{state: "creating", startedAt: time.Minute + time.Second}), st, true},

		// Never-started lease boundary: still leased AT anchor+10m (exclusive).
		{"never-started leased 1s before 10m", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: 10*time.Minute - time.Second}), st, false},
		{"never-started still leased exactly at 10m", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: 10 * time.Minute}), st, false},
		{"never-started abandoned 1s after 10m", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: 10*time.Minute + time.Second}), st, true},

		// In-flight window: now.Before(last_woke_at + 67s).
		{"in flight 1s before window end", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, lastWoke: 67*time.Second - time.Second}), st, false},
		{"not in flight exactly at window end", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, lastWoke: 67 * time.Second}), st, true},
		{"not in flight 1s after window end", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, lastWoke: 67*time.Second + time.Second}), st, true},
		{"startupTimeout 30s shrinks window to 37s", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, lastWoke: 40 * time.Second}), 30 * time.Second, true},
		{"startupTimeout 30s still in flight at 36s", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, lastWoke: 36 * time.Second}), 30 * time.Second, false},
		{"startupTimeout zero defaults to 1m (in flight at 66s)", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, lastWoke: 66 * time.Second}), 0, false},
		{"startupTimeout negative defaults to 1m (in flight at 66s)", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, lastWoke: 66 * time.Second}), -time.Hour, false},
		{"startupTimeout zero: abandoned past 67s", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: longAgo, lastWoke: 68 * time.Second}), 0, true},

		// Row C boundary: started, not in flight, leased only while the attempt is not stale.
		{"row C leased 1s before attempt is 1m old", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: time.Minute - time.Second, lastWoke: 2 * time.Minute}), st, false},
		{"row C abandoned exactly at attempt 1m old", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: time.Minute, lastWoke: 2 * time.Minute}), st, true},
		{"row C 5m old", createInfo(createInfoOpts{state: "creating", claim: true, startedAt: 5 * time.Minute, lastWoke: 5 * time.Minute}), st, true},

		// Unparseable last_woke_at is non-empty: never in flight, lease follows the stale rule.
		{"claim, corrupt last_woke_at, attempt fresh", func() Info {
			i := createInfo(createInfoOpts{state: "creating", claim: true, startedAt: 30 * time.Second})
			i.LastWokeAt = "not-a-time"
			return i
		}(), st, false},
		{"claim, corrupt last_woke_at, attempt stale", func() Info {
			i := createInfo(createInfoOpts{state: "creating", claim: true, startedAt: 5 * time.Minute})
			i.LastWokeAt = "not-a-time"
			return i
		}(), st, true},

		// Anchor selection.
		{"corrupt started_at falls back to CreatedAt (stale)", func() Info {
			i := createInfo(createInfoOpts{state: "creating", createdAt: longAgo})
			i.PendingCreateStartedAt = "garbage"
			return i
		}(), st, true},
		{"corrupt started_at falls back to CreatedAt (fresh)", func() Info {
			i := createInfo(createInfoOpts{state: "creating", createdAt: 10 * time.Second})
			i.PendingCreateStartedAt = "garbage"
			return i
		}(), st, false},
		{"started_at wins over old CreatedAt", createInfo(createInfoOpts{state: "creating", startedAt: 10 * time.Second, createdAt: longAgo}), st, false},
		{"unanchored (no started_at, zero CreatedAt) is stale", createInfo(createInfoOpts{state: "creating"}), st, true},
		{"unanchored claim never-started is abandoned", createInfo(createInfoOpts{state: "creating", claim: true}), st, true},

		// Skew: now before the anchor is never stale.
		{"anchor in the future", func() Info {
			i := createInfo(createInfoOpts{state: "creating"})
			i.PendingCreateStartedAt = abandonedCreateNow.Add(time.Hour).Format(time.RFC3339)
			return i
		}(), st, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := DetectAbandonedCreate(tt.info, abandonedCreateNow, tt.st)
			if got != tt.want {
				t.Fatalf("DetectAbandonedCreate() abandoned = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetectAbandonedCreateFields(t *testing.T) {
	anchor := abandonedCreateNow.Add(-5 * time.Minute)
	info := createInfo(createInfoOpts{state: " start-pending ", startedAt: 5 * time.Minute})
	info.ID = "gc-42"
	info.Template = "  rig/worker "

	got, ok := DetectAbandonedCreate(info, abandonedCreateNow, time.Minute)
	if !ok {
		t.Fatal("DetectAbandonedCreate() = not abandoned, want abandoned")
	}
	if got.ID != "gc-42" {
		t.Errorf("ID = %q, want gc-42", got.ID)
	}
	if got.Template != "rig/worker" {
		t.Errorf("Template = %q, want trimmed rig/worker", got.Template)
	}
	if got.State != StateStartPending {
		t.Errorf("State = %q, want %q", got.State, StateStartPending)
	}
	if !got.Since.Equal(anchor) {
		t.Errorf("Since = %s, want %s", got.Since, anchor)
	}
}

func TestDetectAbandonedCreateUnanchoredSinceIsZero(t *testing.T) {
	got, ok := DetectAbandonedCreate(createInfo(createInfoOpts{state: "creating"}), abandonedCreateNow, time.Minute)
	if !ok {
		t.Fatal("unanchored creating row must be abandoned")
	}
	if !got.Since.IsZero() {
		t.Errorf("Since = %s, want zero for an unanchored row", got.Since)
	}
}

func TestDetectAbandonedCreateEmptyTemplateAllowed(t *testing.T) {
	info := createInfo(createInfoOpts{state: "creating", startedAt: time.Hour})
	info.Template = ""
	got, ok := DetectAbandonedCreate(info, abandonedCreateNow, time.Minute)
	if !ok {
		t.Fatal("a damaged bead with no template must still be reported")
	}
	if got.Template != "" {
		t.Errorf("Template = %q, want empty", got.Template)
	}
}

// A zero now is judged against the wall clock, never "cannot assess, so not
// abandoned": a detector must fail toward reporting.
func TestDetectAbandonedCreateZeroNowUsesWallClock(t *testing.T) {
	wall := time.Now()
	old := Info{ID: "old", MetadataState: "creating", PendingCreateStartedAt: wall.Add(-time.Hour).UTC().Format(time.RFC3339)}
	if _, ok := DetectAbandonedCreate(old, time.Time{}, time.Minute); !ok {
		t.Error("zero now: a create started an hour ago must be abandoned")
	}
	fresh := Info{ID: "fresh", MetadataState: "creating", PendingCreateStartedAt: wall.Add(-time.Second).UTC().Format(time.RFC3339)}
	if _, ok := DetectAbandonedCreate(fresh, time.Time{}, time.Minute); ok {
		t.Error("zero now: a create started a second ago must not be abandoned")
	}
	wedged := Info{
		ID: "wedged", MetadataState: "creating", PendingCreateClaim: true,
		PendingCreateStartedAt: wall.Add(-time.Hour).UTC().Format(time.RFC3339),
		LastWokeAt:             wall.Add(-time.Hour).UTC().Format(time.RFC3339),
	}
	if _, ok := DetectAbandonedCreate(wedged, time.Time{}, time.Minute); !ok {
		t.Error("zero now: row C must be abandoned (no nil-clock mode)")
	}
}

func TestCreateAttemptAnchor(t *testing.T) {
	started := abandonedCreateNow.Add(-3 * time.Minute)
	created := abandonedCreateNow.Add(-time.Hour)
	tests := []struct {
		name string
		info Info
		want time.Time
	}{
		{"started_at preferred", Info{PendingCreateStartedAt: started.Format(time.RFC3339), CreatedAt: created}, started},
		{"started_at padded with spaces", Info{PendingCreateStartedAt: "  " + started.Format(time.RFC3339) + " ", CreatedAt: created}, started},
		{"falls back to CreatedAt when absent", Info{CreatedAt: created}, created},
		{"falls back to CreatedAt when unparseable", Info{PendingCreateStartedAt: "nope", CreatedAt: created}, created},
		{"zero when neither", Info{}, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CreateAttemptAnchor(tt.info); !got.Equal(tt.want) {
				t.Fatalf("CreateAttemptAnchor() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCreateAttemptStale(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want bool
	}{
		{"exactly 1m is stale", createInfo(createInfoOpts{startedAt: time.Minute}), true},
		{"1s short of 1m is fresh", createInfo(createInfoOpts{startedAt: time.Minute - time.Second}), false},
		{"CreatedAt fallback stale", createInfo(createInfoOpts{createdAt: time.Hour}), true},
		{"zero anchor is stale", Info{}, true},
		{"future anchor is not stale", Info{CreatedAt: abandonedCreateNow.Add(time.Hour)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CreateAttemptStale(tt.info, abandonedCreateNow); got != tt.want {
				t.Fatalf("CreateAttemptStale() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetectAbandonedCreatesOrdering(t *testing.T) {
	mk := func(id string, age time.Duration) Info {
		i := createInfo(createInfoOpts{state: "creating", startedAt: age})
		i.ID = id
		return i
	}
	closed := mk("gc-closed", 9*time.Hour)
	closed.Closed = true
	unanchored := Info{ID: "gc-unanchored", MetadataState: "creating"}
	infos := []Info{
		mk("gc-c", 2*time.Hour),
		mk("gc-fresh", 5*time.Second), // not abandoned
		closed,                        // closed: skipped
		mk("gc-b", 3*time.Hour),
		mk("gc-a", 3*time.Hour), // ties with gc-b: ID breaks it
		{ID: "gc-active", MetadataState: "active", PendingCreateStartedAt: abandonedCreateNow.Add(-time.Hour).Format(time.RFC3339)},
		unanchored, // zero Since sorts first (oldest)
	}
	got := DetectAbandonedCreates(infos, abandonedCreateNow, time.Minute)
	var ids []string
	for _, g := range got {
		ids = append(ids, g.ID)
	}
	want := []string{"gc-unanchored", "gc-a", "gc-b", "gc-c"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}

func TestDetectAbandonedCreatesDoesNotMutateInput(t *testing.T) {
	a := createInfo(createInfoOpts{state: "creating", startedAt: time.Hour})
	a.ID = "gc-a"
	b := createInfo(createInfoOpts{state: "creating", startedAt: 2 * time.Hour})
	b.ID = "gc-b"
	infos := []Info{a, b}
	_ = DetectAbandonedCreates(infos, abandonedCreateNow, time.Minute)
	if infos[0].ID != "gc-a" || infos[1].ID != "gc-b" {
		t.Fatalf("input reordered: %s, %s", infos[0].ID, infos[1].ID)
	}
}

func TestDetectAbandonedCreatesEmpty(t *testing.T) {
	if got := DetectAbandonedCreates(nil, abandonedCreateNow, time.Minute); len(got) != 0 {
		t.Fatalf("nil input: got %v, want none", got)
	}
	healthy := []Info{createInfo(createInfoOpts{state: "active", startedAt: time.Hour})}
	if got := DetectAbandonedCreates(healthy, abandonedCreateNow, time.Minute); len(got) != 0 {
		t.Fatalf("healthy input: got %v, want none", got)
	}
}
