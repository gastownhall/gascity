package importsvc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

// Typed errors let callers map a failure to a transport-appropriate status
// (HTTP code or CLI exit) without string matching. Each wraps the underlying
// cause via %w so errors.Is and the detail message both survive.
var (
	// ErrInvalidSource means the source argument could not be normalized into a
	// durable import source (bad path, missing pack.toml, embedded git ref, or
	// a version flag on a non-git source). HTTP: 400.
	ErrInvalidSource = errors.New("invalid import source")
	// ErrNameDerive means no binding name was given and none could be derived
	// from the source. Distinct from ErrInvalidSource so the CLI can reproduce
	// its historical bare "use --name" message. HTTP: 400.
	ErrNameDerive = errors.New("could not derive import name; use --name")
	// ErrReservedPrefix means the requested binding name uses the reserved
	// "default-rig:" prefix. Distinct from ErrInvalidSource for the same
	// CLI-message-parity reason. HTTP: 400.
	ErrReservedPrefix = errors.New("import name uses reserved prefix")
	// ErrImportExists means the resolved binding name is already imported in the
	// target scope (or is owned by a city.toml [imports] override). HTTP: 409.
	ErrImportExists = errors.New("import already exists")
	// ErrVersionResolveFailed means version/HEAD resolution for a git-backed
	// source failed. HTTP: 502 (upstream git probe) or 400 depending on caller.
	ErrVersionResolveFailed = errors.New("import version resolution failed")
	// ErrInstallFailed means the lock sync or lockfile write failed. HTTP: 500.
	ErrInstallFailed = errors.New("import install failed")
	// ErrNotFound means RemoveImport was asked to remove a binding that is not
	// present in any scope. HTTP: 404.
	ErrNotFound = errors.New("import not found")
	// ErrScopeLoad means the import scope could not be loaded (missing/invalid
	// city.toml for a rig-scoped edit, unreadable pack.toml, etc.). HTTP: 400.
	ErrScopeLoad = errors.New("import scope load failed")
)

// AddResult reports what AddImport durably wrote so callers can echo the final
// binding without re-reading the manifest.
type AddResult struct {
	// Name is the local binding name written as the [imports.<Name>] key.
	Name string
	// Source is the canonical, durable source string written to the manifest
	// (remote URL as given, or a file:// promotion of a local git worktree).
	Source string
	// Version is the version constraint written to the manifest: a semver
	// constraint, a "sha:<commit>" pin, or "" for plain path imports.
	Version string
	// GitBacked reports whether the resolved source is a git source (and thus
	// has a lock entry); false for plain local path imports.
	GitBacked bool
}

// RemoveResult reports the binding RemoveImport deleted.
type RemoveResult struct {
	// Name is the binding name that was removed.
	Name string
}

// Deps lets a caller inject the network/git-touching seams (the same vars the
// CLI stubs in its command tests) and the target rig scope. The zero value uses
// the package defaults, which call packman directly; this is what the HTTP
// handler wants. Any nil function field falls back to the package default.
type Deps struct {
	// Rig selects a rig scope for the edit. Empty means the root pack.toml
	// [imports] table.
	Rig string

	// SyncLock, WriteLockfile, ResolveVersion, DefaultConstraint, and
	// ResolveHeadCommit mirror the packman seams. Leave nil to use the package
	// defaults.
	SyncLock          func(cityRoot string, imports map[string]config.Import, mode packman.InstallMode) (*packman.Lockfile, error)
	WriteLockfile     func(fs fsys.FS, cityRoot string, lock *packman.Lockfile) error
	ResolveVersion    func(source, constraint string) (packman.ResolvedVersion, error)
	DefaultConstraint func(version string) (string, error)
	ResolveHeadCommit func(source string) (string, error)
}

func (d Deps) syncLock() func(string, map[string]config.Import, packman.InstallMode) (*packman.Lockfile, error) {
	if d.SyncLock != nil {
		return d.SyncLock
	}
	return syncLock
}

func (d Deps) writeLockfile() func(fsys.FS, string, *packman.Lockfile) error {
	if d.WriteLockfile != nil {
		return d.WriteLockfile
	}
	return writeLockfile
}

func (d Deps) resolveVersion() func(string, string) (packman.ResolvedVersion, error) {
	if d.ResolveVersion != nil {
		return d.ResolveVersion
	}
	return resolveVersion
}

func (d Deps) defaultConstraint() func(string) (string, error) {
	if d.DefaultConstraint != nil {
		return d.DefaultConstraint
	}
	return defaultConstraint
}

func (d Deps) resolveHeadCommit() func(string) (string, error) {
	if d.ResolveHeadCommit != nil {
		return d.ResolveHeadCommit
	}
	return resolveHeadCommit
}

func (d Deps) defaultImportVersionForSource(source string) (string, error) {
	resolved, err := d.resolveVersion()(source, "")
	if err == nil {
		return d.defaultConstraint()(resolved.Version)
	}
	if !errors.Is(err, packman.ErrNoSemverTags) {
		return "", err
	}
	commit, err := d.resolveHeadCommit()(source)
	if err != nil {
		return "", err
	}
	return "sha:" + commit, nil
}

// AddImport resolves source once and writes it as a durable [imports.<name>]
// entry plus a matching packs.lock entry for git-backed sources. It performs
// the git fetch (version/HEAD resolution and lock sync) synchronously: callers
// that need SSRF fencing must validate source before calling. The single
// remote git-fetch line lives in defaultHeadCommit (source.go); lock-time
// fetches happen inside packman.SyncLock.
func AddImport(fs fsys.FS, cityPath, source, nameOverride, versionConstraint string) (*AddResult, error) {
	return AddImportWith(fs, cityPath, source, nameOverride, versionConstraint, Deps{})
}

// AddImportWith is AddImport with injectable seams and rig scope.
func AddImportWith(fs fsys.FS, cityPath, source, nameOverride, versionConstraint string, deps Deps) (*AddResult, error) {
	scope, err := loadImportScope(fs, cityPath, deps.Rig)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}

	source, gitBacked, err := normalizeImportAddSource(fs, cityPath, source)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSource, err)
	}

	name := nameOverride
	if name == "" {
		name = deriveImportName(source)
	}
	if name == "" {
		return nil, ErrNameDerive
	}
	if strings.HasPrefix(name, "default-rig:") {
		return nil, fmt.Errorf("import name %q uses reserved prefix \"default-rig:\": %w", name, ErrReservedPrefix)
	}
	if _, exists := scope.imports[name]; exists {
		return nil, fmt.Errorf("%w: import %q already exists", ErrImportExists, name)
	}
	if scope.isRootPackScope() {
		cityOwned, err := cityRootImportExists(fs, cityPath, name)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
		}
		if cityOwned {
			return nil, fmt.Errorf("%w: import %q is defined by city.toml [imports], which overrides pack.toml; edit city.toml instead", ErrImportExists, name)
		}
	}

	version := versionConstraint
	if gitBacked {
		if hasRepositoryRefInSource(source) {
			return nil, fmt.Errorf("%w: embed refs in --version, not in the source URL", ErrInvalidSource)
		}
		if version == "" {
			version, err = deps.defaultImportVersionForSource(source)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrVersionResolveFailed, err)
			}
		}
	} else if version != "" {
		return nil, fmt.Errorf("%w: --version is only valid for git-backed imports", ErrInvalidSource)
	}

	scope.imports[name] = config.Import{
		Source:  source,
		Version: version,
	}
	allImports, err := CollectAllImports(fs, cityPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	allImports[scope.syntheticKey(name)] = scope.imports[name]
	lock, err := deps.syncLock()(cityPath, allImports, packman.InstallResolveIfNeeded)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	if err := scope.save(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	if err := deps.writeLockfile()(fs, cityPath, lock); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	return &AddResult{
		Name:      name,
		Source:    source,
		Version:   version,
		GitBacked: gitBacked,
	}, nil
}

// RemoveImport deletes the binding name from its owning scope (rig, root pack,
// city.toml root override, or root default-rig) and rewrites packs.lock to the
// remaining graph. It returns ErrNotFound when no scope owns name.
func RemoveImport(fs fsys.FS, cityPath, name string) (*RemoveResult, error) {
	return RemoveImportWith(fs, cityPath, name, Deps{})
}

// RemoveImportWith is RemoveImport with injectable seams and rig scope.
func RemoveImportWith(fs fsys.FS, cityPath, name string, deps Deps) (*RemoveResult, error) {
	scope, err := loadImportScope(fs, cityPath, deps.Rig)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}
	if _, exists := scope.imports[name]; !exists {
		removed, err := removeCityRootImport(fs, cityPath, scope, name)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
		}
		if !removed {
			removed, err = removeRootDefaultRigImport(fs, cityPath, scope, name)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
			}
		}
		if !removed {
			return nil, fmt.Errorf("%w: import %q not found", ErrNotFound, name)
		}
	} else {
		if scope.isRootPackScope() {
			cityOwned, err := cityRootImportExists(fs, cityPath, name)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
			}
			if cityOwned {
				return nil, fmt.Errorf("%w: import %q is overridden by city.toml [imports]; remove the city.toml entry first", ErrImportExists, name)
			}
		}
		delete(scope.imports, name)
	}

	allImports, err := CollectAllImports(fs, cityPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	delete(allImports, scope.syntheticKey(name))
	delete(allImports, "default-rig:"+strings.TrimPrefix(name, "default-rig:"))
	lock, err := deps.syncLock()(cityPath, allImports, packman.InstallResolveIfNeeded)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	if err := scope.save(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	if err := deps.writeLockfile()(fs, cityPath, lock); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	return &RemoveResult{Name: name}, nil
}

// ListImports returns the direct, removable import bindings in the scope that
// AddImport/RemoveImport operate on (the root pack.toml [imports] table by
// default). This is the namespace a list endpoint must surface so that what is
// listed is exactly what can be added and removed — NOT the transitive
// CollectAllImports closure, which includes synthetic keys and resolved
// dependencies that are not individually removable.
func ListImports(fs fsys.FS, cityPath string) (map[string]config.Import, error) {
	return ListImportsWith(fs, cityPath, Deps{})
}

// ListImportsWith is ListImports with an injectable rig scope.
func ListImportsWith(fs fsys.FS, cityPath string, deps Deps) (map[string]config.Import, error) {
	scope, err := loadImportScope(fs, cityPath, deps.Rig)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}
	return copyImports(scope.imports), nil
}

// removeCityRootImport removes a root import owned by city.toml [imports].
// City-only root imports are visible in list/why output, so remove must be able
// to delete them; they live in city.toml, so the save is redirected there.
func removeCityRootImport(fs fsys.FS, cityPath string, scope *importScopeState, name string) (bool, error) {
	if !scope.isRootPackScope() {
		return false, nil
	}
	if _, err := fs.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	cfg, err := loadCityImportManifest(fs, cityPath)
	if err != nil {
		return false, err
	}
	if _, ok := cfg.Imports[name]; !ok {
		return false, nil
	}
	delete(cfg.Imports, name)
	scope.save = func() error {
		return writeCityImportManifest(fs, cityPath, cfg)
	}
	return true, nil
}

func removeRootDefaultRigImport(fs fsys.FS, cityPath string, scope *importScopeState, name string) (bool, error) {
	if !scope.isRootPackScope() {
		return false, nil
	}
	defaultName := strings.TrimPrefix(name, "default-rig:")
	cfg, err := loadCityImportManifest(fs, cityPath)
	if err != nil {
		return false, err
	}
	if _, ok := cfg.Defaults.Rig.Imports[defaultName]; !ok {
		manifest, err := loadCityPackManifest(fs, cityPath)
		if err != nil {
			return false, err
		}
		if _, ok := manifest.Defaults.Rig.Imports[defaultName]; !ok {
			return false, nil
		}
		delete(manifest.Defaults.Rig.Imports, defaultName)
		scope.save = func() error {
			return writeCityPackManifest(fs, cityPath, manifest)
		}
		return true, nil
	}
	delete(cfg.Defaults.Rig.Imports, defaultName)
	scope.save = func() error {
		return writeCityImportManifest(fs, cityPath, cfg)
	}
	return true, nil
}
