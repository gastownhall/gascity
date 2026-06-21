package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// cityRefKind classifies an explicit city reference (a positional argument, a
// --city value, or GC_CITY) by shape. A registered city name can never contain
// a path separator (see supervisor.IsValidCityName), so shape alone decides
// whether a reference could be a name.
type cityRefKind int

const (
	cityRefEmpty cityRefKind = iota
	cityRefName
	cityRefPath
)

func classifyCityRef(ref string) cityRefKind {
	s := strings.TrimSpace(ref)
	switch {
	case s == "":
		return cityRefEmpty
	case supervisor.IsValidCityName(s):
		return cityRefName
	default:
		return cityRefPath
	}
}

// cityRefOpts configures resolveCityRef.
type cityRefOpts struct {
	// allowNameFallback enables resolving a bare registered city NAME. Commands
	// that create a registration from a path (gc register) set this false, so a
	// name-shaped argument is always treated as a path.
	allowNameFallback bool
}

// resolveCityRef resolves an explicit, non-empty city reference to a city path,
// accepting either a directory PATH or a registered city NAME. Callers handle
// the no-argument (cwd) case themselves; ref must be non-empty.
//
// pathResolve is the command's existing path resolver. It is invoked ONLY for
// path-shaped references and for name-shaped references that name an actual
// local city directory — NEVER for a bare name with no local city. This is
// deliberate: the path resolvers end in findCity(), which walks UP the
// directory tree, so feeding a bare name to them would silently resolve to an
// ambient ancestor city. A name-shaped reference with no local city is instead
// resolved against the supervisor registry.
//
// Resolution when name fallback is enabled:
//   - path-shaped              -> pathResolve(ref)            (behavior unchanged)
//   - name + local city only   -> pathResolve(name)          (the local city wins)
//   - name + registered only   -> the registered path        (no path resolver)
//   - name + both, same path   -> pathResolve(name)
//   - name + both, diff paths  -> ambiguous: loud error, caller disambiguates
//   - name + neither           -> not found: loud error
func resolveCityRef(ref string, opts cityRefOpts, pathResolve func(string) (string, error)) (string, error) {
	if classifyCityRef(ref) != cityRefName || !opts.allowNameFallback {
		return pathResolve(ref)
	}
	registeredPath, useLocal, err := resolveCityNameRef(strings.TrimSpace(ref))
	if err != nil {
		return "", err
	}
	if useLocal {
		// The local city wins. Route through the command's path resolver so the
		// path branch behaves exactly as if a path were supplied; because
		// cwd/<name> is a real city, findCity returns it without walking up.
		return pathResolve(strings.TrimSpace(ref))
	}
	return registeredPath, nil
}

// cityNameFacts captures what a name-shaped city reference resolves to against
// the cwd and the supervisor registry, before any command-specific path or rig
// resolution. It is the single source of truth for the local-city vs
// registered-name routing decision, shared by every command that accepts a bare
// name so the trimming and fallback semantics cannot drift between call sites.
type cityNameFacts struct {
	name        string // the trimmed reference
	localDir    string // cwd/<name>
	localIsCity bool   // localDir is itself a city (city.toml or .gc/ runtime root)
	registered  bool   // <name> matches a registered city
	regPath     string // the registered city's path (meaningful when registered)
	loadErr     error  // registry read failure, deferred until a genuine miss
}

// lookupCityNameFacts gathers the local-city and registered-name facts for a
// name-shaped reference. It trims the reference once here so callers never pass
// a raw, space-padded arg on to a path resolver. The registry read error is
// returned as a deferred fact (not an immediate failure) because a corrupt
// registry only changes the outcome when the name does not otherwise resolve to
// a local city.
func lookupCityNameFacts(name string) (cityNameFacts, error) {
	trimmed := strings.TrimSpace(name)
	cwd, err := os.Getwd()
	if err != nil {
		return cityNameFacts{}, fmt.Errorf("resolving working directory: %w", err)
	}
	localDir := filepath.Join(cwd, trimmed)
	entry, registered, loadErr := supervisor.NewRegistry(supervisor.RegistryPath()).LookupCityByNameE(trimmed)
	return cityNameFacts{
		name:        trimmed,
		localDir:    localDir,
		localIsCity: citylayout.HasCityConfig(localDir) || citylayout.HasRuntimeRoot(localDir),
		registered:  registered,
		regPath:     entry.Path,
		loadErr:     loadErr,
	}, nil
}

// ambiguous reports whether the name denotes both a distinct local city and a
// registered city, which callers must reject loudly so neither is targeted by
// accident.
func (f cityNameFacts) ambiguous() bool {
	return f.localIsCity && f.registered && !samePath(f.localDir, f.regPath)
}

// notFoundErr is the terminal error for a name that resolved to nothing. It
// surfaces a deferred registry read failure rather than masquerading a corrupt
// registry as a clean "not registered" miss.
func (f cityNameFacts) notFoundErr() error {
	if f.loadErr != nil {
		return fmt.Errorf("looking up registered city %q: %w", f.name, f.loadErr)
	}
	return cityRefNotFoundErr(f.name, f.localDir)
}

// resolveCityNameRef resolves a name-shaped city reference (the caller
// guarantees classifyCityRef(name) == cityRefName) against the cwd and the
// supervisor registry:
//
//   - useLocal == true: cwd/<name> is itself a city; the caller should resolve
//     it as a local path.
//   - registeredPath != "": the name resolves to a registered city.
//   - err != nil: ambiguous (a local city AND a different registration) or not
//     found (neither a local city nor a registered name).
//
// It never feeds the name to a path resolver, so findCity's upward walk can
// never silently resolve a bare name to an ambient ancestor city.
func resolveCityNameRef(name string) (registeredPath string, useLocal bool, err error) {
	f, err := lookupCityNameFacts(name)
	if err != nil {
		return "", false, err
	}
	switch {
	case f.ambiguous():
		return "", false, cityRefAmbiguousErr(f.name, f.localDir, f.regPath)
	case f.localIsCity:
		return "", true, nil
	case f.registered:
		return f.regPath, false, nil
	default:
		return "", false, f.notFoundErr()
	}
}

// resolveCityNameContext resolves a name-shaped positional argument to a full
// city+rig context for commands whose positional contract also accepts a local
// rig directory (status/reload/suspend/resume and stop). Resolution order:
//
//   - ambiguous (local city AND a different registration)    -> loud error
//   - cwd/<name> is itself a city                            -> localPathResolve
//   - cwd/<name> is a real local rig dir AND <name> is a
//     different registered city                              -> loud error
//   - cwd/<name> is a real local rig directory               -> the rig's context
//   - <name> is a registered city                            -> that city
//   - cwd/<name> only matches an ancestor rig scope          -> that rig's context
//   - otherwise                                              -> loud not-found
//
// It never feeds the name to an upward city walk, so a bare name can never
// silently target an ambient ancestor city. The rig-path probe preserves the
// pre-name-resolution behavior where a slashless rig directory such as
// "frontend" (no city.toml, no .gc/) resolved to its owning city. A real local
// rig directory (cwd/<name> on disk) is resolved before the registry name so a
// same-name registered city cannot silently shadow it; when the two resolve to
// different cities the collision is rejected loudly, mirroring the
// local-city-vs-registration guard. Requiring the directory to exist keeps a
// bare registered name typed from inside an unrelated ancestor rig scope on the
// registry branch (and avoids the rig probe entirely on that common path).
func resolveCityNameContext(name string, localPathResolve func(string) (resolvedContext, error)) (resolvedContext, error) {
	f, err := lookupCityNameFacts(name)
	if err != nil {
		return resolvedContext{}, err
	}
	switch {
	case f.ambiguous():
		return resolvedContext{}, cityRefAmbiguousErr(f.name, f.localDir, f.regPath)
	case f.localIsCity:
		return localPathResolve(f.name)
	}
	// Probe cwd/<name> as a rig path when it is a real local directory, or when
	// the name is unregistered (the ancestor-scope parity fallback). When the
	// name is registered and cwd/<name> is not a real dir, skip the probe so the
	// registered city wins without an extra registry scan, exactly as before.
	localRigDir := isExistingDir(f.localDir)
	if localRigDir || !f.registered {
		ctx, isRig, rigErr := resolveRigPathToContext(f.localDir)
		if rigErr != nil {
			return resolvedContext{}, rigErr
		}
		if isRig {
			if localRigDir && f.registered && !samePath(ctx.CityPath, f.regPath) {
				return resolvedContext{}, cityRefRigVsRegisteredErr(f.name, f.localDir, f.regPath)
			}
			return ctx, nil
		}
	}
	if f.registered {
		return resolvedContext{CityPath: f.regPath}, nil
	}
	return resolvedContext{}, f.notFoundErr()
}

func cityRefAmbiguousErr(name, localDir, registeredPath string) error {
	return fmt.Errorf(
		"%q is ambiguous: it is both a local city directory (%s) and a registered city at %s; pass ./%s for the local one, or cd elsewhere to use the registered city",
		name, localDir, registeredPath, name)
}

func cityRefNotFoundErr(name, localDir string) error {
	return fmt.Errorf(
		"%q is not a registered city name, and %s is not a city directory (run 'gc cities' to list registered cities, or pass a directory path to act on an unregistered city)",
		name, localDir)
}

// cityRefRigVsRegisteredErr is the loud rejection when cwd/<name> is a real
// local rig directory whose owning city differs from a same-name registered
// city. Like cityRefAmbiguousErr it refuses to silently pick either target so a
// command such as `gc stop frontend` can never act on the wrong city.
func cityRefRigVsRegisteredErr(name, localRigDir, registeredPath string) error {
	return fmt.Errorf(
		"%q is ambiguous: it is both a local rig directory (%s) and a registered city at %s; pass ./%s for the local rig, or cd elsewhere to use the registered city",
		name, localRigDir, registeredPath, name)
}

// isExistingDir reports whether p exists and is a directory. It distinguishes a
// real local rig directory (cwd/<name> on disk) from a bare registered name
// that only matches an ancestor rig scope lexically.
func isExistingDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// resolveCityFlagValue resolves the --city flag value, accepting either a
// directory path or a registered city name (parallel to the positional
// argument). validateCityPath provides the path branch.
func resolveCityFlagValue(city string) (string, error) {
	return resolveCityRef(city, cityRefOpts{allowNameFallback: true}, validateCityPath)
}
