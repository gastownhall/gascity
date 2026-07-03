package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/packregistry"
	"github.com/gastownhall/gascity/internal/remotesource"
)

// newRegistryVersionResolver builds the registry-aware version resolver that
// packman consults before falling back to git-tag resolution. The returned
// closure reads gchome.Default() on each call (rather than capturing it) so a
// later GC_HOME change is honored and there is no stale-home race.
//
// Resolution semantics (see engdocs/design/registry-aware-version-resolution.md):
//   - "sha:" constraints return ok=false so pinned commits flow through the
//     existing sha path unchanged.
//   - A source not owned by any configured registry returns ok=false, so plain
//     git-backed sources keep resolving against tags.
//   - A registry-owned source with no release satisfying the constraint returns
//     a fail-closed error listing the available versions — it must never
//     silently degrade to the wrong git tag.
//   - A catalog that cannot be read (cold cache, parse error) is skipped, so an
//     offline resolution falls back to tags rather than failing hard.
func newRegistryVersionResolver() packman.VersionResolver {
	return func(source, constraint string) (packman.ResolvedVersion, bool, error) {
		if strings.HasPrefix(constraint, "sha:") {
			return packman.ResolvedVersion{}, false, nil
		}

		home := gchome.Default()
		cfg, err := packregistry.LoadConfig(home)
		if err != nil {
			return packman.ResolvedVersion{}, false, nil
		}

		wantKey := normalizedRemoteKey(source)
		for _, reg := range cfg.Registries {
			catalog, _, err := packregistry.ReadCachedRegistryCatalog(home, reg)
			if err != nil {
				continue
			}
			pack, ok := findCatalogPackBySource(catalog, wantKey)
			if !ok {
				continue
			}

			resolved, err := packman.SelectVersion(releaseCommitCandidates(pack), constraint)
			if err != nil {
				return packman.ResolvedVersion{}, false, fmt.Errorf(
					"registry %q pack %q has no release matching %q (available: %s)",
					reg.Name, pack.Name, constraint, availableReleaseVersions(pack))
			}
			return resolved, true, nil
		}

		return packman.ResolvedVersion{}, false, nil
	}
}

// findCatalogPackBySource returns the catalog pack whose normalized source key
// equals wantKey.
func findCatalogPackBySource(catalog packregistry.Catalog, wantKey string) (packregistry.CatalogPack, bool) {
	for _, pack := range catalog.Packs {
		if normalizedRemoteKey(pack.Source) == wantKey {
			return pack, true
		}
	}
	return packregistry.CatalogPack{}, false
}

// normalizedRemoteKey reduces a pack source to a comparison key so that
// catalog sources and user-typed sources match despite differences in the
// ".git" suffix, trailing slashes, or scheme handling.
func normalizedRemoteKey(source string) string {
	parsed := remotesource.Parse(source)
	return strings.TrimSuffix(parsed.CloneURL, ".git") + "//" + parsed.Subpath
}

// releaseCommitCandidates builds the version→commit map of non-withdrawn
// releases for SelectVersion.
func releaseCommitCandidates(pack packregistry.CatalogPack) map[string]string {
	candidates := make(map[string]string, len(pack.Releases))
	for _, release := range pack.Releases {
		if release.Withdrawn {
			continue
		}
		candidates[release.Version] = release.Commit
	}
	return candidates
}

// availableReleaseVersions returns the sorted, non-withdrawn release versions
// of pack as a comma-separated list, or "none" when there are none.
func availableReleaseVersions(pack packregistry.CatalogPack) string {
	versions := make([]string, 0, len(pack.Releases))
	for _, release := range pack.Releases {
		if release.Withdrawn {
			continue
		}
		versions = append(versions, release.Version)
	}
	if len(versions) == 0 {
		return "none"
	}
	sort.Strings(versions)
	return strings.Join(versions, ", ")
}

// ensureRegistryCatalogsForImport refreshes any configured registry whose
// catalog cache is missing, so an interactive `gc import add` can resolve a
// registry-owned source end-to-end even on a cold cache. Refresh failures are
// warned, not fatal: resolution then falls back to git tags. Only the missing
// caches are fetched, mirroring readPackRegistryCatalogForCommand.
func ensureRegistryCatalogsForImport(stderr io.Writer) {
	home := gchome.Default()
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		return
	}
	for _, reg := range cfg.Registries {
		if _, err := os.Stat(packregistry.CachePath(home, reg.Name)); err == nil {
			continue
		}
		fmt.Fprintf(stderr, "fetching registry %q catalog\n", reg.Name) //nolint:errcheck
		if _, err := packregistry.RefreshRegistry(context.Background(), home, reg, packregistry.FetchOptions{}); err != nil {
			fmt.Fprintf(stderr, "warning: refreshing registry %q: %v\n", reg.Name, err) //nolint:errcheck
		}
	}
}
