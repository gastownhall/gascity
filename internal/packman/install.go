package packman

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// InstallMode controls whether lock resolution is strict or may refresh.
type InstallMode int

// Install modes define how remote imports interact with the existing lockfile.
const (
	InstallFromLock InstallMode = iota
	InstallResolveIfNeeded
	InstallUpgrade
)

type packConfig struct {
	Imports map[string]config.Import `toml:"imports,omitempty"`
}

// ReadCachedPackImports loads a cached pack's nested imports from pack.toml.
func ReadCachedPackImports(source, commit string) (map[string]config.Import, error) {
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		return nil, err
	}
	if subpath := normalizeRemoteSource(source).Subpath; subpath != "" {
		cachePath = filepath.Join(cachePath, subpath)
	}
	return readPackImports(cachePath)
}

// InstallLocked restores every entry recorded in packs.lock into the shared cache.
func InstallLocked(cityRoot string) (*Lockfile, error) {
	lock, err := ReadLockfile(fsys.OSFS{}, cityRoot)
	if err != nil {
		return nil, err
	}

	sources := make([]string, 0, len(lock.Packs))
	for source := range lock.Packs {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		pack := lock.Packs[source]
		if pack.Commit == "" {
			return nil, fmt.Errorf("lock entry %q is missing commit", source)
		}
		if _, err := EnsureRepoInCache(source, pack.Commit); err != nil {
			return nil, err
		}
	}
	return lock, nil
}

// SyncLock resolves the reachable remote-import closure and returns the updated lock.
func SyncLock(cityRoot string, imports map[string]config.Import, mode InstallMode) (*Lockfile, error) {
	existing, err := ReadLockfile(fsys.OSFS{}, cityRoot)
	if err != nil {
		return nil, err
	}

	state := syncState{
		mode:     mode,
		existing: existing,
		result: &Lockfile{
			Schema: LockfileSchema,
			Packs:  make(map[string]LockedPack),
		},
		seen:        make(map[string]bool),
		constraints: make(map[string]string),
		premerged:   make(map[string]bool),
	}

	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		imp := imports[name]
		if !isRemoteSource(imp.Source) {
			continue
		}
		mergedConstraint, err := mergeConstraints(state.constraints[imp.Source], imp.Version)
		if err != nil {
			return nil, fmt.Errorf("import %q: source %q: %w", name, imp.Source, err)
		}
		state.constraints[imp.Source] = mergedConstraint
		state.premerged[imp.Source] = true
	}
	for _, name := range names {
		if err := state.walk(imports[name], true); err != nil {
			return nil, fmt.Errorf("import %q: %w", name, err)
		}
	}
	return state.result, nil
}

type syncState struct {
	mode        InstallMode
	existing    *Lockfile
	result      *Lockfile
	seen        map[string]bool
	constraints map[string]string
	premerged   map[string]bool
}

func (s *syncState) walk(imp config.Import, direct bool) error {
	if !isRemoteSource(imp.Source) {
		return nil
	}
	mergedConstraint := s.constraints[imp.Source]
	if direct && s.premerged[imp.Source] && !s.seen[imp.Source] {
		s.premerged[imp.Source] = false
	} else {
		var err error
		mergedConstraint, err = mergeConstraints(s.constraints[imp.Source], imp.Version)
		if err != nil {
			return fmt.Errorf("source %q: %w", imp.Source, err)
		}
		s.constraints[imp.Source] = mergedConstraint
	}
	if s.seen[imp.Source] {
		if !matchesExisting(s.result.Packs[imp.Source], mergedConstraint) {
			return fmt.Errorf("source %q has conflicting constraints", imp.Source)
		}
		return nil
	}
	s.seen[imp.Source] = true
	imp.Version = mergedConstraint

	locked, err := s.resolveLockedPack(imp)
	if err != nil {
		return err
	}
	s.result.Packs[imp.Source] = locked

	cachePath, err := EnsureRepoInCache(imp.Source, locked.Commit)
	if err != nil {
		return err
	}
	if subpath := normalizeRemoteSource(imp.Source).Subpath; subpath != "" {
		cachePath = filepath.Join(cachePath, subpath)
	}
	if !imp.ImportIsTransitive() {
		return nil
	}

	nested, err := readPackImports(cachePath)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(nested))
	for name := range nested {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.walk(nested[name], false); err != nil {
			return fmt.Errorf("nested import %q: %w", name, err)
		}
	}
	return nil
}

func (s *syncState) resolveLockedPack(imp config.Import) (LockedPack, error) {
	existing, hasExisting := s.existing.Packs[imp.Source]
	switch s.mode {
	case InstallFromLock:
		if !hasExisting {
			return LockedPack{}, fmt.Errorf("missing lock entry for %q", imp.Source)
		}
		return existing, nil
	case InstallResolveIfNeeded:
		if hasExisting && matchesExisting(existing, imp.Version) {
			return existing, nil
		}
	case InstallUpgrade:
		// Always refresh below.
	default:
		return LockedPack{}, fmt.Errorf("unknown install mode %d", s.mode)
	}

	resolved, err := ResolveVersion(imp.Source, imp.Version)
	if err != nil {
		return LockedPack{}, err
	}
	return LockedPack{
		Version: resolved.Version,
		Commit:  resolved.Commit,
		Fetched: time.Now().UTC(),
	}, nil
}

func matchesExisting(pack LockedPack, constraint string) bool {
	if constraint == "" {
		return true
	}
	if strings.HasPrefix(constraint, "sha:") {
		return pack.Commit == strings.TrimPrefix(constraint, "sha:")
	}
	return matchesConstraint(pack.Version, constraint)
}

func mergeConstraints(existing, next string) (string, error) {
	switch {
	case existing == "":
		return next, nil
	case next == "":
		return existing, nil
	case strings.HasPrefix(existing, "sha:") || strings.HasPrefix(next, "sha:"):
		if existing != next {
			return "", fmt.Errorf("incompatible pinned versions %q and %q", existing, next)
		}
		return existing, nil
	default:
		return existing + "," + next, nil
	}
}

func readPackImports(packDir string) (map[string]config.Import, error) {
	data, err := os.ReadFile(filepath.Join(packDir, "pack.toml"))
	if err != nil {
		return nil, fmt.Errorf("reading pack.toml: %w", err)
	}
	var cfg packConfig
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, fmt.Errorf("parsing pack.toml: %w", err)
	}
	if cfg.Imports == nil {
		cfg.Imports = make(map[string]config.Import)
	}
	return cfg.Imports, nil
}

func isRemoteSource(source string) bool {
	return strings.HasPrefix(source, "git@") ||
		strings.HasPrefix(source, "ssh://") ||
		strings.HasPrefix(source, "https://") ||
		strings.HasPrefix(source, "http://") ||
		strings.HasPrefix(source, "file://") ||
		strings.HasPrefix(source, "github.com/")
}
