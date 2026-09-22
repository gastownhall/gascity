package beads

import (
	"errors"
	"fmt"
)

// ErrProxiedNativeReadOnly is the refusal of a native handle opened read-only
// over bd's proxy.
//
// PR2's split store serves reads natively and sends every mutation through the
// bd CLI, because bd owns the proxy, its Dolt child, the migration decisions
// and the event hooks. A native write on such a scope would bypass all four.
var ErrProxiedNativeReadOnly = errors.New("native Dolt store opened read-only over bd's proxy")

// proxiedNativeReadOnlyReason is what an operator sees appended to the refusal.
const proxiedNativeReadOnlyReason = "this scope's writes go through the bd CLI, which owns the proxy and its Dolt child"

// WithProxiedReadOnly latches a native handle read-only for its whole lifetime.
//
// The fence is a FIELD on *NativeDoltStore and a one-line guard on each
// mutating method, deliberately, rather than a hand-written read-only leaf type
// that forwards the reads.
//
// A leaf type is the obvious design and it is the wrong one here, because
// capability in this package is discovered by type assertion. *NativeDoltStore
// answers to a long tail of optional interfaces — conditional writes, graph
// apply, foreign-id creates, parent-projection waiting, the row witness — and
// CachingStore alone asserts three PACKAGE-PRIVATE ones that a type outside
// this file could not even name. Every interface a hand-written leaf forgot
// would go silently missing: no compile error, no test failure, just a
// capability that stops being found. splittest's strict store exists because
// that has already happened once.
//
// A field costs one branch per mutation and cannot lose an interface, because
// there is no new type. PR3's delta is to stop setting it.
//
// This is the SECOND of the two read-only fences, and the belt to the other's
// braces: the first is that the split wrapper does not implement the graph-apply
// interfaces at all, so policy middleware never routes an ephemeral create
// here. This one holds if a bare native leaf ever escapes the wrapper.
func WithProxiedReadOnly() NativeDoltStoreOption {
	return func(s *NativeDoltStore) { s.readOnlyReason = proxiedNativeReadOnlyReason }
}

// ReadOnlyReporter is a store that can say whether it refuses mutations.
//
// It exists so a WRAPPER can forward the answer instead of swallowing it. Every
// wrapper in the tree embeds the Store interface, which hides this method, and a
// hidden mutation fence is worse than an absent one: a caller that asks "is this
// handle read-only" gets "no" from a wrapper around a handle that refuses every
// write.
type ReadOnlyReporter interface {
	// ReadOnly reports whether the store refuses mutations.
	ReadOnly() bool
}

// ReadOnly reports whether this handle refuses mutations.
func (s *NativeDoltStore) ReadOnly() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readOnlyReason != ""
}

// readOnlyGuard is the one-line refusal every mutating method opens with.
//
// It returns nil for an unlatched handle, so a direct or hosted native store
// pays one atomic-free read of a string field per mutation and behaves exactly
// as it did before. It reads under the store's own RWMutex rather than
// unguarded because PR3 clears the latch on a live handle, and a field that is
// written once "before anyone could see it" today is the kind that grows a race
// the day somebody makes it dynamic.
func (s *NativeDoltStore) readOnlyGuard() error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	reason := s.readOnlyReason
	s.mu.RUnlock()
	if reason == "" {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrProxiedNativeReadOnly, reason)
}
