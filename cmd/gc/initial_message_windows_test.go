//go:build windows

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestPrepareStartCandidate_ACPInitialMessageRidesTheStartupPrompt pins that on
// Windows an ACP session's initial message, free text from the session API,
// travels in the agent's startup session/prompt and never on its command line.
// When the agent command is a batch file, cmd.exe would run the message's & as
// a command and expand its %NAME% references.
func TestPrepareStartCandidate_ACPInitialMessageRidesTheStartupPrompt(t *testing.T) {
	const msg = "a&echo>pwned %PATH%"
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker",
			"template":           "worker",
			"generation":         "1",
			"instance_token":     "tok-worker",
			"state":              "creating",
			"template_overrides": `{"initial_message":"a&echo>pwned %PATH%"}`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := prepareStartCandidate(startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:     "worker",
			SessionName:      "worker",
			Command:          "agent.cmd --acp",
			Prompt:           "startup prompt",
			IsACP:            true,
			ResolvedProvider: &config.ResolvedProvider{Name: "agent"},
		},
		order: 0,
	}, &config.City{Agents: []config.Agent{{Name: "worker"}}}, store, &clock.Fake{Time: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}

	if prepared.cfg.PromptSuffix != "" || prepared.cfg.PromptFlag != "" {
		t.Errorf("command-line prompt = %q %q, want none", prepared.cfg.PromptFlag, prepared.cfg.PromptSuffix)
	}
	if prepared.cfg.Command != "agent.cmd --acp" {
		t.Errorf("Command = %q, want the agent command unchanged", prepared.cfg.Command)
	}
	wantNudge := "startup prompt" + startupPromptNudgeSeparator + "User message:\n" + msg
	if prepared.cfg.Nudge != wantNudge {
		t.Errorf("Nudge = %q, want %q", prepared.cfg.Nudge, wantNudge)
	}
}

// TestExecutePreparedStartWave_ACPAgentDeadAfterStartupPromptDiesDuringStartup
// pins that on Windows an ACP start carrying a startup prompt is checked for
// liveness after Start returns. PowerShell launches the agent there, so an
// agent that exits after session/new leaves the PowerShell process alive past
// Start: the prompt write succeeds into the pipe and Start reports success.
// Without the check the prompt, and the initial message it carries, is lost
// while the start counts as a success.
func TestExecutePreparedStartWave_ACPAgentDeadAfterStartupPromptDiesDuringStartup(t *testing.T) {
	sp := &dieAfterStartProvider{Fake: runtime.NewFake()}
	item := preparedStart{
		candidate: startCandidate{
			info: sessionpkg.Info{
				ID:                  "gc-98",
				SessionName:         "test-agent",
				SessionNameMetadata: "test-agent",
				Template:            "worker",
			},
			tp: TemplateParams{
				Command:      "agent --acp",
				SessionName:  "test-agent",
				TemplateName: "worker",
				IsACP:        true,
			},
		},
		cfg: runtime.Config{Command: "agent --acp", Nudge: "startup prompt"},
	}

	results := executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, t.TempDir(), sp, nil, nil, 10*time.Second, 1,
		withStartStabilityWaiter(immediateStartStabilityWaiter))
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if r := results[0]; r.err == nil || !strings.Contains(r.err.Error(), "died during startup") || !r.diedDuringStartup {
		t.Fatalf("result = err %v, diedDuringStartup %v; want the dead agent reported as dying during startup", r.err, r.diedDuringStartup)
	}
}
