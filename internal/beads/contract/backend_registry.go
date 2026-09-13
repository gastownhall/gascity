package contract

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// BackendName is the value of the `backend` key in .beads/metadata.json. It
// names the storage engine bd serves a scope from; it is not a storage-provider
// ID (that vocabulary belongs to internal/storebinding and is registered
// separately).
type BackendName string

// UnsetBackend is metadata that names no backend at all. It is recognized —
// scopes written before the key existed, and scopes that inherit their city's
// backend, legitimately carry it — but it is never enumerated as something an
// operator may write, because an absent name is not a name.
const UnsetBackend BackendName = ""

// Backend-name registry errors.
var (
	// ErrUnknownBackend reports a backend name this assembly does not register.
	ErrUnknownBackend = errors.New("unknown beads backend")
	// ErrDuplicateBackend reports one backend name registered twice.
	ErrDuplicateBackend = errors.New("duplicate beads backend")
	// ErrInvalidBackendName reports a backend name that cannot be registered.
	ErrInvalidBackendName = errors.New("invalid beads backend name")
	// ErrBackendRegistryFrozen reports a registration attempted after Freeze.
	ErrBackendRegistryFrozen = errors.New("beads backend registry is frozen")
	// ErrBackendRegistryNotFrozen reports a lookup attempted before Freeze.
	ErrBackendRegistryNotFrozen = errors.New("beads backend registry is not frozen")
	// ErrBackendRegistryUnavailable reports a registry that cannot answer.
	ErrBackendRegistryUnavailable = errors.New("beads backend registry is unavailable")
)

// BackendNotOpenedGuarantee is the fail-closed data-safety clause carried by
// every backend refusal: refusing a scope reads its metadata and stops there.
// An operator who meets one of these messages needs to know that the database
// the metadata names is exactly as they left it before they start recovering.
const BackendNotOpenedGuarantee = "no storage database was opened or modified"

// BackendRegistry is an explicit, freezeable, non-global registry of the
// backend names one assembly of gc recognizes.
//
// It is the storebinding.ProviderRegistry shape applied to a second vocabulary:
// registration is explicit and returns an error rather than panicking, the
// registry is frozen before it is read, and lookups return a typed error. There
// is deliberately no package-level Register: which backends a build knows is a
// property of the assembly being built, not of who imported what.
type BackendRegistry struct {
	mu     sync.RWMutex
	known  map[BackendName]struct{}
	names  []BackendName
	frozen bool
}

// NewBackendRegistry creates an empty explicit backend-name registry.
func NewBackendRegistry() *BackendRegistry {
	return &BackendRegistry{known: make(map[BackendName]struct{})}
}

// Register adds one backend name before the registry is frozen. UnsetBackend
// registers the no-backend-named shape and, like every other name, may be
// registered only once.
func (r *BackendRegistry) Register(name BackendName) error {
	if r == nil {
		return fmt.Errorf("%w: registry is nil", ErrBackendRegistryUnavailable)
	}
	if err := validateBackendName(name); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return fmt.Errorf("%w: %q", ErrBackendRegistryFrozen, string(name))
	}
	if r.known == nil {
		r.known = make(map[BackendName]struct{})
	}
	if _, exists := r.known[name]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateBackend, string(name))
	}
	r.known[name] = struct{}{}
	if name != UnsetBackend {
		r.names = append(r.names, name)
	}
	return nil
}

// Freeze prevents further registration and enables lookups.
func (r *BackendRegistry) Freeze() error {
	if r == nil {
		return fmt.Errorf("%w: registry is nil", ErrBackendRegistryUnavailable)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
	return nil
}

// Lookup reports whether a backend name is registered. A registered name
// returns nil; anything else returns an *UnknownBackendError carrying both the
// refused name and the registered set.
func (r *BackendRegistry) Lookup(name BackendName) error {
	if r == nil {
		return fmt.Errorf("%w: registry is nil", ErrBackendRegistryUnavailable)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.frozen {
		return fmt.Errorf("%w: %q", ErrBackendRegistryNotFrozen, string(name))
	}
	if _, ok := r.known[name]; ok {
		return nil
	}
	return &UnknownBackendError{Backend: string(name), Registered: r.namesLocked()}
}

// Names returns the registered operator-selectable backend names in
// registration order. UnsetBackend is never included.
func (r *BackendRegistry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namesLocked()
}

func (r *BackendRegistry) namesLocked() []string {
	names := make([]string, 0, len(r.names))
	for _, name := range r.names {
		names = append(names, string(name))
	}
	return names
}

func validateBackendName(name BackendName) error {
	if name == UnsetBackend {
		return nil
	}
	for _, runeValue := range string(name) {
		if (runeValue >= 'a' && runeValue <= 'z') || (runeValue >= '0' && runeValue <= '9') || runeValue == '-' || runeValue == '_' {
			continue
		}
		return fmt.Errorf("%w: %q", ErrInvalidBackendName, string(name))
	}
	return nil
}

// UnknownBackendError refuses a backend name the registry does not carry.
//
// It reports three things on purpose, and every backend refusal in gc renders
// the same three: the name that was refused, what this build actually
// registers, and the guarantee that nothing was opened. The middle one is why
// this type exists — an assembly that registers a different set must say so
// rather than recite a list compiled into a message somewhere else, or the
// operator is debugging against a build they do not have.
type UnknownBackendError struct {
	// Backend is the refused name, exactly as metadata spelled it.
	Backend string
	// Registered is the operator-selectable set this assembly carries.
	Registered []string
}

// Error implements error.
func (e *UnknownBackendError) Error() string {
	return fmt.Sprintf("unsupported backend %q (supported: %s); %s", e.Backend, e.supported(), BackendNotOpenedGuarantee)
}

// Unwrap supports errors.Is.
func (e *UnknownBackendError) Unwrap() error { return ErrUnknownBackend }

func (e *UnknownBackendError) supported() string {
	if len(e.Registered) == 0 {
		return "none"
	}
	return strings.Join(e.Registered, ", ")
}
