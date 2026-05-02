package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveOptions_ExplicitValues(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "permission_mode", Label: "Permission Mode", Type: "select",
			Default: "auto-edit",
			Choices: []OptionChoice{
				{Value: "auto-edit", Label: "Edit automatically", FlagArgs: []string{"--permission-mode", "auto-edit"}},
				{Value: "plan", Label: "Plan mode", FlagArgs: []string{"--permission-mode", "plan"}},
			},
		},
		{
			Key: "thinking", Label: "Thinking", Type: "select",
			Default: "",
			Choices: []OptionChoice{
				{Value: "", Label: "Default", FlagArgs: nil},
				{Value: "high", Label: "High", FlagArgs: []string{"--thinking", "high"}},
			},
		},
	}

	args, meta, err := ResolveOptions(schema, map[string]string{
		"permission_mode": "plan",
		"thinking":        "high",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Args must be in schema declaration order (deterministic).
	wantArgs := []string{"--permission-mode", "plan", "--thinking", "high"}
	if len(args) != len(wantArgs) {
		t.Fatalf("got args=%v, want %v", args, wantArgs)
	}
	for i, w := range wantArgs {
		if args[i] != w {
			t.Errorf("args[%d]=%q, want %q (full: %v)", i, args[i], w, args)
		}
	}

	// Metadata should have explicit choices.
	if meta["opt_permission_mode"] != "plan" {
		t.Errorf("got meta opt_permission_mode=%q, want plan", meta["opt_permission_mode"])
	}
	if meta["opt_thinking"] != "high" {
		t.Errorf("got meta opt_thinking=%q, want high", meta["opt_thinking"])
	}
}

func TestResolveOptions_DefaultsApplied(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "permission_mode", Label: "Permission Mode", Type: "select",
			Default: "auto-edit",
			Choices: []OptionChoice{
				{Value: "auto-edit", Label: "Edit automatically", FlagArgs: []string{"--permission-mode", "auto-edit"}},
			},
		},
		{
			Key: "thinking", Label: "Thinking", Type: "select",
			Default: "", // empty default — no args injected
			Choices: []OptionChoice{
				{Value: "", Label: "Default", FlagArgs: nil},
			},
		},
	}

	args, meta, err := ResolveOptions(schema, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// permission_mode default should inject args.
	if len(args) != 2 || args[0] != "--permission-mode" || args[1] != "auto-edit" {
		t.Errorf("got args=%v, want [--permission-mode auto-edit]", args)
	}

	// Defaults should NOT be in metadata.
	if len(meta) != 0 {
		t.Errorf("got meta=%v, want empty (defaults not persisted)", meta)
	}
}

func TestResolveOptions_EffectiveDefaultsOverrideSchemaDefaults(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "permission_mode", Label: "Permission Mode", Type: "select",
			Default: "auto-edit",
			Choices: []OptionChoice{
				{Value: "auto-edit", Label: "Edit automatically", FlagArgs: []string{"--permission-mode", "auto-edit"}},
				{Value: "unrestricted", Label: "Bypass permissions", FlagArgs: []string{"--dangerously-skip-permissions"}},
			},
		},
	}

	effectiveDefaults := map[string]string{"permission_mode": "unrestricted"}

	// No user options: should use effective defaults, not schema defaults.
	args, _, err := ResolveOptions(schema, nil, effectiveDefaults)
	if err != nil {
		t.Fatal(err)
	}

	wantArgs := []string{"--dangerously-skip-permissions"}
	if len(args) != len(wantArgs) {
		t.Fatalf("got args=%v, want %v", args, wantArgs)
	}
	for i, w := range wantArgs {
		if args[i] != w {
			t.Errorf("args[%d]=%q, want %q", i, args[i], w)
		}
	}
}

func TestReplaceSchemaFlagsStripsCodexAliases(t *testing.T) {
	codex := BuiltinProviders()["codex"]
	defaultArgs := []string{
		"--dangerously-bypass-approvals-and-sandbox",
		"--model", "gpt-5.5",
		"-c", "model_reasoning_effort=xhigh",
	}

	got := ReplaceSchemaFlags(
		`aimux run codex -- -m gpt-5.5 -c 'model_reasoning_effort="xhigh"'`,
		codex.OptionsSchema,
		defaultArgs,
	)

	if strings.Count(got, "gpt-5.5") != 1 {
		t.Fatalf("ReplaceSchemaFlags() = %q, want one model flag", got)
	}
	if strings.Count(got, "model_reasoning_effort") != 1 {
		t.Fatalf("ReplaceSchemaFlags() = %q, want one effort flag", got)
	}
	if !strings.Contains(got, "--model gpt-5.5") {
		t.Fatalf("ReplaceSchemaFlags() = %q, want canonical model flag", got)
	}
	if strings.Contains(got, "-m gpt-5.5") || strings.Contains(got, `model_reasoning_effort=\"xhigh\"`) {
		t.Fatalf("ReplaceSchemaFlags() = %q, retained non-canonical schema flag", got)
	}
}

func TestCollectAllSchemaFlagsUsesDeclaredFlagAliases(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "model",
			Choices: []OptionChoice{
				{
					Value:       "opus",
					FlagArgs:    []string{"--model", "opus"},
					FlagAliases: [][]string{{"-m", "opus"}},
				},
			},
		},
	}

	flags := CollectAllSchemaFlags(schema)
	got := StripFlags("agent -m opus --other", flags)

	if got != "agent --other" {
		t.Fatalf("StripFlags() = %q, want alias stripped", got)
	}
}

func TestCollectAllSchemaFlagsDoesNotInferUndeclaredProviderAliases(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "model",
			Choices: []OptionChoice{
				{Value: "opus", FlagArgs: []string{"--model", "opus"}},
			},
		},
	}

	flags := CollectAllSchemaFlags(schema)
	got := StripFlags("agent -m opus --other", flags)

	if got != "agent -m opus --other" {
		t.Fatalf("StripFlags() = %q, want undeclared alias preserved", got)
	}
}

func TestStripArgsSliceInfersChoiceFromDeclaredAlias(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "model",
			Choices: []OptionChoice{
				{
					Value:       "opus",
					FlagArgs:    []string{"--model", "opus"},
					FlagAliases: [][]string{{"-m", "opus"}},
				},
			},
		},
	}
	flags := CollectAllSchemaFlags(schema)
	inferred := make(map[string]string)

	got := stripArgsSlice([]string{"run", "-m", "opus", "--other"}, flags, schema, inferred)

	want := []string{"run", "--other"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stripArgsSlice() = %v, want %v", got, want)
	}
	if inferred["model"] != "opus" {
		t.Fatalf("inferred model = %q, want opus", inferred["model"])
	}
}

func TestResolveOptions_UserOptionOverridesEffectiveDefault(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "permission_mode", Label: "Permission Mode", Type: "select",
			Default: "auto-edit",
			Choices: []OptionChoice{
				{Value: "auto-edit", Label: "Edit automatically", FlagArgs: []string{"--permission-mode", "auto-edit"}},
				{Value: "plan", Label: "Plan mode", FlagArgs: []string{"--permission-mode", "plan"}},
				{Value: "unrestricted", Label: "Bypass permissions", FlagArgs: []string{"--dangerously-skip-permissions"}},
			},
		},
	}

	effectiveDefaults := map[string]string{"permission_mode": "unrestricted"}

	// User explicitly selects plan -- should override effective default.
	args, meta, err := ResolveOptions(schema, map[string]string{
		"permission_mode": "plan",
	}, effectiveDefaults)
	if err != nil {
		t.Fatal(err)
	}

	wantArgs := []string{"--permission-mode", "plan"}
	if len(args) != len(wantArgs) {
		t.Fatalf("got args=%v, want %v", args, wantArgs)
	}
	if meta["opt_permission_mode"] != "plan" {
		t.Errorf("meta should record explicit choice, got %q", meta["opt_permission_mode"])
	}
}

func TestResolveOptions_UnknownOption(t *testing.T) {
	schema := []ProviderOption{
		{Key: "mode", Choices: []OptionChoice{{Value: "a"}}},
	}
	_, _, err := ResolveOptions(schema, map[string]string{"bogus": "val"}, nil)
	if err == nil {
		t.Fatal("expected error for unknown option")
	}
}

func TestResolveOptions_InvalidValue(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "mode", Choices: []OptionChoice{
				{Value: "a", Label: "A"},
				{Value: "b", Label: "B"},
			},
		},
	}
	_, _, err := ResolveOptions(schema, map[string]string{"mode": "c"}, nil)
	if err == nil {
		t.Fatal("expected error for invalid value")
	}
}

func TestResolveOptions_EmptyStringChoice(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "thinking", Default: "",
			Choices: []OptionChoice{
				{Value: "", Label: "Default", FlagArgs: nil},
				{Value: "high", Label: "High", FlagArgs: []string{"--thinking", "high"}},
			},
		},
	}

	// Explicit empty string should be accepted (not rejected as "invalid").
	args, meta, err := ResolveOptions(schema, map[string]string{"thinking": ""}, nil)
	if err != nil {
		t.Fatalf("empty string choice should be valid: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("empty string choice should produce no args, got %v", args)
	}
	if meta["opt_thinking"] != "" {
		t.Errorf("explicit empty choice should be in metadata")
	}
	if _, ok := meta["opt_thinking"]; !ok {
		t.Error("explicit empty choice key should exist in metadata")
	}
}

func TestResolveOptions_NilSchema(t *testing.T) {
	args, meta, err := ResolveOptions(nil, map[string]string{"anything": "val"}, nil)
	if err == nil {
		t.Fatal("expected error for option against nil schema")
	}
	_ = args
	_ = meta
}

func TestResolveExplicitOptions_OnlyExplicit(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "permission_mode", Label: "Permission Mode", Type: "select",
			Default: "auto-edit",
			Choices: []OptionChoice{
				{Value: "auto-edit", Label: "Edit automatically", FlagArgs: []string{"--permission-mode", "auto-edit"}},
				{Value: "plan", Label: "Plan mode", FlagArgs: []string{"--permission-mode", "plan"}},
			},
		},
		{
			Key: "effort", Label: "Effort", Type: "select",
			Default: "",
			Choices: []OptionChoice{
				{Value: "", Label: "Default", FlagArgs: nil},
				{Value: "high", Label: "High", FlagArgs: []string{"--effort", "high"}},
			},
		},
		{
			Key: "model", Label: "Model", Type: "select",
			Default: "",
			Choices: []OptionChoice{
				{Value: "", Label: "Default", FlagArgs: nil},
				{Value: "opus", Label: "Opus", FlagArgs: []string{"--model", "claude-opus-4-6"}},
			},
		},
	}

	// Only override effort — permission_mode default must NOT be injected.
	args, err := ResolveExplicitOptions(schema, map[string]string{
		"effort": "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"--effort", "high"}
	if len(args) != len(wantArgs) {
		t.Fatalf("got args=%v, want %v", args, wantArgs)
	}
	for i, w := range wantArgs {
		if args[i] != w {
			t.Errorf("args[%d]=%q, want %q", i, args[i], w)
		}
	}
}

func TestResolveExplicitOptions_EmptyOverrides(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "mode", Default: "auto",
			Choices: []OptionChoice{
				{Value: "auto", FlagArgs: []string{"--mode", "auto"}},
			},
		},
	}
	args, err := ResolveExplicitOptions(schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 0 {
		t.Errorf("empty overrides should produce no args, got %v", args)
	}
}

func TestResolveExplicitOptions_UnknownKey(t *testing.T) {
	schema := []ProviderOption{
		{Key: "mode", Choices: []OptionChoice{{Value: "a"}}},
	}
	_, err := ResolveExplicitOptions(schema, map[string]string{"bogus": "val"})
	if err == nil {
		t.Fatal("expected error for unknown option")
	}
	if !strings.Contains(err.Error(), "unknown option") {
		t.Errorf("expected unknown option error, got: %v", err)
	}
}

func TestResolveExplicitOptions_InvalidValue(t *testing.T) {
	schema := []ProviderOption{
		{Key: "mode", Choices: []OptionChoice{{Value: "a"}, {Value: "b"}}},
	}
	_, err := ResolveExplicitOptions(schema, map[string]string{"mode": "c"})
	if err == nil {
		t.Fatal("expected error for invalid value")
	}
}

func TestResolveExplicitOptions_EmptyStringChoice(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "effort", Default: "",
			Choices: []OptionChoice{
				{Value: "", Label: "Default", FlagArgs: nil},
				{Value: "high", Label: "High", FlagArgs: []string{"--effort", "high"}},
			},
		},
	}
	// Explicit empty string should produce no flags (FlagArgs is nil).
	args, err := ResolveExplicitOptions(schema, map[string]string{"effort": ""})
	if err != nil {
		t.Fatalf("empty string choice should be valid: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("empty string choice should produce no args, got %v", args)
	}
}

func TestResolveExplicitOptions_SchemaOrder(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "effort", Choices: []OptionChoice{
				{Value: "high", FlagArgs: []string{"--effort", "high"}},
			},
		},
		{
			Key: "model", Choices: []OptionChoice{
				{Value: "opus", FlagArgs: []string{"--model", "opus"}},
			},
		},
	}
	// Override both in reverse declaration order — args should be in schema order.
	args, err := ResolveExplicitOptions(schema, map[string]string{
		"model":  "opus",
		"effort": "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"--effort", "high", "--model", "opus"}
	if len(args) != len(wantArgs) {
		t.Fatalf("got args=%v, want %v", args, wantArgs)
	}
	for i, w := range wantArgs {
		if args[i] != w {
			t.Errorf("args[%d]=%q, want %q", i, args[i], w)
		}
	}
}

func TestValidateOptionsSchema_ValidDefaults(t *testing.T) {
	schema := []ProviderOption{
		{Key: "mode", Default: "a", Choices: []OptionChoice{{Value: "a"}, {Value: "b"}}},
		{Key: "empty", Default: "", Choices: []OptionChoice{{Value: ""}}},
	}
	if err := ValidateOptionsSchema(schema); err != nil {
		t.Fatalf("valid schema should pass: %v", err)
	}
}

func TestValidateOptionsSchema_InvalidDefault(t *testing.T) {
	schema := []ProviderOption{
		{Key: "mode", Default: "missing", Choices: []OptionChoice{{Value: "a"}}},
	}
	err := ValidateOptionsSchema(schema)
	if err == nil {
		t.Fatal("expected error for invalid default")
	}
	if !strings.Contains(err.Error(), "not a valid choice") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateOptionsSchema_NoDefault(t *testing.T) {
	schema := []ProviderOption{
		{Key: "mode", Default: "", Choices: []OptionChoice{{Value: "a"}}},
	}
	if err := ValidateOptionsSchema(schema); err != nil {
		t.Fatalf("empty default should pass: %v", err)
	}
}

func TestResolveExplicitOptions_SubsetOfOptions(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "permission_mode", Label: "Permission Mode", Type: "select",
			Default: "auto-edit",
			Choices: []OptionChoice{
				{Value: "auto-edit", Label: "Edit automatically", FlagArgs: []string{"--permission-mode", "auto-edit"}},
				{Value: "plan", Label: "Plan mode", FlagArgs: []string{"--permission-mode", "plan"}},
			},
		},
		{
			Key: "model", Label: "Model", Type: "select",
			Default: "",
			Choices: []OptionChoice{
				{Value: "", Label: "Default", FlagArgs: nil},
				{Value: "opus", Label: "Opus", FlagArgs: []string{"--model", "claude-opus-4-6"}},
			},
		},
	}

	// Only specify model, not permission_mode.
	args, err := ResolveExplicitOptions(schema, map[string]string{"model": "opus"})
	if err != nil {
		t.Fatal(err)
	}

	// Should only return model flags, not permission_mode defaults.
	wantArgs := []string{"--model", "claude-opus-4-6"}
	if len(args) != len(wantArgs) {
		t.Fatalf("got args=%v, want %v", args, wantArgs)
	}
	for i, w := range wantArgs {
		if args[i] != w {
			t.Errorf("args[%d]=%q, want %q", i, args[i], w)
		}
	}
}

func TestResolveExplicitOptions_InvalidKey(t *testing.T) {
	schema := []ProviderOption{
		{Key: "mode", Choices: []OptionChoice{{Value: "a"}}},
	}
	_, err := ResolveExplicitOptions(schema, map[string]string{"bogus": "val"})
	if err == nil {
		t.Fatal("expected error for unknown option")
	}
}

func TestResolveExplicitOptions_EmptyMap(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "mode", Default: "a",
			Choices: []OptionChoice{
				{Value: "a", FlagArgs: []string{"--mode", "a"}},
			},
		},
	}
	args, err := ResolveExplicitOptions(schema, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if args != nil {
		t.Errorf("expected nil args for empty options, got %v", args)
	}
}

// --- New tests for schema-authoritative defaults ---

func TestComputeEffectiveDefaults_AllThreeLayers(t *testing.T) {
	schema := []ProviderOption{
		{Key: "permission_mode", Default: "auto-edit"},
		{Key: "model", Default: ""},
		{Key: "effort", Default: ""},
	}
	providerDefaults := map[string]string{
		"permission_mode": "unrestricted",
		"model":           "sonnet",
	}
	agentDefaults := map[string]string{
		"model":  "opus",
		"effort": "high",
	}

	result := ComputeEffectiveDefaults(schema, providerDefaults, agentDefaults)

	// permission_mode: schema=auto-edit, provider=unrestricted -> unrestricted
	if result["permission_mode"] != "unrestricted" {
		t.Errorf("permission_mode: got %q, want unrestricted", result["permission_mode"])
	}
	// model: schema="", provider=sonnet, agent=opus -> opus (agent wins)
	if result["model"] != "opus" {
		t.Errorf("model: got %q, want opus", result["model"])
	}
	// effort: schema="", agent=high -> high
	if result["effort"] != "high" {
		t.Errorf("effort: got %q, want high", result["effort"])
	}
}

func TestComputeEffectiveDefaults_SchemaOnly(t *testing.T) {
	schema := []ProviderOption{
		{Key: "permission_mode", Default: "auto-edit"},
		{Key: "model", Default: ""},
	}

	result := ComputeEffectiveDefaults(schema, nil, nil)

	if result["permission_mode"] != "auto-edit" {
		t.Errorf("permission_mode: got %q, want auto-edit", result["permission_mode"])
	}
	if result["model"] != "" {
		t.Errorf("model: got %q, want empty", result["model"])
	}
}

func TestComputeEffectiveDefaults_ProviderOverridesSchema(t *testing.T) {
	schema := []ProviderOption{
		{Key: "permission_mode", Default: "auto-edit"},
	}
	providerDefaults := map[string]string{
		"permission_mode": "unrestricted",
	}

	result := ComputeEffectiveDefaults(schema, providerDefaults, nil)

	if result["permission_mode"] != "unrestricted" {
		t.Errorf("permission_mode: got %q, want unrestricted", result["permission_mode"])
	}
}

func TestResolveDefaultArgs_ClaudeSchema(t *testing.T) {
	builtins := BuiltinProviders()
	claude := builtins["claude"]

	rp := &ResolvedProvider{
		OptionsSchema:     claude.OptionsSchema,
		EffectiveDefaults: ComputeEffectiveDefaults(claude.OptionsSchema, claude.OptionDefaults, nil),
	}

	args := rp.ResolveDefaultArgs()

	// Claude effective defaults: permission_mode=unrestricted, effort=max (from OptionDefaults).
	// Should produce --dangerously-skip-permissions --effort max.
	wantArgs := []string{"--dangerously-skip-permissions", "--effort", "max"}
	if len(args) != len(wantArgs) {
		t.Fatalf("got args=%v, want %v", args, wantArgs)
	}
	for i, w := range wantArgs {
		if args[i] != w {
			t.Errorf("args[%d]=%q, want %q", i, args[i], w)
		}
	}
}

func TestResolveDefaultArgs_EmptyDefaults(t *testing.T) {
	rp := &ResolvedProvider{
		OptionsSchema: []ProviderOption{
			{Key: "model", Default: "", Choices: []OptionChoice{
				{Value: "", FlagArgs: nil},
				{Value: "opus", FlagArgs: []string{"--model", "opus"}},
			}},
		},
		EffectiveDefaults: map[string]string{},
	}

	args := rp.ResolveDefaultArgs()
	if len(args) != 0 {
		t.Errorf("empty defaults should produce no args, got %v", args)
	}
}

func TestStripArgsSlice_BasicStripping(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "permission_mode",
			Choices: []OptionChoice{
				{Value: "unrestricted", FlagArgs: []string{"--dangerously-skip-permissions"}},
				{Value: "auto-edit", FlagArgs: []string{"--permission-mode", "auto-edit"}},
			},
		},
	}
	flags := CollectAllSchemaFlags(schema)

	args := []string{"--dangerously-skip-permissions", "--other-flag"}
	inferDefaults := make(map[string]string)
	result := stripArgsSlice(args, flags, schema, inferDefaults)

	if len(result) != 1 || result[0] != "--other-flag" {
		t.Errorf("got %v, want [--other-flag]", result)
	}
	// Should infer unrestricted from the stripped flag.
	if inferDefaults["permission_mode"] != "unrestricted" {
		t.Errorf("inferred permission_mode: got %q, want unrestricted", inferDefaults["permission_mode"])
	}
}

func TestStripArgsSlice_MultiTokenFlag(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "permission_mode",
			Choices: []OptionChoice{
				{Value: "auto-edit", FlagArgs: []string{"--permission-mode", "auto-edit"}},
			},
		},
	}
	flags := CollectAllSchemaFlags(schema)

	args := []string{"--permission-mode", "auto-edit", "--other"}
	inferDefaults := make(map[string]string)
	result := stripArgsSlice(args, flags, schema, inferDefaults)

	if len(result) != 1 || result[0] != "--other" {
		t.Errorf("got %v, want [--other]", result)
	}
	if inferDefaults["permission_mode"] != "auto-edit" {
		t.Errorf("inferred permission_mode: got %q, want auto-edit", inferDefaults["permission_mode"])
	}
}

func TestStripArgsSlice_ExistingDefaultNotOverridden(t *testing.T) {
	schema := []ProviderOption{
		{
			Key: "permission_mode",
			Choices: []OptionChoice{
				{Value: "unrestricted", FlagArgs: []string{"--dangerously-skip-permissions"}},
			},
		},
	}
	flags := CollectAllSchemaFlags(schema)

	args := []string{"--dangerously-skip-permissions"}
	// Pre-populate with an existing default -- should not be overridden.
	inferDefaults := map[string]string{"permission_mode": "plan"}
	result := stripArgsSlice(args, flags, schema, inferDefaults)

	if len(result) != 0 {
		t.Errorf("got %v, want []", result)
	}
	if inferDefaults["permission_mode"] != "plan" {
		t.Errorf("existing default should be preserved, got %q", inferDefaults["permission_mode"])
	}
}

func TestStripArgsSlice_PartialOverlap_CodexSuggest(t *testing.T) {
	// Codex's "suggest" choice has a multi-flag FlagArgs:
	//   ["--ask-for-approval", "untrusted", "--sandbox", "read-only"]
	// After splitFlagArgs, this becomes two groups:
	//   ["--ask-for-approval", "untrusted"] and ["--sandbox", "read-only"]
	// If a user has only --sandbox read-only in args, it should be stripped.
	schema := []ProviderOption{
		{
			Key: "permission_mode",
			Choices: []OptionChoice{
				{Value: "suggest", FlagArgs: []string{"--ask-for-approval", "untrusted", "--sandbox", "read-only"}},
				{Value: "unrestricted", FlagArgs: []string{"--dangerously-bypass-approvals-and-sandbox"}},
			},
		},
	}
	flags := CollectAllSchemaFlags(schema)

	args := []string{"--sandbox", "read-only", "--other"}
	result := stripArgsSlice(args, flags, schema, nil)

	if len(result) != 1 || result[0] != "--other" {
		t.Errorf("partial multi-flag should be stripped, got %v, want [--other]", result)
	}
}

func TestSplitFlagArgs_MultiFlag(t *testing.T) {
	args := []string{"--ask-for-approval", "untrusted", "--sandbox", "read-only"}
	groups := splitFlagArgs(args)

	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2: %v", len(groups), groups)
	}
	if len(groups[0]) != 2 || groups[0][0] != "--ask-for-approval" || groups[0][1] != "untrusted" {
		t.Errorf("group 0: got %v, want [--ask-for-approval untrusted]", groups[0])
	}
	if len(groups[1]) != 2 || groups[1][0] != "--sandbox" || groups[1][1] != "read-only" {
		t.Errorf("group 1: got %v, want [--sandbox read-only]", groups[1])
	}
}

func TestSplitFlagArgs_SingleFlag(t *testing.T) {
	args := []string{"--full-auto"}
	groups := splitFlagArgs(args)

	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %v", len(groups), groups)
	}
	if len(groups[0]) != 1 || groups[0][0] != "--full-auto" {
		t.Errorf("group 0: got %v, want [--full-auto]", groups[0])
	}
}

func TestSplitFlagArgs_Empty(t *testing.T) {
	groups := splitFlagArgs(nil)
	if groups != nil {
		t.Errorf("nil input should return nil, got %v", groups)
	}
}

func TestBuiltinProviders_ClaudeHasNilArgsAndOptionDefaults(t *testing.T) {
	builtins := BuiltinProviders()
	claude := builtins["claude"]

	if claude.Args != nil {
		t.Errorf("claude Args should be nil, got %v", claude.Args)
	}
	if claude.OptionDefaults == nil {
		t.Fatal("claude OptionDefaults should not be nil")
	}
	if claude.OptionDefaults["permission_mode"] != "unrestricted" {
		t.Errorf("claude OptionDefaults[permission_mode] = %q, want unrestricted",
			claude.OptionDefaults["permission_mode"])
	}
}

func TestBuiltinProviders_CodexHasNilArgsAndOptionDefaults(t *testing.T) {
	builtins := BuiltinProviders()
	codex := builtins["codex"]

	if codex.Args != nil {
		t.Errorf("codex Args should be nil, got %v", codex.Args)
	}
	if codex.OptionDefaults == nil {
		t.Fatal("codex OptionDefaults should not be nil")
	}
	if codex.OptionDefaults["permission_mode"] != "unrestricted" {
		t.Errorf("codex OptionDefaults[permission_mode] = %q, want unrestricted",
			codex.OptionDefaults["permission_mode"])
	}
	if codex.OptionDefaults["model"] != "gpt-5.5" {
		t.Errorf("codex OptionDefaults[model] = %q, want gpt-5.5",
			codex.OptionDefaults["model"])
	}
	if !schemaHasChoice(codex.OptionsSchema, "model", "gpt-5.5") {
		t.Error("codex OptionsSchema missing model choice gpt-5.5")
	}
}

func TestBuiltinProviders_GeminiHasNilArgsAndOptionDefaults(t *testing.T) {
	builtins := BuiltinProviders()
	gemini := builtins["gemini"]

	if gemini.Args != nil {
		t.Errorf("gemini Args should be nil, got %v", gemini.Args)
	}
	if gemini.OptionDefaults == nil {
		t.Fatal("gemini OptionDefaults should not be nil")
	}
	if gemini.OptionDefaults["permission_mode"] != "unrestricted" {
		t.Errorf("gemini OptionDefaults[permission_mode] = %q, want unrestricted",
			gemini.OptionDefaults["permission_mode"])
	}
}

func TestValidateOptionDefaults_Valid(t *testing.T) {
	schema := []ProviderOption{
		{Key: "permission_mode", Choices: []OptionChoice{
			{Value: "auto-edit"}, {Value: "unrestricted"},
		}},
	}
	err := ValidateOptionDefaults(schema, map[string]string{"permission_mode": "unrestricted"})
	if err != nil {
		t.Fatalf("valid defaults should pass: %v", err)
	}
}

func TestValidateOptionDefaults_UnknownKey(t *testing.T) {
	schema := []ProviderOption{
		{Key: "permission_mode", Choices: []OptionChoice{{Value: "auto-edit"}}},
	}
	err := ValidateOptionDefaults(schema, map[string]string{"bogus": "val"})
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "not in the options schema") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateOptionDefaults_InvalidValue(t *testing.T) {
	schema := []ProviderOption{
		{Key: "permission_mode", Choices: []OptionChoice{{Value: "auto-edit"}}},
	}
	err := ValidateOptionDefaults(schema, map[string]string{"permission_mode": "typo"})
	if err == nil {
		t.Fatal("expected error for invalid value")
	}
	if !strings.Contains(err.Error(), "not a valid choice") {
		t.Errorf("unexpected error: %v", err)
	}
}

func schemaHasChoice(schema []ProviderOption, key, value string) bool {
	for _, opt := range schema {
		if opt.Key != key {
			continue
		}
		for _, choice := range opt.Choices {
			if choice.Value == value {
				return true
			}
		}
	}
	return false
}
