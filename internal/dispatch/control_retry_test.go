package dispatch

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// closeBlockedErr is the verbatim shape the fleet produced 296,270 times across
// four cities: bd refusing to close a bead whose only blocker is the control
// bead asking for the close.
func closeBlockedErr(target, blocker string) error {
	return fmt.Errorf("%s: completing workflow head: updating bead %q: exit status 1: cannot close blocked issue: %s is blocked by [%s]",
		target, target, target, blocker)
}

// TestEveryTransientNeedleDeclaresATier is the structural guard that keeps this
// outage class from being reintroduced one needle at a time. A needle is how an
// error message becomes "retry"; before the tier split every such answer meant
// retry FOREVER, and nothing forced the author to think about it. The zero value
// of the tier field is TierUndeclared precisely so that an entry written without
// one is invalid rather than silently unbounded.
func TestEveryTransientNeedleDeclaresATier(t *testing.T) {
	t.Parallel()

	if len(transientNeedles) == 0 {
		t.Fatal("transientNeedles is empty; the classifier table lost its contents")
	}
	seen := make(map[string]struct{}, len(transientNeedles))
	for i, entry := range transientNeedles {
		switch entry.tier {
		case TierAvailability, TierSemantic:
		default:
			t.Errorf("transientNeedles[%d] (%q) has tier %v; declare TierAvailability (the store never answered) or TierSemantic (the store answered and refused)",
				i, entry.needle, entry.tier)
		}
		if entry.needle == "" {
			t.Errorf("transientNeedles[%d] has an empty needle", i)
		}
		if _, dup := seen[entry.needle]; dup {
			t.Errorf("transientNeedles[%d] repeats needle %q", i, entry.needle)
		}
		seen[entry.needle] = struct{}{}
	}
}

// TestUndeclaredTierNeedleFailsClosed pins the runtime half of the guard: even
// if the table test were skipped, a needle with no tier must not inherit
// unbounded retry. It stops matching, so the error escalates loudly instead.
func TestUndeclaredTierNeedleFailsClosed(t *testing.T) {
	prev := transientNeedles
	transientNeedles = append([]transientNeedle{{needle: "undeclared marker"}}, prev...)
	t.Cleanup(func() { transientNeedles = prev })

	err := errors.New("some failure: undeclared marker")
	if got := ClassifyControllerError(err); got != TierNone {
		t.Fatalf("ClassifyControllerError(undeclared needle) = %v, want %v (fail closed)", got, TierNone)
	}
	if IsTransientControllerError(err) {
		t.Fatal("IsTransientControllerError(undeclared needle) = true, want false (fail closed)")
	}
}

// TestAvailabilityTierWinsRegardlessOfTableOrder pins the precedence rule
// itself rather than an accident of how transientNeedles happens to be sorted.
// The table below is deliberately inverted — the semantic needle comes first —
// so a first-match-wins classifier answers TierSemantic and fails here. A store
// that is also unreachable must never have the bounded budget burned against
// it: the refusal it did not manage to deliver is not evidence about the graph.
func TestAvailabilityTierWinsRegardlessOfTableOrder(t *testing.T) {
	prev := transientNeedles
	transientNeedles = []transientNeedle{
		{needle: "cannot close blocked issue", tier: TierSemantic},
		{needle: "bad connection", tier: TierAvailability},
	}
	t.Cleanup(func() { transientNeedles = prev })

	err := errors.New("cannot close blocked issue: x is blocked by [y]: bad connection")
	if got := ClassifyControllerError(err); got != TierAvailability {
		t.Fatalf("ClassifyControllerError with the semantic needle declared first = %v, want %v", got, TierAvailability)
	}
	// The reverse table must give the same answer, so the result cannot be an
	// artifact of either ordering.
	transientNeedles = []transientNeedle{
		{needle: "bad connection", tier: TierAvailability},
		{needle: "cannot close blocked issue", tier: TierSemantic},
	}
	if got := ClassifyControllerError(err); got != TierAvailability {
		t.Fatalf("ClassifyControllerError with the availability needle declared first = %v, want %v", got, TierAvailability)
	}
}

// TestClassifyControllerErrorTiers pins which side of the boundary each error
// lands on. Tier A is the pre-split behavior verbatim; only the store refusal
// moved into the bounded tier.
func TestClassifyControllerErrorTiers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want ControllerErrorTier
	}{
		{name: "nil", err: nil, want: TierNone},
		{name: "deadline", err: context.DeadlineExceeded, want: TierAvailability},
		{name: "boundary marker", err: markTransientControllerBoundaryError(errors.New("attach fence lost")), want: TierAvailability},
		{name: "cas retries exhausted", err: &beads.CASRetriesExhaustedError{ID: "x", Key: "k", Attempts: 4}, want: TierAvailability},
		{name: "conditional writes unsupported", err: fmt.Errorf("wrapped: %w", beads.ErrConditionalWriteUnsupported), want: TierAvailability},
		{name: "dolt invalid connection timeout", err: errors.New("failed to check for dependency cycle: invalid connection: i/o timeout"), want: TierAvailability},
		{name: "mysql lock timeout", err: errors.New("Error 1205 (HY000): lock wait timeout exceeded; try restarting transaction"), want: TierAvailability},
		{name: "mysql deadlock", err: errors.New("Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction"), want: TierAvailability},
		{name: "sqlite locked", err: errors.New("listing sqlite ready beads: database is locked (5) (SQLITE_BUSY)"), want: TierAvailability},
		{name: "sqlite table locked", err: errors.New("listing sqlite ready beads: database table is locked"), want: TierAvailability},
		{name: "control work query sigterm", err: errors.New(`querying control work for fixture/core.control-dispatcher: running work query "bd ready": exit status 143: Terminated`), want: TierAvailability},
		{name: "dolt breaker open", err: errors.New("Error: failed to open database: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)"), want: TierAvailability},
		{name: "dolt server unreachable", err: errors.New("begin read tx: dolt server unreachable"), want: TierAvailability},
		{name: "workflow root close blocked", err: closeBlockedErr("gsp-p68ch6", "gsp-yl7fpr"), want: TierSemantic},
		{name: "scope body close blocked", err: closeBlockedErr("pl-pujtf", "pl-mmneh"), want: TierSemantic},
		{name: "non work query sigterm", err: errors.New("starting provider: exit status 143: Terminated"), want: TierNone},
		{name: "bad step spec", err: errors.New("deserializing step spec: invalid character 'n'"), want: TierNone},
		// Availability dominates: a refusal read off a store that is also
		// unreachable must never burn the bounded budget. Order-independence is
		// pinned separately by TestAvailabilityTierWinsRegardlessOfTableOrder.
		{name: "refusal during an outage", err: errors.New("cannot close blocked issue: x is blocked by [y]: bad connection"), want: TierAvailability},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyControllerError(tt.err); got != tt.want {
				t.Fatalf("ClassifyControllerError(%v) = %v, want %v", tt.err, got, tt.want)
			}
			wantTransient := tt.want == TierAvailability || tt.want == TierSemantic
			if got := IsTransientControllerError(tt.err); got != wantTransient {
				t.Fatalf("IsTransientControllerError(%v) = %v, want %v", tt.err, got, wantTransient)
			}
		})
	}
}

func newSemanticControlBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		Title:    "scope check",
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck},
	})
	if err != nil {
		t.Fatalf("create control bead: %v", err)
	}
	return bead
}

// TestRecordSemanticControlRetryStampsTheDeadlineOnce is the restart proof. The
// control dispatcher restarted five times during the three-day outage this
// bounds; a counter in process memory reset on every deploy, which is why the
// budget lives on the bead. Each call below is a fresh dispatcher: it carries
// no state from the previous one and reads the anchor back from the store.
func TestRecordSemanticControlRetryStampsTheDeadlineOnce(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	bead := newSemanticControlBead(t, store)
	cause := closeBlockedErr("pl-pujtf", "pl-mmneh")
	budget := 15 * time.Minute
	start := time.Date(2026, 8, 11, 8, 0, 47, 0, time.UTC)

	first, err := RecordSemanticControlRetry(store, bead.ID, cause, start, budget)
	if err != nil {
		t.Fatalf("first RecordSemanticControlRetry: %v", err)
	}
	if first.Expired {
		t.Fatal("first refusal reported Expired, want the full budget ahead of it")
	}
	if first.Repeat {
		t.Fatal("first refusal reported Repeat, want false so it still wakes the dispatcher")
	}
	if !first.FirstSeen.Equal(start) {
		t.Fatalf("first.FirstSeen = %s, want %s", first.FirstSeen, start)
	}
	if first.Attempts != 1 {
		t.Fatalf("first.Attempts = %d, want 1", first.Attempts)
	}

	// Five dispatcher restarts, each re-attempting inside the budget window.
	// A restart must not move the deadline.
	var last SemanticRetryState
	for restart := 1; restart <= 5; restart++ {
		now := start.Add(time.Duration(restart) * 2 * time.Minute)
		last, err = RecordSemanticControlRetry(store, bead.ID, cause, now, budget)
		if err != nil {
			t.Fatalf("restart %d RecordSemanticControlRetry: %v", restart, err)
		}
		if last.Expired {
			t.Fatalf("restart %d at +%s reported Expired inside a %s budget", restart, now.Sub(start), budget)
		}
		if !last.Repeat {
			t.Fatalf("restart %d reported Repeat=false for a verbatim repeat of the same refusal", restart)
		}
		if !last.FirstSeen.Equal(start) {
			t.Fatalf("restart %d FirstSeen = %s, want the original anchor %s (a restart must not extend the budget)",
				restart, last.FirstSeen, start)
		}
	}
	if last.Attempts != 6 {
		t.Fatalf("Attempts after five restarts = %d, want 6", last.Attempts)
	}

	// The sixth dispatcher starts at +15m01s. It has no memory of the previous
	// five, and the persisted anchor is what expires the budget.
	expired, err := RecordSemanticControlRetry(store, bead.ID, cause, start.Add(budget+time.Second), budget)
	if err != nil {
		t.Fatalf("post-budget RecordSemanticControlRetry: %v", err)
	}
	if !expired.Expired {
		t.Fatalf("refusal at +%s inside a %s budget reported Expired=false; the persisted deadline did not survive the restarts",
			budget+time.Second, budget)
	}
	if !expired.FirstSeen.Equal(start) {
		t.Fatalf("expired.FirstSeen = %s, want the original anchor %s", expired.FirstSeen, start)
	}

	after, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("get control bead: %v", err)
	}
	if got := after.Metadata[beadmeta.ControllerRetryFirstSeenMetadataKey]; got != start.Format(time.RFC3339) {
		t.Fatalf("%s = %q, want %q", beadmeta.ControllerRetryFirstSeenMetadataKey, got, start.Format(time.RFC3339))
	}
	if got := after.Metadata[beadmeta.ControllerRetryCountMetadataKey]; got != strconv.Itoa(expired.Attempts) {
		t.Fatalf("%s = %q, want %q", beadmeta.ControllerRetryCountMetadataKey, got, strconv.Itoa(expired.Attempts))
	}
	if got := after.Metadata[beadmeta.ControllerErrorClassMetadataKey]; got != beadmeta.FailureClassTransient {
		t.Fatalf("%s = %q, want %q", beadmeta.ControllerErrorClassMetadataKey, got, beadmeta.FailureClassTransient)
	}
	if got := after.Metadata[beadmeta.ControllerRetryableMetadataKey]; got != "true" {
		t.Fatalf("%s = %q, want \"true\"", beadmeta.ControllerRetryableMetadataKey, got)
	}
	// pl-mmneh carried zero failure metadata after 6,712 failures. Now the bead
	// explains itself to `bd show`.
	if got := after.Metadata[beadmeta.ControllerErrorMetadataKey]; got != cause.Error() {
		t.Fatalf("%s = %q, want the refusal text", beadmeta.ControllerErrorMetadataKey, got)
	}
}

// TestRecordSemanticControlRetryDoesNotReanchorOnChangedRefusal closes the back
// door into unbounded retry: the blocker list inside the refusal text shifts as
// unrelated siblings close, so re-anchoring on a changed message would let a
// permanently-stuck bead reset its own deadline forever.
func TestRecordSemanticControlRetryDoesNotReanchorOnChangedRefusal(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	bead := newSemanticControlBead(t, store)
	budget := 15 * time.Minute
	start := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC)

	if _, err := RecordSemanticControlRetry(store, bead.ID, closeBlockedErr("pl-pujtf", "pl-mmneh"), start, budget); err != nil {
		t.Fatalf("first RecordSemanticControlRetry: %v", err)
	}

	changed, err := RecordSemanticControlRetry(store, bead.ID,
		closeBlockedErr("pl-pujtf", "pl-mmneh pl-6o6uk"), start.Add(budget+time.Second), budget)
	if err != nil {
		t.Fatalf("changed RecordSemanticControlRetry: %v", err)
	}
	if !changed.FirstSeen.Equal(start) {
		t.Fatalf("FirstSeen after a changed refusal = %s, want the original anchor %s", changed.FirstSeen, start)
	}
	if !changed.Expired {
		t.Fatal("a changed refusal reset the budget; the deadline must anchor once per bead")
	}
	// A changed refusal is new information about the graph, so it is not quiet:
	// the dispatcher wakes back up for it.
	if changed.Repeat {
		t.Fatal("Repeat = true for a different refusal text, want false")
	}
}

func TestRecordSemanticControlRetryBudgetBoundaries(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		budget      time.Duration
		at          time.Duration
		wantExpired bool
	}{
		{name: "zero budget escalates immediately", budget: 0, at: 0, wantExpired: true},
		{name: "negative budget never escalates", budget: -1, at: 365 * 24 * time.Hour, wantExpired: false},
		{name: "one nanosecond before the deadline", budget: 15 * time.Minute, at: 15*time.Minute - time.Nanosecond, wantExpired: false},
		{name: "exactly at the deadline", budget: 15 * time.Minute, at: 15 * time.Minute, wantExpired: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := beads.NewMemStore()
			bead := newSemanticControlBead(t, store)
			cause := closeBlockedErr("pl-pujtf", "pl-mmneh")
			if _, err := RecordSemanticControlRetry(store, bead.ID, cause, start, tt.budget); err != nil {
				t.Fatalf("anchor RecordSemanticControlRetry: %v", err)
			}
			state, err := RecordSemanticControlRetry(store, bead.ID, cause, start.Add(tt.at), tt.budget)
			if err != nil {
				t.Fatalf("RecordSemanticControlRetry: %v", err)
			}
			if state.Expired != tt.wantExpired {
				t.Fatalf("Expired at +%s with budget %s = %v, want %v", tt.at, tt.budget, state.Expired, tt.wantExpired)
			}
		})
	}
}

func TestRecordSemanticControlRetryReanchorsAnUnparseableAnchor(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	bead := newSemanticControlBead(t, store)
	if err := store.SetMetadata(bead.ID, beadmeta.ControllerRetryFirstSeenMetadataKey, "not-a-timestamp"); err != nil {
		t.Fatalf("seed anchor: %v", err)
	}
	now := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC)

	state, err := RecordSemanticControlRetry(store, bead.ID, closeBlockedErr("a", "b"), now, 15*time.Minute)
	if err != nil {
		t.Fatalf("RecordSemanticControlRetry: %v", err)
	}
	if !state.FirstSeen.Equal(now) {
		t.Fatalf("FirstSeen = %s, want a fresh anchor at %s", state.FirstSeen, now)
	}
	if state.Expired {
		t.Fatal("an unparseable anchor expired the budget immediately, want a fresh window")
	}
	after, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("get control bead: %v", err)
	}
	if got := after.Metadata[beadmeta.ControllerRetryFirstSeenMetadataKey]; got != now.Format(time.RFC3339) {
		t.Fatalf("%s = %q, want the rewritten anchor %q", beadmeta.ControllerRetryFirstSeenMetadataKey, got, now.Format(time.RFC3339))
	}
}

func TestRecordSemanticControlRetryTruncatesTheRecordedRefusal(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	bead := newSemanticControlBead(t, store)
	long := errors.New(strings.Repeat("é", maxControllerRetryErrorMetadata))

	state, err := RecordSemanticControlRetry(store, bead.ID, long, time.Now(), 15*time.Minute)
	if err != nil {
		t.Fatalf("RecordSemanticControlRetry: %v", err)
	}
	if state.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", state.Attempts)
	}
	after, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("get control bead: %v", err)
	}
	recorded := after.Metadata[beadmeta.ControllerErrorMetadataKey]
	if len(recorded) > maxControllerRetryErrorMetadata {
		t.Fatalf("recorded refusal length = %d, want <= %d", len(recorded), maxControllerRetryErrorMetadata)
	}
	if !utf8.ValidString(recorded) {
		t.Fatalf("recorded refusal is invalid UTF-8: %q", recorded)
	}
}

func TestRecordSemanticControlRetrySurfacesStoreFailures(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	if _, err := RecordSemanticControlRetry(store, "gc-nonexistent", closeBlockedErr("a", "b"), time.Now(), time.Minute); err == nil {
		t.Fatal("RecordSemanticControlRetry on a missing bead = nil error, want the store failure surfaced")
	}
}

func TestQuietControllerRetryPreservesTheOriginalError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel")
	cause := fmt.Errorf("closing scope body: %w", sentinel)

	if IsQuietControllerRetry(cause) {
		t.Fatal("IsQuietControllerRetry(unmarked) = true, want false")
	}
	quiet := MarkQuietControllerRetry(cause)
	if !IsQuietControllerRetry(quiet) {
		t.Fatal("IsQuietControllerRetry(marked) = false, want true")
	}
	if quiet.Error() != cause.Error() {
		t.Fatalf("marked error message = %q, want the original %q", quiet.Error(), cause.Error())
	}
	if !errors.Is(quiet, sentinel) {
		t.Fatal("marking broke the errors.Is chain")
	}
	if MarkQuietControllerRetry(nil) != nil {
		t.Fatal("MarkQuietControllerRetry(nil) != nil")
	}
	if got := MarkQuietControllerRetry(quiet); !reflect.DeepEqual(got, quiet) {
		t.Fatalf("re-marking allocated a second wrapper: %v", got)
	}
	// The whole point of preserving the message: a quiet retry is still a
	// transient controller error to every existing classifier.
	blocked := MarkQuietControllerRetry(closeBlockedErr("pl-pujtf", "pl-mmneh"))
	if !IsTransientControllerError(blocked) {
		t.Fatal("a marked Tier-B error stopped classifying as transient")
	}
	if got := ClassifyControllerError(blocked); got != TierSemantic {
		t.Fatalf("ClassifyControllerError(marked) = %v, want %v", got, TierSemantic)
	}
}

// TestClearControllerSpawnErrorMetadataClearsTheBudgetAnchor keeps the budget
// tied to the failure it bounds. A bead that reaches a clean disposition and is
// later re-minted or reopened must start from a fresh window, not inherit an
// already-expired deadline and quarantine itself on its first refusal.
func TestClearControllerSpawnErrorMetadataClearsTheBudgetAnchor(t *testing.T) {
	t.Parallel()

	metadata := map[string]string{
		beadmeta.ControllerErrorMetadataKey:          "boom",
		beadmeta.ControllerErrorClassMetadataKey:     beadmeta.FailureClassTransient,
		beadmeta.ControllerRetryableMetadataKey:      "true",
		beadmeta.ControllerRetryFirstSeenMetadataKey: "2026-08-11T08:00:47Z",
		beadmeta.ControllerRetryCountMetadataKey:     "6712",
	}
	clearControllerSpawnErrorMetadata(metadata)

	for _, key := range []string{
		beadmeta.ControllerErrorMetadataKey,
		beadmeta.ControllerErrorClassMetadataKey,
		beadmeta.ControllerRetryableMetadataKey,
		beadmeta.ControllerRetryFirstSeenMetadataKey,
		beadmeta.ControllerRetryCountMetadataKey,
	} {
		if got := metadata[key]; got != "" {
			t.Fatalf("%s = %q after clearing, want empty", key, got)
		}
	}
}
