package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/spf13/cobra"
)

var (
	syncImports             = packman.SyncLock
	syncImportsSelective    = packman.SyncLockSelectiveUpgrade
	installLockedImports    = packman.InstallLocked
	readImportLockfile      = packman.ReadLockfile
	writeImportLockfile     = packman.WriteLockfile
	resolveImportVersion    = packman.ResolveVersion
	defaultImportConstraint = packman.DefaultConstraint
	resolveImportHeadCommit = defaultImportHeadCommit
)

const cityPackSchema = 1

type cityPackManifest struct {
	Pack                  config.PackMeta                `toml:"pack"`
	Imports               map[string]config.Import       `toml:"imports,omitempty"`
	AgentDefaults         config.AgentDefaults           `toml:"agent_defaults,omitempty"`
	Defaults              cityPackDefaults               `toml:"defaults,omitempty"`
	DefaultRigImportOrder []string                       `toml:"-"`
	Agents                []config.Agent                 `toml:"agent,omitempty"`
	NamedSessions         []config.NamedSession          `toml:"named_session,omitempty"`
	Services              []config.Service               `toml:"service,omitempty"`
	Providers             map[string]config.ProviderSpec `toml:"providers,omitempty"`
	Formulas              config.FormulasConfig          `toml:"formulas,omitempty"`
	Patches               config.Patches                 `toml:"patches,omitempty"`
	Doctor                []config.PackDoctorEntry       `toml:"doctor,omitempty"`
	Commands              []config.PackCommandEntry      `toml:"commands,omitempty"`
	Global                config.PackGlobal              `toml:"global,omitempty"`
}

type cityPackDefaults struct {
	Rig cityPackRigDefaults `toml:"rig,omitempty"`
}

type cityPackRigDefaults struct {
	Imports map[string]config.Import `toml:"imports,omitempty"`
}

type cityPackManifestBody struct {
	Pack          config.PackMeta                `toml:"pack"`
	Imports       map[string]config.Import       `toml:"imports,omitempty"`
	AgentDefaults config.AgentDefaults           `toml:"agent_defaults,omitempty"`
	Agents        []config.Agent                 `toml:"agent,omitempty"`
	NamedSessions []config.NamedSession          `toml:"named_session,omitempty"`
	Services      []config.Service               `toml:"service,omitempty"`
	Providers     map[string]config.ProviderSpec `toml:"providers,omitempty"`
	Formulas      config.FormulasConfig          `toml:"formulas,omitempty"`
	Patches       config.Patches                 `toml:"patches,omitempty"`
	Doctor        []config.PackDoctorEntry       `toml:"doctor,omitempty"`
	Commands      []config.PackCommandEntry      `toml:"commands,omitempty"`
	Global        config.PackGlobal              `toml:"global,omitempty"`
}

func newImportCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Manage pack imports",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newImportAddCmd(stdout, stderr),
		newImportRemoveCmd(stdout, stderr),
		newImportInstallCmd(stdout, stderr),
		newImportUpgradeCmd(stdout, stderr),
		newImportListCmd(stdout, stderr),
		newImportWhyCmd(stdout, stderr),
		newImportMigrateCmd(stdout, stderr),
	)
	return cmd
}

func newImportAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var version, name string
	cmd := &cobra.Command{
		Use:   "add <source>",
		Short: "Add a pack import",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc import add: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportAdd(fsys.OSFS{}, cityPath, args[0], name, version, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "Version constraint for git-backed imports")
	cmd.Flags().StringVar(&name, "name", "", "Local binding name override")
	return cmd
}

func newImportRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a pack import",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc import remove: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportRemove(fsys.OSFS{}, cityPath, args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newImportInstallCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install imports from pack.toml and packs.lock",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc import install: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportInstall(cityPath, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newImportUpgradeCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade [name]",
		Short: "Upgrade imported packs within their constraints",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc import upgrade: %v\n", err) //nolint:errcheck
				return errExit
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			if doImportUpgrade(cityPath, name, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newImportListCmd(stdout, stderr io.Writer) *cobra.Command {
	var tree bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List imported packs",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc import list: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportList(cityPath, tree, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&tree, "tree", false, "Show the import dependency tree")
	return cmd
}

func newImportWhyCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "why <name-or-source>",
		Short: "Explain why an import is present",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc import why: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportWhy(cityPath, args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func resolveImportRoot() (string, error) {
	if raw := strings.TrimSpace(cityFlag); raw != "" {
		return validateImportRootPath(raw)
	}
	if raw, ok := resolveExplicitImportPathEnv(); ok {
		return validateImportRootPath(raw)
	}
	if cityPath, err := resolveCity(); err == nil {
		return cityPath, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return findPackRoot(cwd)
}

func resolveExplicitImportPathEnv() (string, bool) {
	for _, key := range []string{"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT"} {
		if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
			return raw, true
		}
	}
	return "", false
}

func validateImportRootPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if cityPath, err := validateCityPath(abs); err == nil {
		return cityPath, nil
	}
	if packExists(abs) {
		return abs, nil
	}
	return "", fmt.Errorf("not a city or pack directory: %s (no city.toml, .gc/, or pack.toml found)", abs)
}

func findPackRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if packExists(abs) {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return "", fmt.Errorf("could not find city or pack root from %s", dir)
}

type importScopeState struct {
	imports      map[string]config.Import
	syntheticTag string
	save         func() error
}

func (s *importScopeState) syntheticKey(name string) string {
	return s.syntheticTag + name
}

func loadImportScopeFS(fs fsys.FS, cityPath string) (*importScopeState, error) {
	targetRig := strings.TrimSpace(rigFlag)
	if targetRig == "" {
		manifest, err := loadCityPackManifestFS(fs, cityPath)
		if err != nil {
			return nil, err
		}
		if manifest.Imports == nil {
			manifest.Imports = make(map[string]config.Import)
		}
		return &importScopeState{
			imports:      manifest.Imports,
			syntheticTag: "pack:",
			save: func() error {
				return writeCityPackManifest(fs, cityPath, manifest)
			},
		}, nil
	}

	if _, err := fs.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("rig-scoped imports require a city directory: %s", cityPath)
		}
		return nil, err
	}

	cfg, err := loadCityImportManifestFS(fs, cityPath)
	if err != nil {
		return nil, err
	}
	rigIndex, rigName, err := findImportRigIndex(cityPath, cfg.Rigs, targetRig)
	if err != nil {
		return nil, err
	}
	if cfg.Rigs[rigIndex].Imports == nil {
		cfg.Rigs[rigIndex].Imports = make(map[string]config.Import)
	}
	return &importScopeState{
		imports:      cfg.Rigs[rigIndex].Imports,
		syntheticTag: "rig:" + rigName + ":",
		save: func() error {
			return writeCityImportManifestFS(fs, cityPath, cfg)
		},
	}, nil
}

func collectAllImportsFS(fs fsys.FS, cityPath string) (map[string]config.Import, error) {
	all := make(map[string]config.Import)

	packManifest, err := loadCityPackManifestFS(fs, cityPath)
	if err != nil {
		return nil, err
	}
	for name, imp := range packManifest.Imports {
		all["pack:"+name] = imp
	}

	if _, err := fs.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		if os.IsNotExist(err) {
			return all, nil
		}
		return nil, err
	}

	cfg, err := loadCityImportManifestFS(fs, cityPath)
	if err != nil {
		return nil, err
	}
	for _, rig := range cfg.Rigs {
		for name, imp := range rig.Imports {
			all["rig:"+rig.Name+":"+name] = imp
		}
	}
	return all, nil
}

func loadCityImportManifestFS(fs fsys.FS, cityPath string) (*config.City, error) {
	return loadCityConfigForEditFS(fs, filepath.Join(cityPath, "city.toml"))
}

func writeCityImportManifestFS(fs fsys.FS, cityPath string, cfg *config.City) error {
	if cfg == nil {
		cfg = &config.City{}
	}
	return writeCityConfigForEditFS(fs, filepath.Join(cityPath, "city.toml"), cfg)
}

func findImportRigIndex(cityPath string, rigs []config.Rig, target string) (int, string, error) {
	for i, rig := range rigs {
		if rig.Name == target {
			return i, rig.Name, nil
		}
	}

	resolvedRigs := append([]config.Rig(nil), rigs...)
	resolveRigPaths(cityPath, resolvedRigs)

	targetPath := target
	if !filepath.IsAbs(targetPath) {
		abs, err := filepath.Abs(filepath.Join(cityPath, targetPath))
		if err == nil {
			targetPath = abs
		}
	}
	for i, rig := range resolvedRigs {
		if samePath(rig.Path, targetPath) {
			return i, rigs[i].Name, nil
		}
	}

	return -1, "", fmt.Errorf("rig %q not found", target)
}

//nolint:unparam // keep fs injectable for parity with the other import helpers and direct tests.
func doImportAdd(fs fsys.FS, cityPath, source, nameOverride, versionFlag string, stdout, stderr io.Writer) int {
	scope, err := loadImportScopeFS(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import add: %v\n", err) //nolint:errcheck
		return 1
	}

	source, gitBacked, err := normalizeImportAddSource(fs, cityPath, source)
	if err != nil {
		fmt.Fprintf(stderr, "gc import add %q: %v\n", source, err) //nolint:errcheck
		return 1
	}

	name := nameOverride
	if name == "" {
		name = deriveImportName(source)
	}
	if name == "" {
		fmt.Fprintln(stderr, "gc import add: could not derive import name; use --name") //nolint:errcheck
		return 1
	}
	if _, exists := scope.imports[name]; exists {
		fmt.Fprintf(stderr, "gc import add: import %q already exists\n", name) //nolint:errcheck
		return 1
	}

	version := versionFlag
	if gitBacked {
		if hasRepositoryRefInSource(source) {
			fmt.Fprintf(stderr, "gc import add %q: embed refs in --version, not in the source URL\n", source) //nolint:errcheck
			return 1
		}
		if version == "" {
			version, err = defaultImportVersionForSource(source)
			if err != nil {
				fmt.Fprintf(stderr, "gc import add %q: %v\n", source, err) //nolint:errcheck
				return 1
			}
		}
	} else if version != "" {
		fmt.Fprintf(stderr, "gc import add %q: --version is only valid for git-backed imports\n", source) //nolint:errcheck
		return 1
	}

	scope.imports[name] = config.Import{
		Source:  source,
		Version: version,
	}
	allImports, err := collectAllImportsFS(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import add %q: %v\n", source, err) //nolint:errcheck
		return 1
	}
	allImports[scope.syntheticKey(name)] = scope.imports[name]
	lock, err := syncImports(cityPath, allImports, packman.InstallResolveIfNeeded)
	if err != nil {
		fmt.Fprintf(stderr, "gc import add %q: %v\n", source, err) //nolint:errcheck
		return 1
	}
	if err := scope.save(); err != nil {
		fmt.Fprintf(stderr, "gc import add %q: %v\n", source, err) //nolint:errcheck
		return 1
	}
	if err := writeImportLockfile(fs, cityPath, lock); err != nil {
		fmt.Fprintf(stderr, "gc import add %q: %v\n", source, err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "Added import %q from %s\n", name, source) //nolint:errcheck
	return 0
}

//nolint:unparam // FS seam is intentional for command tests and symmetry with doImportAdd.
func doImportRemove(fs fsys.FS, cityPath, name string, stdout, stderr io.Writer) int {
	scope, err := loadImportScopeFS(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import remove: %v\n", err) //nolint:errcheck
		return 1
	}
	if _, exists := scope.imports[name]; !exists {
		fmt.Fprintf(stderr, "gc import remove: import %q not found\n", name) //nolint:errcheck
		return 1
	}
	delete(scope.imports, name)

	allImports, err := collectAllImportsFS(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import remove %q: %v\n", name, err) //nolint:errcheck
		return 1
	}
	delete(allImports, scope.syntheticKey(name))
	lock, err := syncImports(cityPath, allImports, packman.InstallResolveIfNeeded)
	if err != nil {
		fmt.Fprintf(stderr, "gc import remove %q: %v\n", name, err) //nolint:errcheck
		return 1
	}
	if err := scope.save(); err != nil {
		fmt.Fprintf(stderr, "gc import remove %q: %v\n", name, err) //nolint:errcheck
		return 1
	}
	if err := writeImportLockfile(fs, cityPath, lock); err != nil {
		fmt.Fprintf(stderr, "gc import remove %q: %v\n", name, err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "Removed import %q\n", name) //nolint:errcheck
	return 0
}

func doImportInstall(cityPath string, stdout, stderr io.Writer) int {
	allImports, err := collectAllImportsFS(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import install: %v\n", err) //nolint:errcheck
		return 1
	}
	lock, err := syncImports(cityPath, allImports, packman.InstallResolveIfNeeded)
	if err != nil {
		fmt.Fprintf(stderr, "gc import install: %v\n", err) //nolint:errcheck
		return 1
	}
	if err := writeImportLockfile(fsys.OSFS{}, cityPath, lock); err != nil {
		fmt.Fprintf(stderr, "gc import install: %v\n", err) //nolint:errcheck
		return 1
	}

	lock, err = installLockedImports(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import install: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "Installed %d remote import(s)\n", len(lock.Packs)) //nolint:errcheck
	return 0
}

func doImportUpgrade(cityPath, target string, stdout, stderr io.Writer) int {
	scope, err := loadImportScopeFS(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import upgrade: %v\n", err) //nolint:errcheck
		return 1
	}

	allImports, collectErr := collectAllImportsFS(fsys.OSFS{}, cityPath)
	if collectErr != nil {
		fmt.Fprintf(stderr, "gc import upgrade: %v\n", collectErr) //nolint:errcheck
		return 1
	}

	var lock *packman.Lockfile
	if target == "" {
		lock, err = syncImports(cityPath, allImports, packman.InstallUpgrade)
	} else {
		targetImp, ok := scope.imports[target]
		if !ok {
			fmt.Fprintf(stderr, "gc import upgrade: import %q not found\n", target) //nolint:errcheck
			return 1
		}
		if !isRemoteImportSource(targetImp.Source) {
			fmt.Fprintf(stderr, "gc import upgrade: import %q is a path import and cannot be upgraded\n", target) //nolint:errcheck
			return 1
		}
		lock, err = syncImportsSelective(cityPath, allImports, map[string]struct{}{
			targetImp.Source: {},
		})
		if err != nil {
			fmt.Fprintf(stderr, "gc import upgrade %q: %v\n", target, err) //nolint:errcheck
			return 1
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc import upgrade: %v\n", err) //nolint:errcheck
		return 1
	}
	if err := writeImportLockfile(fsys.OSFS{}, cityPath, lock); err != nil {
		fmt.Fprintf(stderr, "gc import upgrade: %v\n", err) //nolint:errcheck
		return 1
	}
	if target == "" {
		fmt.Fprintf(stdout, "Upgraded %d remote import(s)\n", len(lock.Packs)) //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "Upgraded import %q\n", target) //nolint:errcheck
	}
	return 0
}

func doImportList(cityPath string, tree bool, stdout, stderr io.Writer) int {
	scope, err := loadImportScopeFS(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import list: %v\n", err) //nolint:errcheck
		return 1
	}
	lock, err := readImportLockfile(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import list: %v\n", err) //nolint:errcheck
		return 1
	}
	var directNames []string
	for name := range scope.imports {
		directNames = append(directNames, name)
	}
	sort.Strings(directNames)
	if tree {
		if err := writeImportTree(stdout, scope.imports, lock); err != nil {
			fmt.Fprintf(stderr, "gc import list: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}

	allImports, err := collectAllImportsFS(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import list: %v\n", err) //nolint:errcheck
		return 1
	}
	allowLockOnlyFallback := len(allImports) == len(scope.imports)

	graph, graphErr := buildImportGraph(scope.imports, lock)
	if graphErr != nil && !allowLockOnlyFallback {
		fmt.Fprintf(stderr, "gc import list: %v\n", graphErr) //nolint:errcheck
		return 1
	}

	directSources := make(map[string]bool)
	for _, name := range directNames {
		imp := scope.imports[name]
		if !isRemoteImportSource(imp.Source) {
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", name, imp.Source, imp.Version, "(path)") //nolint:errcheck
			continue
		}
		directSources[imp.Source] = true
		pack, ok := lock.Packs[imp.Source]
		if !ok {
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", name, imp.Source, imp.Version, "(unlocked)") //nolint:errcheck
			continue
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", name, imp.Source, imp.Version, pack.Version) //nolint:errcheck
	}

	transitive := collectTransitiveImports(graph, directSources)
	if len(transitive) == 0 && allowLockOnlyFallback {
		for source, pack := range lock.Packs {
			if !directSources[source] {
				transitive[source] = pack
			}
		}
	}
	transitiveSources := make([]string, 0, len(transitive))
	for source := range transitive {
		transitiveSources = append(transitiveSources, source)
	}
	sort.Strings(transitiveSources)
	for _, source := range transitiveSources {
		pack := transitive[source]
		fmt.Fprintf(stdout, "(transitive)\t%s\t\t%s\n", source, pack.Version) //nolint:errcheck
	}
	return 0
}

func doImportWhy(cityPath, target string, stdout, stderr io.Writer) int {
	scope, err := loadImportScopeFS(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import why: %v\n", err) //nolint:errcheck
		return 1
	}
	lock, err := readImportLockfile(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import why: %v\n", err) //nolint:errcheck
		return 1
	}
	graph, err := buildImportGraph(scope.imports, lock)
	if err != nil {
		fmt.Fprintf(stderr, "gc import why: %v\n", err) //nolint:errcheck
		return 1
	}

	matches, err := findImportWhyMatches(graph, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc import why: %v\n", err) //nolint:errcheck
		return 1
	}
	if err := writeImportWhy(stdout, target, matches); err != nil {
		fmt.Fprintf(stderr, "gc import why: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

func writeImportTree(stdout io.Writer, imports map[string]config.Import, lock *packman.Lockfile) error {
	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)

	seen := make(map[string]bool)
	for _, name := range names {
		imp := imports[name]
		if err := writeImportTreeNode(stdout, name, imp, lock, "", true, seen); err != nil {
			return err
		}
	}
	return nil
}

func writeImportTreeNode(stdout io.Writer, name string, imp config.Import, lock *packman.Lockfile, prefix string, direct bool, seen map[string]bool) error {
	line := name
	if isRemoteImportSource(imp.Source) {
		pack, ok := lock.Packs[imp.Source]
		if !ok {
			line += fmt.Sprintf(" (unlocked) - %s", imp.Source)
			_, err := fmt.Fprintln(stdout, prefix+line)
			return err
		}
		if imp.Version != "" {
			line += fmt.Sprintf(" %s (%s) - %s", pack.Version, imp.Version, imp.Source)
		} else {
			line += fmt.Sprintf(" %s - %s", pack.Version, imp.Source)
		}
		if !direct && seen[imp.Source] {
			_, err := fmt.Fprintln(stdout, prefix+line)
			return err
		}
		seen[imp.Source] = true
		_, err := fmt.Fprintln(stdout, prefix+line)
		if err != nil {
			return err
		}
		if !imp.ImportIsTransitive() {
			return nil
		}

		children, err := packman.ReadCachedPackImports(imp.Source, pack.Commit)
		if err != nil {
			return err
		}
		childNames := make([]string, 0, len(children))
		for childName := range children {
			childNames = append(childNames, childName)
		}
		sort.Strings(childNames)
		for _, childName := range childNames {
			if err := writeImportTreeNode(stdout, childName, children[childName], lock, prefix+"  ", false, seen); err != nil {
				return err
			}
		}
		return nil
	}

	line += fmt.Sprintf(" (path) - %s", imp.Source)
	_, err := fmt.Fprintln(stdout, prefix+line)
	return err
}

type importGraphNode struct {
	Name       string
	Import     config.Import
	Resolved   packman.LockedPack
	HasLock    bool
	Children   []*importGraphNode
	directRoot bool
}

func buildImportGraph(imports map[string]config.Import, lock *packman.Lockfile) ([]*importGraphNode, error) {
	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)

	nodes := make([]*importGraphNode, 0, len(names))
	for _, name := range names {
		node, err := buildImportGraphNode(name, imports[name], lock, map[string]bool{}, true)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func buildImportGraphNode(name string, imp config.Import, lock *packman.Lockfile, stack map[string]bool, direct bool) (*importGraphNode, error) {
	node := &importGraphNode{
		Name:       name,
		Import:     imp,
		directRoot: direct,
	}
	if !isRemoteImportSource(imp.Source) {
		return node, nil
	}
	pack, ok := lock.Packs[imp.Source]
	if !ok {
		return node, nil
	}
	node.Resolved = pack
	node.HasLock = true
	if !imp.ImportIsTransitive() || stack[imp.Source] {
		return node, nil
	}

	children, err := packman.ReadCachedPackImports(imp.Source, pack.Commit)
	if err != nil {
		return nil, err
	}
	childNames := make([]string, 0, len(children))
	for childName := range children {
		childNames = append(childNames, childName)
	}
	sort.Strings(childNames)

	nextStack := make(map[string]bool, len(stack)+1)
	for key, value := range stack {
		nextStack[key] = value
	}
	nextStack[imp.Source] = true

	node.Children = make([]*importGraphNode, 0, len(childNames))
	for _, childName := range childNames {
		child, err := buildImportGraphNode(childName, children[childName], lock, nextStack, false)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, child)
	}
	return node, nil
}

func collectTransitiveImports(nodes []*importGraphNode, directSources map[string]bool) map[string]packman.LockedPack {
	transitive := make(map[string]packman.LockedPack)
	var walk func(node *importGraphNode)
	walk = func(node *importGraphNode) {
		if node == nil {
			return
		}
		if node.HasLock && !directSources[node.Import.Source] {
			transitive[node.Import.Source] = node.Resolved
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	for _, node := range nodes {
		for _, child := range node.Children {
			walk(child)
		}
	}
	return transitive
}

func findImportWhyMatches(nodes []*importGraphNode, target string) ([][]*importGraphNode, error) {
	var sourceMatches [][]*importGraphNode
	var nameMatches [][]*importGraphNode
	var walk func(node *importGraphNode, path []*importGraphNode)
	walk = func(node *importGraphNode, path []*importGraphNode) {
		if node == nil {
			return
		}
		path = append(path, node)
		if node.Import.Source == target {
			sourceMatches = append(sourceMatches, append([]*importGraphNode(nil), path...))
		}
		if node.Name == target {
			nameMatches = append(nameMatches, append([]*importGraphNode(nil), path...))
		}
		for _, child := range node.Children {
			walk(child, path)
		}
	}
	for _, node := range nodes {
		walk(node, nil)
	}

	if len(sourceMatches) > 0 {
		return sourceMatches, nil
	}
	if len(nameMatches) == 0 {
		return nil, fmt.Errorf("import %q not found", target)
	}

	sources := make(map[string]bool)
	for _, path := range nameMatches {
		last := path[len(path)-1]
		sources[last.Import.Source] = true
	}
	if len(sources) > 1 {
		options := make([]string, 0, len(sources))
		for source := range sources {
			options = append(options, source)
		}
		sort.Strings(options)
		return nil, fmt.Errorf("import %q is ambiguous; use one of: %s", target, strings.Join(options, ", "))
	}

	return nameMatches, nil
}

func writeImportWhy(stdout io.Writer, target string, matches [][]*importGraphNode) error {
	if len(matches) == 0 {
		return fmt.Errorf("no matches")
	}
	primary := matches[0][len(matches[0])-1]
	label := target
	if primary.Name != "" && target == primary.Import.Source {
		label = primary.Import.Source
	} else if primary.Name != "" {
		label = primary.Name
	}

	direct := false
	for _, path := range matches {
		if len(path) == 1 {
			direct = true
			break
		}
	}

	if direct {
		if _, err := fmt.Fprintf(stdout, "%s is a direct import\n", label); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(stdout, "%s is present transitively\n", label); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(stdout, "source: %s\n", primary.Import.Source); err != nil {
		return err
	}
	if primary.Import.Version != "" {
		if _, err := fmt.Fprintf(stdout, "constraint: %s\n", primary.Import.Version); err != nil {
			return err
		}
	}
	if primary.HasLock {
		if _, err := fmt.Fprintf(stdout, "resolved: %s\n", primary.Resolved.Version); err != nil {
			return err
		}
	}
	for _, path := range matches {
		if len(path) <= 1 {
			continue
		}
		names := make([]string, 0, len(path))
		for _, node := range path {
			names = append(names, node.Name)
		}
		if _, err := fmt.Fprintf(stdout, "via: %s\n", strings.Join(names, " -> ")); err != nil {
			return err
		}
	}
	return nil
}

func loadCityPackManifestFS(fs fsys.FS, cityPath string) (*cityPackManifest, error) {
	path := filepath.Join(cityPath, "pack.toml")
	data, err := fs.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		manifest := &cityPackManifest{
			Pack: config.PackMeta{
				Name:   defaultCityPackName(fs, cityPath),
				Schema: cityPackSchema,
			},
			Imports: make(map[string]config.Import),
		}
		return manifest, nil
	}

	var manifest cityPackManifest
	if _, err := toml.Decode(string(data), &manifest); err != nil {
		return nil, fmt.Errorf("parsing pack.toml: %w", err)
	}
	if manifest.Pack.Name == "" {
		manifest.Pack.Name = defaultCityPackName(fs, cityPath)
	}
	if manifest.Pack.Schema == 0 {
		manifest.Pack.Schema = cityPackSchema
	}
	if manifest.Imports == nil {
		manifest.Imports = make(map[string]config.Import)
	}
	if len(manifest.Defaults.Rig.Imports) > 0 {
		ordered, err := config.LoadRootPackDefaultRigImports(fs, cityPath)
		if err != nil {
			return nil, err
		}
		manifest.DefaultRigImportOrder = make([]string, 0, len(ordered))
		for _, bound := range ordered {
			manifest.DefaultRigImportOrder = append(manifest.DefaultRigImportOrder, bound.Binding)
		}
	}
	return &manifest, nil
}

func writeCityPackManifest(fs fsys.FS, cityPath string, manifest *cityPackManifest) error {
	if manifest == nil {
		manifest = &cityPackManifest{}
	}
	if manifest.Pack.Name == "" {
		manifest.Pack.Name = defaultCityPackName(fs, cityPath)
	}
	if manifest.Pack.Schema == 0 {
		manifest.Pack.Schema = cityPackSchema
	}
	if manifest.Imports == nil {
		manifest.Imports = make(map[string]config.Import)
	}

	var buf bytes.Buffer
	body := cityPackManifestBody{
		Pack:          manifest.Pack,
		Imports:       manifest.Imports,
		AgentDefaults: manifest.AgentDefaults,
		Agents:        manifest.Agents,
		NamedSessions: manifest.NamedSessions,
		Services:      manifest.Services,
		Providers:     manifest.Providers,
		Formulas:      manifest.Formulas,
		Patches:       manifest.Patches,
		Doctor:        manifest.Doctor,
		Commands:      manifest.Commands,
		Global:        manifest.Global,
	}
	if err := toml.NewEncoder(&buf).Encode(body); err != nil {
		return fmt.Errorf("encoding pack.toml: %w", err)
	}
	if err := writeOrderedDefaultRigImports(&buf, manifest); err != nil {
		return err
	}
	return fsys.WriteFileAtomic(fs, filepath.Join(cityPath, "pack.toml"), buf.Bytes(), 0o644)
}

func writeOrderedDefaultRigImports(buf *bytes.Buffer, manifest *cityPackManifest) error {
	if manifest == nil || len(manifest.Defaults.Rig.Imports) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(manifest.Defaults.Rig.Imports))
	names := make([]string, 0, len(manifest.Defaults.Rig.Imports))
	for _, name := range manifest.DefaultRigImportOrder {
		if _, ok := manifest.Defaults.Rig.Imports[name]; ok && !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	var remaining []string
	for name := range manifest.Defaults.Rig.Imports {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	names = append(names, remaining...)

	for _, name := range names {
		imp := manifest.Defaults.Rig.Imports[name]
		fmt.Fprintf(buf, "\n[defaults.rig.imports.%s]\n", name) //nolint:errcheck
		if err := toml.NewEncoder(buf).Encode(imp); err != nil {
			return fmt.Errorf("encoding defaults.rig.imports.%s: %w", name, err)
		}
	}
	return nil
}

func defaultCityPackName(fs fsys.FS, cityPath string) string {
	cfg, err := config.Load(fs, filepath.Join(cityPath, "city.toml"))
	if err == nil {
		return config.EffectiveCityName(cfg, filepath.Base(cityPath))
	}
	return filepath.Base(cityPath)
}

func deriveImportName(source string) string {
	trimmed := strings.TrimSuffix(strings.TrimRight(source, "/"), ".git")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	if i := strings.LastIndex(trimmed, ":"); i >= 0 && !strings.Contains(trimmed, string(filepath.Separator)) {
		trimmed = trimmed[i+1:]
	}
	return trimmed
}

func isRemoteImportSource(source string) bool {
	return strings.HasPrefix(source, "git@") ||
		strings.HasPrefix(source, "ssh://") ||
		strings.HasPrefix(source, "https://") ||
		strings.HasPrefix(source, "http://") ||
		strings.HasPrefix(source, "file://") ||
		strings.HasPrefix(source, "github.com/")
}

func hasRepositoryRefInSource(source string) bool {
	if strings.Contains(source, "/tree/") {
		return true
	}
	if i := strings.Index(source, "://"); i >= 0 {
		return strings.Contains(source[i+3:], "#")
	}
	return strings.Contains(source, "#")
}

func defaultImportVersionForSource(source string) (string, error) {
	resolved, err := resolveImportVersion(source, "")
	if err == nil {
		return defaultImportConstraint(resolved.Version)
	}
	if !errors.Is(err, packman.ErrNoSemverTags) {
		return "", err
	}
	commit, err := resolveImportHeadCommit(source)
	if err != nil {
		return "", err
	}
	return "sha:" + commit, nil
}

func normalizeImportAddSource(fs fsys.FS, cityPath, source string) (string, bool, error) {
	if isRemoteImportSource(source) {
		return source, true, nil
	}

	targetDir, err := resolveImportAddPath(cityPath, source)
	if err != nil {
		return "", false, err
	}
	if err := validateImportPackTarget(fs, targetDir); err != nil {
		return "", false, err
	}

	canonical, ok, err := canonicalizeLocalGitImportSource(targetDir)
	if err != nil {
		return "", false, err
	}
	if ok {
		return canonical, true, nil
	}
	return source, false, nil
}

func resolveImportAddPath(cityPath, source string) (string, error) {
	switch {
	case strings.HasPrefix(source, "//"):
		return filepath.Join(cityPath, strings.TrimPrefix(source, "//")), nil
	case source == "~" || strings.HasPrefix(source, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home dir: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(source, "~/")), nil
	case filepath.IsAbs(source):
		return source, nil
	default:
		return filepath.Join(cityPath, source), nil
	}
}

func validateImportPackTarget(fs fsys.FS, targetDir string) error {
	info, err := fs.Stat(targetDir)
	if err != nil {
		return fmt.Errorf("resolving source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory")
	}
	packPath := filepath.Join(targetDir, "pack.toml")
	if _, err := fs.Stat(packPath); err != nil {
		return fmt.Errorf("invalid pack target: missing pack.toml")
	}
	if _, err := config.Load(fs, packPath); err != nil {
		return fmt.Errorf("invalid pack target: %w", err)
	}
	return nil
}

func canonicalizeLocalGitImportSource(targetDir string) (string, bool, error) {
	repoRoot, ok, err := localGitRepoRoot(targetDir)
	if err != nil || !ok {
		return "", ok, err
	}
	resolvedTarget, err := filepath.EvalSymlinks(targetDir)
	if err != nil {
		resolvedTarget = targetDir
	}
	rel, err := filepath.Rel(repoRoot, resolvedTarget)
	if err != nil {
		return "", false, fmt.Errorf("computing import subpath: %w", err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(repoRoot)}
	canonical := u.String()
	if rel != "." {
		canonical += "//" + filepath.ToSlash(rel)
	}
	return canonical, true, nil
}

func localGitRepoRoot(targetDir string) (string, bool, error) {
	cmd := exec.Command("git", "-C", targetDir, "rev-parse", "--show-toplevel")
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := string(out)
		if strings.Contains(text, "not a git repository") {
			return "", false, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("probing git target: %w", err)
	}
	return strings.TrimSpace(string(out)), true, nil
}

func defaultImportHeadCommit(source string) (string, error) {
	cloneURL := config.NormalizeRemoteSource(source)
	cmd := exec.Command("git", "ls-remote", cloneURL, "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolving HEAD for %q: %w", source, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("resolving HEAD for %q: empty response", source)
	}
	return fields[0], nil
}
