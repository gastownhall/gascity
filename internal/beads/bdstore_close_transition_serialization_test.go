package beads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

type serializedBDCloseFixture struct {
	mu                sync.Mutex
	status            string
	reason            string
	session           string
	revision          int64
	closeCalls        int
	firstCloseEntered chan struct{}
	releaseFirstClose chan struct{}
}

func (f *serializedBDCloseFixture) runner(_ string, name string, args ...string) ([]byte, error) {
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %q", name, args)
	}
	if len(args) == 2 && args[1] == "--help" {
		switch args[0] {
		case "update", "close", "assign", "delete":
			return []byte("Flags:\n      --if-revision int\n"), nil
		}
	}
	switch args[0] {
	case "version":
		return []byte("bd version 1.1.0\n"), nil
	case "show":
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.revision == 0 {
			f.revision = 1
		}
		return []byte(fmt.Sprintf(`[{"id":"bd-42","title":"target","status":%q,"issue_type":"task","metadata":{"close_reason":"same cooperative reason"},"close_reason":%q,"revision":%d}]`, f.status, f.reason, f.revision)), nil
	case "query":
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.revision == 0 {
			f.revision = 1
		}
		return []byte(fmt.Sprintf(`[{"id":"bd-42","title":"target","status":%q,"issue_type":"task","metadata":{"close_reason":"same cooperative reason"},"close_reason":%q,"revision":%d}]`, f.status, f.reason, f.revision)), nil
	case "dep":
		return []byte(`[]`), nil
	case "sql":
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.revision == 0 {
			f.revision = 1
		}
		return []byte(fmt.Sprintf(`[{"id":"bd-42","status":%q,"close_reason":%q,"closed_by_session":%q}]`, f.status, f.reason, f.session)), nil
	case "update":
		return []byte(`[{"id":"bd-42"}]`), nil
	case "close":
		f.mu.Lock()
		f.closeCalls++
		call := f.closeCalls
		f.mu.Unlock()
		if call == 1 {
			close(f.firstCloseEntered)
			<-f.releaseFirstClose
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.revision == 0 {
			f.revision = 1
		}
		if expected, ok := revisionArg(args); ok && expected != f.revision {
			return conditionalPreconditionBody(expected, f.revision), errors.New("exit status 9")
		}
		if f.status != "closed" {
			f.status = "closed"
			f.reason = argValue(args, "--reason")
			f.session = "ambient-session"
			f.revision++
		}
		return []byte(fmt.Sprintf(`[{"id":"bd-42","title":"target","status":"closed","issue_type":"task","close_reason":%q,"revision":%d}]`, f.reason, f.revision)), nil
	default:
		return nil, fmt.Errorf("unexpected bd args %q", args)
	}
}

func currentCloseTransitionScopeRefs(scope string) int {
	key := closeTransitionScopeKey(scope)
	lifecycleMutationMutexRegistry.Lock()
	defer lifecycleMutationMutexRegistry.Unlock()
	entry := lifecycleMutationMutexRegistry.entries[key]
	if entry == nil {
		return 0
	}
	return entry.refs
}

func TestBdStoreCloseTransitionSerializesCallersSharingScope(t *testing.T) {
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fixture := &serializedBDCloseFixture{
		status:            "open",
		firstCloseEntered: make(chan struct{}),
		releaseFirstClose: make(chan struct{}),
	}
	stores := []*BdStore{
		NewBdStore(scope, fixture.runner),
		NewBdStore(scope, fixture.runner),
	}
	type result struct {
		transition CloseTransition
		err        error
	}
	results := make(chan result, len(stores))
	start := make(chan struct{})
	for _, store := range stores {
		go func(store *BdStore) {
			<-start
			transition, err := store.CloseWithReasonIfOpen("bd-42", "same cooperative reason")
			results <- result{transition: transition, err: err}
		}(store)
	}
	close(start)
	select {
	case <-fixture.firstCloseEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("first close caller did not reach bd")
	}
	waitForCloseTransitionScopeRefs(t, scope)
	close(fixture.releaseFirstClose)

	winners := 0
	for range stores {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatalf("CloseWithReasonIfOpen: %v", got.err)
			}
			if got.transition.Transitioned {
				winners++
			}
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("serialized close caller did not return")
		}
	}
	if winners != 1 {
		t.Fatalf("transition winners = %d, want exactly 1", winners)
	}
	fixture.mu.Lock()
	closeCalls := fixture.closeCalls
	fixture.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("bd close calls = %d, want 1", closeCalls)
	}
}

func TestBdStoreCloseTransitionSerializesOrdinaryClosePaths(t *testing.T) {
	const reason = "same cooperative reason"
	tests := []struct {
		name  string
		close func(*BdStore) error
	}{
		{
			name: "Close",
			close: func(store *BdStore) error {
				return store.Close("bd-42")
			},
		},
		{
			name: "CloseWithReason",
			close: func(store *BdStore) error {
				return store.CloseWithReason("bd-42", reason)
			},
		},
		{
			name: "CloseIfMatch",
			close: func(store *BdStore) error {
				return store.CloseIfMatch("bd-42", 1)
			},
		},
		{
			name: "CloseAll",
			close: func(store *BdStore) error {
				closed, err := store.CloseAll([]string{"bd-42"}, map[string]string{"close_reason": reason})
				if err == nil && closed != 1 {
					return fmt.Errorf("CloseAll closed = %d, want 1", closed)
				}
				return err
			},
		},
		{
			name: "CloseAllWithReason",
			close: func(store *BdStore) error {
				closed, err := store.CloseAllWithReason([]string{"bd-42"}, reason)
				if err == nil && closed != 1 {
					return fmt.Errorf("CloseAllWithReason closed = %d, want 1", closed)
				}
				return err
			},
		},
		{
			name: "TxClose",
			close: func(store *BdStore) error {
				return store.Tx("test ordinary close", func(tx Tx) error {
					return tx.Close("bd-42")
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := t.TempDir()
			if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			fixture := &serializedBDCloseFixture{
				status:            "open",
				firstCloseEntered: make(chan struct{}),
				releaseFirstClose: make(chan struct{}),
			}
			ordinaryStore := NewBdStore(scope, fixture.runner)
			ordinaryStore.condWriteProbed = true
			ordinaryStore.condWriteCapable = true
			transitionStore := NewBdStore(scope, fixture.runner)

			ordinaryDone := make(chan error, 1)
			go func() { ordinaryDone <- tt.close(ordinaryStore) }()
			select {
			case <-fixture.firstCloseEntered:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("ordinary close did not reach bd")
			}

			type result struct {
				transition CloseTransition
				err        error
			}
			transitionDone := make(chan result, 1)
			go func() {
				transition, err := transitionStore.CloseWithReasonIfOpen("bd-42", reason)
				transitionDone <- result{transition: transition, err: err}
			}()

			// Before the fix, the transition can complete through a second bd close
			// while the ordinary close is paused. With cooperative serialization it
			// instead registers as a waiter on the ordinary caller's held scope.
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
					if currentCloseTransitionScopeRefs(scope) >= 2 {
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

			close(fixture.releaseFirstClose)
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
			fixture.mu.Lock()
			closeCalls := fixture.closeCalls
			fixture.mu.Unlock()
			if closeCalls != 1 {
				t.Fatalf("bd close calls = %d, want 1", closeCalls)
			}
		})
	}
}

func TestBdStoreCloseTransitionReleasesScopeLockOnReadError(t *testing.T) {
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	wantErr := errors.New("injected snapshot failure")
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		if name == "bd" && len(args) > 0 && args[0] == "version" {
			return []byte("bd version 1.1.0\n"), nil
		}
		if name == "bd" && len(args) == 2 && args[1] == "--help" {
			return []byte("Flags:\n      --if-revision int\n"), nil
		}
		return nil, wantErr
	}
	store := NewBdStore(scope, runner)
	if _, err := store.CloseWithReasonIfOpen("bd-42", "reason"); !errors.Is(err, wantErr) {
		t.Fatalf("CloseWithReasonIfOpen error = %v, want %v", err, wantErr)
	}

	done := make(chan error, 1)
	go func() {
		unlock, err := lockCloseTransitionScope(scope)
		if err == nil {
			unlock()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("lockCloseTransitionScope after read error: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("BdStore retained the scope lock after a read error")
	}
}

func TestBdStoreMetadataWritesSerializeWithLifecyclePublication(t *testing.T) {
	tests := []struct {
		name  string
		write func(*BdStore) error
	}{
		{
			name: "SetMetadata",
			write: func(store *BdStore) error {
				return store.SetMetadata("bd-42", "phase", "newer")
			},
		},
		{
			name: "SetMetadataBatch",
			write: func(store *BdStore) error {
				return store.SetMetadataBatch("bd-42", map[string]string{"phase": "newer", "owner": "replacement"})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := newLifecycleMutationLeaseScope(t)
			metadataEntered := make(chan struct{})
			runner := func(_ string, name string, args ...string) ([]byte, error) {
				if name != "bd" || len(args) == 0 || args[0] != "update" {
					return nil, fmt.Errorf("unexpected command %s %q", name, args)
				}
				close(metadataEntered)
				return []byte(`[{"id":"bd-42"}]`), nil
			}
			owner := NewBdStore(scope, runner)
			contender := NewBdStore(scope, runner)

			ownerEntered := make(chan struct{})
			releaseOwner := make(chan struct{})
			ownerDone := make(chan error, 1)
			go func() {
				ownerDone <- owner.WithLifecycleMetadataTransaction("bd-42", func(LifecycleMetadataTransaction) error {
					close(ownerEntered)
					<-releaseOwner
					return nil
				})
			}()
			select {
			case <-ownerEntered:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("lifecycle publication did not acquire its scope")
			}

			writeDone := make(chan error, 1)
			go func() { writeDone <- tt.write(contender) }()
			reachedBeforeSerialization := false
			deadline := time.NewTimer(testutil.GoroutineRaceTimeout)
		waitForContender:
			for {
				select {
				case <-metadataEntered:
					reachedBeforeSerialization = true
					break waitForContender
				case <-deadline.C:
					t.Fatal("metadata write neither executed nor joined the lifecycle scope")
				default:
					if currentCloseTransitionScopeRefs(scope) >= 2 {
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

			close(releaseOwner)
			select {
			case err := <-ownerDone:
				if err != nil {
					t.Fatalf("lifecycle publication: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("lifecycle publication did not release")
			}
			select {
			case err := <-writeDone:
				if err != nil {
					t.Fatalf("metadata write: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("metadata write did not return after lifecycle publication released")
			}
			if reachedBeforeSerialization {
				t.Error("metadata write reached bd before the older lifecycle publication released its scope")
			}
		})
	}
}
