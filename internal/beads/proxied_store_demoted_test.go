package beads

import (
	"context"
	"testing"
)

// demotedHeadMoved is the verdict a long-lived open's native leaf answers when
// the library open itself moved HEAD: non-terminal, so the store starts on the
// bd leaf and the guard owns getting it back.
func demotedHeadMoved() *ProxiedVerdictError {
	return NewNonTerminalProxiedVerdictError(ProxiedVerdictHeadMoved,
		"opening the linked library moved HEAD", nil)
}

// startDemoted replaces the guard fixture's store with one that never had a
// native leaf, over the same admitted pin, bd leaf and recovery.
func (f *guardFixture) startDemoted(verdict *ProxiedVerdictError) {
	f.t.Helper()
	store, err := NewDemotedProxiedStore(f.bd, f.store.Pin(), verdict, WithNativeLeafReopener(
		func(ctx context.Context) (*NativeDoltStore, Pin, error) {
			f.recoveries++
			if f.recover == nil {
				return nil, Pin{}, NewNonTerminalProxiedVerdictError(ProxiedVerdictHeadMoved,
					"this fixture installed no recovery", nil)
			}
			return f.recover(ctx)
		}))
	if err != nil {
		f.t.Fatalf("NewDemotedProxiedStore: %v", err)
	}
	f.store = store
	f.native = nil
	f.start()
}

func TestNewDemotedProxiedStoreRefusesWhatCannotRecover(t *testing.T) {
	f := newGuardFixture(t)
	pin := f.store.Pin()
	reopener := WithNativeLeafReopener(func(context.Context) (*NativeDoltStore, Pin, error) {
		return nil, Pin{}, nil
	})
	terminal := NewSchemaSkewVerdictError(ProxiedSkewLaneIgnored, ProxiedSkewDirBehind, "the database moved")

	for _, tc := range []struct {
		name    string
		bd      Store
		pin     Pin
		verdict *ProxiedVerdictError
		opts    []ProxiedStoreOption
	}{
		{name: "nil bd leaf", bd: nil, pin: pin, verdict: demotedHeadMoved(), opts: []ProxiedStoreOption{reopener}},
		{name: "unadmitted pin", bd: f.bd, pin: Pin{}, verdict: demotedHeadMoved(), opts: []ProxiedStoreOption{reopener}},
		{name: "nil verdict", bd: f.bd, pin: pin, verdict: nil, opts: []ProxiedStoreOption{reopener}},
		{name: "terminal verdict", bd: f.bd, pin: pin, verdict: terminal, opts: []ProxiedStoreOption{reopener}},
		{name: "no reopener", bd: f.bd, pin: pin, verdict: demotedHeadMoved()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := NewDemotedProxiedStore(tc.bd, tc.pin, tc.verdict, tc.opts...)
			if err == nil {
				t.Fatalf("NewDemotedProxiedStore built %v; a store that cannot recover must be refused", store)
			}
		})
	}

	store, err := NewDemotedProxiedStore(f.bd, pin, demotedHeadMoved(), reopener)
	if err != nil {
		t.Fatalf("NewDemotedProxiedStore: %v", err)
	}
	if !store.Demoted() {
		t.Fatal("a store built demoted reports a native leaf")
	}
	if verdict := store.Verdict(); verdict == nil || verdict.Verdict != ProxiedVerdictHeadMoved {
		t.Fatalf("verdict = %v, want the head_moved it was built with", verdict)
	}
	if !store.Report().Demoted {
		t.Fatal("the report does not say demoted")
	}
	if store.BdLeaf() != Store(f.bd) {
		t.Fatal("the demoted store does not hold the bd leaf it was given")
	}
}

func TestDemotedProxiedStoreRecoversOnGuardTick(t *testing.T) {
	t.Run("a refused recovery stays demoted, a served one re-pins on the same bd leaf", func(t *testing.T) {
		f := newGuardFixture(t)
		bd := f.bd
		f.startDemoted(demotedHeadMoved())

		if step := f.tick(); step != proxiedGuardUndecided {
			t.Fatalf("a head_moved recovery reported %s, want undecided", step)
		}
		if !f.store.Demoted() {
			t.Fatal("the store promoted itself on a refused recovery")
		}

		replacement := newNativeDoltStoreForTest(&nativeDoltMemStorage{
			store: &MemStore{IDPrefix: "prx", HonorExplicitIDs: true},
		})
		replacement.idPrefix = "prx"
		created, err := replacement.Create(Bead{Title: "served by the recovered leaf"})
		if err != nil {
			t.Fatalf("seeding the replacement leaf: %v", err)
		}
		f.recover = func(context.Context) (*NativeDoltStore, Pin, error) {
			return replacement, f.store.Pin(), nil
		}

		if step := f.tick(); step != proxiedGuardRepinned {
			t.Fatalf("the recovery tick reported %s, want repinned", step)
		}
		if f.store.Demoted() {
			t.Fatal("the recovery re-pinned and left the store on the bd leaf")
		}
		if f.store.Verdict() != nil {
			t.Fatalf("the re-pin left the opening verdict in place: %v", f.store.Verdict())
		}
		if f.store.BdLeaf() != Store(bd) {
			t.Fatal("the re-pin replaced the bd leaf")
		}
		bd.took()
		got, err := f.store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get after the re-pin: %v", err)
		}
		if got.Title != created.Title {
			t.Fatalf("Get answered %q, want the recovered leaf's %q", got.Title, created.Title)
		}
		if calls := bd.took(); len(calls) != 0 {
			t.Fatalf("a read after the re-pin went to the bd leaf: %v", calls)
		}
		if f.recoveries != 2 {
			t.Fatalf("the guard asked %d time(s), want 2", f.recoveries)
		}
	})

	t.Run("a terminal recovery latches and the tick stops", func(t *testing.T) {
		f := newGuardFixture(t)
		f.recover = func(context.Context) (*NativeDoltStore, Pin, error) {
			return nil, Pin{}, NewSchemaSkewVerdictError(ProxiedSkewLaneIgnored, ProxiedSkewDirBehind,
				"the database moved while this handle was demoted")
		}
		f.startDemoted(demotedHeadMoved())

		if step := f.tick(); step != proxiedGuardStoodDown {
			t.Fatalf("a terminal recovery reported %s, want stood-down", step)
		}
		verdict := f.store.Verdict()
		if verdict == nil || verdict.Verdict != ProxiedVerdictSchemaSkew || !verdict.Terminal() {
			t.Fatalf("verdict = %v, want a terminal schema_skew", verdict)
		}
		if step := f.tick(); step != proxiedGuardStopped {
			t.Fatalf("the tick after a terminal latch reported %s, want stopped", step)
		}
		if f.recoveries != 1 {
			t.Fatalf("the guard asked %d time(s) after a terminal refusal, want 1", f.recoveries)
		}
		if !f.store.Demoted() {
			t.Fatal("a latched store serves natively")
		}
	})
}
