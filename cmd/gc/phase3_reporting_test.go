package main

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	workertest "github.com/gastownhall/gascity/internal/worker/workertest"
)

func newPhase3Reporter(t *testing.T, suite string) *workertest.SuiteReporter {
	t.Helper()

	return workertest.NewSuiteReporter(t, suite, map[string]string{
		"tier":      "worker-core",
		"phase":     "phase3",
		"component": "cmd-gc",
	})
}

func startupCommandMaterializationResult(tc phase3ProviderCase, tp TemplateParams) workertest.Result {
	evidence := phase3TemplateEvidence(tc, tp)
	switch {
	case tp.ResolvedProvider == nil:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization, "ResolvedProvider = nil").WithEvidence(evidence)
	case tp.ResolvedProvider.Name != tc.family:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			fmt.Sprintf("ResolvedProvider.Name = %q, want %q", tp.ResolvedProvider.Name, tc.family)).WithEvidence(evidence)
	case tp.ResolvedProvider.PromptMode != "arg":
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			fmt.Sprintf("PromptMode = %q, want arg", tp.ResolvedProvider.PromptMode)).WithEvidence(evidence)
	case tp.Command != tc.wantCommand:
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			fmt.Sprintf("Command = %q, want %q", tp.Command, tc.wantCommand)).WithEvidence(evidence)
	case !containsOrderedArgs(tp.Command, tp.ResolvedProvider.ResolveDefaultArgs()):
		return workertest.Fail(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			fmt.Sprintf("Command = %q, want default args %v", tp.Command, tp.ResolvedProvider.ResolveDefaultArgs())).WithEvidence(evidence)
	default:
		return workertest.Pass(tc.profileID, workertest.RequirementStartupCommandMaterialization,
			"provider defaults and launch semantics materialized into the startup command").WithEvidence(evidence)
	}
}

func startupRuntimeConfigMaterializationResult(tc phase3ProviderCase, tp TemplateParams, cfg runtime.Config) workertest.Result {
	evidence := phase3ConfigEvidence(tc, tp, cfg)
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
	case cfg.PromptFlag != "":
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
	case cfg.FingerprintExtra["phase"] != "phase3":
		return workertest.Fail(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			fmt.Sprintf("cfg.FingerprintExtra[phase] = %q, want phase3", cfg.FingerprintExtra["phase"])).WithEvidence(evidence)
	default:
		return workertest.Pass(tc.profileID, workertest.RequirementStartupRuntimeConfigMaterialization,
			"templateParamsToConfig preserved the resolved startup materialization").WithEvidence(evidence)
	}
}

func initialMessageFirstStartResult(tc phase3ProviderCase, prepared *preparedStart) workertest.Result {
	got, evidence, err := phase3PromptPayload(tc, prepared)
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

func initialMessageResumeResult(tc phase3ProviderCase, prepared *preparedStart) workertest.Result {
	got, evidence, err := phase3PromptPayload(tc, prepared)
	if err != nil {
		return workertest.Fail(tc.profileID, workertest.RequirementInputInitialMessageResume,
			fmt.Sprintf("PromptSuffix encoding invalid: %v", err)).WithEvidence(evidence)
	}
	switch {
	case got != "Base worker prompt":
		return workertest.Fail(tc.profileID, workertest.RequirementInputInitialMessageResume,
			fmt.Sprintf("PromptSuffix payload = %q, want %q", got, "Base worker prompt")).WithEvidence(evidence)
	case strings.Contains(got, "Do the first task."):
		return workertest.Fail(tc.profileID, workertest.RequirementInputInitialMessageResume,
			fmt.Sprintf("PromptSuffix payload = %q, want no replayed initial message", got)).WithEvidence(evidence)
	default:
		return workertest.Pass(tc.profileID, workertest.RequirementInputInitialMessageResume,
			"resumed sessions do not replay the initial_message after first start").WithEvidence(evidence)
	}
}

func inputOverrideDefaultsResult(tc phase3ProviderCase, prepared *preparedStart) workertest.Result {
	got, evidence, err := phase3PromptPayload(tc, prepared)
	if err != nil {
		return workertest.Fail(tc.profileID, workertest.RequirementInputOverrideDefaults,
			fmt.Sprintf("PromptSuffix encoding invalid: %v", err)).WithEvidence(evidence)
	}
	if prepared == nil || prepared.candidate.tp.ResolvedProvider == nil {
		return workertest.Fail(tc.profileID, workertest.RequirementInputOverrideDefaults,
			"ResolvedProvider = nil, want provider defaults for override comparison").WithEvidence(evidence)
	}
	defaultArgs := prepared.candidate.tp.ResolvedProvider.ResolveDefaultArgs()
	switch {
	case !containsOrderedArgs(prepared.cfg.Command, defaultArgs):
		return workertest.Fail(tc.profileID, workertest.RequirementInputOverrideDefaults,
			fmt.Sprintf("Command = %q, want default args %v", prepared.cfg.Command, defaultArgs)).WithEvidence(evidence)
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

func phase3TemplateEvidence(tc phase3ProviderCase, tp TemplateParams) map[string]string {
	evidence := map[string]string{
		"family":       tc.family,
		"profile":      string(tc.profileID),
		"template":     tp.TemplateName,
		"session_name": tp.SessionName,
		"workdir":      tp.WorkDir,
		"command":      tp.Command,
	}
	if tp.ResolvedProvider != nil {
		evidence["resolved_provider"] = tp.ResolvedProvider.Name
		evidence["prompt_mode"] = tp.ResolvedProvider.PromptMode
		evidence["default_args"] = strings.Join(tp.ResolvedProvider.ResolveDefaultArgs(), " ")
	}
	return evidence
}

func phase3ConfigEvidence(tc phase3ProviderCase, tp TemplateParams, cfg runtime.Config) map[string]string {
	evidence := phase3TemplateEvidence(tc, tp)
	evidence["cfg_command"] = cfg.Command
	evidence["cfg_workdir"] = cfg.WorkDir
	evidence["cfg_prompt_flag"] = cfg.PromptFlag
	evidence["cfg_prompt_suffix"] = cfg.PromptSuffix
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

func phase3PromptPayload(tc phase3ProviderCase, prepared *preparedStart) (string, map[string]string, error) {
	evidence := phase3PreparedEvidence(tc, prepared)
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

func phase3PreparedEvidence(tc phase3ProviderCase, prepared *preparedStart) map[string]string {
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

	if prepared.candidate.tp.ResolvedProvider != nil {
		evidence["resolved_provider"] = prepared.candidate.tp.ResolvedProvider.Name
		evidence["resolved_default_args"] = strings.Join(prepared.candidate.tp.ResolvedProvider.ResolveDefaultArgs(), " ")
	}

	return evidence
}
