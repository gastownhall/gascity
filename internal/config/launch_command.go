package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// ProviderLaunchCommand is the fully composed provider command plus any
// provider-owned settings file discovered for that launch.
type ProviderLaunchCommand struct {
	Command      string
	SettingsPath string
	SettingsRel  string
}

// BuildProviderLaunchCommand composes the final provider launch command used
// for session startup. It starts from the raw provider command, applies
// schema-managed defaults plus any explicit option overrides, and appends a
// provider-owned settings file when present.
func BuildProviderLaunchCommand(cityPath string, resolved *ResolvedProvider, optionOverrides map[string]string) (ProviderLaunchCommand, error) {
	if resolved == nil {
		return ProviderLaunchCommand{}, fmt.Errorf("resolved provider is nil")
	}

	command := resolved.CommandString()
	if len(resolved.OptionsSchema) > 0 {
		mergedOptions := make(map[string]string, len(resolved.EffectiveDefaults)+len(optionOverrides))
		for key, value := range resolved.EffectiveDefaults {
			mergedOptions[key] = value
		}
		for key, value := range optionOverrides {
			if key == "initial_message" {
				continue
			}
			mergedOptions[key] = value
		}
		if len(mergedOptions) > 0 {
			mergedArgs, err := ResolveExplicitOptions(resolved.OptionsSchema, mergedOptions)
			if err != nil {
				return ProviderLaunchCommand{}, err
			}
			command = ReplaceSchemaFlags(command, resolved.OptionsSchema, mergedArgs)
		}
	}

	settingsPath, settingsRel := ProviderSettingsSource(cityPath, resolved.Name)
	if settingsPath != "" {
		command = command + " " + fmt.Sprintf("--settings %q", settingsPath)
	}

	return ProviderLaunchCommand{
		Command:      command,
		SettingsPath: settingsPath,
		SettingsRel:  settingsRel,
	}, nil
}

// ProviderSettingsSource returns the provider-owned settings file that should
// be passed to the launched process, plus the relative destination used when
// staging that file into remote runtimes.
func ProviderSettingsSource(cityPath, providerName string) (src, rel string) {
	if providerName != "claude" {
		return "", ""
	}
	candidates := []struct {
		src string
		rel string
	}{
		{src: filepath.Join(cityPath, ".gc", "settings.json"), rel: path.Join(".gc", "settings.json")},
		{src: citylayout.ClaudeHookFilePath(cityPath), rel: path.Clean(strings.ReplaceAll(citylayout.ClaudeHookFile, string(filepath.Separator), "/"))},
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate.src); err == nil {
			return candidate.src, candidate.rel
		}
	}
	return "", ""
}
