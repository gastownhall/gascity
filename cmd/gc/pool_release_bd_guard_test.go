package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// bdGuardExit stands in for the *exec.ExitError the bd runner wraps; BdStore
// reads the exit status through its ExitCode method.
type bdGuardExit struct{ code int }

func (e bdGuardExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (e bdGuardExit) ExitCode() int { return e.code }

// bdGuardLedger is a one-bead fake bd CLI that honors `bd update`'s
// --if-status / --if-assignee guards the way the pinned bd does: the guards
// are checked against the stored row inside the write, an empty assignee
// means unassigned, and a mismatch writes nothing and exits 13. noGuards
// models a bd without the flags, and beforeUpdate lands a concurrent write
// just before a guarded update is evaluated. `bd list` filters on the RAW
// stored status, as bd does, so a row bd records deferred is absent from
// --status=open; a label filter matches nothing, since the row carries none,
// and the ephemeral-tier `bd query` leg is empty.
type bdGuardLedger struct {
	mu           sync.Mutex
	bead         beads.Bead
	noGuards     bool
	beforeUpdate func(*beads.Bead)
	updates      [][]string
}

func (l *bdGuardLedger) run(_, name string, args ...string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	switch {
	case len(args) == 2 && args[1] == "--help":
		// The conditional-writes probe: this bd has no --if-revision.
		return []byte("Usage: bd " + args[0]), nil
	case args[0] == "show":
		return l.render()
	case args[0] == "list":
		return l.list(args[1:])
	case args[0] == "query":
		// The ephemeral-tier leg of a both-tier list: the row is durable.
		return []byte(`[]`), nil
	case args[0] == "update":
		return l.update(args[1:])
	}
	return nil, fmt.Errorf("bd %v: unsupported by the fake", args)
}

func (l *bdGuardLedger) render() ([]byte, error) {
	return json.Marshal([]map[string]any{l.renderRow()})
}

func (l *bdGuardLedger) renderRow() map[string]any {
	row := map[string]any{
		"id":         l.bead.ID,
		"title":      l.bead.Title,
		"status":     l.bead.Status,
		"assignee":   l.bead.Assignee,
		"issue_type": "task",
		"created_at": nowForBDJSONTest(),
		"metadata":   l.bead.Metadata,
	}
	if l.bead.DeferUntil != nil {
		row["defer_until"] = l.bead.DeferUntil.UTC().Format(time.RFC3339)
	}
	return row
}

func (l *bdGuardLedger) list(args []string) ([]byte, error) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--label") {
			return []byte(`[]`), nil
		}
		if statuses, ok := strings.CutPrefix(arg, "--status="); ok && !slices.Contains(strings.Split(statuses, ","), l.bead.Status) {
			return []byte(`[]`), nil
		}
	}
	return json.Marshal([]map[string]any{l.renderRow()})
}

func (l *bdGuardLedger) update(args []string) ([]byte, error) {
	l.updates = append(l.updates, append([]string(nil), args...))
	if l.beforeUpdate != nil && hasBdFlag(args, "--if-assignee") {
		l.beforeUpdate(&l.bead)
		l.beforeUpdate = nil
	}
	next := l.bead
	next.Metadata = make(map[string]string, len(l.bead.Metadata))
	for k, v := range l.bead.Metadata {
		next.Metadata[k] = v
	}
	var ifStatus, ifAssignee *string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json", l.bead.ID:
		case "--if-status", "--if-assignee":
			if l.noGuards {
				return []byte("Error: unknown flag: " + args[i]), bdGuardExit{code: 1}
			}
			i++
			value := args[i]
			if args[i-1] == "--if-status" {
				ifStatus = &value
			} else {
				ifAssignee = &value
			}
		case "--status":
			i++
			next.Status = args[i]
		case "--assignee":
			i++
			next.Assignee = args[i]
		case "--set-metadata":
			i++
			key, value, _ := strings.Cut(args[i], "=")
			next.Metadata[key] = value
		default:
			return nil, fmt.Errorf("bd update: unsupported arg %q in %v", args[i], args)
		}
	}
	if (ifStatus != nil && *ifStatus != l.bead.Status) || (ifAssignee != nil && *ifAssignee != l.bead.Assignee) {
		return []byte("Error: 1 of 1 issues failed to update"), bdGuardExit{code: 13}
	}
	l.bead = next
	return []byte(`{}`), nil
}

// hasBdFlag reports whether args carries flag, whatever its value.
func hasBdFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func (l *bdGuardLedger) row() beads.Bead {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bead
}

func newBdGuardLedger(status, assignee string, extra map[string]string) *bdGuardLedger {
	metadata := map[string]string{
		"gc.routed_to":        "worker",
		"gc.session_affinity": "require",
	}
	for k, v := range extra {
		metadata[k] = v
	}
	return &bdGuardLedger{bead: beads.Bead{
		ID:       "bd-7",
		Title:    "orphaned pool work",
		Status:   status,
		Assignee: assignee,
		Metadata: metadata,
	}}
}

// TestReleaseOrphanedPoolAssignment_BdStoreGuardedRelease covers decision (c)
// for the bd-backed city. bd has no --if-revision, so the revision-fenced
// release is unavailable there. Each snapshot shape ReleaseIfCurrent cannot
// take is released as ONE `bd update` that carries the snapshot's status and
// assignee as server-side guards, and also clears the affinity metadata.
func TestReleaseOrphanedPoolAssignment_BdStoreGuardedRelease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		assignee string
		extra    map[string]string
		wrap     bool
	}{
		{name: "open-status strand", status: "open", assignee: "worker-dead"},
		{name: "assignee-less in_progress", status: "in_progress", assignee: ""},
		{name: "continuation group", status: "in_progress", assignee: "worker-dead", extra: map[string]string{beadmeta.ContinuationGroupMetadataKey: "grp-1"}},
		{name: "through a resolve-target wrapper", status: "open", assignee: "worker-dead", wrap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newBdGuardLedger(tc.status, tc.assignee, tc.extra)
			var store beads.Store = beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
			if tc.wrap {
				// The typed class wrappers and the cmd/gc policy store declare
				// their target rather than promote the capability.
				store = beads.WorkStore{Store: store}
			}
			wb := ledger.row()

			if !releaseOrphanedPoolAssignment(store, wb, false) {
				t.Fatalf("release = false, want the guarded release to land; bd updates %v", ledger.updates)
			}
			if len(ledger.updates) != 1 {
				t.Fatalf("bd updates = %v, want exactly one guarded write", ledger.updates)
			}
			update := ledger.updates[0]
			if got := testFlagValue(update, "--if-status"); got != tc.status {
				t.Errorf("--if-status = %q, want the snapshot's %q", got, tc.status)
			}
			if got := testFlagValue(update, "--if-assignee"); !hasBdFlag(update, "--if-assignee") || got != tc.assignee {
				t.Errorf("--if-assignee = %q, want the snapshot's %q", got, tc.assignee)
			}
			got := ledger.row()
			if got.Status != "open" || got.Assignee != "" {
				t.Fatalf("row = status %q assignee %q, want open and unassigned", got.Status, got.Assignee)
			}
			for key, want := range clearedSessionAffinityMetadata() {
				if got.Metadata[key] != want {
					t.Errorf("metadata[%q] = %q, want it cleared in the same write", key, got.Metadata[key])
				}
			}
		})
	}
}

// TestReleaseOrphanedPoolAssignment_BdStoreGuardLostToAClaim is the exit-13
// path: a fresh worker claims the bead after the sweep's snapshot. bd checks
// the guard against the claimed row, writes nothing and exits 13, and the
// release skips the bead for the next sweep instead of clobbering the claim.
func TestReleaseOrphanedPoolAssignment_BdStoreGuardLostToAClaim(t *testing.T) {
	ledger := newBdGuardLedger("in_progress", "worker-dead", map[string]string{beadmeta.ContinuationGroupMetadataKey: "grp-1"})
	ledger.beforeUpdate = func(b *beads.Bead) { b.Assignee = "worker-live" }
	store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
	wb := ledger.row()

	var buf bytes.Buffer
	restore := captureLogOutput(&buf)
	defer restore()

	if releaseOrphanedPoolAssignment(store, wb, false) {
		t.Fatal("release = true after bd refused the guard")
	}
	got := ledger.row()
	if got.Status != "in_progress" || got.Assignee != "worker-live" || got.Metadata[beadmeta.ContinuationGroupMetadataKey] != "grp-1" {
		t.Fatalf("row = status %q assignee %q group %q, want the claim untouched", got.Status, got.Assignee, got.Metadata[beadmeta.ContinuationGroupMetadataKey])
	}
	if !strings.Contains(buf.String(), "assignment changed") {
		t.Fatalf("log output = %q, want the lost race logged as a skip", buf.String())
	}
}

// TestReleaseOrphanedPoolAssignment_BdStoreWithoutGuardsRefuses keeps
// refusal (a) for a bd that has neither --if-revision nor the guard flags:
// nothing is written blind.
func TestReleaseOrphanedPoolAssignment_BdStoreWithoutGuardsRefuses(t *testing.T) {
	ledger := newBdGuardLedger("open", "worker-dead", nil)
	ledger.noGuards = true
	store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
	wb := ledger.row()

	var buf bytes.Buffer
	restore := captureLogOutput(&buf)
	defer restore()

	if releaseOrphanedPoolAssignment(store, wb, false) {
		t.Fatal("release = true on a bd that can fence neither way")
	}
	got := ledger.row()
	if got.Status != "open" || got.Assignee != "worker-dead" || got.Metadata["gc.session_affinity"] != "require" {
		t.Fatalf("row = status %q assignee %q affinity %q, want it left as it was", got.Status, got.Assignee, got.Metadata["gc.session_affinity"])
	}
	for _, update := range ledger.updates {
		if !hasBdFlag(update, "--if-status") || !hasBdFlag(update, "--if-assignee") {
			t.Fatalf("bd update without the guards reached the store: %v", update)
		}
	}
	if !strings.Contains(buf.String(), "cannot release it conditionally") {
		t.Fatalf("log output = %q, want the refusal logged", buf.String())
	}
}

// TestReleaseWorkBead_BdStoreGuardedRelease gives ReleaseWorkBead's tier 2
// the pool release's policy on the bd-backed city, and pins the error contract
// its close-gating callers rely on: a landed or lost-to-a-claim release is
// nil, and a bd that cannot fence it is an error with the bead still assigned.
func TestReleaseWorkBead_BdStoreGuardedRelease(t *testing.T) {
	t.Run("lands as one guarded write", func(t *testing.T) {
		ledger := newBdGuardLedger("open", "retired-session", nil)
		store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
		wa := workAssignmentForStore(beads.WorkStore{Store: store})

		if err := wa.ReleaseWorkBead(ledger.row(), ""); err != nil {
			t.Fatalf("ReleaseWorkBead: %v", err)
		}
		if len(ledger.updates) != 1 || testFlagValue(ledger.updates[0], "--if-status") != "open" || testFlagValue(ledger.updates[0], "--if-assignee") != "retired-session" {
			t.Fatalf("bd updates = %v, want one write guarded on open/retired-session", ledger.updates)
		}
		if got := ledger.row(); got.Assignee != "" || got.Status != "open" {
			t.Fatalf("row = status %q assignee %q, want open and unassigned", got.Status, got.Assignee)
		}
	})
	t.Run("lost to a claim", func(t *testing.T) {
		ledger := newBdGuardLedger("open", "retired-session", nil)
		ledger.beforeUpdate = func(b *beads.Bead) { b.Status, b.Assignee = "in_progress", "fresh-worker" }
		store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
		wa := workAssignmentForStore(beads.WorkStore{Store: store})

		if err := wa.ReleaseWorkBead(ledger.row(), ""); err != nil {
			t.Fatalf("ReleaseWorkBead = %v, want nil: the bead moved on, as with tier 1's lost race", err)
		}
		if got := ledger.row(); got.Assignee != "fresh-worker" || got.Status != "in_progress" {
			t.Fatalf("row = status %q assignee %q, want the claim untouched", got.Status, got.Assignee)
		}
	})
	t.Run("bd without guards", func(t *testing.T) {
		ledger := newBdGuardLedger("open", "retired-session", nil)
		ledger.noGuards = true
		store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
		wa := workAssignmentForStore(beads.WorkStore{Store: store})

		err := wa.ReleaseWorkBead(ledger.row(), "")
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			t.Fatalf("ReleaseWorkBead = %v, want an error wrapping ErrConditionalWriteUnsupported so a caller does not close over the bead", err)
		}
		if got := ledger.row(); got.Assignee != "retired-session" {
			t.Fatalf("assignee = %q, want it left assigned", got.Assignee)
		}
	})
}

// sweepBdGuardLedger runs the pool orphan-release loop once over the ledger's
// row, snapshotted through the bd-backed store as the reconciler reads it.
func sweepBdGuardLedger(t *testing.T, store beads.Store, ledger *bdGuardLedger) []releasedPoolAssignment {
	t.Helper()
	wb, err := store.Get(ledger.row().ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return sweepOrphanedPoolRow(store, wb)
}

// sweepOrphanedPoolRow runs the pool orphan-release loop once over wb, a row of
// store, on a single-store city whose session store is store itself.
func sweepOrphanedPoolRow(store beads.Store, wb beads.Bead) []releasedPoolAssignment {
	return releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{wb},
		[]beads.Store{store},
		[]string{""},
		nil,
		nil,
		nil, nil,
	)
}

// TestReleaseOrphanedPoolAssignments_BdStoreKeepsStoreDeferredRow pins the
// release loop's non-change for issue #6207. While bd records the routed row
// deferred, even with an elapsed defer_until, its dead assignee is kept and
// nothing is written: a release would write status=open, silently un-defer
// the row and strand its date, which UpdateOpts cannot clear.
func TestReleaseOrphanedPoolAssignments_BdStoreKeepsStoreDeferredRow(t *testing.T) {
	elapsed := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)
	ledger := newBdGuardLedger("deferred", "worker-dead", nil)
	ledger.bead.DeferUntil = &elapsed
	store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")

	var logs bytes.Buffer
	restore := captureLogOutput(&logs)
	defer restore()

	if released := sweepBdGuardLedger(t, store, ledger); len(released) != 0 {
		t.Fatalf("released = %#v while bd records the row deferred, want none", released)
	}
	if len(ledger.updates) != 0 {
		t.Fatalf("bd updates = %v, want no write to a row bd records deferred", ledger.updates)
	}
	// The keep must come from the live raw-status re-read, not a failed one.
	if strings.Contains(logs.String(), "validation failed") || !strings.Contains(logs.String(), "moved") {
		t.Fatalf("log output = %q, want the row kept as moved by a successful live re-read", logs.String())
	}
	got := ledger.row()
	if got.Status != "deferred" || got.Assignee != "worker-dead" || got.DeferUntil == nil || !got.DeferUntil.Equal(elapsed) {
		t.Fatalf("row = status %q assignee %q defer_until %v, want deferred, worker-dead, %v unchanged", got.Status, got.Assignee, got.DeferUntil, elapsed)
	}
}

// TestReleaseOrphanedPoolAssignments_BdStoreReleasesStoreWokenRow is the other
// half of issue #6207's release contract: once the store wakes the row a
// release kept (status back to open, defer_until cleared, as bd's sweep or
// `bd undefer` writes), the next sweep's guarded release clears the dead
// assignee and the bead reads ready.
func TestReleaseOrphanedPoolAssignments_BdStoreReleasesStoreWokenRow(t *testing.T) {
	elapsed := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)
	ledger := newBdGuardLedger("deferred", "worker-dead", nil)
	ledger.bead.DeferUntil = &elapsed
	store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")

	if released := sweepBdGuardLedger(t, store, ledger); len(released) != 0 {
		t.Fatalf("released = %#v while bd records the row deferred, want none", released)
	}

	ledger.mu.Lock()
	ledger.bead.Status = "open"
	ledger.bead.DeferUntil = nil
	ledger.mu.Unlock()

	if released := sweepBdGuardLedger(t, store, ledger); len(released) != 1 {
		t.Fatalf("released = %#v after the store woke the row, want the dead assignee released; bd updates %v", released, ledger.updates)
	}
	if len(ledger.updates) != 1 || testFlagValue(ledger.updates[0], "--if-status") != "open" || testFlagValue(ledger.updates[0], "--if-assignee") != "worker-dead" {
		t.Fatalf("bd updates = %v, want one write guarded on open/worker-dead", ledger.updates)
	}
	woken, err := store.Get(ledger.row().ID)
	if err != nil {
		t.Fatalf("Get after release: %v", err)
	}
	if woken.Status != "open" || woken.Assignee != "" {
		t.Fatalf("row = status %q assignee %q, want open and unassigned", woken.Status, woken.Assignee)
	}
	now := time.Now()
	if beads.IsDeferred(woken, now) || !beads.IsReadyCandidate(woken, now) {
		t.Fatalf("woken row %+v does not read ready (IsDeferred %v, IsReadyCandidate %v)", woken, beads.IsDeferred(woken, now), beads.IsReadyCandidate(woken, now))
	}
}

// nativeGuardStorage is a one-row native backend under a real NativeDoltStore.
// Its reader role answers a listing as workapi.BuildListFilter does, matching
// the request's status against the RAW stored status: the Live "open" a
// NativeDoltStore pushes as bd's raw open (#7809) omits a row the store
// records deferred, while the folded "open" the census reads keeps it. The row
// carries no label, so a label listing matches nothing. Every write reaches
// ApplyBatch, which records the request and applies its status and assignee.
type nativeGuardStorage struct {
	beadslib.Storage
	mu     sync.Mutex
	row    beadslib.Issue
	writes []issueops.ApplyBatchRequest
}

func (s *nativeGuardStorage) IssueReader() (issueops.Reader, error) {
	return nativeGuardReader{s: s}, nil
}

func (s *nativeGuardStorage) EdgeReader() (issueops.EdgeReader, error) {
	return nativeGuardEdges{}, nil
}

func (s *nativeGuardStorage) BatchApplier() (issueops.BatchApplier, error) {
	return nativeGuardApplier{s: s}, nil
}

// wake is the store's own wake of the row, as bd's sweep or `bd undefer`
// writes it: status back to open, defer_until cleared.
func (s *nativeGuardStorage) wake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.row.Status = beadslib.StatusOpen
	s.row.DeferUntil = nil
	s.row.RowVersion++
}

// state returns the stored row and the number of writes the store received.
func (s *nativeGuardStorage) state() (beadslib.Issue, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.row, len(s.writes)
}

type nativeGuardReader struct {
	issueops.Reader
	s *nativeGuardStorage
}

func (r nativeGuardReader) List(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	row := r.s.row
	page := issueops.IssuePage{Items: []*issueops.IssueWithCounts{}}
	if (req.Status == "" || slices.Contains(strings.Split(req.Status, ","), string(row.Status))) &&
		(req.Assignee == "" || req.Assignee == row.Assignee) &&
		len(req.Labels) == 0 {
		page.Items = append(page.Items, &issueops.IssueWithCounts{Issue: &row})
	}
	return page, nil
}

func (r nativeGuardReader) Get(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if req.ID != r.s.row.ID {
		return nil, fmt.Errorf("%w: %s", issueops.ErrNotFound, req.ID)
	}
	return &issueops.IssueDetails{Issue: r.s.row}, nil
}

type nativeGuardEdges struct{ issueops.EdgeReader }

func (nativeGuardEdges) ReadEdges(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
	result := issueops.EdgeReadResult{Anchors: []issueops.AnchorEdges{}}
	for _, id := range req.IDs {
		result.Anchors = append(result.Anchors, issueops.AnchorEdges{ID: id})
	}
	return result, nil
}

type nativeGuardApplier struct{ s *nativeGuardStorage }

func (a nativeGuardApplier) ApplyBatch(_ context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.s.writes = append(a.s.writes, req)
	for _, item := range req.Items {
		if item.Update == nil || item.Update.Target.ID != a.s.row.ID {
			continue
		}
		patch := item.Update.Patch
		if patch.Status.Set {
			a.s.row.Status = patch.Status.Value
		}
		if patch.Assignee.Set {
			a.s.row.Assignee = patch.Assignee.Value
		}
		a.s.row.RowVersion++
	}
	return issueops.ApplyBatchResult{Items: make([]issueops.ItemResult, len(req.Items))}, nil
}

// TestReleaseOrphanedPoolAssignments_NativeStoreKeepsStoreDeferredRow is
// issue #6207's release contract on the native door, the sibling of the bd
// tests above. The census snapshots the folded "open", which keeps a row the
// store records deferred, and the release loop re-reads it with a Live "open"
// before it writes, which NativeDoltStore pushes to the reader role as bd's
// raw open (#7809). The row is absent from that re-read, so its dead assignee
// is kept and nothing is written; once the store wakes the row, the next sweep
// releases it.
func TestReleaseOrphanedPoolAssignments_NativeStoreKeepsStoreDeferredRow(t *testing.T) {
	elapsed := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)
	storage := &nativeGuardStorage{row: beadslib.Issue{
		ID:         "gc-7",
		Title:      "orphaned pool work",
		Status:     beadslib.StatusDeferred,
		IssueType:  beadslib.TypeTask,
		Assignee:   "worker-dead",
		DeferUntil: &elapsed,
		Metadata:   json.RawMessage(`{"gc.routed_to":"worker","gc.session_affinity":"require"}`),
		RowVersion: 3,
	}}
	store := beads.NewNativeDoltStoreOverStorageForTest(storage)
	census := func(t *testing.T) beads.Bead {
		t.Helper()
		open, err := store.List(beads.ListQuery{Status: "open"})
		if err != nil || len(open) != 1 {
			t.Fatalf("folded List(open) = %#v, %v; want the one row", open, err)
		}
		return open[0]
	}

	var logs bytes.Buffer
	restore := captureLogOutput(&logs)
	defer restore()

	parked := census(t)
	if parked.Status != "open" || !parked.IndefinitelyDeferred {
		t.Fatalf("census row = status %q IndefinitelyDeferred %v, want the store-deferred row folded to open and marked", parked.Status, parked.IndefinitelyDeferred)
	}
	if released := sweepOrphanedPoolRow(store, parked); len(released) != 0 {
		t.Fatalf("released = %#v while the store records the row deferred, want none", released)
	}
	if row, writes := storage.state(); writes != 0 || row.Status != beadslib.StatusDeferred || row.Assignee != "worker-dead" {
		t.Fatalf("native writes = %d, row = status %q assignee %q; want no write to a row the store records deferred", writes, row.Status, row.Assignee)
	}
	// The keep must come from the live raw-status re-read, not a failed one.
	if strings.Contains(logs.String(), "validation failed") || !strings.Contains(logs.String(), `kept "gc-7" of dead assignee "worker-dead": moved`) {
		t.Fatalf("log output = %q, want the row kept as moved by a successful live re-read", logs.String())
	}

	storage.wake()
	if released := sweepOrphanedPoolRow(store, census(t)); len(released) != 1 {
		t.Fatalf("released = %#v after the store woke the row, want the dead assignee released", released)
	}
	if row, writes := storage.state(); writes != 1 || row.Status != beadslib.StatusOpen || row.Assignee != "" {
		t.Fatalf("native writes = %d, row = status %q assignee %q; want one release write leaving the row open and unassigned", writes, row.Status, row.Assignee)
	}
}
