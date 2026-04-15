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
