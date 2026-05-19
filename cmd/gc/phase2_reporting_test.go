package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	workertest "github.com/gastownhall/gascity/internal/worker/workertest"
)

func newPhase2Reporter(t *testing.T, suite string) *workertest.SuiteReporter {
	t.Helper()

	return workertest.NewSuiteReporter(t, suite, map[string]string{
		"tier":      "worker-core",
		"phase":     "phase2",
		"component": "cmd-gc",
	})
}

func startupCommandMaterializationResult(tc phase2ProviderCase, tp TemplateParams) workertest.Result {
	evidence := phase2TemplateEvidence(tc, tp)
	wantPromptMode := tc.wantPromptMode
	if wantPromptMode == "" {
		wantPromptMode = "arg"
	}
	switch {
	case tp.ResolvedProvider == nil:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization, "ResolvedProvider = nil").WithEvidence(evidence)
	case tp.ResolvedProvider.Name != tc.family:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			fmt.Sprintf("ResolvedProvider.Name = %q, want %q", tp.ResolvedProvider.Name, tc.family)).WithEvidence(evidence)
	case tp.ResolvedProvider.PromptMode != wantPromptMode:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			fmt.Sprintf("PromptMode = %q, want %s", tp.ResolvedProvider.PromptMode, wantPromptMode)).WithEvidence(evidence)
	case tc.wantCommand != "" && tp.Command != tc.wantCommand:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			fmt.Sprintf("Command = %q, want %q", tp.Command, tc.wantCommand)).WithEvidence(evidence)
	case tc.wantCommandPrefix != "" && !strings.HasPrefix(tp.Command, tc.wantCommandPrefix):
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			fmt.Sprintf("Command = %q, want prefix %q", tp.Command, tc.wantCommandPrefix)).WithEvidence(evidence)
	case !containsOrderedArgs(tp.Command, tp.ResolvedProvider.ResolveDefaultArgs()):
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			fmt.Sprintf("Command = %q, want default args %v", tp.Command, tp.ResolvedProvider.ResolveDefaultArgs())).WithEvidence(evidence)
	case tc.wantSettingsArg:
		settingsPath, ok := commandFlagValue(tp.Command, "--settings")
		if !ok {
			return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
				fmt.Sprintf("Command = %q, want --settings arg", tp.Command)).WithEvidence(evidence)
		}
		if !strings.HasSuffix(filepath.Clean(settingsPath), filepath.Join(".gc", "settings.json")) {
			return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
				fmt.Sprintf("settings path = %q, want suffix %q", settingsPath, filepath.Join(".gc", "settings.json"))).WithEvidence(evidence)
		}
		return workertest.Pass(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			"provider defaults and launch semantics materialized into the startup command").WithEvidence(evidence)
	default:
		return workertest.Pass(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			"provider defaults and launch semantics materialized into the startup command").WithEvidence(evidence)
	}
}

func startupRuntimeConfigMaterializationResult(tc phase2ProviderCase, tp TemplateParams, cfg runtime.Config) workertest.Result {
	evidence := phase2ConfigEvidence(tc, tp, cfg)
	switch {
	case cfg.Command != tp.Command:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.Command = %q, want %q", cfg.Command, tp.Command)).WithEvidence(evidence)
	case cfg.WorkDir != tp.WorkDir:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.WorkDir = %q, want %q", cfg.WorkDir, tp.WorkDir)).WithEvidence(evidence)
	case cfg.PromptSuffix == "":
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			"cfg.PromptSuffix = empty, want beacon prompt materialized").WithEvidence(evidence)
	case tc.wantPromptFlag != "" && cfg.PromptFlag != tc.wantPromptFlag:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.PromptFlag = %q, want %q", cfg.PromptFlag, tc.wantPromptFlag)).WithEvidence(evidence)
	case tc.wantPromptFlag == "" && cfg.PromptFlag != "":
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.PromptFlag = %q, want empty for arg-mode provider", cfg.PromptFlag)).WithEvidence(evidence)
	case cfg.Env["GC_DIR"] != tp.WorkDir:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("GC_DIR = %q, want %q", cfg.Env["GC_DIR"], tp.WorkDir)).WithEvidence(evidence)
	case cfg.Env["GC_TEMPLATE"] != tp.TemplateName:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("GC_TEMPLATE = %q, want %q", cfg.Env["GC_TEMPLATE"], tp.TemplateName)).WithEvidence(evidence)
	case cfg.Env["GC_SESSION_NAME"] != tp.SessionName:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("GC_SESSION_NAME = %q, want %q", cfg.Env["GC_SESSION_NAME"], tp.SessionName)).WithEvidence(evidence)
	case tp.Prompt != "" && cfg.Env[startupPromptDeliveredEnv] != "1":
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("%s = %q, want 1", startupPromptDeliveredEnv, cfg.Env[startupPromptDeliveredEnv])).WithEvidence(evidence)
	case cfg.Env["WORKER_CORE_MARKER"] != tc.family:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("WORKER_CORE_MARKER = %q, want %q", cfg.Env["WORKER_CORE_MARKER"], tc.family)).WithEvidence(evidence)
	case cfg.ReadyDelayMs != tc.wantReadyDelayMs:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.ReadyDelayMs = %d, want %d", cfg.ReadyDelayMs, tc.wantReadyDelayMs)).WithEvidence(evidence)
	case cfg.ReadyPromptPrefix != tc.wantReadyPromptPrefix:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.ReadyPromptPrefix = %q, want %q", cfg.ReadyPromptPrefix, tc.wantReadyPromptPrefix)).WithEvidence(evidence)
	case !reflect.DeepEqual(cfg.ProcessNames, tc.wantProcessNames):
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.ProcessNames = %v, want %v", cfg.ProcessNames, tc.wantProcessNames)).WithEvidence(evidence)
	case cfg.EmitsPermissionWarning != tc.wantEmitsPermission:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.EmitsPermissionWarning = %v, want %v", cfg.EmitsPermissionWarning, tc.wantEmitsPermission)).WithEvidence(evidence)
	case cfg.Nudge != "nudge-"+tc.family:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.Nudge = %q, want %q", cfg.Nudge, "nudge-"+tc.family)).WithEvidence(evidence)
	case !reflect.DeepEqual(cfg.PreStart, []string{"echo pre-" + tc.family}):
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.PreStart = %v, want %v", cfg.PreStart, []string{"echo pre-" + tc.family})).WithEvidence(evidence)
	case !reflect.DeepEqual(cfg.SessionSetup, []string{"echo setup-" + tc.family}):
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.SessionSetup = %v, want %v", cfg.SessionSetup, []string{"echo setup-" + tc.family})).WithEvidence(evidence)
	case cfg.SessionSetupScript != tp.Hints.SessionSetupScript:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.SessionSetupScript = %q, want %q", cfg.SessionSetupScript, tp.Hints.SessionSetupScript)).WithEvidence(evidence)
	case !reflect.DeepEqual(cfg.SessionLive, []string{"echo live-" + tc.family}):
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.SessionLive = %v, want %v", cfg.SessionLive, []string{"echo live-" + tc.family})).WithEvidence(evidence)
	case cfg.FingerprintExtra["phase"] != "phase2":
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.FingerprintExtra[phase] = %q, want phase2", cfg.FingerprintExtra["phase"])).WithEvidence(evidence)
	default:
		return workertest.Pass(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			"templateParamsToConfig preserved the resolved startup materialization").WithEvidence(evidence)
	}
}

func initialMessageFirstStartResult(tc phase2ProviderCase, prepared *preparedStart) workertest.Result {
	got, evidence, err := phase2PromptPayload(tc, prepared)
	if err != nil {
		return workertest.Fail(tc.profileID, workertest.RequirementInputInitialMessageFirstStart,
			fmt.Sprintf("PromptSuffix encoding invalid: %v", err)).WithEvidence(evidence)
	}
	want := "Base worker prompt\n\n---\n\nUser message:\nDo the first task."
	switch {
	case got != want:
		return workertest.Fail(tc.profileID, workertest.RequirementInputInitialMessageFirstStart,
			fmt.Sprintf("PromptSuffix payload = %q, want %q", got, want)).WithEvidence(evidence)
	case strings.Count(got, "Do the first task.") != 1:
		return workertest.Fail(tc.profileID, workertest.RequirementInputInitialMessageFirstStart,
			fmt.Sprintf("PromptSuffix payload = %q, want initial message exactly once", got)).WithEvidence(evidence)
	default:
		return workertest.Pass(tc.profileID, workertest.RequirementInputInitialMessageFirstStart,
			"configured initial_message is injected into the first start exactly once").WithEvidence(evidence)
	}
}

func initialMessageResumeResult(tc phase2ProviderCase, prepared *preparedStart) workertest.Result {
	got, evidence, err := phase2PromptPayload(tc, prepared)
	if err != nil {
		return workertest.Fail(tc.profileID, workertest.RequirementInputInitialMessageResume,
			fmt.Sprintf("PromptSuffix encoding invalid: %v", err)).WithEvidence(evidence)
	}
	switch {
	case got != "":
		return workertest.Fail(tc.profileID, workertest.RequirementInputInitialMessageResume,
			fmt.Sprintf("PromptSuffix payload = %q, want no startup user-turn on resume", got)).WithEvidence(evidence)
	case strings.Contains(got, "Do the first task."):
		return workertest.Fail(tc.profileID, workertest.RequirementInputInitialMessageResume,
			fmt.Sprintf("PromptSuffix payload = %q, want no replayed initial message", got)).WithEvidence(evidence)
	case prepared.cfg.Env[startupPromptDeliveredEnv] != "":
		return workertest.Fail(tc.profileID, workertest.RequirementInputInitialMessageResume,
			fmt.Sprintf("%s = %q, want unset on resume", startupPromptDeliveredEnv, prepared.cfg.Env[startupPromptDeliveredEnv])).WithEvidence(evidence)
	default:
		return workertest.Pass(tc.profileID, workertest.RequirementInputInitialMessageResume,
			"resumed sessions do not replay startup prompt material as a new user turn").WithEvidence(evidence)
	}
}

func inputOverrideDefaultsResult(tc phase2ProviderCase, prepared *preparedStart) workertest.Result {
	got, evidence, err := phase2PromptPayload(tc, prepared)
	if err != nil {
		return workertest.Fail(tc.profileID, workertest.RequirementInputOverrideDefaults,
			fmt.Sprintf("PromptSuffix encoding invalid: %v", err)).WithEvidence(evidence)
	}
	if prepared == nil || prepared.candidate.tp.ResolvedProvider == nil {
		return workertest.Fail(tc.profileID, workertest.RequirementInputOverrideDefaults,
			"ResolvedProvider = nil, want provider defaults for override comparison").WithEvidence(evidence)
	}
	defaultArgs := defaultArgsExceptOption(prepared.candidate.tp.ResolvedProvider, "model")
	switch {
	case !containsOrderedArgs(prepared.cfg.Command, defaultArgs):
		return workertest.Fail(tc.profileID, workertest.RequirementInputOverrideDefaults,
			fmt.Sprintf("Command = %q, want non-model default args %v", prepared.cfg.Command, defaultArgs)).WithEvidence(evidence)
	case !containsOrderedArgs(prepared.cfg.Command, tc.wantModelOverrideArgs):
		return workertest.Fail(tc.profileID, workertest.RequirementInputOverrideDefaults,
			fmt.Sprintf("Command = %q, want model override args %v", prepared.cfg.Command, tc.wantModelOverrideArgs)).WithEvidence(evidence)
	case !strings.Contains(got, "Ship it."):
		return workertest.Fail(tc.profileID, workertest.RequirementInputOverrideDefaults,
			fmt.Sprintf("PromptSuffix payload = %q, want initial message", got)).WithEvidence(evidence)
	case strings.Count(got, "Ship it.") != 1:
		return workertest.Fail(tc.profileID, workertest.RequirementInputOverrideDefaults,
			fmt.Sprintf("PromptSuffix payload = %q, want initial message exactly once", got)).WithEvidence(evidence)
	default:
		return workertest.Pass(tc.profileID, workertest.RequirementInputOverrideDefaults,
			"provider default launch flags survive schema overrides while first-input delivery stays exact-once").WithEvidence(evidence)
	}
}

func defaultArgsExceptOption(provider *config.ResolvedProvider, optionKey string) []string {
	if provider == nil {
		return nil
	}
	defaultArgs := provider.ResolveDefaultArgs()
	defaultValue := provider.EffectiveDefaults[optionKey]
	for _, opt := range provider.OptionsSchema {
		if opt.Key == optionKey && defaultValue == "" {
			defaultValue = opt.Default
		}
		if opt.Key != optionKey || defaultValue == "" {
			continue
		}
		for _, choice := range opt.Choices {
			if choice.Value == defaultValue {
				return removeContiguousArgs(defaultArgs, choice.FlagArgs)
			}
		}
	}
	return defaultArgs
}

func removeContiguousArgs(args, remove []string) []string {
	if len(args) == 0 || len(remove) == 0 || len(remove) > len(args) {
		return args
	}
	for i := 0; i <= len(args)-len(remove); i++ {
		matched := true
		for j := range remove {
			if args[i+j] != remove[j] {
				matched = false
				break
			}
		}
		if matched {
			out := append([]string{}, args[:i]...)
			out = append(out, args[i+len(remove):]...)
			return out
		}
	}
	return args
}

func phase2TemplateEvidence(tc phase2ProviderCase, tp TemplateParams) map[string]string {
	evidence := map[string]string{
		"family":       tc.family,
		"profile":      string(tc.profileID),
		"template":     tp.TemplateName,
		"session_name": tp.SessionName,
		"workdir":      tp.WorkDir,
		"command":      tp.Command,
		"hook_enabled": strconv.FormatBool(tp.HookEnabled),
	}
	if tp.ResolvedProvider != nil {
		evidence["resolved_provider"] = tp.ResolvedProvider.Name
		evidence["prompt_mode"] = tp.ResolvedProvider.PromptMode
		evidence["default_args"] = strings.Join(tp.ResolvedProvider.ResolveDefaultArgs(), " ")
		evidence["supports_hooks"] = strconv.FormatBool(tp.ResolvedProvider.SupportsHooks)
	}
	return evidence
}

func phase2ConfigEvidence(tc phase2ProviderCase, tp TemplateParams, cfg runtime.Config) map[string]string {
	evidence := phase2TemplateEvidence(tc, tp)
	evidence["cfg_command"] = cfg.Command
	evidence["cfg_workdir"] = cfg.WorkDir
	evidence["cfg_prompt_flag"] = cfg.PromptFlag
	evidence["cfg_prompt_suffix"] = cfg.PromptSuffix
	evidence["cfg_nudge"] = cfg.Nudge
	evidence["cfg_ready_delay_ms"] = strconv.Itoa(cfg.ReadyDelayMs)
	evidence["cfg_ready_prompt_prefix"] = cfg.ReadyPromptPrefix
	evidence["cfg_process_names"] = strings.Join(cfg.ProcessNames, ",")
	evidence["cfg_emits_permission_warning"] = strconv.FormatBool(cfg.EmitsPermissionWarning)
	evidence["gc_dir"] = cfg.Env["GC_DIR"]
	evidence["gc_template"] = cfg.Env["GC_TEMPLATE"]
	evidence["gc_session_name"] = cfg.Env["GC_SESSION_NAME"]
	evidence["worker_core_marker"] = cfg.Env["WORKER_CORE_MARKER"]
	return evidence
}

func phase2PromptPayload(tc phase2ProviderCase, prepared *preparedStart) (string, map[string]string, error) {
	evidence := phase2PreparedEvidence(tc, prepared)
	raw := ""
	if prepared != nil {
		raw = prepared.cfg.PromptSuffix
	}
	evidence["prompt_suffix_raw"] = raw

	value, err := singleShellArgValue(raw)
	if err != nil {
		evidence["prompt_suffix_parse_error"] = err.Error()
		return "", evidence, err
	}
	evidence["prompt_suffix_payload"] = value
	return value, evidence, nil
}

func phase2PreparedEvidence(tc phase2ProviderCase, prepared *preparedStart) map[string]string {
	evidence := map[string]string{
		"family":  tc.family,
		"profile": string(tc.profileID),
	}
	if prepared == nil {
		return evidence
	}

	evidence["command"] = prepared.cfg.Command
	evidence["workdir"] = prepared.cfg.WorkDir
	evidence["session_name"] = prepared.candidate.name()
	evidence["started_config_hash"] = prepared.candidate.session.Metadata["started_config_hash"]
	evidence["template_overrides"] = prepared.candidate.session.Metadata["template_overrides"]
	evidence["hook_enabled"] = strconv.FormatBool(prepared.candidate.tp.HookEnabled)

	if prepared.candidate.tp.ResolvedProvider != nil {
		evidence["resolved_provider"] = prepared.candidate.tp.ResolvedProvider.Name
		evidence["resolved_default_args"] = strings.Join(prepared.candidate.tp.ResolvedProvider.ResolveDefaultArgs(), " ")
		evidence["supports_hooks"] = strconv.FormatBool(prepared.candidate.tp.ResolvedProvider.SupportsHooks)
	}

	return evidence
}
