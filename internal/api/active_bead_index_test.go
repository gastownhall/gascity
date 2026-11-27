package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestActiveBeadIndexPrecedenceAndScope pins the index's lookup order and
// scope: identity priority first, then store order, then newest-first within
// a store; exact assignee matching; a scoped lookup reads only its rig's
// store; a lookup with no identities loads nothing. Cold-rig exclusion is
// pinned by the suspendedRigLookup suite, which runs through the same index.
func TestActiveBeadIndexPrecedenceAndScope(t *testing.T) {
	newScopeState := func(t *testing.T) (*fakeState, *activeWorkCountingStore, *activeWorkCountingStore, *activeWorkCountingStore) {
		t.Helper()
		fs := newFakeState(t)
		early := &activeWorkCountingStore{Store: beads.NewMemStore()} // "aaa-rig"
		late := &activeWorkCountingStore{Store: beads.NewMemStore()}  // "zzz-rig"
		city := &activeWorkCountingStore{Store: beads.NewMemStore()}  // "test-city"
		fs.stores = map[string]beads.Store{"aaa-rig": early, "zzz-rig": late}
		fs.cityBeadStore = city
		return fs, early, late, city
	}

	t.Run("identity first, then store order, then newest within store", func(t *testing.T) {
		fs, early, late, city := newScopeState(t)
		mustCreateInProgress(t, early.Store, "older rig task", "worker")
		newer := mustCreateInProgress(t, early.Store, "newer rig task", "worker")
		mustCreateInProgress(t, late.Store, "late task", "worker")
		cityFallback := mustCreateInProgress(t, city.Store, "city fallback", "fallback")
		lateFallback := mustCreateInProgress(t, late.Store, "late fallback", "fallback")
		citySession := mustCreateInProgress(t, city.Store, "city work for session id", "session-id")
		rigTemplate := mustCreateInProgress(t, early.Store, "rig work for template", "template")

		idx := New(fs).newActiveBeadIndex()
		// Store order: aaa-rig sorts before test-city and zzz-rig, so the
		// newest aaa-rig bead wins over the later stores'.
		if got := idx.lookup("", "worker"); got != newer.ID {
			t.Errorf("lookup(worker) = %q, want aaa-rig's newest %s", got, newer.ID)
		}
		// Store order continues past the first store: test-city sorts before
		// zzz-rig, so the city's fallback bead outranks the rig's.
		if got := idx.lookup("", "fallback"); got != cityFallback.ID {
			t.Errorf("lookup(fallback) = %q, want test-city's %s over zzz-rig's %s", got, cityFallback.ID, lateFallback.ID)
		}
		// Identity order: the first identity's match wins even when a later
		// identity's bead sits in a store that sorts first.
		if got := idx.lookup("", "session-id", "template"); got != citySession.ID {
			t.Errorf("lookup(session-id, template) = %q, want session-id's %s over template's %s", got, citySession.ID, rigTemplate.ID)
		}
		if got := idx.lookup("", "template", "session-id"); got != rigTemplate.ID {
			t.Errorf("lookup(template, session-id) = %q, want template's %s", got, rigTemplate.ID)
		}
	})

	t.Run("exact assignee match", func(t *testing.T) {
		fs, early, _, _ := newScopeState(t)
		prefixed := mustCreateInProgress(t, early.Store, "other session's work", "worker-2")

		idx := New(fs).newActiveBeadIndex()
		if got := idx.lookup("", "worker"); got != "" {
			t.Errorf("lookup(worker) = %q, want none: matching is exact, and the store holds %s for worker-2", got, prefixed.ID)
		}
		if got := idx.lookup("", "worker-2"); got != prefixed.ID {
			t.Errorf("lookup(worker-2) = %q, want %s", got, prefixed.ID)
		}
	})

	t.Run("scoped lookup reads only its rig's store", func(t *testing.T) {
		fs, early, late, city := newScopeState(t)
		earlyWork := mustCreateInProgress(t, early.Store, "aaa work", "worker")
		zzzWork := mustCreateInProgress(t, late.Store, "zzz work", "worker")
		mustCreateInProgress(t, city.Store, "city work", "worker")

		idx := New(fs).newActiveBeadIndex()
		if got := idx.lookup("zzz-rig", "worker"); got != zzzWork.ID {
			t.Errorf("scoped lookup = %q, want zzz-rig's %s, not aaa-rig's %s", got, zzzWork.ID, earlyWork.ID)
		}
		if early.activeReads != 0 || city.activeReads != 0 {
			t.Errorf("a lookup scoped to zzz-rig read other stores: aaa-rig %d, test-city %d", early.activeReads, city.activeReads)
		}
	})

	t.Run("no identities loads nothing", func(t *testing.T) {
		fs, early, late, city := newScopeState(t)
		mustCreateInProgress(t, early.Store, "aaa work", "worker")

		idx := New(fs).newActiveBeadIndex()
		if got := idx.lookup("", ""); got != "" {
			t.Errorf("lookup of no identities = %q, want empty", got)
		}
		if early.activeReads != 0 || late.activeReads != 0 || city.activeReads != 0 {
			t.Errorf("a lookup with no identities read stores: aaa-rig %d, zzz-rig %d, test-city %d", early.activeReads, late.activeReads, city.activeReads)
		}
	})
}
