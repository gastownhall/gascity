package beads

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// TestProxiedStoreFromFindsTheSplitStoreThroughWrappers is the seam's own test,
// on the REAL split store rather than a stand-in: every caller of the seam holds
// the store through at least one wrapper that embeds the beads.Store interface,
// and the whole point of the seam is that such a wrapper no longer hides the
// question.
func TestProxiedStoreFromFindsTheSplitStoreThroughWrappers(t *testing.T) {
	store, _, _ := newSplitFixture(t)

	view, ok := ProxiedStoreFrom(store)
	if !ok || view.Demoted() {
		t.Fatalf("ProxiedStoreFrom(split store) = (%v, %v), want the view of a serving store", view, ok)
	}
	if _, ok := ProxiedStoreFrom(NewCachingStoreForTest(store, nil)); !ok {
		t.Fatal("ProxiedStoreFrom did not see through a CachingStore")
	}
	if _, ok := ProxiedStoreFrom(NewMemStore()); ok {
		t.Fatal("a MemStore answered as a proxied store")
	}
	if _, ok := ProxiedStoreFrom(nil); ok {
		t.Fatal("a nil store answered as a proxied store")
	}
}

// TestLiveProxiedDiagnosticPrefersTheHandleOverTheOpen is the difference the seam
// exists to expose: the open's account is frozen, the handle's is not.
func TestLiveProxiedDiagnosticPrefersTheHandleOverTheOpen(t *testing.T) {
	store, _, _ := newSplitFixture(t)
	atOpen := &ProxiedDiagnostic{
		Endpoint: ProxiedEndpointStamp{Port: 1, PID: 2, Generation: "2:open"},
		Cursors:  proxyendpoint.Cursors{Main: SchemaCursorMain, Ignored: SchemaCursorIgnored},
	}

	live := LiveProxiedDiagnostic(store, atOpen)
	if live == nil || live.Demoted {
		t.Fatalf("live diagnostic = %+v, want a serving handle", live)
	}

	store.standDown(NewProxiedVerdictError(ProxiedVerdictDatabaseGone, "the database went away", nil))
	live = LiveProxiedDiagnostic(store, atOpen)
	if live == nil || !live.Demoted {
		t.Fatalf("live diagnostic = %+v, want the demotion", live)
	}
	if live.Verdict != ProxiedVerdictDatabaseGone {
		t.Errorf("verdict = %q, want database_gone", live.Verdict)
	}

	// A store that carries no split store gets the open's account back
	// untouched, which is what keeps every other lane's payload byte-identical.
	if got := LiveProxiedDiagnostic(NewMemStore(), atOpen); got != atOpen {
		t.Fatalf("LiveProxiedDiagnostic(MemStore) = %+v, want the open's account unchanged", got)
	}
	if got := LiveProxiedDiagnostic(NewMemStore(), nil); got != nil {
		t.Fatalf("LiveProxiedDiagnostic(MemStore, nil) = %+v, want nil so the field stays omitted", got)
	}

	// A refusal recorded at open survives a live handle that has no verdict of
	// its own to restate.
	fallback := &ProxiedDiagnostic{Verdict: ProxiedVerdictIdlePolicyFinite}
	serving, _, _ := newSplitFixture(t)
	if got := LiveProxiedDiagnostic(serving, fallback); got == nil || got.Verdict != ProxiedVerdictIdlePolicyFinite {
		t.Fatalf("live diagnostic = %+v, want the open's verdict preserved", got)
	}
}
