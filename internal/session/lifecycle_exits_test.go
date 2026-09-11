package session

import (
	"testing"
	"time"
)

// These tests pin the exit-classification ladder extracted from the
// reconciler's checkStability/checkChurn pair. The lanes are a contract:
// rate-limit screen detection, then rapid-crash (death inside the stability
// window), then the churn band (survived stability, died before
// productivity). The rapid-crash lane intentionally ignores
// pending_create_claim and sleep_reason; the churn lane respects both —
// asymmetries inherited from the original predicates. Caller-side
// characterization lives in cmd/gc/session_reconcile_test.go
// (TestCheckStability_*, TestCheckChurn_*; SESSION-RECON-010).

func exitFacts() ExitFacts {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	return ExitFacts{
		Alive:                 false,
		LastWokeAt:            now.Add(-10 * time.Second).Format(time.RFC3339),
		Now:                   now,
		StabilityThreshold:    30 * time.Second,
		ProductivityThreshold: 5 * time.Minute,
	}
}

func withWokeAge(f ExitFacts, age time.Duration) ExitFacts {
	f.LastWokeAt = f.Now.Add(-age).Format(time.RFC3339)
	return f
}

func TestDecideSessionExitAliveIsNone(t *testing.T) {
	f := exitFacts()
	f.Alive = true
	if got := DecideSessionExit(f); got != ExitNone {
		t.Fatalf("alive session classified %v, want ExitNone", got)
	}
}

func TestDecideSessionExitRapidCrashLane(t *testing.T) {
	cases := []struct {
		name string
		mut  func(ExitFacts) ExitFacts
		want ExitOutcome
	}{
		{
			"death inside stability window is a rapid crash",
			func(f ExitFacts) ExitFacts { return f }, ExitRapidCrash,
		},
		{
			"subprocess provider exits are intentional",
			func(f ExitFacts) ExitFacts { f.SubprocessProvider = true; return f }, ExitNone,
		},
		{
			"pending drain is not a crash",
			func(f ExitFacts) ExitFacts { f.DrainPending = true; return f }, ExitNone,
		},
		{
			"in-flight pending create is not a crash",
			func(f ExitFacts) ExitFacts { f.PendingCreateStartInFlight = true; return f }, ExitNone,
		},
		{
			"empty last_woke_at disables exit tracking",
			func(f ExitFacts) ExitFacts { f.LastWokeAt = ""; return f }, ExitNone,
		},
		{
			"unparseable last_woke_at disables exit tracking",
			func(f ExitFacts) ExitFacts { f.LastWokeAt = "not-a-time"; return f }, ExitNone,
		},
		{
			"rapid crash fires even with a pending create claim once start is no longer in flight",
			func(f ExitFacts) ExitFacts { f.PendingCreateClaim = true; return f }, ExitRapidCrash,
		},
		{
			"rapid crash fires even with a deliberate sleep reason",
			func(f ExitFacts) ExitFacts { f.SleepReason = "idle"; return f }, ExitRapidCrash,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideSessionExit(tc.mut(exitFacts())); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecideSessionExitRateLimitLane(t *testing.T) {
	base := exitFacts()
	base.ScreenAvailable = true

	if got := DecideSessionExit(base); got != ExitGatherScreen {
		t.Fatalf("unknown screen with peek available: got %v, want ExitGatherScreen", got)
	}

	f := base
	f.Screen = ScreenRateLimit
	if got := DecideSessionExit(f); got != ExitRateLimitQuarantine {
		t.Fatalf("rate-limit screen: got %v, want ExitRateLimitQuarantine", got)
	}

	f = base
	f.Screen = ScreenOther
	if got := DecideSessionExit(f); got != ExitRapidCrash {
		t.Fatalf("non-rate-limit screen falls through to rapid crash: got %v", got)
	}

	// Without a peek source the lane is skipped entirely.
	f = exitFacts()
	if got := DecideSessionExit(f); got != ExitRapidCrash {
		t.Fatalf("no peek source: got %v, want ExitRapidCrash", got)
	}

	// The rate-limit lane shares the crash candidacy gates.
	f = base
	f.DrainPending = true
	if got := DecideSessionExit(f); got != ExitNone {
		t.Fatalf("draining session must not enter the rate-limit lane: got %v", got)
	}

	// Screen state is irrelevant outside the stability window: the lane is
	// candidacy-gated, not band-gated, matching the original predicate.
	f = withWokeAge(base, 90*time.Second)
	if got := DecideSessionExit(f); got != ExitGatherScreen {
		t.Fatalf("rate-limit candidacy is not band-limited: got %v, want ExitGatherScreen", got)
	}
}

func TestDecideSessionExitChurnBand(t *testing.T) {
	cases := []struct {
		name string
		mut  func(ExitFacts) ExitFacts
		want ExitOutcome
	}{
		{
			"death in the churn band records churn",
			func(f ExitFacts) ExitFacts { return withWokeAge(f, 90*time.Second) }, ExitChurn,
		},
		{
			"death exactly at the stability threshold is churn, not rapid",
			func(f ExitFacts) ExitFacts { return withWokeAge(f, 30*time.Second) }, ExitChurn,
		},
		{
			"productive death clears churn instead of recording it",
			func(f ExitFacts) ExitFacts { return withWokeAge(f, 5*time.Minute) }, ExitProductiveDeath,
		},
		{
			"pending create claim suppresses churn",
			func(f ExitFacts) ExitFacts {
				f = withWokeAge(f, 90*time.Second)
				f.PendingCreateClaim = true
				return f
			}, ExitNone,
		},
		{
			"deliberate sleep reason suppresses churn",
			func(f ExitFacts) ExitFacts {
				f = withWokeAge(f, 90*time.Second)
				f.SleepReason = "idle-timeout"
				return f
			}, ExitNone,
		},
		{
			"subprocess provider suppresses churn",
			func(f ExitFacts) ExitFacts {
				f = withWokeAge(f, 90*time.Second)
				f.SubprocessProvider = true
				return f
			}, ExitNone,
		},
		{
			"pending drain suppresses churn",
			func(f ExitFacts) ExitFacts {
				f = withWokeAge(f, 90*time.Second)
				f.DrainPending = true
				return f
			}, ExitNone,
		},
		{
			"in-flight pending create suppresses both lanes",
			func(f ExitFacts) ExitFacts {
				f = withWokeAge(f, 90*time.Second)
				f.PendingCreateStartInFlight = true
				f.PendingCreateClaim = true
				return f
			}, ExitNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideSessionExit(tc.mut(exitFacts())); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsDeliberateSleepReason(t *testing.T) {
	deliberate := []string{
		"idle", "idle-timeout", "no-wake-reason", "config-drift", "drained",
		"city-stop", "user-hold", "wait-hold", "rate_limit", "failed-create",
		"provider-terminal-error",
		" idle ",
	}
	for _, reason := range deliberate {
		if !IsDeliberateSleepReason(reason) {
			t.Errorf("IsDeliberateSleepReason(%q) = false, want true", reason)
		}
	}
	for _, reason := range []string{"", "crash", "context-churn", "quarantine", "max-session-age"} {
		if IsDeliberateSleepReason(reason) {
			t.Errorf("IsDeliberateSleepReason(%q) = true, want false", reason)
		}
	}
}

// The gather loop must terminate: once the screen fact is known the decider
// may not ask for it again.
func TestDecideSessionExitGatherTerminates(t *testing.T) {
	f := exitFacts()
	f.ScreenAvailable = true
	for _, s := range []ScreenFact{ScreenRateLimit, ScreenOther} {
		f.Screen = s
		if got := DecideSessionExit(f); got == ExitGatherScreen {
			t.Fatalf("screen %v already known but decider asked again", s)
		}
	}
}

func TestWakeFailureAccrualPatchBelowThreshold(t *testing.T) {
	until := time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC)
	acc := WakeFailureAccrualPatch(1, 5, until)
	if acc.Quarantined {
		t.Fatal("two failures with max 5 must not quarantine")
	}
	want := MetadataPatch{"wake_attempts": "2"}
	assertPatch(t, acc.Patch, want)
}

func TestWakeFailureAccrualPatchAtThreshold(t *testing.T) {
	until := time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC)
	acc := WakeFailureAccrualPatch(4, 5, until)
	if !acc.Quarantined {
		t.Fatal("fifth failure with max 5 must quarantine")
	}
	want := MetadataPatch{
		"wake_attempts":     "5",
		"quarantined_until": "2026-03-08T13:00:00Z",
		"sleep_reason":      "quarantine",
	}
	assertPatch(t, acc.Patch, want)
	if _, ok := acc.Patch["state"]; ok {
		t.Error("wake-failure quarantine is metadata-only and must not write state")
	}
}

func TestChurnAccrualPatch(t *testing.T) {
	until := time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC)

	acc := ChurnAccrualPatch(0, 3, until)
	if acc.Quarantined {
		t.Fatal("first churn cycle with max 3 must not quarantine")
	}
	assertPatch(t, acc.Patch, MetadataPatch{"churn_count": "1"})

	acc = ChurnAccrualPatch(2, 3, until)
	if !acc.Quarantined {
		t.Fatal("third churn cycle with max 3 must quarantine")
	}
	want := MetadataPatch{
		"churn_count":       "3",
		"quarantined_until": "2026-03-08T13:00:00Z",
		"sleep_reason":      "context-churn",
	}
	assertPatch(t, acc.Patch, want)
}

func TestConversationResetPatch(t *testing.T) {
	// Wake failures clear the config hash so the next start is a first
	// start; churn clears only the conversation binding.
	assertPatch(t, ConversationResetPatch(true), MetadataPatch{
		"session_key":                "",
		"started_config_hash":        "",
		"continuation_reset_pending": "true",
		// Priming markers share started_config_hash's lifetime (S19 Stage 2).
		"primed_at":            "",
		"priming_attempted_at": "",
		"prompt_hash":          "",
	})
	assertPatch(t, ConversationResetPatch(false), MetadataPatch{
		"session_key":                "",
		"continuation_reset_pending": "true",
	})
}

func TestRateLimitQuarantinePatch(t *testing.T) {
	until := time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC)
	assertPatch(t, RateLimitQuarantinePatch(until), MetadataPatch{
		"state":                     string(StateAsleep),
		"quarantined_until":         "2026-03-08T13:00:00Z",
		"sleep_reason":              "rate_limit",
		"last_woke_at":              "",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	})
}

func assertPatch(t *testing.T, got, want MetadataPatch) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("patch has %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("patch[%q] = %q, want %q", k, got[k], v)
		}
	}
}
