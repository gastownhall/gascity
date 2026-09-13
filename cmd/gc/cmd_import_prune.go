package main

import (
	"fmt"
	"io"
	"sort"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/spf13/cobra"
)

const defaultPruneKeepDays = 7

func newImportPruneCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		apply     bool
		allCities bool
		keepDays  int
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove unreferenced clones from the global pack cache",
		Long: `Remove unreferenced clones from the machine-wide pack cache.

The pack cache (~/.gc/cache/repos) is shared by every city on the machine and
is keyed by (source, commit), so commit churn accumulates stale clones over
time. A clone is "referenced" when some city's packs.lock still pins it; prune
keeps every referenced clone and removes only the rest.

By default prune considers every city in the supervisor registry plus the city
resolved from the current directory; pass --all-cities to reference the full
registry set and ignore the current directory. Prune is a dry run unless
--apply is given. The --keep-days guard never removes an unreferenced clone
whose directory was modified more recently than N days ago, protecting
in-flight installs from a race.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doImportPrune(allCities, apply, keepDays, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "Delete unreferenced clones (default: dry run)")
	cmd.Flags().BoolVar(&allCities, "all-cities", false, "Reference every city in the supervisor registry, ignoring the current directory")
	cmd.Flags().IntVar(&keepDays, "keep-days", defaultPruneKeepDays, "Never prune unreferenced clones modified within this many days")
	return cmd
}

func doImportPrune(allCities, apply bool, keepDays int, stdout, stderr io.Writer) int {
	if keepDays < 0 {
		fmt.Fprintln(stderr, "gc import prune: --keep-days must be >= 0") //nolint:errcheck
		return 1
	}

	cityRoots, err := pruneReferenceCityRoots(allCities)
	if err != nil {
		fmt.Fprintf(stderr, "gc import prune: %v\n", err) //nolint:errcheck
		return 1
	}

	referenced, err := pruneReferencedCacheKeys(cityRoots)
	if err != nil {
		fmt.Fprintf(stderr, "gc import prune: %v\n", err) //nolint:errcheck
		return 1
	}

	// Cache root resolves via the production helper (os.UserHomeDir-based), the
	// same computation pack_include.go uses. It deliberately ignores GC_HOME.
	cacheRoot, err := packman.RepoCacheRoot()
	if err != nil {
		fmt.Fprintf(stderr, "gc import prune: %v\n", err) //nolint:errcheck
		return 1
	}

	result, err := packman.Prune(cacheRoot, referenced, keepDays, apply)
	if err != nil {
		fmt.Fprintf(stderr, "gc import prune: %v\n", err) //nolint:errcheck
		return 1
	}

	mode := "dry-run"
	if result.Applied {
		mode = "applied"
	}
	fmt.Fprintf(stdout, "%d referenced, %d unreferenced (%s) [%s]\n", len(result.Kept), len(result.Pruned), formatBytes(result.FreedBytes), mode) //nolint:errcheck
	for _, e := range result.Pruned {
		fmt.Fprintf(stdout, "  %s %s\n", e.Name, formatBytes(e.Bytes)) //nolint:errcheck
	}
	return 0
}

// pruneReferenceCityRoots returns the set of city roots whose packs.lock files
// define the referenced set. When allCities is false the current city (resolved
// from cwd or --city) is always included alongside the registry; when no city
// resolves and the registry is empty, prune errors so it never deletes clones a
// city outside the registry still pins.
func pruneReferenceCityRoots(allCities bool) ([]string, error) {
	seen := make(map[string]bool)
	var roots []string
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		roots = append(roots, path)
	}

	entries, err := supervisor.NewRegistry(supervisor.RegistryPath()).List()
	if err != nil {
		return nil, fmt.Errorf("reading supervisor registry: %w", err)
	}
	for _, e := range entries {
		add(e.Path)
	}

	if !allCities {
		if cityPath, err := resolveImportRoot(); err == nil {
			add(cityPath)
		} else if len(roots) == 0 {
			return nil, fmt.Errorf("no cities to reference: %w", err)
		}
	}

	if len(roots) == 0 {
		return nil, fmt.Errorf("no cities found to reference; register a city or run from a city directory")
	}
	sort.Strings(roots)
	return roots, nil
}

// pruneReferencedCacheKeys reads every city's packs.lock and returns the set of
// RepoCacheKey directory names those locks still pin. A city with no packs.lock
// contributes nothing (not an error).
func pruneReferencedCacheKeys(cityRoots []string) (map[string]bool, error) {
	referenced := make(map[string]bool)
	for _, cityRoot := range cityRoots {
		lock, err := packman.ReadLockfile(fsys.OSFS{}, cityRoot)
		if err != nil {
			return nil, fmt.Errorf("reading packs.lock for %q: %w", cityRoot, err)
		}
		for source, pack := range lock.Packs {
			if pack.Commit == "" {
				continue
			}
			referenced[packman.RepoCacheKey(source, pack.Commit)] = true
		}
	}
	return referenced, nil
}
