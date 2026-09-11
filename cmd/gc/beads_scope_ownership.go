package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// scopeOwnershipFile records the durable lifecycle owner for scopes that were
// created through the provider-owned beads front door. It deliberately does
// not describe a running process; provider state remains observable from the
// provider itself.
const scopeOwnershipFile = "scope-ownership.json"

const (
	providerScopeLifecycleOwner = "provider"
	providerScopeInitializing   = "provider_initializing"
	providerScopeReady          = "ready"
)

type providerScopeIntent struct {
	Transport string `json:"transport,omitempty"`
	Target    string `json:"target,omitempty"`
}

type providerScopeOwnershipEntry struct {
	ScopePath      string              `json:"scope_path"`
	LifecycleOwner string              `json:"lifecycle_owner"`
	State          string              `json:"state"`
	Intent         providerScopeIntent `json:"intent,omitempty"`
}

type providerScopeOwnershipJournal struct {
	Version int                                    `json:"version"`
	Scopes  map[string]providerScopeOwnershipEntry `json:"scopes"`
}

func providerScopeOwnershipPath(cityPath string) string {
	return filepath.Join(normalizePathForCompare(cityPath), ".gc", scopeOwnershipFile)
}

func providerScopeOwnershipKey(cityPath, scopeRoot string) string {
	cityPath = normalizePathForCompare(cityPath)
	scopeRoot = normalizePathForCompare(scopeRoot)
	if samePath(cityPath, scopeRoot) {
		return "city"
	}
	if cfg, err := loadCityConfig(cityPath, io.Discard); err == nil && cfg != nil {
		resolveRigPaths(cityPath, cfg.Rigs)
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Name) != "" && samePath(rig.Path, scopeRoot) {
				return "rig:" + rig.Name
			}
		}
	}
	return "path:" + scopeRoot
}

func normalizeProviderScopeIntent(intent providerScopeIntent) (providerScopeIntent, error) {
	intent.Transport = strings.ToLower(strings.TrimSpace(intent.Transport))
	intent.Target = strings.ToLower(strings.TrimSpace(intent.Target))
	if intent.Transport == "" || intent.Target == "" {
		return providerScopeIntent{}, fmt.Errorf("provider scope ownership requires both transport and target")
	}
	if intent.Transport != "direct" && intent.Transport != "proxied" {
		return providerScopeIntent{}, fmt.Errorf("unsupported provider scope transport %q", intent.Transport)
	}
	if intent.Target != "local" && intent.Target != "external" {
		return providerScopeIntent{}, fmt.Errorf("unsupported provider scope target %q", intent.Target)
	}
	return intent, nil
}

func loadProviderScopeOwnershipJournal(cityPath string) (providerScopeOwnershipJournal, bool, error) {
	path := providerScopeOwnershipPath(cityPath)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return providerScopeOwnershipJournal{}, false, nil
	}
	if err != nil {
		return providerScopeOwnershipJournal{}, false, fmt.Errorf("read scope ownership journal: %w", err)
	}
	var journal providerScopeOwnershipJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return providerScopeOwnershipJournal{}, false, fmt.Errorf("parse scope ownership journal: %w", err)
	}
	if journal.Version != 1 || journal.Scopes == nil {
		return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership journal")
	}
	for key, entry := range journal.Scopes {
		canonicalEntryPath := normalizePathForCompare(entry.ScopePath)
		if key == "" || entry.ScopePath == "" || !filepath.IsAbs(entry.ScopePath) || canonicalEntryPath != entry.ScopePath {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("scope ownership journal path drift for %q", key)
		}
		if strings.HasPrefix(key, "path:") && key != "path:"+entry.ScopePath {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("scope ownership journal path key mismatch for %q", key)
		}
		if key != "city" && !strings.HasPrefix(key, "rig:") && !strings.HasPrefix(key, "path:") {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership journal key %q", key)
		}
		if strings.HasPrefix(key, "rig:") && strings.TrimSpace(strings.TrimPrefix(key, "rig:")) == "" {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership journal key %q", key)
		}
		if entry.LifecycleOwner != providerScopeLifecycleOwner {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership lifecycle owner for %q", key)
		}
		switch entry.State {
		case providerScopeInitializing:
			if _, err := normalizeProviderScopeIntent(entry.Intent); err != nil {
				return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid initializing scope ownership for %q: %w", key, err)
			}
		case providerScopeReady:
			if entry.Intent != (providerScopeIntent{}) {
				return providerScopeOwnershipJournal{}, false, fmt.Errorf("ready scope ownership for %q retains initialization intent", key)
			}
		default:
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("invalid scope ownership state for %q", key)
		}
	}
	seenPaths := make(map[string]string, len(journal.Scopes))
	for key, entry := range journal.Scopes {
		if prior, duplicate := seenPaths[entry.ScopePath]; duplicate {
			return providerScopeOwnershipJournal{}, false, fmt.Errorf("duplicate scope ownership paths for %q and %q", prior, key)
		}
		seenPaths[entry.ScopePath] = key
	}
	return journal, true, nil
}

// providerScopeOwnershipHasInitializingEntry reports whether any durable
// scope in this city still needs bd's fresh provider contract. A pending rig
// is enough to require the newer bd floor even when the city scope itself is
// already ready.
func providerScopeOwnershipHasInitializingEntry(cityPath string) (bool, error) {
	journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
	if err != nil || !exists {
		return false, err
	}
	for _, entry := range journal.Scopes {
		if entry.State == providerScopeInitializing {
			return true, nil
		}
	}
	return false, nil
}

func providerScopeOwnership(cityPath, scopeRoot string) (providerScopeOwnershipEntry, bool, error) {
	_, entry, owned, err := providerScopeOwnershipRecord(cityPath, scopeRoot)
	return entry, owned, err
}

// providerScopeOwnershipRecord resolves the current configured key first,
// then the unique durable scope path. A detached path record is intentionally
// sufficient while rig.Provision initializes an adopted rig before city.toml
// is written.
func providerScopeOwnershipRecord(cityPath, scopeRoot string) (string, providerScopeOwnershipEntry, bool, error) {
	journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
	if err != nil || !exists {
		return "", providerScopeOwnershipEntry{}, false, err
	}
	return providerScopeOwnershipRecordFromJournal(journal, cityPath, scopeRoot)
}

func scopeProviderOwned(cityPath, scopeRoot string) (bool, error) {
	_, owned, err := providerScopeOwnership(cityPath, scopeRoot)
	if err != nil || owned {
		return owned, err
	}
	return committedBeadsHandoffOwnsScope(scopeRoot)
}

func cityScopeProviderOwned(cityPath string) (bool, error) {
	return scopeProviderOwned(cityPath, cityPath)
}

func validateProviderScopeOwnership(cityPath string, cfg *config.City) error {
	journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	expected := map[string]string{"city": normalizePathForCompare(cityPath)}
	if cfg == nil {
		for key, entry := range journal.Scopes {
			if key == "city" && samePath(entry.ScopePath, expected[key]) {
				continue
			}
			if strings.HasPrefix(key, "path:") {
				continue
			}
			if want, ok := expected[key]; !ok || !samePath(entry.ScopePath, want) {
				return fmt.Errorf("scope ownership journal path drift for %q", key)
			}
		}
		return nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		expected["rig:"+rig.Name] = normalizePathForCompare(rig.Path)
	}
	for key, entry := range journal.Scopes {
		if strings.HasPrefix(key, "path:") {
			continue
		}
		want, ok := expected[key]
		if !ok || !samePath(entry.ScopePath, want) {
			return fmt.Errorf("scope ownership journal path drift for %q", key)
		}
	}
	if _, cityProviderOwned := journal.Scopes["city"]; cityProviderOwned {
		for key, root := range expected {
			if key == "city" {
				continue
			}
			if _, _, recorded, err := providerScopeOwnershipRecord(cityPath, root); err != nil {
				return err
			} else if recorded {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, ".beads", "metadata.json")); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("fresh scope %q has no provider ownership record", key)
			} else if err != nil {
				return fmt.Errorf("inspect scope %q ownership marker: %w", key, err)
			}
		}
	}
	return nil
}

// ensureFreshRigProviderOwnership records a newly added, still-uninitialized
// rig before startup validation can route it through a legacy lifecycle. The
// city entry is intentionally left alone: this boundary exists for a fresh
// rig added to an established city.
func ensureFreshRigProviderOwnership(cityPath string, cfg *config.City) error {
	if cfg == nil || !cityUsesManagedDoltBeadsLifecycle(cityPath) {
		return nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	// Do not resolve a city's initialization topology until there is a fresh
	// rig that needs one. Existing embedded and non-Dolt cities remain valid
	// unchanged cities when no new scope is being added.
	freshRigs := make([]config.Rig, 0, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if key, _, owned, err := providerScopeOwnershipRecord(cityPath, rig.Path); err != nil {
			return err
		} else if owned {
			if err := attachProviderScopeOwnershipRecord(cityPath, rig.Name, rig.Path, key); err != nil {
				return err
			}
		}
		initialized, err := scopeHasPersistedBeadsIdentity(rig.Path)
		if err != nil {
			return fmt.Errorf("inspect fresh rig %q: %w", rig.Name, err)
		}
		if !initialized {
			freshRigs = append(freshRigs, rig)
		}
	}
	if len(freshRigs) == 0 {
		return nil
	}
	cityInitialized, err := scopeHasPersistedBeadsIdentity(cityPath)
	if err != nil {
		return fmt.Errorf("inspect city beads identity: %w", err)
	}
	var intent providerScopeIntent
	if cityInitialized {
		intent, err = providerOwnershipIntentFromPersistedCity(cityPath)
	} else {
		intent, err = freshScopeProviderOwnershipIntent(cityPath, *cfg)
	}
	if err != nil {
		return err
	}
	for _, rig := range freshRigs {
		if key, _, owned, err := providerScopeOwnershipRecord(cityPath, rig.Path); err != nil {
			return err
		} else if owned {
			if err := attachProviderScopeOwnershipRecord(cityPath, rig.Name, rig.Path, key); err != nil {
				return err
			}
		} else {
			if err := persistProviderScopeOwnership(cityPath, rig.Path, intent); err != nil {
				return fmt.Errorf("record provider ownership for fresh rig %q: %w", rig.Name, err)
			}
		}
	}
	return nil
}

func attachProviderScopeOwnershipRecord(cityPath, rigName, scopeRoot, actualKey string) error {
	if !strings.HasPrefix(actualKey, "path:") || strings.TrimSpace(rigName) == "" {
		return nil
	}
	want := "rig:" + rigName
	return withProviderScopeOwnershipLock(cityPath, func() error {
		journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
		if err != nil || !exists {
			return err
		}
		entry, ok := journal.Scopes[actualKey]
		if !ok || !samePath(entry.ScopePath, scopeRoot) {
			return fmt.Errorf("missing detached ownership for rig %q", rigName)
		}
		if existing, collision := journal.Scopes[want]; collision && !samePath(existing.ScopePath, scopeRoot) {
			return fmt.Errorf("scope ownership journal key collision for %q", want)
		}
		delete(journal.Scopes, actualKey)
		journal.Scopes[want] = entry
		return writeProviderScopeOwnershipJournal(cityPath, journal)
	})
}

// freshScopeProviderOwnershipIntent keeps every fresh rig on the city's
// durable pending intent while first initialization is incomplete. Falling
// back to the default here would silently turn an explicit direct/external
// city into a proxied/local rig after a failed preflight.
func freshScopeProviderOwnershipIntent(cityPath string, cfg config.City) (providerScopeIntent, error) {
	entry, owned, err := providerScopeOwnership(cityPath, cityPath)
	if err != nil {
		return providerScopeIntent{}, err
	}
	if owned && entry.State == providerScopeInitializing {
		return normalizeProviderScopeIntent(entry.Intent)
	}
	return (hostedDoltInitOptions{}).providerOwnershipIntent(cfg)
}

// ensureProviderScopeOwnershipBeforeInit is the common Provision boundary for
// a new scope. It runs before InitStore can invoke bd and create metadata, so
// a failed provision cannot subsequently be mistaken for a legacy store.
func ensureProviderScopeOwnershipBeforeInit(cityPath, scopeRoot string) error {
	if !cityUsesManagedDoltBeadsLifecycle(cityPath) {
		return nil
	}
	if _, owned, err := providerScopeOwnership(cityPath, scopeRoot); err != nil || owned {
		return err
	}
	initialized, err := scopeHasPersistedBeadsIdentity(scopeRoot)
	if err != nil {
		return fmt.Errorf("inspect scope beads identity: %w", err)
	}
	if initialized {
		return nil
	}
	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return fmt.Errorf("load city config for provider ownership: %w", err)
	}
	var intent providerScopeIntent
	if samePath(cityPath, scopeRoot) {
		intent, err = (hostedDoltInitOptions{}).providerOwnershipIntent(*cfg)
	} else {
		cityInitialized, inspectErr := scopeHasPersistedBeadsIdentity(cityPath)
		if inspectErr != nil {
			return fmt.Errorf("inspect city beads identity: %w", inspectErr)
		}
		if cityInitialized {
			intent, err = providerOwnershipIntentFromPersistedCity(cityPath)
		} else {
			intent, err = freshScopeProviderOwnershipIntent(cityPath, *cfg)
		}
	}
	if err != nil {
		return err
	}
	if err := persistProviderScopeOwnership(cityPath, scopeRoot, intent); err != nil {
		return fmt.Errorf("record provider ownership before store initialization: %w", err)
	}
	return nil
}

// providerOwnershipIntentFromPersistedCity derives the topology a new rig
// inherits from the city's durable beads binding. City.toml is only a legacy
// compatibility input, so it must not reclassify an existing direct city as
// the fresh proxied-local default.
func providerOwnershipIntentFromPersistedCity(cityPath string) (providerScopeIntent, error) {
	metadata, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(cityPath))
	if err != nil {
		return providerScopeIntent{}, fmt.Errorf("load city beads metadata: %w", err)
	}
	if !ok {
		return providerScopeIntent{}, fmt.Errorf("missing city beads metadata")
	}
	if contract.IsDoltBackend(strings.TrimSpace(metadata.Backend)) && strings.EqualFold(strings.TrimSpace(metadata.DoltMode), "embedded") {
		// Embedded Beads metadata has no server transport a fresh rig can
		// inherit. Preserve the city exactly as it is and initialize the new
		// provider-owned rig with the normal fresh-scope default.
		return providerScopeIntent{Transport: "proxied", Target: "local"}, nil
	}
	state, configured, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil {
		return providerScopeIntent{}, fmt.Errorf("load city beads config: %w", err)
	}
	target, err := providerOwnershipTargetFromBinding(cityPath, metadata.DoltMode, state, configured)
	if err != nil {
		return providerScopeIntent{}, err
	}
	resolved, err := contract.ResolveInitIntent(
		contract.InitScopeState{Initialized: true, Backend: metadata.Backend, DoltMode: metadata.DoltMode, Target: target},
		contract.InitIntent{}, contract.InitIntent{}, contract.InitIntent{}, contract.InitIntent{},
	)
	if err != nil {
		return providerScopeIntent{}, fmt.Errorf("resolve persisted city beads topology: %w", err)
	}
	if resolved.PreserveBackend {
		return providerScopeIntent{}, fmt.Errorf("city beads backend %q is authoritative; cannot initialize a managed Dolt rig", metadata.Backend)
	}
	return normalizeProviderScopeIntent(providerScopeIntent{Transport: resolved.Intent.Transport, Target: resolved.Intent.Target})
}

// providerOwnershipTargetFromBinding determines whether a durable bd binding
// owns a local process. Endpoint fields alone cannot answer that question:
// transferred direct scopes can retain a canonical loopback endpoint, while
// external proxied scopes record their upstream only in bd's sidecar.
func providerOwnershipTargetFromBinding(cityPath, doltMode string, state contract.ConfigState, configured bool) (string, error) {
	transport, err := persistedProviderTransport(doltMode)
	if err != nil {
		return "", err
	}
	if transport == "proxied" {
		external, err := proxiedScopeHasExternalUpstream(cityPath)
		if err != nil {
			return "", err
		}
		if external {
			return "external", nil
		}
		return "local", nil
	}
	if !configured {
		// An old direct scope without a canonical binding predates provider
		// ownership. Leave its conservative legacy target local.
		return "local", nil
	}
	// GC's legacy direct lifecycle owns a managed_city binding even though it
	// writes auto-start=false to keep bd from starting a competing server.
	// That marker remains authoritative when a fresh rig inherits the city.
	if state.EndpointOrigin == contract.EndpointOriginManagedCity {
		return "local", nil
	}
	disabled, err := contract.ReadAutoStartDisabled(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil {
		return "", fmt.Errorf("read city beads auto-start policy: %w", err)
	}
	if !disabled {
		return "local", nil
	}
	return "external", nil
}

func persistedProviderTransport(doltMode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(doltMode)) {
	case "", "server":
		return "direct", nil
	case "proxied-server":
		return "proxied", nil
	default:
		return "", fmt.Errorf("persisted dolt mode %q is unsupported", doltMode)
	}
}

// proxiedScopeHasExternalUpstream reads bd's durable proxied-server client
// sidecar. It intentionally does not infer locality from host spelling: a
// loopback TCP or Unix-socket upstream can still be external to this scope.
func proxiedScopeHasExternalUpstream(cityPath string) (bool, error) {
	path := filepath.Join(cityPath, ".beads", "proxied_server_client_info.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read proxied server binding: %w", err)
	}
	var sidecar struct {
		External *struct {
			Host   string `json:"host"`
			Port   int    `json:"port"`
			Socket string `json:"socket"`
		} `json:"external"`
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return false, fmt.Errorf("parse proxied server binding: %w", err)
	}
	if sidecar.External == nil {
		return false, nil
	}
	if strings.TrimSpace(sidecar.External.Socket) != "" {
		return true, nil
	}
	if strings.TrimSpace(sidecar.External.Host) == "" || sidecar.External.Port < 1 || sidecar.External.Port > 65535 {
		return false, fmt.Errorf("invalid external proxied server binding in %s", path)
	}
	return true, nil
}

func persistProviderScopeOwnership(cityPath, scopeRoot string, intent providerScopeIntent) error {
	intent, err := normalizeProviderScopeIntent(intent)
	if err != nil {
		return err
	}
	cityPath = normalizePathForCompare(cityPath)
	scopeRoot = normalizePathForCompare(scopeRoot)
	if cityPath == "" || scopeRoot == "" {
		return fmt.Errorf("scope ownership requires city and scope paths")
	}
	key := providerScopeOwnershipKey(cityPath, scopeRoot)
	return withProviderScopeOwnershipLock(cityPath, func() error {
		journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
		if err != nil {
			return err
		}
		if !exists {
			journal = providerScopeOwnershipJournal{Version: 1, Scopes: map[string]providerScopeOwnershipEntry{}}
		}
		actualKey, current, recorded, err := providerScopeOwnershipRecordFromJournal(journal, cityPath, scopeRoot)
		if err != nil {
			return err
		}
		if recorded {
			if actualKey != key {
				// A detached path record retains this physical scope while it is
				// absent from city.toml or being adopted before that write.
				key = actualKey
			}
			if !samePath(current.ScopePath, scopeRoot) {
				return fmt.Errorf("scope ownership journal path drift for %q", key)
			}
			if current.LifecycleOwner != providerScopeLifecycleOwner {
				return fmt.Errorf("conflicting lifecycle owner for scope %q", scopeRoot)
			}
			if current.State == providerScopeReady {
				return fmt.Errorf("scope %q is already provider-owned and ready", scopeRoot)
			}
			if current.Intent != intent {
				return fmt.Errorf("conflicting provider initialization intent for scope %q", scopeRoot)
			}
			return nil
		}
		journal.Scopes[key] = providerScopeOwnershipEntry{
			ScopePath: scopeRoot, LifecycleOwner: providerScopeLifecycleOwner,
			State: providerScopeInitializing, Intent: intent,
		}
		return writeProviderScopeOwnershipJournal(cityPath, journal)
	})
}

func markProviderScopeOwnershipReady(cityPath, scopeRoot string) error {
	cityPath = normalizePathForCompare(cityPath)
	scopeRoot = normalizePathForCompare(scopeRoot)
	key := providerScopeOwnershipKey(cityPath, scopeRoot)
	return withProviderScopeOwnershipLock(cityPath, func() error {
		journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("missing scope ownership journal for %q", scopeRoot)
		}
		actualKey, entry, recorded, err := providerScopeOwnershipRecordFromJournal(journal, cityPath, scopeRoot)
		if err != nil {
			return err
		}
		if !recorded || entry.LifecycleOwner != providerScopeLifecycleOwner {
			return fmt.Errorf("missing provider ownership for scope %q", scopeRoot)
		}
		key = actualKey
		if !samePath(entry.ScopePath, scopeRoot) {
			return fmt.Errorf("scope ownership journal path drift for %q", key)
		}
		if entry.State == providerScopeReady {
			return nil
		}
		if entry.State != providerScopeInitializing {
			return fmt.Errorf("invalid provider ownership state for scope %q", scopeRoot)
		}
		entry.State = providerScopeReady
		entry.Intent = providerScopeIntent{}
		journal.Scopes[key] = entry
		if err := writeProviderScopeOwnershipJournal(cityPath, journal); err != nil {
			return err
		}
		clearSelectorExternalInitOptions(cityPath, scopeRoot)
		return nil
	})
}

// removeProviderScopeOwnershipRecord detaches a configured rig's label before
// city.toml is mutated. The path remains durable identity so a failed config
// write stays retryable and an adopted re-add can reattach before Provision
// writes city.toml.
func removeProviderScopeOwnershipRecord(cityPath, key string) error {
	if !strings.HasPrefix(key, "rig:") || strings.TrimSpace(strings.TrimPrefix(key, "rig:")) == "" {
		return fmt.Errorf("invalid removable provider scope key %q", key)
	}
	// Legacy cities have no journal. Avoid creating the journal lock (and its
	// containing .gc directory) merely because a legacy rig is removed.
	_, exists, err := loadProviderScopeOwnershipJournal(cityPath)
	if err != nil || !exists {
		return err
	}
	return withProviderScopeOwnershipLock(cityPath, func() error {
		journal, exists, err := loadProviderScopeOwnershipJournal(cityPath)
		if err != nil || !exists {
			return err
		}
		entry, ok := journal.Scopes[key]
		if !ok {
			return nil
		}
		pathKey := "path:" + entry.ScopePath
		if existing, collision := journal.Scopes[pathKey]; collision && !samePath(existing.ScopePath, entry.ScopePath) {
			return fmt.Errorf("scope ownership journal path collision for %q", pathKey)
		}
		delete(journal.Scopes, key)
		journal.Scopes[pathKey] = entry
		return writeProviderScopeOwnershipJournal(cityPath, journal)
	})
}

func providerScopeOwnershipRecordFromJournal(journal providerScopeOwnershipJournal, cityPath, scopeRoot string) (string, providerScopeOwnershipEntry, bool, error) {
	key := providerScopeOwnershipKey(cityPath, scopeRoot)
	var physicalKey string
	for candidate, entry := range journal.Scopes {
		if !samePath(entry.ScopePath, scopeRoot) {
			continue
		}
		if physicalKey != "" {
			return "", providerScopeOwnershipEntry{}, false, fmt.Errorf("duplicate scope ownership paths for %q and %q", physicalKey, candidate)
		}
		physicalKey = candidate
	}
	if entry, ok := journal.Scopes[key]; ok {
		if !samePath(entry.ScopePath, scopeRoot) {
			return "", providerScopeOwnershipEntry{}, false, fmt.Errorf("scope ownership journal path drift for %q", key)
		}
		return key, entry, true, nil
	}
	if physicalKey == "" {
		return "", providerScopeOwnershipEntry{}, false, nil
	}
	return physicalKey, journal.Scopes[physicalKey], true, nil
}

func withProviderScopeOwnershipLock(cityPath string, fn func() error) error {
	path := filepath.Join(normalizePathForCompare(cityPath), ".gc", "scope-ownership.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create scope ownership lock directory: %w", err)
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open scope ownership lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return fmt.Errorf("scope ownership journal is busy")
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()
	return fn()
}

func writeProviderScopeOwnershipJournal(cityPath string, journal providerScopeOwnershipJournal) error {
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode scope ownership journal: %w", err)
	}
	data = append(data, '\n')
	path := providerScopeOwnershipPath(cityPath)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create scope ownership directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, "."+scopeOwnershipFile+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary scope ownership journal: %w", err)
	}
	temporaryPath := temporary.Name()
	temporaryOpen := true
	defer func() {
		if temporaryOpen {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary scope ownership journal mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write temporary scope ownership journal: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary scope ownership journal: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary scope ownership journal: %w", err)
	}
	temporaryOpen = false
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace scope ownership journal: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open scope ownership directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		closeErr := directory.Close()
		return errors.Join(fmt.Errorf("sync scope ownership directory: %w", err), closeErr)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close scope ownership directory after sync: %w", err)
	}
	return nil
}
