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
	packPath := cachePath
	if subpath := normalizeRemoteSource(source).Subpath; subpath != "" {
		packPath = filepath.Join(packPath, subpath)
	}
	root, err := RepoCacheRoot()
	if err != nil {
		return nil, err
	}
	var imports map[string]config.Import
	if err := config.WithRepoCacheReadLock(root, func() error {
		if err := validateCachedRepoCheckout(cachePath, commit); err != nil {
			return err
		}
		var readErr error
		imports, readErr = readPackImports(packPath)
		return readErr
	}); err != nil {
		return nil, err
	}
	return imports, nil
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
	return syncLock(cityRoot, imports, mode, nil)
}

// SyncLockSelectiveUpgrade refreshes only the listed remote sources while
// preserving every other reachable import from the existing lock when possible.
func SyncLockSelectiveUpgrade(cityRoot string, imports map[string]config.Import, upgradeSources map[string]struct{}) (*Lockfile, error) {
	return syncLock(cityRoot, imports, InstallResolveIfNeeded, upgradeSources)
}

func syncLock(cityRoot string, imports map[string]config.Import, mode InstallMode, upgradeSources map[string]struct{}) (*Lockfile, error) {
	existing, err := ReadLockfile(fsys.OSFS{}, cityRoot)
	if err != nil {
		return nil, err
	}

	state := syncState{
		mode:           mode,
		existing:       existing,
		upgradeSources: upgradeSources,
		chosen:         make(map[string]LockedPack),
		refreshed:      make(map[string]bool),
	}

	constraints, reachable, err := mergeDirectConstraints(imports)
	if err != nil {
		return nil, err
	}
	if len(reachable) == 0 {
		return &Lockfile{Schema: LockfileSchema, Packs: make(map[string]LockedPack)}, nil
	}

	for i := 0; ; i++ {
		chosenChanged, err := state.ensureChosen(constraints, reachable)
		if err != nil {
			return nil, err
		}
		nextConstraints, nextReachable, dirty, err := state.discoverReachableClosure(imports)
		if err != nil {
			return nil, err
		}
		if !dirty && !chosenChanged && sameStringMap(constraints, nextConstraints) && sameSet(reachable, nextReachable) {
			return state.buildLock(nextReachable), nil
		}
		constraints = nextConstraints
		reachable = nextReachable
		maxIterations := len(imports) + len(reachable) + len(state.chosen) + len(existing.Packs) + 32
		if i >= maxIterations {
			return nil, fmt.Errorf("import resolution did not converge")
		}
	}
}

type syncState struct {
	mode           InstallMode
	existing       *Lockfile
	upgradeSources map[string]struct{}
	chosen         map[string]LockedPack
	refreshed      map[string]bool
}

func (s *syncState) ensureChosen(constraints map[string]string, reachable map[string]struct{}) (bool, error) {
	names := make([]string, 0, len(reachable))
	for source := range reachable {
		names = append(names, source)
	}
	sort.Strings(names)

	changed := false
	for _, source := range names {
		updated, err := s.resolveSource(source, constraints[source])
		if err != nil {
			return false, err
		}
		if updated {
			changed = true
		}
	}
	return changed, nil
}

func (s *syncState) resolveSource(source, constraint string) (bool, error) {
	forceUpgrade := s.mode == InstallUpgrade
	if !forceUpgrade && s.upgradeSources != nil {
		_, forceUpgrade = s.upgradeSources[source]
	}

	if current, ok := s.chosen[source]; ok && matchesExisting(current, constraint) {
		if !forceUpgrade || s.refreshed[source] {
			return false, nil
		}
	}

	existing, hasExisting := s.existing.Packs[source]
	switch s.mode {
	case InstallFromLock:
		if !hasExisting {
			return false, fmt.Errorf("missing lock entry for %q", source)
		}
		if !matchesExisting(existing, constraint) {
			return false, fmt.Errorf("source %q has conflicting constraints", source)
		}
		return s.storeChosen(source, existing, false), nil
	case InstallUpgrade:
		// Always refresh below unless this sync already resolved the source.
	case InstallResolveIfNeeded:
		if !forceUpgrade && hasExisting && matchesExisting(existing, constraint) {
			return s.storeChosen(source, existing, false), nil
		}
	default:
		return false, fmt.Errorf("unknown install mode %d", s.mode)
	}

	resolved, err := ResolveVersion(source, constraint)
	if err != nil {
		return false, err
	}
	return s.storeChosen(source, LockedPack{
		Version: resolved.Version,
		Commit:  resolved.Commit,
		Fetched: time.Now().UTC(),
	}, true), nil
}

func (s *syncState) discoverReachableClosure(imports map[string]config.Import) (map[string]string, map[string]struct{}, bool, error) {
	constraints := make(map[string]string)
	reachable := make(map[string]struct{})
	seen := make(map[string]bool)
	dirty := false

	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.walkImport(name, imports[name], constraints, reachable, seen, &dirty); err != nil {
			return nil, nil, false, fmt.Errorf("import %q: %w", name, err)
		}
	}
	return constraints, reachable, dirty, nil
}

func (s *syncState) walkImport(_ string, imp config.Import, constraints map[string]string, reachable map[string]struct{}, seen map[string]bool, dirty *bool) error {
	if !isRemoteSource(imp.Source) {
		return nil
	}

	mergedConstraint, err := mergeConstraints(constraints[imp.Source], imp.Version)
	if err != nil {
		return fmt.Errorf("source %q: %w", imp.Source, err)
	}
	constraints[imp.Source] = mergedConstraint
	reachable[imp.Source] = struct{}{}

	chosen, ok := s.chosen[imp.Source]
	if !ok || !matchesExisting(chosen, mergedConstraint) {
		*dirty = true
	}
	if !ok {
		return nil
	}

	if _, err := s.cachedPackPath(imp.Source, chosen.Commit); err != nil {
		return err
	}
	if !imp.ImportIsTransitive() {
		return nil
	}
	if seen[imp.Source] {
		return nil
	}
	seen[imp.Source] = true

	nested, err := ReadCachedPackImports(imp.Source, chosen.Commit)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(nested))
	for name := range nested {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.walkImport(name, nested[name], constraints, reachable, seen, dirty); err != nil {
			return fmt.Errorf("nested import %q: %w", name, err)
		}
	}
	return nil
}

func (s *syncState) cachedPackPath(source, commit string) (string, error) {
	cachePath, err := EnsureRepoInCache(source, commit)
	if err != nil {
		return "", err
	}
	if subpath := normalizeRemoteSource(source).Subpath; subpath != "" {
		cachePath = filepath.Join(cachePath, subpath)
	}
	return cachePath, nil
}

func (s *syncState) storeChosen(source string, pack LockedPack, refreshed bool) bool {
	prev, hadPrev := s.chosen[source]
	prevRefreshed := s.refreshed[source]
	s.chosen[source] = pack
	s.refreshed[source] = refreshed
	if !hadPrev {
		return true
	}
	return prev.Version != pack.Version || prev.Commit != pack.Commit || prevRefreshed != refreshed
}

func (s *syncState) buildLock(reachable map[string]struct{}) *Lockfile {
	lock := &Lockfile{
		Schema: LockfileSchema,
		Packs:  make(map[string]LockedPack, len(reachable)),
	}
	for source := range reachable {
		lock.Packs[source] = s.chosen[source]
	}
	return lock
}

func mergeDirectConstraints(imports map[string]config.Import) (map[string]string, map[string]struct{}, error) {
	constraints := make(map[string]string)
	reachable := make(map[string]struct{})

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
		mergedConstraint, err := mergeConstraints(constraints[imp.Source], imp.Version)
		if err != nil {
			return nil, nil, fmt.Errorf("import %q: source %q: %w", name, imp.Source, err)
		}
		constraints[imp.Source] = mergedConstraint
		reachable[imp.Source] = struct{}{}
	}
	return constraints, reachable, nil
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if _, ok := b[key]; !ok {
			return false
		}
	}
	return true
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
