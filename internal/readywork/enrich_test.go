package readywork

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fanout/fanouttest"
)

// depGatedStore parks every DepList on probe, so a synctest bubble can count
// how many rows' edges are being read at once.
type depGatedStore struct {
	*beads.MemStore
	probe *fanouttest.Probe
}

func (s depGatedStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.probe.Call(0)
	return s.MemStore.DepList(id, direction)
}

// inProgressWithBlockers seeds n in_progress rows, each with one closed and one
// open blocker, in that edge order.
func inProgressWithBlockers(t *testing.T, store *beads.MemStore, n int) {
	t.Helper()
	inProgress := StatusInProgress
	for i := range n {
		row := mustCreate(t, store, beads.Bead{Title: fmt.Sprintf("row %d", i), Type: "task"})
		if err := store.Update(row.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("claim %s: %v", row.ID, err)
		}
		closed := mustCreate(t, store, beads.Bead{Title: fmt.Sprintf("closed blocker %d", i), Type: "task"})
		if err := store.Close(closed.ID); err != nil {
			t.Fatalf("close %s: %v", closed.ID, err)
		}
		open := mustCreate(t, store, beads.Bead{Title: fmt.Sprintf("open blocker %d", i), Type: "task"})
		for _, blocker := range []string{closed.ID, open.ID} {
			if err := store.DepAdd(row.ID, blocker, "blocks"); err != nil {
				t.Fatalf("dep %s -> %s: %v", row.ID, blocker, err)
			}
		}
	}
}

// TestInProgressEnrichmentReadsRowsConcurrently pins ga-ylpbsy.3.4: the
// crash-recovery arm's blocked_by enrichment reads its rows' edges at once
// rather than one row after another, so an unbounded in_progress read costs the
// slowest row rather than the sum of them.
func TestInProgressEnrichmentReadsRowsConcurrently(t *testing.T) {
	const rows = 5
	store := beads.NewMemStore()
	inProgressWithBlockers(t, store, rows)
	synctest.Test(t, func(t *testing.T) {
		probe := fanouttest.NewProbe()
		defer probe.Open()
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err = Read([]Leg{testLeg("city", depGatedStore{MemStore: store, probe: probe})}, Query{Status: StatusInProgress})
		}()
		synctest.Wait()
		if got := probe.Running(); got != rows {
			t.Fatalf("%d of %d rows' edges were being read at once, want all of them", got, rows)
		}
		probe.Open()
		<-done
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	})
}

// TestInProgressEnrichmentKeepsEachRowsBlockersInEdgeOrder pins that the
// concurrent enrichment answers every row exactly as the per-row walk did: its
// blockers, each with its status, in the order the row's own edge read returns
// them.
func TestInProgressEnrichmentKeepsEachRowsBlockersInEdgeOrder(t *testing.T) {
	store := beads.NewMemStore()
	inProgressWithBlockers(t, store, 3)
	got, err := Read([]Leg{testLeg("city", store)}, Query{Status: StatusInProgress})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Read = %d rows, want 3", len(got))
	}
	for _, row := range got {
		deps, err := store.DepList(row.ID, "down")
		if err != nil {
			t.Fatalf("DepList: %v", err)
		}
		var want []ReadyBeadBlocker
		for _, dep := range deps {
			blocker, err := store.Get(dep.DependsOnID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			want = append(want, ReadyBeadBlocker{ID: blocker.ID, Status: blocker.Status})
		}
		if !reflect.DeepEqual(row.BlockedBy, want) {
			t.Fatalf("row %s blocked_by = %+v, want %+v", row.ID, row.BlockedBy, want)
		}
	}
}

// cancellingListStore ends its read's context as its list returns, and counts
// every dependency and blocker read made through it.
type cancellingListStore struct {
	*beads.MemStore
	cancel context.CancelFunc
	reads  *atomic.Int64
}

func (s cancellingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	defer s.cancel()
	return s.MemStore.List(query)
}

func (s cancellingListStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.reads.Add(1)
	return s.MemStore.DepList(id, direction)
}

func (s cancellingListStore) Get(id string) (beads.Bead, error) {
	s.reads.Add(1)
	return s.MemStore.Get(id)
}

// TestInProgressEnrichmentIsNotReadOnceTheContextEnds pins cancellation between
// the phases: a context that ends while the candidates are being read stops the
// read before any row's blockers are, and the answer is the context's error.
func TestInProgressEnrichmentIsNotReadOnceTheContextEnds(t *testing.T) {
	store := beads.NewMemStore()
	inProgressWithBlockers(t, store, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reads atomic.Int64
	leg := testLeg("city", cancellingListStore{MemStore: store, cancel: cancel, reads: &reads})
	rows, err := ReadContext(ctx, []Leg{leg}, Query{Status: StatusInProgress})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadContext = %v, %v; want the context's error once it ended", rows, err)
	}
	if got := reads.Load(); got != 0 {
		t.Fatalf("%d dependency or blocker reads after the context ended, want none", got)
	}
}

// failingBlockerStore fails Get for one blocker id.
type failingBlockerStore struct {
	*beads.MemStore
	failGet string
}

func (s failingBlockerStore) Get(id string) (beads.Bead, error) {
	if id == s.failGet {
		return beads.Bead{}, errors.New("blocker read refused")
	}
	return s.MemStore.Get(id)
}

// TestInProgressEnrichmentFailureNamesTheRowAndBlocker pins fail-loud: a blocker
// the serving leg cannot resolve fails the answer, naming the leg, the blocker
// and the row, exactly as the per-row walk named them.
func TestInProgressEnrichmentFailureNamesTheRowAndBlocker(t *testing.T) {
	store := beads.NewMemStore()
	inProgressWithBlockers(t, store, 2)
	rows, err := Read([]Leg{testLeg("city", store)}, Query{Status: StatusInProgress})
	if err != nil || len(rows) == 0 || len(rows[0].BlockedBy) == 0 {
		t.Fatalf("fixture read = %+v, %v", rows, err)
	}
	failing := rows[0].BlockedBy[0].ID
	_, err = Read([]Leg{testLeg("city", failingBlockerStore{MemStore: store, failGet: failing})}, Query{Status: StatusInProgress})
	if err == nil {
		t.Fatal("an unresolvable blocker produced an answer; a gated bead must not read as unblocked")
	}
	want := fmt.Sprintf("city store: resolving blocker %q of %q", failing, rows[0].ID)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// crossStoreBlocker is a blocking edge's target no leg holds: the edge a split
// topology's strict residence forbids, which the serving leg cannot resolve.
const crossStoreBlocker = "gr-elsewhere"

// inProgressRow creates an in_progress row at priority p, so the canonical ready
// order is the order of p, with a blocking edge to each of blockers.
func inProgressRow(t *testing.T, store beads.Store, title string, p int, blockers ...string) beads.Bead {
	t.Helper()
	inProgress := StatusInProgress
	row := mustCreate(t, store, beads.Bead{Title: title, Type: "task", Priority: intRef(p)})
	if err := store.Update(row.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("claim %s: %v", row.ID, err)
	}
	for _, blocker := range blockers {
		if err := store.DepAdd(row.ID, blocker, "blocks"); err != nil {
			t.Fatalf("dep %s -> %s: %v", row.ID, blocker, err)
		}
	}
	return row
}

// openGate creates an open bead to block on, and closedGate a closed one.
func openGate(t *testing.T, store beads.Store) string {
	t.Helper()
	return mustCreate(t, store, beads.Bead{Title: "open gate", Type: "task"}).ID
}

func closedGate(t *testing.T, store beads.Store) string {
	t.Helper()
	gate := mustCreate(t, store, beads.Bead{Title: "closed gate", Type: "task"})
	if err := store.Close(gate.ID); err != nil {
		t.Fatalf("close %s: %v", gate.ID, err)
	}
	return gate.ID
}

// inProgressWindow is the crash-recovery tier's window of limit rows.
func inProgressWindow(limit int) Query {
	return Query{Status: StatusInProgress, Limit: limit}
}

// readUntilUnblocked is the crash-recovery tier's read: its window, ended at the
// first unblocked row.
func readUntilUnblocked(legs []Leg, limit int) ([]ReadyBead, error) {
	return ReadUntilUnblocked(context.Background(), legs, inProgressWindow(limit))
}

func rowIDs(rows []ReadyBead) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestUntilUnblockedServesTheLeadingClaimPastAnUnresolvableRow pins that a row
// after the cut cannot fail the read. The crash-recovery tier serves the first
// unblocked row of its window, so a row behind it whose blocker no leg can
// resolve is a row the tier never serves: failing the whole read on it hid the
// session's resumable claim and failed its hook. The plain read is the control:
// it resolves every row it returns, and still fails.
func TestUntilUnblockedServesTheLeadingClaimPastAnUnresolvableRow(t *testing.T) {
	store := beads.NewMemStore()
	leading := inProgressRow(t, store, "resumable claim", 0)
	inProgressRow(t, store, "claim with a cross-store blocker", 1, crossStoreBlocker)
	legs := []Leg{testLeg("city", store)}

	rows, err := readUntilUnblocked(legs, 20)
	if err != nil {
		t.Fatalf("Read(--until-unblocked): %v; a row past the cut must not fail the read", err)
	}
	if got := rowIDs(rows); !slices.Equal(got, []string{leading.ID}) {
		t.Fatalf("Read(--until-unblocked) = %v, want only the leading claim %s", got, leading.ID)
	}
	if _, err := Read(legs, inProgressWindow(20)); err == nil {
		t.Fatal("the plain read answered past an unresolvable row; it resolves every row it returns and must fail loud")
	}
}

// TestUntilUnblockedFailsWhenARowAtOrAheadOfTheCutCannotBeResolved pins the
// fail-loud half. The cut cannot be placed past a row whose blockers are
// unknown, because that row may be the one to serve: answering without it would
// hand the session fresh work while a claim it may be able to resume is
// invisible. A gated row ahead of it does not change that.
func TestUntilUnblockedFailsWhenARowAtOrAheadOfTheCutCannotBeResolved(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ahead   int
		rowsFor func(t *testing.T, store beads.Store) string
	}{
		{"leading row", 0, func(t *testing.T, store beads.Store) string {
			bad := inProgressRow(t, store, "claim with a cross-store blocker", 0, crossStoreBlocker)
			inProgressRow(t, store, "resumable claim", 1)
			return bad.ID
		}},
		{"behind a gated row", 1, func(t *testing.T, store beads.Store) string {
			inProgressRow(t, store, "gated claim", 0, openGate(t, store))
			bad := inProgressRow(t, store, "claim with a cross-store blocker", 1, crossStoreBlocker)
			inProgressRow(t, store, "resumable claim", 2)
			return bad.ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			bad := tc.rowsFor(t, store)
			rows, err := readUntilUnblocked([]Leg{testLeg("city", store)}, 20)
			if err == nil {
				t.Fatalf("Read(--until-unblocked) = %v, want a failure: row %s, which the cut cannot pass, has an unresolvable blocker", rowIDs(rows), bad)
			}
			want := fmt.Sprintf("city store: resolving blocker %q of %q", crossStoreBlocker, bad)
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to contain %q", err, want)
			}
		})
	}
}

// TestUntilUnblockedKeepsTheGatedRowsAheadOfTheCut pins the shape of the answer:
// the gated rows ahead of the first unblocked row, each with its blockers, then
// that row, whose blockers (closed ones) are carried too. Nothing past it is
// returned, an unresolvable row included.
func TestUntilUnblockedKeepsTheGatedRowsAheadOfTheCut(t *testing.T) {
	store := beads.NewMemStore()
	gate := openGate(t, store)
	done := closedGate(t, store)
	gated := inProgressRow(t, store, "gated claim", 0, gate)
	resumable := inProgressRow(t, store, "claim whose blocker closed", 1, done)
	inProgressRow(t, store, "claim with a cross-store blocker", 2, crossStoreBlocker)
	inProgressRow(t, store, "later claim", 3)

	rows, err := readUntilUnblocked([]Leg{testLeg("city", store)}, 20)
	if err != nil {
		t.Fatalf("Read(--until-unblocked): %v", err)
	}
	if got := rowIDs(rows); !slices.Equal(got, []string{gated.ID, resumable.ID}) {
		t.Fatalf("Read(--until-unblocked) = %v, want the gated claim then the first unblocked one", got)
	}
	if want := []ReadyBeadBlocker{{ID: gate, Status: "open"}}; !reflect.DeepEqual(rows[0].BlockedBy, want) {
		t.Fatalf("gated row blocked_by = %+v, want %+v", rows[0].BlockedBy, want)
	}
	if want := []ReadyBeadBlocker{{ID: done, Status: "closed"}}; !reflect.DeepEqual(rows[1].BlockedBy, want) {
		t.Fatalf("cut row blocked_by = %+v, want %+v", rows[1].BlockedBy, want)
	}
}

// TestUntilUnblockedWithNoUnblockedRowAnswersTheWholeWindow pins the other end:
// a window whose every row is gated is answered in full, bounded by --limit, so
// the tier sees it has nothing to resume and falls through.
func TestUntilUnblockedWithNoUnblockedRowAnswersTheWholeWindow(t *testing.T) {
	store := beads.NewMemStore()
	gate := openGate(t, store)
	first := inProgressRow(t, store, "gated claim 1", 0, gate)
	second := inProgressRow(t, store, "gated claim 2", 1, gate)
	inProgressRow(t, store, "past the window", 2)

	rows, err := readUntilUnblocked([]Leg{testLeg("city", store)}, 2)
	if err != nil {
		t.Fatalf("Read(--until-unblocked --limit 2): %v", err)
	}
	if got := rowIDs(rows); !slices.Equal(got, []string{first.ID, second.ID}) {
		t.Fatalf("Read(--until-unblocked --limit 2) = %v, want the two gated rows of the window", got)
	}
}

// failingDepListStore fails DepList for one row.
type failingDepListStore struct {
	*beads.MemStore
	failDepList string
}

func (s failingDepListStore) DepList(id, direction string) ([]beads.Dep, error) {
	if id == s.failDepList {
		return nil, errors.New("dependency read refused")
	}
	return s.MemStore.DepList(id, direction)
}

// TestUntilUnblockedScopesADependencyReadFailureToTheCut pins the same scoping
// for the other enrichment failure, a row whose edges cannot be read at all.
func TestUntilUnblockedScopesADependencyReadFailureToTheCut(t *testing.T) {
	store := beads.NewMemStore()
	first := inProgressRow(t, store, "first claim", 0)
	second := inProgressRow(t, store, "second claim", 1)

	rows, err := readUntilUnblocked([]Leg{testLeg("city", failingDepListStore{MemStore: store, failDepList: second.ID})}, 20)
	if err != nil {
		t.Fatalf("Read(--until-unblocked) with the second row's edges unreadable: %v", err)
	}
	if got := rowIDs(rows); !slices.Equal(got, []string{first.ID}) {
		t.Fatalf("Read(--until-unblocked) = %v, want only %s", got, first.ID)
	}
	_, err = readUntilUnblocked([]Leg{testLeg("city", failingDepListStore{MemStore: store, failDepList: first.ID})}, 20)
	want := fmt.Sprintf("city store: listing dependencies of %q", first.ID)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Read(--until-unblocked) with the leading row's edges unreadable = %v, want an error containing %q", err, want)
	}
}

// countingDepListStore counts the rows whose edges are read.
type countingDepListStore struct {
	*beads.MemStore
	reads *atomic.Int64
}

func (s countingDepListStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.reads.Add(1)
	return s.MemStore.DepList(id, direction)
}

// TestUntilUnblockedReadsNoBatchPastTheCut pins the cost: blockers are resolved
// a batch of LegFanOut rows at a time, in answer order, and no batch after the
// one holding the cut is read. A session whose first claim is resumable pays
// one batch, not the whole window.
func TestUntilUnblockedReadsNoBatchPastTheCut(t *testing.T) {
	store := beads.NewMemStore()
	for i := range 3 * LegFanOut {
		inProgressRow(t, store, fmt.Sprintf("claim %d", i), i)
	}
	var reads atomic.Int64
	rows, err := readUntilUnblocked([]Leg{testLeg("city", countingDepListStore{MemStore: store, reads: &reads})}, 3*LegFanOut)
	if err != nil {
		t.Fatalf("Read(--until-unblocked): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Read(--until-unblocked) = %v, want the first claim alone", rowIDs(rows))
	}
	if got := reads.Load(); got != LegFanOut {
		t.Fatalf("%d rows' edges were read, want the first batch of %d and nothing past it", got, LegFanOut)
	}
}

// listRefusingStore fails every List, so a read that reaches a store says so.
type listRefusingStore struct {
	*beads.MemStore
}

func (listRefusingStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list reached")
}

// TestUntilUnblockedIsRefusedOffTheInProgressArm pins that the cut is refused
// where no row carries its blockers, before any store is read: on every other
// arm it would be a flag that looks honored and selects nothing.
func TestUntilUnblockedIsRefusedOffTheInProgressArm(t *testing.T) {
	legs := []Leg{testLeg("city", listRefusingStore{MemStore: beads.NewMemStore()})}
	for _, status := range []string{"", StatusOpen, StatusBlocked, StatusClosed} {
		q := Query{Status: status}
		if err := ValidateUntilUnblocked(q); err == nil || !strings.Contains(err.Error(), "--until-unblocked") {
			t.Errorf("ValidateUntilUnblocked(--status %q) = %v, want a refusal naming the flag", status, err)
		}
		if _, err := ReadUntilUnblocked(context.Background(), legs, q); err == nil || !strings.Contains(err.Error(), "--until-unblocked") {
			t.Errorf("ReadUntilUnblocked(--status %q) = %v, want the refusal before any store is read", status, err)
		}
	}
	if err := ValidateUntilUnblocked(inProgressWindow(20)); err != nil {
		t.Fatalf("ValidateUntilUnblocked(--status in_progress) = %v", err)
	}
	if err := ValidateUntilUnblocked(Query{Status: StatusInProgress, Limit: -1}); err == nil || !strings.Contains(err.Error(), "--limit -1") {
		t.Fatalf("ValidateUntilUnblocked(--limit -1) = %v, want the query's own refusal", err)
	}
}

// TestCutUntilUnblockedEndsAResolvedAnswerAtItsFirstUnblockedRow pins the cut
// the CLI makes of the controller's answer: the first row whose every blocker is
// closed, or that has none, ends it; an answer with no such row is kept whole.
func TestCutUntilUnblockedEndsAResolvedAnswerAtItsFirstUnblockedRow(t *testing.T) {
	gated := ReadyBead{ID: "gated", BlockedBy: []ReadyBeadBlocker{{ID: "gate", Status: "open"}, {ID: "done", Status: "closed"}}}
	inProgressGate := ReadyBead{ID: "gated-in-progress", BlockedBy: []ReadyBeadBlocker{{ID: "gate", Status: "in_progress"}}}
	closedOnly := ReadyBead{ID: "closed-only", BlockedBy: []ReadyBeadBlocker{{ID: "done", Status: "CLOSED"}}}
	free := ReadyBead{ID: "free"}
	for _, tc := range []struct {
		name string
		rows []ReadyBead
		want []string
	}{
		{"leading unblocked row", []ReadyBead{free, gated}, []string{"free"}},
		{"gated rows ahead", []ReadyBead{gated, inProgressGate, closedOnly, free}, []string{"gated", "gated-in-progress", "closed-only"}},
		{"no unblocked row", []ReadyBead{gated, inProgressGate}, []string{"gated", "gated-in-progress"}},
		{"empty answer", nil, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowIDs(CutUntilUnblocked(tc.rows)); !slices.Equal(got, tc.want) {
				t.Fatalf("CutUntilUnblocked = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCutOfTheWholeWindowIsTheUntilUnblockedRead pins the property the CLI's
// controller route rests on: when every row of the window resolves, cutting the
// whole window's answer gives exactly the rows, and the blockers, that
// ReadUntilUnblocked reads in-process.
func TestCutOfTheWholeWindowIsTheUntilUnblockedRead(t *testing.T) {
	store := beads.NewMemStore()
	gate := openGate(t, store)
	done := closedGate(t, store)
	inProgressRow(t, store, "gated claim", 0, gate)
	inProgressRow(t, store, "second gated claim", 1, gate, done)
	inProgressRow(t, store, "claim whose blocker closed", 2, done)
	inProgressRow(t, store, "later claim", 3)
	legs := []Leg{testLeg("city", store)}

	whole, err := Read(legs, inProgressWindow(20))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got, err := readUntilUnblocked(legs, 20)
	if err != nil {
		t.Fatalf("ReadUntilUnblocked: %v", err)
	}
	if want := CutUntilUnblocked(whole); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadUntilUnblocked = %+v\nwant the cut of the whole window %+v", got, want)
	}
	if len(got) != 3 {
		t.Fatalf("ReadUntilUnblocked = %v, want the two gated claims and the first unblocked one", rowIDs(got))
	}
}
