// Package bootstrap installs user-global bootstrap packs used by implicit imports.
package bootstrap

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
)

const implicitImportSchema = 1

//go:embed packs/**
var embeddedBootstrapPacks embed.FS

var bootstrapAssets fs.FS = embeddedBootstrapPacks

// Entry describes a pack that gc init bootstraps into the global cache.
type Entry struct {
	Name     string
	Source   string
	Version  string
	AssetDir string
}

// BootstrapPacks is the hardcoded set of implicit packs bootstrapped by gc init.
var BootstrapPacks = []Entry{
	{Name: "import", Source: "github.com/gastownhall/gc-import", Version: "0.2.0", AssetDir: "packs/import"},
	{Name: "registry", Source: "github.com/gastownhall/gc-registry", Version: "0.1.0", AssetDir: "packs/registry"},
	{Name: "core", Source: "github.com/gastownhall/gc-core", Version: "0.1.0", AssetDir: "packs/core"},
}

type implicitImport struct {
	Source  string `toml:"source"`
	Version string `toml:"version"`
	Commit  string `toml:"commit"`
}

type implicitImportFile struct {
	Schema  int                       `toml:"schema"`
	Imports map[string]implicitImport `toml:"imports"`
}

// EnsureBootstrap populates the global cache and updates implicit-import.toml.
//
// This variant performs no city-level collision check and is retained for
// callers that have no loaded city config (e.g., doctor reconcile paths).
// gc init and gc import install should use EnsureBootstrapForCity to
// surface a hard error when the city's explicit [imports.<name>] would
// shadow a bootstrap pack.
func EnsureBootstrap(gcHome string) error {
	return EnsureBootstrapForCity(gcHome, nil)
}

// EnsureBootstrapForCity is EnsureBootstrap plus collision detection against
// the city's explicit imports map. If any bootstrap pack name collides with
// a user-declared [imports.<name>], it returns an error and does not write
// the implicit-import entry for the colliding bootstrap pack.
//
// Pass a nil or empty userImports map to disable collision detection (the
// historical behavior).
func EnsureBootstrapForCity(gcHome string, userImports map[string]config.Import) error {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("GC_BOOTSTRAP")), "skip") {
		return nil
	}
	if strings.TrimSpace(gcHome) == "" {
		gcHome = defaultGCHome()
	}
	if strings.TrimSpace(gcHome) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(gcHome, "cache", "repos"), 0o755); err != nil {
		return fmt.Errorf("creating bootstrap cache root: %w", err)
	}

	implicitPath := filepath.Join(gcHome, "implicit-import.toml")
	imports, err := readImplicitFile(implicitPath)
	if err != nil {
		return err
	}
	updated := false

	// Collision check across ALL bootstrap entries so the user sees every
	// conflict in one error rather than fixing them one at a time. The
	// spec requires gc init / gc import install to refuse writing
	// bootstrap implicit-import entries when the loading city already
	// declares [imports.<name>] — silent shadowing would replace the
	// user's pack content on upgrade.
	if collisions := CollidesWithBootstrapPack(userImports, PackNames()); len(collisions) > 0 {
		quoted := make([]string, len(collisions))
		for i, name := range collisions {
			quoted[i] = fmt.Sprintf("%q", name)
		}
		return fmt.Errorf(
			"gc init: cannot add implicit import(s) %s — conflicts with city's [imports.<name>] of the same name; rename one side",
			strings.Join(quoted, ", "),
		)
	}

	for _, entry := range BootstrapPacks {
		commit, err := bootstrapPackRevision(entry)
		if err != nil {
			return fmt.Errorf("bootstrapping %q: %w", entry.Name, err)
		}

		cacheDir := config.GlobalRepoCachePath(gcHome, entry.Source, commit)
		if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); err != nil {
			if err := materializeBootstrapPack(cacheDir, entry); err != nil {
				return fmt.Errorf("bootstrapping %q: %w", entry.Name, err)
			}
		}

		next := implicitImport{
			Source:  entry.Source,
			Version: entry.Version,
			Commit:  commit,
		}
		if imports[entry.Name] != next {
			imports[entry.Name] = next
			updated = true
		}
	}

	if updated {
		if err := writeImplicitFile(implicitPath, imports); err != nil {
			return err
		}
	}
	return nil
}

func defaultGCHome() string {
	if v := strings.TrimSpace(os.Getenv("GC_HOME")); v != "" {
		return v
	}
	if strings.HasSuffix(os.Args[0], ".test") {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".gc")
	}
	return filepath.Join(home, ".gc")
}

func bootstrapPackRevision(entry Entry) (string, error) {
	paths, err := collectAssetFiles(entry.AssetDir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, rel := range paths {
		data, err := fs.ReadFile(bootstrapAssets, pathpkg.Join(entry.AssetDir, rel))
		if err != nil {
			return "", err
		}
		h.Write([]byte(rel)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})   //nolint:errcheck // hash.Write never errors
		h.Write(data)        //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})   //nolint:errcheck // hash.Write never errors
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func materializeBootstrapPack(cacheDir string, entry Entry) error {
	stageDir, err := os.MkdirTemp(filepath.Dir(cacheDir), filepath.Base(cacheDir)+".tmp-")
	if err != nil {
		return fmt.Errorf("creating bootstrap stage dir: %w", err)
	}
	_ = os.RemoveAll(stageDir)
	if err := copyEmbeddedTree(entry.AssetDir, stageDir); err != nil {
		_ = os.RemoveAll(stageDir)
		return err
	}
	if _, err := os.Stat(filepath.Join(stageDir, "pack.toml")); err != nil {
		_ = os.RemoveAll(stageDir)
		return fmt.Errorf("embedded bootstrap pack %q is missing pack.toml", entry.AssetDir)
	}
	if err := os.Rename(stageDir, cacheDir); err != nil {
		_ = os.RemoveAll(stageDir)
		if _, statErr := os.Stat(filepath.Join(cacheDir, "pack.toml")); statErr == nil {
			return nil
		}
		return fmt.Errorf("moving bootstrap pack into cache: %w", err)
	}
	return nil
}

func collectAssetFiles(root string) ([]string, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("bootstrap asset directory is required")
	}
	var paths []string
	if _, err := fs.Stat(bootstrapAssets, root); err != nil {
		return nil, fmt.Errorf("reading embedded bootstrap pack %q: %w", root, err)
	}
	err := fs.WalkDir(bootstrapAssets, root, func(assetPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := assetRel(root, assetPath)
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if !containsString(paths, "pack.toml") {
		return nil, fmt.Errorf("embedded bootstrap pack %q is missing pack.toml", root)
	}
	return paths, nil
}

func copyEmbeddedTree(root, dst string) error {
	return fs.WalkDir(bootstrapAssets, root, func(assetPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := assetRel(root, assetPath)
		target := dst
		if rel != "." {
			target = filepath.Join(dst, rel)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		data, err := fs.ReadFile(bootstrapAssets, assetPath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		perm := os.FileMode(0o644)
		if isExecutableScriptAsset(assetPath) {
			perm = 0o755
		}
		return os.WriteFile(target, data, perm)
	})
}

// isExecutableScriptAsset reports whether a materialized asset should be
// marked executable. Shell, Python, and other script interpreters all rely
// on shebang-based direct execution, so the file needs +x regardless of the
// extension (gc discovers commands/doctor checks by convention and invokes
// the resolved path directly — it does not run `bash <script>`).
func isExecutableScriptAsset(assetPath string) bool {
	for _, suffix := range []string{".sh", ".py", ".bash"} {
		if strings.HasSuffix(assetPath, suffix) {
			return true
		}
	}
	return false
}

func assetRel(root, assetPath string) string {
	cleanRoot := pathpkg.Clean(root)
	cleanPath := pathpkg.Clean(assetPath)
	if cleanPath == cleanRoot {
		return "."
	}
	return strings.TrimPrefix(cleanPath, cleanRoot+"/")
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func readImplicitFile(path string) (map[string]implicitImport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]implicitImport), nil
		}
		return nil, fmt.Errorf("reading implicit-import.toml: %w", err)
	}

	var file implicitImportFile
	if _, err := toml.Decode(string(data), &file); err != nil {
		return nil, fmt.Errorf("parsing implicit-import.toml: %w", err)
	}
	if file.Schema != 0 && file.Schema != implicitImportSchema {
		return nil, fmt.Errorf("unsupported implicit import schema %d", file.Schema)
	}
	if file.Imports == nil {
		file.Imports = make(map[string]implicitImport)
	}
	return file.Imports, nil
}

func writeImplicitFile(path string, imports map[string]implicitImport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating implicit-import dir: %w", err)
	}
	var names []string
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("schema = 1\n")
	for _, name := range names {
		imp := imports[name]
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("[imports.%q]\n", name))
		b.WriteString(fmt.Sprintf("source = %q\n", imp.Source))
		if imp.Version != "" {
			b.WriteString(fmt.Sprintf("version = %q\n", imp.Version))
		}
		if imp.Commit != "" {
			b.WriteString(fmt.Sprintf("commit = %q\n", imp.Commit))
		}
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("creating implicit-import temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
	if _, err := tmpFile.WriteString(b.String()); err != nil {
		tmpFile.Close() //nolint:errcheck // best effort
		return fmt.Errorf("writing implicit-import.toml: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing implicit-import.toml temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing implicit-import.toml: %w", err)
	}
	return nil
}
