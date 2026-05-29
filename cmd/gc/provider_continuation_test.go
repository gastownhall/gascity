package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestProviderHistoryInvalidDetectedUsesConfiguredClassifier(t *testing.T) {
	resolved := &config.ResolvedProvider{
		FatalResumeErrors: []config.ProviderFatalResumeError{config.ProviderFatalResumeClaudeThinkingBlockMutation},
	}
	output := "400 invalid request: messages.12.content.0.type redacted_thinking cannot be modified"
	if !providerHistoryInvalidDetected(resolved, nil, output) {
		t.Fatal("expected configured Claude thinking-block mutation classifier to match")
	}
	if providerHistoryInvalidDetected(&config.ResolvedProvider{}, nil, output) {
		t.Fatal("unconfigured provider matched fatal resume error")
	}
}

func TestDrainAckStopBoundary(t *testing.T) {
	agentAck := drainAckStopBoundary(beads.Bead{}, false)
	if agentAck != sessionpkg.StopBoundaryAgentAck {
		t.Fatalf("agent ack boundary = %q", agentAck)
	}
	reconcilerIdle := drainAckStopBoundary(beads.Bead{Metadata: map[string]string{
		"last_stop_boundary": string(sessionpkg.StopBoundaryIdleVerified),
	}}, true)
	if reconcilerIdle != sessionpkg.StopBoundaryIdleVerified {
		t.Fatalf("reconciler idle boundary = %q", reconcilerIdle)
	}
	reconcilerUnknown := drainAckStopBoundary(beads.Bead{Metadata: map[string]string{}}, true)
	if reconcilerUnknown != sessionpkg.StopBoundaryUnknown {
		t.Fatalf("reconciler unknown boundary = %q", reconcilerUnknown)
	}
}
