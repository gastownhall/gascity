package config

import (
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/shellquote"
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
	if len(resolved.OptionsSchema) > 0 && hasProviderOptionValues(resolved, optionOverrides) {
		mergedArgs, err := providerOptionArgs(resolved, optionOverrides)
		if err != nil {
			return ProviderLaunchCommand{}, err
		}
		command = ReplaceSchemaFlags(command, resolved.OptionsSchema, mergedArgs)
	}

	return appendProviderSettings(cityPath, providerSettingsFamily(resolved), command), nil
}

// BuildProviderResumeCommand applies schema-managed option overrides to a
// provider's explicit resume_command template.
func BuildProviderResumeCommand(resolved *ResolvedProvider, optionOverrides map[string]string) (string, error) {
	if resolved == nil {
		return "", fmt.Errorf("resolved provider is nil")
	}
	command := strings.TrimSpace(resolved.ResumeCommand)
	if command == "" || len(resolved.OptionsSchema) == 0 || !hasSchemaOptionOverrides(optionOverrides) {
		return command, nil
	}
	mergedArgs, err := providerOptionArgs(resolved, optionOverrides)
	if err != nil {
		return "", err
	}
	return replaceResumeSchemaFlags(command, resolved.ResumeFlag, resolved.ResumeStyle, resolved.OptionsSchema, mergedArgs), nil
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
	return appendProviderSettings(cityPath, providerSettingsFamily(resolved), providerLaunchBaseCommand(resolved, transport)), nil
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

func providerOptionArgs(resolved *ResolvedProvider, optionOverrides map[string]string) ([]string, error) {
	if resolved == nil || len(resolved.OptionsSchema) == 0 {
		return nil, nil
	}
	mergedOptions := make(map[string]string, providerOptionMapCapacity(len(resolved.EffectiveDefaults), len(optionOverrides)))
	for key, value := range resolved.EffectiveDefaults {
		mergedOptions[key] = value
	}
	for key, value := range optionOverrides {
		if key == "initial_message" {
			continue
		}
		mergedOptions[key] = value
	}
	if len(mergedOptions) == 0 {
		return nil, nil
	}
	return ResolveExplicitOptions(resolved.OptionsSchema, mergedOptions)
}

func providerOptionMapCapacity(defaultsLen, overridesLen int) int {
	if overridesLen > 0 && defaultsLen <= math.MaxInt-overridesLen {
		return defaultsLen + overridesLen
	}
	return defaultsLen
}

func hasProviderOptionValues(resolved *ResolvedProvider, optionOverrides map[string]string) bool {
	if resolved != nil && len(resolved.EffectiveDefaults) > 0 {
		return true
	}
	return hasSchemaOptionOverrides(optionOverrides)
}

func hasSchemaOptionOverrides(optionOverrides map[string]string) bool {
	for key := range optionOverrides {
		if key != "initial_message" {
			return true
		}
	}
	return false
}

func replaceResumeSchemaFlags(command, resumeFlag, resumeStyle string, schema []ProviderOption, overrideArgs []string) string {
	stripped := StripFlags(command, CollectAllSchemaFlags(schema))
	if len(overrideArgs) == 0 {
		return unquoteSessionKeyTemplate(stripped)
	}
	if resumeStyle == "subcommand" && resumeFlag != "" {
		tokens := shellquote.Split(stripped)
		insertAt := subcommandResumeInsertIndex(tokens, resumeFlag)
		out := make([]string, 0, len(tokens)+len(overrideArgs))
		out = append(out, tokens[:insertAt]...)
		out = append(out, overrideArgs...)
		out = append(out, tokens[insertAt:]...)
		return unquoteSessionKeyTemplate(shellquote.Join(out))
	}
	return unquoteSessionKeyTemplate(stripped + " " + shellquote.Join(overrideArgs))
}

func unquoteSessionKeyTemplate(command string) string {
	return strings.ReplaceAll(command, "'{{.SessionKey}}'", "{{.SessionKey}}")
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

func providerSettingsFamily(resolved *ResolvedProvider) string {
	if resolved == nil {
		return ""
	}
	if family := strings.TrimSpace(resolved.BuiltinAncestor); family != "" {
		return family
	}
	// Keep settings discovery aligned with resolvedProviderLaunchFamily in
	// cmd/gc: deprecated Kind is descriptive metadata, not launch family.
	return strings.TrimSpace(resolved.Name)
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
