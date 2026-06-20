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
	// cmd is the command label used in diagnostics, e.g. "gc unregister".
	cmd string
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
	cwd, werr := os.Getwd()
	if werr != nil {
		return "", false, fmt.Errorf("resolving working directory: %w", werr)
	}
	localDir := filepath.Join(cwd, name)
	localIsCity := citylayout.HasCityConfig(localDir)

	entry, registered := supervisor.NewRegistry(supervisor.RegistryPath()).LookupCityByName(name)

	switch {
	case localIsCity && registered && !samePath(localDir, entry.Path):
		return "", false, cityRefAmbiguousErr(name, localDir, entry.Path)
	case localIsCity:
		return "", true, nil
	case registered:
		return entry.Path, false, nil
	default:
		return "", false, cityRefNotFoundErr(name, localDir)
	}
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

// resolveCityFlagValue resolves the --city flag value, accepting either a
// directory path or a registered city name (parallel to the positional
// argument). validateCityPath provides the path branch.
func resolveCityFlagValue(city string) (string, error) {
	return resolveCityRef(city, cityRefOpts{cmd: "gc", allowNameFallback: true}, validateCityPath)
}
