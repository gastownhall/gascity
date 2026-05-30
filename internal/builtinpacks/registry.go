// Package builtinpacks describes the packs bundled into the gc binary.
package builtinpacks

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/examples/bd"
	"github.com/gastownhall/gascity/examples/dolt"
	"github.com/gastownhall/gascity/examples/gastown/packs/gastown"
	"github.com/gastownhall/gascity/examples/gastown/packs/maintenance"
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/fsys"
	gitutil "github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/remotesource"
)

const (
	// Repository is the canonical clone URL for bundled pack imports.
	Repository = "https://github.com/gastownhall/gascity.git"

	// PublicRepository is the wave-one public pack repository. The gc binary
	// can serve its bundled public-pack aliases from the embedded pack set
	// when the network is unavailable during init or doctor repair.
	PublicRepository = "https://github.com/gastownhall/gascity-packs.git"

	// SyntheticCacheNamespace separates bundled synthetic repo caches from
	// ordinary git checkouts that point at the same repository and commit.
	SyntheticCacheNamespace = "bundled-synthetic-v1"

	syntheticMarkerFile = ".gc-bundled-pack-cache.toml"
)

// Pack describes a bundled pack and its canonical import source. Bundled
// sources resolve to the pack content embedded in the running gc binary.
type Pack struct {
	Name    string
	Subpath string
	FS      fs.FS
}

// All returns every pack bundled with gc in deterministic order.
func All() []Pack {
	return []Pack{
		{Name: "core", Subpath: "internal/bootstrap/packs/core", FS: core.PackFS},
		{Name: "bd", Subpath: "examples/bd", FS: bd.PackFS},
		{Name: "dolt", Subpath: "examples/dolt", FS: dolt.PackFS},
		{Name: "maintenance", Subpath: "examples/gastown/packs/maintenance", FS: maintenance.PackFS},
		{Name: "gastown", Subpath: "examples/gastown/packs/gastown", FS: gastown.PackFS},
	}
}

// Source returns the canonical remote import source for a bundled pack.
func Source(name string) (string, bool) {
	pack, ok := ByName(name)
	if !ok {
		return "", false
	}
	return Repository + "//" + pack.Subpath, true
}

// MustSource returns the canonical remote import source for a bundled pack.
func MustSource(name string) string {
	source, ok := Source(name)
	if !ok {
		panic("unknown bundled pack " + name)
	}
	return source
}

// ByName returns the bundled pack for name.
func ByName(name string) (Pack, bool) {
	for _, pack := range All() {
		if pack.Name == name {
			return pack, true
		}
	}
	return Pack{}, false
}

// NameForSource reports the bundled pack addressed by source.
func NameForSource(source string) (string, bool) {
	normalizedRepo, subpath := splitSource(source)
	for _, layout := range syntheticPackLayouts() {
		if normalizedRepo == layout.Repository && subpath == layout.Subpath {
			return layout.Pack.Name, true
		}
	}
	return "", false
}

type syntheticPackLayout struct {
	Repository string
	Subpath    string
	Pack       Pack
}

func syntheticPackLayouts() []syntheticPackLayout {
	packs := All()
	layouts := make([]syntheticPackLayout, 0, len(packs)+2)
	for _, pack := range packs {
		layouts = append(layouts, syntheticPackLayout{
			Repository: Repository,
			Subpath:    pack.Subpath,
			Pack:       pack,
		})
		if publicSubpath, ok := publicSubpathForPack(pack.Name); ok {
			layouts = append(layouts, syntheticPackLayout{
				Repository: PublicRepository,
				Subpath:    publicSubpath,
				Pack:       pack,
			})
		}
	}
	return layouts
}

func publicSubpathForPack(name string) (string, bool) {
	switch name {
	case "gastown", "maintenance":
		return name, true
	default:
		return "", false
	}
}

// IsSource reports whether source addresses one of gc's bundled packs.
func IsSource(source string) bool {
	_, ok := NameForSource(source)
	return ok
}

// MaterializeSyntheticRepo writes the running binary's bundled pack tree to dst
// as a synthetic repository cache for commit. The commit is the lock/cache tag
// requested by the import resolver; the marker content hash is what binds the
// cache to the current binary content. The cache is repo-shaped so relative
// imports between bundled pack subpaths resolve like a real checkout. Callers
// must hold any repo-cache write lock for dst and pass only a disposable cache
// directory; existing contents are removed unconditionally before writing.
func MaterializeSyntheticRepo(dst, commit string) error {
	if strings.TrimSpace(commit) == "" {
		return fmt.Errorf("commit is required")
	}
	if err := validateSyntheticDestination(dst); err != nil {
		return err
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("removing stale bundled pack cache %q: %w", dst, err)
	}
	for _, layout := range syntheticPackLayouts() {
		target := filepath.Join(dst, filepath.FromSlash(layout.Subpath))
		if err := materializeFS(layout.Pack.FS, target); err != nil {
			return fmt.Errorf("materializing bundled pack %q at %s: %w", layout.Pack.Name, layout.Subpath, err)
		}
	}
	hash, err := SyntheticContentHash()
	if err != nil {
		return err
	}
	marker := syntheticMarker{
		Schema:      1,
		Repository:  Repository,
		Commit:      commit,
		ContentHash: hash,
	}
	data, err := toml.Marshal(marker)
	if err != nil {
		return fmt.Errorf("marshaling bundled pack cache marker: %w", err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, filepath.Join(dst, syntheticMarkerFile), data, 0o644); err != nil {
		return fmt.Errorf("writing bundled pack cache marker: %w", err)
	}
	return nil
}

// ValidateSyntheticRepo verifies that dir is a synthetic bundled-pack cache
// created for the current binary content and requested lock/cache commit tag.
func ValidateSyntheticRepo(dir, commit string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing bundled pack cache marker")
		}
		return fmt.Errorf("checking bundled pack cache root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("bundled pack cache root %q is a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("bundled pack cache root %q is not a directory", dir)
	}

	data, err := os.ReadFile(filepath.Join(dir, syntheticMarkerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing bundled pack cache marker")
		}
		return fmt.Errorf("reading bundled pack cache marker: %w", err)
	}
	var marker syntheticMarker
	if _, err := toml.Decode(string(data), &marker); err != nil {
		return fmt.Errorf("parsing bundled pack cache marker: %w", err)
	}
	if marker.Schema != 1 {
		return fmt.Errorf("unsupported bundled pack cache marker schema %d", marker.Schema)
	}
	if marker.Repository != Repository {
		return fmt.Errorf("bundled pack cache repository %q does not match %q", marker.Repository, Repository)
	}
	if !gitutil.SameCommit(marker.Commit, commit) {
		return fmt.Errorf("bundled pack cache commit %q does not match %q", marker.Commit, commit)
	}
	wantHash, err := SyntheticContentHash()
	if err != nil {
		return err
	}
	if marker.ContentHash != wantHash {
		return fmt.Errorf("bundled pack cache content hash %q does not match current binary %q", marker.ContentHash, wantHash)
	}
	if err := validateSyntheticRepoFileSet(dir); err != nil {
		return err
	}
	for _, layout := range syntheticPackLayouts() {
		if err := validatePackFiles(layout.Pack, filepath.Join(dir, filepath.FromSlash(layout.Subpath))); err != nil {
			return err
		}
	}
	return nil
}

// MaterializedFileMode returns the filesystem mode used for bundled pack files
// when they are materialized from embed.FS.
func MaterializedFileMode(path string) os.FileMode {
	for _, suffix := range []string{".sh", ".py", ".bash"} {
		if strings.HasSuffix(path, suffix) {
			return 0o755
		}
	}
	return 0o644
}

// SyntheticContentHash returns a stable hash of all bundled pack file content
// and modes.
func SyntheticContentHash() (string, error) {
	var entries []string
	for _, layout := range syntheticPackLayouts() {
		pack := layout.Pack
		manifest, err := manifestForFS(pack.FS)
		if err != nil {
			return "", fmt.Errorf("hashing bundled pack %q: %w", pack.Name, err)
		}
		paths := make([]string, 0, len(manifest))
		for rel := range manifest {
			paths = append(paths, rel)
		}
		sort.Strings(paths)
		for _, rel := range paths {
			file := manifest[rel]
			sum := sha256.Sum256(file.data)
			entries = append(entries, fmt.Sprintf("%s/%s %04o %x", layout.Subpath, rel, file.perm.Perm(), sum[:]))
		}
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}

type syntheticMarker struct {
	Schema      int    `toml:"schema"`
	Repository  string `toml:"repository"`
	Commit      string `toml:"commit"`
	ContentHash string `toml:"content_hash"`
}

type fileEntry struct {
	data []byte
	perm os.FileMode
}

func materializeFS(src fs.FS, dst string) error {
	manifest, err := manifestForFS(src)
	if err != nil {
		return err
	}
	for rel, file := range manifest {
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, target, file.data, file.perm); err != nil {
			return err
		}
	}
	return nil
}

func validatePackFiles(pack Pack, dst string) error {
	manifest, err := manifestForFS(pack.FS)
	if err != nil {
		return fmt.Errorf("reading bundled pack %q manifest: %w", pack.Name, err)
	}
	for rel, want := range manifest {
		target := filepath.Join(dst, filepath.FromSlash(rel))
		info, err := os.Lstat(target)
		if err != nil {
			return fmt.Errorf("checking bundled pack cache %q file %s: %w", pack.Name, rel, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != want.perm.Perm() {
			return fmt.Errorf("bundled pack cache %q file %s has mode %s, expected %s", pack.Name, rel, info.Mode().Perm(), want.perm.Perm())
		}
		got, err := os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("reading bundled pack cache %q file %s: %w", pack.Name, rel, err)
		}
		if !bytes.Equal(got, want.data) {
			return fmt.Errorf("bundled pack cache %q file %s content differs from current binary", pack.Name, rel)
		}
	}
	if err := filepath.WalkDir(dst, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := manifest[rel]; !ok {
			return fmt.Errorf("bundled pack cache %q contains unexpected file %s", pack.Name, rel)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("validating bundled pack cache %q file set: %w", pack.Name, err)
	}
	return nil
}

func validateSyntheticRepoFileSet(dir string) error {
	allowedFiles, allowedDirs, err := syntheticRepoAllowedPaths()
	if err != nil {
		return err
	}
	firstUnexpectedDir := ""
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundled pack cache contains symlink %s", rel)
		}
		if entry.IsDir() {
			if _, ok := allowedDirs[rel]; ok {
				return nil
			}
			if firstUnexpectedDir == "" {
				firstUnexpectedDir = rel
			}
			return nil
		}
		if _, ok := allowedFiles[rel]; ok {
			return nil
		}
		return fmt.Errorf("bundled pack cache contains unexpected file %s", rel)
	}); err != nil {
		return fmt.Errorf("validating bundled pack cache file set: %w", err)
	}
	if firstUnexpectedDir != "" {
		return fmt.Errorf("validating bundled pack cache file set: bundled pack cache contains unexpected directory %s", firstUnexpectedDir)
	}
	return nil
}

func syntheticRepoAllowedPaths() (map[string]struct{}, map[string]struct{}, error) {
	files := map[string]struct{}{syntheticMarkerFile: {}}
	dirs := make(map[string]struct{})
	for _, layout := range syntheticPackLayouts() {
		subpath := filepath.ToSlash(layout.Subpath)
		manifest, err := manifestForFS(layout.Pack.FS)
		if err != nil {
			return nil, nil, fmt.Errorf("reading bundled pack %q manifest: %w", layout.Pack.Name, err)
		}
		for rel := range manifest {
			full := path.Join(subpath, rel)
			files[full] = struct{}{}
			for dir := path.Dir(full); dir != "." && dir != "/"; dir = path.Dir(dir) {
				dirs[dir] = struct{}{}
			}
		}
	}
	return files, dirs, nil
}

func manifestForFS(src fs.FS) (map[string]fileEntry, error) {
	manifest := make(map[string]fileEntry)
	if err := fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		manifest[filepath.ToSlash(path)] = fileEntry{
			data: data,
			perm: MaterializedFileMode(path),
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(manifest) == 0 {
		return nil, fmt.Errorf("bundled pack manifest is empty")
	}
	if _, ok := manifest["pack.toml"]; !ok {
		return nil, fmt.Errorf("bundled pack manifest is missing pack.toml")
	}
	return manifest, nil
}

func splitSource(source string) (repository, subpath string) {
	parsed := remotesource.Parse(source)
	return normalizeRepository(parsed.CloneURL), strings.Trim(parsed.Subpath, "/")
}

func normalizeRepository(repo string) string {
	repo = strings.TrimRight(strings.TrimSpace(repo), "/")
	if strings.HasPrefix(repo, "github.com/") {
		repo = "https://" + repo
	}
	if repo == "https://github.com/gastownhall/gascity" {
		return Repository
	}
	if repo == "https://github.com/gastownhall/gascity-packs" {
		return PublicRepository
	}
	return repo
}

func validateSyntheticDestination(dst string) error {
	if strings.TrimSpace(dst) == "" {
		return fmt.Errorf("refusing to materialize synthetic repo to unsafe path %q", dst)
	}
	clean := filepath.Clean(dst)
	root := filepath.VolumeName(clean) + string(filepath.Separator)
	if clean == "." || clean == root {
		return fmt.Errorf("refusing to materialize synthetic repo to unsafe path %q", dst)
	}
	return nil
}
