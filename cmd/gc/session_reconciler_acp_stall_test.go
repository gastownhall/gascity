package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionacp "github.com/gastownhall/gascity/internal/runtime/acp"
)

// transportCapabilityProvider drives a fake runtime while reporting a real
// transport's capability surface. It is how these tests assert on the shipped
// ACP declaration rather than on a hand-copied duplicate of it: if the ACP
// provider ever stops reporting activity, the stall test below fails.
type transportCapabilityProvider struct {
	runtime.Provider
	caps  runtime.ProviderCapabilities
	sleep runtime.SessionSleepCapability
}

func (p *transportCapabilityProvider) Capabilities() runtime.ProviderCapabilities {
	return p.caps
}

func (p *transportCapabilityProvider) SleepCapability(string) runtime.SessionSleepCapability {
	return p.sleep
}

// acpShapedProvider wraps the fake runtime with the live ACP provider's
// capability surface.
func acpShapedProvider(t *testing.T, sp runtime.Provider) runtime.Provider {
	t.Helper()
	acp := sessionacp.NewProviderWithDir(t.TempDir(), sessionacp.Config{})
	return &transportCapabilityProvider{
		Provider: sp,
		caps:     acp.Capabilities(),
		sleep:    acp.SleepCapability(""),
	}
}

// TestReconcileSessionBeads_ProgressStallUsesReportedACPActivityWhenOptedIn
// verifies that ACP participates in the existing progress-stall policy when an
// operator explicitly configures it. An aged activity timestamp proves only
// that no session/update was observed during the interval; it does not identify
// why activity stopped or independently prove that the provider session died.
func TestReconcileSessionBeads_ProgressStallUsesReportedACPActivityWhenOptedIn(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)

	// newProgressStallTestEnv sets a 30m progress_stall_timeout and pins the
	// reported activity an hour back. The policy is opt-in; without that
	// configuration the reconciler does not recycle based on activity age.
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q is not running", sessionName)
	}

	env.reconcileAtPathWithProvider(t.TempDir(), acpShapedProvider(t, env.sp), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still reported running after configured progress-stall threshold", sessionName)
	}
	if !strings.Contains(env.stderr.String(), "progress-stalled") {
		t.Fatalf("stderr = %q, want a progress-stalled diagnostic", env.stderr.String())
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

// TestReconcileSessionBeads_ProgressStallSkipsProviderWithoutActivitySignal
// pins the other half of the contract, so the fix above stays a capability
// declaration and never degrades into removing the gate.
//
// A transport that cannot observe activity must still be left alone: recycling
// it would be based on missing evidence rather than an aged observation.
func TestReconcileSessionBeads_ProgressStallSkipsProviderWithoutActivitySignal(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)

	sp := &transportCapabilityProvider{
		Provider: env.sp,
		caps:     runtime.ProviderCapabilities{},
		sleep:    runtime.SessionSleepCapabilityTimedOnly,
	}
	env.reconcileAtPathWithProvider(t.TempDir(), sp, []beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q was recycled on a transport that cannot report activity", sessionName)
	}
	if strings.Contains(env.stderr.String(), "progress-stalled") {
		t.Fatalf("stderr = %q, want no progress-stalled diagnostic", env.stderr.String())
	}
}

// TestSessionActivityReportableForACPTransport is the direct unit assertion on
// the capability gate used by activity-derived policies.
func TestSessionActivityReportableForACPTransport(t *testing.T) {
	acp := sessionacp.NewProviderWithDir(t.TempDir(), sessionacp.Config{})

	if !sessionActivityReportable(acp, "test-session") {
		t.Fatal("sessionActivityReportable = false for the ACP transport")
	}
}
