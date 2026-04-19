// Package bootstrap reconciles legacy user-global implicit-import state for
// compatibility tooling. Launch-time system packs now come from .gc/system/packs.
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
)

const implicitImportSchema = 1

// Entry describes a bootstrap-managed implicit import identity.
type Entry struct {
	Name   string
	Source string
}

// BootstrapPacks is the currently-supported compatibility set. It is empty for
// the gc import launch path: cities rely on .gc/system/packs and explicit
// [imports], not user-global implicit imports.
var BootstrapPacks []Entry

// RetiredBootstrapPacks are legacy implicit imports that older gc releases
// wrote into ~/.gc/implicit-import.toml. EnsureBootstrap prunes matching
// entries so upgraded installs stop carrying stale launch-only state forever.
var RetiredBootstrapPacks = []Entry{
	{Name: "import", Source: "github.com/gastownhall/gc-import"},
	{Name: "registry", Source: "github.com/gastownhall/gc-registry"},
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

// EnsureBootstrap prunes retired bootstrap-managed implicit imports and
// materializes any still-supported compatibility packs.
func EnsureBootstrap(gcHome string) error {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("GC_BOOTSTRAP")), "skip") {
		return nil
	}
	if strings.TrimSpace(gcHome) == "" {
		gcHome = defaultGCHome()
	}
	if strings.TrimSpace(gcHome) == "" {
		return nil
	}

	implicitPath := filepath.Join(gcHome, "implicit-import.toml")
	imports, err := readImplicitFile(implicitPath)
	if err != nil {
		return err
	}
	updated := false

	for _, retired := range RetiredBootstrapPacks {
		existing, ok := imports[retired.Name]
		if !ok {
			continue
		}
		if config.NormalizeRemoteSource(existing.Source) != config.NormalizeRemoteSource(retired.Source) {
			continue
		}
		delete(imports, retired.Name)
		updated = true
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
