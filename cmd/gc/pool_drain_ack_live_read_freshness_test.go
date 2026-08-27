package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestControllerCityStoreLiveHandleObservesForeignClose pins the read contract
// the keyed drain-finalize guard depends on: the controller's city store is a
// CachingStore (api_state.go's wrapWithCachingStore), whose cached view cannot
// see a close another process committed until the reconciler catches up, so the
// guard reads through beads.HandlesFor(store).Live. That Live handle must reach
// the backing store, not the cache — otherwise the guard sees status=open for a
// trigger the worker durably closed and burns its whole finalize budget
// (ga-f7v2ft.131).
func TestControllerCityStoreLiveHandleObservesForeignClose(t *testing.T) {
	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{Title: "routed trigger", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// A background-refresh-free cache is the deterministic stand-in for "the
	// reconciler has not run since the foreign write".
	cityStore := wrapWithCachingStore(context.Background(), backing, nil, false)
	if cityStore == nil {
		t.Fatal("wrapWithCachingStore returned nil")
	}
	primed, err := cityStore.Get(work.ID)
	if err != nil {
		t.Fatalf("prime cache on open trigger: %v", err)
	}
	if primed.Status != "open" {
		t.Fatalf("primed trigger status = %q, want open", primed.Status)
	}

	// The worker closes its own trigger in another process: the controller's
	// cache never sees the write.
	if err := backing.Close(work.ID); err != nil {
		t.Fatalf("foreign close of trigger: %v", err)
	}
	stale, err := cityStore.Get(work.ID)
	if err != nil {
		t.Fatalf("cached read after foreign close: %v", err)
	}
	if stale.Status != "open" {
		t.Fatalf("cached read status = %q, want the stale open this test exists to out-run", stale.Status)
	}

	live, err := beads.HandlesFor(cityStore).Live.Get(work.ID)
	if err != nil {
		t.Fatalf("live read after foreign close: %v", err)
	}
	if live.Status != "closed" {
		t.Fatalf("live read status = %q, want closed: the drain-finalize guard's Live handle must observe a commit the writer completed (ga-f7v2ft.131)", live.Status)
	}
}
