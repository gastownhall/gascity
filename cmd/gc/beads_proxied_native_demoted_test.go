package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// demotedOpenFixture is an opener over an admitted scope whose library open
// moved HEAD in a window gc cannot attribute, with the leaf and the bd opens
// counted.
type demotedOpenFixture struct {
	opener  *proxiedNativeOpener
	leaf    *closeCountingStorage
	bd      *beads.MemStore
	bdOpens int
}

func newDemotedOpenFixture(t *testing.T) *demotedOpenFixture {
	t.Helper()
	f := newProxiedScopeFixture(t)
	beads.ForgetProxiedPin(f.scopeRoot, "beads")
	t.Cleanup(func() { beads.ForgetProxiedPin(f.scopeRoot, "beads") })
	d := &demotedOpenFixture{leaf: &closeCountingStorage{}, bd: beads.NewMemStore()}
	d.opener = &proxiedNativeOpener{
		cityPath:     t.TempDir(),
		scopeRoot:    f.scopeRoot,
		database:     "beads",
		processTable: f.processTable(),
		probe: func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			served := proxyendpoint.ServedProbeForTest(f.pinnedCursors(), proxyendpoint.CursorReality{})
			served.Head = "before0000"
			return served
		},
		observed:  beads.NewGenerationSet(),
		recovered: beads.NewGenerationSet(),
		sleep:     func(context.Context, time.Duration) error { return nil },
		openNative: func(context.Context, string, map[string]string, ...beads.NativeDoltStoreOption) (*beads.NativeDoltStore, error) {
			return beads.NewNativeDoltStoreOverStorageForTest(d.leaf), nil
		},
		leafObserve: func(context.Context, *beads.NativeDoltStore, string) (proxyendpoint.PostOpenReport, error) {
			return observedHead("after11111"), nil
		},
		openBd: func() (beads.Store, error) {
			d.bdOpens++
			return d.bd, nil
		},
	}
	return d
}

func TestProxiedLongLivedOpenServesADemotedStore(t *testing.T) {
	t.Run("a non-terminal leaf verdict serves the bd leaf under a running guard", func(t *testing.T) {
		d := newDemotedOpenFixture(t)

		got, _, err := d.opener.open(context.Background(), true)
		if err != nil {
			t.Fatalf("a long-lived open whose leaf answered head_moved returned %v; want a demoted store", err)
		}
		store, ok := got.(*beads.ProxiedStore)
		if !ok {
			t.Fatalf("open returned %T, want *beads.ProxiedStore", got)
		}
		t.Cleanup(func() { _ = store.CloseStore() })
		if !store.Demoted() {
			t.Fatal("the store serves a native leaf the open refused")
		}
		if verdict := store.Verdict(); verdict == nil || verdict.Verdict != beads.ProxiedVerdictHeadMoved || verdict.Terminal() {
			t.Fatalf("verdict = %v, want the leaf's non-terminal head_moved", verdict)
		}
		if store.BdLeaf() != beads.Store(d.bd) || d.bdOpens != 1 {
			t.Fatalf("the store holds bd leaf %v after %d bd open(s), want the one bd leaf", store.BdLeaf(), d.bdOpens)
		}
		if !store.GuardArmedForTest() {
			t.Fatal("the demoted store has no running guard holding the leaf reopener, so it can never re-pin")
		}
		if d.leaf.closed != 1 {
			t.Fatalf("the refused leaf was closed %d time(s), want exactly 1", d.leaf.closed)
		}
		if err := store.CloseStore(); err != nil {
			t.Fatalf("CloseStore: %v", err)
		}
		if d.leaf.closed != 1 {
			t.Fatalf("closing the demoted store closed the refused leaf again (%d closes)", d.leaf.closed)
		}
	})

	t.Run("a terminal leaf verdict still takes the bd front door", func(t *testing.T) {
		d := newDemotedOpenFixture(t)
		d.opener.openNative = func(context.Context, string, map[string]string, ...beads.NativeDoltStoreOption) (*beads.NativeDoltStore, error) {
			return nil, beads.NewSchemaSkewVerdictError(beads.ProxiedSkewLaneIgnored, beads.ProxiedSkewDirBehind,
				"the database moved")
		}

		got, _, err := d.opener.open(context.Background(), true)
		verdict, typed := beads.ProxiedVerdictOf(err)
		if got != nil || !typed || !verdict.Terminal() {
			t.Fatalf("open = (%v, %v), want no store and the terminal verdict", got, err)
		}
		if d.bdOpens != 0 {
			t.Fatalf("a terminal verdict opened the bd leaf %d time(s); the factory opens its own", d.bdOpens)
		}
	})

	t.Run("a bd leaf that cannot open is returned, not hidden behind the verdict", func(t *testing.T) {
		d := newDemotedOpenFixture(t)
		d.opener.openBd = func() (beads.Store, error) { return nil, errors.New("bd is not on PATH") }

		got, _, err := d.opener.open(context.Background(), true)
		if got != nil || err == nil || err.Error() != "bd is not on PATH" {
			t.Fatalf("open = (%v, %v), want no store and the bd open's error", got, err)
		}
		if d.leaf.closed != 1 {
			t.Fatalf("the refused leaf was closed %d time(s), want exactly 1", d.leaf.closed)
		}
	})
}

// TestProxiedOneShotOpenKeepsTheBdFrontDoor is the other half of the demoted
// store: a one-shot open has no guard to re-pin anything, so a leaf that
// answers head_moved still hands the factory the verdict and no store, and the
// factory takes the bd front door exactly as it did before demoted stores.
func TestProxiedOneShotOpenKeepsTheBdFrontDoor(t *testing.T) {
	d := newDemotedOpenFixture(t)

	got, _, err := d.opener.open(context.Background(), false)
	verdict, typed := beads.ProxiedVerdictOf(err)
	if got != nil {
		t.Fatalf("a one-shot open served %T; want no store, so the factory takes the bd front door", got)
	}
	if !typed || verdict.Verdict != beads.ProxiedVerdictHeadMoved || verdict.Terminal() {
		t.Fatalf("open err = %v, want the leaf's non-terminal head_moved", err)
	}
	if d.bdOpens != 0 {
		t.Fatalf("a one-shot open opened the bd leaf %d time(s); the factory opens its own", d.bdOpens)
	}
	if d.leaf.closed != 1 {
		t.Fatalf("the refused leaf was closed %d time(s), want exactly 1", d.leaf.closed)
	}
}
