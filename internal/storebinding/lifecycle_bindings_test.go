package storebinding

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

// programBinding is one opened-binding fixture that counts closes and can be told
// to fail. Close counting is the whole point: "exactly once" is a claim about
// how many times a handle's Close ran, and no other observation proves it.
type programBinding struct {
	descriptor Descriptor
	closes     int
	failWith   error
}

func (b *programBinding) Descriptor() Descriptor { return b.descriptor.Clone() }

func (b *programBinding) Capabilities() ClassCapabilities { return b.descriptor.Capabilities }

func (b *programBinding) Work() (WorkTopology, bool) { return WorkTopology{}, false }

func (b *programBinding) Graph() (GraphStore, bool) { return nil, false }

func (b *programBinding) Sessions() (SessionsStore, bool) { return nil, false }

func (b *programBinding) Messaging() (MessagingFrontDoorBinder, bool) { return nil, false }

func (b *programBinding) Orders() (OrdersStore, bool) { return nil, false }

func (b *programBinding) Nudges() (NudgeFrontDoors, bool) { return NudgeFrontDoors{}, false }

func (b *programBinding) Close() error {
	b.closes++
	return b.failWith
}

func programCapabilities(classes ClassSet) ClassCapabilities {
	var capabilities ClassCapabilities
	available := ClassCapability{Available: true}
	for _, class := range classes.Classes() {
		switch class {
		case coordclass.ClassWork:
			capabilities.Work = available
		case coordclass.ClassGraph:
			capabilities.Graph = available
		case coordclass.ClassSessions:
			capabilities.Sessions = available
		case coordclass.ClassMessaging:
			capabilities.Messaging = available
		case coordclass.ClassOrders:
			capabilities.Orders = available
		case coordclass.ClassNudges:
			capabilities.Nudges = available
		}
	}
	return capabilities
}

func programDescriptor(t *testing.T, binding BindingName, classes ClassSet) Descriptor {
	t.Helper()
	descriptor, err := NewDescriptor(Descriptor{
		Version:                 1,
		SemanticContractVersion: "gascity.storage-class.v1",
		Provider:                "test-provider",
		ImplementationVersion:   "1.0.0",
		Components: []ComponentDescriptor{{
			ID:               ComponentID(string(binding) + "-main"),
			Locator:          ComponentLocator("/var/lib/gascity/" + string(binding)),
			PhysicalIdentity: PhysicalIdentity(string(binding) + "-physical"),
			Classes:          classes,
			Format:           "test-format",
			SchemaVersion:    "7",
			ABIVersion:       "test-abi-1",
			Marker:           MarkerState{Name: string(binding) + ".migrated", Present: true},
		}},
		Capabilities:    programCapabilities(classes),
		ConfigRefDigest: ConfigRefDigest(canonicalDigest([]byte(binding))),
	})
	if err != nil {
		t.Fatalf("building the %q descriptor: %v", binding, err)
	}
	return descriptor
}

func programOpen(t *testing.T, binding BindingName, classes ClassSet) *programBinding {
	t.Helper()
	return &programBinding{descriptor: programDescriptor(t, binding, classes)}
}

func programClasses(t *testing.T, classes ...coordclass.Class) ClassSet {
	t.Helper()
	set, err := NewClassSet(classes...)
	if err != nil {
		t.Fatalf("building a class set: %v", err)
	}
	return set
}

// TestBindingLifecycleClosesEachDistinctBindingExactlyOnce is the headline
// close-once assertion, in the shape that has bitten before: several class
// fronts projected out of ONE binding. Six classes, one handle, one close.
func TestBindingLifecycleClosesEachDistinctBindingExactlyOnce(t *testing.T) {
	all := programClasses(t, coordclass.Classes()...)
	implicit := programOpen(t, "work", all)
	lifecycle := NewBindingLifecycle()
	if err := lifecycle.Adopt("work", implicit, all); err != nil {
		t.Fatalf("adopting the implicit all-classes binding: %v", err)
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if implicit.closes != 1 {
		t.Fatalf("the implicit all-classes binding was closed %d times, want exactly 1", implicit.closes)
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if implicit.closes != 1 {
		t.Fatalf("a second Close closed the handle again (%d closes); shutdown is not idempotent", implicit.closes)
	}
	if !lifecycle.Sealed() {
		t.Fatal("a fully closed lifecycle does not report itself sealed")
	}
}

// TestBindingLifecycleClosesInReverseAdoptionOrder pins close ordering. A
// binding opened later may depend on one opened earlier, so the close program
// is the exact reverse of the open program.
func TestBindingLifecycleClosesInReverseAdoptionOrder(t *testing.T) {
	var order []BindingName
	lifecycle := NewBindingLifecycle()
	names := []BindingName{"first", "second", "third"}
	classes := []coordclass.Class{coordclass.ClassWork, coordclass.ClassGraph, coordclass.ClassSessions}
	handles := make([]*programBinding, len(names))
	for index, name := range names {
		set := programClasses(t, classes[index])
		handle := programOpen(t, name, set)
		handles[index] = handle
		recorded := name
		wrapped := &programRecordingBinding{programBinding: handle, record: func() { order = append(order, recorded) }}
		if err := lifecycle.Adopt(name, wrapped, set); err != nil {
			t.Fatalf("adopting %q: %v", name, err)
		}
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []BindingName{"third", "second", "first"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("close order = %v, want %v", order, want)
	}
	for index, handle := range handles {
		if handle.closes != 1 {
			t.Errorf("binding %q was closed %d times, want 1", names[index], handle.closes)
		}
	}
}

type programRecordingBinding struct {
	*programBinding
	record func()
}

func (b *programRecordingBinding) Close() error {
	b.record()
	return b.programBinding.Close()
}

// TestBindingLifecycleClosesEveryHandleWhenOneFails walks the failure position
// across the whole program. A close failure at position N must not strand the
// handles on either side of it, must not mark the failed handle closed, and
// must leave a retry that closes exactly the handle that failed.
func TestBindingLifecycleClosesEveryHandleWhenOneFails(t *testing.T) {
	names := []BindingName{"first", "second", "third"}
	classes := []coordclass.Class{coordclass.ClassWork, coordclass.ClassGraph, coordclass.ClassSessions}
	for failing := range names {
		t.Run(string(names[failing]), func(t *testing.T) {
			failure := fmt.Errorf("closing %q is broken", names[failing])
			lifecycle := NewBindingLifecycle()
			handles := make([]*programBinding, len(names))
			for index, name := range names {
				set := programClasses(t, classes[index])
				handle := programOpen(t, name, set)
				if index == failing {
					handle.failWith = failure
				}
				handles[index] = handle
				if err := lifecycle.Adopt(name, handle, set); err != nil {
					t.Fatalf("adopting %q: %v", name, err)
				}
			}

			err := lifecycle.Close()
			if !errors.Is(err, failure) {
				t.Fatalf("Close = %v, want the injected failure", err)
			}
			for index, handle := range handles {
				if handle.closes != 1 {
					t.Fatalf("binding %q was closed %d times on the first pass, want 1; a failure at position %d stranded a neighbor", names[index], handle.closes, failing)
				}
			}
			if lifecycle.Sealed() {
				t.Fatal("a lifecycle with an unclosed handle reports itself sealed")
			}

			// The retry must close exactly the handle that has not closed.
			handles[failing].failWith = nil
			if err := lifecycle.Close(); err != nil {
				t.Fatalf("retrying Close: %v", err)
			}
			for index, handle := range handles {
				want := 1
				if index == failing {
					want = 2
				}
				if handle.closes != want {
					t.Errorf("binding %q was closed %d times in total, want %d; a successful close was repeated or the failed one was not retried", names[index], handle.closes, want)
				}
			}
			if !lifecycle.Sealed() {
				t.Error("the lifecycle is not sealed after every handle finally closed")
			}
		})
	}
}

// TestBindingLifecycleUnwindsAPartialOpen is the partial-open path: the third
// binding never opens, and the two already adopted are released exactly once
// with the cause preserved.
func TestBindingLifecycleUnwindsAPartialOpen(t *testing.T) {
	lifecycle := NewBindingLifecycle()
	work := programOpen(t, "work", programClasses(t, coordclass.ClassWork))
	graph := programOpen(t, "graph", programClasses(t, coordclass.ClassGraph))
	if err := lifecycle.Adopt("work", work, programClasses(t, coordclass.ClassWork)); err != nil {
		t.Fatalf("adopting work: %v", err)
	}
	if err := lifecycle.Adopt("graph", graph, programClasses(t, coordclass.ClassGraph)); err != nil {
		t.Fatalf("adopting graph: %v", err)
	}

	cause := errors.New("opening the third binding failed")
	err := lifecycle.Unwind(cause)
	if !errors.Is(err, cause) {
		t.Fatalf("Unwind = %v, want the open failure preserved", err)
	}
	if work.closes != 1 || graph.closes != 1 {
		t.Fatalf("unwind closed work %d times and graph %d times, want 1 each", work.closes, graph.closes)
	}
	if err := lifecycle.Adopt("late", programOpen(t, "late", programClasses(t, coordclass.ClassOrders)), programClasses(t, coordclass.ClassOrders)); !errors.Is(err, ErrBindingLifecycleClosed) {
		t.Fatalf("adopting into an unwound lifecycle = %v, want ErrBindingLifecycleClosed", err)
	}
}

// TestBindingLifecycleUnwindJoinsACloseFailure proves a failed unwind reports
// BOTH the reason the open stopped and the handle that could not be released.
// Reporting only one of them is how a leak becomes invisible.
func TestBindingLifecycleUnwindJoinsACloseFailure(t *testing.T) {
	lifecycle := NewBindingLifecycle()
	closeFailure := errors.New("handle is wedged")
	handle := programOpen(t, "work", programClasses(t, coordclass.ClassWork))
	handle.failWith = closeFailure
	if err := lifecycle.Adopt("work", handle, programClasses(t, coordclass.ClassWork)); err != nil {
		t.Fatalf("adopting work: %v", err)
	}
	cause := errors.New("opening the next binding failed")
	err := lifecycle.Unwind(cause)
	if !errors.Is(err, cause) {
		t.Errorf("Unwind lost the open failure: %v", err)
	}
	if !errors.Is(err, closeFailure) {
		t.Errorf("Unwind lost the close failure: %v", err)
	}
}

// TestBindingLifecycleRejectsCorruptAdoptions pins every shape that would make
// the close program wrong before it ever runs.
func TestBindingLifecycleRejectsCorruptAdoptions(t *testing.T) {
	work := programClasses(t, coordclass.ClassWork)
	graph := programClasses(t, coordclass.ClassGraph)

	lifecycle := NewBindingLifecycle()
	if err := lifecycle.Adopt("", programOpen(t, "work", work), work); !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Errorf("adopting an unnamed binding = %v, want ErrInvalidBindingAdoption", err)
	}
	if err := lifecycle.Adopt("work", nil, work); !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Errorf("adopting a nil handle = %v, want ErrInvalidBindingAdoption", err)
	}
	if err := lifecycle.Adopt("work", programOpen(t, "work", work), ClassSet{}); !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Errorf("adopting a binding that serves no class = %v, want ErrInvalidBindingAdoption", err)
	}

	first := programOpen(t, "work", work)
	if err := lifecycle.Adopt("work", first, work); err != nil {
		t.Fatalf("adopting work: %v", err)
	}
	if err := lifecycle.Adopt("work", programOpen(t, "work", graph), graph); !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Errorf("adopting the same binding name twice = %v, want ErrInvalidBindingAdoption", err)
	}
	if err := lifecycle.Adopt("other", programOpen(t, "other", work), work); !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Errorf("adopting a second owner for one class = %v, want ErrInvalidBindingAdoption", err)
	}

	// A rejected adoption must not have taken ownership of anything.
	if err := lifecycle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if first.closes != 1 {
		t.Fatalf("the only owned binding was closed %d times, want 1", first.closes)
	}
}

// TestBindingLifecycleMapsEveryClassToItsBinding pins the ownership map the
// diagnostics and the wrapper stack both read.
func TestBindingLifecycleMapsEveryClassToItsBinding(t *testing.T) {
	lifecycle := NewBindingLifecycle()
	shared := programClasses(t, coordclass.ClassGraph, coordclass.ClassOrders)
	work := programClasses(t, coordclass.ClassWork)
	if err := lifecycle.Adopt("work", programOpen(t, "work", work), work); err != nil {
		t.Fatalf("adopting work: %v", err)
	}
	if err := lifecycle.Adopt("shared", programOpen(t, "shared", shared), shared); err != nil {
		t.Fatalf("adopting shared: %v", err)
	}
	for class, want := range map[coordclass.Class]BindingName{
		coordclass.ClassWork:   "work",
		coordclass.ClassGraph:  "shared",
		coordclass.ClassOrders: "shared",
	} {
		got, assigned := lifecycle.BindingFor(class)
		if !assigned || got != want {
			t.Errorf("BindingFor(%s) = (%q, %v), want (%q, true)", class, got, assigned, want)
		}
	}
	if _, assigned := lifecycle.BindingFor(coordclass.ClassNudges); assigned {
		t.Error("BindingFor reports an assignment for a class nobody adopted")
	}
	if got := len(lifecycle.Bindings()); got != 2 {
		t.Fatalf("the lifecycle owns %d distinct bindings, want 2", got)
	}
	if got := len(lifecycle.Assignments()); got != 3 {
		t.Fatalf("the lifecycle reports %d class assignments, want 3", got)
	}
}
