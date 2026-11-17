//go:build windows

package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestOpenOptionValueRefusesWhatPowerShellWouldRead pins the Windows limit on
// open option values. An open value (a model id or effort tier pinned through
// the session API) is joined into the launch command with POSIX quoting and
// the command is re-tokenized by POSIX rules before PowerShell runs it, so a
// value is honored only when it reads as the same literal word under both.
func TestOpenOptionValueRefusesWhatPowerShellWouldRead(t *testing.T) {
	opt := ProviderOption{Key: "model", FlagTemplate: []string{"--model", OptionValuePlaceholder}}
	for _, value := range []string{
		"claude-opus-4-5",
		"claude-sonnet-4-5[1m]",
		"us.anthropic.claude-3-5-sonnet-20241022-v2:0",
		"openrouter/x-ai/grok-4",
		"claude-opus-4@20250514",
		"gpt-5.1-codex+beta=1",
		"claude-opus-4-8[context=1m,effort=high]",
	} {
		if args, ok := OptionFlagArgs(opt, value); !ok || len(args) != 2 || args[1] != value {
			t.Errorf("OptionFlagArgs(%q) = %q, %v; want the value honored", value, args, ok)
		}
		if strings.ContainsRune(value, ',') && !strings.HasPrefix(shellquote.Join([]string{value}), "'") {
			t.Errorf("shellquote.Join leaves %q bare; PowerShell would read its comma as an array", value)
		}
	}
	for _, value := range []string{
		"x\u2019;New-Item -ItemType File -Path pwned;\u2019",
		"x';New-Item -ItemType File -Path pwned;'",
		"\u2018x\u2019",
		"\u201Cx\u201D",
		`x"y`,
		"a,b",
		"@splat",
		"-flag",
		"a b",
		"$(calc)",
		"a`b",
		"line\nbreak",
	} {
		if args, ok := OptionFlagArgs(opt, value); ok {
			t.Errorf("OptionFlagArgs(%q) = %q, honored; want it refused", value, args)
		}
	}
}

// TestOpenOptionRefusalNamesTheWindowsRule pins that a value refused only by
// the Windows plain-word rule says so, wherever it is refused, so a user with a
// legitimate path-like or spaced id learns why rather than seeing a typo's
// message. A value a closed option does not list keeps the plain wording.
func TestOpenOptionRefusalNamesTheWindowsRule(t *testing.T) {
	const rule = "plain word"
	open := ProviderOption{Key: "model", FlagTemplate: []string{"--model", OptionValuePlaceholder}}
	schema := []ProviderOption{open}

	if _, err := ResolveExplicitOptions(schema, map[string]string{"model": `C:\models\x.gguf`}); err == nil || !strings.Contains(err.Error(), rule) {
		t.Errorf("ResolveExplicitOptions = %v, want it to name the Windows rule", err)
	}
	if _, _, err := ResolveOptions(schema, map[string]string{"model": "my model"}, nil); err == nil || !strings.Contains(err.Error(), rule) {
		t.Errorf("ResolveOptions = %v, want it to name the Windows rule", err)
	}
	if err := ValidateOptionDefaults(schema, map[string]string{"model": "~/m.gguf"}); err == nil || !strings.Contains(err.Error(), rule) {
		t.Errorf("ValidateOptionDefaults = %v, want it to name the Windows rule", err)
	}
	withDefault := open
	withDefault.Default = "my model"
	if err := ValidateOptionsSchema([]ProviderOption{withDefault}); err == nil || !strings.Contains(err.Error(), rule) {
		t.Errorf("ValidateOptionsSchema = %v, want it to name the Windows rule", err)
	}
	pins := (&ResolvedProvider{OptionsSchema: schema, EffectiveDefaults: map[string]string{"model": "my model"}}).UnhonoredOptionPins()
	if len(pins) != 1 || pins[0].Reason != "unsafe_on_platform" {
		t.Fatalf("UnhonoredOptionPins = %+v, want one unsafe_on_platform pin", pins)
	}
	if msg := FormatUnhonoredOptionPin("worker", "agent", pins[0]); !strings.Contains(msg, rule) {
		t.Errorf("FormatUnhonoredOptionPin = %q, want it to name the Windows rule", msg)
	}

	closed := []ProviderOption{{Key: "effort", Choices: []OptionChoice{{Value: "high", FlagArgs: []string{"--effort", "high"}}}}}
	if _, err := ResolveExplicitOptions(closed, map[string]string{"effort": "my effort"}); err == nil || strings.Contains(err.Error(), rule) {
		t.Errorf("ResolveExplicitOptions(closed) = %v, want the plain invalid-value error", err)
	}
}
