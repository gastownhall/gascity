package tmux

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestDoStartSession_OversizedPromptNudgeFallbackSendsFullContentAfterReady is
// the integration-level proof (gastownhall/gascity ga-a1bdd4, deploy-gate
// rework of ga-q8wgom.1.1) that the oversized-argv-prompt guard's nudge
// fallback is more than a pure routing decision: it is actually carried out
// by the real tmux startup orchestration.
//
// cmd/gc's promptDelivery (prompt_delivery_test.go) only proves that an
// oversized prompt is routed into runtime.Config.Nudge instead of argv. It
// never constructs a runtime.Provider or calls Start/readiness/nudge-submit,
// so it cannot show the fallback content actually reaches a session.
//
// That submission happens inside (*Provider).Start (adapter.go), which is a
// thin staging+delegation wrapper around doStartSession -- Start derives env,
// stages overlay files, then calls doStartSession(ctx, newTmuxStartOps(...),
// name, cfg, setupTimeout) unmodified. doStartSession -> finishLaunch ->
// launchOrchestration contains 100% of the actual readiness/nudge sequencing
// (Step 4: ops.waitForReady, then Step 6: ops.sendKeys(name, cfg.Nudge)).
// Exercising doStartSession through its established fakeStartOps seam is
// therefore a genuine test of production orchestration logic, not a re-mock
// of the routing decision already covered elsewhere.
func TestDoStartSession_OversizedPromptNudgeFallbackSendsFullContentAfterReady(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult: true,
	}

	// Representative of promptDelivery's OversizedFallback output: a rendered
	// prompt at cmd/gc's maxPromptSuffixRawBytes threshold (100000 bytes),
	// prepended ahead of the original startup nudge text. This is never
	// placed in argv (the launch command below stays a bare "claude") --
	// it must reach the runtime exclusively through the nudge-submit path.
	bigPrompt := strings.Repeat("a", 100000)
	const nudgeSuffix = "\n\n---\n\nwake"
	nudge := bigPrompt + nudgeSuffix

	cfg := runtime.Config{
		WorkDir:                "/proj",
		Command:                "claude",
		ReadyPromptPrefix:      "> ",
		ProcessNames:           []string{"claude"},
		EmitsPermissionWarning: true,
		Nudge:                  nudge,
	}

	err := doStartSession(context.Background(), ops, "gc-city-mayor", cfg, DefaultConfig().SetupTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	methods := ops.callMethods()
	readyIdx := methodIndex(methods, "waitForReady")
	sendIdx := methodIndex(methods, "sendKeys")
	if readyIdx < 0 {
		t.Fatalf("waitForReady was never called: %v", methods)
	}
	if sendIdx < 0 {
		t.Fatalf("sendKeys was never called -- the oversized-prompt nudge fallback never reached the runtime: %v", methods)
	}
	if readyIdx > sendIdx {
		t.Fatalf("sendKeys (index %d) must be sequenced after waitForReady (index %d), not before: %v", sendIdx, readyIdx, methods)
	}

	sendCalls := callsByMethod(t, ops, "sendKeys", 1)
	sent := sendCalls[0].command
	if len(sent) != len(nudge) {
		t.Fatalf("sendKeys content length = %d, want %d: the full oversized nudge must reach the runtime, not be truncated", len(sent), len(nudge))
	}
	if sent != nudge {
		t.Fatalf("sendKeys content does not match the oversized-fallback nudge byte-for-byte")
	}
	if !strings.HasSuffix(sent, "wake") {
		t.Fatalf("sendKeys content lost the trailing nudge text")
	}

	// The launch command (argv) must stay small: the 100KB prompt went
	// through sendKeys above, never through createSession's command string.
	createCalls := callsByMethod(t, ops, "createSession", 1)
	if got := createCalls[0].command; got != "env -u CI -u NO_COLOR claude" {
		t.Fatalf("createSession command = %q, want the bare launch command unaffected by the oversized nudge", got)
	}
}
