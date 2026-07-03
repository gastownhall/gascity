package packman

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	gitutil "github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/remotesource"
)

// InstallMode controls whether lock resolution is strict or may refresh.
type InstallMode int

// Install modes define how remote imports interact with the existing lockfile.
const (
	InstallFromLock InstallMode = iota
	InstallResolveIfNeeded
	InstallUpgrade
)

// SourcePolicy validates a remote import source before packman resolves or
// fetches it. It is applied to every source in the reachable closure — the
// direct imports AND every transitive import discovered from a cached pack.toml
// — before any `ResolveVersion` (git ls-remote) or `EnsureRepoInCache`
// (clone/checkout) seam runs for that source. A non-nil error aborts the whole
// sync before that source is touched, so an accepted top-level pack cannot
// smuggle an internal, file, or link-local nested import past an API-layer
// fence. A nil policy (the trusted CLI/local path) allows every source.
type SourcePolicy func(source string) error

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
		if config.IsBundledSourceAtCanonicalPin(source, commit) {
			if err := builtinpacks.ValidateSyntheticRepo(cachePath, commit); err != nil {
				gitInfo, gitErr := os.Stat(filepath.Join(cachePath, ".git"))
				if gitutil.MissingCheckoutMarker(gitInfo, gitErr) {
					return fmt.Errorf("synthetic cache is invalid: %w", err)
				}
				if gitErr != nil {
					return fmt.Errorf("checking bundled repo cache %q: %w; synthetic cache is invalid: %w", cachePath, gitErr, err)
				}
				if err := validateCachedRepoCheckout(cachePath, commit); err != nil {
					return err
				}
			}
		} else {
			if err := validateCachedRepoCheckout(cachePath, commit); err != nil {
				return err
			}
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

// EnsureBundledPacksCurrent repairs any bundled pack synthetic caches that were
// written by a different binary version. Each call to EnsureRepoInCache for a
// bundled source validates the cache against the running binary's embedded
// content and re-materializes it when the hashes differ. This prevents the
// config loader's strict content-hash check from failing after a binary upgrade
// where the running controller and the most-recently-installed binary differ.
//
// Callers that need to ensure all packs (including remote git clones) are
// present should use InstallLocked instead.
func EnsureBundledPacksCurrent(cityRoot string) error {
	lock, err := ReadLockfile(fsys.OSFS{}, cityRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No lockfile; nothing to repair.
		}
		return err
	}
	sources := make([]string, 0, len(lock.Packs))
	for source := range lock.Packs {
		if builtinpacks.IsSource(source) {
			sources = append(sources, source)
		}
	}
	sort.Strings(sources)
	for _, source := range sources {
		pack := lock.Packs[source]
		if pack.Commit == "" {
			continue
		}
		if _, err := EnsureRepoInCache(source, pack.Commit); err != nil {
			return err
		}
	}
	return nil
}

// SyncLock resolves the reachable remote-import closure and returns the updated lock.
func SyncLock(cityRoot string, imports map[string]config.Import, mode InstallMode) (*Lockfile, error) {
	return syncLock(cityRoot, imports, mode, nil, nil)
}

// SyncLockWithPolicy is SyncLock with an untrusted-source policy applied to every
// reachable source (direct and transitive) before it is resolved or fetched, so
// an accepted public pack cannot pull an internal or file-backed nested import
// past the caller's fence. A nil policy behaves exactly like SyncLock.
func SyncLockWithPolicy(cityRoot string, imports map[string]config.Import, mode InstallMode, policy SourcePolicy) (*Lockfile, error) {
	return syncLock(cityRoot, imports, mode, nil, policy)
}

// SyncLockSelectiveUpgrade refreshes only the listed remote sources while
// preserving every other reachable import from the existing lock when possible.
func SyncLockSelectiveUpgrade(cityRoot string, imports map[string]config.Import, upgradeSources map[string]struct{}) (*Lockfile, error) {
	return syncLock(cityRoot, imports, InstallResolveIfNeeded, upgradeSources, nil)
}

func syncLock(cityRoot string, imports map[string]config.Import, mode InstallMode, upgradeSources map[string]struct{}, policy SourcePolicy) (*Lockfile, error) {
	existing, err := ReadLockfile(fsys.OSFS{}, cityRoot)
	if err != nil {
		return nil, err
	}

	state := syncState{
		mode:           mode,
		existing:       existing,
		upgradeSources: upgradeSources,
		policy:         policy,
		chosen:         make(map[string]LockedPack),
		refreshed:      make(map[string]bool),
		validated:      make(map[string]bool),
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
	policy         SourcePolicy
	chosen         map[string]LockedPack
	refreshed      map[string]bool
	validated      map[string]bool
}

// checkPolicy runs the untrusted-source policy for source once per sync. It is
// the single gate every source passes before resolveSource resolves it or
// walkImport caches it, so a policy rejection aborts the sync before any git or
// cache seam runs for that source — the transitive-import fence.
func (s *syncState) checkPolicy(source string) error {
	if s.policy == nil || s.validated[source] {
		return nil
	}
	if err := s.policy(source); err != nil {
		return err
	}
	s.validated[source] = true
	return nil
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
	// Fence the source before any resolution or cache fetch. resolveSource is the
	// choke point every reachable source (direct and transitive) flows through
	// before it is chosen, and walkImport only caches already-chosen sources, so
	// gating here blocks both the ResolveVersion and EnsureRepoInCache seams.
	if err := s.checkPolicy(source); err != nil {
		return false, err
	}

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
	return remotesource.IsRemote(source)
}
