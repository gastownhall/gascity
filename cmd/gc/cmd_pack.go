package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/deps"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/packregistry"
	"github.com/gastownhall/gascity/internal/packsource"
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
	cmd.AddCommand(newPackRegistryCmd(stdout, stderr))
	cmd.AddCommand(newPackAddCmd(stdout, stderr))
	cmd.AddCommand(newPackRemoveCmd(stdout, stderr))
	cmd.AddCommand(newPackSyncCmd(stdout, stderr))
	cmd.AddCommand(newPackCheckCmd(stdout, stderr))
	cmd.AddCommand(newPackUpgradeCmd(stdout, stderr))
	cmd.AddCommand(newPackWhyCmd(stdout, stderr))
	cmd.AddCommand(newPackFetchCmd(stdout, stderr))
	cmd.AddCommand(newPackListCmd(stdout, stderr))
	return cmd
}

func newPackAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var version, name string
	cmd := &cobra.Command{
		Use:   "add <source>",
		Short: "Add a pack dependency",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc pack add: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doPackAdd(cityPath, args[0], name, version, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "Version constraint for git-backed dependencies")
	cmd.Flags().StringVar(&name, "name", "", "Local binding name override")
	return cmd
}

func newPackRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a pack dependency",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc pack remove: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportRemoveAs("gc pack remove", fsys.OSFS{}, cityPath, args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newPackSyncCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Reconcile pack dependencies with the lockfile and local cache",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc pack sync: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportInstallAs("gc pack sync", cityPath, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newPackCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Verify pack dependencies against the lockfile and local cache",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc pack check: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportCheckAs("gc pack check", cityPath, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newPackUpgradeCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade [name]",
		Short: "Upgrade pack dependencies within their constraints",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc pack upgrade: %v\n", err) //nolint:errcheck
				return errExit
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			if doImportUpgradeAs("gc pack upgrade", cityPath, name, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newPackWhyCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "why <name-or-source>",
		Short: "Explain why a pack dependency is present",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc pack why: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportWhyAs("gc pack why", cityPath, args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func doPackAdd(cityPath, rawSource, nameOverride, versionFlag string, stdout, stderr io.Writer) int {
	classification := packsource.Classify(rawSource)
	switch classification.Kind {
	case packsource.KindRegistryLocator, packsource.KindQualifiedName, packsource.KindBareName:
		return doPackAddRegistrySelector(cityPath, rawSource, classification, nameOverride, versionFlag, stdout, stderr)
	default:
		return doImportAddAs("gc pack add", fsys.OSFS{}, cityPath, rawSource, nameOverride, versionFlag, stdout, stderr)
	}
}

func doPackAddRegistrySelector(cityPath, rawSource string, classification packsource.Classification, nameOverride, versionFlag string, stdout, stderr io.Writer) int {
	locator, err := resolvePackRegistryAddLocator(classification)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack add %q: %v\n", rawSource, err) //nolint:errcheck
		return 1
	}
	resolverSource := packsource.RegistryLocatorString(locator.Registry, locator.Pack)
	resolved, err := packman.ResolveVersionWithOptions(resolverSource, versionFlag, packman.ResolveOptions{
		GCHome: gchome.Default(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc pack add %q: %v\n", rawSource, err) //nolint:errcheck
		return 1
	}
	version := versionFlag
	if version == "" {
		version, err = defaultImportConstraint(resolved.Version)
		if err != nil {
			fmt.Fprintf(stderr, "gc pack add %q: %v\n", rawSource, err) //nolint:errcheck
			return 1
		}
	}
	name := nameOverride
	if name == "" {
		name = deriveImportName(locator.Pack)
	}
	if name == "" {
		fmt.Fprintln(stderr, "gc pack add: could not derive import name; use --name") //nolint:errcheck
		return 1
	}
	if strings.HasPrefix(name, "default-rig:") {
		fmt.Fprintf(stderr, "gc pack add: import name %q uses reserved prefix \"default-rig:\"\n", name) //nolint:errcheck
		return 1
	}

	scope, err := loadImportScopeFS(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack add: %v\n", err) //nolint:errcheck
		return 1
	}
	if _, exists := scope.imports[name]; exists {
		fmt.Fprintf(stderr, "gc pack add: import %q already exists\n", name) //nolint:errcheck
		return 1
	}
	scope.imports[name] = config.Import{
		Source:  resolved.Source,
		Version: version,
	}
	allImports, err := collectAllImportsFS(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack add %q: %v\n", rawSource, err) //nolint:errcheck
		return 1
	}
	allImports[scope.syntheticKey(name)] = scope.imports[name]
	lock, err := packman.SyncLockWithHints(cityPath, allImports, packman.InstallResolveIfNeeded, map[string]packman.SourceHint{
		resolved.Source: {ResolverSource: resolverSource},
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc pack add %q: %v\n", rawSource, err) //nolint:errcheck
		return 1
	}
	if err := scope.save(); err != nil {
		fmt.Fprintf(stderr, "gc pack add %q: %v\n", rawSource, err) //nolint:errcheck
		return 1
	}
	if err := writeImportLockfile(fsys.OSFS{}, cityPath, lock); err != nil {
		fmt.Fprintf(stderr, "gc pack add %q: %v\n", rawSource, err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "Added pack dependency %q from %s (selected via registry %s:%s)\n", name, resolved.Source, locator.Registry, locator.Pack) //nolint:errcheck
	return 0
}

func resolvePackRegistryAddLocator(classification packsource.Classification) (packsource.RegistryLocator, error) {
	switch classification.Kind {
	case packsource.KindRegistryLocator, packsource.KindQualifiedName:
		return packsource.RegistryLocator{Registry: classification.Registry, Pack: classification.Pack}, nil
	case packsource.KindBareName:
		return resolveBarePackRegistryLocator(classification.Pack)
	default:
		return packsource.RegistryLocator{}, fmt.Errorf("not a registry selector")
	}
}

func resolveBarePackRegistryLocator(packName string) (packsource.RegistryLocator, error) {
	home := gchome.Default()
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		return packsource.RegistryLocator{}, err
	}
	var matches []packsource.RegistryLocator
	var unavailable []string
	for _, reg := range cfg.Registries {
		catalog, _, err := packregistry.ReadCachedRegistryCatalog(home, reg)
		if err != nil {
			unavailable = append(unavailable, reg.Name)
			continue
		}
		for _, pack := range catalog.Packs {
			if pack.Name == packName {
				matches = append(matches, packsource.RegistryLocator{Registry: reg.Name, Pack: packName})
			}
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		choices := make([]string, 0, len(matches))
		for _, match := range matches {
			choices = append(choices, match.Registry+":"+match.Pack)
		}
		return packsource.RegistryLocator{}, fmt.Errorf("pack %q is ambiguous; use one of: %s", packName, strings.Join(choices, ", "))
	}
	if len(unavailable) > 0 {
		return packsource.RegistryLocator{}, fmt.Errorf("pack %q was not found and registry cache(s) unavailable: %s", packName, strings.Join(unavailable, ", "))
	}
	return packsource.RegistryLocator{}, fmt.Errorf("pack %q not found in cached registries", packName)
}

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
	cfg, err := packregistry.LoadConfig(gchome.Default())
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
		catalog, _, err := packregistry.ReadCachedRegistryCatalog(home, reg)
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
		catalog, _, err := packregistry.ReadCachedRegistryCatalog(home, reg)
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
	fmt.Fprintf(stdout, "Latest:      %s\n", latestVersion(match.pack))          //nolint:errcheck
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
