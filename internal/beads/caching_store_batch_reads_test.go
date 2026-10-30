package beads_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// countingFakeBd is a fake bd that records every command a BdStore runs.
type countingFakeBd struct {
	*beadstest.FakeBd
	mu    sync.Mutex // guards calls: a two-tier List runs its two bd reads at once
	calls [][]string
}

func newCountingFakeBd() *countingFakeBd {
	return &countingFakeBd{FakeBd: beadstest.NewFakeBd()}
}

func (f *countingFakeBd) run(dir, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(args))
	f.mu.Unlock()
	return f.Run(dir, name, args...)
}

func (f *countingFakeBd) store() *beads.BdStore {
	return beads.NewBdStore("/fake-bd", f.run)
}

// verbCalls counts the recorded commands whose argv starts with verb's words.
func (f *countingFakeBd) verbCalls(verb ...string) int {
	n := 0
	for _, call := range f.calls {
		if len(call) >= len(verb) && slices.Equal(call[:len(verb)], verb) {
			n++
		}
	}
	return n
}

func seedFake(t *testing.T, f *countingFakeBd, b beads.Bead) beads.Bead {
	t.Helper()
	seeded, err := f.Seed(b)
	if err != nil {
		t.Fatalf("seed %q: %v", b.Title, err)
	}
	return seeded
}

func childIDs(children []beads.Bead) []string {
	ids := make([]string, 0, len(children))
	for _, c := range children {
		ids = append(ids, c.ID+"<"+c.ParentID)
	}
	slices.Sort(ids)
	return ids
}

// perParentChildren is the per-parent answer a batched children read must
// reproduce: the union of Children(id, IncludeClosed, opts...).
func perParentChildren(t *testing.T, store beads.Store, parents []string, opts ...beads.QueryOpt) []string {
	t.Helper()
	var all []beads.Bead
	for _, p := range parents {
		children, err := store.Children(p, append([]beads.QueryOpt{beads.IncludeClosed}, opts...)...)
		if err != nil {
			t.Fatalf("Children(%s): %v", p, err)
		}
		all = append(all, children...)
	}
	return childIDs(all)
}

// TestCachingStoreChildrenOfAnyReadsThroughTheBackingBatch pins that a cached
// bd store answers a frontier's children with bd's batched read, not one
// `bd list --parent` (plus one `bd query` for the wisp tier) per parent, and
// that the batch answers what the per-parent reads answer at each tier: closed
// children included, the ephemeral child only when the read asks for wisps.
func TestCachingStoreChildrenOfAnyReadsThroughTheBackingBatch(t *testing.T) {
	for _, primed := range []bool{false, true} {
		name := "cold cache"
		if primed {
			name = "primed cache"
		}
		t.Run(name, func(t *testing.T) {
			fake := newCountingFakeBd()
			var parents []string
			for range 4 {
				p := seedFake(t, fake, beads.Bead{Title: "parent"})
				parents = append(parents, p.ID)
				seedFake(t, fake, beads.Bead{Title: "child", ParentID: p.ID})
				closed := seedFake(t, fake, beads.Bead{Title: "closed child", ParentID: p.ID})
				if _, err := fake.Run("", "bd", "close", "--force", "--json", closed.ID); err != nil {
					t.Fatalf("close %s: %v", closed.ID, err)
				}
				seedFake(t, fake, beads.Bead{Title: "wisp child", ParentID: p.ID, Ephemeral: true})
			}
			cs := beads.NewCachingStoreForTest(fake.store(), nil)
			if primed {
				if err := cs.Prime(context.Background()); err != nil {
					t.Fatalf("Prime: %v", err)
				}
			}
			if _, ok := any(cs).(beads.ChildrenBatchLister); !ok {
				t.Fatal("CachingStore does not advertise ChildrenOfAny, so a cached bd store reads children one parent at a time")
			}

			for _, tc := range []struct {
				name string
				opts []beads.QueryOpt
				want int
			}{
				{"issues tier", nil, 8},
				{"both tiers", []beads.QueryOpt{beads.WithBothTiers}, 12},
			} {
				want := perParentChildren(t, cs, parents, tc.opts...)
				if len(want) != tc.want {
					t.Fatalf("%s: per-parent children = %v, want %d rows", tc.name, want, tc.want)
				}
				fake.calls = nil
				batched, err := beads.ChildrenOfAny(cs, parents, tc.opts...)
				if err != nil {
					t.Fatalf("%s: ChildrenOfAny: %v", tc.name, err)
				}
				if got := childIDs(batched); !slices.Equal(got, want) {
					t.Fatalf("%s: ChildrenOfAny = %v, want the per-parent union %v", tc.name, got, want)
				}
				if n := fake.verbCalls("list") + fake.verbCalls("query"); n != 0 {
					t.Fatalf("%s: ChildrenOfAny ran %d per-parent list/query commands %v, want the batched read", tc.name, n, fake.calls)
				}
				// The issues-tier read over a primed cache also re-reads each
				// cached wisp child, which the List over its parent cannot see:
				// the same refresh a per-parent Children runs. The policy layer
				// reads at both tiers, where nothing cached is unaccounted for.
				if (!primed || tc.opts != nil) && len(fake.calls) != 2 {
					t.Fatalf("%s: ChildrenOfAny over %d parents ran %d bd commands %v, want 2 (one dep list, one show)", tc.name, len(parents), len(fake.calls), fake.calls)
				}
			}
		})
	}
}

// TestCachingStoreChildrenOfAnyWithoutABackingBatchReadsEachParent pins the
// fallback: a backing store with no batched read is read one parent at a time
// through the cache's own Children, with the same answer.
func TestCachingStoreChildrenOfAnyWithoutABackingBatchReadsEachParent(t *testing.T) {
	mem := beads.NewMemStore()
	p1, _ := mem.Create(beads.Bead{Title: "p1"})
	p2, _ := mem.Create(beads.Bead{Title: "p2"})
	c1, _ := mem.Create(beads.Bead{Title: "c1", ParentID: p1.ID})
	c2, _ := mem.Create(beads.Bead{Title: "c2", ParentID: p2.ID})
	if err := mem.Close(c2.ID); err != nil {
		t.Fatal(err)
	}
	cs := beads.NewCachingStoreForTest(mem, nil)
	children, err := cs.ChildrenOfAny([]string{p1.ID, p2.ID})
	if err != nil {
		t.Fatalf("ChildrenOfAny: %v", err)
	}
	if got, want := childIDs(children), childIDs([]beads.Bead{c1, c2}); !slices.Equal(got, want) {
		t.Fatalf("children = %v, want %v", got, want)
	}
}

// TestCachingStoreDepListBatchAnswersAsDepList pins the cache's batched
// dep-edge read: one bd command for many anchors, each anchor answered as the
// cache's own DepList answers it, a lone anchor read with `bd dep list <id>`
// (whose one-id output is rows, not records), and a backing store without the
// batch reported with the sentinel callers fall back on.
func TestCachingStoreDepListBatchAnswersAsDepList(t *testing.T) {
	fake := newCountingFakeBd()
	a := seedFake(t, fake, beads.Bead{Title: "a"})
	b := seedFake(t, fake, beads.Bead{Title: "b"})
	c := seedFake(t, fake, beads.Bead{Title: "c", ParentID: a.ID})
	if err := fake.SeedDep(b.ID, a.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := fake.SeedDep(c.ID, b.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	cs := beads.NewCachingStoreForTest(fake.store(), nil)
	batch, ok := beads.DepListBatchFor(cs)
	if !ok {
		t.Fatal("CachingStore does not advertise DepListBatch, so a cached bd store reads close dependencies one anchor at a time")
	}

	ids := []string{a.ID, b.ID, c.ID}
	fake.calls = nil
	got, err := batch.DepListBatch(ids)
	if err != nil {
		t.Fatalf("DepListBatch: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("DepListBatch over %d anchors ran %d bd commands %v, want 1", len(ids), len(fake.calls), fake.calls)
	}
	for _, id := range ids {
		single, err := cs.DepList(id, "down")
		if err != nil {
			t.Fatalf("DepList(%s): %v", id, err)
		}
		if len(single) == 0 && len(got[id]) == 0 {
			continue
		}
		if !reflect.DeepEqual(sortedDeps(single), sortedDeps(got[id])) {
			t.Errorf("anchor %s: batch %v, DepList %v", id, got[id], single)
		}
	}

	lone, err := batch.DepListBatch([]string{c.ID})
	if err != nil {
		t.Fatalf("DepListBatch(one id): %v", err)
	}
	if len(lone[c.ID]) != 2 {
		t.Fatalf("DepListBatch(%s) = %v, want its parent-child and blocks edges", c.ID, lone)
	}

	unbatched := beads.NewCachingStoreForTest(storeWithoutDepBatch{Store: beads.NewMemStore()}, nil)
	deps, err := unbatched.DepListBatch([]string{"gc-1", "gc-2"})
	if !errors.Is(err, beads.ErrDepListBatchUnsupported) || deps != nil {
		t.Fatalf("DepListBatch over a backing without the batch = (%v, %v), want (nil, ErrDepListBatchUnsupported)", deps, err)
	}
}

// postgresRelationRefusal is how the bd/Postgres backend refuses a relation
// read (ga-7i7ts), as bd prints it.
const postgresRelationRefusal = `exit status 1: Error: operation "IssueRelations" not supported by the postgres backend`

// refuseBatchDepList wraps a fake bd so a multi-id DOWN `bd dep list` (the
// batch) is refused the way the bd/Postgres backend refuses it, while every
// other command, a one-id `bd dep list` included, is answered.
func refuseBatchDepList(run beads.CommandRunner) beads.CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		if len(args) > 3 && args[0] == "dep" && args[1] == "list" && !slices.Contains(args, "--direction=up") {
			ids := 0
			for _, arg := range args[2:] {
				if !strings.HasPrefix(arg, "-") {
					ids++
				}
			}
			if ids > 1 {
				return nil, errors.New(postgresRelationRefusal)
			}
		}
		return run(dir, name, args...)
	}
}

// TestBdStoreDepListBatchReportsAnUnsupportedBackend pins that a backend
// refusing the batched dep read is reported with ErrDepListBatchUnsupported
// and no map, not as an empty map: an empty map reads to every caller as
// "these anchors have no edges", so close ordering drops its blockers and the
// scope-skip walk sees no dependencies, where the sentinel sends each caller
// to its per-anchor reads.
func TestBdStoreDepListBatchReportsAnUnsupportedBackend(t *testing.T) {
	fake := newCountingFakeBd()
	a := seedFake(t, fake, beads.Bead{Title: "a"})
	b := seedFake(t, fake, beads.Bead{Title: "b"})
	if err := fake.SeedDep(b.ID, a.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	store := beads.NewBdStore("/fake-bd", refuseBatchDepList(fake.run))
	deps, err := store.DepListBatch([]string{a.ID, b.ID})
	if !errors.Is(err, beads.ErrDepListBatchUnsupported) {
		t.Fatalf("DepListBatch on a backend that refuses it = (%v, %v), want ErrDepListBatchUnsupported", deps, err)
	}
	if deps != nil {
		t.Fatalf("DepListBatch returned %v alongside its refusal; a caller reading it sees a graph with no edges", deps)
	}
}

// TestBdStoreDepListBatchOneIDReadsTheRawRecords pins the one-id batch. bd
// picks its dep list shape by how many ids it is handed: one id prints the
// depended-on beads' rows, a join that drops every edge whose target has no
// row, and several print the raw dependency records. A lone anchor is asked
// as a repeated pair, so it gets the records a multi-id batch gives it — the
// edge to a bead no store holds included — in one bd command.
func TestBdStoreDepListBatchOneIDReadsTheRawRecords(t *testing.T) {
	fake := newCountingFakeBd()
	a := seedFake(t, fake, beads.Bead{Title: "a"})
	b := seedFake(t, fake, beads.Bead{Title: "b"})
	const dangling = "discovered-from:gc-404"
	for _, target := range []string{a.ID, dangling} {
		if err := fake.SeedDep(b.ID, target, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	store := fake.store()
	if rows, err := store.DepList(b.ID, "down"); err != nil || len(rows) != 1 {
		t.Fatalf("DepList(%s) = (%v, %v); the fixture's one-id dep list must drop the edge to %s, as bd's does", b.ID, rows, err, dangling)
	}
	fake.calls = nil
	got, err := store.DepListBatch([]string{b.ID})
	if err != nil {
		t.Fatalf("DepListBatch: %v", err)
	}
	want := []beads.Dep{{IssueID: b.ID, DependsOnID: a.ID, Type: "blocks"}, {IssueID: b.ID, DependsOnID: dangling, Type: "blocks"}}
	if len(got) != 1 || !slices.Equal(sortedDeps(got[b.ID]), sortedDeps(want)) {
		t.Fatalf("DepListBatch(%s) = %#v, want the raw records %v", b.ID, got, want)
	}
	if wantCall := "dep list " + b.ID + " " + b.ID + " --json"; len(fake.calls) != 1 || strings.Join(fake.calls[0], " ") != wantCall {
		t.Fatalf("a one-id batch ran %v, want the one command `bd %s`", fake.calls, wantCall)
	}
}

// TestBdStoreDepListBatchKeepsEachRecordOnce: a bd that prints an anchor's
// records once per time the anchor is named (bd before v1.3, and the proxied
// path, which hands the ids to its edge read as typed) answers the repeated
// one-id pair, and a caller's own repeated id, with every record twice. Each
// edge is kept once.
func TestBdStoreDepListBatchKeepsEachRecordOnce(t *testing.T) {
	const twice = `[` +
		`{"issue_id":"gc-2","depends_on_id":"gc-1","type":"blocks"},` +
		`{"issue_id":"gc-2","depends_on_id":"discovered-from:gc-404","type":"blocks"},` +
		`{"issue_id":"gc-2","depends_on_id":"gc-1","type":"blocks"},` +
		`{"issue_id":"gc-2","depends_on_id":"discovered-from:gc-404","type":"blocks"}]`
	store := beads.NewBdStore("/fake-bd", func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(twice), nil
	})
	want := map[string][]beads.Dep{"gc-2": {
		{IssueID: "gc-2", DependsOnID: "gc-1", Type: "blocks"},
		{IssueID: "gc-2", DependsOnID: "discovered-from:gc-404", Type: "blocks"},
	}}
	for _, ids := range [][]string{{"gc-2"}, {"gc-2", "gc-2"}} {
		got, err := store.DepListBatch(ids)
		if err != nil {
			t.Fatalf("DepListBatch(%v): %v", ids, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("DepListBatch(%v) = %#v, want each edge once %#v", ids, got, want)
		}
	}
}

// TestBdStoreDepListBatchRefusesRowsThatAreNotRecords: bd before v1.3 answers a
// multi-id `bd dep list` with the depended-on beads' rows when its batched read
// fails or an anchor does not resolve. Read as records those rows carry no
// anchor, so every edge would be filed under "" and every anchor read as
// edge-free. The batch fails instead.
func TestBdStoreDepListBatchRefusesRowsThatAreNotRecords(t *testing.T) {
	const rows = `[{"id":"gc-1","title":"blocker","status":"open","issue_type":"task","dependency_type":"blocks"}]`
	store := beads.NewBdStore("/fake-bd", func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(rows), nil
	})
	for _, ids := range [][]string{{"gc-2"}, {"gc-2", "gc-3"}} {
		if got, err := store.DepListBatch(ids); err == nil || got != nil {
			t.Errorf("DepListBatch(%v) over issue rows = (%v, %v), want an error and no map", ids, got, err)
		}
	}
}

// TestCachingStoreDepListBatchOverAnUnsupportedBackendFallsBack pins the
// cache's answer over a bd backend that refuses the batch: the sentinel, so the
// caller reads each anchor with DepList, and those reads answer the edges.
func TestCachingStoreDepListBatchOverAnUnsupportedBackendFallsBack(t *testing.T) {
	fake := newCountingFakeBd()
	a := seedFake(t, fake, beads.Bead{Title: "a"})
	b := seedFake(t, fake, beads.Bead{Title: "b"})
	if err := fake.SeedDep(b.ID, a.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	cs := beads.NewCachingStoreForTest(beads.NewBdStore("/fake-bd", refuseBatchDepList(fake.run)), nil)
	deps, err := cs.DepListBatch([]string{a.ID, b.ID})
	if !errors.Is(err, beads.ErrDepListBatchUnsupported) || deps != nil {
		t.Fatalf("cached DepListBatch over a refusing backend = (%v, %v), want (nil, ErrDepListBatchUnsupported)", deps, err)
	}
	single, err := cs.DepList(b.ID, "down")
	if err != nil || len(single) != 1 || single[0].DependsOnID != a.ID {
		t.Fatalf("per-anchor DepList(%s) = (%v, %v), want the blocks edge to %s", b.ID, single, err, a.ID)
	}
}

// storeWithoutDepBatch hides MemStore's DepListBatch.
type storeWithoutDepBatch struct {
	beads.Store
}

func sortedDeps(deps []beads.Dep) []string {
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		out = append(out, d.IssueID+">"+d.DependsOnID+":"+d.Type)
	}
	slices.Sort(out)
	return out
}

// TestCachingStoreCloseAllRefreshesWithOneBatchedRead pins that the post-close
// refresh reads every closed bead back with one exact batched `bd show`, not
// one `bd show` per bead, and still installs each closed row.
func TestCachingStoreCloseAllRefreshesWithOneBatchedRead(t *testing.T) {
	for _, primed := range []bool{false, true} {
		name := "cold cache"
		if primed {
			name = "primed cache"
		}
		t.Run(name, func(t *testing.T) {
			fake := newCountingFakeBd()
			var ids []string
			for range 6 {
				ids = append(ids, seedFake(t, fake, beads.Bead{Title: "step"}).ID)
			}
			cs := beads.NewCachingStoreForTest(fake.store(), nil)
			if primed {
				if err := cs.Prime(context.Background()); err != nil {
					t.Fatalf("Prime: %v", err)
				}
			}
			fake.calls = nil
			n, err := cs.CloseAll(ids, map[string]string{"close_reason": "canceled by the batched refresh test"})
			if err != nil || n != len(ids) {
				t.Fatalf("CloseAll = (%d, %v), want (%d, nil)", n, err, len(ids))
			}
			closeAt := slices.IndexFunc(fake.calls, func(call []string) bool { return call[0] == "close" })
			if closeAt < 0 {
				t.Fatalf("CloseAll ran no bd close: %v", fake.calls)
			}
			var refreshes []string
			for _, call := range fake.calls[closeAt+1:] {
				refreshes = append(refreshes, strings.Join(call, " "))
			}
			if want := "show --json " + strings.Join(ids, " "); len(refreshes) != 1 || refreshes[0] != want {
				t.Fatalf("refresh after close = %q, want one batched %q", refreshes, want)
			}
			fake.calls = nil
			for _, id := range ids {
				got, err := cs.Get(id)
				if err != nil || got.Status != "closed" {
					t.Fatalf("Get(%s) = (%s, %v), want the closed row", id, got.Status, err)
				}
			}
			if primed && len(fake.calls) != 0 {
				t.Fatalf("a primed cache re-read %d closed rows from bd after CloseAll installed them: %v", len(fake.calls), fake.calls)
			}
		})
	}
}
