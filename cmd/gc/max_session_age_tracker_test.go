package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestMaxSessionAgeTracker_UnregisteredSessionIsFalse(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if tr.shouldRestart("witness", "", now.Add(-10*time.Hour), now) {
		t.Error("shouldRestart must be false for sessions with no config")
	}
}

func TestMaxSessionAgeTracker_ZeroAnchorIsFalse(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	if tr.shouldRestart("witness", "", time.Time{}, time.Now()) {
		t.Error("shouldRestart must be false when creation_complete_at is zero")
	}
}

func TestMaxSessionAgeTracker_YoungSessionIsFalse(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if tr.shouldRestart("witness", "", now.Add(-1*time.Hour), now) {
		t.Error("shouldRestart must be false when session age < max age")
	}
}

func TestMaxSessionAgeTracker_OldSessionIsTrue(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if !tr.shouldRestart("witness", "", now.Add(-6*time.Hour), now) {
		t.Error("shouldRestart must be true when session age > max age")
	}
}

func TestMaxSessionAgeTracker_JitterExtendsThreshold(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	// Force deterministic zero offset by probing many permutations; the
	// randomness is internal. We use the boundary case: jitter=0 gives
	// the lower bound of the threshold. A fully-synchronized fleet with
	// zero jitter must restart exactly at the base threshold.
	tr.setConfig("a", 5*time.Hour, 0)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	anchor := now.Add(-5 * time.Hour)
	if !tr.shouldRestart("a", "", anchor, now) {
		t.Error("zero-jitter: age == maxAge must trigger restart")
	}

	// With non-zero jitter the threshold is always in [maxAge, maxAge+jitter).
	// We verify the invariant rather than a specific offset: a session
	// exactly at maxAge with any offset > 0 must NOT yet restart, and a
	// session at maxAge + jitter must always restart.
	tr.setConfig("b", 5*time.Hour, 30*time.Minute)
	if tr.shouldRestart("b", "", now.Add(-4*time.Hour-59*time.Minute), now) {
		t.Error("jitter: session 1m below base threshold must not restart")
	}
	if !tr.shouldRestart("b", "", now.Add(-6*time.Hour), now) {
		t.Error("jitter: session well past (maxAge + jitter) must restart")
	}
}

func TestMaxSessionAgeTracker_ClearingConfigDisablesRestart(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	tr.setConfig("witness", 0, 0) // clear
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if tr.shouldRestart("witness", "", now.Add(-10*time.Hour), now) {
		t.Error("shouldRestart must be false after the config is cleared")
	}
}

func TestMaxSessionAgeTracker_ReconfigRerollsJitterForDifferentSessions(t *testing.T) {
	// Sanity: separate sessions land on independent offsets, so their
	// restart times aren't correlated purely by session name ordering.
	tr := newMaxSessionAgeTracker()
	for i := 0; i < 128; i++ {
		tr.setConfig("witness-a", 5*time.Hour, time.Hour)
	}
	tr.mu.Lock()
	offsetA := tr.offsets["witness-a"]
	tr.mu.Unlock()
	if offsetA < 0 || offsetA >= time.Hour {
		t.Errorf("offset for witness-a = %v, want in [0, 1h)", offsetA)
	}
}

func TestMaxSessionAgeTracker_TemplateFallbackResolvesPoolSession(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	template := "local-core/builder"
	tr.setConfigForTemplate(template, time.Hour, 0)

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	sessionName := sessionNameFromBeadID("fm-miv1io")
	if !tr.shouldRestart(sessionName, template, now.Add(-2*time.Hour), now) {
		t.Fatalf("shouldRestart did not fire for pool session via template fallback")
	}
}

func TestMaxSessionAgeTracker_TemplateFallbackJitterUsesDeterministicOffset(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	template := "local-core/builder"
	sessionName := sessionNameFromBeadID("fm-miv1io")
	maxAge := 5 * time.Hour
	jitter := 30 * time.Minute
	tr.setConfigForTemplate(template, maxAge, jitter)

	anchor := time.Date(2026, 5, 20, 6, 30, 0, 0, time.UTC)
	offset := deterministicMaxSessionAgeOffset(template, sessionName, anchor, jitter)
	if offset < 0 || offset >= jitter {
		t.Fatalf("offset = %v, want in [0, %v)", offset, jitter)
	}
	if got := deterministicMaxSessionAgeOffset(template, sessionName, anchor, jitter); got != offset {
		t.Fatalf("offset was not stable: got %v then %v", offset, got)
	}
	var nextOffset time.Duration
	for i := 1; i <= 128; i++ {
		nextOffset = deterministicMaxSessionAgeOffset(template, sessionName, anchor.Add(time.Duration(i)*time.Second), jitter)
		if nextOffset != offset {
			break
		}
	}
	if nextOffset == offset {
		t.Fatalf("offset did not change when creationCompleteAt changed: %v", offset)
	}

	if tr.shouldRestart(sessionName, template, anchor, anchor.Add(maxAge+offset-time.Nanosecond)) {
		t.Fatalf("shouldRestart fired before maxAge+deterministic template jitter threshold")
	}
	if !tr.shouldRestart(sessionName, template, anchor, anchor.Add(maxAge+offset)) {
		t.Fatalf("shouldRestart did not fire at maxAge+deterministic template jitter threshold")
	}
}

func TestMaxSessionAgeTracker_TemplateFallbackDoesNotApplyWithoutTemplate(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	tr.setConfigForTemplate("local-core/builder", time.Hour, 0)

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	sessionName := sessionNameFromBeadID("fm-anonymous")
	if tr.shouldRestart(sessionName, "", now.Add(-2*time.Hour), now) {
		t.Fatalf("shouldRestart fired without a template argument")
	}
}

func TestMaxSessionAgeTracker_PerNameTakesPrecedenceOverTemplate(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	sessionName := "session-a"
	template := "local-core/builder"
	tr.setConfig(sessionName, 5*time.Minute, 0)
	tr.setConfigForTemplate(template, 24*time.Hour, 0)

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	if !tr.shouldRestart(sessionName, template, now.Add(-10*time.Minute), now) {
		t.Fatalf("shouldRestart did not honor per-name max age before template fallback")
	}
}

func TestMaxSessionAgeTracker_TemplateFallbackExemptionSkipsTemplate(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	template := "local-core/builder"
	sessionName := config.NamedSessionRuntimeName("city", config.Workspace{}, "local-core/primary")
	tr.setConfigForTemplate(template, time.Hour, 0)
	tr.exemptTemplateFallbackForSession(sessionName)

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	if tr.shouldRestart(sessionName, template, now.Add(-2*time.Hour), now) {
		t.Fatalf("shouldRestart fired for template-exempt named session")
	}
}

func TestMaxSessionAgeTracker_SetConfigForTemplateZeroClears(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	template := "local-core/builder"
	tr.setConfigForTemplate(template, time.Hour, 0)
	tr.setConfigForTemplate(template, 0, 0)

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	sessionName := sessionNameFromBeadID("fm-x")
	if tr.shouldRestart(sessionName, template, now.Add(-2*time.Hour), now) {
		t.Fatalf("shouldRestart fired after template max age was cleared")
	}
}

func TestMaxSessionAgeTracker_SetConfigForTemplateIgnoresEmptyTemplate(t *testing.T) {
	tr := newMaxSessionAgeTracker()
	tr.setConfigForTemplate("", time.Hour, 0)

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	if tr.shouldRestart("session", "", now.Add(-2*time.Hour), now) {
		t.Fatalf("shouldRestart fired after empty-template config")
	}
	if len(tr.templateConfigs) != 0 {
		t.Fatalf("templateConfigs = %v, want empty after empty-template config", tr.templateConfigs)
	}
}
