package packman

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/packregistry"
	"github.com/gastownhall/gascity/internal/packsource"
)

// ErrNoSemverTags reports that a source has no semver tags to resolve.
var ErrNoSemverTags = errors.New("no semver tags found")

// ResolvedVersion is the concrete source resolution for a version query.
type ResolvedVersion struct {
	Version         string
	Commit          string
	Source          string
	SourceKind      string
	Ref             string
	Hash            string
	Registry        string
	RegistrySource  string
	Pack            string
	Withdrawn       bool
	WithdrawnReason string
}

type ResolveOptions struct {
	GCHome   string
	Existing *LockedPack
}

// ResolveVersion discovers tags for source and selects the highest tag matching constraint.
// Empty constraint means "latest stable semver tag". "sha:<hex>" bypasses tag discovery.
func ResolveVersion(source, constraint string) (ResolvedVersion, error) {
	return ResolveVersionWithOptions(source, constraint, ResolveOptions{})
}

func ResolveVersionWithOptions(source, constraint string, opts ResolveOptions) (ResolvedVersion, error) {
	if strings.HasPrefix(source, "registry:") {
		return resolveRegistryVersion(source, constraint, opts)
	}
	if strings.HasPrefix(constraint, "sha:") {
		commit := strings.TrimPrefix(constraint, "sha:")
		if commit == "" {
			return ResolvedVersion{}, fmt.Errorf("empty sha constraint")
		}
		return ResolvedVersion{Version: constraint, Commit: commit, Source: source, SourceKind: "git"}, nil
	}

	tags, err := listRemoteTags(source)
	if err != nil {
		return ResolvedVersion{}, err
	}
	if len(tags) == 0 {
		return ResolvedVersion{}, fmt.Errorf("%w for %q", ErrNoSemverTags, source)
	}

	versions := make([]string, 0, len(tags))
	for version := range tags {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool {
		return compareSemver(mustParseSemver(versions[i]), mustParseSemver(versions[j])) > 0
	})

	for _, version := range versions {
		if constraint == "" || matchesConstraint(version, constraint) {
			return ResolvedVersion{
				Version:    version,
				Commit:     tags[version],
				Source:     source,
				SourceKind: "git",
			}, nil
		}
	}
	return ResolvedVersion{}, fmt.Errorf("no tags for %q match constraint %q", source, constraint)
}

// DefaultConstraint returns the default caret constraint for a selected version.
func DefaultConstraint(version string) (string, error) {
	v, err := parseSemver(version)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("^%d.%d", v.Major, v.Minor), nil
}

func resolveRegistryVersion(source, constraint string, opts ResolveOptions) (ResolvedVersion, error) {
	loc, err := packsource.ParseRegistryLocator(source)
	if err != nil {
		return ResolvedVersion{}, err
	}
	home := opts.GCHome
	if home == "" {
		home = gchome.Default()
	}
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		return ResolvedVersion{}, err
	}
	reg, ok := findRegistry(cfg, loc.Registry)
	if !ok {
		return ResolvedVersion{}, fmt.Errorf("registry %q is not configured", loc.Registry)
	}
	catalog, _, err := packregistry.ReadCachedCatalog(home, loc.Registry)
	if err != nil {
		return ResolvedVersion{}, fmt.Errorf("reading cached catalog for registry %q: %w; run \"gc pack registry refresh %s\"", loc.Registry, err, loc.Registry)
	}
	pack, ok := findCatalogPack(catalog, loc.Pack)
	if !ok {
		return ResolvedVersion{}, fmt.Errorf("registry %q has no pack %q", loc.Registry, loc.Pack)
	}
	release, err := selectRegistryRelease(pack, constraint, opts.Existing)
	if err != nil {
		return ResolvedVersion{}, fmt.Errorf("registry %q pack %q: %w", loc.Registry, loc.Pack, err)
	}
	return ResolvedVersion{
		Version:         release.Version,
		Commit:          release.Commit,
		Source:          pack.Source,
		SourceKind:      pack.SourceKind,
		Ref:             release.Ref,
		Hash:            release.Hash,
		Registry:        loc.Registry,
		RegistrySource:  reg.Source,
		Pack:            loc.Pack,
		Withdrawn:       release.Withdrawn,
		WithdrawnReason: release.WithdrawnReason,
	}, nil
}

func findRegistry(cfg packregistry.Config, name string) (packregistry.Registry, bool) {
	for _, reg := range cfg.Registries {
		if reg.Name == name {
			return reg, true
		}
	}
	return packregistry.Registry{}, false
}

func findCatalogPack(catalog packregistry.Catalog, name string) (packregistry.CatalogPack, bool) {
	for _, pack := range catalog.Packs {
		if pack.Name == name {
			return pack, true
		}
	}
	return packregistry.CatalogPack{}, false
}

func selectRegistryRelease(pack packregistry.CatalogPack, constraint string, existing *LockedPack) (packregistry.CatalogRelease, error) {
	if strings.HasPrefix(constraint, "sha:") {
		commit := strings.TrimPrefix(constraint, "sha:")
		if !strictCommitSHA(commit) {
			return packregistry.CatalogRelease{}, fmt.Errorf("sha constraint must be sha:<40 lowercase hex>")
		}
		for _, release := range pack.Releases {
			if release.Commit == commit {
				return release, nil
			}
		}
		return packregistry.CatalogRelease{}, fmt.Errorf("no release has commit %s", commit)
	}
	if existing != nil && existing.Commit != "" {
		for _, release := range pack.Releases {
			if release.Version == existing.Version && release.Commit == existing.Commit && release.Hash == existing.Hash && release.Ref == existing.Ref {
				if constraint == "" || matchesConstraint(release.Version, constraint) {
					return release, nil
				}
			}
		}
	}
	candidates := make([]packregistry.CatalogRelease, 0, len(pack.Releases))
	for _, release := range pack.Releases {
		if release.Withdrawn {
			continue
		}
		if constraint == "" || matchesConstraint(release.Version, constraint) {
			candidates = append(candidates, release)
		}
	}
	if len(candidates) == 0 {
		return packregistry.CatalogRelease{}, fmt.Errorf("no non-withdrawn releases match constraint %q", constraint)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return compareSemver(mustParseSemver(candidates[i].Version), mustParseSemver(candidates[j].Version)) > 0
	})
	return candidates[0], nil
}

func strictCommitSHA(commit string) bool {
	if len(commit) != 40 {
		return false
	}
	for _, r := range commit {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func listRemoteTags(source string) (map[string]string, error) {
	out, err := runGit("", "ls-remote", "--tags", normalizeRemoteSource(source).CloneURL)
	if err != nil {
		return nil, fmt.Errorf("listing tags for %q: %w", source, err)
	}

	tags := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		commit := fields[0]
		ref := fields[1]
		if !strings.HasPrefix(ref, "refs/tags/") {
			continue
		}
		tag := strings.TrimPrefix(ref, "refs/tags/")
		tag = strings.TrimSuffix(tag, "^{}")
		version, ok := normalizeTagVersion(tag)
		if !ok {
			continue
		}
		tags[version] = commit
	}
	return tags, nil
}

func normalizeTagVersion(tag string) (string, bool) {
	tag = strings.TrimPrefix(tag, "v")
	if _, err := parseSemver(tag); err != nil {
		return "", false
	}
	return tag, true
}

type semver struct {
	Major int
	Minor int
	Patch int
}

func parseSemver(version string) (semver, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return semver{}, fmt.Errorf("invalid semver %q", version)
	}
	parse := func(s string) (int, error) {
		if s == "" {
			return 0, fmt.Errorf("empty version component")
		}
		return strconv.Atoi(s)
	}
	major, err := parse(parts[0])
	if err != nil {
		return semver{}, fmt.Errorf("invalid semver %q", version)
	}
	minor, err := parse(parts[1])
	if err != nil {
		return semver{}, fmt.Errorf("invalid semver %q", version)
	}
	patch := 0
	if len(parts) == 3 {
		patch, err = parse(parts[2])
		if err != nil {
			return semver{}, fmt.Errorf("invalid semver %q", version)
		}
	}
	return semver{Major: major, Minor: minor, Patch: patch}, nil
}

func mustParseSemver(version string) semver {
	v, err := parseSemver(version)
	if err != nil {
		panic(err)
	}
	return v
}

func compareSemver(a, b semver) int {
	switch {
	case a.Major != b.Major:
		return cmpInt(a.Major, b.Major)
	case a.Minor != b.Minor:
		return cmpInt(a.Minor, b.Minor)
	default:
		return cmpInt(a.Patch, b.Patch)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func matchesConstraint(version, constraint string) bool {
	v, err := parseSemver(version)
	if err != nil {
		return false
	}
	for _, raw := range strings.Split(constraint, ",") {
		part := strings.TrimSpace(raw)
		if part == "" {
			continue
		}
		if !matchesOne(v, part) {
			return false
		}
	}
	return true
}

func matchesOne(version semver, constraint string) bool {
	switch {
	case strings.HasPrefix(constraint, "^"):
		base, err := parseSemver(strings.TrimPrefix(constraint, "^"))
		if err != nil {
			return false
		}
		if compareSemver(version, base) < 0 {
			return false
		}
		upper := semver{Major: base.Major + 1}
		if base.Major == 0 {
			upper = semver{Major: 0, Minor: base.Minor + 1}
		}
		return compareSemver(version, upper) < 0
	case strings.HasPrefix(constraint, "~"):
		base, err := parseSemver(strings.TrimPrefix(constraint, "~"))
		if err != nil {
			return false
		}
		if compareSemver(version, base) < 0 {
			return false
		}
		upper := semver{Major: base.Major, Minor: base.Minor + 1}
		return compareSemver(version, upper) < 0
	case strings.HasPrefix(constraint, ">="):
		base, err := parseSemver(strings.TrimPrefix(constraint, ">="))
		return err == nil && compareSemver(version, base) >= 0
	case strings.HasPrefix(constraint, "<="):
		base, err := parseSemver(strings.TrimPrefix(constraint, "<="))
		return err == nil && compareSemver(version, base) <= 0
	case strings.HasPrefix(constraint, ">"):
		base, err := parseSemver(strings.TrimPrefix(constraint, ">"))
		return err == nil && compareSemver(version, base) > 0
	case strings.HasPrefix(constraint, "<"):
		base, err := parseSemver(strings.TrimPrefix(constraint, "<"))
		return err == nil && compareSemver(version, base) < 0
	default:
		base, err := parseSemver(constraint)
		return err == nil && compareSemver(version, base) == 0
	}
}
