package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// DiscoveredCommand is a convention-discovered pack command.
type DiscoveredCommand struct {
	Name        string
	Command     []string
	Description string
	RunScript   string
	HelpFile    string
	SourceDir   string
	PackDir     string
	PackName    string
	BindingName string
}

type commandManifest struct {
	Command     []string `toml:"command"`
	Description string   `toml:"description"`
	Run         string   `toml:"run"`
}

func resolveContainedRunPath(packDir, nodeDir, runRel string) (string, error) {
	if filepath.IsAbs(runRel) {
		return "", fmt.Errorf("run path %q must stay within the pack directory", runRel)
	}

	candidate := filepath.Clean(filepath.Join(nodeDir, runRel))
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
		return "", fmt.Errorf("run path %q escapes the pack directory", runRel)
	}
	return candidate, nil
}

// DiscoverPackCommands scans a pack's commands/ tree and returns
// convention-discovered commands. Each directory containing run.sh is a
// command leaf. Nested directories imply nested command words by default.
func DiscoverPackCommands(fs fsys.FS, packDir, packName string) ([]DiscoveredCommand, error) {
	commandsDir := filepath.Join(packDir, "commands")
	if _, err := fs.Stat(commandsDir); err != nil {
		return nil, nil
	}

	var discovered []DiscoveredCommand
	if err := walkCommandDirs(fs, packDir, packName, commandsDir, nil, &discovered); err != nil {
		return nil, err
	}
	return discovered, nil
}

func walkCommandDirs(fs fsys.FS, packDir, packName, dir string, words []string, discovered *[]DiscoveredCommand) error {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}

		childDir := filepath.Join(dir, name)
		childWords := append(append([]string{}, words...), name)

		cmd, ok, err := discoveredCommandFromDir(fs, packDir, packName, childDir, childWords)
		if err != nil {
			return err
		}
		if ok {
			*discovered = append(*discovered, cmd)
			continue
		}

		if err := walkCommandDirs(fs, packDir, packName, childDir, childWords, discovered); err != nil {
			return err
		}
	}

	return nil
}

func discoveredCommandFromDir(fs fsys.FS, packDir, packName, commandDir string, defaultWords []string) (DiscoveredCommand, bool, error) {
	runRel := "run.sh"
	helpPath := filepath.Join(commandDir, "help.md")
	manifestPath := filepath.Join(commandDir, "command.toml")
	words := append([]string{}, defaultWords...)
	description := ""

	if data, err := fs.ReadFile(manifestPath); err == nil {
		var manifest commandManifest
		if _, err := toml.Decode(string(data), &manifest); err != nil {
			rel, _ := filepath.Rel(filepath.Join(packDir, "commands"), manifestPath)
			return DiscoveredCommand{}, false, fmt.Errorf("commands/%s: %w", filepath.ToSlash(rel), err)
		}
		if len(manifest.Command) > 0 {
			words = append([]string{}, manifest.Command...)
		}
		if manifest.Description != "" {
			description = manifest.Description
		}
		if manifest.Run != "" {
			runRel = manifest.Run
		}
	}

	if strings.Contains(runRel, "{{") {
		runPath := runRel
		helpFile := ""
		if _, err := fs.Stat(helpPath); err == nil {
			helpFile = helpPath
		}

		relDir, err := filepath.Rel(filepath.Join(packDir, "commands"), commandDir)
		if err != nil {
			relDir = commandDir
		}

		return DiscoveredCommand{
			Name:        filepath.ToSlash(relDir),
			Command:     append([]string{}, words...),
			Description: description,
			RunScript:   runPath,
			HelpFile:    helpFile,
			SourceDir:   commandDir,
			PackDir:     packDir,
			PackName:    packName,
		}, true, nil
	}

	runPath, err := resolveContainedRunPath(packDir, commandDir, runRel)
	if err != nil {
		return DiscoveredCommand{}, false, err
	}
	if _, err := fs.Stat(runPath); err != nil {
		return DiscoveredCommand{}, false, nil
	}

	helpFile := ""
	if _, err := fs.Stat(helpPath); err == nil {
		helpFile = helpPath
	}

	relDir, err := filepath.Rel(filepath.Join(packDir, "commands"), commandDir)
	if err != nil {
		relDir = commandDir
	}

	return DiscoveredCommand{
		Name:        filepath.ToSlash(relDir),
		Command:     append([]string{}, words...),
		Description: description,
		RunScript:   runPath,
		HelpFile:    helpFile,
		SourceDir:   commandDir,
		PackDir:     packDir,
		PackName:    packName,
	}, true, nil
}
