package beads_test

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

type closeTransitionStorePair struct {
	primary beads.Store
	peer    beads.Store
}

func forEachCloseTransitionStore(t *testing.T, test func(*testing.T, closeTransitionStorePair)) {
	t.Helper()

	tests := []struct {
		name string
		open func(*testing.T) closeTransitionStorePair
	}{
		{
			name: "MemStore",
			open: func(*testing.T) closeTransitionStorePair {
				store := beads.NewMemStore()
				return closeTransitionStorePair{primary: store, peer: store}
			},
		},
		{
			name: "FileStore",
			open: func(t *testing.T) closeTransitionStorePair {
				path := filepath.Join(t.TempDir(), "beads.json")
				primary, err := beads.OpenFileStore(fsys.OSFS{}, path)
				if err != nil {
					t.Fatalf("OpenFileStore(primary): %v", err)
				}
				peer, err := beads.OpenFileStore(fsys.OSFS{}, path)
				if err != nil {
					t.Fatalf("OpenFileStore(peer): %v", err)
				}
				return closeTransitionStorePair{primary: primary, peer: peer}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			test(t, tt.open(t))
		})
	}
}

func requireCloseTransitioner(t *testing.T, store beads.Store) beads.CloseTransitioner {
	t.Helper()
	closer, ok := beads.CloseTransitionerFor(store)
	if !ok {
		t.Fatalf("CloseTransitionerFor(%T) reported unsupported", store)
	}
	return closer
}

func createCloseTransitionBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{
		Title:    "atomic close target",
		Metadata: beads.StringMap{"existing": "kept"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func getCloseTransitionBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return got
}

func TestCloseTransitionerFirstCloseReturnsExactSnapshots(t *testing.T) {
	forEachCloseTransitionStore(t, func(t *testing.T, stores closeTransitionStorePair) {
		created := createCloseTransitionBead(t, stores.primary)
		before := getCloseTransitionBead(t, stores.peer, created.ID)
		closer := requireCloseTransitioner(t, stores.primary)

		transition, err := closer.CloseWithReasonIfOpen(before.ID, "all children closed")
		if err != nil {
			t.Fatalf("CloseWithReasonIfOpen: %v", err)
		}
		if !transition.Transitioned {
			t.Fatal("CloseWithReasonIfOpen Transitioned = false, want true")
		}
		if transition.ObserverNotified {
			t.Fatal("CloseWithReasonIfOpen ObserverNotified = true for a bare store, want false")
		}
		if !reflect.DeepEqual(transition.Before, before) {
			t.Fatalf("CloseWithReasonIfOpen Before = %#v, want exact pre-close snapshot %#v", transition.Before, before)
		}

		durable := getCloseTransitionBead(t, stores.peer, before.ID)
		if !reflect.DeepEqual(transition.After, durable) {
			t.Fatalf("CloseWithReasonIfOpen After = %#v, want exact durable snapshot %#v", transition.After, durable)
		}
		if transition.After.Status != "closed" {
			t.Errorf("After.Status = %q, want closed", transition.After.Status)
		}
		if got := transition.After.Metadata["close_reason"]; got != "all children closed" {
			t.Errorf("After.Metadata[close_reason] = %q, want %q", got, "all children closed")
		}
		if got := transition.After.Metadata["existing"]; got != "kept" {
			t.Errorf("After.Metadata[existing] = %q, want kept", got)
		}
		if transition.After.Revision != before.Revision+1 {
			t.Errorf("After.Revision = %d, want before revision %d + 1", transition.After.Revision, before.Revision)
		}
	})
}

func TestCloseTransitionerRepeatClosePreservesFirstReason(t *testing.T) {
	forEachCloseTransitionStore(t, func(t *testing.T, stores closeTransitionStorePair) {
		created := createCloseTransitionBead(t, stores.primary)
		firstCloser := requireCloseTransitioner(t, stores.primary)
		first, err := firstCloser.CloseWithReasonIfOpen(created.ID, "first reason wins")
		if err != nil {
			t.Fatalf("first CloseWithReasonIfOpen: %v", err)
		}
		if !first.Transitioned {
			t.Fatal("first CloseWithReasonIfOpen Transitioned = false, want true")
		}

		repeatCloser := requireCloseTransitioner(t, stores.peer)
		repeat, err := repeatCloser.CloseWithReasonIfOpen(created.ID, "later reason must lose")
		if err != nil {
			t.Fatalf("repeat CloseWithReasonIfOpen: %v", err)
		}
		if repeat.Transitioned {
			t.Fatal("repeat CloseWithReasonIfOpen Transitioned = true, want false")
		}
		if repeat.ObserverNotified {
			t.Fatal("repeat CloseWithReasonIfOpen ObserverNotified = true for a bare store, want false")
		}

		durable := getCloseTransitionBead(t, stores.primary, created.ID)
		if got := durable.Metadata["close_reason"]; got != "first reason wins" {
			t.Fatalf("durable close reason = %q, want first reason %q", got, "first reason wins")
		}
		if durable.Revision != first.After.Revision {
			t.Fatalf("repeat close changed revision from %d to %d", first.After.Revision, durable.Revision)
		}
		if !reflect.DeepEqual(repeat.After, durable) {
			t.Fatalf("repeat CloseWithReasonIfOpen After = %#v, want durable winner %#v", repeat.After, durable)
		}
	})
}

func TestCloseTransitionerConcurrentClosesHaveOneDurableWinner(t *testing.T) {
	forEachCloseTransitionStore(t, func(t *testing.T, stores closeTransitionStorePair) {
		created := createCloseTransitionBead(t, stores.primary)
		closers := []beads.CloseTransitioner{
			requireCloseTransitioner(t, stores.primary),
			requireCloseTransitioner(t, stores.peer),
		}
		reasons := []string{"concurrent reason one", "concurrent reason two"}

		type closeResult struct {
			reason     string
			transition beads.CloseTransition
			err        error
		}
		start := make(chan struct{})
		results := make(chan closeResult, len(closers))
		var ready sync.WaitGroup
		ready.Add(len(closers))
		for i := range closers {
			go func(closer beads.CloseTransitioner, reason string) {
				ready.Done()
				<-start
				transition, err := closer.CloseWithReasonIfOpen(created.ID, reason)
				results <- closeResult{reason: reason, transition: transition, err: err}
			}(closers[i], reasons[i])
		}
		ready.Wait()
		close(start)

		var winner *closeResult
		var allResults []closeResult
		for range closers {
			result := <-results
			if result.err != nil {
				t.Fatalf("CloseWithReasonIfOpen(%q): %v", result.reason, result.err)
			}
			allResults = append(allResults, result)
			if result.transition.Transitioned {
				if winner != nil {
					t.Fatalf("multiple closes transitioned: %q and %q", winner.reason, result.reason)
				}
				resultCopy := result
				winner = &resultCopy
			}
		}
		if winner == nil {
			t.Fatal("no concurrent close transitioned, want exactly one")
		}

		durable := getCloseTransitionBead(t, stores.primary, created.ID)
		if got := durable.Metadata["close_reason"]; got != winner.reason {
			t.Fatalf("durable close reason = %q, want winning reason %q", got, winner.reason)
		}
		if durable.Revision != created.Revision+1 {
			t.Fatalf("durable revision = %d, want exactly one bump from %d", durable.Revision, created.Revision)
		}
		if !reflect.DeepEqual(winner.transition.After, durable) {
			t.Fatalf("winning After = %#v, want exact durable snapshot %#v", winner.transition.After, durable)
		}
		for _, result := range allResults {
			if result.transition.ObserverNotified {
				t.Errorf("CloseWithReasonIfOpen(%q) ObserverNotified = true for a bare store, want false", result.reason)
			}
			if !reflect.DeepEqual(result.transition.After, durable) {
				t.Errorf("CloseWithReasonIfOpen(%q) After = %#v, want durable winner %#v", result.reason, result.transition.After, durable)
			}
		}
	})
}
