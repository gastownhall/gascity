package storebinding

// Provider-neutral binding lifecycle ownership.
//
// Several storage classes routinely share one opened binding: the implicit
// all-classes Work binding serves every class from one handle, and an explicit
// binding may serve any subset. A class handle is therefore NOT a unit of
// ownership — the binding is. BindingLifecycle is the one place that
// distinction lives, so shutdown closes each distinct binding exactly once no
// matter how many class fronts were projected out of it.
//
// It owns nothing else. It opens no binding, resolves no provider, and reads
// no configuration: a caller adopts an already-opened handle and the lifecycle
// takes over its close obligation from that moment. That is what makes the
// close program executable from a partially-completed open — the positions
// where a leak or a double close has historically happened.

import (
	"errors"
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/coordclass"
)

var (
	// ErrInvalidBindingAdoption reports an adoption that would leave a binding
	// unowned, doubly owned, or owned after shutdown.
	ErrInvalidBindingAdoption = errors.New("invalid storage binding adoption")
	// ErrBindingLifecycleClosed reports use of a lifecycle whose bindings have
	// all been closed.
	ErrBindingLifecycleClosed = errors.New("storage binding lifecycle is closed")
)

// AdoptedBinding is one distinct binding whose close obligation the lifecycle
// holds: its name, the classes projected out of it, and the descriptor it was
// opened against.
//
// It deliberately does NOT carry the opened handle. Handing the handle back
// would hand back its Close, and a consumer that closed it directly would
// defeat the one guarantee this type exists to make. Everything a diagnostic
// needs is in the descriptor, which is snapshotted at adoption.
type AdoptedBinding struct {
	Binding    BindingName
	Classes    ClassSet
	Descriptor Descriptor
	// Closed reports whether this binding's handle has already been closed
	// successfully. A handle whose Close returned an error is NOT closed and
	// stays owned, so a retry can finish the job.
	Closed bool
}

type ownedBinding struct {
	binding    BindingName
	classes    ClassSet
	descriptor Descriptor
	opened     OpenedBinding
	closed     bool
}

// BindingLifecycle owns every distinct opened binding of one resolved plan and
// executes the close program in exact reverse adoption order.
type BindingLifecycle struct {
	mu          sync.Mutex
	order       []*ownedBinding
	byName      map[BindingName]*ownedBinding
	assignments map[coordclass.Class]BindingName
	sealed      bool
}

// NewBindingLifecycle returns an empty lifecycle that owns no binding yet.
func NewBindingLifecycle() *BindingLifecycle {
	return &BindingLifecycle{
		byName:      make(map[BindingName]*ownedBinding),
		assignments: make(map[coordclass.Class]BindingName),
	}
}

// Adopt takes over the close obligation for one opened binding and records the
// classes it serves. It is the only way a handle enters the lifecycle, and it
// rejects every shape that would corrupt the close program: a nil handle, a
// second adoption of the same binding name, a class already served by another
// binding, an empty class set, or an adoption after shutdown.
//
// Adoption order is close order, reversed. A caller opening a program of
// bindings adopts each handle as soon as it exists, so a failure at open N
// leaves bindings 1..N-1 owned and closable by Unwind.
func (l *BindingLifecycle) Adopt(binding BindingName, opened OpenedBinding, classes ClassSet) error {
	if err := validateIdentifier("binding name", string(binding)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBindingAdoption, err)
	}
	if isNilInterface(opened) {
		return fmt.Errorf("%w: binding %q supplied no opened handle", ErrInvalidBindingAdoption, binding)
	}
	if classes.Empty() {
		return fmt.Errorf("%w: binding %q serves no class", ErrInvalidBindingAdoption, binding)
	}
	// The descriptor is snapshotted and validated here, once, so no later
	// diagnostic has to touch the handle and no invalid descriptor can reach a
	// status view or a support bundle.
	descriptor := opened.Descriptor()
	if err := descriptor.Validate(); err != nil {
		return fmt.Errorf("%w: binding %q: %w", ErrInvalidBindingAdoption, binding, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sealed {
		return fmt.Errorf("%w: binding %q", ErrBindingLifecycleClosed, binding)
	}
	if _, exists := l.byName[binding]; exists {
		return fmt.Errorf("%w: binding %q is already owned", ErrInvalidBindingAdoption, binding)
	}
	for _, class := range classes.Classes() {
		if owner, assigned := l.assignments[class]; assigned {
			return fmt.Errorf("%w: class %s is already served by binding %q", ErrInvalidBindingAdoption, class, owner)
		}
	}
	owned := &ownedBinding{binding: binding, classes: classes, descriptor: descriptor, opened: opened}
	l.order = append(l.order, owned)
	l.byName[binding] = owned
	for _, class := range classes.Classes() {
		l.assignments[class] = binding
	}
	return nil
}

// AdoptOrClose adopts one opened handle and, if the adoption is refused,
// closes the handle it refused before returning.
//
// This exists because a refused adoption is the one partial-open position that
// leaks. [Adopt] deliberately does not touch a handle it did not take (a
// lifecycle that closed a handle it refused would be closing something it never
// owned), so a caller that only calls Adopt in its open loop must remember to
// release the refused handle itself — and the refusal cases are duplicate
// names, class conflicts and invalid descriptors, i.e. exactly the situations
// where a caller is already in unfamiliar territory. There is no recovery from
// a refusal that keeps the handle useful, so the release is not a policy
// choice, and putting it here means the open loop is one call per binding:
//
//	if err := lifecycle.AdoptOrClose(name, opened, classes); err != nil {
//		return lifecycle.Unwind(err)
//	}
//
// The refusal and any close failure are joined, because a leaked handle and the
// reason it was refused are two different facts an operator needs.
func (l *BindingLifecycle) AdoptOrClose(binding BindingName, opened OpenedBinding, classes ClassSet) error {
	adoptErr := l.Adopt(binding, opened, classes)
	if adoptErr == nil {
		return nil
	}
	if isNilInterface(opened) {
		return adoptErr
	}
	if closeErr := opened.Close(); closeErr != nil {
		return errors.Join(adoptErr, fmt.Errorf("closing the refused storage binding %q: %w", binding, closeErr))
	}
	return adoptErr
}

// BindingFor returns the binding serving one class.
func (l *BindingLifecycle) BindingFor(class coordclass.Class) (BindingName, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	binding, assigned := l.assignments[class]
	return binding, assigned
}

// Assignments returns the class-to-binding map in canonical class order.
func (l *BindingLifecycle) Assignments() []ClassAssignment {
	l.mu.Lock()
	defer l.mu.Unlock()
	assignments := make([]ClassAssignment, 0, len(l.assignments))
	for _, class := range coordclass.Classes() {
		if binding, assigned := l.assignments[class]; assigned {
			assignments = append(assignments, ClassAssignment{Class: class, Binding: binding})
		}
	}
	return assignments
}

// Bindings returns one entry per distinct owned binding, in adoption order.
func (l *BindingLifecycle) Bindings() []AdoptedBinding {
	l.mu.Lock()
	defer l.mu.Unlock()
	adopted := make([]AdoptedBinding, len(l.order))
	for index, owned := range l.order {
		adopted[index] = AdoptedBinding{
			Binding:    owned.binding,
			Classes:    owned.classes,
			Descriptor: owned.descriptor.Clone(),
			Closed:     owned.closed,
		}
	}
	return adopted
}

// Close executes the close program: every distinct owned binding, in exact
// reverse adoption order, closed exactly once.
//
// A handle whose Close returns an error is deliberately NOT marked closed. The
// provider contract says a failed close leaves ownership with the caller, so
// the handle stays owned and a later Close retries exactly the handles that
// have not yet succeeded. A handle that has already closed successfully is
// never closed a second time, which is the half of "exactly once" that a naive
// retry loop breaks.
//
// Close is idempotent: once every handle has closed successfully the lifecycle
// is sealed, further Close calls are no-ops, and Adopt is refused.
func (l *BindingLifecycle) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeLocked()
}

// Unwind closes every owned binding after a failed open and joins the cause
// with any close failure. It is the partial-open path: the caller reports why
// it stopped, and the lifecycle guarantees that what it had already adopted is
// released exactly once.
func (l *BindingLifecycle) Unwind(cause error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	closeErr := l.closeLocked()
	if cause == nil {
		return closeErr
	}
	if closeErr == nil {
		return cause
	}
	return errors.Join(cause, closeErr)
}

func (l *BindingLifecycle) closeLocked() error {
	if l.sealed {
		return nil
	}
	errs := make([]error, 0, len(l.order))
	for index := len(l.order) - 1; index >= 0; index-- {
		owned := l.order[index]
		if owned.closed {
			continue
		}
		if err := owned.opened.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing storage binding %q: %w", owned.binding, err))
			continue
		}
		owned.closed = true
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	l.sealed = true
	return nil
}

// Sealed reports whether every owned binding has closed successfully.
func (l *BindingLifecycle) Sealed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sealed
}
