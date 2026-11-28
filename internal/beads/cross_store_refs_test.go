package beads_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// showOnlyBdRunner is a bd CLI that answers `bd show --json <ids...>` from its
// rows and refuses every other command, recording each one it is asked to run,
// so a test sees exactly what a read cost the ledger.
type showOnlyBdRunner struct {
	mu    sync.Mutex
	rows  map[string]string
	calls []string
}

func (r *showOnlyBdRunner) run(_, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	if name != "bd" || len(args) < 2 || args[0] != "show" || args[1] != "--json" {
		return nil, fmt.Errorf("unexpected command: %s", call)
	}
	var found []string
	for _, id := range args[2:] {
		if row, ok := r.rows[id]; ok {
			found = append(found, row)
		}
	}
	return []byte("[" + strings.Join(found, ",") + "]"), nil
}

func (r *showOnlyBdRunner) took() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

// TestGetManyReadsACachedBdLegWithOneShow is the controller leg's cost (#8360
// review, iteration 2 blocker). A CachingStore over bd has no exact batch read
// of its own, and GetMany used to answer it with its live handle's List by id,
// which bd can only run as a whole-ledger `bd list --all --limit 0` and a whole
// ephemeral `bd query`. Bare or cached, the ledger must be asked one `bd show`
// for the ids and nothing else.
func TestGetManyReadsACachedBdLegWithOneShow(t *testing.T) {
	runner := &showOnlyBdRunner{rows: map[string]string{
		"gc-1": `{"id":"gc-1","title":"one","status":"open","issue_type":"task"}`,
		"gc-2": `{"id":"gc-2","title":"two","status":"closed","issue_type":"task"}`,
	}}
	bd := beads.NewBdStore("/city", runner.run)
	for _, leg := range []struct {
		name  string
		store beads.Store
	}{
		{"bare", bd},
		{"cached", beads.NewCachingStoreForTest(bd, nil)},
	} {
		runner.took()
		found, err := beads.GetMany(leg.store, []string{"gc-1", "gc-2", "gc-3", "gc-1"})
		if err != nil {
			t.Fatalf("%s: GetMany: %v", leg.name, err)
		}
		if len(found) != 2 || found["gc-1"].Status != "open" || found["gc-2"].Status != "closed" {
			t.Errorf("%s: GetMany = %+v, want gc-1 open and gc-2 closed, gc-3 absent", leg.name, found)
		}
		if got, want := runner.took(), []string{"bd show --json gc-1 gc-2 gc-3"}; !slices.Equal(got, want) {
			t.Errorf("%s: GetMany ran %q, want only %q: a by-id read is keyed", leg.name, got, want)
		}
	}
}

// getOnlyStore is a store with no exact batch read. It records the ids it is
// asked to Get and counts its Lists, and its Get of an id in fail returns that
// error and of an id in alias answers the bead aliased to it.
type getOnlyStore struct {
	beads.Store
	fail  map[string]error
	alias map[string]string

	mu    sync.Mutex
	gets  []string
	lists int
}

func (s *getOnlyStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	s.gets = append(s.gets, id)
	s.mu.Unlock()
	if err := s.fail[id]; err != nil {
		return beads.Bead{}, err
	}
	if target, ok := s.alias[id]; ok {
		return s.Store.Get(target)
	}
	return s.Store.Get(id)
}

func (s *getOnlyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Store.List(q)
}

// TestGetManyReadsAStoreWithNoBatchByID: a store with no exact batch read is
// read with one keyed Get per id, never with a List. A miss reads as absent,
// as do a substring collision, a Get that answers a different bead, and a
// store stating it holds no row under a relocated class's reserved prefix; any
// other failure fails the read, because a blocker that could not be read is
// not one that is absent.
func TestGetManyReadsAStoreWithNoBatchByID(t *testing.T) {
	mem := beads.NewMemStore()
	open, err := mem.Create(beads.Bead{Title: "open", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := mem.Create(beads.Bead{Title: "closed", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	store := &getOnlyStore{
		Store: mem,
		fail: map[string]error{
			"gcg-1":      fmt.Errorf("get gcg-1: %w", beads.ErrBdSQLClassRelocated),
			"gc-collide": fmt.Errorf("get gc-collide: %w", beads.ErrIDCollision),
		},
		alias: map[string]string{"gc-short": open.ID},
	}
	ids := []string{open.ID, closed.ID, "gc-missing", "gcg-1", "gc-collide", "gc-short", open.ID}
	found, err := beads.GetMany(store, ids)
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if len(found) != 2 || found[open.ID].Status != "open" || found[closed.ID].Status != "closed" {
		t.Errorf("GetMany = %+v, want exactly %s (open) and %s (closed)", found, open.ID, closed.ID)
	}
	if store.lists != 0 {
		t.Errorf("GetMany listed a store with no batch read %d time(s), want 0: a by-id read is keyed", store.lists)
	}
	got := slices.Clone(store.gets)
	slices.Sort(got)
	want := []string{open.ID, closed.ID, "gc-missing", "gcg-1", "gc-collide", "gc-short"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("GetMany read %v by id, want each distinct id once: %v", got, want)
	}

	broken := errors.New("dial tcp 127.0.0.1:1: connection refused")
	store.fail["gc-broken"] = broken
	if _, err := beads.GetMany(store, []string{open.ID, "gc-broken"}); !errors.Is(err, broken) {
		t.Fatalf("GetMany over a failing Get = %v, want the read's own error", err)
	}
}

// TestGetManyOverACachedNativeLegIsKeyed: a CachingStore over the native store
// (a controller leg) is read by id without a listing that walks the ledger;
// the native backend counts every listing request it sends, and the read adds
// no unkeyed one.
func TestGetManyOverACachedNativeLegIsKeyed(t *testing.T) {
	native := splittest.NewNativeWorkStore(t, "gc")
	open, err := native.Create(beads.Bead{Title: "open", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := native.Create(beads.Bead{Title: "closed", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := native.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	cached := beads.NewCachingStoreForTest(native, nil)
	before, ok := beads.ListRequestStatsOf(native)
	if !ok {
		t.Fatal("fixture: the native store keeps no listing counts")
	}
	found, err := beads.GetMany(cached, []string{open.ID, closed.ID, "gc-absent"})
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	after, _ := beads.ListRequestStatsOf(native)
	if unkeyed := after.Unkeyed - before.Unkeyed; unkeyed != 0 {
		t.Errorf("GetMany sent %d unkeyed listing request(s), want 0: a by-id read never walks the ledger", unkeyed)
	}
	if len(found) != 2 || found[open.ID].Status != "open" || found[closed.ID].Status != "closed" {
		t.Errorf("GetMany = %+v, want %s open and %s closed, gc-absent absent", found, open.ID, closed.ID)
	}
}

// tieredBdRunner is a bd CLI over the two tiers bd keeps: `bd show --json`
// reads only the issue tier (one id it does not hold fails "no issue found",
// several answer the rows it holds), and `bd query --json "ephemeral=true AND
// id=<id>"` reads the wisp tier. It refuses every other command.
type tieredBdRunner struct {
	issues, wisps map[string]string
}

func (r *tieredBdRunner) run(_, name string, args ...string) ([]byte, error) {
	switch {
	case name == "bd" && len(args) > 2 && args[0] == "show" && args[1] == "--json":
		var found []string
		for _, id := range args[2:] {
			if row, ok := r.issues[id]; ok {
				found = append(found, row)
			}
		}
		if len(found) == 0 && len(args) == 3 {
			return nil, fmt.Errorf("Error: no issue found matching %q", args[2])
		}
		return []byte("[" + strings.Join(found, ",") + "]"), nil
	case name == "bd" && len(args) > 2 && args[0] == "query" && args[1] == "--json":
		if id, ok := strings.CutPrefix(args[2], "ephemeral=true AND id="); ok {
			if row, held := r.wisps[id]; held {
				return []byte("[" + row + "]"), nil
			}
			return []byte("[]"), nil
		}
	}
	return nil, fmt.Errorf("unexpected command: %s %s", name, strings.Join(args, " "))
}

// TestGetEachFindsTheWispABdBatchReadMisses: bd's batch read is `bd show`,
// which reads only the issue tier, so GetMany reads a wisp a bd ledger holds
// as absent. GetEach reads each id with Get, which falls back to the wisp
// tier, so a caller that reads again the ids the batch did not find finds the
// wisp, and an id the ledger holds in neither tier still reads as absent.
func TestGetEachFindsTheWispABdBatchReadMisses(t *testing.T) {
	runner := &tieredBdRunner{
		issues: map[string]string{"gc-1": `{"id":"gc-1","title":"issue","status":"open","issue_type":"task"}`},
		wisps:  map[string]string{"gc-wisp-2": `{"id":"gc-wisp-2","title":"wisp","status":"open","issue_type":"task"}`},
	}
	bd := beads.NewBdStore("/city", runner.run)
	found, err := beads.GetMany(bd, []string{"gc-1", "gc-wisp-2", "gc-3"})
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if _, ok := found["gc-1"]; !ok || len(found) != 1 {
		t.Fatalf("precondition: GetMany = %+v, want only gc-1: bd's batch read misses the wisp tier", found)
	}
	read, err := beads.GetEach(bd, []string{"gc-wisp-2", "gc-3", "gc-wisp-2"})
	if err != nil {
		t.Fatalf("GetEach: %v", err)
	}
	if wisp, ok := read["gc-wisp-2"]; !ok || !wisp.Ephemeral || len(read) != 1 {
		t.Fatalf("GetEach = %+v, want the wisp gc-wisp-2 and gc-3 absent", read)
	}
}

// TestGetEachKeepsExactIDsAndFailsOnAFailedRead: GetEach reads each distinct
// id once, keeps only beads answered under their exact id, and fails on a
// read that failed rather than reading the bead as absent.
func TestGetEachKeepsExactIDsAndFailsOnAFailedRead(t *testing.T) {
	mem := beads.NewMemStore()
	held, err := mem.Create(beads.Bead{Title: "held", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	broken := errors.New("dial tcp 127.0.0.1:1: connection refused")
	store := &getOnlyStore{
		Store: mem,
		fail:  map[string]error{"gc-broken": broken},
		alias: map[string]string{"gc-short": held.ID},
	}
	found, err := beads.GetEach(store, []string{held.ID, "gc-short", "gc-missing", held.ID})
	if err != nil {
		t.Fatalf("GetEach: %v", err)
	}
	if len(found) != 1 || found[held.ID].ID != held.ID {
		t.Fatalf("GetEach = %+v, want only %s", found, held.ID)
	}
	got := slices.Sorted(slices.Values(store.gets))
	want := slices.Sorted(slices.Values([]string{held.ID, "gc-short", "gc-missing"}))
	if !slices.Equal(got, want) {
		t.Fatalf("GetEach read %v, want each distinct id once: %v", got, want)
	}
	if _, err := beads.GetEach(store, []string{held.ID, "gc-broken"}); !errors.Is(err, broken) {
		t.Fatalf("GetEach over a failing Get = %v, want the read's own error", err)
	}
}
