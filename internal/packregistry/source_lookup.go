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
	cfg, err := LoadConfig(home)
	if err != nil {
		return PackLookup{}, err
	}
	want := PackSourceIdentity(source)
	var lookup PackLookup
	for _, reg := range cfg.Registries {
		catalog, err := readCatalogRefreshingMissing(ctx, home, reg)
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
