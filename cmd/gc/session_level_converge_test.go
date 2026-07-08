package main

import (
	"reflect"
	"testing"
	"time"
)

func TestDeriveFirstStart(t *testing.T) {
	tests := []struct {
		name       string
		hash       string
		transcript sessTranscriptState
		want       bool
	}{
		{"no hash, unknown transcript -> first start", "", sessTranscriptUnknown, true},
		{"no hash, present transcript -> first start", "", sessTranscriptPresent, true},
		{"no hash, absent transcript -> first start", "", sessTranscriptAbsent, true},
		{"whitespace hash NOT trimmed in stage 1 -> resume", "   ", sessTranscriptUnknown, false},
		{"hash present, unknown transcript -> resume (legacy behavior)", "abc123", sessTranscriptUnknown, false},
		{"hash present, present transcript -> resume", "abc123", sessTranscriptPresent, false},
		{"hash present, absent transcript -> first start (#3849 fix, inert until probe)", "abc123", sessTranscriptAbsent, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveFirstStart(tt.hash, tt.transcript); got != tt.want {
				t.Errorf("deriveFirstStart(%q, %v) = %v, want %v", tt.hash, tt.transcript, got, tt.want)
			}
		})
	}
}

// TestDeriveFirstStartMatchesLegacyWhenUnprobed is the permanent legacy-parity
// pin (spec §7.2): with sessTranscriptUnknown, the result MUST equal the legacy
// `started_config_hash == ""` expression for every hash, byte-for-byte, with no
// TrimSpace. The trim variant is adopted with its own test at Stage 4.
func TestDeriveFirstStartMatchesLegacyWhenUnprobed(t *testing.T) {
	for _, hash := range []string{"", "x", "core-hash-42", " ", "\t"} {
		legacy := hash == ""
		got := deriveFirstStart(hash, sessTranscriptUnknown)
		if got != legacy {
			t.Errorf("deriveFirstStart(%q, unknown) = %v, legacy (== \"\") = %v", hash, got, legacy)
		}
	}
}

func TestAttemptEligible(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		d    durableFacts
		want bool
	}{
		{
			name: "never attempted -> eligible",
			d:    durableFacts{now: now},
			want: true,
		},
		{
			name: "attempted just now, same prompt -> not eligible (backoff)",
			d:    durableFacts{now: now, primingAttemptedAt: now, primedPromptHash: "h", currentPromptHash: "h"},
			want: false,
		},
		{
			name: "attempted beyond interval -> eligible",
			d:    durableFacts{now: now, primingAttemptedAt: now.Add(-primeReattemptInterval), primedPromptHash: "h", currentPromptHash: "h"},
			want: true,
		},
		{
			name: "attempted within interval but prompt changed -> eligible",
			d:    durableFacts{now: now, primingAttemptedAt: now.Add(-time.Second), primedPromptHash: "old", currentPromptHash: "new"},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := attemptEligible(tt.d); got != tt.want {
				t.Errorf("attemptEligible(%+v) = %v, want %v", tt.d, got, tt.want)
			}
		})
	}
}

func TestDeriveConvergeActions(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	observedLive := runtimeFacts{observed: true, live: true}

	tests := []struct {
		name    string
		durable durableFacts
		runtime runtimeFacts
		want    []sessConvergeAction
	}{
		{
			name:    "absent durable + observed-live runtime -> rollback only",
			durable: durableFacts{absent: true, canonicalIdentity: "", promptConfigured: true, now: now},
			runtime: observedLive,
			want:    []sessConvergeAction{actionRollbackRuntimeToAbsent},
		},
		{
			name:    "absent durable + observed-dead runtime -> nothing",
			durable: durableFacts{absent: true, now: now},
			runtime: runtimeFacts{observed: true, live: false},
			want:    nil,
		},
		{
			name:    "absent durable + UNOBSERVED runtime -> nothing (C5)",
			durable: durableFacts{absent: true, now: now},
			runtime: runtimeFacts{observed: false, live: true},
			want:    nil,
		},
		{
			name:    "missing canonical identity -> stamp identity (durable-only, unobserved ok)",
			durable: durableFacts{canonicalIdentity: "", now: now},
			runtime: runtimeFacts{observed: false},
			want:    []sessConvergeAction{actionStampCanonicalIdentity},
		},
		{
			name:    "whitespace canonical identity treated as missing",
			durable: durableFacts{canonicalIdentity: "  ", now: now},
			runtime: runtimeFacts{observed: false},
			want:    []sessConvergeAction{actionStampCanonicalIdentity},
		},
		{
			name:    "identity present, nothing else -> no actions",
			durable: durableFacts{canonicalIdentity: "worker-1", now: now},
			runtime: observedLive,
			want:    nil,
		},
		{
			name:    "observed-live, prompt configured, never primed/attempted -> attempt prime",
			durable: durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "", now: now},
			runtime: observedLive,
			want:    []sessConvergeAction{actionAttemptPrime},
		},
		{
			name:    "observed-live, primedEnv set but no durable confirmation -> stamp from runtime (no nudge)",
			durable: durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "", now: now},
			runtime: runtimeFacts{observed: true, live: true, primedEnv: true},
			want:    []sessConvergeAction{actionStampPrimedFromRuntime},
		},
		{
			name:    "observed-live, attempted within interval, same prompt -> no re-attempt (bounded)",
			durable: durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "", primingAttemptedAt: now, primedPromptHash: "h", currentPromptHash: "h", now: now},
			runtime: observedLive,
			want:    nil,
		},
		{
			name:    "observed-live, attempt stale beyond interval -> re-attempt",
			durable: durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "", primingAttemptedAt: now.Add(-primeReattemptInterval), primedPromptHash: "h", currentPromptHash: "h", now: now},
			runtime: observedLive,
			want:    []sessConvergeAction{actionAttemptPrime},
		},
		{
			name:    "observed-live, prompt hash changed under attempt -> re-attempt",
			durable: durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "", primingAttemptedAt: now, primedPromptHash: "old", currentPromptHash: "new", now: now},
			runtime: observedLive,
			want:    []sessConvergeAction{actionAttemptPrime},
		},
		{
			name:    "observed-live, already durably confirmed -> no priming",
			durable: durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "2026-07-07T00:00:00Z", now: now},
			runtime: observedLive,
			want:    nil,
		},
		{
			name:    "not live -> no priming even if unprimed",
			durable: durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "", now: now},
			runtime: runtimeFacts{observed: true, live: false},
			want:    nil,
		},
		{
			name:    "unobserved runtime -> no priming even if durable says prompt configured (C5)",
			durable: durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "", now: now},
			runtime: runtimeFacts{observed: false, live: true},
			want:    nil,
		},
		{
			name:    "observed-live but no prompt configured -> no priming (P5)",
			durable: durableFacts{canonicalIdentity: "worker-1", promptConfigured: false, primedAt: "", now: now},
			runtime: observedLive,
			want:    nil,
		},
		{
			name:    "missing identity AND unprimed -> identity then priming order",
			durable: durableFacts{canonicalIdentity: "", promptConfigured: true, primedAt: "", now: now},
			runtime: observedLive,
			want:    []sessConvergeAction{actionStampCanonicalIdentity, actionAttemptPrime},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveConvergeActions(tt.durable, tt.runtime)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("deriveConvergeActions() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDeriveConvergeActionsIdempotent asserts C2: applying an action's durable
// effect to the facts and re-deriving yields the empty list. This encodes the
// "converged session emits nothing" invariant for each action.
func TestDeriveConvergeActionsIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	observedLive := runtimeFacts{observed: true, live: true}

	t.Run("identity stamp then re-derive", func(t *testing.T) {
		d := durableFacts{canonicalIdentity: "", now: now}
		r := runtimeFacts{observed: false}
		if got := deriveConvergeActions(d, r); !reflect.DeepEqual(got, []sessConvergeAction{actionStampCanonicalIdentity}) {
			t.Fatalf("pre-heal = %v", got)
		}
		d.canonicalIdentity = "worker-1" // action landed
		if got := deriveConvergeActions(d, r); got != nil {
			t.Errorf("post-heal re-derive = %v, want nil", got)
		}
	})

	t.Run("attempt prime confirmed then re-derive", func(t *testing.T) {
		d := durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "", now: now}
		if got := deriveConvergeActions(d, observedLive); !reflect.DeepEqual(got, []sessConvergeAction{actionAttemptPrime}) {
			t.Fatalf("pre-prime = %v", got)
		}
		d.primedAt = now.Format(time.RFC3339) // confirmation landed
		if got := deriveConvergeActions(d, observedLive); got != nil {
			t.Errorf("post-confirm re-derive = %v, want nil", got)
		}
	})

	t.Run("attempt stamped but not confirmed -> bounded (no re-attempt within interval)", func(t *testing.T) {
		d := durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "", now: now}
		if got := deriveConvergeActions(d, observedLive); !reflect.DeepEqual(got, []sessConvergeAction{actionAttemptPrime}) {
			t.Fatalf("pre-prime = %v", got)
		}
		// Crash after write-ahead attempt stamp, before confirmation.
		d.primingAttemptedAt = now
		d.primedPromptHash = d.currentPromptHash
		if got := deriveConvergeActions(d, observedLive); got != nil {
			t.Errorf("re-derive within interval = %v, want nil (bounded)", got)
		}
		// After the interval elapses, exactly one re-attempt is emitted.
		d.now = now.Add(primeReattemptInterval)
		if got := deriveConvergeActions(d, observedLive); !reflect.DeepEqual(got, []sessConvergeAction{actionAttemptPrime}) {
			t.Errorf("re-derive after interval = %v, want re-attempt", got)
		}
	})

	t.Run("stamp from runtime confirmed then re-derive", func(t *testing.T) {
		d := durableFacts{canonicalIdentity: "worker-1", promptConfigured: true, primedAt: "", now: now}
		r := runtimeFacts{observed: true, live: true, primedEnv: true}
		if got := deriveConvergeActions(d, r); !reflect.DeepEqual(got, []sessConvergeAction{actionStampPrimedFromRuntime}) {
			t.Fatalf("pre-stamp = %v", got)
		}
		d.primedAt = now.Format(time.RFC3339) // confirmation landed
		if got := deriveConvergeActions(d, r); got != nil {
			t.Errorf("post-stamp re-derive = %v, want nil", got)
		}
	})

	t.Run("rollback torn down then re-derive", func(t *testing.T) {
		d := durableFacts{absent: true, now: now}
		if got := deriveConvergeActions(d, runtimeFacts{observed: true, live: true}); !reflect.DeepEqual(got, []sessConvergeAction{actionRollbackRuntimeToAbsent}) {
			t.Fatalf("pre-rollback = %v", got)
		}
		// Runtime torn down: no longer live.
		if got := deriveConvergeActions(d, runtimeFacts{observed: true, live: false}); got != nil {
			t.Errorf("post-rollback re-derive = %v, want nil", got)
		}
	})
}
