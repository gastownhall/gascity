//go:build integration

package tmux

import (
	"strings"
	"testing"
)

// TestHiddenAttachArmsAndClearsClientAttachedHook guards the event-based
// readiness path: ensureHiddenAttachedClient must arm a per-attach wait-for
// channel via a client-attached hook (so waitForHiddenAttachReady wakes on a
// server-pushed event rather than the legacy poll), and CloseHiddenAttachClient
// must remove it. Without this, a silent fall-back to polling would still pass
// TestHiddenAttachedClientCanSendText while reintroducing the CI-starvation
// flake this hook fixes.
func TestHiddenAttachArmsAndClearsClientAttachedHook(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	session := "gt-test-attach-hook-" + t.Name()
	_ = tm.KillSession(session)
	if err := tm.NewSession(session, "sleep 600"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(session) }()

	if err := tm.ensureHiddenAttachedClient(session); err != nil {
		t.Fatalf("ensureHiddenAttachedClient: %v", err)
	}

	client := tm.hiddenAttachClient(session)
	if client == nil {
		t.Fatal("no hidden client tracked after ensureHiddenAttachedClient")
	}
	if client.channel == "" {
		t.Fatal("hook channel empty: readiness fell back to polling instead of the client-attached hook")
	}
	hooks, err := tm.run("show-hooks", "-t", session)
	if err != nil {
		t.Fatalf("show-hooks: %v", err)
	}
	if !strings.Contains(hooks, "client-attached") || !strings.Contains(hooks, client.channel) {
		t.Fatalf("client-attached hook not armed to signal %q; hooks:\n%s", client.channel, hooks)
	}

	tm.CloseHiddenAttachClient(session)
	if hooks, err := tm.run("show-hooks", "-t", session); err == nil && strings.Contains(hooks, "client-attached") {
		t.Fatalf("client-attached hook not removed on close; hooks:\n%s", hooks)
	}
}
