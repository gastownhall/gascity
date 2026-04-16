package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

// DiscoveredDoctor is a convention-discovered pack doctor check.
type DiscoveredDoctor struct {
	Name        string
	Description string
	RunScript   string
	HelpFile    string
	SourceDir   string
	PackDir     string
	PackName    string
	BindingName string
}

type doctorManifest struct {
	Description string `toml:"description"`
	Run         string `toml:"run"`
}

func resolveContainedDoctorRunPath(packDir, checkDir, runRel string) (string, error) {
	if filepath.IsAbs(runRel) {
		return "", fmt.Errorf("run path %q must stay within the pack directory", runRel)
	}

	candidate := filepath.Clean(filepath.Join(checkDir, runRel))
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
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("run path %q escapes the pack directory", runRel)
	}
	return candidate, nil
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
	}

	runPath, err := resolveContainedDoctorRunPath(packDir, checkDir, runRel)
	if err != nil {
		return DiscoveredDoctor{}, false, err
	}
	if _, err := fs.Stat(runPath); err != nil {
		return DiscoveredDoctor{}, false, nil
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
		HelpFile:    helpFile,
		SourceDir:   checkDir,
		PackDir:     packDir,
		PackName:    packName,
	}, true, nil
}
