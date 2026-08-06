package storebinding

import (
	"errors"
	"strings"
	"testing"
)

func TestProviderRegistryRequiresExactFrozenRegistration(t *testing.T) {
	registry := NewProviderRegistry()
	first := &recordingFactory{id: ProviderID("builtin-one")}
	if err := registry.Register(first); err != nil {
		t.Fatalf("Register(first): %v", err)
	}
	if err := registry.Register(&recordingFactory{id: ProviderID("builtin-one")}); !errors.Is(err, ErrDuplicateProvider) {
		t.Fatalf("Register(duplicate) error = %v, want ErrDuplicateProvider", err)
	}
	if err := registry.Freeze(); err != nil {
		t.Fatalf("Freeze(): %v", err)
	}
	if err := registry.Register(&recordingFactory{id: ProviderID("builtin-two")}); !errors.Is(err, ErrProviderRegistryFrozen) {
		t.Fatalf("Register after Freeze error = %v, want ErrProviderRegistryFrozen", err)
	}

	_, err := registry.New(BindingSpec{Name: BindingName("infra"), Provider: ProviderID("builtin-missing")})
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("New(unknown) error = %v, want ErrUnknownProvider", err)
	}
	if first.calls != 0 {
		t.Fatalf("unknown lookup called a registered factory %d times", first.calls)
	}
}

func TestProviderRegistryUsesOneExactFactoryAndHasNoGlobalFallback(t *testing.T) {
	registry := NewProviderRegistry()
	wanted := &recordingFactory{id: ProviderID("builtin-wanted"), provider: &recordingProvider{}}
	other := &recordingFactory{id: ProviderID("builtin-other"), provider: &recordingProvider{}}
	for _, factory := range []ProviderFactory{wanted, other} {
		if err := registry.Register(factory); err != nil {
			t.Fatalf("Register(%s): %v", factory.ID(), err)
		}
	}
	if err := registry.Freeze(); err != nil {
		t.Fatalf("Freeze(): %v", err)
	}
	if _, err := registry.New(BindingSpec{Name: BindingName("infra"), Provider: wanted.id}); err != nil {
		t.Fatalf("New(exact): %v", err)
	}
	if wanted.calls != 1 || other.calls != 0 {
		t.Fatalf("factory calls = wanted:%d other:%d, want wanted:1 other:0", wanted.calls, other.calls)
	}

	separate := NewProviderRegistry()
	if err := separate.Freeze(); err != nil {
		t.Fatalf("separate Freeze(): %v", err)
	}
	if _, err := separate.New(BindingSpec{Name: BindingName("infra"), Provider: wanted.id}); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("separate registry error = %v, want ErrUnknownProvider", err)
	}
}

func TestProviderRegistryZeroValueSupportsRegisterFreezeLookupAndNew(t *testing.T) {
	var registry ProviderRegistry
	factory := &recordingFactory{id: ProviderID("builtin-zero-value"), provider: &recordingProvider{}}
	if err := registry.Register(factory); err != nil {
		t.Fatalf("zero-value Register(): %v", err)
	}
	if err := registry.Freeze(); err != nil {
		t.Fatalf("zero-value Freeze(): %v", err)
	}
	got, err := registry.Lookup(factory.id)
	if err != nil {
		t.Fatalf("zero-value Lookup(): %v", err)
	}
	if got != factory {
		t.Fatalf("zero-value Lookup() = %#v, want registered factory %#v", got, factory)
	}
	if _, err := registry.New(BindingSpec{Name: BindingName("zero-value"), Provider: factory.id}); err != nil {
		t.Fatalf("zero-value New(): %v", err)
	}
}

type recordingFactory struct {
	id       ProviderID
	provider Provider
	err      error
	calls    int
}

func (f *recordingFactory) ID() ProviderID { return f.id }

func (f *recordingFactory) New(BindingSpec) (Provider, error) {
	f.calls++
	return f.provider, f.err
}

func TestProviderRegistryRejectsInvalidLookupIDsAndFactoryValueWithError(t *testing.T) {
	registry := NewProviderRegistry()
	if err := registry.Register(&recordingFactory{id: ProviderID("builtin-safe"), provider: &recordingProvider{}}); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	if err := registry.Freeze(); err != nil {
		t.Fatalf("Freeze(): %v", err)
	}
	if _, err := registry.Lookup(ProviderID("sqlite://user:token@host")); !errors.Is(err, ErrInvalidProviderID) {
		t.Fatalf("Lookup(secret-shaped ID) error = %v, want ErrInvalidProviderID", err)
	} else if err != nil && (strings.Contains(err.Error(), "sqlite://") || strings.Contains(err.Error(), "token")) {
		t.Fatalf("Lookup(secret-shaped ID) leaked provider ID in %q", err)
	}

	broken := &recordingFactory{id: ProviderID("builtin-broken"), provider: &recordingProvider{}, err: errors.New("factory failed")}
	registry = NewProviderRegistry()
	if err := registry.Register(broken); err != nil {
		t.Fatalf("Register(broken): %v", err)
	}
	if err := registry.Freeze(); err != nil {
		t.Fatalf("Freeze(broken): %v", err)
	}
	if _, err := registry.New(BindingSpec{Name: BindingName("infra"), Provider: broken.id}); !errors.Is(err, ErrProviderFactoryContract) {
		t.Fatalf("New(factory value and error) error = %v, want ErrProviderFactoryContract", err)
	}
}
