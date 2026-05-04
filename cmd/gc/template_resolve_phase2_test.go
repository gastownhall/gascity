package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/shellquote"
	workertest "github.com/gastownhall/gascity/internal/worker/workertest"
)

type phase2ProviderCase struct {
	profileID             workertest.ProfileID
	family                string
	wantCommand           string
	wantCommandPrefix     string
	wantPromptMode        string
	wantPromptFlag        string
	wantSettingsArg       bool
	wantReadyDelayMs      int
	wantReadyPromptPrefix string
	wantProcessNames      []string
	wantEmitsPermission   bool
	wantModelOverride     string
	wantModelOverrideArgs []string
}

func TestPhase2StartupMaterialization(t *testing.T) {
	reporter := newPhase2Reporter(t, "phase2-startup-materialization")

	for _, tc := range selectedPhase2ProviderCases(t) {
		tc := tc
		t.Run(string(tc.profileID), func(t *testing.T) {
			tp := resolvePhase2Template(t, tc)

			t.Run(string(workertest.RequirementStartupCommandMaterialization), func(t *testing.T) {
				reporter.Require(t, startupCommandMaterializationResult(tc, tp))
			})

			t.Run(string(workertest.RequirementStartupRuntimeConfigMaterialization), func(t *testing.T) {
				reporter.Require(t, startupRuntimeConfigMaterializationResult(tc, tp, templateParamsToConfig(tp)))
			})
		})
	}
}

func selectedPhase2ProviderCases(t *testing.T) []phase2ProviderCase {
	t.Helper()

	all := []phase2ProviderCase{
		{
			profileID:             "claude/tmux-cli",
			family:                "claude",
			wantCommandPrefix:     "claude --dangerously-skip-permissions --effort max",
			wantSettingsArg:       true,
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
			wantCommand:           "codex --dangerously-bypass-approvals-and-sandbox --model gpt-5.5 -c model_reasoning_effort=xhigh",
			wantReadyDelayMs:      3000,
			wantReadyPromptPrefix: "› ",
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
			wantReadyPromptPrefix: "> ",
			wantProcessNames:      []string{"gemini", "node"},
			wantEmitsPermission:   false,
			wantModelOverride:     "gemini-2.5-pro",
			wantModelOverrideArgs: []string{"--model", "gemini-2.5-pro"},
		},
		{
			profileID:        "opencode/tmux-cli",
			family:           "opencode",
			wantCommand:      "opencode",
			wantPromptMode:   "flag",
			wantPromptFlag:   "--prompt",
			wantReadyDelayMs: 8000,
			wantProcessNames: []string{"opencode", "node", "bun"},
		},
	}

	filter := strings.TrimSpace(os.Getenv("PROFILE"))
	if filter == "" {
		return all
	}

	var selected []phase2ProviderCase
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

func phase2ProviderCaseForFamily(t *testing.T, family string) phase2ProviderCase {
	t.Helper()

	if filter := strings.TrimSpace(os.Getenv("PROFILE")); filter != "" && filter != family && !strings.HasPrefix(filter, family+"/") {
		t.Skipf("PROFILE=%q excludes %s phase2 provider case", filter, family)
	}
	for _, tc := range selectedPhase2ProviderCases(t) {
		if tc.family == family {
			return tc
		}
	}
	t.Fatalf("phase2 provider case for family %q not found", family)
	return phase2ProviderCase{}
}

func resolvePhase2Template(t *testing.T, tc phase2ProviderCase) TemplateParams {
	t.Helper()

	cityPath := t.TempDir()
	params := &agentBuildParams{
		cityName:   "phase2-city",
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
		WorkDir:            filepath.Join(".gc", "agents", "phase2", tc.family),
		Nudge:              "nudge-" + tc.family,
		PreStart:           []string{"echo pre-" + tc.family},
		SessionSetup:       []string{"echo setup-" + tc.family},
		SessionSetupScript: filepath.Join("scripts", tc.family+".sh"),
		SessionLive:        []string{"echo live-" + tc.family},
		Env:                map[string]string{"WORKER_CORE_MARKER": tc.family},
	}
	if strings.HasSuffix(string(tc.profileID), "/tmux-cli") {
		agentCfg.Session = "tmux"
	}

	tp, err := resolveTemplate(params, agentCfg, agentCfg.QualifiedName(), map[string]string{"phase": "phase2"})
	if err != nil {
		t.Fatalf("resolveTemplate(%s): %v", tc.profileID, err)
	}
	return tp
}

func phase2TemplateParams(t *testing.T, tc phase2ProviderCase, prompt string) TemplateParams {
	t.Helper()
	tp := resolvePhase2Template(t, tc)
	tp.Prompt = prompt
	return tp
}

func singleShellArgValue(quoted string) (string, error) {
	if quoted == "" {
		return "", nil
	}
	args := shellquote.Split(quoted)
	if len(args) != 1 {
		return "", fmt.Errorf("shellquote.Split(%q) = %v, want 1 arg", quoted, args)
	}
	return args[0], nil
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

func commandFlagValue(command, flag string) (string, bool) {
	parts := shellquote.Split(command)
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if part == flag {
			if i+1 >= len(parts) {
				return "", false
			}
			return parts[i+1], true
		}
		if strings.HasPrefix(part, flag+"=") {
			return strings.TrimPrefix(part, flag+"="), true
		}
	}
	return "", false
}
