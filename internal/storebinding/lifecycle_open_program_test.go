package storebinding

// The close program, executed from every position an open program can stop at.
//
// "Shutdown closes each distinct binding exactly once" is easy to satisfy on
// the happy path and easy to break everywhere else, and the everywhere-else is
// what has bitten: a program of bindings can stop because an open failed,
// because an adoption was refused after the handle already existed, or because
// a close failed while unwinding another failure. Each of those leaves a
// different prefix of the program owned, and each has its own way of leaking a
// handle or closing one twice.
//
// So this file does not test one failure. It enumerates the cross product of
// (failure kind x position) over a fixed program and asserts the same two
// invariants every time: every handle that was successfully OPENED is closed
// exactly once, and every reason the program stopped survives in the error.
// Handles that were never opened are never closed, which is the other half of
// exactly-once and the half a leak-counting test misses.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

// openProgramStep is one binding of an open program: its name and the classes
// projected out of it. The program below is deliberately not one-class-per-
// binding — a shared binding serving several classes is the shape that makes
// "per distinct binding" different from "per class".
type openProgramStep struct {
	binding BindingName
	classes []coordclass.Class
}

func openProgramSteps() []openProgramStep {
	return []openProgramStep{
		{binding: "work", classes: []coordclass.Class{coordclass.ClassWork}},
		{binding: "infra", classes: []coordclass.Class{coordclass.ClassGraph, coordclass.ClassOrders}},
		{binding: "sessions", classes: []coordclass.Class{coordclass.ClassSessions}},
		{binding: "fabric", classes: []coordclass.Class{coordclass.ClassMessaging, coordclass.ClassNudges}},
	}
}

// openFailureKind is how an open program can stop at a position.
type openFailureKind int

const (
	// openStepFails: the provider never returned a handle, so there is nothing
	// at this position to close.
	openStepFails openFailureKind = iota
	// adoptRefused: the handle exists but the lifecycle refuses it. This is
	// the leak position — the handle is open and unowned.
	adoptRefused
	// closeStepFails: the open program completes, and one handle fails to close
	// during the shutdown that follows.
	closeStepFails
)

func (k openFailureKind) String() string {
	switch k {
	case openStepFails:
		return "open-fails"
	case adoptRefused:
		return "adopt-refused"
	case closeStepFails:
		return "close-fails"
	default:
		return fmt.Sprintf("openFailureKind(%d)", int(k))
	}
}

// openProgramRun is what one execution observed.
type openProgramRun struct {
	lifecycle *BindingLifecycle
	// opened holds one entry per program position, nil where the open failed.
	opened []*programBinding
	err    error
	// injected is the failure the executor forced, for the kinds that force
	// one of their own. An adopt refusal is produced by the lifecycle itself,
	// so that kind is checked against ErrInvalidBindingAdoption instead.
	injected error
}

// runOpenProgram executes the open program with one injected failure at one
// position, using the caller pattern the lifecycle documents: adopt through
// AdoptOrClose, and unwind on the first error.
//
// The executor is the subject as much as the lifecycle is. If this loop is
// wrong, real boot code written the same way is wrong, so it is written the
// way boot code would be — no test-only bookkeeping compensates for it.
func runOpenProgram(t *testing.T, kind openFailureKind, position int) openProgramRun {
	t.Helper()
	steps := openProgramSteps()
	injected := fmt.Errorf("injected %s at position %d", kind, position)
	run := openProgramRun{lifecycle: NewBindingLifecycle(), opened: make([]*programBinding, len(steps)), injected: injected}

	for index, step := range steps {
		if kind == openStepFails && index == position {
			run.err = run.lifecycle.Unwind(injected)
			return run
		}
		classes := programClasses(t, step.classes...)
		handle := programOpen(t, step.binding, classes)
		if kind == adoptRefused && index == position {
			// An unadoptable handle: the descriptor it reports cannot pass
			// validation, which is how a real provider mismatch shows up.
			handle.descriptor.ImplementationVersion = ""
		}
		if kind == closeStepFails && index == position {
			handle.failWith = injected
		}
		run.opened[index] = handle
		if err := run.lifecycle.AdoptOrClose(step.binding, handle, classes); err != nil {
			run.err = run.lifecycle.Unwind(err)
			return run
		}
	}
	run.err = run.lifecycle.Close()
	return run
}

// handleFor returns the handle the program opened for one binding name.
func (r openProgramRun) handleFor(t *testing.T, binding BindingName) *programBinding {
	t.Helper()
	for index, step := range openProgramSteps() {
		if step.binding == binding {
			if r.opened[index] == nil {
				t.Fatalf("status reports binding %q, which this run never opened", binding)
			}
			return r.opened[index]
		}
	}
	t.Fatalf("status reports binding %q, which is not in the program", binding)
	return nil
}

// TestCloseProgramRunsFromEveryPartialOpenPosition is AC4's real evidence.
func TestCloseProgramRunsFromEveryPartialOpenPosition(t *testing.T) {
	steps := openProgramSteps()
	for _, kind := range []openFailureKind{openStepFails, adoptRefused, closeStepFails} {
		for position := range steps {
			t.Run(fmt.Sprintf("%s/%s", kind, steps[position].binding), func(t *testing.T) {
				run := runOpenProgram(t, kind, position)

				if run.err == nil {
					t.Fatalf("the program reported success despite an injected %s at position %d", kind, position)
				}
				// Whatever stopped the program has to survive the unwind. An
				// unwind that reports only "closed cleanly" is how the reason
				// a boot failed gets lost.
				if kind == adoptRefused {
					if !errors.Is(run.err, ErrInvalidBindingAdoption) {
						t.Errorf("the program error %q is not the adoption refusal", run.err)
					}
					if !strings.Contains(run.err.Error(), string(steps[position].binding)) {
						t.Errorf("the program error %q does not name the refused binding %q", run.err, steps[position].binding)
					}
				} else if !errors.Is(run.err, run.injected) {
					t.Errorf("the program error %q does not carry the injected failure", run.err)
				}

				for index, handle := range run.opened {
					if handle == nil {
						// Nothing was opened at this position. There is
						// therefore nothing to close, and a close program that
						// invented one would be closing a handle it never had.
						continue
					}
					if handle.closes != 1 {
						t.Errorf("binding %q was closed %d times, want exactly 1 (injected %s at position %d)",
							steps[index].binding, handle.closes, kind, position)
					}
				}

				// A handle whose Close failed is still owned, so the lifecycle
				// is not sealed; everything else finished and it is.
				wantSealed := kind != closeStepFails
				if run.lifecycle.Sealed() != wantSealed {
					t.Errorf("lifecycle sealed = %v, want %v after an injected %s at position %d",
						run.lifecycle.Sealed(), wantSealed, kind, position)
				}

				// Status has to agree with the handles the process actually
				// holds: a binding that closed reports itself closed and its
				// classes unavailable, and a binding whose close FAILED is
				// still open and still available. A status view that disagrees
				// with reality is worse than no status view.
				if len(run.lifecycle.Bindings()) == 0 {
					return
				}
				report, err := run.lifecycle.Health()
				if err != nil {
					t.Fatalf("building the status view after the program stopped: %v", err)
				}
				for _, entry := range report.Bindings {
					handle := run.handleFor(t, entry.Binding)
					wantClosed := handle.failWith == nil
					if entry.Closed != wantClosed {
						t.Errorf("binding %q reports Closed=%v, want %v", entry.Binding, entry.Closed, wantClosed)
					}
					for _, class := range entry.Classes {
						if entry.Available(class) == wantClosed {
							t.Errorf("binding %q reports class %s available=%v while Closed=%v",
								entry.Binding, class, entry.Available(class), entry.Closed)
						}
					}
				}
			})
		}
	}
}

// TestCloseProgramRetriesOnlyTheHandleThatFailed pins the retry half of
// exactly-once from a partial position: after a close failure the program can
// be closed again, and the handles that already closed are not closed twice.
func TestCloseProgramRetriesOnlyTheHandleThatFailed(t *testing.T) {
	steps := openProgramSteps()
	for position := range steps {
		t.Run(string(steps[position].binding), func(t *testing.T) {
			run := runOpenProgram(t, closeStepFails, position)
			if run.err == nil {
				t.Fatalf("closing the program with a wedged handle at position %d reported success", position)
			}
			run.opened[position].failWith = nil
			if err := run.lifecycle.Close(); err != nil {
				t.Fatalf("retrying the close program: %v", err)
			}
			for index, handle := range run.opened {
				want := 1
				if index == position {
					want = 2
				}
				if handle.closes != want {
					t.Errorf("binding %q was closed %d times in total, want %d", steps[index].binding, handle.closes, want)
				}
			}
			if !run.lifecycle.Sealed() {
				t.Error("the lifecycle is not sealed after the retry closed the last handle")
			}
		})
	}
}

// TestAdoptLeavesARefusedHandleToTheCaller pins the ownership rule
// AdoptOrClose is built on: Adopt does not close what it refuses. If it ever
// starts to, a caller that (correctly, today) releases the refused handle
// itself would be double-closing it.
func TestAdoptLeavesARefusedHandleToTheCaller(t *testing.T) {
	lifecycle := NewBindingLifecycle()
	work := programClasses(t, coordclass.ClassWork)
	first := programOpen(t, "work", work)
	if err := lifecycle.Adopt("work", first, work); err != nil {
		t.Fatalf("adopting work: %v", err)
	}

	refused := programOpen(t, "work", work)
	if err := lifecycle.Adopt("work", refused, work); !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Fatalf("re-adopting the same binding name = %v, want ErrInvalidBindingAdoption", err)
	}
	if refused.closes != 0 {
		t.Fatalf("Adopt closed the handle it refused %d times; the lifecycle closed something it never owned", refused.closes)
	}

	// AdoptOrClose is the same refusal with the release attached.
	released := programOpen(t, "work", work)
	if err := lifecycle.AdoptOrClose("work", released, work); !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Fatalf("AdoptOrClose on a refused adoption = %v, want ErrInvalidBindingAdoption", err)
	}
	if released.closes != 1 {
		t.Fatalf("AdoptOrClose closed the refused handle %d times, want exactly 1", released.closes)
	}

	// A refused handle that cannot be released reports BOTH facts: an operator
	// needs the reason it was refused and the handle that is now leaked.
	wedged := programOpen(t, "work", work)
	wedgeFailure := errors.New("refused handle is wedged")
	wedged.failWith = wedgeFailure
	err := lifecycle.AdoptOrClose("work", wedged, work)
	if !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Errorf("AdoptOrClose lost the refusal: %v", err)
	}
	if !errors.Is(err, wedgeFailure) {
		t.Errorf("AdoptOrClose lost the close failure: %v", err)
	}

	if err := lifecycle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if first.closes != 1 {
		t.Fatalf("the only owned binding was closed %d times, want 1", first.closes)
	}
}

// TestAdoptOrCloseAcceptsANilHandle proves the one refusal with nothing to
// release does not panic reaching for a Close that is not there. A typed nil
// is a non-nil interface, so this is a live hazard rather than a formality.
func TestAdoptOrCloseAcceptsANilHandle(t *testing.T) {
	lifecycle := NewBindingLifecycle()
	work := programClasses(t, coordclass.ClassWork)
	if err := lifecycle.AdoptOrClose("work", nil, work); !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Errorf("AdoptOrClose with no handle = %v, want ErrInvalidBindingAdoption", err)
	}
	var typedNil *programBinding
	if err := lifecycle.AdoptOrClose("work", typedNil, work); !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Errorf("AdoptOrClose with a typed-nil handle = %v, want ErrInvalidBindingAdoption", err)
	}
}
