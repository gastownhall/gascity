package packman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/packregistry"
)

// ErrRegistryRelease marks a failure to answer a version constraint from a
// pack registry that publishes the source: no release matches, two registries
// disagree on a release, or the fetched content does not match the release's
// content hash. Such a source never falls back to git tags.
var ErrRegistryRelease = errors.New("registry release resolution failed")

// RegistryRelease is the registry catalog release that answers a version
// constraint for a registry-published pack source.
type RegistryRelease struct {
	// Pack names the release's pack for messages, e.g. "main:gascity".
	Pack string
	// Version is the release's semver version.
	Version string
	// Commit is the full commit the release pins.
	Commit string
	// Hash is the release's pack content hash (sha256:<hex>).
	Hash string
}

// String renders the release as "<registry>:<pack> <version>".
func (r RegistryRelease) String() string {
	return r.Pack + " " + r.Version
}

// RegistryFetch selects how a registry lookup may touch the network.
type RegistryFetch int

const (
	// RegistryCachedOnly reads cached catalogs and never fetches.
	RegistryCachedOnly RegistryFetch = iota
	// RegistryRefreshMissing fetches only registries with no cached catalog.
	RegistryRefreshMissing
	// RegistryRefreshAll refreshes every registry first (upgrade), falling
	// back to a cached catalog when a refresh fails.
	RegistryRefreshAll
)

// RegistryLookup finds the registry catalog packs that publish a source.
type RegistryLookup func(source string, fetch RegistryFetch) (packregistry.PackLookup, error)

// lookupRegistryPacks is the registry seam every sync and add resolves
// through. Tests replace it.
var lookupRegistryPacks RegistryLookup = defaultLookupRegistryPacks

// defaultLookupRegistryPacks reads the registries configured under the same
// Gas City home that holds the shared repo cache. With no home (hermetic unit
// tests that did not opt into GC_HOME) no registry is consulted.
func defaultLookupRegistryPacks(source string, fetch RegistryFetch) (packregistry.PackLookup, error) {
	home := config.ImplicitGCHome()
	if home == "" {
		return packregistry.PackLookup{}, nil
	}
	switch fetch {
	case RegistryRefreshAll:
		return packregistry.LookupRefreshedPacksBySource(context.Background(), home, source)
	case RegistryRefreshMissing:
		return packregistry.LookupPacksBySource(context.Background(), home, source)
	default:
		return packregistry.LookupCachedPacksBySource(home, source)
	}
}

// ResolveRegistryRelease answers constraint for source from the configured
// pack registries. Registry packs are versioned by their catalog release
// entries, not by git tags: a pack repository can hold many packs and tag none
// of them, or carry repository-level tags that belong to no pack, so the
// release entry is the only authority on which commit a pack version names.
// An empty constraint selects the newest release; "sha:" constraints are not
// registry questions and report ok=false.
//
// ok=false means no readable registry publishes source; unavailable then
// names registries that could not be read, so a later git-tag failure can say
// the source may be a registry pack. Errors wrap ErrRegistryRelease.
func ResolveRegistryRelease(source, constraint string) (release RegistryRelease, ok bool, unavailable error, err error) {
	return resolveRegistryRelease(source, constraint, RegistryRefreshMissing)
}

func resolveRegistryRelease(source, constraint string, fetch RegistryFetch) (RegistryRelease, bool, error, error) {
	if strings.HasPrefix(constraint, "sha:") {
		return RegistryRelease{}, false, nil, nil
	}
	lookup, err := lookupRegistryPacks(source, fetch)
	if err != nil {
		return RegistryRelease{}, false, nil, fmt.Errorf("%w: reading pack registries: %w", ErrRegistryRelease, err)
	}
	if len(lookup.Matches) == 0 {
		return RegistryRelease{}, false, errors.Join(lookup.Unavailable...), nil
	}
	release, err := SelectRegistryRelease(lookup.Matches, constraint)
	if err != nil {
		return RegistryRelease{}, false, nil, fmt.Errorf("%w: %w", ErrRegistryRelease, err)
	}
	return release, true, nil, nil
}

// SelectRegistryRelease picks the highest non-withdrawn release satisfying
// constraint across every registry entry that publishes a source, using the
// same selection rule as git-tag resolution. The same version published with
// a different commit or hash by two registries is ambiguous and refused
// rather than guessed. A constraint nothing satisfies is an error listing the
// available versions (and the withdrawal reason when the exact version was
// withdrawn).
func SelectRegistryRelease(matches []packregistry.PackMatch, constraint string) (RegistryRelease, error) {
	type candidate struct {
		pack    string
		release packregistry.CatalogRelease
	}
	byVersion := map[string][]candidate{}
	withdrawn := map[string]string{}
	var names []string
	for _, match := range matches {
		name := match.Registry + ":" + match.Pack.Name
		names = append(names, name)
		for _, release := range match.Pack.Releases {
			if release.Withdrawn {
				withdrawn[release.Version] = release.WithdrawnReason
				continue
			}
			byVersion[release.Version] = append(byVersion[release.Version], candidate{pack: name, release: release})
		}
	}
	versions := make([]string, 0, len(byVersion))
	for version := range byVersion {
		versions = append(versions, version)
	}
	version, ok := SelectVersion(versions, constraint)
	if !ok {
		msg := fmt.Sprintf("registry pack %s has no release matching %q", strings.Join(names, ", "), constraint)
		trimmed := strings.TrimSpace(constraint)
		if reason, isWithdrawn := withdrawn[trimmed]; isWithdrawn {
			msg += fmt.Sprintf("; %s was withdrawn", trimmed)
			if reason != "" {
				msg += ": " + reason
			}
		}
		available := SortVersions(versions)
		if len(available) == 0 {
			return RegistryRelease{}, fmt.Errorf("%s (it publishes no active releases)", msg)
		}
		return RegistryRelease{}, fmt.Errorf("%s (available: %s)", msg, strings.Join(available, ", "))
	}
	candidates := byVersion[version]
	first := candidates[0]
	for _, other := range candidates[1:] {
		if other.release.Commit != first.release.Commit || other.release.Hash != first.release.Hash {
			conflicts := make([]string, 0, len(candidates))
			for _, c := range candidates {
				conflicts = append(conflicts, fmt.Sprintf("%s@%s", c.pack, c.release.Commit))
			}
			sort.Strings(conflicts)
			return RegistryRelease{}, fmt.Errorf("release %s is published with different content by %s; pin one with version = \"sha:<commit>\"", version, strings.Join(conflicts, ", "))
		}
	}
	return RegistryRelease{
		Pack:    first.pack,
		Version: version,
		Commit:  first.release.Commit,
		Hash:    first.release.Hash,
	}, nil
}

// isRegistryRelease reports whether a locked (version, commit) pair is a
// published release of a registry pack. ok=false means no cached registry
// publishes source, so the question does not apply.
func isRegistryRelease(source string, locked LockedPack) (isRelease, ok bool) {
	lookup, err := lookupRegistryPacks(source, RegistryCachedOnly)
	if err != nil || len(lookup.Matches) == 0 {
		return false, false
	}
	for _, match := range lookup.Matches {
		for _, release := range match.Pack.Releases {
			if !release.Withdrawn && release.Version == locked.Version && release.Commit == locked.Commit {
				return true, true
			}
		}
	}
	return false, true
}

// RegistryReleaseLabel names the registry release a locked (version, commit)
// pair is, e.g. "main:gascity 0.1.6", using cached catalogs only. It returns
// "" when no cached registry publishes that release.
func RegistryReleaseLabel(source string, locked LockedPack) string {
	lookup, err := lookupRegistryPacks(source, RegistryCachedOnly)
	if err != nil {
		return ""
	}
	for _, match := range lookup.Matches {
		for _, release := range match.Pack.Releases {
			if !release.Withdrawn && release.Version == locked.Version && release.Commit == locked.Commit {
				return match.Registry + ":" + match.Pack.Name + " " + release.Version
			}
		}
	}
	return ""
}

// verifyRegistryReleaseContent checks the cached source@commit pack content
// against the release hash. A git checkout is hashed from its tree at commit;
// the synthetic cache gc materializes from its embedded packs (a bundled
// source at its canonical pin) has no git history, so its files are hashed
// directly with the same manifest format.
func verifyRegistryReleaseContent(source string, release RegistryRelease) error {
	cachePath, err := RepoCachePath(source, release.Commit)
	if err != nil {
		return err
	}
	subpath := normalizeRemoteSource(source).Subpath
	var verifyErr error
	if _, err := os.Stat(filepath.Join(cachePath, ".git")); err == nil {
		verifyErr = packregistry.VerifyPackContentHash(cachePath, release.Commit, subpath, release.Hash)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking repo cache %q: %w", cachePath, err)
	} else {
		verifyErr = packregistry.VerifyPackDirContentHash(filepath.Join(cachePath, filepath.FromSlash(subpath)), release.Hash)
	}
	if verifyErr != nil {
		return fmt.Errorf("%w: registry release %s (commit %s) failed content verification: %w", ErrRegistryRelease, release, release.Commit, verifyErr)
	}
	return nil
}
