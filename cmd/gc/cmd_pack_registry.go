package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/deps"
	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/packregistry"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/spf13/cobra"
)

func newPackRegistryCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage pack registries",
		Long:  "Manage configured Gas City pack registries and inspect cached catalog entries.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPackRegistryListCmd(stdout, stderr))
	cmd.AddCommand(newPackRegistryAddCmd(stdout, stderr))
	cmd.AddCommand(newPackRegistryRemoveCmd(stdout, stderr))
	cmd.AddCommand(newPackRegistryRefreshCmd(stdout, stderr))
	cmd.AddCommand(newPackRegistrySearchCmd(stdout, stderr))
	cmd.AddCommand(newPackRegistryShowCmd(stdout, stderr))
	cmd.AddCommand(newRegistryLoginCmd(stdout, stderr))
	cmd.AddCommand(newRegistryPublishCmd(stdout, stderr))
	cmd.AddCommand(newRegistryWhoamiCmd(stdout, stderr))
	return cmd
}

func newPackRegistryListCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured pack registries",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doPackRegistryList(jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

func newPackRegistryAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var noValidate bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "add <registry-name> <source>",
		Short: "Add a pack registry",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if doPackRegistryAdd(args[0], args[1], noValidate, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noValidate, "no-validate", false, "record the registry without fetching its catalog now")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

func newPackRegistryRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "remove <registry-name>",
		Short: "Remove a pack registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doPackRegistryRemove(args[0], jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

func newPackRegistryRefreshCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "refresh [registry-name]",
		Short: "Refresh cached pack registry catalogs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			if doPackRegistryRefresh(name, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

func newPackRegistrySearchCmd(stdout, stderr io.Writer) *cobra.Command {
	var registry string
	var refresh bool
	var limit int
	var all bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search cached pack registry catalogs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			query := ""
			if len(args) > 0 {
				query = args[0]
			}
			if doPackRegistrySearch(query, registry, refresh, limit, all, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&registry, "registry", "", "search only one registry")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "refresh catalogs before searching")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum number of results")
	cmd.Flags().BoolVar(&all, "all", false, "show all results")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

func newPackRegistryShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var refresh bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show <pack-name>",
		Short: "Show one pack registry entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doPackRegistryShow(args[0], refresh, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "refresh catalogs before showing")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

type packRegistryRefJSON struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type packRegistryListJSONResult struct {
	SchemaVersion string                `json:"schema_version"`
	Count         int                   `json:"count"`
	Registries    []packRegistryRefJSON `json:"registries"`
}

type packRegistryAddJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	Name          string `json:"name"`
	Source        string `json:"source"`
	Validated     bool   `json:"validated"`
	Cached        bool   `json:"cached"`
}

type packRegistryRemoveJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	Name          string `json:"name"`
	Removed       bool   `json:"removed"`
}

type packRegistryRefreshJSONResult struct {
	SchemaVersion string                    `json:"schema_version"`
	Target        string                    `json:"target,omitempty"`
	Refreshed     []packRegistryRefreshJSON `json:"refreshed"`
	Failures      []packRegistryFailureJSON `json:"failures"`
	PrunedCaches  bool                      `json:"pruned_caches"`
}

type packRegistryRefreshJSON struct {
	Name      string `json:"name"`
	PackCount int    `json:"pack_count"`
}

type packRegistryFailureJSON struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

type packRegistrySearchJSONResult struct {
	SchemaVersion string                    `json:"schema_version"`
	Query         string                    `json:"query"`
	Registry      string                    `json:"registry,omitempty"`
	Refreshed     bool                      `json:"refreshed"`
	Limit         int                       `json:"limit"`
	All           bool                      `json:"all"`
	Truncated     bool                      `json:"truncated"`
	Count         int                       `json:"count"`
	Results       []packRegistryPackJSON    `json:"results"`
	Failures      []packRegistryFailureJSON `json:"failures"`
}

type packRegistryShowJSONResult struct {
	SchemaVersion string                    `json:"schema_version"`
	Registry      string                    `json:"registry"`
	Name          string                    `json:"name"`
	Description   string                    `json:"description"`
	Source        string                    `json:"source"`
	SourceKind    string                    `json:"source_kind"`
	Latest        string                    `json:"latest"`
	Releases      []packRegistryReleaseJSON `json:"releases"`
}

type packRegistryPackJSON struct {
	Registry    string `json:"registry"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	SourceKind  string `json:"source_kind"`
	Latest      string `json:"latest"`
}

type packRegistryReleaseJSON struct {
	Version         string `json:"version"`
	Ref             string `json:"ref"`
	Commit          string `json:"commit"`
	Hash            string `json:"hash"`
	Description     string `json:"description"`
	Withdrawn       bool   `json:"withdrawn"`
	WithdrawnReason string `json:"withdrawn_reason,omitempty"`
}

func doPackRegistryList(jsonOutput bool, stdout, stderr io.Writer) int {
	home := gchome.Default()
	if err := packregistry.EnsureDefaultRegistryConfig(home); err != nil {
		fmt.Fprintf(stderr, "gc pack registry list: %v\n", err) //nolint:errcheck
		return 1
	}
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack registry list: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOutput {
		registries := make([]packRegistryRefJSON, 0, len(cfg.Registries))
		for _, reg := range cfg.Registries {
			registries = append(registries, packRegistryRefJSON{Name: reg.Name, Source: reg.Source})
		}
		if err := writeCLIJSONLine(stdout, packRegistryListJSONResult{
			SchemaVersion: "1",
			Count:         len(registries),
			Registries:    registries,
		}); err != nil {
			fmt.Fprintf(stderr, "gc pack registry list: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	if len(cfg.Registries) == 0 {
		fmt.Fprintln(stdout, "No pack registries configured.") //nolint:errcheck
		return 0
	}
	fmt.Fprintln(stdout, "Name                  Source") //nolint:errcheck
	for _, reg := range cfg.Registries {
		fmt.Fprintf(stdout, "%-21s %s\n", reg.Name, reg.Source) //nolint:errcheck
	}
	return 0
}

func doPackRegistryAdd(name, source string, noValidate, jsonOutput bool, stdout, stderr io.Writer) int {
	home := gchome.Default()
	reg := packregistry.Registry{Name: name, Source: source}
	if err := packregistry.ValidateRegistryName(name); err != nil {
		fmt.Fprintf(stderr, "gc pack registry add: %v\n", err) //nolint:errcheck
		return 1
	}
	var catalogData []byte
	if !noValidate {
		_, data, _, err := packregistry.ReadCatalog(context.Background(), source, packregistry.FetchOptions{})
		if err != nil {
			fmt.Fprintf(stderr, "gc pack registry add: validating catalog: %v\n", err) //nolint:errcheck
			return 1
		}
		catalogData = data
	}
	if name != packregistry.DefaultRegistryName {
		if err := packregistry.EnsureDefaultRegistryConfig(home); err != nil {
			fmt.Fprintf(stderr, "gc pack registry add: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if err := packregistry.AddRegistryWithCache(home, reg, catalogData); err != nil {
		fmt.Fprintf(stderr, "gc pack registry add: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, packRegistryAddJSONResult{
			SchemaVersion: "1",
			Name:          name,
			Source:        source,
			Validated:     !noValidate,
			Cached:        !noValidate,
		}); err != nil {
			fmt.Fprintf(stderr, "gc pack registry add: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Added pack registry %q.\n", name) //nolint:errcheck
	return 0
}

func doPackRegistryRemove(name string, jsonOutput bool, stdout, stderr io.Writer) int {
	home := gchome.Default()
	if err := packregistry.ValidateRegistryName(name); err != nil {
		fmt.Fprintf(stderr, "gc pack registry remove: %v\n", err) //nolint:errcheck
		return 1
	}
	if name == packregistry.DefaultRegistryName {
		if err := packregistry.EnsureDefaultRegistryConfig(home); err != nil {
			fmt.Fprintf(stderr, "gc pack registry remove: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	removed, err := packregistry.RemoveRegistry(home, name)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack registry remove: %v\n", err) //nolint:errcheck
		return 1
	}
	if !removed {
		fmt.Fprintf(stderr, "gc pack registry remove: registry %q is not configured\n", name) //nolint:errcheck
		return 1
	}
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, packRegistryRemoveJSONResult{
			SchemaVersion: "1",
			Name:          name,
			Removed:       true,
		}); err != nil {
			fmt.Fprintf(stderr, "gc pack registry remove: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Removed pack registry %q.\n", name) //nolint:errcheck
	return 0
}

func doPackRegistryRefresh(name string, jsonOutput bool, stdout, stderr io.Writer) int {
	home := gchome.Default()
	if err := packregistry.EnsureDefaultRegistryConfig(home); err != nil {
		fmt.Fprintf(stderr, "gc pack registry refresh: %v\n", err) //nolint:errcheck
		return 1
	}
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack registry refresh: %v\n", err) //nolint:errcheck
		return 1
	}
	prunedCaches := false
	if name == "" {
		if err := pruneInactiveRegistryCaches(home, cfg.Registries); err != nil {
			fmt.Fprintf(stderr, "gc pack registry refresh: pruning cache: %v\n", err) //nolint:errcheck
			return 1
		}
		prunedCaches = true
	}
	regs, err := selectRegistries(cfg.Registries, name)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack registry refresh: %v\n", err) //nolint:errcheck
		return 1
	}
	if len(regs) == 0 {
		if jsonOutput {
			if err := writeCLIJSONLine(stdout, packRegistryRefreshJSONResult{
				SchemaVersion: "1",
				Target:        name,
				Refreshed:     []packRegistryRefreshJSON{},
				Failures:      []packRegistryFailureJSON{},
				PrunedCaches:  prunedCaches,
			}); err != nil {
				fmt.Fprintf(stderr, "gc pack registry refresh: %v\n", err) //nolint:errcheck
				return 1
			}
			return 0
		}
		fmt.Fprintln(stdout, "No pack registries configured.") //nolint:errcheck
		return 0
	}
	refreshed := []packRegistryRefreshJSON{}
	failures := []packRegistryFailureJSON{}
	for _, reg := range regs {
		catalog, err := packregistry.RefreshRegistry(context.Background(), home, reg, packregistry.FetchOptions{})
		if err != nil {
			failures = append(failures, packRegistryFailureJSON{Name: reg.Name, Message: err.Error()})
			fmt.Fprintf(stderr, "gc pack registry refresh: %s: %v\n", reg.Name, err) //nolint:errcheck
			continue
		}
		refreshed = append(refreshed, packRegistryRefreshJSON{Name: reg.Name, PackCount: len(catalog.Packs)})
		if jsonOutput {
			continue
		}
		fmt.Fprintf(stdout, "%s: refreshed %d pack(s)\n", reg.Name, len(catalog.Packs)) //nolint:errcheck
	}
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, packRegistryRefreshJSONResult{
			SchemaVersion: "1",
			Target:        name,
			Refreshed:     refreshed,
			Failures:      failures,
			PrunedCaches:  prunedCaches,
		}); err != nil {
			fmt.Fprintf(stderr, "gc pack registry refresh: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if len(refreshed) == 0 {
		return 1
	}
	return 0
}

type registrySearchResult struct {
	registry string
	pack     packregistry.CatalogPack
}

func doPackRegistrySearch(query, registry string, refresh bool, limit int, all bool, jsonOutput bool, stdout, stderr io.Writer) int {
	home := gchome.Default()
	if err := packregistry.EnsureDefaultRegistryConfig(home); err != nil {
		fmt.Fprintf(stderr, "gc pack registry search: %v\n", err) //nolint:errcheck
		return 1
	}
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack registry search: %v\n", err) //nolint:errcheck
		return 1
	}
	regs, err := selectRegistries(cfg.Registries, registry)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack registry search: %v\n", err) //nolint:errcheck
		return 1
	}
	results := []registrySearchResult{}
	refreshFailures := []packRegistryFailureJSON{}
	cacheFailures := []packRegistryFailureJSON{}
	failures := 0
	lowerQuery := strings.ToLower(query)
	for _, reg := range regs {
		if refresh {
			if _, err := packregistry.RefreshRegistry(context.Background(), home, reg, packregistry.FetchOptions{}); err != nil {
				refreshFailures = append(refreshFailures, packRegistryFailureJSON{Name: reg.Name, Message: err.Error()})
				fmt.Fprintf(stderr, "warning: registry %s refresh failed: %v\n", reg.Name, err) //nolint:errcheck
			}
		}
		catalog, err := readPackRegistryCatalogForCommand(context.Background(), home, reg, !refresh)
		if err != nil {
			failures++
			cacheFailures = append(cacheFailures, packRegistryFailureJSON{Name: reg.Name, Message: err.Error()})
			fmt.Fprintf(stderr, "warning: registry %s cache unavailable: %v\n", reg.Name, err) //nolint:errcheck
			continue
		}
		warnStaleRegistryCache(home, reg.Name, stderr)
		for _, pack := range catalog.Packs {
			if query == "" || strings.Contains(strings.ToLower(pack.Name), lowerQuery) || strings.Contains(strings.ToLower(pack.Description), lowerQuery) {
				results = append(results, registrySearchResult{registry: reg.Name, pack: pack})
			}
		}
	}
	if len(regs) > 0 && failures == len(regs) {
		fmt.Fprintln(stderr, "gc pack registry search: no registry caches were available") //nolint:errcheck
		return 1
	}
	slices.SortFunc(results, func(a, b registrySearchResult) int {
		left, right := a.registry+":"+a.pack.Name, b.registry+":"+b.pack.Name
		if left < right {
			return -1
		}
		if left > right {
			return 1
		}
		return 0
	})
	if limit <= 0 {
		limit = 50
	}
	truncated := false
	if !all && len(results) > limit {
		results = results[:limit]
		truncated = true
	}
	if jsonOutput {
		jsonResults := make([]packRegistryPackJSON, 0, len(results))
		for _, result := range results {
			jsonResults = append(jsonResults, packRegistryPackJSON{
				Registry:    result.registry,
				Name:        result.pack.Name,
				Description: result.pack.Description,
				Source:      result.pack.Source,
				SourceKind:  result.pack.SourceKind,
				Latest:      latestVersion(result.pack),
			})
		}
		allFailures := append([]packRegistryFailureJSON{}, refreshFailures...)
		allFailures = append(allFailures, cacheFailures...)
		if err := writeCLIJSONLine(stdout, packRegistrySearchJSONResult{
			SchemaVersion: "1",
			Query:         query,
			Registry:      registry,
			Refreshed:     refresh,
			Limit:         limit,
			All:           all,
			Truncated:     truncated,
			Count:         len(jsonResults),
			Results:       jsonResults,
			Failures:      allFailures,
		}); err != nil {
			fmt.Fprintf(stderr, "gc pack registry search: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	if len(results) == 0 {
		fmt.Fprintln(stdout, "No registry packs found.") //nolint:errcheck
		return 0
	}
	fmt.Fprintln(stdout, "Registry  Name                  Latest        Description") //nolint:errcheck
	for _, result := range results {
		fmt.Fprintf(stdout, "%-9s %-21s %-13s %s\n", result.registry, result.pack.Name, latestVersion(result.pack), result.pack.Description) //nolint:errcheck
	}
	if truncated {
		fmt.Fprintf(stderr, "warning: results truncated to %d; use --all to show all\n", limit) //nolint:errcheck
	}
	return 0
}

func doPackRegistryShow(target string, refresh bool, jsonOutput bool, stdout, stderr io.Writer) int {
	home := gchome.Default()
	if err := packregistry.EnsureDefaultRegistryConfig(home); err != nil {
		fmt.Fprintf(stderr, "gc pack registry show: %v\n", err) //nolint:errcheck
		return 1
	}
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack registry show: %v\n", err) //nolint:errcheck
		return 1
	}
	regs := cfg.Registries
	name := target
	qualified := false
	if regName, packName, ok := strings.Cut(target, ":"); ok {
		selected, err := selectRegistries(cfg.Registries, regName)
		if err != nil {
			fmt.Fprintf(stderr, "gc pack registry show: %v\n", err) //nolint:errcheck
			return 1
		}
		regs = selected
		name = packName
		qualified = true
	}
	matches := []registrySearchResult{}
	unavailable := []string{}
	for _, reg := range regs {
		if refresh {
			if _, err := packregistry.RefreshRegistry(context.Background(), home, reg, packregistry.FetchOptions{}); err != nil {
				fmt.Fprintf(stderr, "warning: registry %s refresh failed: %v\n", reg.Name, err) //nolint:errcheck
			}
		}
		catalog, err := readPackRegistryCatalogForCommand(context.Background(), home, reg, !refresh)
		if err != nil {
			unavailable = append(unavailable, reg.Name)
			continue
		}
		warnStaleRegistryCache(home, reg.Name, stderr)
		for _, pack := range catalog.Packs {
			if pack.Name == name {
				matches = append(matches, registrySearchResult{registry: reg.Name, pack: pack})
			}
		}
	}
	if !qualified && len(unavailable) > 0 {
		fmt.Fprintf(stderr, "gc pack registry show: registry %s unavailable; qualify the pack name after refreshing registries\n", strings.Join(unavailable, ", ")) //nolint:errcheck
		return 1
	}
	if qualified && len(unavailable) > 0 && len(matches) == 0 {
		fmt.Fprintf(stderr, "gc pack registry show: registry %s cache unavailable\n", strings.Join(unavailable, ", ")) //nolint:errcheck
		return 1
	}
	if len(matches) == 0 {
		fmt.Fprintf(stderr, "gc pack registry show: pack %q not found in cached registries\n", target) //nolint:errcheck
		return 1
	}
	if len(matches) > 1 {
		var choices []string
		for _, match := range matches {
			choices = append(choices, match.registry+":"+match.pack.Name)
		}
		fmt.Fprintf(stderr, "gc pack registry show: pack %q is ambiguous; use one of: %s\n", target, strings.Join(choices, ", ")) //nolint:errcheck
		return 1
	}
	match := matches[0]
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, packRegistryShowJSONResult{
			SchemaVersion: "1",
			Registry:      match.registry,
			Name:          match.pack.Name,
			Description:   match.pack.Description,
			Source:        match.pack.Source,
			SourceKind:    match.pack.SourceKind,
			Latest:        latestVersion(match.pack),
			Releases:      releaseJSONRows(match.pack.Releases),
		}); err != nil {
			fmt.Fprintf(stderr, "gc pack registry show: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Pack:        %s:%s\n", match.registry, match.pack.Name) //nolint:errcheck
	fmt.Fprintf(stdout, "Description: %s\n", match.pack.Description)             //nolint:errcheck
	fmt.Fprintf(stdout, "Source:      %s\n", match.pack.Source)                  //nolint:errcheck
	fmt.Fprintf(stdout, "Source kind: %s\n", match.pack.SourceKind)              //nolint:errcheck
	latest := latestVersion(match.pack)
	fmt.Fprintf(stdout, "Latest:      %s\n", latest) //nolint:errcheck
	if latest != "" {
		floating, exact := importCommandSuggestions(match.pack, latest)
		fmt.Fprintln(stdout, "Import commands:")                       //nolint:errcheck
		fmt.Fprintf(stdout, "  This version or later: %s\n", floating) //nolint:errcheck
		fmt.Fprintf(stdout, "  Exactly this version:  %s\n", exact)    //nolint:errcheck
	}
	if len(match.pack.Releases) > 0 {
		fmt.Fprintln(stdout, "Releases:") //nolint:errcheck
		for _, release := range match.pack.Releases {
			suffix := ""
			if release.Withdrawn {
				suffix = " withdrawn"
			}
			fmt.Fprintf(stdout, "  %s %s %s%s\n", release.Version, release.Ref, shortCommit(release.Commit), suffix) //nolint:errcheck
		}
	}
	return 0
}

func importCommandSuggestions(pack packregistry.CatalogPack, latest string) (string, string) {
	base := []string{"import", "add", pack.Source, "--name", pack.Name, "--version"}
	floating := append([]string{"gc"}, append(base, ">="+latest)...)
	exact := append([]string{"gc"}, append(base, latest)...)
	return shellquote.Join(floating), shellquote.Join(exact)
}

func readPackRegistryCatalogForCommand(ctx context.Context, home string, reg packregistry.Registry, refreshMissing bool) (packregistry.Catalog, error) {
	catalog, _, err := packregistry.ReadCachedRegistryCatalog(home, reg)
	if err == nil {
		return catalog, nil
	}
	if !refreshMissing || !os.IsNotExist(err) {
		return packregistry.Catalog{}, err
	}
	return packregistry.RefreshRegistry(ctx, home, reg, packregistry.FetchOptions{})
}

func warnStaleRegistryCache(home, registry string, stderr io.Writer) {
	maxAge, err := packregistry.FreshnessFromEnv(24 * time.Hour)
	if err != nil {
		fmt.Fprintf(stderr, "warning: %v\n", err) //nolint:errcheck
		return
	}
	fresh, err := packregistry.CatalogFresh(packregistry.CachePath(home, registry), time.Now(), maxAge)
	if err == nil && !fresh {
		fmt.Fprintf(stderr, "warning: registry %s cache is stale; use --refresh to update\n", registry) //nolint:errcheck
	}
}

func selectRegistries(regs []packregistry.Registry, name string) ([]packregistry.Registry, error) {
	if name == "" {
		return regs, nil
	}
	for _, reg := range regs {
		if reg.Name == name {
			return []packregistry.Registry{reg}, nil
		}
	}
	return nil, fmt.Errorf("registry %q is not configured", name)
}

func pruneInactiveRegistryCaches(home string, regs []packregistry.Registry) error {
	active := map[string]bool{}
	for _, reg := range regs {
		active[reg.Name] = true
	}
	return packregistry.PruneRemovedRegistryCaches(home, active)
}

func latestVersion(pack packregistry.CatalogPack) string {
	latest := ""
	for _, release := range pack.Releases {
		if release.Withdrawn {
			continue
		}
		if latest == "" || deps.CompareVersions(latest, release.Version) < 0 {
			latest = release.Version
		}
	}
	return latest
}

func releaseJSONRows(releases []packregistry.CatalogRelease) []packRegistryReleaseJSON {
	rows := make([]packRegistryReleaseJSON, 0, len(releases))
	for _, release := range releases {
		rows = append(rows, packRegistryReleaseJSON{
			Version:         release.Version,
			Ref:             release.Ref,
			Commit:          release.Commit,
			Hash:            release.Hash,
			Description:     release.Description,
			Withdrawn:       release.Withdrawn,
			WithdrawnReason: release.WithdrawnReason,
		})
	}
	return rows
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
