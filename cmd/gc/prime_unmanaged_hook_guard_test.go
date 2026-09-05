package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// A `gc prime --hook` invocation that cannot prove it belongs to a live
// Gas City session must stay silent.
//
// Every provider overlay gc stages (11 providers, 13 files) runs
// `gc prime --hook` from the session work directory, and that work directory is
// commonly the city or rig root. The overlays are ordinary
// directory-scoped provider config — .opencode/plugins/gascity.js,
// .codex/hooks.json, .pi/extensions/gc-hooks.js and friends — so a human who
// simply opens that provider in the same directory loads them too. Its output
// is the full Gas City worker persona ("You are an agent in a Gas City
// workspace. Claim available work and execute it."), which the provider
// prepends to the system prompt on every turn. A human-launched session then
// ignores what the human typed and starts claiming queue beads instead, and
// because it has no agent identity the claim fails with
// "gc hook: agent not specified".
//
// The guard for this already existed but was reachable only when the hook
// context named a SessionStart event. Providers whose hooks pass no event name
// bypassed it, so whether a human-launched session got hijacked depended on
// whether that provider's overlay happened to set GC_HOOK_EVENT_NAME.
//
// Explicit `gc prime` (no --hook) is unaffected: a human asking for the prompt
// still gets it. See TestDoPrimeExplicitInvocationStillFormatsDefaultFallback.

// unmanagedPrimeHookEnv clears everything that could make prime believe it is
// running inside a live managed session.
func unmanagedPrimeHookEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GC_CITY", filepath.Join(t.TempDir(), "missing-city"))
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_SESSION_NAME", "")
	t.Setenv("GC_TEMPLATE", "")
	t.Setenv("GC_HOOK_EVENT_NAME", "")
	t.Setenv("GC_HOOK_SOURCE", "")
	t.Setenv(managedSessionHookEnv, "")
	t.Setenv(startupPromptDeliveredEnv, "")
}

// TestDoPrimeHookWithoutEventNameStaysSilentWhenUnmanaged is the regression
// guard for the hijack: this is exactly how the OpenCode plugin invokes prime —
// hook mode, no hook event name, no managed-session identity.
func TestDoPrimeHookWithoutEventNameStaysSilentWhenUnmanaged(t *testing.T) {
	unmanagedPrimeHookEnv(t)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, "", false)
	if code != 0 {
		t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
	}
	if out := stdout.String(); out != "" {
		t.Fatalf("unmanaged hook invocation emitted the worker persona; a human-launched\n"+
			"provider session in this directory would be hijacked into claiming work.\ngot: %q", out)
	}
}

// TestDoPrimeHookWithoutEventNameStaysSilentForEveryHookFormat pins the guard
// across the provider hook formats, since the bypass was provider-dependent.
func TestDoPrimeHookWithoutEventNameStaysSilentForEveryHookFormat(t *testing.T) {
	for _, format := range []string{"", hookOutputFormatCodex} {
		t.Run("format="+format, func(t *testing.T) {
			unmanagedPrimeHookEnv(t)

			var stdout, stderr bytes.Buffer
			code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, format, false)
			if code != 0 {
				t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
			}
			if out := stdout.String(); out != "" {
				t.Fatalf("unmanaged hook invocation emitted output for format %q: %q", format, out)
			}
		})
	}
}

// TestPersistPrimeHookProviderSessionKeyQuietWhenNotAManagedSession covers the
// error banner the hijack also produced. A provider overlay sets
// GC_PROVIDER_SESSION_ID_REQUIRED on every session it opens, managed or not, so
// an absent GC_SESSION_ID is the ordinary state of a human-launched session,
// not a fault. Reporting it surfaced
// "gascity opencode plugin: gc prime --hook: provider session key not persisted
// for opencode: GC_SESSION_ID is empty" as an error banner in the provider UI.
func TestPersistPrimeHookProviderSessionKeyQuietWhenNotAManagedSession(t *testing.T) {
	unmanagedPrimeHookEnv(t)
	t.Setenv("GC_PROVIDER_SESSION_ID_REQUIRED", "opencode")
	t.Setenv("GC_PROVIDER_SESSION_ID", "ses_opencode_123")

	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey("", &stderr)

	if got := stderr.String(); got != "" {
		t.Fatalf("an unmanaged session has no session key to persist, so this is\n"+
			"expected state and must not be reported as a failure.\ngot: %q", got)
	}
}

// TestPersistPrimeHookProviderSessionKeyStillWarnsForManagedSession is the
// counterweight: silencing the unmanaged case must not silence a genuinely
// broken managed session that lost GC_SESSION_ID. GC_MANAGED_SESSION_HOOK is
// set only by gc's own managed hook wrappers, so it still proves managed intent.
func TestPersistPrimeHookProviderSessionKeyStillWarnsForManagedSession(t *testing.T) {
	unmanagedPrimeHookEnv(t)
	t.Setenv("GC_PROVIDER_SESSION_ID_REQUIRED", "opencode")
	t.Setenv("GC_PROVIDER_SESSION_ID", "ses_opencode_123")
	t.Setenv(managedSessionHookEnv, "1")

	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey("", &stderr)

	got := stderr.String()
	if !strings.Contains(got, "provider session key not persisted") ||
		!strings.Contains(got, "GC_SESSION_ID is empty") {
		t.Fatalf("a managed hook missing GC_SESSION_ID is a real fault and must stay\n"+
			"reported.\ngot: %q", got)
	}
}

// TestPersistPrimeHookProviderSessionKeyStillWarnsWithAgentIdentity covers the
// other managed-intent marker: an agent identity in the environment.
func TestPersistPrimeHookProviderSessionKeyStillWarnsWithAgentIdentity(t *testing.T) {
	unmanagedPrimeHookEnv(t)
	t.Setenv("GC_PROVIDER_SESSION_ID_REQUIRED", "opencode")
	t.Setenv("GC_PROVIDER_SESSION_ID", "ses_opencode_123")
	t.Setenv("GC_AGENT", "worker")

	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey("", &stderr)

	if got := stderr.String(); !strings.Contains(got, "GC_SESSION_ID is empty") {
		t.Fatalf("an agent identity proves managed intent; the diagnostic must stay.\ngot: %q", got)
	}
}

// TestDoPrimeHookExplicitAgentStillServedWhenUnmanaged pins the carve-out the
// mail and nudge guards already have: naming an agent is a deliberate request,
// and no staged overlay ever passes one, so it must not be silenced.
func TestDoPrimeHookExplicitAgentStillServedWhenUnmanaged(t *testing.T) {
	unmanagedPrimeHookEnv(t)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithHookFormat([]string{"mayor"}, &stdout, &stderr, true, "", false)
	if code != 0 {
		t.Fatalf("doPrimeWithHookFormat() = %d, want 0; stderr=%q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Fatal("an explicitly named agent must still be served; the identity guard over-suppressed")
	}
}
