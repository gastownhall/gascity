package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// DiscoveredDoctor is a convention-discovered pack doctor check.
type DiscoveredDoctor struct {
	Name        string
	Description string
	RunScript   string
	// FixScript is the optional remediation script. When non-empty the
	// check opts into `gc doctor --fix`. Empty means check is diagnostic-
	// only (the pre-existing behavior).
	FixScript   string
	HelpFile    string
	SourceDir   string
	PackDir     string
	PackName    string
	BindingName string
}

type doctorManifest struct {
	Description string `toml:"description"`
	Run         string `toml:"run"`
	Fix         string `toml:"fix"`
}

func resolveContainedDoctorPath(kind, packDir, checkDir, relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("%s path %q must stay within the pack directory", kind, relPath)
	}

	candidate := filepath.Clean(filepath.Join(checkDir, relPath))
	absPackDir, err := filepath.Abs(packDir)
	if err != nil {
		return "", err
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absPackDir, absCandidate)
	if err != nil {
		return "", err
	}
	if pathutil.IsOutsideDir(rel) {
		return "", fmt.Errorf("%s path %q escapes the pack directory", kind, relPath)
	}
	return candidate, nil
}

func resolveContainedDoctorRunPath(packDir, checkDir, runRel string) (string, error) {
	return resolveContainedDoctorPath("run", packDir, checkDir, runRel)
}

func resolveContainedDoctorFixPath(packDir, checkDir, fixRel string) (string, error) {
	return resolveContainedDoctorPath("fix", packDir, checkDir, fixRel)
}

// DiscoverPackDoctors scans a pack's doctor/ directory and returns
// convention-discovered checks. Each immediate child directory with a
// run.sh script is a doctor check.
func DiscoverPackDoctors(fs fsys.FS, packDir, packName string) ([]DiscoveredDoctor, error) {
	doctorDir := filepath.Join(packDir, "doctor")
	entries, err := fs.ReadDir(doctorDir)
	if err != nil {
		return nil, nil
	}

	var discovered []DiscoveredDoctor
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}

		checkDir := filepath.Join(doctorDir, name)
		check, ok, err := discoveredDoctorFromDir(fs, packDir, checkDir, name, packName)
		if err != nil {
			return nil, err
		}
		if ok {
			discovered = append(discovered, check)
		}
	}

	return discovered, nil
}

func discoveredDoctorFromDir(fs fsys.FS, packDir, checkDir, name, packName string) (DiscoveredDoctor, bool, error) {
	runRel := "run.sh"
	description := ""
	fixRel := ""

	manifestPath := filepath.Join(checkDir, "doctor.toml")
	if data, err := fs.ReadFile(manifestPath); err == nil {
		var manifest doctorManifest
		if _, err := toml.Decode(string(data), &manifest); err != nil {
			return DiscoveredDoctor{}, false, fmt.Errorf("doctor/%s/doctor.toml: %w", name, err)
		}
		description = manifest.Description
		if manifest.Run != "" {
			runRel = manifest.Run
		}
		if manifest.Fix != "" {
			fixRel = manifest.Fix
		}
	}

	runPath, err := resolveContainedDoctorRunPath(packDir, checkDir, runRel)
	if err != nil {
		return DiscoveredDoctor{}, false, err
	}
	if _, err := fs.Stat(runPath); err != nil {
		return DiscoveredDoctor{}, false, nil
	}

	fixPath := ""
	if fixRel != "" {
		resolved, err := resolveContainedDoctorFixPath(packDir, checkDir, fixRel)
		if err != nil {
			return DiscoveredDoctor{}, false, fmt.Errorf("doctor/%s fix: %w", name, err)
		}
		if _, err := fs.Stat(resolved); err == nil {
			fixPath = resolved
		} else {
			return DiscoveredDoctor{}, false, fmt.Errorf("doctor/%s fix %q: %w", name, fixRel, err)
		}
	} else {
		// Sibling-convention auto-discovery: if a fix script named
		// `fix.sh` exists next to the check script, the check opts into
		// `gc doctor --fix` without needing a manifest. This mirrors how
		// `run.sh` is the default when no manifest is provided.
		candidate := filepath.Join(checkDir, "fix.sh")
		if _, err := fs.Stat(candidate); err == nil {
			fixPath = candidate
		}
	}

	helpPath := filepath.Join(checkDir, "help.md")
	helpFile := ""
	if _, err := fs.Stat(helpPath); err == nil {
		helpFile = helpPath
	}

	return DiscoveredDoctor{
		Name:        name,
		Description: description,
		RunScript:   runPath,
		FixScript:   fixPath,
		HelpFile:    helpFile,
		SourceDir:   checkDir,
		PackDir:     packDir,
		PackName:    packName,
	}, true, nil
}
