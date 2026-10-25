package beads

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// fakeClock is a settable wall clock for MemStore's creation stamps. A frozen
// fakeClock is the coarse clock a macOS runner shows a burst of creates: every
// create in one tick reads the same time.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func ids(bs []Bead) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

func reversed(s []string) []string {
	out := slices.Clone(s)
	slices.Reverse(out)
	return out
}

// wantCreationOrder checks that the canonical created-order reads return
// exactly the creation order (oldest first) and its reverse (newest first).
func wantCreationOrder(t *testing.T, s Store, label string, created []string) {
	t.Helper()
	asc, err := s.List(ListQuery{Label: label, Sort: SortCreatedAsc})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(asc); !slices.Equal(got, created) {
		t.Errorf("SortCreatedAsc = %v, want creation order %v", got, created)
	}
	desc, err := s.ListByLabel(label, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(desc), reversed(created); !slices.Equal(got, want) {
		t.Errorf("ListByLabel (newest first) = %v, want %v", got, want)
	}
}

func wantStrictlyIncreasing(t *testing.T, bs []Bead) {
	t.Helper()
	for i := 1; i < len(bs); i++ {
		if !bs[i].CreatedAt.After(bs[i-1].CreatedAt) {
			t.Errorf("CreatedAt of %s (%v) is not after %s (%v)", bs[i].ID, bs[i].CreatedAt, bs[i-1].ID, bs[i-1].CreatedAt)
		}
	}
}

// Minted ids compare lexically (gc-10 < gc-9), so an id tie-break cannot stand
// in for creation order: beads created in one clock tick must still carry
// strictly increasing stamps.
func TestMemStoreMintedCreatesInOneTickKeepCreationOrder(t *testing.T) {
	clock := newFakeClock()
	m := NewMemStore()
	m.now = clock.now

	var created []Bead
	for i := 0; i < 12; i++ {
		b, err := m.Create(Bead{Title: fmt.Sprint(i), Labels: []string{"burst"}})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, b)
	}
	wantStrictlyIncreasing(t, created)
	wantCreationOrder(t, m, "burst", ids(created))
}

// Pinned ids (a guarded batch's facts, explicit ids) are arbitrary hashes, so
// their id order says nothing about creation order either. The batch path and
// Create share one monotonic stamp.
func TestMemStorePinnedCreatesInOneTickKeepCreationOrder(t *testing.T) {
	clock := newFakeClock()
	m := NewMemStore()
	m.HonorExplicitIDs = true
	m.now = clock.now

	// Each pinned id sorts lexically BEFORE the one created ahead of it.
	if _, err := m.Create(Bead{ID: "gc-zz", Labels: []string{"facts"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyGuarded(context.Background(), GuardedBatch{Items: []GuardedItem{
		{Create: &Bead{ID: "gc-yy", Labels: []string{"facts"}}},
		{Create: &Bead{ID: "gc-xx", Labels: []string{"facts"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(Bead{ID: "gc-ww", Labels: []string{"facts"}}); err != nil {
		t.Fatal(err)
	}
	wantCreationOrder(t, m, "facts", []string{"gc-zz", "gc-yy", "gc-xx", "gc-ww"})
}

// Once the clock moves past the last stamp, creates take the clock's time
// exactly: the tie-break nudges a stamp only within one tick, so stamps never
// run ahead of a clock that keeps moving.
func TestMemStoreCreatedAtTracksTheClockOnceItMoves(t *testing.T) {
	clock := newFakeClock()
	m := NewMemStore()
	m.now = clock.now

	for i := 0; i < 3; i++ {
		if _, err := m.Create(Bead{}); err != nil {
			t.Fatal(err)
		}
	}
	clock.advance(time.Microsecond)
	b, err := m.Create(Bead{})
	if err != nil {
		t.Fatal(err)
	}
	if !b.CreatedAt.Equal(clock.now()) {
		t.Errorf("CreatedAt = %v, want the clock's %v", b.CreatedAt, clock.now())
	}
	if !b.UpdatedAt.Equal(b.CreatedAt) {
		t.Errorf("UpdatedAt = %v, want CreatedAt %v", b.UpdatedAt, b.CreatedAt)
	}
}

// A wall clock that steps backward (NTP) does not reorder later creates ahead
// of earlier ones.
func TestMemStoreCreatedAtSurvivesABackwardClockStep(t *testing.T) {
	clock := newFakeClock()
	m := NewMemStore()
	m.now = clock.now

	first, err := m.Create(Bead{Labels: []string{"step"}})
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(-time.Second)
	second, err := m.Create(Bead{Labels: []string{"step"}})
	if err != nil {
		t.Fatal(err)
	}
	wantStrictlyIncreasing(t, []Bead{first, second})
	wantCreationOrder(t, m, "step", []string{first.ID, second.ID})
}

func openFileStoreWithClock(t *testing.T, path string, clock *fakeClock) *FileStore {
	t.Helper()
	fs, err := OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	fs.now = clock.now
	return fs
}

// FileStore persists the stamps, so the order a burst was created in survives
// a reopen, and a process that opens the file in the same tick still creates
// after every bead already on disk.
func TestFileStoreKeepsCreationOrderAcrossReopenInOneTick(t *testing.T) {
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "beads.json")

	first := openFileStoreWithClock(t, path, clock)
	var created []string
	for i := 0; i < 11; i++ {
		b, err := first.Create(Bead{Labels: []string{"burst"}})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, b.ID)
	}

	reopened := openFileStoreWithClock(t, path, clock)
	wantCreationOrder(t, reopened, "burst", created)

	b, err := reopened.Create(Bead{Labels: []string{"burst"}})
	if err != nil {
		t.Fatal(err)
	}
	created = append(created, b.ID)
	wantCreationOrder(t, reopened, "burst", created)
}

// Two handles on one file (the CLI and the controller) interleave creates in
// one tick: the writer reloads before it writes, so it stamps after whatever
// the other handle persisted.
func TestFileStoreHandlesSharingAFileKeepCreationOrderInOneTick(t *testing.T) {
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "beads.json")
	a := openFileStoreWithClock(t, path, clock)
	b := openFileStoreWithClock(t, path, clock)
	lock := NewFileFlock(path + ".lock")
	a.SetLocker(lock)
	b.SetLocker(lock)

	var created []string
	for i := 0; i < 12; i++ {
		s := a
		if i%2 == 1 {
			s = b
		}
		bead, err := s.Create(Bead{Labels: []string{"shared"}})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, bead.ID)
	}
	reader := openFileStoreWithClock(t, path, clock)
	wantCreationOrder(t, reader, "shared", created)
}

// Restarts do not ratchet the stamps into the future: each reopen seeds from
// the newest persisted stamp, which a burst puts at most one nanosecond per
// create past the tick, and the next create takes the (later) clock again.
func TestFileStoreCreatedAtDoesNotDriftAcrossRestarts(t *testing.T) {
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "beads.json")
	const burst = 50
	for restart := 0; restart < 5; restart++ {
		s := openFileStoreWithClock(t, path, clock)
		for i := 0; i < burst; i++ {
			b, err := s.Create(Bead{})
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 && !b.CreatedAt.Equal(clock.now()) {
				t.Fatalf("restart %d: first create stamped %v, want the clock's %v", restart, b.CreatedAt, clock.now())
			}
			if ahead := b.CreatedAt.Sub(clock.now()); ahead > burst*time.Nanosecond {
				t.Fatalf("restart %d: CreatedAt runs %v ahead of the clock", restart, ahead)
			}
		}
		clock.advance(time.Millisecond)
	}
}

// A bead persisted with a stamp ahead of this host's clock by no more than
// maxCreatedAtLead (a small backward clock step) keeps sorting before beads
// created after it. The stamps run ahead only until the clock catches up,
// and then track it exactly.
func TestFileStoreCreatesAfterASlightlyFutureStampedBead(t *testing.T) {
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "beads.json")
	s := openFileStoreWithClock(t, path, clock)
	clock.advance(maxCreatedAtLead / 2)
	future, err := s.Create(Bead{Labels: []string{"skew"}})
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(-maxCreatedAtLead / 2)

	reopened := openFileStoreWithClock(t, path, clock)
	next, err := reopened.Create(Bead{Labels: []string{"skew"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := next.CreatedAt, future.CreatedAt.Add(time.Nanosecond); !got.Equal(want) {
		t.Errorf("create after a slightly future stamp = %v, want %v", got, want)
	}
	wantCreationOrder(t, reopened, "skew", []string{future.ID, next.ID})

	clock.advance(maxCreatedAtLead)
	caughtUp, err := reopened.Create(Bead{})
	if err != nil {
		t.Fatal(err)
	}
	if !caughtUp.CreatedAt.Equal(clock.now()) {
		t.Errorf("once the clock passes the future stamp, CreatedAt = %v, want the clock's %v", caughtUp.CreatedAt, clock.now())
	}
}

// One bead stamped far in the future (a corrupt or foreign record) must not
// drag every later create into the future with it: purge cutoffs, grace
// periods and ages all read CreatedAt against the clock. Past
// maxCreatedAtLead, creates take the clock again and the store warns once.
func TestFileStoreFarFutureStampedBeadDoesNotDragCreatesForward(t *testing.T) {
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "beads.json")
	s := openFileStoreWithClock(t, path, clock)
	clock.advance(365 * 24 * time.Hour)
	if _, err := s.Create(Bead{Title: "a year ahead"}); err != nil {
		t.Fatal(err)
	}
	clock.advance(-365 * 24 * time.Hour)

	reopened := openFileStoreWithClock(t, path, clock)
	var warnings []string
	reopened.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	var created []Bead
	for i := 0; i < 3; i++ {
		b, err := reopened.Create(Bead{})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, b)
	}
	if !created[0].CreatedAt.Equal(clock.now()) {
		t.Errorf("first create after a far-future bead = %v, want the clock's %v", created[0].CreatedAt, clock.now())
	}
	wantStrictlyIncreasing(t, created)
	if ahead := created[2].CreatedAt.Sub(clock.now()); ahead > 2*time.Nanosecond {
		t.Errorf("creates run %v ahead of the clock after the reset", ahead)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ahead of the clock") {
		t.Errorf("warnings = %q, want one about the stamp ahead of the clock", warnings)
	}

	// A restart re-reads the far-future bead; the store recovers again.
	again := openFileStoreWithClock(t, path, clock)
	again.warnf = func(string, ...any) {}
	b, err := again.Create(Bead{})
	if err != nil {
		t.Fatal(err)
	}
	if ahead := b.CreatedAt.Sub(clock.now()); ahead > maxCreatedAtLead {
		t.Errorf("after a restart, CreatedAt runs %v ahead of the clock", ahead)
	}
}

// A wall clock that jumps back further than maxCreatedAtLead in-process (a
// host clock corrected by hours) resets the stamps to the clock too.
func TestMemStoreLargeBackwardClockStepTakesTheClock(t *testing.T) {
	clock := newFakeClock()
	m := NewMemStore()
	m.now = clock.now
	m.warnf = func(string, ...any) {}
	if _, err := m.Create(Bead{}); err != nil {
		t.Fatal(err)
	}
	clock.advance(-time.Hour)
	b, err := m.Create(Bead{})
	if err != nil {
		t.Fatal(err)
	}
	if !b.CreatedAt.Equal(clock.now()) {
		t.Errorf("CreatedAt after a one-hour backward step = %v, want the clock's %v", b.CreatedAt, clock.now())
	}
}

// An update never stamps UpdatedAt before the bead's CreatedAt, even while
// a same-tick burst holds CreatedAt a few nanoseconds ahead of the clock.
func TestMemStoreUpdatedAtNeverPrecedesCreatedAt(t *testing.T) {
	// The store's clock reads ahead of the process clock, as the stamps of a
	// same-tick burst (or a slightly future persisted bead) do.
	clock := &fakeClock{t: time.Now().Add(maxCreatedAtLead / 2)}
	m := NewMemStore()
	m.now = clock.now
	var last Bead
	for i := 0; i < 5; i++ {
		b, err := m.Create(Bead{})
		if err != nil {
			t.Fatal(err)
		}
		last = b
	}
	if err := m.SetMetadata(last.ID, "k", "v"); err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(last.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Errorf("UpdatedAt %v precedes CreatedAt %v", got.UpdatedAt, got.CreatedAt)
	}
}
