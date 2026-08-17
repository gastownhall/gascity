package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const implicitImportSchema = 1

// ImplicitImport describes a legacy user-global import record retained for
// compatibility tooling. Config composition no longer splices these imports
// into every city.
type ImplicitImport struct {
	Source  string `toml:"source"`
	Version string `toml:"version"`
	Commit  string `toml:"commit"`
}

type implicitImportFile struct {
	Schema  int                       `toml:"schema"`
	Imports map[string]ImplicitImport `toml:"imports"`
}

// ReadImplicitImports reads ~/.gc/implicit-import.toml (or $GC_HOME) and
// returns its imports. Missing files are treated as empty.
func ReadImplicitImports() (map[string]ImplicitImport, string, error) {
	imports, path, _, err := readImplicitImportsWithData()
	return imports, path, err
}

func readImplicitImportsWithData() (map[string]ImplicitImport, string, []byte, error) {
	path := implicitImportPath()
	if path == "" {
		return map[string]ImplicitImport{}, "", nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]ImplicitImport{}, path, nil, nil
		}
		return nil, path, nil, fmt.Errorf("reading implicit imports: %w", err)
	}

	var file implicitImportFile
	if _, err := toml.Decode(string(data), &file); err != nil {
		return nil, path, nil, fmt.Errorf("parsing implicit imports: %w", err)
	}
	if file.Schema != 0 && file.Schema != implicitImportSchema {
		return nil, path, nil, fmt.Errorf("unsupported implicit import schema %d", file.Schema)
	}
	if file.Imports == nil {
		file.Imports = make(map[string]ImplicitImport)
	}
	return file.Imports, path, data, nil
}

func implicitImportPath() string {
	home := ImplicitGCHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "implicit-import.toml")
}

// ImplicitGCHome returns the user-global GC_HOME directory used to
// resolve implicit-import bookkeeping and bootstrap pack caches.
//
// Resolution order: GC_HOME env var → user home/.gc → tmp fallback.
// Returns "" under `go test` to keep unit tests hermetic unless the
// caller opts in by setting GC_HOME explicitly.
func ImplicitGCHome() string {
	if v := strings.TrimSpace(os.Getenv("GC_HOME")); v != "" {
		return v
	}
	// Keep unit tests hermetic unless they explicitly opt into a GC_HOME.
	if strings.HasSuffix(os.Args[0], ".test") {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".gc")
	}
	// Home unresolved. Never fall back to a fixed os.TempDir()/.gc: that path
	// is shared and world-writable, so concurrent processes clobber each
	// other's state and unrelated city scans pick it up as a real city
	// (#3506). Hand out a process-unique directory instead.
	if dir, err := os.MkdirTemp("", "gc-home-*"); err == nil {
		return dir
	}
	// MkdirTemp failed, so the temp directory itself is unusable. Return a
	// process-unique path under it rather than "" (which callers would join
	// into a CWD-relative path, silently writing state to the wrong place) or
	// the shared os.TempDir()/.gc that #3506 is about. The caller then fails
	// loudly when it cannot create or write this path.
	return filepath.Join(os.TempDir(), fmt.Sprintf("gc-home-%d", os.Getpid()))
}

// GlobalRepoCacheRoot returns the user-global repo cache root
// (<GC_HOME>/cache/repos), honoring the GC_HOME override the same way
// every other user-global path does. It errors when no GC_HOME is
// resolvable (hermetic test binaries without GC_HOME set) instead of
// silently touching the developer's real cache.
func GlobalRepoCacheRoot() (string, error) {
	gcHome := ImplicitGCHome()
	if gcHome == "" {
		return "", fmt.Errorf("no GC_HOME available to resolve the repo cache; set GC_HOME")
	}
	return filepath.Join(gcHome, "cache", "repos"), nil
}

// GlobalRepoCachePath returns the user-global cache path for a source+commit pair.
func GlobalRepoCachePath(gcHome, source, commit string) string {
	return filepath.Join(gcHome, "cache", "repos", GlobalRepoCacheDirName(source, commit))
}

// GlobalRepoCacheDirName returns the user-global cache directory name for a source+commit pair.
func GlobalRepoCacheDirName(source, commit string) string {
	return RepoCacheKey(source, commit)
}
