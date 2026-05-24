package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
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
define agent configurations for rigs. New dependency entries are
declared in pack.toml with a durable source and optional version
constraint.`,
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
	cmd.AddCommand(newPackListCmd(stdout, stderr))
	cmd.AddCommand(newPackShowCmd(stdout, stderr))
	cmd.AddCommand(newPackOutdatedCmd(stdout, stderr))
	cmd.AddCommand(newPackUpgradeCmd(stdout, stderr))
	cmd.AddCommand(newPackWhyCmd(stdout, stderr))
	cmd.AddCommand(newPackFetchCmd(stdout, stderr))
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

func newPackListCmd(stdout, stderr io.Writer) *cobra.Command {
	var tree bool
	var legacy bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pack dependencies",
		Long: `List pack dependencies in the selected city or pack scope.

Use --legacy to show the old [packs] cache-status view during the
PackV2 transition.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc pack list: %v\n", err) //nolint:errcheck
				return errExit
			}
			if legacy {
				if jsonOutput {
					fmt.Fprintln(stderr, "gc pack list: --json is not supported for legacy [packs] status; migrate to [imports] or omit --json") //nolint:errcheck
					return errExit
				}
				if doLegacyPackList(cityPath, stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			if doPackDependencyList(cityPath, tree, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&tree, "transitive", false, "show the dependency tree")
	cmd.Flags().BoolVar(&legacy, "legacy", false, "show legacy [packs] cache status")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

func newPackShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show <name-or-source>",
		Short: "Show one pack dependency",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc pack show: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doPackDependencyShow(cityPath, args[0], jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

func newPackOutdatedCmd(stdout, stderr io.Writer) *cobra.Command {
	var refresh bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "outdated [name-or-source]",
		Short: "Show pack dependencies with newer allowed versions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc pack outdated: %v\n", err) //nolint:errcheck
				return errExit
			}
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			if doPackDependencyOutdated(cityPath, target, refresh, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "refresh registry catalogs before checking")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
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

type packDependencyListJSONResult struct {
	SchemaVersion string               `json:"schema_version"`
	Transitive    bool                 `json:"transitive"`
	Imports       []packDependencyJSON `json:"imports"`
}

type packDependencyShowJSONResult struct {
	SchemaVersion string             `json:"schema_version"`
	Import        packDependencyJSON `json:"import"`
}

type packDependencyOutdatedJSONResult struct {
	SchemaVersion string                 `json:"schema_version"`
	Refreshed     bool                   `json:"refreshed"`
	Items         []packOutdatedItemJSON `json:"items"`
}

type packDependencyJSON struct {
	Name            string `json:"name"`
	Source          string `json:"source"`
	Origin          string `json:"origin"`
	SourceKind      string `json:"source_kind"`
	Constraint      string `json:"constraint,omitempty"`
	ResolvedVersion string `json:"resolved_version,omitempty"`
	Ref             string `json:"ref,omitempty"`
	Commit          string `json:"commit,omitempty"`
	Hash            string `json:"hash,omitempty"`
	Synced          bool   `json:"synced"`
	Scope           string `json:"scope"`
	Withdrawn       bool   `json:"withdrawn"`
	WithdrawnReason string `json:"withdrawn_reason,omitempty"`
}

type packOutdatedItemJSON struct {
	Name            string `json:"name"`
	Current         string `json:"current"`
	LatestAllowed   string `json:"latest_allowed"`
	LatestAvailable string `json:"latest_available"`
	Status          string `json:"status"`
	Origin          string `json:"origin"`
	Withdrawn       bool   `json:"withdrawn"`
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

Fetches legacy [packs] sources from their git repositories,
updates the local cache, and writes the legacy pack.lock file.
New PackV2 dependencies should use "gc pack sync".`,
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

func doPackDependencyList(cityPath string, tree bool, jsonOutput bool, stdout, stderr io.Writer) int {
	scope, err := loadImportScopeFS(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack list: %v\n", err) //nolint:errcheck
		return 1
	}
	inspectImports, err := collectInspectableImportsFS(fsys.OSFS{}, cityPath, scope)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack list: %v\n", err) //nolint:errcheck
		return 1
	}
	if len(inspectImports) == 0 && legacyPackSourceCount(cityPath) > 0 {
		if jsonOutput {
			fmt.Fprintln(stderr, "gc pack list: --json is not supported for legacy [packs] status; migrate to [imports] or use text --legacy") //nolint:errcheck
			return 1
		}
		fmt.Fprintln(stderr, "warning: showing legacy [packs] status; run \"gc pack sync\" after migrating to [imports]") //nolint:errcheck
		return doLegacyPackList(cityPath, stdout, stderr)
	}
	if len(inspectImports) == 0 {
		if jsonOutput {
			if err := writeCLIJSONLine(stdout, packDependencyListJSONResult{
				SchemaVersion: "1",
				Transitive:    tree,
				Imports:       []packDependencyJSON{},
			}); err != nil {
				fmt.Fprintf(stderr, "gc pack list: %v\n", err) //nolint:errcheck
				return 1
			}
			return 0
		}
		fmt.Fprintln(stdout, "No pack dependencies configured.") //nolint:errcheck
		return 0
	}
	if legacyPackSourceCount(cityPath) > 0 {
		fmt.Fprintln(stderr, "warning: legacy [packs] entries are not shown; use \"gc pack list --legacy\" for old cache status") //nolint:errcheck
	}
	if jsonOutput {
		lock, err := readImportLockfile(fsys.OSFS{}, cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc pack list: %v\n", err) //nolint:errcheck
			return 1
		}
		nodes, err := buildImportGraph(inspectImports, lock)
		if err != nil {
			fmt.Fprintf(stderr, "gc pack list: %v\n", err) //nolint:errcheck
			return 1
		}
		if err := writeCLIJSONLine(stdout, packDependencyListJSONResult{
			SchemaVersion: "1",
			Transitive:    tree,
			Imports:       packDependencyJSONRows(nodes, tree),
		}); err != nil {
			fmt.Fprintf(stderr, "gc pack list: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	return doImportListAs("gc pack list", cityPath, tree, stdout, stderr)
}

func doPackDependencyShow(cityPath, target string, jsonOutput bool, stdout, stderr io.Writer) int {
	node, err := findPackDependencyNode(cityPath, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack show: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, packDependencyShowJSONResult{
			SchemaVersion: "1",
			Import:        packDependencyJSONRow(node),
		}); err != nil {
			fmt.Fprintf(stderr, "gc pack show: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	writePackDependencyDetails(stdout, node)
	return 0
}

func doPackDependencyOutdated(cityPath, target string, refresh bool, jsonOutput bool, stdout, stderr io.Writer) int {
	nodes, err := packDependencyNodes(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack outdated: %v\n", err) //nolint:errcheck
		return 1
	}
	if target != "" {
		node, err := selectPackDependencyNode(nodes, target)
		if err != nil {
			fmt.Fprintf(stderr, "gc pack outdated: %v\n", err) //nolint:errcheck
			return 1
		}
		nodes = []*importGraphNode{node}
	}

	if refresh {
		if err := refreshPackDependencyRegistries(nodes, stderr); err != nil {
			fmt.Fprintf(stderr, "gc pack outdated: %v\n", err) //nolint:errcheck
			return 1
		}
	}

	rows := []packOutdatedRow{}
	for _, node := range nodes {
		row, ok, err := resolvePackOutdatedRow(node)
		if err != nil {
			fmt.Fprintf(stderr, "gc pack outdated: %v\n", err) //nolint:errcheck
			return 1
		}
		if ok {
			rows = append(rows, row)
		}
	}
	if jsonOutput {
		items := make([]packOutdatedItemJSON, 0, len(rows))
		for _, row := range rows {
			items = append(items, packOutdatedItemJSON(row))
		}
		if err := writeCLIJSONLine(stdout, packDependencyOutdatedJSONResult{
			SchemaVersion: "1",
			Refreshed:     refresh,
			Items:         items,
		}); err != nil {
			fmt.Fprintf(stderr, "gc pack outdated: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "All pack dependencies are current.") //nolint:errcheck
		return 0
	}
	fmt.Fprintln(stdout, "Name                  Current       Latest allowed Latest available Status") //nolint:errcheck
	for _, row := range rows {
		fmt.Fprintf(stdout, "%-21s %-13s %-14s %-16s %s\n", row.Name, row.Current, row.LatestAllowed, row.LatestAvailable, row.Status) //nolint:errcheck
	}
	return 0
}

func findPackDependencyNode(cityPath, target string) (*importGraphNode, error) {
	nodes, err := packDependencyNodes(cityPath)
	if err != nil {
		return nil, err
	}
	return selectPackDependencyNode(nodes, target)
}

func packDependencyNodes(cityPath string) ([]*importGraphNode, error) {
	scope, err := loadImportScopeFS(fsys.OSFS{}, cityPath)
	if err != nil {
		return nil, err
	}
	lock, err := readImportLockfile(fsys.OSFS{}, cityPath)
	if err != nil {
		return nil, err
	}
	inspectImports, err := collectInspectableImportsFS(fsys.OSFS{}, cityPath, scope)
	if err != nil {
		return nil, err
	}
	if len(inspectImports) == 0 {
		return []*importGraphNode{}, nil
	}
	return buildImportGraph(inspectImports, lock)
}

func selectPackDependencyNode(nodes []*importGraphNode, target string) (*importGraphNode, error) {
	matches, err := findImportWhyMatches(nodes, target)
	if err != nil {
		return nil, err
	}
	return matches[0][len(matches[0])-1], nil
}

func writePackDependencyDetails(stdout io.Writer, node *importGraphNode) {
	name := importDisplayName(node.Name)
	fmt.Fprintf(stdout, "Import:      %s\n", name)               //nolint:errcheck
	fmt.Fprintf(stdout, "Source:      %s\n", node.Import.Source) //nolint:errcheck
	if origin := packDependencyOrigin(node); origin != "" {
		fmt.Fprintf(stdout, "Origin:      %s\n", origin) //nolint:errcheck
	}
	if node.Import.Version != "" {
		fmt.Fprintf(stdout, "Constraint:  %s\n", node.Import.Version) //nolint:errcheck
	}
	if !isRemoteImportSource(node.Import.Source) {
		fmt.Fprintln(stdout, "Synced:      path import")                         //nolint:errcheck
		fmt.Fprintf(stdout, "Scope:       %s\n", packDependencyScope(node.Name)) //nolint:errcheck
		return
	}
	if !node.HasLock {
		fmt.Fprintln(stdout, "Synced:      no")                                  //nolint:errcheck
		fmt.Fprintf(stdout, "Scope:       %s\n", packDependencyScope(node.Name)) //nolint:errcheck
		return
	}
	fmt.Fprintf(stdout, "Resolved:    %s\n", node.Resolved.Version) //nolint:errcheck
	fmt.Fprintf(stdout, "Commit:      %s\n", node.Resolved.Commit)  //nolint:errcheck
	if node.Resolved.Ref != "" {
		fmt.Fprintf(stdout, "Ref:         %s\n", node.Resolved.Ref) //nolint:errcheck
	}
	if node.Resolved.Hash != "" {
		fmt.Fprintf(stdout, "Hash:        %s\n", node.Resolved.Hash) //nolint:errcheck
	}
	if node.Resolved.Withdrawn {
		fmt.Fprintln(stdout, "Withdrawn:   yes") //nolint:errcheck
		if node.Resolved.WithdrawnReason != "" {
			fmt.Fprintf(stdout, "Reason:      %s\n", node.Resolved.WithdrawnReason) //nolint:errcheck
		}
	}
	fmt.Fprintln(stdout, "Synced:      yes")                                 //nolint:errcheck
	fmt.Fprintf(stdout, "Scope:       %s\n", packDependencyScope(node.Name)) //nolint:errcheck
}

func packDependencyJSONRows(nodes []*importGraphNode, transitive bool) []packDependencyJSON {
	rows := []packDependencyJSON{}
	var walk func(node *importGraphNode)
	walk = func(node *importGraphNode) {
		if node == nil {
			return
		}
		rows = append(rows, packDependencyJSONRow(node))
		if transitive {
			for _, child := range node.Children {
				walk(child)
			}
		}
	}
	for _, node := range nodes {
		walk(node)
	}
	return rows
}

func packDependencyJSONRow(node *importGraphNode) packDependencyJSON {
	row := packDependencyJSON{
		Name:       importDisplayName(node.Name),
		Source:     node.Import.Source,
		Origin:     packDependencyOrigin(node),
		SourceKind: "path",
		Constraint: node.Import.Version,
		Synced:     node.HasLock,
		Scope:      packDependencyScope(node.Name),
	}
	if isRemoteImportSource(node.Import.Source) {
		row.SourceKind = "git"
	}
	if node.HasLock {
		if node.Resolved.SourceKind != "" {
			row.SourceKind = node.Resolved.SourceKind
		}
		row.ResolvedVersion = node.Resolved.Version
		row.Ref = node.Resolved.Ref
		row.Commit = node.Resolved.Commit
		row.Hash = node.Resolved.Hash
		row.Withdrawn = node.Resolved.Withdrawn
		row.WithdrawnReason = node.Resolved.WithdrawnReason
	}
	return row
}

type packOutdatedRow struct {
	Name            string
	Current         string
	LatestAllowed   string
	LatestAvailable string
	Status          string
	Origin          string
	Withdrawn       bool
}

func resolvePackOutdatedRow(node *importGraphNode) (packOutdatedRow, bool, error) {
	if node == nil || !isRemoteImportSource(node.Import.Source) || !node.HasLock {
		return packOutdatedRow{}, false, nil
	}
	allowed, allowedErr := resolveLatestPackDependency(node, node.Import.Version)
	available, availableErr := resolveLatestPackDependency(node, "")
	if allowedErr != nil || availableErr != nil {
		name := importDisplayName(node.Name)
		switch {
		case allowedErr != nil && availableErr != nil:
			return packOutdatedRow{}, false, fmt.Errorf("%s: checking versions: %w", name, errors.Join(
				fmt.Errorf("latest allowed: %w", allowedErr),
				fmt.Errorf("latest available: %w", availableErr),
			))
		case allowedErr != nil:
			return packOutdatedRow{}, false, fmt.Errorf("%s: checking latest allowed version: %w", name, allowedErr)
		default:
			return packOutdatedRow{}, false, fmt.Errorf("%s: checking latest available version: %w", name, availableErr)
		}
	}
	current := node.Resolved.Version
	row := packOutdatedRow{
		Name:            importDisplayName(node.Name),
		Current:         current,
		LatestAllowed:   current,
		LatestAvailable: current,
		Status:          "current",
		Origin:          packDependencyOrigin(node),
		Withdrawn:       node.Resolved.Withdrawn,
	}
	if allowedErr == nil {
		row.LatestAllowed = allowed.Version
	}
	if availableErr == nil {
		row.LatestAvailable = available.Version
	}
	switch {
	case row.LatestAllowed != "" && row.LatestAllowed != current:
		row.Status = "upgrade_available"
	case row.LatestAvailable != "" && row.LatestAvailable != current:
		row.Status = "newer_outside_constraint"
	default:
		return packOutdatedRow{}, false, nil
	}
	return row, true, nil
}

func resolveLatestPackDependency(node *importGraphNode, constraint string) (packman.ResolvedVersion, error) {
	source := node.Import.Source
	if node.Resolved.Registry != "" && node.Resolved.RegistryPack != "" {
		source = packsource.RegistryLocatorString(node.Resolved.Registry, node.Resolved.RegistryPack)
		return packman.ResolveVersionWithOptions(source, constraint, packman.ResolveOptions{GCHome: gchome.Default()})
	}
	return resolveImportVersion(source, constraint)
}

func refreshPackDependencyRegistries(nodes []*importGraphNode, stderr io.Writer) error {
	home := gchome.Default()
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		return err
	}
	needed := map[string]packregistry.Registry{}
	for _, node := range nodes {
		if node != nil && node.Resolved.Registry != "" {
			for _, reg := range cfg.Registries {
				if reg.Name == node.Resolved.Registry {
					needed[reg.Name] = reg
				}
			}
		}
	}
	names := make([]string, 0, len(needed))
	for name := range needed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := packregistry.RefreshRegistry(context.Background(), home, needed[name], packregistry.FetchOptions{}); err != nil {
			fmt.Fprintf(stderr, "warning: registry %s refresh failed: %v\n", name, err) //nolint:errcheck
			return fmt.Errorf("registry %s refresh failed: %w", name, err)
		}
	}
	return nil
}

func packDependencyOrigin(node *importGraphNode) string {
	if node == nil {
		return ""
	}
	if node.Resolved.Registry == "" {
		if !isRemoteImportSource(node.Import.Source) {
			return "path"
		}
		return "source"
	}
	if node.Resolved.RegistryPack != "" {
		return node.Resolved.Registry + ":" + node.Resolved.RegistryPack
	}
	return node.Resolved.Registry
}

func packDependencyScope(name string) string {
	switch {
	case strings.HasPrefix(name, "default-rig:"):
		return "default rig"
	case strings.HasPrefix(name, "rig:"):
		return "rig"
	default:
		return "pack"
	}
}

func legacyPackSourceCount(cityPath string) int {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return 0
	}
	return len(cfg.Packs)
}

// doLegacyPackList shows configured legacy [packs] and their cache status.
func doLegacyPackList(cityPath string, stdout, stderr io.Writer) int {
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
