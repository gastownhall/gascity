package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/shellquote"
	workertest "github.com/gastownhall/gascity/internal/worker/workertest"
)

type phase3ProviderCase struct {
	profileID             workertest.ProfileID
	family                string
	wantCommand           string
	wantReadyDelayMs      int
	wantReadyPromptPrefix string
	wantProcessNames      []string
	wantEmitsPermission   bool
	wantModelOverride     string
	wantModelOverrideArgs []string
}

func TestPhase3StartupMaterialization(t *testing.T) {
	for _, tc := range selectedPhase3ProviderCases(t) {
		tc := tc
		t.Run(string(tc.profileID), func(t *testing.T) {
			tp := resolvePhase3Template(t, tc)

			t.Run(string(workertest.RequirementStartupCommandMaterialization), func(t *testing.T) {
				if tp.ResolvedProvider == nil {
					t.Fatal("ResolvedProvider = nil")
				}
				if tp.ResolvedProvider.Name != tc.family {
					t.Fatalf("ResolvedProvider.Name = %q, want %q", tp.ResolvedProvider.Name, tc.family)
				}
				if tp.ResolvedProvider.PromptMode != "arg" {
					t.Fatalf("PromptMode = %q, want arg", tp.ResolvedProvider.PromptMode)
				}
				if tp.Command != tc.wantCommand {
					t.Fatalf("Command = %q, want %q", tp.Command, tc.wantCommand)
				}
				if defaultArgs := tp.ResolvedProvider.ResolveDefaultArgs(); !containsOrderedArgs(tp.Command, defaultArgs) {
					t.Fatalf("Command = %q, want default args %v", tp.Command, defaultArgs)
				}
			})

			t.Run(string(workertest.RequirementStartupRuntimeConfigMaterialization), func(t *testing.T) {
				cfg := templateParamsToConfig(tp)

				if cfg.Command != tp.Command {
					t.Fatalf("cfg.Command = %q, want %q", cfg.Command, tp.Command)
				}
				if cfg.WorkDir != tp.WorkDir {
					t.Fatalf("cfg.WorkDir = %q, want %q", cfg.WorkDir, tp.WorkDir)
				}
				if cfg.PromptSuffix == "" {
					t.Fatal("cfg.PromptSuffix = empty, want beacon prompt materialized")
				}
				if cfg.PromptFlag != "" {
					t.Fatalf("cfg.PromptFlag = %q, want empty for arg-mode provider", cfg.PromptFlag)
				}
				if got := cfg.Env["GC_DIR"]; got != tp.WorkDir {
					t.Fatalf("GC_DIR = %q, want %q", got, tp.WorkDir)
				}
				if got := cfg.Env["GC_TEMPLATE"]; got != tp.TemplateName {
					t.Fatalf("GC_TEMPLATE = %q, want %q", got, tp.TemplateName)
				}
				if got := cfg.Env["GC_SESSION_NAME"]; got != tp.SessionName {
					t.Fatalf("GC_SESSION_NAME = %q, want %q", got, tp.SessionName)
				}
				if got := cfg.Env["WORKER_CORE_MARKER"]; got != tc.family {
					t.Fatalf("WORKER_CORE_MARKER = %q, want %q", got, tc.family)
				}
				if cfg.ReadyDelayMs != tc.wantReadyDelayMs {
					t.Fatalf("cfg.ReadyDelayMs = %d, want %d", cfg.ReadyDelayMs, tc.wantReadyDelayMs)
				}
				if cfg.ReadyPromptPrefix != tc.wantReadyPromptPrefix {
					t.Fatalf("cfg.ReadyPromptPrefix = %q, want %q", cfg.ReadyPromptPrefix, tc.wantReadyPromptPrefix)
				}
				if !reflect.DeepEqual(cfg.ProcessNames, tc.wantProcessNames) {
					t.Fatalf("cfg.ProcessNames = %v, want %v", cfg.ProcessNames, tc.wantProcessNames)
				}
				if cfg.EmitsPermissionWarning != tc.wantEmitsPermission {
					t.Fatalf("cfg.EmitsPermissionWarning = %v, want %v", cfg.EmitsPermissionWarning, tc.wantEmitsPermission)
				}
				if cfg.Nudge != "nudge-"+tc.family {
					t.Fatalf("cfg.Nudge = %q, want %q", cfg.Nudge, "nudge-"+tc.family)
				}
				if !reflect.DeepEqual(cfg.PreStart, []string{"echo pre-" + tc.family}) {
					t.Fatalf("cfg.PreStart = %v, want %v", cfg.PreStart, []string{"echo pre-" + tc.family})
				}
				if !reflect.DeepEqual(cfg.SessionSetup, []string{"echo setup-" + tc.family}) {
					t.Fatalf("cfg.SessionSetup = %v, want %v", cfg.SessionSetup, []string{"echo setup-" + tc.family})
				}
				if cfg.SessionSetupScript != tp.Hints.SessionSetupScript {
					t.Fatalf("cfg.SessionSetupScript = %q, want %q", cfg.SessionSetupScript, tp.Hints.SessionSetupScript)
				}
				if !reflect.DeepEqual(cfg.SessionLive, []string{"echo live-" + tc.family}) {
					t.Fatalf("cfg.SessionLive = %v, want %v", cfg.SessionLive, []string{"echo live-" + tc.family})
				}
				if got := cfg.FingerprintExtra["phase"]; got != "phase3" {
					t.Fatalf("cfg.FingerprintExtra[phase] = %q, want phase3", got)
				}
			})
		})
	}
}

func selectedPhase3ProviderCases(t *testing.T) []phase3ProviderCase {
	t.Helper()

	all := []phase3ProviderCase{
		{
			profileID:             "claude/tmux-cli",
			family:                "claude",
			wantCommand:           "claude --dangerously-skip-permissions --effort max",
			wantReadyDelayMs:      10000,
			wantReadyPromptPrefix: "❯ ",
			wantProcessNames:      []string{"node", "claude"},
			wantEmitsPermission:   true,
			wantModelOverride:     "sonnet",
			wantModelOverrideArgs: []string{"--model", "claude-sonnet-4-6"},
		},
		{
			profileID:             "codex/tmux-cli",
			family:                "codex",
			wantCommand:           "codex --dangerously-bypass-approvals-and-sandbox -c model_reasoning_effort=xhigh",
			wantReadyDelayMs:      3000,
			wantReadyPromptPrefix: "",
			wantProcessNames:      []string{"codex"},
			wantEmitsPermission:   false,
			wantModelOverride:     "o3",
			wantModelOverrideArgs: []string{"--model", "o3"},
		},
		{
			profileID:             "gemini/tmux-cli",
			family:                "gemini",
			wantCommand:           "gemini --approval-mode yolo",
			wantReadyDelayMs:      5000,
			wantReadyPromptPrefix: "",
			wantProcessNames:      []string{"gemini"},
			wantEmitsPermission:   false,
			wantModelOverride:     "gemini-2.5-pro",
			wantModelOverrideArgs: []string{"--model", "gemini-2.5-pro"},
		},
	}

	filter := strings.TrimSpace(os.Getenv("PROFILE"))
	if filter == "" {
		return all
	}

	var selected []phase3ProviderCase
	for _, tc := range all {
		if filter == string(tc.profileID) || filter == tc.family {
			selected = append(selected, tc)
		}
	}
	if len(selected) == 0 {
		t.Fatalf("unknown PROFILE %q", filter)
	}
	return selected
}

func resolvePhase3Template(t *testing.T, tc phase3ProviderCase) TemplateParams {
	t.Helper()

	cityPath := t.TempDir()
	params := &agentBuildParams{
		cityName:   "phase3-city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: tc.family},
		lookPath:   func(name string) (string, error) { return filepath.Join("/usr/bin", name), nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agentCfg := &config.Agent{
		Name:               "worker",
		Provider:           tc.family,
		WorkDir:            filepath.Join(".gc", "agents", "phase3", tc.family),
		Nudge:              "nudge-" + tc.family,
		PreStart:           []string{"echo pre-" + tc.family},
		SessionSetup:       []string{"echo setup-" + tc.family},
		SessionSetupScript: filepath.Join("scripts", tc.family+".sh"),
		SessionLive:        []string{"echo live-" + tc.family},
		Env:                map[string]string{"WORKER_CORE_MARKER": tc.family},
	}

	tp, err := resolveTemplate(params, agentCfg, agentCfg.QualifiedName(), map[string]string{"phase": "phase3"})
	if err != nil {
		t.Fatalf("resolveTemplate(%s): %v", tc.profileID, err)
	}
	return tp
}

func phase3TemplateParams(t *testing.T, tc phase3ProviderCase, prompt string) TemplateParams {
	t.Helper()
	tp := resolvePhase3Template(t, tc)
	tp.Prompt = prompt
	return tp
}

func singleShellArg(t *testing.T, quoted string) string {
	t.Helper()

	if quoted == "" {
		return ""
	}
	args := shellquote.Split(quoted)
	if len(args) != 1 {
		t.Fatalf("shellquote.Split(%q) = %v, want 1 arg", quoted, args)
	}
	return args[0]
}

func containsOrderedArgs(command string, args []string) bool {
	if len(args) == 0 {
		return true
	}
	parts := shellquote.Split(command)
	if len(parts) == 0 {
		return false
	}

	start := 0
	for _, want := range args {
		found := false
		for start < len(parts) {
			if parts[start] == want {
				found = true
				start++
				break
			}
			start++
		}
		if !found {
			return false
		}
	}
	return true
}
