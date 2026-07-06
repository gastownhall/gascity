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

	// SourcePolicy fences every remote import source before it is probed or
	// fetched — the caller-supplied source and every transitive import packman
	// resolves during lock sync. The HTTP handler injects its SSRF fence here so
	// an accepted public pack cannot pull an internal, file, or link-local nested
	// import past the API. Leave nil (the trusted CLI/local path) to allow every
	// source.
	SourcePolicy func(source string) error

	// SyncLock, WriteLockfile, ResolveVersion, DefaultConstraint, and
	// ResolveHeadCommit mirror the packman seams. Leave nil to use the package
	// defaults.
	SyncLock          func(cityRoot string, imports map[string]config.Import, mode packman.InstallMode) (*packman.Lockfile, error)
	WriteLockfile     func(fs fsys.FS, cityRoot string, lock *packman.Lockfile) error
	ResolveVersion    func(cityRoot, source, constraint string) (packman.ResolvedVersion, error)
	DefaultConstraint func(version string) (string, error)
	ResolveHeadCommit func(cityRoot, source string) (string, error)
}

func (d Deps) syncLock() func(string, map[string]config.Import, packman.InstallMode) (*packman.Lockfile, error) {
	if d.SyncLock != nil {
		return d.SyncLock
	}
	if d.SourcePolicy == nil {
		return syncLock
	}
	// Route the default sync through the policy-aware packman entry point so the
	// fence reaches transitive imports discovered inside lock resolution, which
	// the caller can't see to pre-check.
	return func(cityRoot string, imports map[string]config.Import, mode packman.InstallMode) (*packman.Lockfile, error) {
		return packman.SyncLockWithPolicy(cityRoot, imports, mode, d.SourcePolicy)
	}
}

func (d Deps) writeLockfile() func(fsys.FS, string, *packman.Lockfile) error {
	if d.WriteLockfile != nil {
		return d.WriteLockfile
	}
	return writeLockfile
}

func (d Deps) resolveVersion() func(string, string, string) (packman.ResolvedVersion, error) {
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

func (d Deps) resolveHeadCommit() func(string, string) (string, error) {
	if d.ResolveHeadCommit != nil {
		return d.ResolveHeadCommit
	}
	return resolveHeadCommit
}

func (d Deps) defaultImportVersionForSource(cityRoot, source string) (string, error) {
	resolved, err := d.resolveVersion()(cityRoot, source, "")
	if err == nil {
		return d.defaultConstraint()(resolved.Version)
	}
	if !errors.Is(err, packman.ErrNoSemverTags) {
		return "", err
	}
	commit, err := d.resolveHeadCommit()(cityRoot, source)
	if err != nil {
		return "", err
	}
	return "sha:" + commit, nil
}

// fenceSource applies the injected untrusted-source policy to source. It is the
// service-boundary SSRF fence: the HTTP handler injects its host/file policy so
// AddImportWith never drives a git probe at an internal target, even for a
// caller that skips the handler's own pre-check. A nil policy allows everything.
func (d Deps) fenceSource(source string) error {
	if d.SourcePolicy == nil {
		return nil
	}
	return d.SourcePolicy(source)
}

// resolveImportVersion validates and resolves the version constraint for an add.
// Git-backed sources reject a ref embedded in the URL and, when no constraint is
// given, default to the resolved semver/HEAD; non-git path sources reject a
// constraint outright. The returned error already carries the transport-mapped
// sentinel (ErrInvalidSource or ErrVersionResolveFailed).
func (d Deps) resolveImportVersion(cityRoot, source, versionConstraint string, gitBacked bool) (string, error) {
	if !gitBacked {
		if versionConstraint != "" {
			return "", fmt.Errorf("%w: --version is only valid for git-backed imports", ErrInvalidSource)
		}
		return "", nil
	}
	if hasRepositoryRefInSource(source) {
		return "", fmt.Errorf("%w: embed refs in --version, not in the source URL", ErrInvalidSource)
	}
	if versionConstraint != "" {
		return versionConstraint, nil
	}
	version, err := d.defaultImportVersionForSource(cityRoot, source)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrVersionResolveFailed, err)
	}
	return version, nil
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

	// Fence the resolved source before the HEAD probe below shells `git
	// ls-remote`. The HTTP handler pre-checks the caller-supplied source too, but
	// applying the policy here keeps AddImportWith self-fencing so no future
	// caller can drive the direct git probe at an internal target.
	if err := deps.fenceSource(source); err != nil {
		return nil, err
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

	version, err := deps.resolveImportVersion(cityPath, source, versionConstraint, gitBacked)
	if err != nil {
		return nil, err
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
// remaining graph. When a root name is defined by both pack.toml and a city.toml
// [imports] override, the city override owns the effective (listed) binding, so
// remove peels the city override first and leaves the pack.toml entry declared;
// a second remove then deletes it. It returns ErrNotFound when no scope owns
// name.
func RemoveImport(fs fsys.FS, cityPath, name string) (*RemoveResult, error) {
	return RemoveImportWith(fs, cityPath, name, Deps{})
}

// RemoveImportWith is RemoveImport with injectable seams and rig scope.
func RemoveImportWith(fs fsys.FS, cityPath, name string, deps Deps) (*RemoveResult, error) {
	scope, err := loadImportScope(fs, cityPath, deps.Rig)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}

	target, err := resolveRemoval(fs, cityPath, name, scope)
	if err != nil {
		return nil, err
	}

	allImports, err := CollectAllImports(fs, cityPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	if target.packRemnant != nil {
		// The pack.toml binding survives the peel and is effective again;
		// CollectAllImports still layered the not-yet-saved city override on top,
		// so re-point the merged graph to the pack binding.
		allImports[scope.syntheticKey(name)] = *target.packRemnant
	} else {
		// Drop exactly the removed binding's graph key. Keying off removedKey (not
		// an unconditional "default-rig:"+name delete) stops a bare root-import
		// removal from silently dropping a same-named default-rig binding.
		delete(allImports, target.removedKey)
	}
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

// removalTarget records what RemoveImportWith resolved to delete. Exactly one
// field is set: removedKey is the CollectAllImports synthetic key to drop (it
// depends on which scope owned the removal, not on the requested name — a bare
// name can resolve to a "default-rig:<name>" binding); packRemnant is set
// instead when a city.toml root override is peeled off a same-named pack.toml
// import, so the surviving pack binding is re-pointed rather than dropped.
type removalTarget struct {
	removedKey  string
	packRemnant *config.Import
}

// resolveRemoval finds the scope that owns name, mutates that scope's in-memory
// state and save closure, and reports the graph effect. It isolates the
// scope-precedence branching (rig / root-pack / city-override / default-rig)
// from RemoveImportWith's collect→sync→save→write flow.
func resolveRemoval(fs fsys.FS, cityPath, name string, scope *importScopeState) (removalTarget, error) {
	if _, exists := scope.imports[name]; exists {
		return resolveScopedRemoval(fs, cityPath, name, scope)
	}
	return resolveFallbackRemoval(fs, cityPath, name, scope)
}

// resolveScopedRemoval handles a name that lives directly in the loaded scope: a
// rig scope, a plain root-pack binding, or a root-pack binding shadowed by a
// city.toml [imports] override that must be peeled first.
func resolveScopedRemoval(fs fsys.FS, cityPath, name string, scope *importScopeState) (removalTarget, error) {
	if !scope.isRootPackScope() {
		delete(scope.imports, name)
		return removalTarget{removedKey: scope.syntheticKey(name)}, nil
	}
	cityOwned, err := cityRootImportExists(fs, cityPath, name)
	if err != nil {
		return removalTarget{}, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}
	if !cityOwned {
		delete(scope.imports, name)
		return removalTarget{removedKey: scope.syntheticKey(name)}, nil
	}
	// city.toml [imports] owns the effective binding ListImports surfaces, so
	// removing the listed name peels the city override (removeCityRootImport
	// redirects the save to city.toml) and leaves the shadowed pack.toml entry
	// declared. A follow-up remove of the same name then deletes the pack entry.
	// Without this, GET listed a binding DELETE could never remove.
	removed, err := removeCityRootImport(fs, cityPath, scope, name)
	if err != nil {
		return removalTarget{}, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}
	if !removed {
		return removalTarget{}, fmt.Errorf("%w: import %q not found", ErrNotFound, name)
	}
	remnant := scope.imports[name]
	return removalTarget{packRemnant: &remnant}, nil
}

// resolveFallbackRemoval handles a name absent from the loaded scope by trying
// the city.toml root [imports] overrides first, then the root default-rig
// imports, returning ErrNotFound when no scope owns it.
func resolveFallbackRemoval(fs fsys.FS, cityPath, name string, scope *importScopeState) (removalTarget, error) {
	removed, err := removeCityRootImport(fs, cityPath, scope, name)
	if err != nil {
		return removalTarget{}, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}
	if removed {
		// A city-only root override lives under the root pack synthetic key.
		return removalTarget{removedKey: scope.syntheticKey(name)}, nil
	}
	removed, err = removeRootDefaultRigImport(fs, cityPath, scope, name)
	if err != nil {
		return removalTarget{}, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}
	if !removed {
		return removalTarget{}, fmt.Errorf("%w: import %q not found", ErrNotFound, name)
	}
	// The default-rig binding is keyed by its bare name in the graph, whether the
	// caller passed "default-rig:<name>" or just "<name>".
	return removalTarget{removedKey: "default-rig:" + strings.TrimPrefix(name, "default-rig:")}, nil
}

// ListImports returns the direct, removable import bindings a client can list,
// add, and remove over one namespace. For the root scope that is the root
// pack.toml [imports] table, the city.toml root [imports] overrides layered on
// top, and root default-rig imports surfaced as "default-rig:<binding>" — the
// exact names DELETE accepts. It is deliberately NOT the transitive
// CollectAllImports closure, whose synthetic keys and resolved dependencies are
// not individually removable.
func ListImports(fs fsys.FS, cityPath string) (map[string]config.Import, error) {
	return ListImportsWith(fs, cityPath, Deps{})
}

// ListImportsWith is ListImports with an injectable rig scope. For the root
// pack scope it returns the full inspectable namespace that AddImport and
// RemoveImport treat as in scope: the root pack.toml [imports] table, the
// city.toml root [imports] overrides layered on top (city entries own the
// effective root import, so AddImport rejects and RemoveImport redirects to
// them), and root default-rig imports keyed "default-rig:<binding>" (removable
// by that name). This mirrors the CLI's collectInspectableImports so GET,
// POST, and DELETE all agree on one namespace. For a rig scope it returns that
// rig's [rigs.imports] table.
func ListImportsWith(fs fsys.FS, cityPath string, deps Deps) (map[string]config.Import, error) {
	scope, err := loadImportScope(fs, cityPath, deps.Rig)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}
	imports := copyImports(scope.imports)
	if !scope.isRootPackScope() {
		return imports, nil
	}
	if err := applyCityRootImportOverrides(fs, cityPath, imports); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}
	defaults, err := config.LoadRootPackDefaultRigImports(fs, cityPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScopeLoad, err)
	}
	for _, bound := range defaults {
		key := "default-rig:" + bound.Binding
		if _, exists := imports[key]; exists {
			return nil, fmt.Errorf("%w: import %q conflicts with reserved default-rig inspection key", ErrScopeLoad, key)
		}
		imports[key] = bound.Import
	}
	return imports, nil
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
