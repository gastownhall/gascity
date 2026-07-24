package session

import (
	"testing"
	"time"
)

func TestDecideInertRecovery(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	const (
		grace   = 60 * time.Second
		backoff = 2 * time.Minute
		maxAtt  = 3
		fp      = "stream_disconnected"
	)

	// candidate returns a facts value where the recovery condition holds
	// (alive, eligible, an inert transport failure present), so each test can
	// override only the fields it exercises.
	candidate := func() InertRecoveryFacts {
		return InertRecoveryFacts{
			Alive:            true,
			Eligible:         true,
			TransportFailure: true,
			Fingerprint:      fp,
			Now:              base,
			Grace:            grace,
			Backoff:          backoff,
			MaxAttempts:      maxAtt,
		}
	}

	tests := []struct {
		name   string
		mutate func(*InertRecoveryFacts)
		want   InertRecoveryOutcome
	}{
		{
			name:   "dead session with no marker does nothing",
			mutate: func(f *InertRecoveryFacts) { f.Alive = false },
			want:   RecoverNone,
		},
		{
			name: "dead session clears a stale marker",
			mutate: func(f *InertRecoveryFacts) {
				f.Alive = false
				f.MarkedFingerprint = fp
				f.Attempts = 1
			},
			want: RecoverClear,
		},
		{
			name:   "ineligible session does nothing",
			mutate: func(f *InertRecoveryFacts) { f.Eligible = false },
			want:   RecoverNone,
		},
		{
			name: "recovered session (no failure) clears the marker",
			mutate: func(f *InertRecoveryFacts) {
				f.TransportFailure = false
				f.Fingerprint = ""
				f.MarkedFingerprint = fp
				f.Attempts = 2
			},
			want: RecoverClear,
		},
		{
			name:   "first sighting starts the grace clock",
			mutate: func(f *InertRecoveryFacts) { f.MarkedFingerprint = "" },
			want:   RecoverObserve,
		},
		{
			name: "a different failure class re-arms",
			mutate: func(f *InertRecoveryFacts) {
				f.MarkedFingerprint = "dns_lookup_failure"
				f.Attempts = 3
				f.LastActionAt = base.Add(-time.Hour)
			},
			want: RecoverObserve,
		},
		{
			name: "inside grace, hold",
			mutate: func(f *InertRecoveryFacts) {
				f.MarkedFingerprint = fp
				f.Attempts = 0
				f.LastActionAt = base.Add(-30 * time.Second)
			},
			want: RecoverWait,
		},
		{
			name: "past grace, nudge",
			mutate: func(f *InertRecoveryFacts) {
				f.MarkedFingerprint = fp
				f.Attempts = 0
				f.LastActionAt = base.Add(-90 * time.Second)
			},
			want: RecoverNudge,
		},
		{
			name: "inside backoff, hold",
			mutate: func(f *InertRecoveryFacts) {
				f.MarkedFingerprint = fp
				f.Attempts = 1
				f.LastActionAt = base.Add(-1 * time.Minute)
			},
			want: RecoverWait,
		},
		{
			name: "past backoff, nudge again",
			mutate: func(f *InertRecoveryFacts) {
				f.MarkedFingerprint = fp
				f.Attempts = 1
				f.LastActionAt = base.Add(-3 * time.Minute)
			},
			want: RecoverNudge,
		},
		{
			name: "at max attempts, give up",
			mutate: func(f *InertRecoveryFacts) {
				f.MarkedFingerprint = fp
				f.Attempts = maxAtt
				f.LastActionAt = base.Add(-1 * time.Hour)
			},
			want: RecoverExhausted,
		},
		{
			name: "past max attempts, still exhausted (no storm)",
			mutate: func(f *InertRecoveryFacts) {
				f.MarkedFingerprint = fp
				f.Attempts = maxAtt + 5
				f.LastActionAt = base.Add(-1 * time.Hour)
			},
			want: RecoverExhausted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := candidate()
			tt.mutate(&f)
			if got := DecideInertRecovery(f); got != tt.want {
				t.Errorf("DecideInertRecovery() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInertRecoveryPatches(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	stamp := now.UTC().Format(time.RFC3339)

	observe := InertRecoveryObservePatch("stream_disconnected", now)
	if observe[InertRecoveryFingerprintKey] != "stream_disconnected" ||
		observe[InertRecoveryAttemptsKey] != "0" ||
		observe[InertRecoveryAtKey] != stamp {
		t.Fatalf("observe patch = %v", observe)
	}

	nudge := InertRecoveryNudgePatch("stream_disconnected", 2, now)
	if nudge[InertRecoveryFingerprintKey] != "stream_disconnected" ||
		nudge[InertRecoveryAttemptsKey] != "2" ||
		nudge[InertRecoveryAtKey] != stamp {
		t.Fatalf("nudge patch = %v", nudge)
	}

	clear := InertRecoveryClearPatch()
	for _, k := range []string{InertRecoveryFingerprintKey, InertRecoveryAttemptsKey, InertRecoveryAtKey} {
		if v, ok := clear[k]; !ok || v != "" {
			t.Fatalf("clear patch key %q = %q (ok=%v), want empty", k, v, ok)
		}
	}
}
