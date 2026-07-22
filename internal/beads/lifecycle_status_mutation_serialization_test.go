package beads

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestNativeDoltStoreStatusMutationsWaitForLifecycleLeaseAcrossHandles(t *testing.T) {
	closed := "closed"
	inProgress := "in_progress"
	tests := []struct {
		name         string
		create       Bead
		setup        func(*NativeDoltStore, string) error
		mutate       func(*NativeDoltStore, string) error
		beforeStatus string
		beforeOwner  string
		afterStatus  string
		afterOwner   string
	}{
		{
			name: "UpdateStatus",
			mutate: func(store *NativeDoltStore, id string) error {
				return store.Update(id, UpdateOpts{Status: &closed})
			},
			beforeStatus: "open",
			afterStatus:  "closed",
		},
		{
			name: "Reopen",
			setup: func(store *NativeDoltStore, id string) error {
				return store.Close(id)
			},
			mutate: func(store *NativeDoltStore, id string) error {
				return store.Reopen(id)
			},
			beforeStatus: "closed",
			afterStatus:  "open",
		},
		{
			name:   "ReleaseIfCurrent",
			create: Bead{Assignee: "worker-1"},
			setup: func(store *NativeDoltStore, id string) error {
				return store.Update(id, UpdateOpts{Status: &inProgress})
			},
			mutate: func(store *NativeDoltStore, id string) error {
				released, err := store.ReleaseIfCurrent(id, "worker-1")
				if err != nil {
					return err
				}
				if !released {
					return fmt.Errorf("ReleaseIfCurrent released = false, want true")
				}
				return nil
			},
			beforeStatus: "in_progress",
			beforeOwner:  "worker-1",
			afterStatus:  "open",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := newLifecycleStatusMutationScope(t)
			shared := NewMemStore()
			owner := newNativeDoltStoreForTest(&nativeDoltMemStorage{store: shared})
			contender := newNativeDoltStoreForTest(&nativeDoltMemStorage{store: shared})
			owner.scopeRoot = scope
			contender.scopeRoot = scope

			seed := tt.create
			seed.Title = "native lifecycle status mutation"
			created, err := owner.Create(seed)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tt.setup != nil {
				if err := tt.setup(owner, created.ID); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}

			leaseEntered := make(chan struct{})
			releaseLease := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseLease) }) }
			defer release()
			ownerDone := make(chan error, 1)
			go func() {
				ownerDone <- owner.WithLifecycleMetadataTransaction(created.ID, func(tx LifecycleMetadataTransaction) error {
					close(leaseEntered)
					<-releaseLease
					fresh, err := tx.Get()
					if err != nil {
						return err
					}
					if fresh.Status != tt.beforeStatus || fresh.Assignee != tt.beforeOwner {
						return fmt.Errorf("row changed while lifecycle lease was held: status=%q assignee=%q, want %q/%q",
							fresh.Status, fresh.Assignee, tt.beforeStatus, tt.beforeOwner)
					}
					return nil
				})
			}()
			waitForLifecycleStatusMutationSignal(t, "lifecycle lease", leaseEntered)

			mutationDone := make(chan error, 1)
			go func() { mutationDone <- tt.mutate(contender, created.ID) }()
			waitForLifecycleStatusMutationContender(t, scope, tt.name, mutationDone)
			select {
			case err := <-mutationDone:
				t.Fatalf("%s returned while lifecycle lease was held: %v", tt.name, err)
			default:
			}

			release()
			waitForLifecycleStatusMutationResult(t, "lifecycle owner", ownerDone)
			waitForLifecycleStatusMutationResult(t, tt.name, mutationDone)

			fresh, err := owner.Get(created.ID)
			if err != nil {
				t.Fatalf("Get after %s: %v", tt.name, err)
			}
			if fresh.Status != tt.afterStatus || fresh.Assignee != tt.afterOwner {
				t.Fatalf("row after %s: status=%q assignee=%q, want %q/%q",
					tt.name, fresh.Status, fresh.Assignee, tt.afterStatus, tt.afterOwner)
			}
		})
	}
}

func TestBdStoreReleaseIfCurrentWaitsForLifecycleLeaseAcrossHandles(t *testing.T) {
	scope := newLifecycleStatusMutationScope(t)
	legacyRunner := func(_ string, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("release-if-current unexpectedly used legacy runner: %s %q", name, args)
	}
	runnerEntered := make(chan struct{})
	var mutationEnv map[string]string
	envRunner := func(_ string, name string, env map[string]string, args ...string) ([]byte, error) {
		mutationEnv = maps.Clone(env)
		close(runnerEntered)
		if name != "bd" || len(args) != 3 || args[0] != "sql" || args[1] != "--json" {
			return nil, fmt.Errorf("unexpected release-if-current command: %s %q", name, args)
		}
		return []byte(`{"rows_affected":1}`), nil
	}
	owner := NewBdStore(scope, legacyRunner, WithBdStoreCommandEnvRunner(envRunner))
	contender := NewBdStore(scope, legacyRunner, WithBdStoreCommandEnvRunner(envRunner))

	leaseEntered := make(chan struct{})
	releaseLease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseLease) }) }
	defer release()
	ownerDone := make(chan error, 1)
	go func() {
		ownerDone <- owner.WithLifecycleMetadataTransaction("bd-42", func(LifecycleMetadataTransaction) error {
			close(leaseEntered)
			<-releaseLease
			return nil
		})
	}()
	waitForLifecycleStatusMutationSignal(t, "bd lifecycle lease", leaseEntered)

	mutationDone := make(chan error, 1)
	go func() {
		released, err := contender.ReleaseIfCurrent("bd-42", "worker-1")
		if err == nil && !released {
			err = fmt.Errorf("ReleaseIfCurrent released = false, want true")
		}
		mutationDone <- err
	}()
	waitForLifecycleStatusMutationContender(t, scope, "BdStore.ReleaseIfCurrent", mutationDone)
	select {
	case <-runnerEntered:
		t.Fatal("bd release-if-current reached its child command while lifecycle lease was held")
	case err := <-mutationDone:
		t.Fatalf("bd release-if-current returned while lifecycle lease was held: %v", err)
	default:
	}

	release()
	waitForLifecycleStatusMutationResult(t, "bd lifecycle owner", ownerDone)
	waitForLifecycleStatusMutationResult(t, "BdStore.ReleaseIfCurrent", mutationDone)
	waitForLifecycleStatusMutationSignal(t, "bd release-if-current runner", runnerEntered)
	if got, want := mutationEnv[lifecycleMutationScopeEnv], closeTransitionScopeKey(scope); got != want {
		t.Fatalf("bd release-if-current lifecycle scope = %q, want %q", got, want)
	}
	if mutationEnv[lifecycleMutationTokenEnv] == "" {
		t.Fatal("bd release-if-current lifecycle token is empty")
	}
}

func TestBdStoreClaimWaitsForLifecycleLeaseAcrossHandles(t *testing.T) {
	scope := newLifecycleStatusMutationScope(t)
	legacyRunner := func(_ string, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("claim unexpectedly used legacy runner: %s %q", name, args)
	}
	runnerEntered := make(chan struct{})
	var mutationEnv map[string]string
	envRunner := func(_ string, name string, env map[string]string, args ...string) ([]byte, error) {
		mutationEnv = maps.Clone(env)
		close(runnerEntered)
		if name != "bd" || len(args) != 4 || args[0] != "update" || args[1] != "bd-42" || args[2] != "--claim" || args[3] != "--json" {
			return nil, fmt.Errorf("unexpected claim command: %s %q", name, args)
		}
		return []byte(`[{"id":"bd-42","status":"in_progress","assignee":"worker-1"}]`), nil
	}
	owner := NewBdStore(scope, legacyRunner, WithBdStoreCommandEnvRunner(envRunner))
	contender := NewBdStore(scope, legacyRunner, WithBdStoreCommandEnvRunner(envRunner))

	leaseEntered := make(chan struct{})
	releaseLease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseLease) }) }
	defer release()
	ownerDone := make(chan error, 1)
	go func() {
		ownerDone <- owner.WithLifecycleMetadataTransaction("bd-42", func(LifecycleMetadataTransaction) error {
			close(leaseEntered)
			<-releaseLease
			return nil
		})
	}()
	waitForLifecycleStatusMutationSignal(t, "bd lifecycle lease", leaseEntered)

	mutationDone := make(chan error, 1)
	go func() {
		_, claimed, err := contender.Claim("bd-42")
		if err == nil && !claimed {
			err = fmt.Errorf("Claim claimed = false, want true")
		}
		mutationDone <- err
	}()
	waitForLifecycleStatusMutationContender(t, scope, "BdStore.Claim", mutationDone)
	select {
	case <-runnerEntered:
		t.Fatal("bd claim reached its child command while lifecycle lease was held")
	case err := <-mutationDone:
		t.Fatalf("bd claim returned while lifecycle lease was held: %v", err)
	default:
	}

	release()
	waitForLifecycleStatusMutationResult(t, "bd lifecycle owner", ownerDone)
	waitForLifecycleStatusMutationResult(t, "BdStore.Claim", mutationDone)
	waitForLifecycleStatusMutationSignal(t, "bd claim runner", runnerEntered)
	if got, want := mutationEnv[lifecycleMutationScopeEnv], closeTransitionScopeKey(scope); got != want {
		t.Fatalf("bd claim lifecycle scope = %q, want %q", got, want)
	}
	if mutationEnv[lifecycleMutationTokenEnv] == "" {
		t.Fatal("bd claim lifecycle token is empty")
	}
}

func TestBdStoreTopologyMutationsWaitForLifecycleLeaseAcrossHandles(t *testing.T) {
	parentID := "bd-root"
	tests := []struct {
		name    string
		prepare func(*BdStore) error
		mutate  func(*BdStore) error
	}{
		{
			name: "CreateParented",
			mutate: func(store *BdStore) error {
				_, err := store.Create(Bead{ID: "bd-child", Title: "late child", ParentID: parentID})
				return err
			},
		},
		{
			name: "CreateRootOwned",
			mutate: func(store *BdStore) error {
				_, err := store.Create(Bead{
					ID:       "bd-child",
					Title:    "late root-owned child",
					Metadata: map[string]string{"gc.root_bead_id": parentID},
				})
				return err
			},
		},
		{
			name: "UpdateParent",
			mutate: func(store *BdStore) error {
				return store.Update("bd-child", UpdateOpts{ParentID: &parentID})
			},
		},
		{
			name: "UpdateAllParent",
			mutate: func(store *BdStore) error {
				_, err := store.UpdateAll([]string{"bd-child"}, UpdateOpts{ParentID: &parentID})
				return err
			},
		},
		{
			name: "UpdateIfMatchParent",
			prepare: func(store *BdStore) error {
				capable, err := store.conditionalWritesCapable()
				if err != nil {
					return err
				}
				if !capable {
					return errors.New("conditional writes unexpectedly unavailable")
				}
				return nil
			},
			mutate: func(store *BdStore) error {
				return store.UpdateIfMatch("bd-child", 7, UpdateOpts{ParentID: &parentID})
			},
		},
		{
			name: "CompareAndSetMetadata",
			prepare: func(store *BdStore) error {
				capable, err := store.conditionalWritesCapable()
				if err != nil {
					return err
				}
				if !capable {
					return errors.New("conditional writes unexpectedly unavailable")
				}
				return nil
			},
			mutate: func(store *BdStore) error {
				swapped, err := store.CompareAndSetMetadataKey("bd-child", "gc.root_bead_id", "", parentID)
				if err == nil && !swapped {
					return errors.New("CompareAndSetMetadataKey swapped = false, want true")
				}
				return err
			},
		},
		{
			name: "ApplyGraphPlan",
			mutate: func(store *BdStore) error {
				_, err := store.ApplyGraphPlan(t.Context(), &GraphApplyPlan{Nodes: []GraphApplyNode{
					{Key: "root", Title: "late root"},
					{Key: "child", Title: "late child", ParentKey: "root"},
				}})
				return err
			},
		},
		{
			name: "AddParentChildDependency",
			mutate: func(store *BdStore) error {
				return store.DepAdd("bd-child", parentID, "parent-child")
			},
		},
		{
			name: "RemoveDependency",
			mutate: func(store *BdStore) error {
				return store.DepRemove("bd-child", parentID)
			},
		},
		{
			name: "Delete",
			mutate: func(store *BdStore) error {
				return store.Delete("bd-child")
			},
		},
		{
			name: "DeleteBatch",
			mutate: func(store *BdStore) error {
				return store.DeleteBatch([]string{"bd-child", "bd-other"})
			},
		},
		{
			name: "DeleteIfMatch",
			prepare: func(store *BdStore) error {
				capable, err := store.conditionalWritesCapable()
				if err != nil {
					return err
				}
				if !capable {
					return errors.New("conditional writes unexpectedly unavailable")
				}
				return nil
			},
			mutate: func(store *BdStore) error {
				return store.DeleteIfMatch("bd-child", 7)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := newLifecycleStatusMutationScope(t)
			var runnerMu sync.Mutex
			var runnerOnce sync.Once
			var mutationEnv map[string]string
			runnerEntered := make(chan struct{})
			trackRunner := false

			run := func(env map[string]string, _ string, name string, args ...string) ([]byte, error) {
				if name != "bd" || len(args) == 0 {
					return nil, fmt.Errorf("unexpected command %s %q", name, args)
				}
				if len(args) == 2 && args[1] == "--help" {
					return []byte("Flags:\n      --if-revision int\n"), nil
				}
				if args[0] != "show" && args[0] != "query" {
					runnerMu.Lock()
					if trackRunner {
						mutationEnv = maps.Clone(env)
						runnerOnce.Do(func() { close(runnerEntered) })
					}
					runnerMu.Unlock()
				}
				switch {
				case args[0] == "show" || args[0] == "query":
					return []byte(`[{"id":"bd-child","title":"child","status":"open","issue_type":"task","revision":7,"metadata":{}}]`), nil
				case args[0] == "create" && len(args) > 1 && args[1] == "--graph":
					return []byte(`{"ids":{"root":"bd-root","child":"bd-child"}}`), nil
				case args[0] == "create":
					return []byte(`{"id":"bd-child","title":"late child","status":"open","issue_type":"task"}`), nil
				default:
					return []byte(`{}`), nil
				}
			}
			legacyRunner := func(dir, name string, args ...string) ([]byte, error) {
				return run(nil, dir, name, args...)
			}
			envRunner := func(dir, name string, env map[string]string, args ...string) ([]byte, error) {
				return run(env, dir, name, args...)
			}
			owner := NewBdStore(scope, legacyRunner, WithBdStoreCommandEnvRunner(envRunner))
			contender := NewBdStore(scope, legacyRunner, WithBdStoreCommandEnvRunner(envRunner))
			if tt.prepare != nil {
				if err := tt.prepare(contender); err != nil {
					t.Fatalf("prepare: %v", err)
				}
			}
			runnerMu.Lock()
			trackRunner = true
			runnerMu.Unlock()

			leaseEntered := make(chan struct{})
			releaseLease := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseLease) }) }
			defer release()
			ownerDone := make(chan error, 1)
			go func() {
				ownerDone <- owner.WithLifecycleMetadataTransaction(parentID, func(LifecycleMetadataTransaction) error {
					close(leaseEntered)
					<-releaseLease
					return nil
				})
			}()
			waitForLifecycleStatusMutationSignal(t, "bd lifecycle lease", leaseEntered)

			mutationDone := make(chan error, 1)
			go func() { mutationDone <- tt.mutate(contender) }()
			waitForLifecycleStatusMutationContender(t, scope, tt.name, mutationDone)
			select {
			case <-runnerEntered:
				t.Fatalf("%s reached its bd child while lifecycle lease was held", tt.name)
			case err := <-mutationDone:
				t.Fatalf("%s returned while lifecycle lease was held: %v", tt.name, err)
			default:
			}

			release()
			waitForLifecycleStatusMutationResult(t, "bd lifecycle owner", ownerDone)
			waitForLifecycleStatusMutationResult(t, tt.name, mutationDone)
			waitForLifecycleStatusMutationSignal(t, tt.name+" runner", runnerEntered)
			runnerMu.Lock()
			env := maps.Clone(mutationEnv)
			runnerMu.Unlock()
			if got, want := env[lifecycleMutationScopeEnv], closeTransitionScopeKey(scope); got != want {
				t.Fatalf("%s lifecycle scope = %q, want %q", tt.name, got, want)
			}
			if env[lifecycleMutationTokenEnv] == "" {
				t.Fatalf("%s lifecycle token is empty", tt.name)
			}
		})
	}
}

func TestBdUpdateRequiresLifecycleMutationLeaseOnlyForEligibilityFields(t *testing.T) {
	text := "value"
	priority := 1
	tests := []struct {
		name string
		opts UpdateOpts
		want bool
	}{
		{name: "empty", opts: UpdateOpts{}, want: false},
		{name: "title", opts: UpdateOpts{Title: &text}, want: false},
		{name: "priority", opts: UpdateOpts{Priority: &priority}, want: false},
		{name: "description", opts: UpdateOpts{Description: &text}, want: false},
		{name: "assignee", opts: UpdateOpts{Assignee: &text}, want: false},
		{name: "labels", opts: UpdateOpts{Labels: []string{"label"}}, want: false},
		{name: "remove labels", opts: UpdateOpts{RemoveLabels: []string{"label"}}, want: false},
		{name: "status", opts: UpdateOpts{Status: &text}, want: true},
		{name: "type", opts: UpdateOpts{Type: &text}, want: true},
		{name: "parent", opts: UpdateOpts{ParentID: &text}, want: true},
		{name: "metadata", opts: UpdateOpts{Metadata: map[string]string{"key": "value"}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bdUpdateRequiresLifecycleMutationLease(tt.opts); got != tt.want {
				t.Fatalf("bdUpdateRequiresLifecycleMutationLease() = %v, want %v", got, tt.want)
			}
		})
	}
}

func newLifecycleStatusMutationScope(t *testing.T) string {
	t.Helper()
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}
	return scope
}

func waitForLifecycleStatusMutationContender(t *testing.T, scope, operation string, done <-chan error) {
	t.Helper()
	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	key := closeTransitionScopeKey(scope)
	for {
		lifecycleMutationMutexRegistry.Lock()
		entry := lifecycleMutationMutexRegistry.entries[key]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		lifecycleMutationMutexRegistry.Unlock()
		if refs >= 2 {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("%s returned before joining the held lifecycle lease: %v", operation, err)
		case <-timer.C:
			t.Fatalf("%s lifecycle lease refs = %d, want at least 2", operation, refs)
		default:
			runtime.Gosched()
		}
	}
}

func waitForLifecycleStatusMutationSignal(t *testing.T, operation string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s did not occur", operation)
	}
}

func waitForLifecycleStatusMutationResult(t *testing.T, operation string, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s did not finish", operation)
	}
}
