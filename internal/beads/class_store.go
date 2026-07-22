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
// Optional capabilities are not promoted automatically: a type assertion on a
// typed store value asserts on the wrapper, not the underlying store. Selected
// capabilities with package resolver helpers are forwarded explicitly below;
// otherwise assert on the embedded .Store field (for example,
// `c, ok := s.Store.(beads.Counter)`) or pass that field to a generic helper.

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

// CloseTransitionerHandle forwards the atomic close capability hidden by the
// typed Store embedding.
func (s WorkStore) CloseTransitionerHandle() (CloseTransitioner, bool) {
	return CloseTransitionerFor(s.Store)
}

// CloseTransitionerHandle forwards the atomic close capability hidden by the
// typed Store embedding.
func (s GraphStore) CloseTransitionerHandle() (CloseTransitioner, bool) {
	return CloseTransitionerFor(s.Store)
}

// CloseTransitionerHandle forwards the atomic close capability hidden by the
// typed Store embedding.
func (s SessionStore) CloseTransitionerHandle() (CloseTransitioner, bool) {
	return CloseTransitionerFor(s.Store)
}

// CloseTransitionerHandle forwards the atomic close capability hidden by the
// typed Store embedding.
func (s MailStore) CloseTransitionerHandle() (CloseTransitioner, bool) {
	return CloseTransitionerFor(s.Store)
}

// CloseTransitionerHandle forwards the atomic close capability hidden by the
// typed Store embedding.
func (s OrdersStore) CloseTransitionerHandle() (CloseTransitioner, bool) {
	return CloseTransitionerFor(s.Store)
}

// CloseTransitionerHandle forwards the atomic close capability hidden by the
// typed Store embedding.
func (s NudgesStore) CloseTransitionerHandle() (CloseTransitioner, bool) {
	return CloseTransitionerFor(s.Store)
}

// CloseAllTransitionerHandle forwards backing-owned batch classification
// hidden by the typed Store embedding.
func (s WorkStore) CloseAllTransitionerHandle() (CloseAllTransitioner, bool) {
	return CloseAllTransitionerFor(s.Store)
}

// CloseAllTransitionerHandle forwards backing-owned batch classification
// hidden by the typed Store embedding.
func (s GraphStore) CloseAllTransitionerHandle() (CloseAllTransitioner, bool) {
	return CloseAllTransitionerFor(s.Store)
}

// CloseAllTransitionerHandle forwards backing-owned batch classification
// hidden by the typed Store embedding.
func (s SessionStore) CloseAllTransitionerHandle() (CloseAllTransitioner, bool) {
	return CloseAllTransitionerFor(s.Store)
}

// CloseAllTransitionerHandle forwards backing-owned batch classification
// hidden by the typed Store embedding.
func (s MailStore) CloseAllTransitionerHandle() (CloseAllTransitioner, bool) {
	return CloseAllTransitionerFor(s.Store)
}

// CloseAllTransitionerHandle forwards backing-owned batch classification
// hidden by the typed Store embedding.
func (s OrdersStore) CloseAllTransitionerHandle() (CloseAllTransitioner, bool) {
	return CloseAllTransitionerFor(s.Store)
}

// CloseAllTransitionerHandle forwards backing-owned batch classification
// hidden by the typed Store embedding.
func (s NudgesStore) CloseAllTransitionerHandle() (CloseAllTransitioner, bool) {
	return CloseAllTransitionerFor(s.Store)
}

// UpdateTransitionerHandle forwards atomic full-update classification hidden
// by the typed Store embedding.
func (s WorkStore) UpdateTransitionerHandle() (UpdateTransitioner, bool) {
	return UpdateTransitionerFor(s.Store)
}

// UpdateTransitionerHandle forwards atomic full-update classification hidden
// by the typed Store embedding.
func (s GraphStore) UpdateTransitionerHandle() (UpdateTransitioner, bool) {
	return UpdateTransitionerFor(s.Store)
}

// UpdateTransitionerHandle forwards atomic full-update classification hidden
// by the typed Store embedding.
func (s SessionStore) UpdateTransitionerHandle() (UpdateTransitioner, bool) {
	return UpdateTransitionerFor(s.Store)
}

// UpdateTransitionerHandle forwards atomic full-update classification hidden
// by the typed Store embedding.
func (s MailStore) UpdateTransitionerHandle() (UpdateTransitioner, bool) {
	return UpdateTransitionerFor(s.Store)
}

// UpdateTransitionerHandle forwards atomic full-update classification hidden
// by the typed Store embedding.
func (s OrdersStore) UpdateTransitionerHandle() (UpdateTransitioner, bool) {
	return UpdateTransitionerFor(s.Store)
}

// UpdateTransitionerHandle forwards atomic full-update classification hidden
// by the typed Store embedding.
func (s NudgesStore) UpdateTransitionerHandle() (UpdateTransitioner, bool) {
	return UpdateTransitionerFor(s.Store)
}

// CloseObserverSuppressorHandle forwards per-call observer suppression hidden
// by the typed Store embedding.
func (s WorkStore) CloseObserverSuppressorHandle() (CloseObserverSuppressor, bool) {
	return CloseObserverSuppressorFor(s.Store)
}

// CloseObserverSuppressorHandle forwards per-call observer suppression hidden
// by the typed Store embedding.
func (s GraphStore) CloseObserverSuppressorHandle() (CloseObserverSuppressor, bool) {
	return CloseObserverSuppressorFor(s.Store)
}

// CloseObserverSuppressorHandle forwards per-call observer suppression hidden
// by the typed Store embedding.
func (s SessionStore) CloseObserverSuppressorHandle() (CloseObserverSuppressor, bool) {
	return CloseObserverSuppressorFor(s.Store)
}

// CloseObserverSuppressorHandle forwards per-call observer suppression hidden
// by the typed Store embedding.
func (s MailStore) CloseObserverSuppressorHandle() (CloseObserverSuppressor, bool) {
	return CloseObserverSuppressorFor(s.Store)
}

// CloseObserverSuppressorHandle forwards per-call observer suppression hidden
// by the typed Store embedding.
func (s OrdersStore) CloseObserverSuppressorHandle() (CloseObserverSuppressor, bool) {
	return CloseObserverSuppressorFor(s.Store)
}

// CloseObserverSuppressorHandle forwards per-call observer suppression hidden
// by the typed Store embedding.
func (s NudgesStore) CloseObserverSuppressorHandle() (CloseObserverSuppressor, bool) {
	return CloseObserverSuppressorFor(s.Store)
}

// ObserverBarrierHandle forwards non-mutating observer sequencing hidden by
// the typed Store embedding.
func (s WorkStore) ObserverBarrierHandle() (ObserverBarrier, bool) {
	return ObserverBarrierFor(s.Store)
}

// ObserverBarrierHandle forwards non-mutating observer sequencing hidden by
// the typed Store embedding.
func (s GraphStore) ObserverBarrierHandle() (ObserverBarrier, bool) {
	return ObserverBarrierFor(s.Store)
}

// ObserverBarrierHandle forwards non-mutating observer sequencing hidden by
// the typed Store embedding.
func (s SessionStore) ObserverBarrierHandle() (ObserverBarrier, bool) {
	return ObserverBarrierFor(s.Store)
}

// ObserverBarrierHandle forwards non-mutating observer sequencing hidden by
// the typed Store embedding.
func (s MailStore) ObserverBarrierHandle() (ObserverBarrier, bool) {
	return ObserverBarrierFor(s.Store)
}

// ObserverBarrierHandle forwards non-mutating observer sequencing hidden by
// the typed Store embedding.
func (s OrdersStore) ObserverBarrierHandle() (ObserverBarrier, bool) {
	return ObserverBarrierFor(s.Store)
}

// ObserverBarrierHandle forwards non-mutating observer sequencing hidden by
// the typed Store embedding.
func (s NudgesStore) ObserverBarrierHandle() (ObserverBarrier, bool) {
	return ObserverBarrierFor(s.Store)
}

var (
	_ ConditionalWritesResolveTargeter      = WorkStore{}
	_ ConditionalWritesResolveTargeter      = GraphStore{}
	_ ConditionalWritesResolveTargeter      = SessionStore{}
	_ ConditionalWritesResolveTargeter      = MailStore{}
	_ ConditionalWritesResolveTargeter      = OrdersStore{}
	_ ConditionalWritesResolveTargeter      = NudgesStore{}
	_ CloseTransitionerHandleProvider       = WorkStore{}
	_ CloseTransitionerHandleProvider       = GraphStore{}
	_ CloseTransitionerHandleProvider       = SessionStore{}
	_ CloseTransitionerHandleProvider       = MailStore{}
	_ CloseTransitionerHandleProvider       = OrdersStore{}
	_ CloseTransitionerHandleProvider       = NudgesStore{}
	_ CloseAllTransitionerHandleProvider    = WorkStore{}
	_ CloseAllTransitionerHandleProvider    = GraphStore{}
	_ CloseAllTransitionerHandleProvider    = SessionStore{}
	_ CloseAllTransitionerHandleProvider    = MailStore{}
	_ CloseAllTransitionerHandleProvider    = OrdersStore{}
	_ CloseAllTransitionerHandleProvider    = NudgesStore{}
	_ UpdateTransitionerHandleProvider      = WorkStore{}
	_ UpdateTransitionerHandleProvider      = GraphStore{}
	_ UpdateTransitionerHandleProvider      = SessionStore{}
	_ UpdateTransitionerHandleProvider      = MailStore{}
	_ UpdateTransitionerHandleProvider      = OrdersStore{}
	_ UpdateTransitionerHandleProvider      = NudgesStore{}
	_ CloseObserverSuppressorHandleProvider = WorkStore{}
	_ CloseObserverSuppressorHandleProvider = GraphStore{}
	_ CloseObserverSuppressorHandleProvider = SessionStore{}
	_ CloseObserverSuppressorHandleProvider = MailStore{}
	_ CloseObserverSuppressorHandleProvider = OrdersStore{}
	_ CloseObserverSuppressorHandleProvider = NudgesStore{}
	_ ObserverBarrierHandleProvider         = WorkStore{}
	_ ObserverBarrierHandleProvider         = GraphStore{}
	_ ObserverBarrierHandleProvider         = SessionStore{}
	_ ObserverBarrierHandleProvider         = MailStore{}
	_ ObserverBarrierHandleProvider         = OrdersStore{}
	_ ObserverBarrierHandleProvider         = NudgesStore{}
)
