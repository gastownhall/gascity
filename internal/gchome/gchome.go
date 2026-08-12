// Package gchome resolves machine-local Gas City state paths.
package gchome

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Provenance identifies the branch that selected a Gas City home. It is
// recorded at resolution time; callers must not infer it from path prefixes.
type Provenance uint8

const (
	// ProvenanceExplicit identifies a non-empty GC_HOME value.
	ProvenanceExplicit Provenance = iota + 1
	// ProvenanceUserHome identifies the .gc directory below os.UserHomeDir.
	ProvenanceUserHome
	// ProvenanceMkdirTemp identifies a process-unique temporary directory.
	ProvenanceMkdirTemp
	// ProvenanceLastResort identifies the PID-stamped fallback used when a
	// temporary directory cannot be created.
	ProvenanceLastResort
)

// String returns the stable diagnostic name for provenance.
func (provenance Provenance) String() string {
	switch provenance {
	case ProvenanceExplicit:
		return "explicit-gc-home"
	case ProvenanceUserHome:
		return "user-home"
	case ProvenanceMkdirTemp:
		return "temporary-fallback"
	case ProvenanceLastResort:
		return "last-resort-fallback"
	default:
		return "unknown"
	}
}

// Stable reports whether provenance names a durable operator- or user-home
// location eligible for product-metrics trust inspection.
func (provenance Provenance) Stable() bool {
	return provenance == ProvenanceExplicit || provenance == ProvenanceUserHome
}

// ResolvedHome is a Gas City home paired with resolution-time provenance.
// Its fields are private so another package cannot relabel a fallback path as
// stable based on its spelling.
type ResolvedHome struct {
	path       string
	provenance Provenance
}

// Path returns the selected Gas City home path.
func (home ResolvedHome) Path() string { return home.path }

// Provenance returns the branch that selected the path.
func (home ResolvedHome) Provenance() Provenance { return home.provenance }

type homeResolverDeps struct {
	getenv      func(string) string
	userHomeDir func() (string, error)
	mkdirTemp   func(string, string) (string, error)
	tempDir     func() string
	pid         func() int
}

func systemHomeResolverDeps() homeResolverDeps {
	return homeResolverDeps{
		getenv:      os.Getenv,
		userHomeDir: os.UserHomeDir,
		mkdirTemp:   os.MkdirTemp,
		tempDir:     os.TempDir,
		pid:         os.Getpid,
	}
}

func resolveHome(deps homeResolverDeps, createTemporaryFallback bool) ResolvedHome {
	if value := strings.TrimSpace(deps.getenv("GC_HOME")); value != "" {
		return ResolvedHome{path: value, provenance: ProvenanceExplicit}
	}
	if userHome, err := deps.userHomeDir(); err == nil && userHome != "" {
		return ResolvedHome{path: filepath.Join(userHome, ".gc"), provenance: ProvenanceUserHome}
	}
	if createTemporaryFallback {
		if temporaryHome, err := deps.mkdirTemp("", "gc-home-*"); err == nil {
			return ResolvedHome{path: temporaryHome, provenance: ProvenanceMkdirTemp}
		}
	}
	return ResolvedHome{
		path:       filepath.Join(deps.tempDir(), fmt.Sprintf("gc-home-%d", deps.pid())),
		provenance: ProvenanceLastResort,
	}
}

// ResolveDefault resolves the legacy default and reports which branch won.
// Like Default, it may create a process-unique temporary fallback.
func ResolveDefault() ResolvedHome {
	return resolveHome(systemHomeResolverDeps(), true)
}

// ResolveReadOnly resolves explicit and user-home paths without creating a
// fallback directory. When neither stable source is available, it returns the
// deterministic last-resort candidate marked unstable.
func ResolveReadOnly() ResolvedHome {
	return resolveHome(systemHomeResolverDeps(), false)
}

// Default returns the Gas City machine-local state directory.
//
// Resolution order: GC_HOME, user home/.gc, process-unique temp fallback.
func Default() string {
	return ResolveDefault().Path()
}

// ProductUsageHome is a read-only trust snapshot for the product-usage root.
// It is not an authorization capability: mutating storage must repeat the
// walk using retained directory descriptors before creating or writing.
type ProductUsageHome struct {
	home          ResolvedHome
	root          string
	needsCreation bool
}

// Home returns the resolved Gas City home used by this snapshot.
func (inspection ProductUsageHome) Home() ResolvedHome { return inspection.home }

// Root returns the lexical product-usage root below Home.
func (inspection ProductUsageHome) Root() string { return inspection.root }

// NeedsCreation reports whether the home or product root was absent when
// inspected. The value is advisory and must be revalidated before mutation.
func (inspection ProductUsageHome) NeedsCreation() bool { return inspection.needsCreation }

// InspectProductUsageHome performs a side-effect-free trust inspection of a
// resolved product-usage home. It never creates, repairs, or resolves a path.
func InspectProductUsageHome(home ResolvedHome) (ProductUsageHome, error) {
	inspection := ProductUsageHome{
		home: home,
		root: filepath.Join(home.Path(), "product-usage"),
	}
	if !home.Provenance().Stable() {
		return inspection, fmt.Errorf("gchome: unstable %s home %q is ineligible for product usage", home.Provenance(), home.Path())
	}
	if !filepath.IsAbs(home.Path()) {
		return inspection, fmt.Errorf("gchome: product usage home %q is not absolute", home.Path())
	}
	if cleaned := filepath.Clean(home.Path()); cleaned != home.Path() {
		return inspection, fmt.Errorf("gchome: product usage home %q is not clean (clean form %q)", home.Path(), cleaned)
	}
	needsCreation, err := inspectTrustedProductUsagePath(home.Path(), inspection.root)
	if err != nil {
		return inspection, err
	}
	inspection.needsCreation = needsCreation
	return inspection, nil
}

// RegistriesPath returns the configured registry file path under home.
func RegistriesPath(home string) string {
	return filepath.Join(home, "registries.toml")
}

// RegistryCacheRoot returns the registry catalog cache directory under home.
func RegistryCacheRoot(home string) string {
	return filepath.Join(home, "registry-cache")
}
