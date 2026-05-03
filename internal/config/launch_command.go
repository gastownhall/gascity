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
//
// When transport is "acp", the ACP-specific command (ACPCommand/ACPArgs) is
// used as the base instead of the default Command/Args. Pass "" for the
// provider default or "tmux" for the tmux-backed CLI transport.
func BuildProviderLaunchCommand(cityPath string, resolved *ResolvedProvider, optionOverrides map[string]string, transport string) (ProviderLaunchCommand, error) {
	if resolved == nil {
		return ProviderLaunchCommand{}, fmt.Errorf("resolved provider is nil")
	}
	if !IsValidSessionTransport(transport) {
		return ProviderLaunchCommand{}, fmt.Errorf("unknown session transport %q", strings.TrimSpace(transport))
	}

	command := providerLaunchBaseCommand(resolved, transport)
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

	return appendProviderSettings(cityPath, resolved.Name, command), nil
}

// BuildProviderLaunchCommandWithoutOptions composes the transport-specific
// provider command plus any provider-owned settings file without applying
// schema-managed defaults or explicit option overrides.
//
// Deferred agent-session creation uses this helper because option state is
// stored separately in template_overrides and applied later at actual start
// time, but the stored base command must still match the selected transport
// and provider-owned settings semantics.
func BuildProviderLaunchCommandWithoutOptions(cityPath string, resolved *ResolvedProvider, transport string) (ProviderLaunchCommand, error) {
	if resolved == nil {
		return ProviderLaunchCommand{}, fmt.Errorf("resolved provider is nil")
	}
	if !IsValidSessionTransport(transport) {
		return ProviderLaunchCommand{}, fmt.Errorf("unknown session transport %q", strings.TrimSpace(transport))
	}
	return appendProviderSettings(cityPath, resolved.Name, providerLaunchBaseCommand(resolved, transport)), nil
}

func providerLaunchBaseCommand(resolved *ResolvedProvider, transport string) string {
	switch strings.TrimSpace(transport) {
	case SessionTransportACP:
		return resolved.ACPCommandString()
	case "", SessionTransportTmux:
		return resolved.CommandString()
	default:
		return resolved.CommandString()
	}
}

func appendProviderSettings(cityPath, providerName, command string) ProviderLaunchCommand {
	settingsPath, settingsRel := ProviderSettingsSource(cityPath, providerName)
	if settingsPath != "" {
		command = command + " " + fmt.Sprintf("--settings %q", settingsPath)
	}

	return ProviderLaunchCommand{
		Command:      command,
		SettingsPath: settingsPath,
		SettingsRel:  settingsRel,
	}
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
