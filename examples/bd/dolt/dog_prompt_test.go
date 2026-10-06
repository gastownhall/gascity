package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// claimProtocolFragment is the core pack's canonical source for the hook-claim
// startup protocol. The dog prompt carries the same text verbatim instead of
// composing the fragment by name: the dolt pack cannot import core just for
// the reference — a version-less [imports.core] resolves against upstream
// tags on a networked gc init instead of the binary's canonical pin, and any
// core import here double-composes core's agents into every default city
// through the bd pack's export chain — so the sync assertion below is what
// keeps the two copies from drifting.
const claimProtocolFragment = "internal/bootstrap/packs/core/template-fragments/claim-protocol.template.md"

// claimProtocolFragmentBody returns the fragment's text between its
// {{ define }} / {{ end }} wrapper lines.
func claimProtocolFragmentBody(t *testing.T) string {
	t.Helper()
	// repoRoot is examples/bd/dolt; the core pack lives three levels up.
	path := filepath.Join(repoRoot(t), "..", "..", "..", filepath.FromSlash(claimProtocolFragment))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", claimProtocolFragment, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 3 || !strings.Contains(lines[0], `{{ define "claim-protocol"`) || !strings.Contains(lines[len(lines)-1], "end }}") {
		t.Fatalf("%s no longer has the define/end wrapper shape:\n%s", claimProtocolFragment, data)
	}
	return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
}

// TestDogPromptNamesTheHookClaimCommand pins the dog prompt to the claim
// protocol. The Startup section once described finding routed work without
// naming any command for it, so the model invented a bd list query that
// cannot see pool work — unclaimed routed items have no assignee and wisps
// are hidden from list/ready queries by default — and the stale-database
// order stalled for 41 hours while its wisp sat open (ga-tmzjx6, recurred as
// ga-2q2r0). The prompt must carry the protocol text, stay byte-identical to
// the core pack's claim-protocol fragment body (the canonical copy), match
// the nudge vocabulary in agent.toml, and never reintroduce the
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

	if body := claimProtocolFragmentBody(t); !strings.Contains(prompt, body) {
		t.Errorf("dog prompt drifted from the core claim-protocol fragment; update both together\n--- fragment body ---\n%s", body)
	}

	agentData, err := os.ReadFile(filepath.Join(root, "agents", "dog", "agent.toml"))
	if err != nil {
		t.Fatalf("ReadFile agent.toml: %v", err)
	}
	if strings.Contains(string(agentData), "hook") && !strings.Contains(prompt, "gc hook") {
		t.Error("agent.toml nudge says to check the hook but the prompt never names a gc hook command")
	}
}
