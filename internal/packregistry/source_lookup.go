package packregistry

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/remotesource"
)

// PackMatch is one catalog pack, in one configured registry, whose published
// source addresses the same pack as an import source.
type PackMatch struct {
	// Registry is the configured registry name the pack was found in.
	Registry string
	// Pack is the catalog entry, including its release history.
	Pack CatalogPack
}

// PackLookup is the result of looking an import source up across every
// configured registry.
type PackLookup struct {
	// Matches lists every catalog pack whose source addresses the import
	// source, in configured-registry order.
	Matches []PackMatch
	// Unavailable records registries whose catalog could not be read (no
	// usable cache and the refresh failed, or an invalid cache). A source that
	// matched nothing may still be published by one of them.
	Unavailable []error
}

// PackSourceIdentity reduces a pack source to the identity two spellings of
// the same pack share: the clone URL without a trailing ".git" plus the pack
// subpath. The browse ref of a GitHub tree URL is not part of the identity —
// registry release entries pin the exact commit — so the tree-URL form a
// catalog publishes and the "<repo>.git//<subpath>" form resolve alike.
func PackSourceIdentity(source string) string {
	parsed := remotesource.Parse(source)
	clone := strings.TrimSuffix(strings.TrimRight(parsed.CloneURL, "/"), ".git")
	return clone + "//" + strings.Trim(parsed.Subpath, "/")
}

// LookupPacksBySource finds the catalog packs, across every registry
// configured under home, whose published source addresses source. It reads
// the cached catalogs and refreshes a registry only when its cache is absent,
// mirroring `gc pack registry show`. A missing registries.toml means the
// default public registry, as everywhere else.
func LookupPacksBySource(ctx context.Context, home, source string) (PackLookup, error) {
	return lookupPacksBySource(home, source, func(reg Registry) (Catalog, error) {
		return readCatalogRefreshingMissing(ctx, home, reg)
	})
}

// LookupRefreshedPacksBySource is LookupPacksBySource after refreshing every
// configured registry, so a lookup sees releases published since the cache
// was written. A registry whose refresh fails falls back to its cached
// catalog; it is reported unavailable only when no cache exists either.
func LookupRefreshedPacksBySource(ctx context.Context, home, source string) (PackLookup, error) {
	return lookupPacksBySource(home, source, func(reg Registry) (Catalog, error) {
		catalog, refreshErr := RefreshRegistry(ctx, home, reg, FetchOptions{})
		if refreshErr == nil {
			return catalog, nil
		}
		cached, _, err := ReadCachedRegistryCatalog(home, reg)
		if err != nil {
			return Catalog{}, fmt.Errorf("refresh failed (%w) and no usable cache: %w", refreshErr, err)
		}
		return cached, nil
	})
}

// LookupCachedPacksBySource is LookupPacksBySource without any fetch: a
// registry with no cached catalog is reported unavailable. It suits hot paths
// that must not turn into network operations.
func LookupCachedPacksBySource(home, source string) (PackLookup, error) {
	return lookupPacksBySource(home, source, func(reg Registry) (Catalog, error) {
		catalog, _, err := ReadCachedRegistryCatalog(home, reg)
		return catalog, err
	})
}

func lookupPacksBySource(home, source string, read func(Registry) (Catalog, error)) (PackLookup, error) {
	cfg, err := LoadConfig(home)
	if err != nil {
		return PackLookup{}, err
	}
	want := PackSourceIdentity(source)
	// A remote catalog may not publish a local source (ValidateCatalog), so a
	// local source is looked up only in local registries: no fetch can answer
	// it.
	localSource := isLocalPackSource(strings.TrimSpace(source))
	var lookup PackLookup
	for _, reg := range cfg.Registries {
		if localSource {
			if normalized, err := NormalizeSource(reg.Source); err == nil && normalized.Remote {
				continue
			}
		}
		catalog, err := read(reg)
		if err != nil {
			lookup.Unavailable = append(lookup.Unavailable, fmt.Errorf("registry %s: %w", reg.Name, err))
			continue
		}
		for _, pack := range catalog.Packs {
			if pack.Source != "" && PackSourceIdentity(pack.Source) == want {
				lookup.Matches = append(lookup.Matches, PackMatch{Registry: reg.Name, Pack: pack})
			}
		}
	}
	return lookup, nil
}

func readCatalogRefreshingMissing(ctx context.Context, home string, reg Registry) (Catalog, error) {
	catalog, _, err := ReadCachedRegistryCatalog(home, reg)
	if err == nil {
		return catalog, nil
	}
	if !os.IsNotExist(err) {
		return Catalog{}, err
	}
	return RefreshRegistry(ctx, home, reg, FetchOptions{})
}
