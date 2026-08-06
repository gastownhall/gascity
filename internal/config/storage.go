package config

import (
	"fmt"
	"sort"

	"github.com/BurntSushi/toml"
)

const (
	// StorageWorkBinding is the immutable reserved binding backed by the
	// bootstrap Work topology.
	StorageWorkBinding = "work"
	// StorageProviderSQLiteBeads is the built-in provider: one SQLite bead
	// ledger projected into the storage classes a binding serves.
	StorageProviderSQLiteBeads = "sqlite-beads"
	// DefaultSQLiteStoragePath is the provider-owned root used when a SQLite
	// binding omits path.
	DefaultSQLiteStoragePath = ".gc/store"
)

// StorageClass is one of the six closed semantic storage classes accepted by
// city.toml. It is config-owned so this leaf does not depend on the task-store
// classification package; storage composition translates it at the adapter
// edge.
type StorageClass string

const (
	// StorageClassWork is the shareable work ledger.
	StorageClassWork StorageClass = "work"
	// StorageClassGraph is formula graph state.
	StorageClassGraph StorageClass = "graph"
	// StorageClassSessions is session lifecycle and durable wait state.
	StorageClassSessions StorageClass = "sessions"
	// StorageClassMessaging is mail and external-message state.
	StorageClassMessaging StorageClass = "messaging"
	// StorageClassOrders is order-run state.
	StorageClassOrders StorageClass = "orders"
	// StorageClassNudges is durable nudge queue state.
	StorageClassNudges StorageClass = "nudges"
)

// String returns the canonical city.toml key for the class.
func (c StorageClass) String() string { return string(c) }

// StorageConfig assigns each semantic storage class to a named binding and
// defines every nonreserved binding used by those assignments; omitting
// `[storage]` keeps every class on the reserved `work` binding.
//
// A SQLite infrastructure split uses one shared binding:
//
//	[storage.classes]
//	work = "work"
//	graph = "infra"
//	sessions = "infra"
//	messaging = "infra"
//	orders = "infra"
//	nudges = "infra"
//
//	[storage.bindings.infra]
//	provider = "sqlite-beads"
//	path = ".gc/store"
//
// A non-built-in compiled provider uses an opaque configuration reference:
//
//	[storage.bindings.tasks]
//	provider = "<compiled-provider-id>"
//	config_ref = "city-work"
type StorageConfig struct {
	// Classes contains the complete class-to-binding assignment.
	Classes StorageClasses `toml:"classes" jsonschema:"required"`
	// Bindings defines named provider-backed bindings. The reserved work
	// binding is synthesized and must not appear here.
	Bindings map[string]StorageBindingConfig `toml:"bindings,omitempty"`
}

// StorageClasses is the closed set of semantic storage assignments; when
// `[storage]` is authored, all six fields are required after fragment layering,
// while omission assigns every class to `work`.
type StorageClasses struct {
	// Work selects the binding for the shareable work ledger.
	Work string `toml:"work" jsonschema:"required"`
	// Graph selects the binding for formula graph state.
	Graph string `toml:"graph" jsonschema:"required"`
	// Sessions selects the binding for session lifecycle and durable waits.
	Sessions string `toml:"sessions" jsonschema:"required"`
	// Messaging selects the binding for mail and external-message state.
	Messaging string `toml:"messaging" jsonschema:"required"`
	// Orders selects the binding for order-run state.
	Orders string `toml:"orders" jsonschema:"required"`
	// Nudges selects the binding for the durable nudge queue.
	Nudges string `toml:"nudges" jsonschema:"required"`
}

// StorageBindingConfig selects one compiled storage provider and its typed,
// secret-free configuration; SQLite accepts `path` (default `.gc/store`), while
// other providers accept an opaque `config_ref` resolved by that provider.
type StorageBindingConfig struct {
	// Provider is the exact ID of a provider compiled into this gc binary.
	Provider string `toml:"provider" jsonschema:"required"`
	// Path is the SQLite binding root. Empty defaults to ".gc/store".
	Path string `toml:"path,omitempty" jsonschema:"default=.gc/store"`
	// ConfigRef is an opaque, secret-free reference resolved by a non-built-in
	// provider.
	ConfigRef string `toml:"config_ref,omitempty"`
}

// Clone returns a detached storage configuration.
func (s StorageConfig) Clone() StorageConfig {
	out := s
	if s.Bindings != nil {
		out.Bindings = make(map[string]StorageBindingConfig, len(s.Bindings))
		for name, binding := range s.Bindings {
			out.Bindings[name] = binding
		}
	}
	return out
}

// BindingFor returns the binding assigned to one semantic class.
func (s StorageClasses) BindingFor(class StorageClass) string {
	switch class {
	case StorageClassWork:
		return s.Work
	case StorageClassGraph:
		return s.Graph
	case StorageClassSessions:
		return s.Sessions
	case StorageClassMessaging:
		return s.Messaging
	case StorageClassOrders:
		return s.Orders
	case StorageClassNudges:
		return s.Nudges
	default:
		return ""
	}
}

func (s *StorageClasses) setBinding(class StorageClass, binding string) {
	switch class {
	case StorageClassWork:
		s.Work = binding
	case StorageClassGraph:
		s.Graph = binding
	case StorageClassSessions:
		s.Sessions = binding
	case StorageClassMessaging:
		s.Messaging = binding
	case StorageClassOrders:
		s.Orders = binding
	case StorageClassNudges:
		s.Nudges = binding
	}
}

func defaultStorageConfig() StorageConfig {
	return StorageConfig{
		Classes: StorageClasses{
			Work:      StorageWorkBinding,
			Graph:     StorageWorkBinding,
			Sessions:  StorageWorkBinding,
			Messaging: StorageWorkBinding,
			Orders:    StorageWorkBinding,
			Nudges:    StorageWorkBinding,
		},
	}
}

// EffectiveStorage returns a detached, normalized storage configuration.
// Existing cities with no [storage] resolve every class to the reserved work
// binding. An omitted SQLite path resolves to DefaultSQLiteStoragePath.
func (c *City) EffectiveStorage() StorageConfig {
	if c == nil || c.Storage == nil {
		return defaultStorageConfig()
	}
	return normalizeStorageConfig(c.Storage.Clone())
}

func normalizeStorageConfig(storage StorageConfig) StorageConfig {
	for name, binding := range storage.Bindings {
		if binding.Provider == StorageProviderSQLiteBeads && binding.Path == "" {
			binding.Path = DefaultSQLiteStoragePath
			storage.Bindings[name] = binding
		}
	}
	return storage
}

// Equal reports whether two storage configurations select the same normalized
// assignments, providers, and provider configuration.
func (s StorageConfig) Equal(other StorageConfig) bool {
	left := normalizeStorageConfig(s.Clone())
	right := normalizeStorageConfig(other.Clone())
	if left.Classes != right.Classes || len(left.Bindings) != len(right.Bindings) {
		return false
	}
	for name, binding := range left.Bindings {
		if right.Bindings[name] != binding {
			return false
		}
	}
	return true
}

// StorageReloadRequiresRestart reports whether a reload changes any effective
// class assignment, provider, or provider configuration. Live storage handles
// are immutable; callers must keep the current configuration when this is true.
func StorageReloadRequiresRestart(current, next *City) bool {
	return !current.EffectiveStorage().Equal(next.EffectiveStorage())
}

// validateStorageLayer validates the storage surface of a single config
// layer — one city.toml or one include fragment, before composition. It checks
// only what a layer owns on its own: identifier syntax, the reserved work
// binding, and each binding's provider configuration. Class completeness and
// binding resolution span layers (a fragment may supply the binding a root
// class references, or the final class assignment), so those live in
// ValidateStorageConfig and run after include composition.
func validateStorageLayer(cfg *City) error {
	if cfg == nil || cfg.Storage == nil {
		return nil
	}
	storage := cfg.Storage
	for _, class := range storageConfigClassOrder() {
		binding := storage.Classes.BindingFor(class)
		if binding == "" {
			// A later layer may still assign this class.
			continue
		}
		if err := validateStorageIdentifier("binding name", binding); err != nil {
			return fmt.Errorf("storage.classes.%s: %w", class, err)
		}
	}

	for _, name := range sortedStorageBindingNames(storage.Bindings) {
		if name == StorageWorkBinding {
			return fmt.Errorf("storage.bindings.work is reserved and cannot be defined")
		}
		if err := validateStorageIdentifier("binding name", name); err != nil {
			return fmt.Errorf("storage.bindings.%s: %w", name, err)
		}
		if err := validateStorageBindingConfig(name, storage.Bindings[name]); err != nil {
			return err
		}
	}
	return nil
}

// ValidateStorageConfig validates the fully layered storage configuration:
// every layer-local invariant plus the cross-layer ones — all six classes
// assigned, and every assignment resolving to the reserved work binding or to
// a binding some layer defines. Call it on the composed root, not on a single
// parsed file. It performs no provider I/O and does not know the IDs of
// out-of-tree providers.
func ValidateStorageConfig(cfg *City) error {
	if err := validateStorageLayer(cfg); err != nil {
		return err
	}
	if cfg == nil || cfg.Storage == nil {
		return nil
	}
	storage := cfg.Storage
	for _, class := range storageConfigClassOrder() {
		binding := storage.Classes.BindingFor(class)
		if binding == "" {
			return fmt.Errorf("storage.classes.%s is required when [storage] is present", class)
		}
		if binding == StorageWorkBinding {
			continue
		}
		if _, ok := storage.Bindings[binding]; !ok {
			return fmt.Errorf("storage.classes.%s references undefined binding %q", class, binding)
		}
	}
	return nil
}

func validateStorageBindingConfig(name string, binding StorageBindingConfig) error {
	prefix := "storage.bindings." + name
	if err := validateStorageIdentifier("provider ID", binding.Provider); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if binding.Path != "" && binding.ConfigRef != "" {
		return fmt.Errorf("%s: path and config reference are mutually exclusive", prefix)
	}
	if binding.Provider == StorageProviderSQLiteBeads {
		if binding.ConfigRef != "" {
			return fmt.Errorf("%s: provider %q does not accept config_ref", prefix, StorageProviderSQLiteBeads)
		}
		return nil
	}
	if binding.Path != "" {
		return fmt.Errorf("%s: path is only supported by provider %q", prefix, StorageProviderSQLiteBeads)
	}
	if binding.ConfigRef == "" {
		return fmt.Errorf("%s: config_ref is required for provider %q", prefix, binding.Provider)
	}
	if err := validateStorageIdentifier("config_ref", binding.ConfigRef); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}

func validateStorageIdentifier(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", field)
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("%s has invalid characters", field)
	}
	return nil
}

// ValidateStorageBindings invokes validate once for every explicitly defined
// binding after syntax and reference validation. assigned is the typed set of
// classes that select the binding, in canonical config order. A compiled
// provider bundle can use this pre-open seam to reject unknown providers,
// provider-owned configuration, or unsupported class capabilities without
// teaching internal/config the IDs of out-of-tree providers.
func ValidateStorageBindings(
	cfg *City,
	validate func(name string, binding StorageBindingConfig, assigned []StorageClass) error,
) error {
	if err := ValidateStorageConfig(cfg); err != nil {
		return err
	}
	effective := cfg.EffectiveStorage()
	names := sortedStorageBindingNames(effective.Bindings)
	if len(names) > 0 && validate == nil {
		return fmt.Errorf("storage binding validator is required")
	}
	for _, name := range names {
		var assigned []StorageClass
		for _, class := range storageConfigClassOrder() {
			if effective.Classes.BindingFor(class) == name {
				assigned = append(assigned, class)
			}
		}
		if err := validate(name, effective.Bindings[name], assigned); err != nil {
			return fmt.Errorf("storage binding %q: %w", name, err)
		}
	}
	return nil
}

func storageConfigClassOrder() []StorageClass {
	return []StorageClass{
		StorageClassWork,
		StorageClassGraph,
		StorageClassSessions,
		StorageClassMessaging,
		StorageClassOrders,
		StorageClassNudges,
	}
}

func sortedStorageBindingNames(bindings map[string]StorageBindingConfig) []string {
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func validateStorageAuthoringSurface(md toml.MetaData) error {
	for _, key := range md.Undecoded() {
		parts := []string(key)
		if len(parts) == 0 || parts[0] != "storage" {
			continue
		}
		leaf := parts[len(parts)-1]
		if leaf == "migration" || leaf == "mode" {
			return fmt.Errorf("storage migration and mode keys are not supported")
		}
		if len(parts) >= 3 && parts[1] == "classes" && !isStorageClassName(parts[2]) {
			return fmt.Errorf("unknown storage class %q", parts[2])
		}
		return fmt.Errorf("unknown storage field %q", key.String())
	}
	return nil
}

func isStorageClassName(name string) bool {
	for _, class := range storageConfigClassOrder() {
		if class.String() == name {
			return true
		}
	}
	return false
}
