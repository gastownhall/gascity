// Package builtinpacks describes the packs bundled into the gc binary.
package builtinpacks

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
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
)

const (
	// Repository is the canonical clone URL for bundled pack imports.
	Repository = "https://github.com/gastownhall/gascity.git"

	syntheticMarkerFile = ".gc-bundled-pack-cache.toml"
)

// Pack describes a bundled pack and its canonical import source.
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
	if normalizedRepo != Repository {
		return "", false
	}
	for _, pack := range All() {
		if subpath == pack.Subpath {
			return pack.Name, true
		}
	}
	return "", false
}

// IsSource reports whether source addresses one of gc's bundled packs.
func IsSource(source string) bool {
	_, ok := NameForSource(source)
	return ok
}

// MaterializeSyntheticRepo writes the bundled pack tree to dst as a synthetic
// repository cache for commit. The cache is repo-shaped so relative imports
// between bundled pack subpaths resolve like a real checkout. Callers must
// hold any repo-cache write lock for dst and pass only a disposable cache
// directory; existing contents are removed unconditionally before writing.
func MaterializeSyntheticRepo(dst, commit string) error {
	if strings.TrimSpace(commit) == "" {
		return fmt.Errorf("commit is required")
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("removing stale bundled pack cache %q: %w", dst, err)
	}
	for _, pack := range All() {
		target := filepath.Join(dst, filepath.FromSlash(pack.Subpath))
		if err := materializeFS(pack.FS, target); err != nil {
			return fmt.Errorf("materializing bundled pack %q: %w", pack.Name, err)
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
// created for the current binary content and requested commit.
func ValidateSyntheticRepo(dir, commit string) error {
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
	if !sameCommit(marker.Commit, commit) {
		return fmt.Errorf("bundled pack cache commit %q does not match %q", marker.Commit, commit)
	}
	wantHash, err := SyntheticContentHash()
	if err != nil {
		return err
	}
	if marker.ContentHash != wantHash {
		return fmt.Errorf("bundled pack cache content hash %q does not match current binary %q", marker.ContentHash, wantHash)
	}
	for _, pack := range All() {
		if err := validatePackFiles(pack, filepath.Join(dir, filepath.FromSlash(pack.Subpath))); err != nil {
			return err
		}
	}
	return nil
}

// SyntheticContentHash returns a stable hash of all bundled pack file content
// and modes.
func SyntheticContentHash() (string, error) {
	var entries []string
	for _, pack := range All() {
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
			entries = append(entries, fmt.Sprintf("%s/%s %04o %x", pack.Subpath, rel, file.perm.Perm(), sum[:]))
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
			perm: fileMode(path),
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return manifest, nil
}

func fileMode(path string) os.FileMode {
	if strings.HasSuffix(path, ".sh") {
		return 0o755
	}
	return 0o644
}

func splitSource(source string) (repository, subpath string) {
	withoutRef := source
	if i := strings.LastIndex(withoutRef, "#"); i >= 0 {
		withoutRef = withoutRef[:i]
	}
	searchFrom := 0
	if i := strings.Index(withoutRef, "://"); i >= 0 {
		searchFrom = i + 3
	}
	if i := strings.Index(withoutRef[searchFrom:], "//"); i >= 0 {
		pos := searchFrom + i
		repository = withoutRef[:pos]
		subpath = strings.Trim(withoutRef[pos+2:], "/")
	} else {
		repository = withoutRef
	}
	repository = normalizeRepository(repository)
	return repository, subpath
}

func normalizeRepository(repo string) string {
	repo = strings.TrimRight(strings.TrimSpace(repo), "/")
	if strings.HasPrefix(repo, "github.com/") {
		repo = "https://" + repo
	}
	if repo == "https://github.com/gastownhall/gascity" {
		return Repository
	}
	return repo
}

func sameCommit(actual, expected string) bool {
	actual = strings.TrimSpace(actual)
	expected = strings.TrimSpace(expected)
	if actual == "" || expected == "" {
		return false
	}
	if strings.EqualFold(actual, expected) {
		return true
	}
	return len(expected) >= 7 && len(expected) < len(actual) && strings.HasPrefix(strings.ToLower(actual), strings.ToLower(expected))
}
