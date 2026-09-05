package builtin

import (
	"testing"
)

func TestBuiltinProvidersAndOrder(t *testing.T) {
	providers := BuiltinProviders()
	order := BuiltinProviderOrder()

	if len(providers) != 18 {
		t.Fatalf("len(BuiltinProviders()) = %d, want 18", len(providers))
	}
	if len(order) != 18 {
		t.Fatalf("len(BuiltinProviderOrder()) = %d, want 18", len(order))
	}

	for _, name := range order {
		spec, ok := providers[name]
		if !ok {
			t.Fatalf("BuiltinProviders() missing %q", name)
		}
		if spec.Command == "" {
			t.Fatalf("provider %q has empty Command", name)
		}
		if spec.DisplayName == "" {
			t.Fatalf("provider %q has empty DisplayName", name)
		}
	}
}

func TestBuiltinProviderMimoCodeSpec(t *testing.T) {
	providers := BuiltinProviders()
	spec, ok := providers["mimocode"]
	if !ok {
		t.Fatal("BuiltinProviders() missing mimocode")
	}
	if spec.Command != "mimo" {
		t.Errorf("mimocode Command = %q, want %q", spec.Command, "mimo")
	}
	if spec.DisplayName != "MiMo Code" {
		t.Errorf("mimocode DisplayName = %q, want %q", spec.DisplayName, "MiMo Code")
	}
	if len(spec.Args) != 1 || spec.Args[0] != "--never-ask" {
		t.Errorf("mimocode Args = %v, want [--never-ask]", spec.Args)
	}
	if spec.PromptMode != "flag" || spec.PromptFlag != "--prompt" {
		t.Errorf("mimocode prompt = (%q, %q), want (flag, --prompt)", spec.PromptMode, spec.PromptFlag)
	}
	if !spec.SupportsACP || !spec.SupportsHooks {
		t.Errorf("mimocode SupportsACP=%v SupportsHooks=%v, want both true", spec.SupportsACP, spec.SupportsHooks)
	}
	if spec.ResumeFlag != "--session" || spec.ResumeStyle != "flag" {
		t.Errorf("mimocode resume = (%q, %q), want (--session, flag)", spec.ResumeFlag, spec.ResumeStyle)
	}
	if len(spec.ACPArgs) != 1 || spec.ACPArgs[0] != "acp" {
		t.Errorf("mimocode ACPArgs = %v, want [acp]", spec.ACPArgs)
	}
	if spec.InstructionsFile != "AGENTS.md" {
		t.Errorf("mimocode InstructionsFile = %q, want AGENTS.md", spec.InstructionsFile)
	}

	order := BuiltinProviderOrder()
	opencodeIdx, mimocodeIdx := -1, -1
	for i, name := range order {
		switch name {
		case "opencode":
			opencodeIdx = i
		case "mimocode":
			mimocodeIdx = i
		}
	}
	if mimocodeIdx == -1 {
		t.Fatal("BuiltinProviderOrder() missing mimocode")
	}
	if mimocodeIdx != opencodeIdx+1 {
		t.Errorf("mimocode order index = %d, want immediately after opencode (%d)", mimocodeIdx, opencodeIdx)
	}
}

func TestBuiltinProviderZCodeSpec(t *testing.T) {
	spec, ok := BuiltinProviders()["zcode"]
	if !ok {
		t.Fatal("BuiltinProviders() missing zcode")
	}
	if spec.Command != "zcode-repl" {
		t.Errorf("zcode Command = %q, want zcode-repl", spec.Command)
	}
	if spec.DisplayName != "ZCode (Z.ai GLM harness)" {
		t.Errorf("zcode DisplayName = %q", spec.DisplayName)
	}
	if spec.PromptMode != "none" {
		t.Errorf("zcode PromptMode = %q, want none (the adapter is a REPL)", spec.PromptMode)
	}
	if spec.PromptFlag != "" {
		t.Errorf("zcode PromptFlag = %q, want empty under PromptMode none", spec.PromptFlag)
	}
	if spec.ReadyPromptPrefix != "zcode-repl ready" {
		t.Errorf("zcode ReadyPromptPrefix = %q, want the adapter marker", spec.ReadyPromptPrefix)
	}
	if spec.ReadyDelayMs <= 0 {
		t.Errorf("zcode ReadyDelayMs = %d, want a positive probe budget", spec.ReadyDelayMs)
	}
	if len(spec.ProcessNames) != 2 || spec.ProcessNames[0] != "bash" || spec.ProcessNames[1] != "node" {
		t.Errorf("zcode ProcessNames = %v, want [bash node]", spec.ProcessNames)
	}
	// Restart continuity is adapter-internal (persisted provider session id
	// replayed with --resume), so gc must not compose any resume flag.
	if spec.ResumeFlag != "" || spec.ResumeStyle != "" || spec.ResumeCommand != "" || spec.SessionIDFlag != "" {
		t.Errorf("zcode declares resume wiring (%q,%q,%q,%q); continuity is adapter-internal",
			spec.ResumeFlag, spec.ResumeStyle, spec.ResumeCommand, spec.SessionIDFlag)
	}
	if spec.AcceptStartupDialogs == nil || *spec.AcceptStartupDialogs {
		t.Errorf("zcode AcceptStartupDialogs = %v, want an explicit false", spec.AcceptStartupDialogs)
	}
	if spec.SupportsHooks {
		t.Error("zcode SupportsHooks = true, want false")
	}
	if spec.SupportsACP {
		t.Error("zcode SupportsACP = true, want false")
	}
	// Evidence: the bundle's NodeContextSourceAdapter context-source list is
	// ["AGENTS.md"], and /init writes AGENTS.md.
	if spec.InstructionsFile != "AGENTS.md" {
		t.Errorf("zcode InstructionsFile = %q, want AGENTS.md", spec.InstructionsFile)
	}
	if spec.UpstreamBaseURLEnv != "ZCODE_BASE_URL" || spec.UpstreamAPIKeyEnv != "ZCODE_API_KEY" {
		t.Errorf("zcode upstream binding = (%q, %q), want (ZCODE_BASE_URL, ZCODE_API_KEY)",
			spec.UpstreamBaseURLEnv, spec.UpstreamAPIKeyEnv)
	}

	identity, ok := CanonicalProfileIdentity("zcode/tmux-cli")
	if !ok {
		t.Fatal("CanonicalProfileIdentity(zcode/tmux-cli) not registered")
	}
	if identity.ProviderFamily != "zcode" {
		t.Errorf("zcode/tmux-cli family = %q, want zcode", identity.ProviderFamily)
	}
}

func TestBuiltinProvidersReturnClonedData(t *testing.T) {
	a := BuiltinProviders()
	b := BuiltinProviders()

	a["claude"] = BuiltinProviderSpec{Command: "mutated"}
	if b["claude"].Command == "mutated" {
		t.Fatal("BuiltinProviders() should return a cloned map")
	}

	claude := a["codex"]
	claude.ProcessNames[0] = "mutated"
	a["codex"] = claude
	if b["codex"].ProcessNames[0] == "mutated" {
		t.Fatal("BuiltinProviders() should clone nested slices")
	}
}

func TestBuiltinCodexModelChoicesUseAvailable53CodexAlias(t *testing.T) {
	unavailable53CodexAlias := "gpt-5.3-" + "codex-spark"
	codex, ok := BuiltinProviders()["codex"]
	if !ok {
		t.Fatal("BuiltinProviders() missing codex")
	}

	var modelOption BuiltinProviderOption
	for _, option := range codex.OptionsSchema {
		if option.Key == "model" {
			modelOption = option
			break
		}
	}
	if modelOption.Key == "" {
		t.Fatal("codex provider missing model option")
	}

	var found53 bool
	for _, choice := range modelOption.Choices {
		if choice.Value == unavailable53CodexAlias {
			t.Fatalf("codex model choices include unavailable alias %q", choice.Value)
		}
		if choice.Value == "gpt-5.3-codex" {
			found53 = true
			if got, want := choice.Label, "GPT-5.3 Codex"; got != want {
				t.Fatalf("gpt-5.3-codex label = %q, want %q", got, want)
			}
		}
	}
	if !found53 {
		t.Fatal("codex model choices missing gpt-5.3-codex")
	}
}

func TestBuiltinAntigravityPermissionModeChoices(t *testing.T) {
	agy, ok := BuiltinProviders()["antigravity"]
	if !ok {
		t.Fatal("BuiltinProviders() missing antigravity")
	}

	var permissionOption BuiltinProviderOption
	for _, option := range agy.OptionsSchema {
		if option.Key == "permission_mode" {
			permissionOption = option
			break
		}
	}
	if permissionOption.Key == "" {
		t.Fatal("antigravity provider missing permission_mode option")
	}
	if permissionOption.Default != "unrestricted" {
		t.Errorf("permission_mode Default = %q, want %q", permissionOption.Default, "unrestricted")
	}
	if agy.OptionDefaults["permission_mode"] != "unrestricted" {
		t.Errorf("OptionDefaults[permission_mode] = %q, want %q", agy.OptionDefaults["permission_mode"], "unrestricted")
	}

	byValue := make(map[string]BuiltinOptionChoice, len(permissionOption.Choices))
	for _, choice := range permissionOption.Choices {
		byValue[choice.Value] = choice
	}
	wantFlagArgs := map[string][]string{
		"unrestricted": {"--dangerously-skip-permissions"},
		"standard":     {},
		"accept-edits": {"--mode", "accept-edits"},
		"plan":         {"--mode", "plan"},
	}
	if len(permissionOption.Choices) != len(wantFlagArgs) {
		t.Errorf("permission_mode choice count = %d, want %d", len(permissionOption.Choices), len(wantFlagArgs))
	}
	for value, want := range wantFlagArgs {
		choice, ok := byValue[value]
		if !ok {
			t.Fatalf("permission_mode choices missing %q", value)
		}
		if len(choice.FlagArgs) != len(want) {
			t.Fatalf("%s FlagArgs = %v, want %v", value, choice.FlagArgs, want)
		}
		for i := range want {
			if choice.FlagArgs[i] != want[i] {
				t.Fatalf("%s FlagArgs = %v, want %v", value, choice.FlagArgs, want)
			}
		}
	}

	// PermissionModes is the config-only display table; it mirrors the
	// flag-bearing schema choices (the no-flag "standard" mode stays unmapped).
	wantModes := map[string]string{
		"unrestricted": "--dangerously-skip-permissions",
		"accept-edits": "--mode accept-edits",
		"plan":         "--mode plan",
	}
	if len(agy.PermissionModes) != len(wantModes) {
		t.Errorf("PermissionModes = %v, want %v", agy.PermissionModes, wantModes)
	}
	for mode, want := range wantModes {
		if agy.PermissionModes[mode] != want {
			t.Errorf("PermissionModes[%s] = %q, want %q", mode, agy.PermissionModes[mode], want)
		}
	}
}

func TestBuiltinAntigravityEffortChoices(t *testing.T) {
	agy, ok := BuiltinProviders()["antigravity"]
	if !ok {
		t.Fatal("BuiltinProviders() missing antigravity")
	}

	var effortOption BuiltinProviderOption
	for _, option := range agy.OptionsSchema {
		if option.Key == "effort" {
			effortOption = option
			break
		}
	}
	if effortOption.Key == "" {
		t.Fatal("antigravity provider missing effort option")
	}

	// agy < 1.1.10 silently ignores --effort on the --prompt-interactive
	// launch path, so the option must never fire by default: no schema
	// Default and no OptionDefaults entry.
	if effortOption.Default != "" {
		t.Errorf("effort Default = %q, want empty (agy <1.1.10 drops the flag silently)", effortOption.Default)
	}
	if _, ok := agy.OptionDefaults["effort"]; ok {
		t.Errorf("OptionDefaults[effort] = %q, want absent (agy <1.1.10 drops the flag silently)", agy.OptionDefaults["effort"])
	}

	if len(effortOption.Choices) == 0 || effortOption.Choices[0].Value != "" || len(effortOption.Choices[0].FlagArgs) != 0 {
		t.Fatalf("effort choices must lead with the empty no-flag sentinel, got %v", effortOption.Choices)
	}

	byValue := make(map[string]BuiltinOptionChoice, len(effortOption.Choices))
	for _, choice := range effortOption.Choices {
		byValue[choice.Value] = choice
	}
	// agy exposes exactly three effort levels (agy --help: --effort low|medium|high).
	wantValues := []string{"low", "medium", "high"}
	if len(effortOption.Choices) != len(wantValues)+1 {
		t.Errorf("effort choice count = %d, want %d (sentinel + %v)", len(effortOption.Choices), len(wantValues)+1, wantValues)
	}
	for _, value := range wantValues {
		choice, ok := byValue[value]
		if !ok {
			t.Fatalf("effort choices missing %q", value)
		}
		if len(choice.FlagArgs) != 2 || choice.FlagArgs[0] != "--effort" || choice.FlagArgs[1] != value {
			t.Errorf("%s FlagArgs = %v, want [--effort %s]", value, choice.FlagArgs, value)
		}
	}
}

func TestBuiltinAntigravityModelChoices(t *testing.T) {
	agy, ok := BuiltinProviders()["antigravity"]
	if !ok {
		t.Fatal("BuiltinProviders() missing antigravity")
	}

	var modelOption BuiltinProviderOption
	for _, option := range agy.OptionsSchema {
		if option.Key == "model" {
			modelOption = option
			break
		}
	}
	if modelOption.Key == "" {
		t.Fatal("antigravity provider missing model option")
	}

	// agy < 1.1.10 silently ignores --model on the --prompt-interactive
	// launch path, so the option must never fire by default.
	if modelOption.Default != "" {
		t.Errorf("model Default = %q, want empty (agy <1.1.10 drops the flag silently)", modelOption.Default)
	}
	if _, ok := agy.OptionDefaults["model"]; ok {
		t.Errorf("OptionDefaults[model] = %q, want absent (agy <1.1.10 drops the flag silently)", agy.OptionDefaults["model"])
	}

	if len(modelOption.Choices) == 0 || modelOption.Choices[0].Value != "" || len(modelOption.Choices[0].FlagArgs) != 0 {
		t.Fatalf("model choices must lead with the empty no-flag sentinel, got %v", modelOption.Choices)
	}

	byValue := make(map[string]BuiltinOptionChoice, len(modelOption.Choices))
	for _, choice := range modelOption.Choices {
		byValue[choice.Value] = choice
	}

	// Stable slugs + display names as enumerated by `agy models` (agy 1.1.5+).
	wantLabels := map[string]string{
		"gemini-3.6-flash-high":    "Gemini 3.6 Flash (High)",
		"gemini-3.6-flash-medium":  "Gemini 3.6 Flash (Medium)",
		"gemini-3.6-flash-low":     "Gemini 3.6 Flash (Low)",
		"gemini-3.5-flash-high":    "Gemini 3.5 Flash (High)",
		"gemini-3.5-flash-medium":  "Gemini 3.5 Flash (Medium)",
		"gemini-3.5-flash-low":     "Gemini 3.5 Flash (Low)",
		"gemini-3.1-pro-high":      "Gemini 3.1 Pro (High)",
		"gemini-3.1-pro-low":       "Gemini 3.1 Pro (Low)",
		"claude-sonnet-4-6":        "Claude Sonnet 4.6 (Thinking)",
		"claude-opus-4-6-thinking": "Claude Opus 4.6 (Thinking)",
		"gpt-oss-120b-medium":      "GPT-OSS 120B (Medium)",
	}
	if len(modelOption.Choices) != len(wantLabels)+1 {
		t.Errorf("model choice count = %d, want %d (sentinel + %d slugs)", len(modelOption.Choices), len(wantLabels)+1, len(wantLabels))
	}
	for slug, wantLabel := range wantLabels {
		choice, ok := byValue[slug]
		if !ok {
			t.Fatalf("model choices missing %q", slug)
		}
		if choice.Label != wantLabel {
			t.Errorf("%s label = %q, want %q", slug, choice.Label, wantLabel)
		}
		if len(choice.FlagArgs) != 2 || choice.FlagArgs[0] != "--model" || choice.FlagArgs[1] != slug {
			t.Errorf("%s FlagArgs = %v, want [--model %s]", slug, choice.FlagArgs, slug)
		}
		// agy has no -m short alias (unlike claude/codex) — keep aliases empty.
		if len(choice.FlagAliases) != 0 {
			t.Errorf("%s FlagAliases = %v, want none (agy --help defines no -m)", slug, choice.FlagAliases)
		}
	}
}

func TestBuiltinCodexModelChoicesIncludeGPT56Variants(t *testing.T) {
	codex, ok := BuiltinProviders()["codex"]
	if !ok {
		t.Fatal("BuiltinProviders() missing codex")
	}

	var modelOption BuiltinProviderOption
	for _, option := range codex.OptionsSchema {
		if option.Key == "model" {
			modelOption = option
			break
		}
	}
	if modelOption.Key == "" {
		t.Fatal("codex provider missing model option")
	}

	byValue := make(map[string]BuiltinOptionChoice, len(modelOption.Choices))
	for _, choice := range modelOption.Choices {
		byValue[choice.Value] = choice
	}

	wantLabels := map[string]string{
		"gpt-5.6-sol":   "GPT-5.6 Sol",
		"gpt-5.6-terra": "GPT-5.6 Terra",
		"gpt-5.6-luna":  "GPT-5.6 Luna",
	}
	for value, wantLabel := range wantLabels {
		choice, ok := byValue[value]
		if !ok {
			t.Fatalf("codex model choices missing %q", value)
		}
		if choice.Label != wantLabel {
			t.Errorf("%s label = %q, want %q", value, choice.Label, wantLabel)
		}
		wantFlagArgs := []string{"--model", value}
		if len(choice.FlagArgs) != 2 || choice.FlagArgs[0] != wantFlagArgs[0] || choice.FlagArgs[1] != wantFlagArgs[1] {
			t.Errorf("%s FlagArgs = %v, want %v", value, choice.FlagArgs, wantFlagArgs)
		}
		if len(choice.FlagAliases) != 1 || len(choice.FlagAliases[0]) != 2 ||
			choice.FlagAliases[0][0] != "-m" || choice.FlagAliases[0][1] != value {
			t.Errorf("%s FlagAliases = %v, want [[-m %s]]", value, choice.FlagAliases, value)
		}
	}
}

// GH#4602: Claude Code >= 2.1.170 rejects --permission-mode auto-edit / full-auto.
// Config-facing values stay "auto-edit"/"full-auto"; CLI args must be modern modes.
func TestClaudePermissionModeMapsToAcceptedCLIValues(t *testing.T) {
	providers := BuiltinProviders()
	claude, ok := providers["claude"]
	if !ok {
		t.Fatal("BuiltinProviders() missing claude")
	}

	if got := claude.PermissionModes["auto-edit"]; got != "--permission-mode acceptEdits" {
		t.Errorf("PermissionModes[auto-edit] = %q, want --permission-mode acceptEdits", got)
	}
	if got := claude.PermissionModes["full-auto"]; got != "--permission-mode dontAsk" {
		t.Errorf("PermissionModes[full-auto] = %q, want --permission-mode dontAsk", got)
	}

	var permOpt BuiltinProviderOption
	for _, option := range claude.OptionsSchema {
		if option.Key == "permission_mode" {
			permOpt = option
			break
		}
	}
	if permOpt.Key == "" {
		t.Fatal("claude provider missing permission_mode option")
	}
	byValue := make(map[string]BuiltinOptionChoice, len(permOpt.Choices))
	for _, c := range permOpt.Choices {
		byValue[c.Value] = c
	}
	autoEdit, ok := byValue["auto-edit"]
	if !ok {
		t.Fatal("claude permission_mode choices missing auto-edit")
	}
	if len(autoEdit.FlagArgs) != 2 || autoEdit.FlagArgs[0] != "--permission-mode" || autoEdit.FlagArgs[1] != "acceptEdits" {
		t.Errorf("auto-edit FlagArgs = %v, want [--permission-mode acceptEdits]", autoEdit.FlagArgs)
	}
	fullAuto, ok := byValue["full-auto"]
	if !ok {
		t.Fatal("claude permission_mode choices missing full-auto")
	}
	if len(fullAuto.FlagArgs) != 2 || fullAuto.FlagArgs[0] != "--permission-mode" || fullAuto.FlagArgs[1] != "dontAsk" {
		t.Errorf("full-auto FlagArgs = %v, want [--permission-mode dontAsk]", fullAuto.FlagArgs)
	}
}

// #5415: codex CLI >= 0.128 deprecated --full-auto, and >= 0.147.0 removes it
// entirely (codex exec --full-auto now errors instead of running). auto-edit
// must use the modern replacement: --sandbox workspace-write plus
// --ask-for-approval never. (--full-auto was an alias for
// `-a on-failure --sandbox workspace-write`; gc pins `never` rather than
// `on-failure` because an escalation prompt in an unattended slung pane
// blocks forever.)
func TestCodexAutoEditMapsToUnremovedCLIFlags(t *testing.T) {
	providers := BuiltinProviders()
	codex, ok := providers["codex"]
	if !ok {
		t.Fatal("BuiltinProviders() missing codex")
	}

	const want = "--sandbox workspace-write --ask-for-approval never"
	if got := codex.PermissionModes["auto-edit"]; got != want {
		t.Errorf("PermissionModes[auto-edit] = %q, want %q", got, want)
	}

	var permOpt BuiltinProviderOption
	for _, option := range codex.OptionsSchema {
		if option.Key == "permission_mode" {
			permOpt = option
			break
		}
	}
	if permOpt.Key == "" {
		t.Fatal("codex provider missing permission_mode option")
	}
	byValue := make(map[string]BuiltinOptionChoice, len(permOpt.Choices))
	for _, c := range permOpt.Choices {
		byValue[c.Value] = c
	}
	autoEdit, ok := byValue["auto-edit"]
	if !ok {
		t.Fatal("codex permission_mode choices missing auto-edit")
	}
	wantArgs := []string{"--sandbox", "workspace-write", "--ask-for-approval", "never"}
	if len(autoEdit.FlagArgs) != len(wantArgs) {
		t.Fatalf("auto-edit FlagArgs = %v, want %v", autoEdit.FlagArgs, wantArgs)
	}
	for i, arg := range wantArgs {
		if autoEdit.FlagArgs[i] != arg {
			t.Errorf("auto-edit FlagArgs = %v, want %v", autoEdit.FlagArgs, wantArgs)
			break
		}
	}
}
