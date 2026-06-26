package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/packregistry"
	"github.com/gastownhall/gascity/internal/remotesource"
	"github.com/spf13/cobra"
)

func newPackReleaseCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Author pack registry release metadata",
		Long:  "Author pack registry release metadata, including canonical pack content hashes.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPackReleaseHashCmd(stdout, stderr))
	cmd.AddCommand(newPackReleaseVerifyCmd(stdout, stderr))
	cmd.AddCommand(newPackReleaseStampCmd(stdout, stderr))
	cmd.AddCommand(newPackReleaseValidateCmd(stdout, stderr))
	return cmd
}

func newPackReleaseHashCmd(stdout, stderr io.Writer) *cobra.Command {
	var packPath string
	var commit string
	cmd := &cobra.Command{
		Use:   "hash <source>",
		Short: "Compute a pack release content hash",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doPackReleaseHash(args[0], packPath, commit, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&packPath, "path", "", "pack path inside the source repository")
	cmd.Flags().StringVar(&commit, "commit", "", "git commit or ref to hash")
	_ = cmd.MarkFlagRequired("commit")
	return cmd
}

func newPackReleaseVerifyCmd(stdout, stderr io.Writer) *cobra.Command {
	var packPath string
	var commit string
	var hash string
	cmd := &cobra.Command{
		Use:   "verify <source>",
		Short: "Verify a pack release content hash",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doPackReleaseVerify(args[0], packPath, commit, hash, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&packPath, "path", "", "pack path inside the source repository")
	cmd.Flags().StringVar(&commit, "commit", "", "git commit or ref to verify")
	cmd.Flags().StringVar(&hash, "hash", "", "expected sha256:<64hex> content hash")
	_ = cmd.MarkFlagRequired("commit")
	_ = cmd.MarkFlagRequired("hash")
	return cmd
}

type packReleaseStampOptions struct {
	Version         string
	Ref             string
	Commit          string
	ReleaseDesc     string
	Source          string
	PackPath        string
	PackDescription string
	Replace         bool
}

func newPackReleaseStampCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts packReleaseStampOptions
	cmd := &cobra.Command{
		Use:   "stamp <registry.toml> <pack-name>",
		Short: "Stamp a registry release entry with a computed content hash",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if doPackReleaseStamp(args[0], args[1], opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Version, "version", "", "release version to stamp")
	cmd.Flags().StringVar(&opts.Ref, "ref", "", "release ref to record")
	cmd.Flags().StringVar(&opts.Commit, "commit", "", "git commit or ref to hash and record")
	cmd.Flags().StringVar(&opts.ReleaseDesc, "description", "", "release description")
	cmd.Flags().StringVar(&opts.Source, "source", "", "pack source; required when creating a new [[pack]]")
	cmd.Flags().StringVar(&opts.PackPath, "path", "", "pack path inside the source repository")
	cmd.Flags().StringVar(&opts.PackDescription, "pack-description", "", "pack description; required when creating a new [[pack]]")
	cmd.Flags().BoolVar(&opts.Replace, "replace", false, "replace an existing release with the same version")
	_ = cmd.MarkFlagRequired("version")
	_ = cmd.MarkFlagRequired("ref")
	_ = cmd.MarkFlagRequired("commit")
	_ = cmd.MarkFlagRequired("description")
	return cmd
}

func newPackReleaseValidateCmd(stdout, stderr io.Writer) *cobra.Command {
	var packName string
	var includeWithdrawn bool
	cmd := &cobra.Command{
		Use:   "validate <registry.toml>",
		Short: "Validate registry release content hashes",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doPackReleaseValidate(args[0], packName, includeWithdrawn, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&packName, "pack", "", "validate only one registry pack")
	cmd.Flags().BoolVar(&includeWithdrawn, "include-withdrawn", false, "also validate withdrawn releases")
	return cmd
}

func doPackReleaseHash(source, packPath, commit string, stdout, stderr io.Writer) int {
	resolved, err := resolvePackReleaseSource(source, packPath, commit)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack release hash: %v\n", err) //nolint:errcheck
		return 1
	}
	hash, err := packregistry.PackContentHash(resolved.RepoDir, resolved.Commit, resolved.PackPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack release hash: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stdout, hash) //nolint:errcheck
	return 0
}

func doPackReleaseVerify(source, packPath, commit, hash string, stdout, stderr io.Writer) int {
	resolved, err := resolvePackReleaseSource(source, packPath, commit)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack release verify: %v\n", err) //nolint:errcheck
		return 1
	}
	if err := packregistry.VerifyPackContentHash(resolved.RepoDir, resolved.Commit, resolved.PackPath, hash); err != nil {
		fmt.Fprintf(stderr, "gc pack release verify: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintln(stdout, "ok") //nolint:errcheck
	return 0
}

func doPackReleaseStamp(registryPath, packName string, opts packReleaseStampOptions, stdout, stderr io.Writer) int {
	if err := stampPackRelease(registryPath, packName, opts); err != nil {
		fmt.Fprintf(stderr, "gc pack release stamp: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "stamped %s %s in %s\n", packName, opts.Version, registryPath) //nolint:errcheck
	return 0
}

func doPackReleaseValidate(registryPath, packName string, includeWithdrawn bool, stdout, stderr io.Writer) int {
	checked, err := validatePackReleaseHashes(registryPath, packName, includeWithdrawn)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack release validate: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "registry release hashes ok (%d checked)\n", checked) //nolint:errcheck
	return 0
}

type resolvedPackReleaseSource struct {
	RepoDir  string
	PackPath string
	Commit   string
}

func resolvePackReleaseSource(source, packPath, commit string) (resolvedPackReleaseSource, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return resolvedPackReleaseSource{}, fmt.Errorf("source is required")
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return resolvedPackReleaseSource{}, fmt.Errorf("commit is required")
	}
	if remotesource.IsRemote(source) {
		parsed := remotesource.Parse(source)
		resolvedPackPath, err := normalizePackReleasePath(packPath)
		if err != nil {
			return resolvedPackReleaseSource{}, err
		}
		if resolvedPackPath == "" {
			resolvedPackPath, err = normalizePackReleasePath(parsed.Subpath)
			if err != nil {
				return resolvedPackReleaseSource{}, err
			}
		}
		repoDir, err := ensurePackReleaseRepoInCache(parsed.CloneURL, commit)
		if err != nil {
			return resolvedPackReleaseSource{}, err
		}
		resolvedCommit, err := packregistry.ResolveGitCommit(repoDir, commit)
		if err != nil {
			return resolvedPackReleaseSource{}, err
		}
		return resolvedPackReleaseSource{RepoDir: repoDir, PackPath: resolvedPackPath, Commit: resolvedCommit}, nil
	}
	repoDir, inferredPackPath, err := resolveLocalPackReleaseSource(source, packPath)
	if err != nil {
		return resolvedPackReleaseSource{}, err
	}
	resolvedCommit, err := packregistry.ResolveGitCommit(repoDir, commit)
	if err != nil {
		return resolvedPackReleaseSource{}, err
	}
	return resolvedPackReleaseSource{RepoDir: repoDir, PackPath: inferredPackPath, Commit: resolvedCommit}, nil
}

func resolveLocalPackReleaseSource(source, packPath string) (repoDir, resolvedPackPath string, err error) {
	sourcePath := source
	if sourcePath == "~" || strings.HasPrefix(sourcePath, "~/") {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", "", fmt.Errorf("resolving home dir: %w", homeErr)
		}
		sourcePath = filepath.Join(home, strings.TrimPrefix(sourcePath, "~/"))
	}
	absSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return "", "", fmt.Errorf("resolving source path: %w", err)
	}
	// Resolve symlinks so filepath.Rel agrees with git's real-path repo root (macOS: /tmp -> /private/tmp).
	if resolved, evalErr := filepath.EvalSymlinks(absSource); evalErr == nil {
		absSource = resolved
	}
	repoDir, err = localGitRoot(absSource)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(packPath) != "" {
		resolvedPackPath, err := normalizePackReleasePath(packPath)
		if err != nil {
			return "", "", err
		}
		return repoDir, resolvedPackPath, nil
	}
	rel, err := filepath.Rel(repoDir, absSource)
	if err != nil {
		return "", "", fmt.Errorf("computing pack path: %w", err)
	}
	if rel == "." {
		return repoDir, "", nil
	}
	return repoDir, filepath.ToSlash(rel), nil
}

func localGitRoot(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	// Strip git-locating env vars (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, ...)
	// so the toplevel resolves from path, not a parent repo leaked through a
	// pre-commit hook or nested worktree tooling.
	cmd.Env = git.SanitizedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolving local git root for %q: %s: %w", path, strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func ensurePackReleaseRepoInCache(cloneURL, commit string) (string, error) {
	cloneURL = strings.TrimSpace(cloneURL)
	if cloneURL == "" {
		return "", fmt.Errorf("remote clone URL is required")
	}
	root, err := packReleaseRepoCacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("creating pack release repo cache: %w", err)
	}
	sum := sha256.Sum256([]byte(cloneURL + "\x00" + strings.TrimSpace(commit)))
	cachePath := filepath.Join(root, fmt.Sprintf("%x", sum[:]))
	if _, err := os.Stat(filepath.Join(cachePath, ".git")); err == nil {
		if err := runPackReleaseGitCommand(cachePath, "fetch", "--quiet", "origin"); err != nil {
			if removeErr := os.RemoveAll(cachePath); removeErr != nil {
				return "", fmt.Errorf("removing stale pack release repo cache after fetch failure: %w", removeErr)
			}
		} else if err := checkoutPackReleaseRepo(cachePath, commit); err == nil {
			return cachePath, nil
		} else if removeErr := os.RemoveAll(cachePath); removeErr != nil {
			return "", fmt.Errorf("removing stale pack release repo cache after checkout failure: %w", removeErr)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("checking pack release repo cache: %w", err)
	} else if err := os.RemoveAll(cachePath); err != nil {
		return "", fmt.Errorf("removing invalid pack release repo cache: %w", err)
	}
	if err := runPackReleaseGitCommand("", "clone", "--quiet", cloneURL, cachePath); err != nil {
		return "", fmt.Errorf("cloning %q: %w", cloneURL, err)
	}
	if err := checkoutPackReleaseRepo(cachePath, commit); err != nil {
		return "", err
	}
	return cachePath, nil
}

func checkoutPackReleaseRepo(repoDir, commit string) error {
	if err := runPackReleaseGitCommand(repoDir, "checkout", "--quiet", commit); err != nil {
		return fmt.Errorf("checking out %q: %w", commit, err)
	}
	if err := runPackReleaseGitCommand(repoDir, "reset", "--hard", "--quiet", commit); err != nil {
		return fmt.Errorf("resetting pack release repo cache: %w", err)
	}
	if err := runPackReleaseGitCommand(repoDir, "clean", "-ffdx", "--quiet"); err != nil {
		return fmt.Errorf("cleaning pack release repo cache: %w", err)
	}
	return nil
}

func packReleaseRepoCacheRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".gc", "cache", "registry-release-repos"), nil
}

func runPackReleaseGitCommand(dir string, args ...string) error {
	cmdArgs := append([]string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.untrackedCache=false",
	}, args...)
	cmd := exec.Command("git", cmdArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Use the shared hermetic git environment so the isolated release-repo cache
	// commands run with the canonical blacklist plus the discovery/config
	// isolation and config pins, instead of a hand-maintained duplicate that can
	// silently drift from git.HermeticEnv().
	cmd.Env = git.HermeticEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

func normalizePackReleasePath(raw string) (string, error) {
	raw = strings.TrimSpace(filepath.ToSlash(raw))
	if raw == "" || raw == "." {
		return "", nil
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("pack path %q must be relative", raw)
	}
	clean := path.Clean(strings.Trim(raw, "/"))
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("pack path %q must stay within the repository", raw)
	}
	return clean, nil
}

func stampPackRelease(registryPath, packName string, opts packReleaseStampOptions) error {
	if err := packregistry.ValidatePackName(packName); err != nil {
		return err
	}
	if strings.TrimSpace(opts.Version) == "" {
		return fmt.Errorf("version is required")
	}
	if strings.TrimSpace(opts.Ref) == "" {
		return fmt.Errorf("ref is required")
	}
	if strings.TrimSpace(opts.ReleaseDesc) == "" {
		return fmt.Errorf("description is required")
	}
	catalog, err := readPackReleaseCatalog(registryPath)
	if err != nil {
		return err
	}
	catalog.Schema = packregistry.CatalogSchema
	packIndex := -1
	for i := range catalog.Packs {
		if catalog.Packs[i].Name == packName {
			packIndex = i
			break
		}
	}
	if packIndex == -1 {
		if strings.TrimSpace(opts.Source) == "" {
			return fmt.Errorf("source is required when creating pack %q", packName)
		}
		if strings.TrimSpace(opts.PackDescription) == "" {
			return fmt.Errorf("pack-description is required when creating pack %q", packName)
		}
		catalog.Packs = append(catalog.Packs, packregistry.CatalogPack{
			Name:        packName,
			Description: opts.PackDescription,
			Source:      opts.Source,
			SourceKind:  "git",
		})
		packIndex = len(catalog.Packs) - 1
	} else if strings.TrimSpace(opts.Source) != "" {
		catalog.Packs[packIndex].Source = opts.Source
	}
	pack := &catalog.Packs[packIndex]
	if strings.TrimSpace(pack.SourceKind) == "" {
		pack.SourceKind = "git"
	}
	resolved, err := resolvePackReleaseSource(pack.Source, opts.PackPath, opts.Commit)
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.PackPath) != "" {
		pack.Source = packReleaseCatalogSource(pack.Source, resolved.RepoDir, resolved.PackPath)
	}
	actualName, err := packregistry.PackNameAtCommit(resolved.RepoDir, resolved.Commit, resolved.PackPath)
	if err != nil {
		return err
	}
	if actualName != packName {
		return fmt.Errorf("pack name %q does not match %s/pack.toml name %q", packName, displayResolvedPackPath(resolved.PackPath), actualName)
	}
	hash, err := packregistry.PackContentHash(resolved.RepoDir, resolved.Commit, resolved.PackPath)
	if err != nil {
		return err
	}
	release := packregistry.CatalogRelease{
		Version:     strings.TrimSpace(opts.Version),
		Ref:         strings.TrimSpace(opts.Ref),
		Commit:      resolved.Commit,
		Hash:        hash,
		Description: strings.TrimSpace(opts.ReleaseDesc),
	}
	if err := upsertPackRelease(pack, release, opts.Replace); err != nil {
		return err
	}
	if err := packregistry.ValidateCatalog(catalog, false); err != nil {
		return err
	}
	return writePackReleaseCatalog(registryPath, catalog)
}

func upsertPackRelease(pack *packregistry.CatalogPack, release packregistry.CatalogRelease, replace bool) error {
	for i, existing := range pack.Releases {
		if existing.Version != release.Version {
			continue
		}
		if !replace && (existing.Ref != release.Ref || existing.Commit != release.Commit || existing.Hash != release.Hash) {
			return fmt.Errorf("release %s already exists for pack %q with different immutable metadata; use --replace to rewrite it", release.Version, pack.Name)
		}
		release.Withdrawn = existing.Withdrawn
		release.WithdrawnReason = existing.WithdrawnReason
		pack.Releases[i] = release
		return nil
	}
	pack.Releases = append(pack.Releases, release)
	return nil
}

func packReleaseCatalogSource(source, repoDir, packPath string) string {
	packPath = strings.Trim(strings.TrimSpace(filepath.ToSlash(packPath)), "/")
	if packPath == "" {
		return source
	}
	if remotesource.IsRemote(source) {
		return remotePackReleaseSource(remotesource.Parse(source), packPath)
	}
	return filepath.Join(repoDir, filepath.FromSlash(packPath))
}

func remotePackReleaseSource(parsed remotesource.Parsed, packPath string) string {
	source := strings.TrimRight(parsed.CloneURL, "/")
	if strings.TrimSpace(packPath) != "" {
		source += "//" + strings.Trim(strings.TrimSpace(filepath.ToSlash(packPath)), "/")
	}
	if parsed.Ref != "" {
		source += "#" + parsed.Ref
	}
	return source
}

func validatePackReleaseHashes(registryPath, packName string, includeWithdrawn bool) (int, error) {
	catalog, err := readPackReleaseCatalog(registryPath)
	if err != nil {
		return 0, err
	}
	if err := packregistry.ValidateCatalog(catalog, false); err != nil {
		return 0, err
	}
	if strings.TrimSpace(packName) != "" {
		if err := packregistry.ValidatePackName(packName); err != nil {
			return 0, err
		}
	}
	checked := 0
	foundPack := packName == ""
	for _, pack := range catalog.Packs {
		if packName != "" && pack.Name != packName {
			continue
		}
		foundPack = true
		for _, release := range pack.Releases {
			if release.Withdrawn && !includeWithdrawn {
				continue
			}
			resolved, err := resolvePackReleaseSource(pack.Source, "", release.Commit)
			if err != nil {
				return checked, fmt.Errorf("%s %s: %w", pack.Name, release.Version, err)
			}
			if err := packregistry.VerifyPackContentHash(resolved.RepoDir, resolved.Commit, resolved.PackPath, release.Hash); err != nil {
				return checked, fmt.Errorf("%s %s: %w", pack.Name, release.Version, err)
			}
			checked++
		}
	}
	if !foundPack {
		return checked, fmt.Errorf("pack %q not found", packName)
	}
	return checked, nil
}

func readPackReleaseCatalog(registryPath string) (packregistry.Catalog, error) {
	data, err := os.ReadFile(registryPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return packregistry.Catalog{Schema: packregistry.CatalogSchema}, nil
		}
		return packregistry.Catalog{}, fmt.Errorf("reading %s: %w", registryPath, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return packregistry.Catalog{Schema: packregistry.CatalogSchema}, nil
	}
	catalog, err := packregistry.ParseCatalog(data)
	if err != nil {
		return packregistry.Catalog{}, err
	}
	return catalog, nil
}

func writePackReleaseCatalog(registryPath string, catalog packregistry.Catalog) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(catalog); err != nil {
		return fmt.Errorf("encoding registry catalog: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(registryPath), 0o755); err != nil {
		return fmt.Errorf("creating registry directory: %w", err)
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, registryPath, buf.Bytes(), 0o644)
}

func displayResolvedPackPath(packPath string) string {
	if strings.TrimSpace(packPath) == "" {
		return "."
	}
	return packPath
}
