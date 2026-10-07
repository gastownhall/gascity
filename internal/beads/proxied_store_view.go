package beads

// The unwrap seam for the split store.
//
// A ProxiedStore is never what a caller holds. By the time it reaches gc's
// command code it has been through wrapStoreWithBeadPolicies, and often a
// CachingStore as well, and every one of those wrappers EMBEDS the beads.Store
// interface — which is exactly why they are safe (no capability can be forgotten
// by accident) and exactly why they are opaque (no capability outside the
// interface can be found either).
//
// Two callers need to see through them, and they need different halves of the
// same fact:
//
//   - cmd/gc's scopedStoreLike asks "is this store currently forking bd?", so a
//     command's deadline can be bound to the child. While the native leaf serves,
//     the answer is no and the reads cost nothing; after a stand-down the answer
//     is yes and the bd clone must be rebuilt ctx-bound, or a 3s timeout abandons
//     a live child instead of killing it.
//   - internal/doctor asks "what is this handle doing NOW?". The store-open
//     diagnostic is the account AT OPEN; a handle that stood down half an hour
//     ago would still report itself native, which is the one thing an operator
//     reading `gc doctor` about a degraded city must not be told.
//
// The seam is an INTERFACE rather than the concrete type on purpose: a caller
// that type-asserted *ProxiedStore would be unable to test its own branch (the
// struct cannot be built outside this package without an admitted Pin, which is
// the gate that makes the whole lane safe), and internal/doctor would have to
// import a concrete store type to ask a question about behavior.

// ProxiedStoreView is what a caller may ask a split store about itself.
//
// It is deliberately read-only and deliberately small: four questions, none of
// which can change the store's state. A caller that could reach in and promote a
// demoted handle would defeat the one-way demotion the whole lane rests on.
type ProxiedStoreView interface {
	// Demoted reports whether the native read leaf has stood down, so every
	// read as well as every write is now on the bd leaf.
	Demoted() bool
	// Verdict is why it stood down, nil while it is serving natively.
	Verdict() *ProxiedVerdictError
	// Report is the store's current account of itself — the generation it is
	// pinned to now (which a guard-tick re-pin may have moved since the open),
	// the evidence, the idle policy, the cursors, and whether it is demoted.
	Report() ProxiedOpenReport
	// BdLeaf is the store that does the forking.
	BdLeaf() Store
}

// The split store answers the view itself. The assertion is here rather than in
// proxied_store.go so a signature change to either side fails to compile at the
// seam, where the reason lives.
var _ ProxiedStoreView = (*ProxiedStore)(nil)

// ProxiedStoreCarrier is implemented by a wrapper that can hand back the split
// store underneath it.
//
// The method is EXPORTED, unlike this package's other carrier interfaces
// (localSidecarCarrier, conditionalWritesModeCarrier), because the wrapper that
// must implement it lives in cmd/gc: the bead-policy layer is applied outside
// the factory, so it is the outermost thing every caller holds. An unexported
// method would make the seam unreachable by the one wrapper that has to
// participate in it.
type ProxiedStoreCarrier interface {
	// ProxiedStore returns the split store this wrapper wraps, if any.
	ProxiedStore() (ProxiedStoreView, bool)
}

// proxiedStoreUnwrapDepth bounds the walk. Real stacks are two or three layers
// (policy over cache over store); the bound only guards an unexpected cycle.
const proxiedStoreUnwrapDepth = 8

// ProxiedStoreFrom finds the split store inside a wrapped one.
//
// It walks the two wrappers this package knows about (a ProxiedStore itself, and
// CachingStore's backing) and asks anything else whether it carries one. A store
// that is not proxied at all — every direct, hosted, file, exec and mem store in
// the tree — answers false after at most a couple of cheap type assertions.
func ProxiedStoreFrom(store Store) (ProxiedStoreView, bool) {
	for range proxiedStoreUnwrapDepth {
		if store == nil {
			return nil, false
		}
		switch inner := store.(type) {
		case ProxiedStoreView:
			return inner, true
		case *CachingStore:
			backing := inner.Backing()
			if backing == nil {
				return nil, false
			}
			store = backing
			continue
		}
		carrier, ok := store.(ProxiedStoreCarrier)
		if !ok {
			return nil, false
		}
		view, ok := carrier.ProxiedStore()
		if !ok {
			return nil, false
		}
		return view, true
	}
	return nil, false
}

// LiveProxiedDiagnostic projects what the handle is doing NOW, falling back to
// the account the open produced.
//
// The difference between the two is the whole reason this exists. The open-time
// account says which generation was admitted and why the lane was taken; the
// live one says whether the handle is still serving from it. A demotion that
// happened after the open — a schema migration under a controller store, a proxy
// that went away — is invisible in the first and obvious in the second.
//
// atOpen is returned unchanged for any store that carries no split store, so the
// flag-off lane and every other engine serialize byte-identically to what they
// serialize today (atOpen is nil there, and nil is omitempty).
func LiveProxiedDiagnostic(store Store, atOpen *ProxiedDiagnostic) *ProxiedDiagnostic {
	view, ok := ProxiedStoreFrom(store)
	if !ok {
		return atOpen
	}
	verdict, detail := ProxiedVerdictNone, ""
	if refusal := view.Verdict(); refusal != nil {
		verdict, detail = refusal.Verdict, refusal.Detail
	}
	live := view.Report().diagnostic(verdict, detail)
	if atOpen != nil && live.Verdict == ProxiedVerdictNone && atOpen.Verdict != ProxiedVerdictNone {
		// The open refused for a reason the live handle cannot restate (it is
		// serving, so it has no verdict of its own). Keep the open's account of
		// WHY rather than dropping it.
		live.Verdict, live.Detail = atOpen.Verdict, atOpen.Detail
	}
	return live
}
