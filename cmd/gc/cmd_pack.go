package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

func newPackCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Manage remote pack sources",
		Long: `Manage remote pack sources that provide agent configurations.

Packs are git repositories containing pack.toml files that
define agent configurations for rigs. They are cached locally and
can be pinned to specific git refs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPackFetchCmd(stdout, stderr))
	cmd.AddCommand(newPackListCmd(stdout, stderr))
	return cmd
}

func newPackFetchCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "fetch",
		Short: "Clone missing and update existing remote packs",
		Long: `Clone missing and update existing remote pack caches.

Fetches all configured pack sources from their git repositories,
updates the local cache, and writes a lockfile with commit hashes
for reproducibility. Automatically called during "gc start".`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doPackFetch(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// doPackFetch clones missing packs and updates existing ones.
func doPackFetch(stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc pack fetch: %v\n", err) //nolint:errcheck
		return 1
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack fetch: %v\n", err) //nolint:errcheck
		return 1
	}

	if len(cfg.Packs) == 0 {
		fmt.Fprintln(stdout, "No remote packs configured.") //nolint:errcheck
		return 0
	}

	fmt.Fprintf(stdout, "Fetching %d pack source(s)...\n", len(cfg.Packs)) //nolint:errcheck
	if err := config.FetchPacks(cfg.Packs, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc pack fetch: %v\n", err) //nolint:errcheck
		return 1
	}

	// Write lockfile.
	lock, err := config.LockFromCache(cfg.Packs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack fetch: building lock: %v\n", err) //nolint:errcheck
		return 1
	}
	if err := config.WriteLock(cityPath, lock); err != nil {
		fmt.Fprintf(stderr, "gc pack fetch: writing lock: %v\n", err) //nolint:errcheck
		return 1
	}

	for name := range cfg.Packs {
		lt := lock.Packs[name]
		commit := lt.Commit
		if len(commit) > 12 {
			commit = commit[:12]
		}
		fmt.Fprintf(stdout, "  %s: %s\n", name, commit) //nolint:errcheck
	}
	fmt.Fprintln(stdout, "Done.") //nolint:errcheck
	return 0
}

func newPackListCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show remote pack sources and cache status",
		Long: `Show configured pack sources with their cache status.

Displays each pack's name, source URL, git ref, cache status,
and locked commit hash (if available).`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doPackList(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// doPackList shows configured packs and their cache status.
func doPackList(stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc pack list: %v\n", err) //nolint:errcheck
		return 1
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack list: %v\n", err) //nolint:errcheck
		return 1
	}

	if len(cfg.Packs) == 0 {
		fmt.Fprintln(stdout, "No remote packs configured.") //nolint:errcheck
		return 0
	}

	lock, _ := config.ReadLock(cityPath)

	for name, src := range cfg.Packs {
		cached := "not cached"
		cachePath := config.PackCachePath(cityPath, name, src)
		fs := fsys.OSFS{}
		if _, statErr := fs.ReadFile(filepath.Join(cachePath, "pack.toml")); statErr == nil {
			cached = "cached"
		}

		ref := src.Ref
		if ref == "" {
			ref = "HEAD"
		}

		line := fmt.Sprintf("%-20s %-40s ref=%-12s %s", name, src.Source, ref, cached)

		if lt, ok := lock.Packs[name]; ok && lt.Commit != "" {
			commit := lt.Commit
			if len(commit) > 12 {
				commit = commit[:12]
			}
			line += fmt.Sprintf("  commit=%s", commit)
		}

		fmt.Fprintln(stdout, line) //nolint:errcheck
	}
	return 0
}
