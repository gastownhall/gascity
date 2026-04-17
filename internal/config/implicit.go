package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const implicitImportSchema = 1

// ImplicitImport describes a user-global import spliced into every city.
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
	path := implicitImportPath()
	if path == "" {
		return map[string]ImplicitImport{}, "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]ImplicitImport{}, path, nil
		}
		return nil, path, fmt.Errorf("reading implicit imports: %w", err)
	}

	var file implicitImportFile
	if _, err := toml.Decode(string(data), &file); err != nil {
		return nil, path, fmt.Errorf("parsing implicit imports: %w", err)
	}
	if file.Schema != 0 && file.Schema != implicitImportSchema {
		return nil, path, fmt.Errorf("unsupported implicit import schema %d", file.Schema)
	}
	if file.Imports == nil {
		file.Imports = make(map[string]ImplicitImport)
	}
	return file.Imports, path, nil
}

func implicitImportPath() string {
	home := implicitGCHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "implicit-import.toml")
}

func implicitGCHome() string {
	if v := strings.TrimSpace(os.Getenv("GC_HOME")); v != "" {
		return v
	}
	// Keep unit tests hermetic unless they explicitly opt into a GC_HOME.
	if strings.HasSuffix(os.Args[0], ".test") {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".gc")
	}
	return filepath.Join(home, ".gc")
}

func resolveImplicitImport(imp ImplicitImport) Import {
	source := imp.Source
	if imp.Commit != "" {
		if home := implicitGCHome(); home != "" {
			source = GlobalRepoCachePath(home, imp.Source, imp.Commit)
		}
	}
	return Import{
		Source:  source,
		Version: imp.Version,
	}
}

// GlobalRepoCachePath returns the user-global cache path for a source+commit pair.
func GlobalRepoCachePath(gcHome, source, commit string) string {
	return filepath.Join(gcHome, "cache", "repos", GlobalRepoCacheDirName(source, commit))
}

// GlobalRepoCacheDirName returns the user-global cache directory name for a source+commit pair.
func GlobalRepoCacheDirName(source, commit string) string {
	return RepoCacheKey(source, commit)
}
