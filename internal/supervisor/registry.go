// Package supervisor provides the machine-wide supervisor registry and
// configuration. The registry tracks which cities are managed by the
// supervisor; the config controls the supervisor's own behavior (API
// port, patrol interval, etc.).
package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// validCityName matches names safe for use in URL path segments.
// Must start with alphanumeric and contain only alphanumerics, hyphens,
// underscores, and dots.
var validCityName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// CityEntry is one registered city in the supervisor registry.
type CityEntry struct {
	Path string `toml:"path"`           // absolute path to city root directory
	Name string `toml:"name,omitempty"` // effective city name (workspace.name or basename)
}

// EffectiveName returns the city's effective name.
func (e CityEntry) EffectiveName() string {
	return e.Name
}

// RigEntry is one registered rig in the supervisor registry.
// Rigs are global entities with an optional default city association.
type RigEntry struct {
	Path        string `toml:"path"`                   // absolute path to rig root directory
	Name        string `toml:"name"`                   // globally unique rig name
	DefaultCity string `toml:"default_city,omitempty"` // absolute path to default city (empty = unset)
}

// registryFile is the TOML structure of ~/.gc/cities.toml.
type registryFile struct {
	Cities []CityEntry `toml:"cities"`
	Rigs   []RigEntry  `toml:"rigs,omitempty"`
}

// Registry manages the set of registered cities. Thread-safe.
// Backed by a TOML file at the given path.
type Registry struct {
	mu   sync.RWMutex
	path string
}

// NewRegistry creates a Registry backed by the given file path.
// The file need not exist yet — it will be created on first write.
func NewRegistry(path string) *Registry {
	return &Registry{path: path}
}

func (r *Registry) refuseHostRegistryDuringTests() {
	if !isTestBinary() {
		return
	}
	if home, err := os.UserHomeDir(); err == nil {
		hostRegistry := filepath.Join(home, ".gc")
		if strings.HasPrefix(r.path, hostRegistry+string(filepath.Separator)) || r.path == hostRegistry {
			panic("supervisor.Registry: refusing to write to host registry during tests")
		}
	}
}

// List returns all registered cities. Returns an empty slice (not nil)
// if the file doesn't exist or is empty.
func (r *Registry) List() ([]CityEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.loadLocked()
}

// Register adds a city to the registry. The path is resolved to an
// absolute path. effectiveName is the city's runtime identity
// (workspace.name from city.toml, or directory basename if unset).
// Returns an error if the city is already registered (by path) or if
// a different city with the same effective name is already registered.
// Uses file-level locking for cross-process safety.
func (r *Registry) Register(cityPath, effectiveName string) error {
	r.refuseHostRegistryDuringTests()

	abs, err := resolveAbsPath(cityPath)
	if err != nil {
		return err
	}
	if effectiveName == "" {
		effectiveName = filepath.Base(abs)
	}
	if !validCityName.MatchString(effectiveName) {
		return fmt.Errorf("city name %q contains invalid characters (must match %s)", effectiveName, validCityName.String())
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	unlock, err := r.fileLock()
	if err != nil {
		return err
	}
	defer unlock()

	entries, err := r.loadLocked()
	if err != nil {
		return err
	}

	for i, e := range entries {
		if sameRegistryPath(e.Path, abs) {
			if e.Name == effectiveName {
				return nil // already registered with same name — idempotent
			}
			// Name changed — check for conflicts with other entries, then update.
			for j, other := range entries {
				if j != i && other.EffectiveName() == effectiveName {
					return fmt.Errorf("city name %q already registered at %s (choose a unique registration name, for example with gc register --name)", effectiveName, other.Path)
				}
			}
			entries[i].Name = effectiveName
			return r.saveLocked(entries)
		}
		if e.EffectiveName() == effectiveName {
			return fmt.Errorf("city name %q already registered at %s (choose a unique registration name, for example with gc register --name)", effectiveName, e.Path)
		}
	}

	entries = append(entries, CityEntry{Path: abs, Name: effectiveName})
	return r.saveLocked(entries)
}

// Unregister removes a city from the registry by path. Returns an
// error if the city is not registered. The path is resolved to
// absolute before comparison. Uses file-level locking for cross-process safety.
func (r *Registry) Unregister(cityPath string) error {
	r.refuseHostRegistryDuringTests()

	abs, err := resolveAbsPath(cityPath)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	unlock, err := r.fileLock()
	if err != nil {
		return err
	}
	defer unlock()

	entries, err := r.loadLocked()
	if err != nil {
		return err
	}

	found := false
	filtered := entries[:0]
	for _, e := range entries {
		if sameRegistryPath(e.Path, abs) {
			found = true
			continue
		}
		filtered = append(filtered, e)
	}
	if !found {
		return fmt.Errorf("city at %s is not registered", abs)
	}
	return r.saveLocked(filtered)
}

// loadAllLocked reads the full registry file. Caller must hold at least r.mu.RLock.
func (r *Registry) loadAllLocked() (registryFile, error) {
	data, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return registryFile{}, nil
	}
	if err != nil {
		return registryFile{}, fmt.Errorf("reading registry: %w", err)
	}
	var rf registryFile
	if err := toml.Unmarshal(data, &rf); err != nil {
		return registryFile{}, fmt.Errorf("parsing registry: %w", err)
	}
	return rf, nil
}

// loadLocked reads the city entries from the registry file. Caller must hold at least r.mu.RLock.
func (r *Registry) loadLocked() ([]CityEntry, error) {
	rf, err := r.loadAllLocked()
	if err != nil {
		return nil, err
	}
	return rf.Cities, nil
}

// fileLock acquires an exclusive flock on a sibling .lock file for
// cross-process safety during read-modify-write operations. Returns
// an unlock function. Caller must hold r.mu.Lock.
func (r *Registry) fileLock() (func(), error) {
	lockPath := r.path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening registry lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close() //nolint:errcheck
		return nil, fmt.Errorf("acquiring registry lock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()                                   //nolint:errcheck
	}, nil
}

// saveAllLocked writes the full registry file atomically. Caller must hold r.mu.Lock.
func (r *Registry) saveAllLocked(rf registryFile) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("creating registry dir: %w", err)
	}
	tmp := r.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("creating temp registry file: %w", err)
	}
	if err := toml.NewEncoder(f).Encode(rf); err != nil {
		f.Close()      //nolint:errcheck // best-effort cleanup
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("encoding registry: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()      //nolint:errcheck // best-effort cleanup
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("syncing temp registry file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("closing temp registry file: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("renaming registry file: %w", err)
	}
	return nil
}

// saveLocked writes the city entries, preserving existing rig entries.
// Caller must hold r.mu.Lock and fileLock.
func (r *Registry) saveLocked(entries []CityEntry) error {
	rf, err := r.loadAllLocked()
	if err != nil {
		// If we can't load, start fresh with just cities.
		rf = registryFile{}
	}
	rf.Cities = entries
	return r.saveAllLocked(rf)
}

// ListRigs returns all registered rigs. Returns an empty slice (not nil)
// if the file doesn't exist or contains no rigs.
func (r *Registry) ListRigs() ([]RigEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rf, err := r.loadAllLocked()
	if err != nil {
		return nil, err
	}
	return rf.Rigs, nil
}

// RegisterRig adds or updates a rig in the registry. Names must be globally
// unique — a different path with the same name is rejected. If the rig path
// already exists, the entry is updated. Uses file-level locking for
// cross-process safety.
func (r *Registry) RegisterRig(rigPath, name, defaultCity string) error {
	r.refuseHostRegistryDuringTests()

	abs, err := resolveAbsPath(rigPath)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("rig name must not be empty")
	}
	if !validCityName.MatchString(name) {
		return fmt.Errorf("rig name %q contains invalid characters (must match %s)", name, validCityName.String())
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	unlock, err := r.fileLock()
	if err != nil {
		return err
	}
	defer unlock()

	rf, err := r.loadAllLocked()
	if err != nil {
		return err
	}

	for i, e := range rf.Rigs {
		if sameRegistryPath(e.Path, abs) {
			// Same path — update name and default if needed.
			if e.Name != name {
				// Check new name doesn't conflict.
				for j, other := range rf.Rigs {
					if j != i && other.Name == name {
						return fmt.Errorf("rig name %q already registered at %s", name, other.Path)
					}
				}
			}
			rf.Rigs[i].Name = name
			if defaultCity != "" {
				rf.Rigs[i].DefaultCity = defaultCity
			}
			return r.saveAllLocked(rf)
		}
		if e.Name == name {
			return fmt.Errorf("rig name %q already registered at %s", name, e.Path)
		}
	}

	rf.Rigs = append(rf.Rigs, RigEntry{
		Path:        abs,
		Name:        name,
		DefaultCity: defaultCity,
	})
	return r.saveAllLocked(rf)
}

// UnregisterRig removes a rig from the registry by path. Returns an error
// if the rig is not registered. Uses file-level locking for cross-process safety.
func (r *Registry) UnregisterRig(rigPath string) error {
	r.refuseHostRegistryDuringTests()

	abs, err := resolveAbsPath(rigPath)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	unlock, err := r.fileLock()
	if err != nil {
		return err
	}
	defer unlock()

	rf, err := r.loadAllLocked()
	if err != nil {
		return err
	}

	found := false
	filtered := rf.Rigs[:0]
	for _, e := range rf.Rigs {
		if sameRegistryPath(e.Path, abs) {
			found = true
			continue
		}
		filtered = append(filtered, e)
	}
	if !found {
		return fmt.Errorf("rig at %s is not registered", abs)
	}
	rf.Rigs = filtered
	return r.saveAllLocked(rf)
}

// LookupRigByPath finds a rig whose path is a prefix of the given directory.
// Returns the matching rig entry and true, or a zero entry and false if no
// match. Uses the longest prefix match when multiple rigs match.
func (r *Registry) LookupRigByPath(dir string) (RigEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rf, err := r.loadAllLocked()
	if err != nil {
		return RigEntry{}, false
	}

	abs, err := resolveAbsPath(dir)
	if err != nil {
		return RigEntry{}, false
	}

	var best RigEntry
	bestLen := 0
	for _, e := range rf.Rigs {
		entryPath := pathutil.NormalizePathForCompare(e.Path)
		if pathHasPrefix(abs, entryPath) && len(entryPath) > bestLen {
			e.Path = entryPath
			best = e
			bestLen = len(entryPath)
		}
	}
	return best, bestLen > 0
}

// LookupRigByName finds a rig by its globally unique name.
// Returns the matching rig entry and true, or a zero entry and false.
func (r *Registry) LookupRigByName(name string) (RigEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rf, err := r.loadAllLocked()
	if err != nil {
		return RigEntry{}, false
	}

	for _, e := range rf.Rigs {
		if e.Name == name {
			return e, true
		}
	}
	return RigEntry{}, false
}

// SetRigDefault sets the default city for a rig. The rig must already be
// registered. Uses file-level locking for cross-process safety.
func (r *Registry) SetRigDefault(rigPath, defaultCity string) error {
	r.refuseHostRegistryDuringTests()

	abs, err := resolveAbsPath(rigPath)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	unlock, err := r.fileLock()
	if err != nil {
		return err
	}
	defer unlock()

	rf, err := r.loadAllLocked()
	if err != nil {
		return err
	}

	for i, e := range rf.Rigs {
		if sameRegistryPath(e.Path, abs) {
			rf.Rigs[i].DefaultCity = defaultCity
			return r.saveAllLocked(rf)
		}
	}
	return fmt.Errorf("rig at %s is not registered", abs)
}

type rigState struct {
	name   string
	cities map[string]bool
}

// ReconcileRigs rebuilds the rig index from city configurations. For each
// (rigPath, rigName, cityPath) tuple, ensures a [[rigs]] entry exists.
// Auto-sets default_city when a rig belongs to exactly one city. Clears
// default_city if the referenced city no longer contains the rig. Removes
// rig entries that no longer belong to any city.
func (r *Registry) ReconcileRigs(rigCityMap []RigCityMapping) error {
	r.refuseHostRegistryDuringTests()

	r.mu.Lock()
	defer r.mu.Unlock()

	unlock, err := r.fileLock()
	if err != nil {
		return err
	}
	defer unlock()

	rf, err := r.loadAllLocked()
	if err != nil {
		return err
	}

	// Build desired state: rig path → {name, set of cities}.
	desired := make(map[string]*rigState)
	for _, m := range rigCityMap {
		rigPath, err := resolveAbsPath(m.RigPath)
		if err != nil {
			return err
		}
		cityPath, err := resolveAbsPath(m.CityPath)
		if err != nil {
			return err
		}
		s, ok := desired[rigPath]
		if !ok {
			s = &rigState{name: m.RigName, cities: make(map[string]bool)}
			desired[rigPath] = s
		}
		s.cities[cityPath] = true
	}

	// Update existing entries and track which paths we've seen.
	seen := make(map[string]bool)
	kept := rf.Rigs[:0]
	for _, e := range rf.Rigs {
		desiredPath, s, ok := desiredRigStateForEntry(desired, e.Path)
		if !ok {
			// Rig no longer in any city — drop it.
			continue
		}
		seen[desiredPath] = true
		e.Path = desiredPath
		e.Name = s.name
		// Clear stale default.
		if e.DefaultCity != "" {
			defaultCity := pathutil.NormalizePathForCompare(e.DefaultCity)
			if !s.cities[defaultCity] {
				e.DefaultCity = ""
			} else {
				e.DefaultCity = defaultCity
			}
		}
		// Auto-set default when exactly one city.
		if e.DefaultCity == "" && len(s.cities) == 1 {
			for city := range s.cities {
				e.DefaultCity = city
			}
		}
		kept = append(kept, e)
	}

	// Add new entries.
	for path, s := range desired {
		if seen[path] {
			continue
		}
		entry := RigEntry{Path: path, Name: s.name}
		if len(s.cities) == 1 {
			for city := range s.cities {
				entry.DefaultCity = city
			}
		}
		kept = append(kept, entry)
	}

	rf.Rigs = kept
	return r.saveAllLocked(rf)
}

// RigCityMapping is an input to ReconcileRigs describing one rig's
// membership in one city.
type RigCityMapping struct {
	RigPath  string
	RigName  string
	CityPath string
}

// resolveAbsPath resolves a path to an absolute canonical comparison form.
func resolveAbsPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}
	return pathutil.NormalizePathForCompare(abs), nil
}

func sameRegistryPath(a, b string) bool {
	return pathutil.SamePath(a, b)
}

func desiredRigStateForEntry(desired map[string]*rigState, entryPath string) (string, *rigState, bool) {
	if s, ok := desired[entryPath]; ok {
		return entryPath, s, true
	}
	for path, s := range desired {
		if sameRegistryPath(path, entryPath) {
			return path, s, true
		}
	}
	return "", nil, false
}

// pathHasPrefix reports whether path starts with prefix as a directory
// boundary (not just a string prefix). e.g. /a/bc is not under /a/b.
func pathHasPrefix(path, prefix string) bool {
	path = pathutil.NormalizePathForCompare(path)
	prefix = pathutil.NormalizePathForCompare(prefix)
	if path == prefix {
		return true
	}
	return len(path) > len(prefix) && path[len(prefix)] == '/' &&
		path[:len(prefix)] == prefix
}
