package contract

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func frozenRegistry(t *testing.T, names ...BackendName) *BackendRegistry {
	t.Helper()
	registry := NewBackendRegistry()
	for _, name := range names {
		if err := registry.Register(name); err != nil {
			t.Fatalf("Register(%q) error = %v, want nil", string(name), err)
		}
	}
	if err := registry.Freeze(); err != nil {
		t.Fatalf("Freeze() error = %v, want nil", err)
	}
	return registry
}

func TestBackendRegistryRefusesDuplicateRegistration(t *testing.T) {
	registry := NewBackendRegistry()
	if err := registry.Register("dolt"); err != nil {
		t.Fatalf("first Register(dolt) error = %v, want nil", err)
	}

	err := registry.Register("dolt")
	if !errors.Is(err, ErrDuplicateBackend) {
		t.Fatalf("second Register(dolt) error = %v, want ErrDuplicateBackend", err)
	}
	if !strings.Contains(err.Error(), `"dolt"`) {
		t.Fatalf("duplicate refusal %q does not name the backend", err)
	}
}

// TestBackendRegistryRefusesDuplicateUnsetBackend pins that the no-backend-named
// sentinel is an ordinary registration rather than a special case: registering
// it twice is the same defect as registering "dolt" twice.
func TestBackendRegistryRefusesDuplicateUnsetBackend(t *testing.T) {
	registry := NewBackendRegistry()
	if err := registry.Register(UnsetBackend); err != nil {
		t.Fatalf("first Register(unset) error = %v, want nil", err)
	}
	if err := registry.Register(UnsetBackend); !errors.Is(err, ErrDuplicateBackend) {
		t.Fatalf("second Register(unset) error = %v, want ErrDuplicateBackend", err)
	}
}

func TestBackendRegistryRefusesRegistrationAfterFreeze(t *testing.T) {
	registry := frozenRegistry(t, "dolt")

	err := registry.Register("doltlite")
	if !errors.Is(err, ErrBackendRegistryFrozen) {
		t.Fatalf("Register after Freeze error = %v, want ErrBackendRegistryFrozen", err)
	}
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"dolt"}) {
		t.Fatalf("a refused registration still landed: Names() = %v", got)
	}
}

func TestBackendRegistryRefusesLookupBeforeFreeze(t *testing.T) {
	registry := NewBackendRegistry()
	if err := registry.Register("dolt"); err != nil {
		t.Fatal(err)
	}

	if err := registry.Lookup("dolt"); !errors.Is(err, ErrBackendRegistryNotFrozen) {
		t.Fatalf("Lookup before Freeze error = %v, want ErrBackendRegistryNotFrozen", err)
	}
}

func TestBackendRegistryRefusesInvalidName(t *testing.T) {
	registry := NewBackendRegistry()
	for _, name := range []BackendName{"Postgres", "post gres", "pg:14", "dolt\n"} {
		if err := registry.Register(name); !errors.Is(err, ErrInvalidBackendName) {
			t.Fatalf("Register(%q) error = %v, want ErrInvalidBackendName", string(name), err)
		}
	}
}

func TestBackendRegistryNamesOmitTheUnsetBackend(t *testing.T) {
	registry := frozenRegistry(t, UnsetBackend, "dolt", "doltlite")

	if got, want := registry.Names(), []string{"dolt", "doltlite"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v (registration order, no empty name)", got, want)
	}
	if err := registry.Lookup(UnsetBackend); err != nil {
		t.Fatalf("Lookup(unset) error = %v, want nil — metadata that names no backend is recognized", err)
	}
}

// TestBackendRegistryRefusalNamesBackendAndEnumeratesRegistered is the property
// the whole registry exists for: the refusal must be true about the binary that
// produced it, so an assembly registering a different set says a different
// sentence rather than reciting a list compiled into a format string.
func TestBackendRegistryRefusalNamesBackendAndEnumeratesRegistered(t *testing.T) {
	oss := frozenRegistry(t, UnsetBackend, "dolt", "doltlite")

	err := oss.Lookup("postgres")
	if !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("Lookup(postgres) error = %v, want ErrUnknownBackend", err)
	}
	var unknown *UnknownBackendError
	if !errors.As(err, &unknown) {
		t.Fatalf("Lookup error %T = %v, want *UnknownBackendError", err, err)
	}
	if unknown.Backend != "postgres" {
		t.Fatalf("UnknownBackendError.Backend = %q, want %q", unknown.Backend, "postgres")
	}
	if got, want := unknown.Registered, []string{"dolt", "doltlite"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UnknownBackendError.Registered = %v, want %v", got, want)
	}
	if got, want := err.Error(), `unsupported backend "postgres" (supported: dolt, doltlite); `+BackendNotOpenedGuarantee; got != want {
		t.Fatalf("refusal = %q, want %q", got, want)
	}

	withPostgres := frozenRegistry(t, UnsetBackend, "dolt", "doltlite", "postgres")
	if err := withPostgres.Lookup("postgres"); err != nil {
		t.Fatalf("an assembly that registers postgres refused it: %v", err)
	}
	if got, want := withPostgres.Lookup("postgress").Error(),
		`unsupported backend "postgress" (supported: dolt, doltlite, postgres); `+BackendNotOpenedGuarantee; got != want {
		t.Fatalf("refusal = %q, want %q", got, want)
	}
}

func TestBackendRegistryRefusalOnEmptyRegistrySaysNone(t *testing.T) {
	registry := frozenRegistry(t)

	if got, want := registry.Lookup("dolt").Error(),
		`unsupported backend "dolt" (supported: none); `+BackendNotOpenedGuarantee; got != want {
		t.Fatalf("refusal = %q, want %q", got, want)
	}
}

// TestCompiledBackendBundleIsWellFormed pins the composition root itself. The
// bundle is the one list an assembly edits, and a duplicate or malformed entry
// in it would otherwise surface as an unrelated failure at a city's first read.
func TestCompiledBackendBundleIsWellFormed(t *testing.T) {
	registry, err := compiledBackendRegistry()
	if err != nil {
		t.Fatalf("compiledBackendRegistry() error = %v, want nil", err)
	}
	if err := registry.Register("late"); !errors.Is(err, ErrBackendRegistryFrozen) {
		t.Fatalf("the compiled registry is not frozen: Register error = %v", err)
	}
}

// TestCompiledBackendBundleCarriesOnlyTheBackendsGCImplements replaces the
// pin that held the opposite while E0 was in flight.
//
// E0 kept postgres registered deliberately, because removing it before an
// assembly could add it back would have left OSS unable to read a city whose
// metadata named it. That reason is gone: a city served by a backend gc does
// not implement now names an opaque storage binding, which is recognized
// before metadata parsing and served by withholding the projected environment
// whole. A name belongs in this bundle only when gc reads its metadata shape,
// projects its environment and manages its runtime — and postgres is now an
// assembly's registration, not OSS's.
//
// The exact-equality assertion is the point. A registry that quietly grew a
// name would make every refusal message in gc describe a binary nobody built.
func TestCompiledBackendBundleCarriesOnlyTheBackendsGCImplements(t *testing.T) {
	names, err := RegisteredBackends()
	if err != nil {
		t.Fatalf("RegisteredBackends() error = %v, want nil", err)
	}
	if want := []string{"dolt", "doltlite"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("RegisteredBackends() = %v, want %v", names, want)
	}
	if err := RecognizeBackend("postgres"); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("RecognizeBackend(postgres) error = %v, want ErrUnknownBackend", err)
	}
}

func TestRecognizeBackendAcceptsEveryCompiledName(t *testing.T) {
	for _, name := range compiledBackendNames() {
		if err := RecognizeBackend(string(name)); err != nil {
			t.Fatalf("RecognizeBackend(%q) error = %v, want nil", string(name), err)
		}
	}
}

func TestRecognizeBackendRefusalEnumeratesTheCompiledSet(t *testing.T) {
	err := RecognizeBackend("postgress")
	if !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("RecognizeBackend(postgress) error = %v, want ErrUnknownBackend", err)
	}

	names, namesErr := RegisteredBackends()
	if namesErr != nil {
		t.Fatal(namesErr)
	}
	want := `unsupported backend "postgress" (supported: ` + strings.Join(names, ", ") + `); ` + BackendNotOpenedGuarantee
	if err.Error() != want {
		t.Fatalf("refusal = %q, want %q", err.Error(), want)
	}
	if !strings.Contains(err.Error(), BackendNotOpenedGuarantee) {
		t.Fatalf("refusal %q drops the data-safety guarantee", err)
	}
}
