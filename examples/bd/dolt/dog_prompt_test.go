package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDogPromptNamesTheHookClaimCommand pins the dog prompt to the claim
// protocol. The Startup section once described finding routed work without
// naming any command for it, so the model invented a bd list query that
// cannot see pool work — unclaimed routed items have no assignee and wisps
// are hidden from bd list/bd ready by default — and the stale-database order
// stalled for 41 hours while its wisp sat open (ga-tmzjx6, recurred as
// ga-2q2r0). The prompt must name gc hook --claim, must match the nudge
// vocabulary in agent.toml, and must not reintroduce the
// claim-without-discovery idiom.
func TestDogPromptNamesTheHookClaimCommand(t *testing.T) {
	root := repoRoot(t)
	promptData, err := os.ReadFile(filepath.Join(root, "agents", "dog", "prompt.template.md"))
	if err != nil {
		t.Fatalf("ReadFile prompt: %v", err)
	}
	prompt := string(promptData)
	for _, want := range []string{
		"gc hook --claim --drain-ack --json",
		"ga-tmzjx6",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("dog prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, "gc bd update <id> --claim") {
		t.Error("dog prompt reintroduces the claim-without-discovery idiom; claims go through gc hook --claim")
	}

	agentData, err := os.ReadFile(filepath.Join(root, "agents", "dog", "agent.toml"))
	if err != nil {
		t.Fatalf("ReadFile agent.toml: %v", err)
	}
	if strings.Contains(string(agentData), "hook") && !strings.Contains(prompt, "gc hook") {
		t.Error("agent.toml nudge says to check the hook but the prompt never names a gc hook command")
	}
}
