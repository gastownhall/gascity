package beads

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestDirectStoreLifecycleCloseFencesEligibilityTopologyMutations(t *testing.T) {
	storeFactories := []struct {
		name string
		open func(*testing.T) (Store, Store)
	}{
		{
			name: "memory",
			open: func(*testing.T) (Store, Store) {
				store := NewMemStore()
				return store, store
			},
		},
		{
			name: "file replacement handle",
			open: func(t *testing.T) (Store, Store) {
				path := filepath.Join(t.TempDir(), "beads.json")
				owner, err := OpenFileStore(fsys.OSFS{}, path)
				if err != nil {
					t.Fatalf("Open owner FileStore: %v", err)
				}
				contender, err := OpenFileStore(fsys.OSFS{}, path)
				if err != nil {
					t.Fatalf("Open replacement FileStore: %v", err)
				}
				return owner, contender
			},
		},
	}

	for _, factory := range storeFactories {
		for _, mutation := range []string{
			"create descendant",
			"reparent descendant",
			"root metadata",
			"source status",
		} {
			t.Run(factory.name+"/"+mutation, func(t *testing.T) {
				owner, contender := factory.open(t)
				root, err := owner.Create(Bead{Title: "workflow root", Type: "molecule"})
				if err != nil {
					t.Fatalf("Create root: %v", err)
				}
				terminal, err := owner.Create(Bead{Title: "terminal descendant", Type: "step", ParentID: root.ID})
				if err != nil {
					t.Fatalf("Create terminal descendant: %v", err)
				}
				if err := owner.Close(terminal.ID); err != nil {
					t.Fatalf("Close terminal descendant: %v", err)
				}
				outsider, err := owner.Create(Bead{Title: "possible descendant", Type: "step"})
				if err != nil {
					t.Fatalf("Create possible descendant: %v", err)
				}
				source, err := owner.Create(Bead{Title: "source work"})
				if err != nil {
					t.Fatalf("Create source: %v", err)
				}
				if err := owner.Close(source.ID); err != nil {
					t.Fatalf("Close source: %v", err)
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
				go func() {
					switch mutation {
					case "create descendant":
						_, err := contender.Create(Bead{Title: "late descendant", Type: "step", ParentID: root.ID})
						mutationDone <- err
					case "reparent descendant":
						parentID := root.ID
						mutationDone <- contender.Update(outsider.ID, UpdateOpts{ParentID: &parentID})
					case "root metadata":
						mutationDone <- contender.SetMetadata(root.ID, "gc.source_bead_id", source.ID)
					case "source status":
						status := "open"
						mutationDone <- contender.Update(source.ID, UpdateOpts{Status: &status})
					}
				}()

				admittedBeforeClose := false
				select {
				case err := <-mutationDone:
					if err != nil {
						t.Fatalf("%s before root close: %v", mutation, err)
					}
					admittedBeforeClose = true
				case <-time.After(100 * time.Millisecond):
				}
				close(releaseClose)
				waitCacheLifecycleTopologyOperation(t, "direct lifecycle close", lifecycleDone)
				if !admittedBeforeClose {
					waitCacheLifecycleTopologyOperation(t, mutation, mutationDone)
				}
				if admittedBeforeClose {
					t.Fatalf("%s committed before the direct-store root close completed", mutation)
				}
			})
		}
	}
}
