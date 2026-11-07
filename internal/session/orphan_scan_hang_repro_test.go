package session

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// hangingScanFake is a Fake whose process-table scan never returns until the
// test releases it, as a wedged process-table read would.
type hangingScanFake struct {
	*runtime.Fake
	release chan struct{}
}

func (f *hangingScanFake) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	<-f.release
	return nil, nil
}

// A pre-start orphan scan that never finishes must not wedge the start: once
// the start context's deadline passes, killExistingOrphans must return an
// error (the start defers) rather than block forever.
func TestKillExistingOrphansReturnsWhenScanNeverFinishes(t *testing.T) {
	sp := &hangingScanFake{Fake: runtime.NewFake(), release: make(chan struct{})}
	t.Cleanup(func() { close(sp.release) })
	mgr := newTestManager(t, beads.NewMemStore(), sp)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mgr.killExistingOrphans(ctx, "sid-wedged") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("killExistingOrphans = nil for an unfinished scan, want an error so the start defers")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("killExistingOrphans still blocked 5s after the start context's 50ms deadline: the wedged scan is unbounded")
	}
}
