package beads

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltStoreCloseTransitionPreservesEmptySessionAttribution(t *testing.T) {
	storage := newNativeCloseTransitionStorage()
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "native session attribution"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	transition, err := store.CloseWithReasonIfOpen(created.ID, "native close")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if !transition.Transitioned {
		t.Fatal("Transitioned = false, want true")
	}
	storage.mu.Lock()
	session := storage.session
	storage.mu.Unlock()
	if session != "" {
		t.Fatalf("closed_by_session = %q, want NativeDoltStore.Close-compatible empty attribution", session)
	}
}

func TestNativeDoltStoreCloseTransitionRecognizesDifferentReasonWinner(t *testing.T) {
	storage := &nativeRawWinnerBeforeTransactionStorage{
		nativeCloseTransitionStorage: newNativeCloseTransitionStorage(),
		reason:                       "external winner",
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "external winner"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	storage.targetID = created.ID

	transition, err := store.CloseWithReasonIfOpen(created.ID, "autoclose reason")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if transition.Transitioned {
		t.Fatal("Transitioned = true, want false for a different-reason external winner")
	}
	if got := transition.After.Metadata["close_reason"]; got != "external winner" {
		t.Fatalf("After close_reason = %q, want external winner", got)
	}
}

type nativeDuplicateCloseTransitionStorage struct {
	*nativeCloseTransitionStorage
	issueTwin *beadslib.Issue
}

func (s *nativeDuplicateCloseTransitionStorage) SearchIssues(ctx context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	wisp, err := s.GetIssue(ctx, s.issueTwin.ID)
	if err != nil {
		return nil, err
	}
	if filter.IncludeDependencies {
		wisp.Dependencies, err = s.GetDependencyRecords(ctx, wisp.ID)
		if err != nil {
			return nil, err
		}
	}
	issueTwin := *s.issueTwin
	return []*beadslib.Issue{&issueTwin, wisp}, nil
}

func (s *nativeDuplicateCloseTransitionStorage) GetIssuesByIDs(ctx context.Context, ids []string) ([]*beadslib.Issue, error) {
	if len(ids) != 1 || ids[0] != s.issueTwin.ID {
		return nil, nil
	}
	wisp, err := s.GetIssue(ctx, ids[0])
	if err != nil {
		return nil, err
	}
	return []*beadslib.Issue{wisp}, nil
}

func TestNativeDoltStoreCloseTransitionUsesCanonicalWispSnapshotOnDuplicateID(t *testing.T) {
	base := newNativeCloseTransitionStorage()
	blocker, err := base.store.Create(Bead{Title: "canonical blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	wisp, err := base.store.Create(Bead{
		Title:     "canonical wisp",
		Labels:    []string{"canonical-label"},
		Metadata:  StringMap{"source": "canonical-wisp"},
		NoHistory: true,
	})
	if err != nil {
		t.Fatalf("Create wisp: %v", err)
	}
	if err := base.store.DepAdd(wisp.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	storage := &nativeDuplicateCloseTransitionStorage{
		nativeCloseTransitionStorage: base,
		issueTwin: &beadslib.Issue{
			ID:          wisp.ID,
			Title:       "stale issue twin",
			Status:      beadslib.StatusClosed,
			IssueType:   beadslib.TypeTask,
			CloseReason: "stale issue reason",
		},
	}
	store := newNativeDoltStoreForTest(storage)

	transition, err := store.CloseWithReasonIfOpen(wisp.ID, "canonical close reason")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if !transition.Transitioned {
		t.Fatal("Transitioned = false, want true for the canonical wisp close")
	}
	if transition.Before.Title != "canonical wisp" || transition.Before.Status != "open" || !transition.Before.NoHistory {
		t.Fatalf("Before = %#v, want the open canonical no-history wisp", transition.Before)
	}
	if got := transition.Before.Metadata["source"]; got != "canonical-wisp" {
		t.Fatalf("Before source = %q, want canonical-wisp", got)
	}
	if len(transition.Before.Labels) != 1 || transition.Before.Labels[0] != "canonical-label" {
		t.Fatalf("Before labels = %v, want canonical-label", transition.Before.Labels)
	}
	if len(transition.Before.Dependencies) != 1 || transition.Before.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("Before dependencies = %#v, want canonical wisp dependency on %s", transition.Before.Dependencies, blocker.ID)
	}
	if transition.After.Title != "canonical wisp" || transition.After.Status != "closed" || !transition.After.NoHistory {
		t.Fatalf("After = %#v, want the closed canonical no-history wisp", transition.After)
	}
	if got := transition.After.Metadata["close_reason"]; got != "canonical close reason" {
		t.Fatalf("After close_reason = %q, want canonical close reason", got)
	}
	if storage.issueTwin.Status != beadslib.StatusClosed || storage.issueTwin.CloseReason != "stale issue reason" {
		t.Fatalf("stale issue twin changed: %#v", storage.issueTwin)
	}
}

type nativeBlockingCloseTransitionStorage struct {
	*nativeCloseTransitionStorage
	closeEntered       chan struct{}
	releaseClose       chan struct{}
	concurrentSnapshot chan struct{}
	enteredOnce        sync.Once
	closeBlocked       atomic.Bool
}

func (s *nativeBlockingCloseTransitionStorage) CloseIssue(ctx context.Context, id, reason, actor, session string) error {
	s.closeBlocked.Store(true)
	s.enteredOnce.Do(func() { close(s.closeEntered) })
	<-s.releaseClose
	err := s.nativeCloseTransitionStorage.CloseIssue(ctx, id, reason, actor, session)
	s.closeBlocked.Store(false)
	return err
}

func (s *nativeBlockingCloseTransitionStorage) RunInTransaction(
	_ context.Context,
	_ string,
	fn func(beadslib.Transaction) error,
) error {
	return runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(nativeDoltTransactionForTest{storage: s})
	})
}

func (s *nativeBlockingCloseTransitionStorage) GetIssue(ctx context.Context, id string) (*beadslib.Issue, error) {
	if s.closeBlocked.Load() {
		select {
		case s.concurrentSnapshot <- struct{}{}:
		default:
		}
	}
	return s.nativeCloseTransitionStorage.GetIssue(ctx, id)
}

func TestNativeDoltStoreCloseTransitionSerializesSameScope(t *testing.T) {
	storage := &nativeBlockingCloseTransitionStorage{
		nativeCloseTransitionStorage: newNativeCloseTransitionStorage(),
		closeEntered:                 make(chan struct{}),
		releaseClose:                 make(chan struct{}),
		concurrentSnapshot:           make(chan struct{}, 1),
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "serialized native close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	type result struct {
		transition CloseTransition
		err        error
	}
	results := make(chan result, 2)
	go func() {
		transition, err := store.CloseWithReasonIfOpen(created.ID, "same reason")
		results <- result{transition: transition, err: err}
	}()
	<-storage.closeEntered

	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		transition, err := store.CloseWithReasonIfOpen(created.ID, "same reason")
		results <- result{transition: transition, err: err}
	}()
	<-secondStarted

	select {
	case <-storage.concurrentSnapshot:
		t.Fatal("second transition read the backing while the first scope close was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(storage.releaseClose)

	winners := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("CloseWithReasonIfOpen: %v", result.err)
		}
		if result.transition.Transitioned {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("transition winners = %d, want exactly 1", winners)
	}
}

type nativeOrdinaryCloseRaceStorage struct {
	*nativeCloseTransitionStorage
	raceMu            sync.Mutex
	closeCalls        int
	firstCloseEntered chan struct{}
	releaseFirstClose chan struct{}
}

func (s *nativeOrdinaryCloseRaceStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	return runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(nativeDoltTransactionForTest{storage: s})
	})
}

func (s *nativeOrdinaryCloseRaceStorage) CloseIssue(ctx context.Context, id, reason, actor, session string) error {
	s.raceMu.Lock()
	s.closeCalls++
	call := s.closeCalls
	s.raceMu.Unlock()
	if call == 1 {
		close(s.firstCloseEntered)
		<-s.releaseFirstClose
	}
	return s.nativeCloseTransitionStorage.CloseIssue(ctx, id, reason, actor, session)
}

func TestNativeDoltStoreCloseTransitionSerializesOrdinaryClosePaths(t *testing.T) {
	const reason = "same cooperative reason"
	tests := []struct {
		name  string
		close func(*NativeDoltStore, string) error
	}{
		{
			name: "Close",
			close: func(store *NativeDoltStore, id string) error {
				return store.Close(id)
			},
		},
		{
			name: "CloseAll",
			close: func(store *NativeDoltStore, id string) error {
				closed, err := store.CloseAll([]string{id}, map[string]string{"close_reason": reason})
				if err == nil && closed != 1 {
					return fmt.Errorf("CloseAll closed = %d, want 1", closed)
				}
				return err
			},
		},
		{
			name: "TxClose",
			close: func(store *NativeDoltStore, id string) error {
				return store.Tx("test ordinary close", func(tx Tx) error {
					return tx.Close(id)
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage := &nativeOrdinaryCloseRaceStorage{
				nativeCloseTransitionStorage: newNativeCloseTransitionStorage(),
				firstCloseEntered:            make(chan struct{}),
				releaseFirstClose:            make(chan struct{}),
			}
			store := newNativeDoltStoreForTest(storage)
			created, err := store.Create(Bead{
				Title:    "ordinary close wins",
				Metadata: StringMap{"close_reason": reason},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			ordinaryDone := make(chan error, 1)
			go func() { ordinaryDone <- tt.close(store, created.ID) }()
			select {
			case <-storage.firstCloseEntered:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("ordinary close did not reach native storage")
			}

			type result struct {
				transition CloseTransition
				err        error
			}
			transitionDone := make(chan result, 1)
			go func() {
				transition, err := store.CloseWithReasonIfOpen(created.ID, reason)
				transitionDone <- result{transition: transition, err: err}
			}()

			var got result
			haveResult := false
			deadline := time.NewTimer(testutil.GoroutineRaceTimeout)
		waitForContender:
			for {
				select {
				case got = <-transitionDone:
					haveResult = true
					break waitForContender
				case <-deadline.C:
					t.Fatal("transition neither completed nor waited on the ordinary close")
				default:
					if currentCloseTransitionScopeRefs(store.closeTransitionScopeKey()) >= 2 {
						break waitForContender
					}
					runtime.Gosched()
				}
			}
			if !deadline.Stop() {
				select {
				case <-deadline.C:
				default:
				}
			}

			close(storage.releaseFirstClose)
			select {
			case err := <-ordinaryDone:
				if err != nil {
					t.Fatalf("ordinary close: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("ordinary close did not return")
			}
			if !haveResult {
				select {
				case got = <-transitionDone:
				case <-time.After(testutil.GoroutineRaceTimeout):
					t.Fatal("transition did not return after ordinary close released")
				}
			}
			if got.err != nil {
				t.Fatalf("CloseWithReasonIfOpen: %v", got.err)
			}
			if got.transition.Transitioned {
				t.Fatal("Transitioned = true, want false after ordinary Gas City close won")
			}
			storage.raceMu.Lock()
			closeCalls := storage.closeCalls
			storage.raceMu.Unlock()
			if closeCalls != 1 {
				t.Fatalf("native close calls = %d, want 1", closeCalls)
			}
		})
	}
}
