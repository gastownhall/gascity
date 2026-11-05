package storebinding

// Provider-neutral binding health and status.
//
// Status has to answer one question without opening anything: which class is
// served by which binding, on which provider, in which physical format, and is
// it available. Every fact below comes from the descriptor the binding was
// opened against and from the lifecycle's own ownership map — never from a
// registry lookup, a provider re-resolution, or a fresh open. A diagnostic that
// re-resolves is a diagnostic that can report a binding the process is not
// actually using.
//
// The report is secret-free by construction, and by construction ONLY: a
// descriptor is validated once, when its handle is adopted, and a
// credential-bearing one never becomes owned. Status therefore repeats no
// secret check of its own — it prints the descriptor facts of handles that
// could not have been adopted carrying a secret. The one fact it deliberately
// omits is the component locator, which is the configured path.

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/coordclass"
)

// ErrInvalidHealthReport reports a status view that could not be built from the
// descriptors the process actually opened.
var ErrInvalidHealthReport = errors.New("invalid storage health report")

// ClassAssignment names the binding serving one storage class.
type ClassAssignment struct {
	Class   coordclass.Class
	Binding BindingName
}

// ComponentHealth is the secret-free physical identity of one component of one
// binding: its opaque provider-owned format, its schema or ABI version, and the
// provider-observed database identity. It deliberately carries no locator —
// that is the configured path, and status is read by humans and shipped in
// support bundles.
type ComponentHealth struct {
	Component        ComponentID
	Classes          []coordclass.Class
	Format           FormatID
	SchemaVersion    string
	ABIVersion       string
	PhysicalIdentity PhysicalIdentity
	MarkerName       string
	MarkerPresent    bool
}

// BindingHealth is one distinct binding's status: exactly one entry per opened
// binding, however many classes it serves.
type BindingHealth struct {
	Binding                 BindingName
	Provider                ProviderID
	ImplementationVersion   string
	SemanticContractVersion ContractVersion
	Classes                 []coordclass.Class
	Components              []ComponentHealth
	Capabilities            ClassCapabilities
	// Closed reports that this binding's handle has been closed. A closed
	// binding is reported rather than hidden: a status view that silently drops
	// it cannot distinguish an orderly shutdown from a lost handle.
	Closed bool
}

// Available reports whether one class is available on this binding.
func (b BindingHealth) Available(class coordclass.Class) bool {
	if b.Closed {
		return false
	}
	return b.Capabilities.For(class).Available
}

// HealthReport is the complete provider-neutral status view: the class-to-
// binding map plus one descriptor summary per distinct binding.
type HealthReport struct {
	Assignments []ClassAssignment
	Bindings    []BindingHealth
}

// BindingFor returns the health entry for one binding name.
func (r HealthReport) BindingFor(binding BindingName) (BindingHealth, bool) {
	for _, entry := range r.Bindings {
		if entry.Binding == binding {
			return entry, true
		}
	}
	return BindingHealth{}, false
}

// ClassHealth returns the binding health serving one class.
func (r HealthReport) ClassHealth(class coordclass.Class) (BindingHealth, bool) {
	for _, assignment := range r.Assignments {
		if assignment.Class == class {
			return r.BindingFor(assignment.Binding)
		}
	}
	return BindingHealth{}, false
}

// Health builds the status view from the bindings this lifecycle owns. It
// opens nothing and resolves nothing: every fact is read from the already-open
// handle's descriptor.
func (l *BindingLifecycle) Health() (HealthReport, error) {
	report := HealthReport{Assignments: l.Assignments()}
	for _, adopted := range l.Bindings() {
		entry, err := bindingHealth(adopted)
		if err != nil {
			return HealthReport{}, err
		}
		report.Bindings = append(report.Bindings, entry)
	}
	if err := report.validate(); err != nil {
		return HealthReport{}, err
	}
	return report, nil
}

func bindingHealth(adopted AdoptedBinding) (BindingHealth, error) {
	descriptor := adopted.Descriptor
	entry := BindingHealth{
		Binding:                 adopted.Binding,
		Provider:                descriptor.Provider,
		ImplementationVersion:   descriptor.ImplementationVersion,
		SemanticContractVersion: descriptor.SemanticContractVersion,
		Classes:                 adopted.Classes.Classes(),
		Capabilities:            descriptor.Capabilities,
		Closed:                  adopted.Closed,
	}
	for _, component := range descriptor.Components {
		entry.Components = append(entry.Components, ComponentHealth{
			Component:        component.ID,
			Classes:          component.Classes.Classes(),
			Format:           component.Format,
			SchemaVersion:    component.SchemaVersion,
			ABIVersion:       component.ABIVersion,
			PhysicalIdentity: component.PhysicalIdentity,
			MarkerName:       component.Marker.Name,
			MarkerPresent:    component.Marker.Present,
		})
	}
	if err := entry.validate(); err != nil {
		return BindingHealth{}, err
	}
	return entry, nil
}

// validate proves the binding declares every class the lifecycle assigned to
// it as available.
//
// It checks nothing else. Descriptor.Validate is the single gate for
// credential material and for the descriptor's own internal consistency — it
// already refuses a descriptor whose declared capabilities disagree with the
// classes its components carry — and adoption runs it before a handle can be
// owned. A second copy of either check here would be defense no test could
// prove load-bearing, which is how validation layers multiply until nobody
// knows which one is real.
func (b BindingHealth) validate() error {
	for _, class := range b.Classes {
		if !b.Capabilities.For(class).Available {
			return fmt.Errorf("%w: binding %q: class %s is assigned to a binding that declares it unavailable: %w", ErrInvalidHealthReport, b.Binding, class, ErrMissingCapability)
		}
	}
	return nil
}

func (r HealthReport) validate() error {
	if len(r.Assignments) == 0 || len(r.Bindings) == 0 {
		return fmt.Errorf("%w: the lifecycle owns no binding", ErrInvalidHealthReport)
	}
	return nil
}
