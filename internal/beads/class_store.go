package beads

// This file declares the strongly-typed per-class store wrappers that form the
// compile-time seam over the otherwise class-agnostic Store interface.
//
// Each type embeds the Store interface (field name Store), so it promotes every
// Store method and therefore IS a Store for all Store operations. The point is
// purely static: a function that handles a statically-known coordination class
// takes/returns its typed store, and the compiler then refuses to let a caller
// hand it a store belonging to a different class. At runtime each typed value
// wraps the SAME underlying store value the call site already used — no new
// backend, no extra caching or policy layer — so behavior is byte-identical.
//
// Optional capabilities (e.g. Counter, GraphApplyStore, GraphApplyFor,
// StorageCreateStore, Backing/ReadyLive) are NOT promoted through the embedding:
// a type assertion on a typed store value asserts on the wrapper, not the
// underlying store, and will fail. Access optional capabilities by asserting on
// the embedded .Store field instead (e.g. `c, ok := s.Store.(beads.Counter)`).
// Likewise pass the unwrapped .Store field when calling a generic Store helper
// that is shared across multiple classes.

// WorkStore is a strongly-typed view over a single Store holding work beads
// (the city's general task ledger). It is backed by the same underlying store
// it wraps; the wrapper exists so the compiler enforces that a work-class
// consumer cannot be handed another class's store. Access optional capabilities
// by asserting on the embedded .Store field.
type WorkStore struct {
	Store
}

// GraphStore is a strongly-typed view over a single Store holding graph beads
// (controller graph / molecule state). It is backed by the same underlying
// store it wraps; the wrapper exists so the compiler enforces that a graph-class
// consumer cannot be handed another class's store. Access optional capabilities
// by asserting on the embedded .Store field.
type GraphStore struct {
	Store
}

// SessionStore is a strongly-typed view over a single Store holding session
// beads (session lifecycle projection). It is backed by the same underlying
// store it wraps; the wrapper exists so the compiler enforces that a
// session-class consumer cannot be handed another class's store. Access optional
// capabilities by asserting on the embedded .Store field.
type SessionStore struct {
	Store
}

// MailStore is a strongly-typed view over a single Store holding mail beads
// (inter-agent messages). It is backed by the same underlying store it wraps;
// the wrapper exists so the compiler enforces that a mail-class consumer cannot
// be handed another class's store. Access optional capabilities by asserting on
// the embedded .Store field.
type MailStore struct {
	Store
}

// OrdersStore is a strongly-typed view over a single Store holding order beads
// (scheduled/event-gated formula triggers). It is backed by the same underlying
// store it wraps; the wrapper exists so the compiler enforces that an
// orders-class consumer cannot be handed another class's store. Access optional
// capabilities by asserting on the embedded .Store field.
type OrdersStore struct {
	Store
}

// NudgesStore is a strongly-typed view over a single Store holding nudge beads
// (session nudges). It is backed by the same underlying store it wraps; the
// wrapper exists so the compiler enforces that a nudges-class consumer cannot be
// handed another class's store. Access optional capabilities by asserting on the
// embedded .Store field.
type NudgesStore struct {
	Store
}

// The typed class wrappers declare their embedded store as the
// conditional-writes resolution target, so ResolveConditionalWriter works on
// a typed handle without the caller remembering to unwrap — the one optional
// capability where forgetting the unwrap would not fail loudly but silently
// resolve unset→legacy (fatal under require). All other optional capabilities
// keep the assert-on-.Store convention above.

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s WorkStore) ConditionalWritesResolveTarget() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s GraphStore) ConditionalWritesResolveTarget() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s SessionStore) ConditionalWritesResolveTarget() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s MailStore) ConditionalWritesResolveTarget() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s OrdersStore) ConditionalWritesResolveTarget() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s NudgesStore) ConditionalWritesResolveTarget() Store { return s.Store }

var (
	_ ConditionalWritesResolveTargeter = WorkStore{}
	_ ConditionalWritesResolveTargeter = GraphStore{}
	_ ConditionalWritesResolveTargeter = SessionStore{}
	_ ConditionalWritesResolveTargeter = MailStore{}
	_ ConditionalWritesResolveTargeter = OrdersStore{}
	_ ConditionalWritesResolveTargeter = NudgesStore{}
)
