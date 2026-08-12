package storebinding

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

// TestHealthReportsOneDescriptorPerDistinctBinding is the diagnostic-parity
// assertion: however many classes share a binding, status shows that binding
// once, with the class assignments naming it.
func TestHealthReportsOneDescriptorPerDistinctBinding(t *testing.T) {
	lifecycle := NewBindingLifecycle()
	work := programClasses(t, coordclass.ClassWork)
	shared := programClasses(t, coordclass.ClassGraph, coordclass.ClassOrders, coordclass.ClassSessions, coordclass.ClassMessaging, coordclass.ClassNudges)
	if err := lifecycle.Adopt("work", programOpen(t, "work", work), work); err != nil {
		t.Fatalf("adopting work: %v", err)
	}
	if err := lifecycle.Adopt("infra", programOpen(t, "infra", shared), shared); err != nil {
		t.Fatalf("adopting infra: %v", err)
	}

	report, err := lifecycle.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(report.Bindings) != 2 {
		t.Fatalf("status reports %d bindings, want 2 (one per distinct opened binding)", len(report.Bindings))
	}
	if len(report.Assignments) != len(coordclass.Classes()) {
		t.Fatalf("status reports %d class assignments, want %d", len(report.Assignments), len(coordclass.Classes()))
	}

	infra, found := report.ClassHealth(coordclass.ClassGraph)
	if !found {
		t.Fatal("status has no entry for the Graph class")
	}
	if infra.Binding != "infra" {
		t.Errorf("Graph is served by binding %q, want infra", infra.Binding)
	}
	if infra.Provider != "test-provider" {
		t.Errorf("status reports provider %q, want test-provider", infra.Provider)
	}
	if infra.ImplementationVersion != "1.0.0" || infra.SemanticContractVersion != "gascity.storage-class.v1" {
		t.Errorf("status reports implementation %q / contract %q, want 1.0.0 / gascity.storage-class.v1", infra.ImplementationVersion, infra.SemanticContractVersion)
	}
	if len(infra.Components) != 1 {
		t.Fatalf("status reports %d components for infra, want 1", len(infra.Components))
	}
	component := infra.Components[0]
	if component.Format != "test-format" || component.SchemaVersion != "7" || component.ABIVersion != "test-abi-1" {
		t.Errorf("status reports format %q / schema %q / ABI %q, want test-format / 7 / test-abi-1",
			component.Format, component.SchemaVersion, component.ABIVersion)
	}
	if component.PhysicalIdentity != "infra-physical" {
		t.Errorf("status reports database identity %q, want infra-physical", component.PhysicalIdentity)
	}
	if !infra.Available(coordclass.ClassGraph) {
		t.Error("status reports the Graph class as unavailable on an open binding")
	}
	for _, class := range coordclass.Classes() {
		if _, found := report.ClassHealth(class); !found {
			t.Errorf("status has no entry for class %s", class)
		}
	}
}

// TestHealthNeverPrintsAComponentLocator pins the one fact status deliberately
// omits. The locator is the configured path; status is read by humans and
// shipped in support bundles, so the identity it prints is the provider's
// opaque physical identity.
func TestHealthNeverPrintsAComponentLocator(t *testing.T) {
	fields := map[string]bool{}
	for _, field := range componentHealthFieldNames() {
		fields[field] = true
	}
	if fields["Locator"] {
		t.Fatal("ComponentHealth carries the component locator; status must not print a configured path")
	}
	if !fields["PhysicalIdentity"] {
		t.Fatal("ComponentHealth no longer carries a database identity; status cannot tell two components apart")
	}
}

func componentHealthFieldNames() []string {
	componentType := reflect.TypeOf(ComponentHealth{})
	names := make([]string, 0, componentType.NumField())
	for index := 0; index < componentType.NumField(); index++ {
		names = append(names, componentType.Field(index).Name)
	}
	return names
}

// TestHealthReportsAClosedBindingAsUnavailable proves shutdown is visible.
// A closed binding that still reported its classes available would let a probe
// pass against storage nobody can reach.
func TestHealthReportsAClosedBindingAsUnavailable(t *testing.T) {
	lifecycle := NewBindingLifecycle()
	classes := programClasses(t, coordclass.ClassGraph)
	if err := lifecycle.Adopt("infra", programOpen(t, "infra", classes), classes); err != nil {
		t.Fatalf("adopting infra: %v", err)
	}
	before, err := lifecycle.Health()
	if err != nil {
		t.Fatalf("Health before close: %v", err)
	}
	if entry, _ := before.ClassHealth(coordclass.ClassGraph); !entry.Available(coordclass.ClassGraph) {
		t.Fatal("an open binding reports its class unavailable")
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after, err := lifecycle.Health()
	if err != nil {
		t.Fatalf("Health after close: %v", err)
	}
	entry, found := after.ClassHealth(coordclass.ClassGraph)
	if !found {
		t.Fatal("status dropped a closed binding; an orderly shutdown is indistinguishable from a lost handle")
	}
	if !entry.Closed {
		t.Error("status does not report the binding as closed")
	}
	if entry.Available(coordclass.ClassGraph) {
		t.Error("a closed binding still reports its class available")
	}
}

// TestHealthRejectsAnAssignmentTheBindingCannotHonour proves status fails
// loudly rather than printing a class-to-binding map the binding cannot serve.
// The lifecycle records the assignment a caller declares; only the descriptor
// knows whether the binding can honor it.
func TestHealthRejectsAnAssignmentTheBindingCannotHonour(t *testing.T) {
	lifecycle := NewBindingLifecycle()
	served := programClasses(t, coordclass.ClassGraph)
	claimed := programClasses(t, coordclass.ClassGraph, coordclass.ClassNudges)
	if err := lifecycle.Adopt("infra", programOpen(t, "infra", served), claimed); err != nil {
		t.Fatalf("adopting infra: %v", err)
	}
	if _, err := lifecycle.Health(); !errors.Is(err, ErrInvalidHealthReport) {
		t.Fatalf("Health over a mismatched assignment = %v, want ErrInvalidHealthReport", err)
	}
}

// TestAdoptionRejectsCredentialMaterial proves the descriptor is checked once,
// at the boundary where a handle enters the lifecycle. A credential-bearing
// descriptor never becomes adoptable, so it can never reach a status view or a
// support bundle downstream.
func TestAdoptionRejectsCredentialMaterial(t *testing.T) {
	classes := programClasses(t, coordclass.ClassGraph)
	handle := programOpen(t, "infra", classes)
	handle.descriptor.ImplementationVersion = "postgres://user:hunter2@db/gascity" // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
	lifecycle := NewBindingLifecycle()
	err := lifecycle.Adopt("infra", handle, classes)
	if !errors.Is(err, ErrInvalidBindingAdoption) {
		t.Fatalf("adopting a credential-bearing descriptor = %v, want ErrInvalidBindingAdoption", err)
	}
	if !errors.Is(err, ErrSecretMaterial) {
		t.Fatalf("adoption error = %v, want it to name the secret material", err)
	}
	if _, healthErr := lifecycle.Health(); !errors.Is(healthErr, ErrInvalidHealthReport) {
		t.Fatalf("Health over a lifecycle that owns nothing = %v, want ErrInvalidHealthReport", healthErr)
	}
}

// TestHealthOpensNothing is the "never re-resolve" assertion in its observable
// form: building the report touches no provider, so a lifecycle whose handles
// report their descriptor and nothing else answers status completely.
func TestHealthOpensNothing(t *testing.T) {
	classes := programClasses(t, coordclass.ClassGraph)
	handle := programOpen(t, "infra", classes)
	lifecycle := NewBindingLifecycle()
	if err := lifecycle.Adopt("infra", handle, classes); err != nil {
		t.Fatalf("adopting infra: %v", err)
	}
	for round := 0; round < 3; round++ {
		if _, err := lifecycle.Health(); err != nil {
			t.Fatalf("Health round %d: %v", round, err)
		}
	}
	if handle.closes != 0 {
		t.Fatalf("building status closed the handle %d times; a diagnostic must not touch the lifecycle", handle.closes)
	}
	// Status is served entirely from the snapshot taken at adoption, so a
	// handle that stopped answering cannot break it.
	if len(lifecycle.Bindings()) != 1 || lifecycle.Bindings()[0].Descriptor.Provider != "test-provider" {
		t.Fatalf("the lifecycle no longer carries the adopted descriptor: %+v", lifecycle.Bindings())
	}
}
