package beads

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

type cacheLifecycleTopologyBacking struct {
	*MemStore
	scope string

	createEntered     chan struct{}
	reparentEntered   chan struct{}
	metadataEntered   chan struct{}
	statusEntered     chan struct{}
	typeEntered       chan struct{}
	dependencyEntered chan struct{}
	createOnce        sync.Once
	reparentOnce      sync.Once
	metadataOnce      sync.Once
	statusOnce        sync.Once
	typeOnce          sync.Once
	dependencyOnce    sync.Once
}

func (s *cacheLifecycleTopologyBacking) CacheMutationScope() string { return s.scope }

func (s *cacheLifecycleTopologyBacking) WithLifecycleMetadataTransaction(
	id string,
	fn func(LifecycleMetadataTransaction) error,
) error {
	return fn(lifecycleMetadataDirectTransaction{
		id:     id,
		store:  s.MemStore,
		reader: s.MemStore,
		writer: s.MemStore,
	})
}

func (s *cacheLifecycleTopologyBacking) Create(bead Bead) (Bead, error) {
	signalCacheLifecycleTopologyMutation(s.createEntered, &s.createOnce)
	return s.MemStore.Create(bead)
}

func (s *cacheLifecycleTopologyBacking) Update(id string, opts UpdateOpts) error {
	if opts.ParentID != nil {
		signalCacheLifecycleTopologyMutation(s.reparentEntered, &s.reparentOnce)
	}
	if opts.Status != nil {
		signalCacheLifecycleTopologyMutation(s.statusEntered, &s.statusOnce)
	}
	if opts.Type != nil {
		signalCacheLifecycleTopologyMutation(s.typeEntered, &s.typeOnce)
	}
	return s.MemStore.Update(id, opts)
}

func (s *cacheLifecycleTopologyBacking) DepAdd(issueID, dependsOnID, depType string) error {
	if depType == "parent-child" {
		signalCacheLifecycleTopologyMutation(s.dependencyEntered, &s.dependencyOnce)
	}
	return s.MemStore.DepAdd(issueID, dependsOnID, depType)
}

func (s *cacheLifecycleTopologyBacking) SetMetadata(id, key, value string) error {
	signalCacheLifecycleTopologyMutation(s.metadataEntered, &s.metadataOnce)
	return s.MemStore.SetMetadata(id, key, value)
}

func signalCacheLifecycleTopologyMutation(ch chan struct{}, once *sync.Once) {
	if ch != nil {
		once.Do(func() { close(ch) })
	}
}

func waitCacheLifecycleTopologyOperation(t *testing.T, operation string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s did not complete", operation)
	}
}

func TestCachingStoreLifecycleCloseFencesEligibilityTopologyAcrossReplacementHandles(t *testing.T) {
	for _, mutation := range []string{
		"create descendant",
		"reparent descendant",
		"retype root",
		"add parent-child dependency",
		"root metadata",
		"source status",
	} {
		t.Run(mutation, func(t *testing.T) {
			base := NewMemStore()
			root, err := base.Create(Bead{Title: "workflow root", Type: "molecule"})
			if err != nil {
				t.Fatalf("Create root: %v", err)
			}
			terminal, err := base.Create(Bead{Title: "terminal descendant", Type: "step", ParentID: root.ID})
			if err != nil {
				t.Fatalf("Create terminal descendant: %v", err)
			}
			if err := base.Close(terminal.ID); err != nil {
				t.Fatalf("Close terminal descendant: %v", err)
			}
			outsider, err := base.Create(Bead{Title: "possible descendant", Type: "step"})
			if err != nil {
				t.Fatalf("Create possible descendant: %v", err)
			}
			source, err := base.Create(Bead{Title: "source work"})
			if err != nil {
				t.Fatalf("Create source: %v", err)
			}
			if err := base.Close(source.ID); err != nil {
				t.Fatalf("Close source: %v", err)
			}

			scope := filepath.Join(t.TempDir(), "durable-city")
			ownerBacking := &cacheLifecycleTopologyBacking{MemStore: base, scope: scope}
			contenderBacking := &cacheLifecycleTopologyBacking{
				MemStore:          base,
				scope:             scope,
				createEntered:     make(chan struct{}),
				reparentEntered:   make(chan struct{}),
				metadataEntered:   make(chan struct{}),
				statusEntered:     make(chan struct{}),
				typeEntered:       make(chan struct{}),
				dependencyEntered: make(chan struct{}),
			}
			owner := NewCachingStoreForTest(ownerBacking, nil)
			contender := NewCachingStoreForTest(contenderBacking, nil)
			for name, cache := range map[string]*CachingStore{"owner": owner, "replacement": contender} {
				if err := cache.Prime(context.Background()); err != nil {
					t.Fatalf("Prime %s cache: %v", name, err)
				}
			}

			eligibilityChecked := make(chan struct{})
			releaseClose := make(chan struct{})
			lifecycleDone := make(chan error, 1)
			go func() {
				lifecycleDone <- WithLifecycleMetadataTransaction(owner, root.ID, func(tx LifecycleMetadataTransaction) error {
					fresh, err := tx.Get()
					if err != nil {
						return err
					}
					if fresh.Status != "open" {
						return fmt.Errorf("root status before close = %q, want open", fresh.Status)
					}
					// Model the final descendant/source eligibility check. No topology
					// mutation may commit after this point and before the root close.
					close(eligibilityChecked)
					<-releaseClose
					result, err := CloseWithinLifecycleMetadataTransaction(tx, "eligible lifecycle close")
					if err != nil {
						return err
					}
					if !result.AuthoritativeClosed(root.ID) {
						return fmt.Errorf("root close was not authoritative: %+v", result)
					}
					return nil
				})
			}()
			select {
			case <-eligibilityChecked:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("lifecycle transaction did not reach final eligibility window")
			}

			mutationDone := make(chan error, 1)
			var entered <-chan struct{}
			switch mutation {
			case "create descendant":
				entered = contenderBacking.createEntered
				go func() {
					_, err := contender.Create(Bead{Title: "late descendant", Type: "step", ParentID: root.ID})
					mutationDone <- err
				}()
			case "reparent descendant":
				entered = contenderBacking.reparentEntered
				go func() {
					parentID := root.ID
					mutationDone <- contender.Update(outsider.ID, UpdateOpts{ParentID: &parentID})
				}()
			case "retype root":
				entered = contenderBacking.typeEntered
				go func() {
					typeTask := "task"
					mutationDone <- contender.Update(root.ID, UpdateOpts{Type: &typeTask})
				}()
			case "add parent-child dependency":
				entered = contenderBacking.dependencyEntered
				go func() {
					mutationDone <- contender.DepAdd(outsider.ID, root.ID, "parent-child")
				}()
			case "root metadata":
				entered = contenderBacking.metadataEntered
				go func() {
					mutationDone <- contender.SetMetadata(root.ID, "gc.source_bead_id", source.ID)
				}()
			case "source status":
				entered = contenderBacking.statusEntered
				go func() {
					status := "open"
					mutationDone <- contender.Update(source.ID, UpdateOpts{Status: &status})
				}()
			}

			admittedBeforeClose := false
			select {
			case <-entered:
				admittedBeforeClose = true
			case <-time.After(100 * time.Millisecond):
			}
			close(releaseClose)
			waitCacheLifecycleTopologyOperation(t, "lifecycle close", lifecycleDone)
			waitCacheLifecycleTopologyOperation(t, mutation, mutationDone)
			if admittedBeforeClose {
				t.Fatalf("%s reached the replacement backing before the root close completed", mutation)
			}
		})
	}
}
