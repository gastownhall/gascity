package storebinding

// Provider-neutral class events.
//
// Every wrapped front door emits the same typed event for the same closed
// contract operation, whatever is underneath it. That is the whole point:
// "typed event parity for every class/provider combination" means an observer
// watching the SQLite binding and an observer watching an out-of-tree
// provider's binding see identical streams for identical calls, so a dashboard, an audit
// trail, or a health probe written against one provider is not silently wrong
// against another.
//
// Parity is structural rather than conventional. An event names its operation
// with a string, but the legal names for a class are not free text: they are
// the method set of that class's CLOSED CONTRACT, resolved from the interface
// type itself at init. A wrapper cannot emit an operation the contract does not
// have, and a provider cannot add one, because no provider names an operation
// at all — only the generic wrappers in this package do.
//
// Emission never changes an operation's result. An observer sees the outcome; it
// cannot alter it, delay it, or fail it. Observers therefore must not panic and
// must not block: this package deliberately does not recover, because a wrapper
// that swallowed an observer panic would hide a broken observer forever.

import (
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
)

// ErrInvalidClassEvent reports an event that does not name a real class
// contract operation.
var ErrInvalidClassEvent = errors.New("invalid storage class event")

// ClassEventKind is the closed set of things a wrapper can report.
type ClassEventKind uint8

const (
	// ClassEventRead reports a contract read.
	ClassEventRead ClassEventKind = iota + 1
	// ClassEventWrite reports a contract mutation.
	ClassEventWrite
	// ClassEventClaim reports a compare-and-swap acquisition.
	ClassEventClaim
	// ClassEventTransaction reports a contract transaction.
	ClassEventTransaction
	// ClassEventGraphApply reports a graph-plan application.
	ClassEventGraphApply
	// ClassEventProbe reports a liveness probe.
	ClassEventProbe
	// ClassEventCache reports a cache decision taken by the wrapper itself
	// rather than by the binding underneath it.
	ClassEventCache
	// ClassEventMaintenance reports a wrapper-owned maintenance action.
	ClassEventMaintenance
)

// Valid reports whether kind belongs to the closed kind set.
func (k ClassEventKind) Valid() bool { return k >= ClassEventRead && k <= ClassEventMaintenance }

// String returns the stable diagnostic kind name.
func (k ClassEventKind) String() string {
	switch k {
	case ClassEventRead:
		return "read"
	case ClassEventWrite:
		return "write"
	case ClassEventClaim:
		return "claim"
	case ClassEventTransaction:
		return "transaction"
	case ClassEventGraphApply:
		return "graph-apply"
	case ClassEventProbe:
		return "probe"
	case ClassEventCache:
		return "cache"
	case ClassEventMaintenance:
		return "maintenance"
	default:
		return "unknown"
	}
}

// ClassOutcome is the closed set of ways a wrapped operation can end.
type ClassOutcome uint8

const (
	// ClassOutcomeOK reports an operation that returned no error.
	ClassOutcomeOK ClassOutcome = iota + 1
	// ClassOutcomeConflict reports a compare-and-swap that lost. A conflict is
	// not a failure: the contract reports it as ok=false with a nil error, and
	// collapsing the two would make every dashboard read a healthy race as an
	// outage.
	ClassOutcomeConflict
	// ClassOutcomeFailed reports an operation that returned an error.
	ClassOutcomeFailed
	// ClassOutcomeHit reports a cache hit.
	ClassOutcomeHit
	// ClassOutcomeMiss reports a cache miss.
	ClassOutcomeMiss
	// ClassOutcomeInvalidated reports a cache invalidation.
	ClassOutcomeInvalidated
)

// Valid reports whether outcome belongs to the closed outcome set.
func (o ClassOutcome) Valid() bool { return o >= ClassOutcomeOK && o <= ClassOutcomeInvalidated }

// String returns the stable diagnostic outcome name.
func (o ClassOutcome) String() string {
	switch o {
	case ClassOutcomeOK:
		return "ok"
	case ClassOutcomeConflict:
		return "conflict"
	case ClassOutcomeFailed:
		return "failed"
	case ClassOutcomeHit:
		return "hit"
	case ClassOutcomeMiss:
		return "miss"
	case ClassOutcomeInvalidated:
		return "invalidated"
	default:
		return "unknown"
	}
}

// ClassEvent is one provider-neutral observation of a wrapped front-door
// operation. It carries no bead, no session, no payload and no provider
// detail: an event is a fact about the CONTRACT, and anything provider-shaped
// in it would break parity by definition.
type ClassEvent struct {
	Class     coordclass.Class
	Binding   BindingName
	Kind      ClassEventKind
	Operation string
	Outcome   ClassOutcome
}

// Validate rejects an event that does not name a real operation of its class's
// closed contract.
func (e ClassEvent) Validate() error {
	if !e.Kind.Valid() {
		return fmt.Errorf("%w: unknown kind", ErrInvalidClassEvent)
	}
	if !e.Outcome.Valid() {
		return fmt.Errorf("%w: unknown outcome", ErrInvalidClassEvent)
	}
	if err := validateIdentifier("event binding name", string(e.Binding)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidClassEvent, err)
	}
	if !ClassContractHasOperation(e.Class, e.Operation) {
		return fmt.Errorf("%w: %s has no contract operation %q", ErrInvalidClassEvent, e.Class, e.Operation)
	}
	return nil
}

// String returns the stable diagnostic form used by parity comparisons.
func (e ClassEvent) String() string {
	return fmt.Sprintf("%s/%s %s %s %s", e.Class, e.Binding, e.Kind, e.Operation, e.Outcome)
}

// ClassObserver receives every event a wrapped front door emits. Implementations
// must not panic, must not block, and must not mutate the storage they observe.
type ClassObserver interface {
	ObserveClassEvent(ClassEvent)
}

// ClassObserverFunc adapts a function to ClassObserver.
type ClassObserverFunc func(ClassEvent)

// ObserveClassEvent implements ClassObserver.
func (f ClassObserverFunc) ObserveClassEvent(event ClassEvent) { f(event) }

// classContractOperations is the legal operation name set per class, resolved
// from the closed contract interfaces themselves. Registering the interface
// type rather than a hand-written list is what keeps the census honest: adding
// a method to a contract adds its operation name here automatically, and no
// hand-maintained list can drift away from the contract it describes.
var classContractOperations = map[coordclass.Class]map[string]struct{}{
	coordclass.ClassGraph:    contractOperations(reflect.TypeOf((*GraphStore)(nil)).Elem()),
	coordclass.ClassSessions: contractOperations(reflect.TypeOf((*SessionsStore)(nil)).Elem()),
	coordclass.ClassOrders:   contractOperations(reflect.TypeOf((*OrdersStore)(nil)).Elem()),
	coordclass.ClassNudges: contractOperations(
		reflect.TypeOf((*NudgeQueue)(nil)).Elem(),
		reflect.TypeOf((*NudgeShadows)(nil)).Elem(),
	),
	coordclass.ClassMessaging: contractOperations(
		reflect.TypeOf((*mail.Provider)(nil)).Elem(),
		reflect.TypeOf((*extmsg.BindingService)(nil)).Elem(),
		reflect.TypeOf((*extmsg.DeliveryContextService)(nil)).Elem(),
		reflect.TypeOf((*extmsg.GroupService)(nil)).Elem(),
		reflect.TypeOf((*extmsg.TranscriptService)(nil)).Elem(),
	),
}

func contractOperations(contracts ...reflect.Type) map[string]struct{} {
	operations := make(map[string]struct{})
	for _, contract := range contracts {
		for index := 0; index < contract.NumMethod(); index++ {
			operations[contract.Method(index).Name] = struct{}{}
		}
	}
	return operations
}

// ClassContractHasOperation reports whether name is a method of the closed
// contract for class.
func ClassContractHasOperation(class coordclass.Class, name string) bool {
	operations, known := classContractOperations[class]
	if !known {
		return false
	}
	_, found := operations[name]
	return found
}

// ClassContractOperations returns the sorted operation names of one class's
// closed contract. Diagnostics and parity tests use it to enumerate what a
// wrapper could legally emit.
func ClassContractOperations(class coordclass.Class) []string {
	operations, known := classContractOperations[class]
	if !known {
		return nil
	}
	names := make([]string, 0, len(operations))
	for name := range operations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// classEmitter is the per-class emission seam shared by every wrapper. It holds
// the identity facts an event needs and nothing else.
type classEmitter struct {
	class    coordclass.Class
	binding  BindingName
	observer ClassObserver
}

func (e classEmitter) emit(kind ClassEventKind, operation string, outcome ClassOutcome) {
	if e.observer == nil {
		return
	}
	e.observer.ObserveClassEvent(ClassEvent{
		Class:     e.class,
		Binding:   e.binding,
		Kind:      kind,
		Operation: operation,
		Outcome:   outcome,
	})
}

// observe reports an operation whose only failure signal is an error.
func (e classEmitter) observe(kind ClassEventKind, operation string, err error) {
	e.emit(kind, operation, outcomeFor(err))
}

// observeClaim reports a compare-and-swap, which has three ends rather than
// two: acquired, lost to another holder, or failed outright.
func (e classEmitter) observeClaim(operation string, acquired bool, err error) {
	if err != nil {
		e.emit(ClassEventClaim, operation, ClassOutcomeFailed)
		return
	}
	if !acquired {
		e.emit(ClassEventClaim, operation, ClassOutcomeConflict)
		return
	}
	e.emit(ClassEventClaim, operation, ClassOutcomeOK)
}

func outcomeFor(err error) ClassOutcome {
	if err != nil {
		return ClassOutcomeFailed
	}
	return ClassOutcomeOK
}
