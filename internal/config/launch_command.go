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

	return appendProviderSettings(cityPath, providerLaunchFamily(resolved), command), nil
}

// BuildProviderTemplateLaunchCommand composes the launch command an agent
// template resolves to before any per-session option overrides: the
// ProviderTemplateCommand plus any provider-owned settings file. It is the
// command the controller's template resolution composes for the same agent,
// before that resolution expands session-setup placeholders such as
// {{.WorkDir}}, so a deferred create can persist it as the baseline the
// async-start drift check compares against.
func BuildProviderTemplateLaunchCommand(cityPath string, resolved *ResolvedProvider, transport string, installHooks []string, providers map[string]ProviderSpec) (ProviderLaunchCommand, error) {
	command, err := ProviderTemplateCommand(resolved, transport, installHooks, providers)
	if err != nil {
		return ProviderLaunchCommand{}, err
	}
	return appendProviderSettings(cityPath, providerLaunchFamily(resolved), command), nil
}

// ProviderTemplateCommand composes an agent template's provider command
// without provider settings: the transport-specific base command, the Kimi
// hook config argument when installHooks covers the Kimi family, and the
// schema-managed defaults. Unlike BuildProviderLaunchCommand it never fails on
// an option default: one that cannot be rendered is omitted, as in
// ResolvedProvider.ResolveDefaultArgs, and callers report it through
// ResolvedProvider.UnhonoredOptionPins.
func ProviderTemplateCommand(resolved *ResolvedProvider, transport string, installHooks []string, providers map[string]ProviderSpec) (string, error) {
	if resolved == nil {
		return "", fmt.Errorf("resolved provider is nil")
	}
	if !IsValidSessionTransport(transport) {
		return "", fmt.Errorf("unknown session transport %q", strings.TrimSpace(transport))
	}
	command := providerLaunchBaseCommand(resolved, transport)
	if providerLaunchFamily(resolved) == "kimi" && installHooksIncludeFamily(installHooks, "kimi", providers) {
		command = appendKimiHookConfigArg(command)
	}
	// Append schema-derived default args (e.g., --dangerously-skip-permissions
	// from EffectiveDefaults["permission_mode"] = "unrestricted").
	if defaultArgs := resolved.ResolveDefaultArgs(); len(defaultArgs) > 0 {
		command = command + " " + shellquote.Join(defaultArgs)
	}
	return command, nil
}

func installHooksIncludeFamily(installHooks []string, family string, providers map[string]ProviderSpec) bool {
	family = strings.TrimSpace(family)
	if family == "" {
		return false
	}
	for _, hook := range installHooks {
		hook = strings.TrimSpace(hook)
		if hook == "" {
			continue
		}
		if hook == family || BuiltinFamily(hook, providers) == family {
			return true
		}
	}
	return false
}

func appendKimiHookConfigArg(command string) string {
	parts := shellquote.Split(command)
	if len(parts) == 0 {
		return command
	}
	configArgs := []string{"--config-file", ".kimi/config.toml"}
	if parts[len(parts)-1] == "acp" {
		withConfig := make([]string, 0, len(parts)+len(configArgs))
		withConfig = append(withConfig, parts[:len(parts)-1]...)
		withConfig = append(withConfig, configArgs...)
		withConfig = append(withConfig, parts[len(parts)-1])
		return shellquote.Join(withConfig)
	}
	parts = append(parts, configArgs...)
	return shellquote.Join(parts)
}

// BuildProviderResumeCommand applies schema-managed option overrides to a
// provider's explicit resume_command template.
func BuildProviderResumeCommand(resolved *ResolvedProvider, optionOverrides map[string]string) (string, error) {
	if resolved == nil {
		return "", fmt.Errorf("resolved provider is nil")
	}
	command := strings.TrimSpace(resolved.ResumeCommand)
	// Unlike the launch-side gate above, this deliberately checks explicit
	// overrides only. For a provider-supplied resume_command, resolve.go has
	// already run completeResumeCommandDefaults, inserting the missing
	// EffectiveDefaults flags around the template's own tokens. Gating on
	// EffectiveDefaults here too would take the path below instead, which
	// strips the schema's declared flags and aliases (an undeclared alias
	// survives) and re-emits the merged set in schema order,
	// reordering a template whose flags were already positioned deliberately
	// and replacing a declared short alias with its canonical spelling.
	//
	// That resolve-time completion is skipped when the agent supplies its own
	// resume_command (resolve.go's agent.ResumeCommand == "" guard). Such a
	// template still picks up EffectiveDefaults here when an explicit override
	// is present, since providerOptionArgs merges defaults with overrides, but
	// with no override it keeps none of them. Widening this gate is not the fix
	// for that, for the reason above; see issue #5659.
	if command == "" || len(resolved.OptionsSchema) == 0 || !hasSchemaOptionOverrides(optionOverrides) {
		return command, nil
	}
	mergedArgs, err := providerOptionArgs(resolved, optionOverrides)
	if err != nil {
		return "", err
	}
	return replaceResumeSchemaFlags(command, resolved.ResumeFlag, resolved.ResumeStyle, resolved.OptionsSchema, mergedArgs), nil
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
	// Same gap as ReplaceSchemaFlags: exact matching misses a value rendered
	// through an open option's FlagTemplate, which would then be re-emitted
	// alongside the override instead of replaced (ga-fyh).
	stripped = stripShapes(stripped, CollectOpenOptionShapes(schema))
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

func providerLaunchFamily(resolved *ResolvedProvider) string {
	if resolved == nil {
		return ""
	}
	if family := strings.TrimSpace(resolved.BuiltinAncestor); family != "" {
		return family
	}
	// Keep launch composition aligned with resolvedProviderLaunchFamily in
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
