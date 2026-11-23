package beadstest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// RunGuardedBatchConformance asserts the beads.GuardedBatcher contract against
// the batcher beads.GuardedBatcherFor resolves for the store open returns:
// every batch lands whole or not at all, guards and pinned creates refuse the
// whole batch, returned revisions are the ones later reads report, and a batch
// validation refuses writes nothing.
//
// Reads go through the store open returned, not through the batcher, so a
// wrapper that routes reads and writes to different places is held to the
// same answers.
func RunGuardedBatchConformance(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Helper()
	for _, leg := range []struct {
		name string
		run  func(t *testing.T, f *guardedFixture)
	}{
		{"guard_miss_at_item_k_persists_nothing", guardedGuardMissPersistsNothing},
		{"pinned_create_over_existing_row_refuses", guardedPinnedCreateOverRowRefuses},
		{"update_of_missing_row_refuses", guardedUpdateOfMissingRowRefuses},
		{"labeled_update_of_missing_row_refuses", guardedLabeledUpdateOfMissingRowRefuses},
		{"returned_revisions_equal_later_gets", guardedRevisionsEqualLaterGets},
		{"concurrent_batches_on_one_row_have_one_winner", guardedConcurrentBatchesHaveOneWinner},
		{"fields_apply", guardedFieldsApply},
		{"no_history_and_ephemeral_per_item", guardedStorageTierPerItem},
		{"closed_no_history_pinned_create", guardedClosedNoHistoryPinnedCreate},
		{"invalid_batches_refused_before_any_write", guardedInvalidBatchesRefused},
	} {
		t.Run(name+"/"+leg.name, func(t *testing.T) { leg.run(t, newGuardedFixture(t, open)) })
	}
}

// guardedGuardMissPersistsNothing: a stale guard on one item refuses the
// whole batch, items before it included.
func guardedGuardMissPersistsNothing(t *testing.T, f *guardedFixture) {
	a, b, c := f.seed("a"), f.seed("b"), f.seed("c")
	stale := b.Revision + 1000
	pinned := f.pin("miss")
	_, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: pinned, Title: "would be created"}},
		{Update: &beads.GuardedUpdate{ID: a.ID, ExpectedRevision: &a.Revision, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "a"}}}},
		{Update: &beads.GuardedUpdate{ID: b.ID, ExpectedRevision: &stale, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "b"}}}},
		{Update: &beads.GuardedUpdate{ID: c.ID, ExpectedRevision: &c.Revision, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "c"}}}},
	}})
	if !beads.IsPreconditionFailed(err) {
		t.Fatalf("ApplyGuarded with a stale guard at item 2 = %v, want a precondition failure", err)
	}
	f.wantItemError(err, 2, b.ID)
	f.wantAbsent(pinned)
	f.wantUnchanged(a, b, c)
}

// guardedPinnedCreateOverRowRefuses: a pinned create is create-only.
func guardedPinnedCreateOverRowRefuses(t *testing.T, f *guardedFixture) {
	existing, other := f.seed("existing"), f.seed("other")
	_, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Update: &beads.GuardedUpdate{ID: other.ID, ExpectedRevision: &other.Revision, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "v"}}}},
		{Create: &beads.Bead{ID: existing.ID, Title: "overwrite attempt", Metadata: map[string]string{"k": "clobbered"}}},
	}})
	if !errors.Is(err, beads.ErrAlreadyExists) {
		t.Fatalf("ApplyGuarded creating over %s = %v, want ErrAlreadyExists", existing.ID, err)
	}
	f.wantItemError(err, 1, existing.ID)
	f.wantUnchanged(existing, other)
}

// guardedUpdateOfMissingRowRefuses: an update of a row that does not exist
// refuses the batch as that item's ErrNotFound.
func guardedUpdateOfMissingRowRefuses(t *testing.T, f *guardedFixture) {
	pinned, missing := f.pin("created"), f.pin("missing")
	_, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: pinned, Title: "would be created"}},
		{Update: &beads.GuardedUpdate{ID: missing, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "v"}}}},
	}})
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("ApplyGuarded updating missing %s = %v, want ErrNotFound", missing, err)
	}
	f.wantItemError(err, 1, missing)
	f.wantAbsent(pinned, missing)
}

// guardedLabeledUpdateOfMissingRowRefuses is the same refusal for an update
// that carries labels, which a backend may read before the batch for the label
// revision the edit advances: that read finding no row still refuses as the
// item's ErrNotFound.
func guardedLabeledUpdateOfMissingRowRefuses(t *testing.T, f *guardedFixture) {
	pinned, missing := f.pin("created"), f.pin("missing")
	_, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: pinned, Title: "would be created"}},
		{Update: &beads.GuardedUpdate{ID: missing, Opts: beads.UpdateOpts{Labels: []string{"added"}}}},
	}})
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("ApplyGuarded labeling missing %s = %v, want ErrNotFound", missing, err)
	}
	f.wantItemError(err, 1, missing)
	f.wantAbsent(pinned, missing)
}

// guardedRevisionsEqualLaterGets: the revisions a batch returns are the ones
// later reads report, and each is a usable guard exactly once.
func guardedRevisionsEqualLaterGets(t *testing.T, f *guardedFixture) {
	guarded, unguarded := f.seed("guarded"), f.seed("unguarded")
	pinned := f.pin("fresh")
	res, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{
		Reason: "conformance: revisions",
		Items: []beads.GuardedItem{
			{Create: &beads.Bead{ID: pinned, Title: "fresh row"}},
			{Update: &beads.GuardedUpdate{ID: guarded.ID, ExpectedRevision: &guarded.Revision, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "v"}}}},
			{Update: &beads.GuardedUpdate{ID: unguarded.ID, Opts: beads.UpdateOpts{Title: guardedPtr("retitled")}}},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	if len(res.Revisions) != 3 {
		t.Fatalf("Revisions = %v, want one per item", res.Revisions)
	}
	for id, rev := range res.Revisions {
		if got := f.get(id).Revision; got != rev {
			t.Errorf("Revisions[%s] = %d, later Get reports %d", id, rev, got)
		}
	}
	if res.Revisions[guarded.ID] == guarded.Revision {
		t.Errorf("guarded update left revision %d unchanged", guarded.Revision)
	}
	if res.Revisions[unguarded.ID] == unguarded.Revision {
		t.Errorf("unguarded update left revision %d unchanged", unguarded.Revision)
	}
	// The revision a batch returns is a usable guard for the next one.
	next := res.Revisions[guarded.ID]
	if _, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Update: &beads.GuardedUpdate{ID: guarded.ID, ExpectedRevision: &next, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "again"}}}},
	}}); err != nil {
		t.Fatalf("ApplyGuarded guarded on the returned revision: %v", err)
	}
	// And the revision before it no longer is.
	if _, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Update: &beads.GuardedUpdate{ID: guarded.ID, ExpectedRevision: &next, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "stale"}}}},
	}}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("ApplyGuarded on a consumed revision = %v, want a precondition failure", err)
	}
}

// guardedConcurrentBatchesHaveOneWinner: batches racing on one guarded row
// land exactly one, and the losers write none of their items.
func guardedConcurrentBatchesHaveOneWinner(t *testing.T, f *guardedFixture) {
	row := f.seed("contended")
	const racers = 32
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
				{Create: &beads.Bead{ID: f.pin(fmt.Sprintf("racer%d", i)), Title: "racer"}},
				{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Metadata: map[string]string{"winner": fmt.Sprint(i)}}}},
			}})
		}()
	}
	close(start)
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatalf("racers %d and %d both won a batch guarded on revision %d", winner, i, row.Revision)
			}
			winner = i
		case !beads.IsPreconditionFailed(err):
			t.Fatalf("racer %d lost with %v, want a precondition failure", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no racer won")
	}
	if got := f.get(row.ID).Metadata["winner"]; got != fmt.Sprint(winner) {
		t.Fatalf("row records winner %q, want %d", got, winner)
	}
	for i := range racers {
		id := f.pin(fmt.Sprintf("racer%d", i))
		if i == winner {
			f.get(id)
			continue
		}
		f.wantAbsent(id)
	}
}

// guardedFieldsApply: every field an item carries lands, except the
// CloseReason of a create left open, which records none.
func guardedFieldsApply(t *testing.T, f *guardedFixture) {
	patched := f.seedWith(beads.Bead{Title: "patched", Labels: []string{"keep", "drop"}, Metadata: map[string]string{"old": "1"}})
	closing := f.seed("closing")
	pinned := f.pin("full")
	_, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Update: &beads.GuardedUpdate{ID: patched.ID, ExpectedRevision: &patched.Revision, Opts: beads.UpdateOpts{
			Title:        guardedPtr("retitled"),
			Assignee:     guardedPtr("worker-1"),
			Labels:       []string{"added"},
			RemoveLabels: []string{"drop"},
			Metadata:     map[string]string{"new": "2"},
		}}},
		{Update: &beads.GuardedUpdate{ID: closing.ID, ExpectedRevision: &closing.Revision, Opts: beads.UpdateOpts{Status: guardedPtr("closed")}}},
		{Create: &beads.Bead{
			ID:          pinned,
			Title:       "pinned row",
			Type:        "task",
			Assignee:    "worker-2",
			Labels:      []string{"pinned"},
			Metadata:    map[string]string{"fact": "yes"},
			CloseReason: "stale",
		}},
	}})
	if err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	got := f.get(patched.ID)
	if got.Title != "retitled" || got.Assignee != "worker-1" {
		t.Errorf("patched row title=%q assignee=%q, want retitled/worker-1", got.Title, got.Assignee)
	}
	guardedWantLabels(t, got, []string{"keep", "added"}, []string{"drop"})
	if got.Metadata["old"] != "1" || got.Metadata["new"] != "2" {
		t.Errorf("patched row metadata = %v, want old=1 merged with new=2", got.Metadata)
	}
	if got := f.get(closing.ID); got.Status != "closed" {
		t.Errorf("closing row status = %q, want closed", got.Status)
	}
	created := f.get(pinned)
	if created.ID != pinned || created.Title != "pinned row" || created.Assignee != "worker-2" || created.Status != "open" {
		t.Errorf("created row = %+v, want the pinned id, title, assignee and status open", created)
	}
	guardedWantLabels(t, created, []string{"pinned"}, nil)
	if created.Metadata["fact"] != "yes" {
		t.Errorf("created row metadata = %v, want fact=yes", created.Metadata)
	}
	if created.CloseReason != "" {
		t.Errorf("created row CloseReason = %q, want none: an open create records no close reason", created.CloseReason)
	}
}

// guardedStorageTierPerItem: each create keeps its own storage tier.
func guardedStorageTierPerItem(t *testing.T, f *guardedFixture) {
	noHistory, ephemeral, plain := f.pin("nohistory"), f.pin("ephemeral"), f.pin("plain")
	if _, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: noHistory, Title: "no history", NoHistory: true}},
		{Create: &beads.Bead{ID: ephemeral, Title: "ephemeral", Ephemeral: true}},
		{Create: &beads.Bead{ID: plain, Title: "plain"}},
	}}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	for _, tc := range []struct {
		id                   string
		noHistory, ephemeral bool
	}{{noHistory, true, false}, {ephemeral, false, true}, {plain, false, false}} {
		got := f.get(tc.id)
		if got.NoHistory != tc.noHistory || got.Ephemeral != tc.ephemeral {
			t.Errorf("%s: NoHistory=%v Ephemeral=%v, want %v/%v", tc.id, got.NoHistory, got.Ephemeral, tc.noHistory, tc.ephemeral)
		}
	}
}

// guardedClosedNoHistoryPinnedCreate is the fact-row shape: a row written
// already closed, with no history, under a pinned id, carrying its labels and
// metadata, and recording the trimmed metadata.close_reason a close would
// rather than the request's own CloseReason. A second create of the same id
// refuses, so a replayed fact never lands twice.
func guardedClosedNoHistoryPinnedCreate(t *testing.T, f *guardedFixture) {
	fact := f.pin("fact")
	create := beads.GuardedBatch{Items: []beads.GuardedItem{{Create: &beads.Bead{
		ID:          fact,
		Title:       "fact",
		Status:      "closed",
		NoHistory:   true,
		Labels:      []string{"fact"},
		Metadata:    map[string]string{"kind": "fact", "close_reason": "  recorded  "},
		CloseReason: "ignored",
	}}}}
	res, err := f.batcher.ApplyGuarded(context.Background(), create)
	if err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	got := f.get(fact)
	if got.Status != "closed" || !got.NoHistory || got.Ephemeral {
		t.Errorf("fact row status=%q NoHistory=%v Ephemeral=%v, want closed/true/false", got.Status, got.NoHistory, got.Ephemeral)
	}
	guardedWantLabels(t, got, []string{"fact"}, nil)
	if got.Metadata["kind"] != "fact" {
		t.Errorf("fact row metadata = %v, want kind=fact", got.Metadata)
	}
	if got.CloseReason != "recorded" {
		t.Errorf("fact row CloseReason = %q, want the trimmed metadata.close_reason %q, not the request's", got.CloseReason, "recorded")
	}
	if got.Revision != res.Revisions[fact] {
		t.Errorf("fact row revision %d, batch returned %d", got.Revision, res.Revisions[fact])
	}
	_, err = f.batcher.ApplyGuarded(context.Background(), create)
	if !errors.Is(err, beads.ErrAlreadyExists) {
		t.Fatalf("second create of %s = %v, want ErrAlreadyExists", fact, err)
	}
	f.wantItemError(err, 0, fact)
	f.wantUnchanged(got)
}

// guardedInvalidBatchesRefused: a batch refused by validation writes none of
// its items.
func guardedInvalidBatchesRefused(t *testing.T, f *guardedFixture) {
	row := f.seed("row")
	oversized := make([]beads.GuardedItem, 0, beads.MaxGuardedItems+1)
	for i := range beads.MaxGuardedItems + 1 {
		oversized = append(oversized, beads.GuardedItem{Create: &beads.Bead{ID: f.pin(fmt.Sprintf("over%d", i)), Title: "over"}})
	}
	parent := row.ID
	for _, tc := range []struct {
		name  string
		items []beads.GuardedItem
	}{
		{"empty", nil},
		{"oversized", oversized},
		{"duplicate_row", []beads.GuardedItem{
			{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Title: guardedPtr("first")}}},
			{Update: &beads.GuardedUpdate{ID: row.ID, Opts: beads.UpdateOpts{Title: guardedPtr("second")}}},
		}},
		{"create_then_update_same_row", []beads.GuardedItem{
			{Create: &beads.Bead{ID: f.pin("twice"), Title: "twice"}},
			{Update: &beads.GuardedUpdate{ID: f.pin("twice"), Opts: beads.UpdateOpts{Title: guardedPtr("again")}}},
		}},
		{"neither_member", []beads.GuardedItem{{}}},
		{"both_members", []beads.GuardedItem{{
			Create: &beads.Bead{ID: f.pin("both"), Title: "both"},
			Update: &beads.GuardedUpdate{ID: row.ID, Opts: beads.UpdateOpts{Title: guardedPtr("both")}},
		}}},
		{"create_without_id", []beads.GuardedItem{
			{Update: &beads.GuardedUpdate{ID: row.ID, Opts: beads.UpdateOpts{Title: guardedPtr("rides along")}}},
			{Create: &beads.Bead{Title: "no id"}},
		}},
		{"create_ephemeral_and_no_history", []beads.GuardedItem{{Create: &beads.Bead{ID: f.pin("tiers"), Title: "both tiers", Ephemeral: true, NoHistory: true}}}},
		{"create_with_parent", []beads.GuardedItem{{Create: &beads.Bead{ID: f.pin("child"), Title: "child", ParentID: parent}}}},
		{"create_with_needs", []beads.GuardedItem{{Create: &beads.Bead{ID: f.pin("needs"), Title: "needs", Needs: []string{row.ID}}}}},
		{"create_with_dependencies", []beads.GuardedItem{{Create: &beads.Bead{ID: f.pin("deps"), Title: "deps", Dependencies: []beads.Dep{{IssueID: f.pin("deps"), DependsOnID: row.ID, Type: "blocks"}}}}}},
		{"update_with_parent", []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: row.ID, Opts: beads.UpdateOpts{ParentID: &parent}}}}},
		{"empty_update", []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision}}}},
	} {
		_, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: tc.items})
		if !errors.Is(err, beads.ErrInvalidGuardedBatch) {
			t.Errorf("%s: ApplyGuarded = %v, want ErrInvalidGuardedBatch", tc.name, err)
		}
	}
	f.wantUnchanged(row)
	f.wantAbsent(f.pin("over0"), f.pin(fmt.Sprintf("over%d", beads.MaxGuardedItems)), f.pin("twice"), f.pin("both"), f.pin("child"), f.pin("needs"), f.pin("deps"), f.pin("tiers"))
}

// guardedFixture is one fresh store plus its resolved batcher.
type guardedFixture struct {
	t       *testing.T
	store   beads.Store
	batcher beads.GuardedBatcher
	prefix  string
}

func newGuardedFixture(t *testing.T, open func(t *testing.T) beads.Store) *guardedFixture {
	t.Helper()
	store := open(t)
	batcher, ok := beads.GuardedBatcherFor(store)
	if !ok {
		t.Fatalf("GuardedBatcherFor(%T) = false, want a guarded batcher", store)
	}
	f := &guardedFixture{t: t, store: store, batcher: batcher}
	// Pinned ids share the namespace the store mints under, so a store fenced
	// to its own namespace accepts them.
	probe := f.seed("namespace probe")
	cut := strings.LastIndex(probe.ID, "-")
	if cut <= 0 {
		t.Fatalf("minted id %q has no namespace prefix to pin under", probe.ID)
	}
	f.prefix = probe.ID[:cut]
	return f
}

// pin returns a pinned id in the store's namespace that no mint produces.
func (f *guardedFixture) pin(name string) string {
	return f.prefix + "-gb" + name
}

func (f *guardedFixture) seed(title string) beads.Bead {
	return f.seedWith(beads.Bead{Title: title})
}

func (f *guardedFixture) seedWith(b beads.Bead) beads.Bead {
	f.t.Helper()
	created, err := f.store.Create(b)
	if err != nil {
		f.t.Fatalf("Create(%q): %v", b.Title, err)
	}
	return f.get(created.ID)
}

func (f *guardedFixture) get(id string) beads.Bead {
	f.t.Helper()
	got, err := f.store.Get(id)
	if err != nil {
		f.t.Fatalf("Get(%s): %v", id, err)
	}
	return got
}

func (f *guardedFixture) wantAbsent(ids ...string) {
	f.t.Helper()
	for _, id := range ids {
		if got, err := f.store.Get(id); !errors.Is(err, beads.ErrNotFound) {
			f.t.Errorf("Get(%s) = (%+v, %v), want ErrNotFound: a refused batch persisted it", id, got, err)
		}
	}
}

// wantUnchanged compares each row with its earlier read, field for field,
// revision included. Times compare as instants: a store that reloads from disk
// decodes them in UTC.
func (f *guardedFixture) wantUnchanged(before ...beads.Bead) {
	f.t.Helper()
	for _, b := range before {
		if after := f.get(b.ID); !reflect.DeepEqual(guardedInstants(after), guardedInstants(b)) {
			f.t.Errorf("refused batch changed %s:\nbefore %#v\nafter  %#v", b.ID, b, after)
		}
	}
}

func (f *guardedFixture) wantItemError(err error, index int, id string) {
	f.t.Helper()
	var itemErr *beads.BatchItemError
	if !errors.As(err, &itemErr) {
		f.t.Fatalf("error %v is not a *BatchItemError", err)
	}
	if itemErr.Index != index || itemErr.ID != id {
		f.t.Fatalf("BatchItemError names item %d (%s), want item %d (%s)", itemErr.Index, itemErr.ID, index, id)
	}
}

func guardedWantLabels(t *testing.T, b beads.Bead, present, absent []string) {
	t.Helper()
	for _, l := range present {
		if !slices.Contains(b.Labels, l) {
			t.Errorf("%s labels %v lack %q", b.ID, b.Labels, l)
		}
	}
	for _, l := range absent {
		if slices.Contains(b.Labels, l) {
			t.Errorf("%s labels %v still carry %q", b.ID, b.Labels, l)
		}
	}
}

// guardedInstants returns b with its times in UTC.
func guardedInstants(b beads.Bead) beads.Bead {
	b.CreatedAt = b.CreatedAt.UTC()
	b.UpdatedAt = b.UpdatedAt.UTC()
	if b.DeferUntil != nil {
		at := b.DeferUntil.UTC()
		b.DeferUntil = &at
	}
	return b
}

func guardedPtr[T any](v T) *T { return &v }
