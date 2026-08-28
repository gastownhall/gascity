package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// ValidateOptionsSchema checks that every option default resolves to a declared choice.
// Call at config load time to catch misconfigured providers early.
func ValidateOptionsSchema(schema []ProviderOption) error {
	for _, opt := range schema {
		if opt.Default == "" {
			continue
		}
		if _, ok := OptionFlagArgs(opt, opt.Default); !ok {
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
		if _, ok := OptionFlagArgs(*opt, value); !ok {
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
		if _, ok := OptionFlagArgs(*opt, value); !ok {
			return nil, nil, fmt.Errorf("invalid value for %s: %s", key, value)
		}
	}

	// Iterate in schema declaration order for deterministic arg ordering.
	for _, opt := range schema {
		if value, ok := options[opt.Key]; ok {
			flagArgs, _ := OptionFlagArgs(opt, value)
			extraArgs = append(extraArgs, flagArgs...)
			metadata[beadmeta.OptionMetadataPrefix+opt.Key] = value
		} else {
			// Use effective default, falling back to schema default.
			defValue := effectiveDefaults[opt.Key]
			if defValue == "" {
				defValue = opt.Default
			}
			if flagArgs, ok := OptionFlagArgs(opt, defValue); ok {
				extraArgs = append(extraArgs, flagArgs...)
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
		if _, ok := OptionFlagArgs(*opt, value); !ok {
			return nil, fmt.Errorf("invalid value for %s: %s", key, value)
		}
	}

	// Iterate in schema declaration order for deterministic arg ordering.
	for _, opt := range schema {
		value, ok := overrides[opt.Key]
		if !ok {
			continue
		}
		flagArgs, _ := OptionFlagArgs(opt, value)
		extraArgs = append(extraArgs, flagArgs...)
	}

	return extraArgs, nil
}

func completeResumeCommandDefaults(command, resumeFlag, resumeStyle string, schema []ProviderOption, effectiveDefaults map[string]string) string {
	if strings.TrimSpace(command) == "" || len(schema) == 0 || len(effectiveDefaults) == 0 {
		return command
	}
	missingArgs := missingDefaultArgsForCommand(command, schema, effectiveDefaults)
	if len(missingArgs) == 0 {
		return command
	}
	tokens := shellquote.Split(command)
	insertAt := len(tokens)
	if resumeStyle == "subcommand" && resumeFlag != "" {
		insertAt = subcommandResumeInsertIndex(tokens, resumeFlag)
	}
	out := make([]string, 0, len(tokens)+len(missingArgs))
	out = append(out, tokens[:insertAt]...)
	out = append(out, missingArgs...)
	out = append(out, tokens[insertAt:]...)
	joined := shellquote.Join(out)
	return strings.ReplaceAll(joined, "'{{.SessionKey}}'", "{{.SessionKey}}")
}

func subcommandResumeInsertIndex(tokens []string, resumeFlag string) int {
	sessionIndex := -1
	for i, token := range tokens {
		if token == "{{.SessionKey}}" {
			sessionIndex = i
			break
		}
	}
	if sessionIndex >= 0 {
		for i := sessionIndex - 1; i >= 0; i-- {
			if tokens[i] == resumeFlag {
				return i + 1
			}
		}
		return sessionIndex
	}
	for i := len(tokens) - 1; i >= 0; i-- {
		if tokens[i] == resumeFlag {
			return i + 1
		}
	}
	return len(tokens)
}

func missingDefaultArgsForCommand(command string, schema []ProviderOption, effectiveDefaults map[string]string) []string {
	tokens := shellquote.Split(command)
	var missing []string
	for _, opt := range schema {
		if commandContainsOption(tokens, opt) {
			continue
		}
		value := effectiveDefaults[opt.Key]
		if value == "" {
			value = opt.Default
		}
		if value == "" {
			continue
		}
		flagArgs, ok := OptionFlagArgs(opt, value)
		if !ok || len(flagArgs) == 0 {
			continue
		}
		missing = append(missing, flagArgs...)
	}
	return missing
}

func commandContainsOption(tokens []string, opt ProviderOption) bool {
	for _, choice := range opt.Choices {
		if commandContainsChoice(tokens, choice) {
			return true
		}
	}
	return false
}

func commandContainsChoice(tokens []string, choice OptionChoice) bool {
	for _, groups := range choiceGroupedFlagSequences(choice) {
		if len(groups) == 0 {
			continue
		}
		if len(groups) == 1 {
			if tokenSequenceShapeContains(tokens, groups[0]) {
				return true
			}
			continue
		}
		if tokenSequenceGroupsShapeContain(tokens, groups) {
			return true
		}
	}
	return false
}

func tokenSequenceShapeContains(tokens, seq []string) bool {
	for i := 0; i+len(seq) <= len(tokens); i++ {
		if tokenSequenceShapeMatchesAt(tokens, i, seq, nil) {
			return true
		}
	}
	return false
}

func tokenSequenceGroupsShapeContain(tokens []string, groups [][]string) bool {
	used := make([]bool, len(tokens))
	for _, group := range groups {
		start, ok := findTokenSequenceShapeInArgs(tokens, group, used)
		if !ok {
			return false
		}
		for i := start; i < start+len(group); i++ {
			used[i] = true
		}
	}
	return true
}

func findTokenSequenceShapeInArgs(args, seq []string, used []bool) (int, bool) {
	for i := 0; i+len(seq) <= len(args); i++ {
		if tokenSequenceShapeMatchesAt(args, i, seq, used) {
			return i, true
		}
	}
	return 0, false
}

func tokenSequenceShapeMatchesAt(tokens []string, start int, seq []string, used []bool) bool {
	if len(seq) == 0 || start+len(seq) > len(tokens) {
		return false
	}
	for i, want := range seq {
		if used != nil && used[start+i] {
			return false
		}
		got := tokens[start+i]
		if i == 0 || strings.HasPrefix(want, "-") {
			if prefix, ok := assignmentPrefix(want); ok {
				if !strings.HasPrefix(got, prefix) {
					return false
				}
				continue
			}
			if got != want {
				return false
			}
			continue
		}
		if prefix, ok := assignmentPrefix(want); ok {
			if !strings.HasPrefix(got, prefix) {
				return false
			}
			continue
		}
		if got == "" || strings.HasPrefix(got, "-") || isTemplateToken(got) {
			return false
		}
	}
	return true
}

func isTemplateToken(token string) bool {
	return strings.HasPrefix(token, "{{") && strings.HasSuffix(token, "}}")
}

func assignmentPrefix(token string) (string, bool) {
	idx := strings.Index(token, "=")
	if idx <= 0 {
		return "", false
	}
	return token[:idx+1], true
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

// CollectAllSchemaFlags gathers all FlagArgs and FlagAliases from all choices
// across all options. Multi-flag sequences are split at "--" boundaries so that
// each independent flag group can be matched separately during stripping.
func CollectAllSchemaFlags(schema []ProviderOption) [][]string {
	var flags [][]string
	seen := make(map[string]bool)
	for _, opt := range schema {
		for _, choice := range opt.Choices {
			for _, seq := range choiceFlagSequences(choice) {
				key := strings.Join(seq, "\x00")
				if seen[key] {
					continue
				}
				seen[key] = true
				flags = append(flags, cloneStrings(seq))
			}
		}
	}
	return flags
}

func choiceFlagSequences(choice OptionChoice) [][]string {
	var sequences [][]string
	sequences = append(sequences, splitFlagArgs(choice.FlagArgs)...)
	for _, alias := range choice.FlagAliases {
		sequences = append(sequences, splitFlagArgs(alias)...)
	}
	return sequences
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
// inferDefaults is non-nil the inferred value is set. Explicit provider args
// are the leaf layer in provider inheritance, so they must override defaults
// inherited from a base provider.
func stripArgsSlice(args []string, flags [][]string, schema []ProviderOption, inferDefaults map[string]string) []string {
	if args == nil {
		return nil
	}
	if inferDefaults != nil {
		inferChoicesFromArgs(schema, args, inferDefaults)
	}
	result := make([]string, 0, len(args))
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

func inferChoicesFromArgs(schema []ProviderOption, args []string, defaults map[string]string) {
	covered, groupedLastStart := inferGroupedChoicesFromArgs(schema, args, defaults)
	for i := 0; i < len(args); {
		if covered[i] {
			i++
			continue
		}
		match, ok := longestChoiceMatchAt(schema, args, i, covered)
		if !ok {
			i++
			continue
		}
		if lastStart, ok := groupedLastStart[match.key]; ok && i < lastStart {
			i += match.length
			continue
		}
		defaults[match.key] = match.value
		i += match.length
	}
}

type tokenSpan struct {
	start int
	end   int
}

type groupedChoiceMatch struct {
	key        string
	value      string
	spans      []tokenSpan
	tokenCount int
	lastStart  int
}

func inferGroupedChoicesFromArgs(schema []ProviderOption, args []string, defaults map[string]string) ([]bool, map[string]int) {
	covered := make([]bool, len(args))
	groupedLastStart := make(map[string]int)
	for _, opt := range schema {
		var best groupedChoiceMatch
		found := false
		for _, choice := range opt.Choices {
			for _, groups := range choiceGroupedFlagSequences(choice) {
				if len(groups) < 2 {
					continue
				}
				spans, ok := findFlagGroupsInArgs(args, groups)
				if !ok {
					continue
				}
				candidate := groupedChoiceMatch{
					key:   opt.Key,
					value: choice.Value,
					spans: spans,
				}
				for _, span := range spans {
					candidate.tokenCount += span.end - span.start
					if span.start > candidate.lastStart {
						candidate.lastStart = span.start
					}
				}
				if !found || betterGroupedChoice(candidate, best) {
					best = candidate
					found = true
				}
			}
		}
		if !found {
			continue
		}
		defaults[best.key] = best.value
		groupedLastStart[best.key] = best.lastStart
		for _, span := range best.spans {
			for i := span.start; i < span.end; i++ {
				covered[i] = true
			}
		}
	}
	return covered, groupedLastStart
}

func betterGroupedChoice(candidate, current groupedChoiceMatch) bool {
	if candidate.tokenCount != current.tokenCount {
		return candidate.tokenCount > current.tokenCount
	}
	return candidate.lastStart > current.lastStart
}

func choiceGroupedFlagSequences(choice OptionChoice) [][][]string {
	var sequences [][][]string
	if groups := splitFlagArgs(choice.FlagArgs); len(groups) > 0 {
		sequences = append(sequences, groups)
	}
	for _, alias := range choice.FlagAliases {
		if groups := splitFlagArgs(alias); len(groups) > 0 {
			sequences = append(sequences, groups)
		}
	}
	return sequences
}

func findFlagGroupsInArgs(args []string, groups [][]string) ([]tokenSpan, bool) {
	used := make([]bool, len(args))
	spans := make([]tokenSpan, 0, len(groups))
	for _, group := range groups {
		start, ok := findTokenSequenceInArgs(args, group, used)
		if !ok {
			return nil, false
		}
		end := start + len(group)
		for i := start; i < end; i++ {
			used[i] = true
		}
		spans = append(spans, tokenSpan{start: start, end: end})
	}
	return spans, true
}

func findTokenSequenceInArgs(args, seq []string, used []bool) (int, bool) {
	for i := 0; i+len(seq) <= len(args); i++ {
		if tokenSequenceMatchesAt(args, i, seq, used) {
			return i, true
		}
	}
	return 0, false
}

type choiceMatch struct {
	key    string
	value  string
	length int
}

func longestChoiceMatchAt(schema []ProviderOption, args []string, start int, covered []bool) (choiceMatch, bool) {
	var best choiceMatch
	for _, opt := range schema {
		for _, choice := range opt.Choices {
			for _, seq := range choiceFullFlagSequences(choice) {
				if len(seq) <= best.length || !tokenSequenceMatchesAt(args, start, seq, covered) {
					continue
				}
				best = choiceMatch{
					key:    opt.Key,
					value:  choice.Value,
					length: len(seq),
				}
			}
		}
	}
	return best, best.length > 0
}

func choiceFullFlagSequences(choice OptionChoice) [][]string {
	var sequences [][]string
	if len(choice.FlagArgs) > 0 {
		sequences = append(sequences, choice.FlagArgs)
	}
	for _, alias := range choice.FlagAliases {
		if len(alias) > 0 {
			sequences = append(sequences, alias)
		}
	}
	return sequences
}

func tokenSequenceMatchesAt(tokens []string, start int, seq []string, covered []bool) bool {
	if len(seq) == 0 || start+len(seq) > len(tokens) {
		return false
	}
	for i, want := range seq {
		if covered != nil && covered[start+i] {
			return false
		}
		if tokens[start+i] != want {
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

// OptionValuePlaceholder is the token substituted with the pinned value when
// rendering ProviderOption.FlagTemplate.
const OptionValuePlaceholder = "{value}"

// IsOpenOption reports whether the option honors values outside its declared
// Choices by rendering ProviderOption.FlagTemplate.
func IsOpenOption(opt ProviderOption) bool {
	return len(opt.FlagTemplate) > 0
}

// OptionFlagArgs resolves one option value to the CLI args that pin it.
//
// A declared choice always wins, so curated entries keep their exact flag
// shape (including forms that are not a plain "--flag value", such as an
// alias that expands to a different id or codex's -c assignment). A value
// with no matching choice resolves only when the option is open, by
// rendering FlagTemplate.
//
// The second return is false when the value cannot be honored at all. Callers
// must never treat that as success: omitting the flag silently unpins the
// agent onto the provider default (ga-fyh).
func OptionFlagArgs(opt ProviderOption, value string) ([]string, bool) {
	// A declared choice wins even when it is the empty "Default" entry, which
	// several schemas expose as an explicitly selectable "leave unset" value.
	if choice := findChoice(opt.Choices, value); choice != nil {
		return cloneStrings(choice.FlagArgs), true
	}
	if value == "" || !IsOpenOption(opt) {
		return nil, false
	}
	return renderFlagTemplate(opt.FlagTemplate, value), true
}

// renderFlagTemplate substitutes value for every OptionValuePlaceholder token.
// Substitution is whole-token: a template entry is either the literal flag or
// the placeholder, never a concatenation, except that a placeholder embedded
// in a longer token (codex's "model_reasoning_effort={value}") is expanded in
// place.
func renderFlagTemplate(template []string, value string) []string {
	out := make([]string, len(template))
	for i, tok := range template {
		out[i] = strings.ReplaceAll(tok, OptionValuePlaceholder, value)
	}
	return out
}

func findChoice(choices []OptionChoice, value string) *OptionChoice {
	for i := range choices {
		if choices[i].Value == value {
			return &choices[i]
		}
	}
	return nil
}

// Unhonored pin reasons. Launch used to skip these silently (ra-jbbv0 / ga-fyh).
const (
	UnhonoredPinUnknownOption = "unknown_option"
	UnhonoredPinUnknownValue  = "unknown_value"
)

// UnhonoredOptionPin is an EffectiveDefaults entry that cannot be turned into
// FlagArgs. Callers must surface these — omitting the flag without a warning
// silently unpins the agent onto the provider default.
type UnhonoredOptionPin struct {
	Key    string
	Value  string
	Reason string
	Valid  []string
}

// UnhonoredOptionPins reports EffectiveDefaults that ResolveDefaultArgs will
// not emit as flags: unknown option keys, and known keys whose value is not
// a declared choice. Empty values are unset, not unhonored.
func (rp *ResolvedProvider) UnhonoredOptionPins() []UnhonoredOptionPin {
	if rp == nil {
		return nil
	}
	var pins []UnhonoredOptionPin
	seen := make(map[string]bool, len(rp.OptionsSchema))
	for _, opt := range rp.OptionsSchema {
		seen[opt.Key] = true
		value := rp.EffectiveDefaults[opt.Key]
		if value == "" {
			continue
		}
		if _, ok := OptionFlagArgs(opt, value); ok {
			continue
		}
		pins = append(pins, UnhonoredOptionPin{
			Key:    opt.Key,
			Value:  value,
			Reason: UnhonoredPinUnknownValue,
			Valid:  nonEmptyChoiceValues(opt),
		})
	}
	var extra []string
	for key, value := range rp.EffectiveDefaults {
		if value == "" || seen[key] {
			continue
		}
		extra = append(extra, key)
	}
	sort.Strings(extra)
	for _, key := range extra {
		pins = append(pins, UnhonoredOptionPin{
			Key:    key,
			Value:  rp.EffectiveDefaults[key],
			Reason: UnhonoredPinUnknownOption,
		})
	}
	return pins
}

// FormatUnhonoredOptionPin names the agent, rejected value, and valid set so
// a stale enum cannot unpin a session without a loud startup warning.
func FormatUnhonoredOptionPin(agentName, providerName string, pin UnhonoredOptionPin) string {
	agent := strings.TrimSpace(agentName)
	if agent == "" {
		agent = "(unknown)"
	}
	provider := strings.TrimSpace(providerName)
	if provider == "" {
		provider = "(unknown)"
	}
	switch pin.Reason {
	case UnhonoredPinUnknownOption:
		return fmt.Sprintf("WARNING: agent %q: option_defaults %s=%q is not in the %q provider schema; flag omitted", agent, pin.Key, pin.Value, provider)
	default:
		valid := strings.Join(pin.Valid, ", ")
		if valid == "" {
			valid = "(none)"
		}
		return fmt.Sprintf("WARNING: agent %q: option_defaults %s=%q is not a valid choice for provider %q (valid: %s); flag omitted", agent, pin.Key, pin.Value, provider, valid)
	}
}

func nonEmptyChoiceValues(opt ProviderOption) []string {
	var out []string
	for _, c := range opt.Choices {
		if c.Value != "" {
			out = append(out, c.Value)
		}
	}
	return out
}
