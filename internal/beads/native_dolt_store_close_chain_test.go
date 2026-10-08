package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// CloseAll's PER-BEAD route, which is what a backing with no batch applier
// takes. The route CloseAll takes when one is available is one request per
// chunk and is pinned next door, in native_dolt_store_batch_close_test.go;
// closeChainStorage below refuses the applier so this file keeps measuring the
// loop rather than silently becoming a second test of the batch.
//
// The metadata-then-close pair is gc's one write chain that needs a value an
// earlier write in the same chain produced: gc stamps close_reason into the
// metadata, and the close carries that reason.
//
// It used to recover the reason with a follow-up read — Close's own GetIssue,
// dialed on top of the status read the loop already makes. The lifecycle role
// answers every write with its post-state snapshot (issueops.UpdateResult.Issue,
// the member that also carries the post-write RowVersion a guarded chain
// composes its next ExpectedVersion from), so the reason comes off the result.
//
// The doubles below make both halves observable: rawReads counts the raw
// read-backs the chain dials, and the lifecycle records every request. A
// re-introduced follow-up Get shows up as a raw read; a reason taken from the
// pre-write row shows up as the stale value.

type closeChainLifecycle struct {
	storage *closeChainStorage
	updates []issueops.UpdateRequest
	closes  []issueops.CloseRequest
	// hydrate, when false, answers an update with no post-state issue — the
	// contract violation the chain has to survive rather than mis-close on.
	hydrate bool
	// beforeClose runs when the update returns, which is the window between the
	// chain's status read and its fallback re-read.
	beforeClose func()
	// updateErrs and closeErrs fail the leading attempts of their leg, one error
	// per attempt, and a failed attempt writes nothing: the shape of a write
	// that lost its serialization race.
	updateErrs []error
	closeErrs  []error
}

var _ issueops.Lifecycle = (*closeChainLifecycle)(nil)

func (l *closeChainLifecycle) Create(context.Context, issueops.CreateRequest) (issueops.CreateResult, error) {
	return issueops.CreateResult{}, errors.New("create is not part of the close chain")
}

func (l *closeChainLifecycle) Update(_ context.Context, req issueops.UpdateRequest) (issueops.UpdateResult, error) {
	l.updates = append(l.updates, req)
	if err := nextScriptedAttemptError(&l.updateErrs); err != nil {
		return issueops.UpdateResult{}, err
	}
	l.storage.merge(req.IssueID, req.Patch.Metadata.Set)
	if l.beforeClose != nil {
		l.beforeClose()
	}
	if !l.hydrate {
		return issueops.UpdateResult{Changed: true}, nil
	}
	return issueops.UpdateResult{Issue: l.storage.issue(req.IssueID), Changed: true}, nil
}

func (l *closeChainLifecycle) Close(_ context.Context, req issueops.CloseRequest) (issueops.CloseResult, error) {
	l.closes = append(l.closes, req)
	if err := nextScriptedAttemptError(&l.closeErrs); err != nil {
		return issueops.CloseResult{}, err
	}
	return issueops.CloseResult{Issue: l.storage.issue(req.IssueID), Changed: true}, nil
}

// nextScriptedAttemptError pops the next scripted failure off errs, or answers
// nil once none are left.
func nextScriptedAttemptError(errs *[]error) error {
	if len(*errs) == 0 {
		return nil
	}
	err := (*errs)[0]
	*errs = (*errs)[1:]
	return err
}

func (l *closeChainLifecycle) Reopen(context.Context, issueops.ReopenRequest) (issueops.ReopenResult, error) {
	return issueops.ReopenResult{}, errors.New("reopen is not part of the close chain")
}

// closeChainStorage answers the detail read through the reader role and counts
// every RAW GetIssue beside it. The raw read is the follow-up this chain is
// supposed to have stopped dialing, so its count is the assertion.
type closeChainStorage struct {
	beadslib.Storage
	metadata map[string]map[string]string
	rawReads int
	// lifecycles counts accessor resolutions. It is the deadlock guard: the
	// chain runs under the store's read lock, so a fallback that re-entered a
	// public door would take that lock a second time on the same goroutine and
	// deadlock behind any queued writer. One acquisition means one accessor
	// resolution, so a second resolution IS the re-entry, observed without
	// having to race a writer into the window.
	lifecycles int
	roleReads  int
	// vanished makes the raw read answer "no such row", the state a bead
	// deleted between this chain's status read and its fallback re-read is in.
	vanished  bool
	lifecycle *closeChainLifecycle
}

func newCloseChainStorage(seed map[string]string) *closeChainStorage {
	s := &closeChainStorage{metadata: map[string]map[string]string{"gc-1": {}}}
	for k, v := range seed {
		s.metadata["gc-1"][k] = v
	}
	s.lifecycle = &closeChainLifecycle{storage: s, hydrate: true}
	return s
}

func (s *closeChainStorage) merge(id string, values map[string]json.RawMessage) {
	if s.metadata[id] == nil {
		s.metadata[id] = map[string]string{}
	}
	for key, raw := range values {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			continue
		}
		s.metadata[id][key] = decoded
	}
}

func (s *closeChainStorage) issue(id string) *beadslib.Issue {
	raw, err := metadataRawFromMap(s.metadata[id])
	if err != nil {
		return nil
	}
	return &beadslib.Issue{ID: id, Title: "chain", Status: beadslib.StatusOpen, Metadata: raw}
}

func (s *closeChainStorage) GetIssue(_ context.Context, id string) (*beadslib.Issue, error) {
	s.rawReads++
	if s.vanished {
		return nil, nil
	}
	return s.issue(id), nil
}

func (s *closeChainStorage) IssueLifecycle() (issueops.Lifecycle, error) {
	s.lifecycles++
	return s.lifecycle, nil
}

// BatchApplier refuses the way a backend without one does, which is what routes
// every case in this file down the per-bead loop it was written to measure.
func (s *closeChainStorage) BatchApplier() (issueops.BatchApplier, error) {
	return nil, &beadslib.ErrUnsupported{Op: "BatchApplier", Backend: "close-chain-double"}
}

func (s *closeChainStorage) IssueReader() (issueops.Reader, error) { return closeChainReader{s}, nil }

// EdgeReader serves the edge half of the detail read: the chain's bead has no
// edges.
func (s *closeChainStorage) EdgeReader() (issueops.EdgeReader, error) { return edgelessReader{}, nil }

type closeChainReader struct{ storage *closeChainStorage }

func (r closeChainReader) Get(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	r.storage.roleReads++
	issue := r.storage.issue(req.ID)
	if issue == nil {
		return nil, errors.New("not found")
	}
	return &issueops.IssueDetails{Issue: *issue}, nil
}

func (r closeChainReader) List(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("List is not part of the close chain")
}

func (r closeChainReader) Ready(context.Context, issueops.ReadyRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("Ready is not part of the close chain")
}

// The chain reads the status once through the reader role and never dials the
// raw read-back: the reason it closes with is the one the update answered with.
func TestCloseAllPerBeadRouteTakesTheCloseReasonOffTheUpdateResult(t *testing.T) {
	storage := newCloseChainStorage(map[string]string{"close_reason": "STALE row value"})
	store := newNativeDoltStoreForTest(storage)

	closed, err := store.CloseAll([]string{"gc-1"}, map[string]string{"close_reason": "  the sweep's reason  "})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
	if storage.rawReads != 0 {
		t.Errorf("the chain dialed %d raw read-back(s); the close reason rides home on the update's post-state result, so there is nothing left to re-read", storage.rawReads)
	}
	if storage.roleReads != 1 {
		t.Errorf("the chain made %d detail reads, want the single status read the loop needs", storage.roleReads)
	}
	if len(storage.lifecycle.closes) != 1 {
		t.Fatalf("closes = %d, want 1", len(storage.lifecycle.closes))
	}
	if got := storage.lifecycle.closes[0].Reason; got != "the sweep's reason" {
		t.Errorf("close reason = %q, want the reason THIS call stamped — a reason read before the write is the row's stale one", got)
	}
	if !storage.lifecycle.closes[0].Force {
		t.Error("the close is not forced; a molecule root routinely closes over open children and the storage-layer close this replaced applied no policy")
	}
}

// A backend that answers a write with no post-state snapshot is violating the
// role contract, but the chain must not close on a reason it never read. It
// re-reads the row — ON THE HANDLE IT ALREADY HOLDS.
//
// That last part is the deadlock this test also guards. The chain runs under
// the store's read lock (acquireStorage hands back s.mu.RUnlock), and a
// fallback that delegated to Close would take that lock a second time on the
// same goroutine. Go's RWMutex forbids recursive read locking, so a writer
// arriving in between — the reconnect handle swap, or CloseStore — parks in
// front of the inner RLock and all three hang. Racing a writer into that window
// is not observable (an RWMutex publishes no waiter count), so the assertion is
// on the structure instead: one storage acquisition resolves one accessor, so a
// SECOND accessor resolution is the re-entry, deterministically and with no
// timing. The path exists precisely for the misbehaving backend, and hanging is
// a worse answer than the one it was written to give.
func TestCloseAllPerBeadRouteFallsBackWhenTheUpdateAnswersNoPostState(t *testing.T) {
	storage := newCloseChainStorage(nil)
	storage.lifecycle.hydrate = false
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.CloseAll([]string{"gc-1"}, map[string]string{"close_reason": "the sweep's reason"}); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if storage.rawReads == 0 {
		t.Error("the chain closed without reading the reason from anywhere: no post-state snapshot and no re-read")
	}
	if got := storage.lifecycle.closes[0].Reason; got != "the sweep's reason" {
		t.Errorf("close reason = %q, want the reason the fallback re-read", got)
	}
	if storage.lifecycles != 1 {
		t.Errorf("the fallback resolved %d lifecycle accessors, want 1: a second resolution means it re-entered a public door and took the store's read lock recursively, which deadlocks behind any queued writer", storage.lifecycles)
	}
}

// A bead that vanishes between the chain's status read and the fallback's
// re-read is ErrNotFound, which is what Close answers for the same row. Closing
// it with an empty reason instead would record a retirement for a bead nothing
// can show, and it is the shape the fallback falls into if it stops asking.
func TestCloseAllPerBeadRouteReportsABeadThatVanishedMidChain(t *testing.T) {
	storage := newCloseChainStorage(nil)
	storage.lifecycle.hydrate = false
	store := newNativeDoltStoreForTest(storage)
	storage.lifecycle.beforeClose = func() { storage.vanished = true }

	_, err := store.CloseAll([]string{"gc-1"}, map[string]string{"close_reason": "the sweep's reason"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CloseAll over a bead that vanished mid-chain = %v, want ErrNotFound", err)
	}
	if len(storage.lifecycle.closes) != 0 {
		t.Errorf("it closed anyway, with reason %q", storage.lifecycle.closes[0].Reason)
	}
}

// With no metadata to stamp there is no chain: nothing in this call wrote a
// reason, so the close's own read is the only one it makes and it stays.
func TestCloseAllPerBeadRouteWithoutMetadataKeepsTheCloseDoorsOwnRead(t *testing.T) {
	storage := newCloseChainStorage(map[string]string{"close_reason": "the reason the row already held"})
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.CloseAll([]string{"gc-1"}, nil); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if len(storage.lifecycle.updates) != 0 {
		t.Errorf("an empty metadata map dialed %d update(s), want none", len(storage.lifecycle.updates))
	}
	if got := storage.lifecycle.closes[0].Reason; got != "the reason the row already held" {
		t.Errorf("close reason = %q, want the row's own", got)
	}
}

// Each leg of the chain retries its own serialization conflict, and only its
// own. A conflicted stamp committed nothing, so replaying it is the replay
// SetMetadataBatch makes of the identical merge. A conflicted close follows a
// stamp that DID commit, so it replays alone: replaying the pair would re-run a
// write that won its race to recover one that lost. Neither replay resolves the
// lifecycle accessor again, which is the re-entry the deadlock guard above
// counts.
func TestCloseAllPerBeadRouteRetriesEachLegOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		updateErrs  []error
		closeErrs   []error
		wantUpdates int
		wantCloses  int
		wantErr     string
	}{
		{
			name:        "the stamp lost its serialization race",
			updateErrs:  []error{errors.New(serializationConflictErr)},
			wantUpdates: 2,
			wantCloses:  1,
		},
		{
			name:        "the close lost its serialization race",
			closeErrs:   []error{errors.New(serializationConflictErr)},
			wantUpdates: 1,
			wantCloses:  2,
		},
		{
			name:        "the stamp failed for a reason a replay cannot fix",
			updateErrs:  []error{errors.New("Error 1062 (23000): duplicate entry")},
			wantUpdates: 1,
			wantErr:     "duplicate entry",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := newCloseChainStorage(nil)
			storage.lifecycle.updateErrs = tc.updateErrs
			storage.lifecycle.closeErrs = tc.closeErrs
			store := newNativeDoltStoreForTest(storage)

			closed, err := store.CloseAll([]string{"gc-1"}, map[string]string{"close_reason": "the sweep's reason"})
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("CloseAll error = %v, want one carrying %q", err, tc.wantErr)
				}
			case err != nil || closed != 1:
				t.Fatalf("CloseAll = (%d, %v), want (1, nil)", closed, err)
			}
			if got := len(storage.lifecycle.updates); got != tc.wantUpdates {
				t.Errorf("stamp attempts = %d, want %d", got, tc.wantUpdates)
			}
			if got := len(storage.lifecycle.closes); got != tc.wantCloses {
				t.Errorf("close attempts = %d, want %d", got, tc.wantCloses)
			}
			for _, req := range storage.lifecycle.closes {
				if req.Reason != "the sweep's reason" {
					t.Errorf("close reason = %q, want the reason the committed stamp answered with", req.Reason)
				}
			}
			if storage.lifecycles != 1 {
				t.Errorf("the chain resolved %d lifecycle accessors, want 1: a replay that re-entered a public door would take the store's read lock recursively", storage.lifecycles)
			}
		})
	}
}
