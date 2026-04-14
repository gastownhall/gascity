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
	installLockedImports    = packman.InstallLocked
	readImportLockfile      = packman.ReadLockfile
	writeImportLockfile     = packman.WriteLockfile
	resolveImportVersion    = packman.ResolveVersion
	defaultImportConstraint = packman.DefaultConstraint
	resolveImportHeadCommit = defaultImportHeadCommit
)

const cityPackSchema = 1

type cityPackManifest struct {
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
		Short: "Install imports from packs.lock",
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

//nolint:unparam // keep fs injectable for parity with the other import helpers and direct tests.
func doImportAdd(fs fsys.FS, cityPath, source, nameOverride, versionFlag string, stdout, stderr io.Writer) int {
	manifest, err := loadCityPackManifestFS(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import add: %v\n", err) //nolint:errcheck
		return 1
	}
	if manifest.Imports == nil {
		manifest.Imports = make(map[string]config.Import)
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
	if _, exists := manifest.Imports[name]; exists {
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

	manifest.Imports[name] = config.Import{
		Source:  source,
		Version: version,
	}
	lock, err := syncImports(cityPath, manifest.Imports, packman.InstallResolveIfNeeded)
	if err != nil {
		fmt.Fprintf(stderr, "gc import add %q: %v\n", source, err) //nolint:errcheck
		return 1
	}
	if err := writeCityPackManifest(fs, cityPath, manifest); err != nil {
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

func doImportRemove(fs fsys.FS, cityPath, name string, stdout, stderr io.Writer) int {
	manifest, err := loadCityPackManifestFS(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import remove: %v\n", err) //nolint:errcheck
		return 1
	}
	if _, exists := manifest.Imports[name]; !exists {
		fmt.Fprintf(stderr, "gc import remove: import %q not found\n", name) //nolint:errcheck
		return 1
	}
	delete(manifest.Imports, name)

	lock, err := syncImports(cityPath, manifest.Imports, packman.InstallResolveIfNeeded)
	if err != nil {
		fmt.Fprintf(stderr, "gc import remove %q: %v\n", name, err) //nolint:errcheck
		return 1
	}
	if err := writeCityPackManifest(fs, cityPath, manifest); err != nil {
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
	manifest, err := loadCityPackManifestFS(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import install: %v\n", err) //nolint:errcheck
		return 1
	}
	lock, err := syncImports(cityPath, manifest.Imports, packman.InstallFromLock)
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
	manifest, err := loadCityPackManifestFS(fsys.OSFS{}, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import upgrade: %v\n", err) //nolint:errcheck
		return 1
	}

	var lock *packman.Lockfile
	if target == "" {
		lock, err = syncImports(cityPath, manifest.Imports, packman.InstallUpgrade)
	} else {
		targetImp, ok := manifest.Imports[target]
		if !ok {
			fmt.Fprintf(stderr, "gc import upgrade: import %q not found\n", target) //nolint:errcheck
			return 1
		}
		if !isRemoteImportSource(targetImp.Source) {
			fmt.Fprintf(stderr, "gc import upgrade: import %q is a path import and cannot be upgraded\n", target) //nolint:errcheck
			return 1
		}
		upgraded, err := syncImports(cityPath, map[string]config.Import{target: targetImp}, packman.InstallUpgrade)
		if err != nil {
			fmt.Fprintf(stderr, "gc import upgrade %q: %v\n", target, err) //nolint:errcheck
			return 1
		}
		remaining := make(map[string]config.Import)
		for name, imp := range manifest.Imports {
			if name == target {
				continue
			}
			remaining[name] = imp
		}
		preserved, err := syncImports(cityPath, remaining, packman.InstallResolveIfNeeded)
		if err != nil {
			fmt.Fprintf(stderr, "gc import upgrade %q: %v\n", target, err) //nolint:errcheck
			return 1
		}
		lock = &packman.Lockfile{Schema: packman.LockfileSchema, Packs: make(map[string]packman.LockedPack)}
		for source, pack := range preserved.Packs {
			lock.Packs[source] = pack
		}
		for source, pack := range upgraded.Packs {
			lock.Packs[source] = pack
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
	manifest, err := loadCityPackManifestFS(fsys.OSFS{}, cityPath)
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
	for name := range manifest.Imports {
		directNames = append(directNames, name)
	}
	sort.Strings(directNames)
	if tree {
		if err := writeImportTree(stdout, manifest.Imports, lock); err != nil {
			fmt.Fprintf(stderr, "gc import list: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	directSources := make(map[string]bool)
	for _, name := range directNames {
		imp := manifest.Imports[name]
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

	var transitiveSources []string
	for source := range lock.Packs {
		if !directSources[source] {
			transitiveSources = append(transitiveSources, source)
		}
	}
	sort.Strings(transitiveSources)
	for _, source := range transitiveSources {
		pack := lock.Packs[source]
		fmt.Fprintf(stdout, "(transitive)\t%s\t\t%s\n", source, pack.Version) //nolint:errcheck
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
	if err := toml.NewEncoder(&buf).Encode(manifest); err != nil {
		return fmt.Errorf("encoding pack.toml: %w", err)
	}
	return fsys.WriteFileAtomic(fs, filepath.Join(cityPath, "pack.toml"), buf.Bytes(), 0o644)
}

func defaultCityPackName(fs fsys.FS, cityPath string) string {
	cfg, err := config.Load(fs, filepath.Join(cityPath, "city.toml"))
	if err == nil {
		if cfg.Workspace.Name != "" {
			return cfg.Workspace.Name
		}
		if cfg.ResolvedWorkspaceName != "" {
			return cfg.ResolvedWorkspaceName
		}
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
