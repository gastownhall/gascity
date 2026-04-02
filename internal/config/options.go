package config

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// ValidateOptionsSchema checks that every option default resolves to a declared choice.
// Call at config load time to catch misconfigured providers early.
func ValidateOptionsSchema(schema []ProviderOption) error {
	for _, opt := range schema {
		if opt.Default != "" && findChoice(opt.Choices, opt.Default) == nil {
			return fmt.Errorf("option %q: default %q is not a valid choice", opt.Key, opt.Default)
		}
	}
	return nil
}

// ValidateOptionDefaults checks that every value in the defaults map resolves to a
// declared choice in the schema. Call at config load time to catch typos in
// option_defaults early.
func ValidateOptionDefaults(schema []ProviderOption, defaults map[string]string) error {
	for key, value := range defaults {
		opt := findOption(schema, key)
		if opt == nil {
			return fmt.Errorf("option_defaults key %q is not in the options schema", key)
		}
		if findChoice(opt.Choices, value) == nil {
			return fmt.Errorf("option_defaults key %q: value %q is not a valid choice", key, value)
		}
	}
	return nil
}

// ComputeEffectiveDefaults merges schema defaults, provider option_defaults,
// and agent option_defaults into a single map. Later layers override earlier:
//
//	Layer 1: schema-declared defaults (ProviderOption.Default)
//	Layer 2: provider-level overrides (ProviderSpec.OptionDefaults)
//	Layer 3: agent-level overrides (Agent.OptionDefaults)
func ComputeEffectiveDefaults(schema []ProviderOption, providerDefaults, agentDefaults map[string]string) map[string]string {
	result := make(map[string]string)

	// Layer 1: schema-declared defaults.
	for _, opt := range schema {
		if opt.Default != "" {
			result[opt.Key] = opt.Default
		}
	}

	// Layer 2: provider-level overrides.
	for k, v := range providerDefaults {
		result[k] = v
	}

	// Layer 3: agent-level overrides.
	for k, v := range agentDefaults {
		result[k] = v
	}

	return result
}

// ResolveOptions validates user-specified options against a provider's schema
// and produces extra CLI args to inject into the command. Options not specified
// by the user fall back to effectiveDefaults (then schema Default). Returns the
// extra args and metadata entries (opt_<key>=<value>) for bead persistence.
//
// Args are emitted in schema declaration order for deterministic command lines.
func ResolveOptions(schema []ProviderOption, options map[string]string, effectiveDefaults map[string]string) (extraArgs []string, metadata map[string]string, err error) {
	metadata = make(map[string]string)

	// Validate user-specified option keys and values up front.
	for key, value := range options {
		opt := findOption(schema, key)
		if opt == nil {
			return nil, nil, fmt.Errorf("%w: %s", ErrUnknownOption, key)
		}
		if findChoice(opt.Choices, value) == nil {
			return nil, nil, fmt.Errorf("invalid value for %s: %s", key, value)
		}
	}

	// Iterate in schema declaration order for deterministic arg ordering.
	for _, opt := range schema {
		if value, ok := options[opt.Key]; ok {
			choice := findChoice(opt.Choices, value)
			extraArgs = append(extraArgs, choice.FlagArgs...)
			metadata["opt_"+opt.Key] = value
		} else {
			// Use effective default, falling back to schema default.
			defValue := effectiveDefaults[opt.Key]
			if defValue == "" {
				defValue = opt.Default
			}
			if defValue != "" {
				choice := findChoice(opt.Choices, defValue)
				if choice != nil {
					extraArgs = append(extraArgs, choice.FlagArgs...)
				}
			}
			// Defaults are NOT written to metadata -- only explicit choices are persisted.
		}
	}

	return extraArgs, metadata, nil
}

// ResolveExplicitOptions validates user-specified options against a provider's
// schema and produces extra CLI args ONLY for explicitly provided options.
// Unlike ResolveOptions, schema defaults are NOT applied -- only the options
// present in the overrides map generate flags. This is used for template_overrides
// where agent sessions already have their own base CLI flags from config.
//
// Args are emitted in schema declaration order for deterministic command lines.
func ResolveExplicitOptions(schema []ProviderOption, overrides map[string]string) (extraArgs []string, err error) {
	if len(overrides) == 0 {
		return nil, nil
	}

	// Validate override keys and values up front.
	for key, value := range overrides {
		opt := findOption(schema, key)
		if opt == nil {
			return nil, fmt.Errorf("%w: %s", ErrUnknownOption, key)
		}
		if findChoice(opt.Choices, value) == nil {
			return nil, fmt.Errorf("invalid value for %s: %s", key, value)
		}
	}

	// Iterate in schema declaration order for deterministic arg ordering.
	for _, opt := range schema {
		value, ok := overrides[opt.Key]
		if !ok {
			continue
		}
		choice := findChoice(opt.Choices, value)
		extraArgs = append(extraArgs, choice.FlagArgs...)
	}

	return extraArgs, nil
}

// ReplaceSchemaFlags strips all CLI flags associated with the provider's
// OptionsSchema from the command, then appends the given override flags.
func ReplaceSchemaFlags(command string, schema []ProviderOption, overrideArgs []string) string {
	allFlags := CollectAllSchemaFlags(schema)
	stripped := StripFlags(command, allFlags)
	if len(overrideArgs) > 0 {
		stripped = stripped + " " + shellquote.Join(overrideArgs)
	}
	return stripped
}

// CollectAllSchemaFlags gathers all FlagArgs from all choices across all options.
// Multi-flag FlagArgs sequences are split at "--" boundaries so that each
// independent flag group can be matched separately during stripping.
func CollectAllSchemaFlags(schema []ProviderOption) [][]string {
	var flags [][]string
	for _, opt := range schema {
		for _, choice := range opt.Choices {
			if len(choice.FlagArgs) > 0 {
				flags = append(flags, splitFlagArgs(choice.FlagArgs)...)
			}
		}
	}
	return flags
}

// splitFlagArgs splits a FlagArgs slice into independent flag groups at
// "--" prefix boundaries. For example:
//
//	["--ask-for-approval", "untrusted", "--sandbox", "read-only"]
//
// becomes:
//
//	[["--ask-for-approval", "untrusted"], ["--sandbox", "read-only"]]
//
// A single-flag sequence like ["--full-auto"] returns [["--full-auto"]].
func splitFlagArgs(args []string) [][]string {
	if len(args) == 0 {
		return nil
	}
	var groups [][]string
	var current []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") && len(current) > 0 {
			groups = append(groups, current)
			current = nil
		}
		current = append(current, arg)
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// StripFlags removes known flag sequences from a tokenized command.
// Flag sequences are matched greedily in declaration order.
func StripFlags(command string, flags [][]string) string {
	tokens := shellquote.Split(command)
	var result []string
	i := 0
	for i < len(tokens) {
		matched := false
		for _, seq := range flags {
			if len(seq) == 0 {
				continue
			}
			if i+len(seq) > len(tokens) {
				continue
			}
			match := true
			for j, part := range seq {
				if tokens[i+j] != part {
					match = false
					break
				}
			}
			if match {
				i += len(seq)
				matched = true
				break
			}
		}
		if !matched {
			result = append(result, tokens[i])
			i++
		}
	}
	return shellquote.Join(result)
}

// stripArgsSlice removes known flag sequences from an args slice.
// Same logic as StripFlags but operates on []string directly instead
// of a shell-quoted command string. Flag sequences are matched greedily
// in declaration order.
//
// When a flag is stripped and it maps to a known choice value, if
// inferDefaults is non-nil and the corresponding key is not already
// present, the inferred value is set. This preserves user intent
// during the Args-to-OptionDefaults migration (review major 3.1).
func stripArgsSlice(args []string, flags [][]string, schema []ProviderOption, inferDefaults map[string]string) []string {
	var result []string
	i := 0
	for i < len(args) {
		matched := false
		for _, seq := range flags {
			if len(seq) == 0 {
				continue
			}
			if i+len(seq) > len(args) {
				continue
			}
			match := true
			for j, part := range seq {
				if args[i+j] != part {
					match = false
					break
				}
			}
			if match {
				if inferDefaults != nil {
					inferChoiceFromFlags(schema, seq, inferDefaults)
				}
				i += len(seq)
				matched = true
				break
			}
		}
		if !matched {
			result = append(result, args[i])
			i++
		}
	}
	return result
}

// inferChoiceFromFlags finds which schema option+choice produced the given
// flag sequence and, if the key is not already present in defaults, sets
// the inferred value. Only infers from exact full-FlagArgs matches to
// avoid ambiguity with partial multi-flag matches.
func inferChoiceFromFlags(schema []ProviderOption, flagSeq []string, defaults map[string]string) {
	for _, opt := range schema {
		if _, exists := defaults[opt.Key]; exists {
			continue
		}
		for _, choice := range opt.Choices {
			if flagsEqual(choice.FlagArgs, flagSeq) {
				defaults[opt.Key] = choice.Value
				return
			}
		}
	}
}

func flagsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func findOption(schema []ProviderOption, key string) *ProviderOption {
	for i := range schema {
		if schema[i].Key == key {
			return &schema[i]
		}
	}
	return nil
}

func findChoice(choices []OptionChoice, value string) *OptionChoice {
	for i := range choices {
		if choices[i].Value == value {
			return &choices[i]
		}
	}
	return nil
}
