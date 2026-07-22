package beads

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestNativeDoltLifecycleCloseFencesEligibilityTopologyAcrossReplacementHandles(t *testing.T) {
	for _, mutation := range []string{
		"create descendant",
		"reparent descendant",
		"add parent-child dependency",
		"remove parent-child dependency",
		"delete descendant",
		"root metadata",
		"source status",
	} {
		t.Run(mutation, func(t *testing.T) {
			scope := t.TempDir()
			if err := os.Mkdir(filepath.Join(scope, ".beads"), 0o755); err != nil {
				t.Fatalf("create native lifecycle scope: %v", err)
			}
			shared := NewMemStore()
			owner := newNativeDoltStoreForTest(&nativeDoltMemStorage{store: shared})
			contender := newNativeDoltStoreForTest(&nativeDoltMemStorage{store: shared})
			owner.scopeRoot = scope
			contender.scopeRoot = scope

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
				lifecycleDone <- owner.WithLifecycleMetadataTransaction(root.ID, func(tx LifecycleMetadataTransaction) error {
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
				t.Fatal("native lifecycle transaction did not reach final eligibility window")
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
				case "add parent-child dependency":
					mutationDone <- contender.DepAdd(outsider.ID, root.ID, "parent-child")
				case "remove parent-child dependency":
					mutationDone <- contender.DepRemove(terminal.ID, root.ID)
				case "delete descendant":
					mutationDone <- contender.Delete(terminal.ID)
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
			waitNativeLifecycleResult(t, "native lifecycle close", lifecycleDone)
			if !admittedBeforeClose {
				waitNativeLifecycleResult(t, mutation, mutationDone)
			}
			if admittedBeforeClose {
				t.Fatalf("%s committed before the native root close completed", mutation)
			}
		})
	}
}
