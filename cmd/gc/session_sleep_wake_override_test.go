package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestReconcilerWakeDemandOverridesSleepSuppressionForExplicitAndInteractiveRoutedDemand(t *testing.T) {
	interactive := resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveFresh}
	eval := wakeEvaluation{}

	routed := AwakeDecision{ShouldWake: true, Reason: "routed-demand"}
	if !wakeDemandOverridesSleepSuppression(routed, eval, interactive, nil, "olivia", false, false) {
		t.Fatal("routed demand must override interactive sleep suppression: the holder owns the canonical alias and no standby can serve it")
	}
	if wakeDemandOverridesSleepSuppression(routed, eval, interactive, nil, "olivia", true, false) {
		t.Fatal("explicit sleep intent should still override routed demand")
	}

	explicit := AwakeDecision{ShouldWake: true, Reason: "explicit-wake"}
	if !wakeDemandOverridesSleepSuppression(explicit, eval, interactive, nil, "olivia", false, true) {
		t.Fatal("a durable explicit wake request must override the idle latch")
	}
	// The awake set may re-label an explicitly woken named holder (e.g. to
	// "named-demand"); the durable wake_request still carries the override.
	relabeled := AwakeDecision{ShouldWake: true, Reason: "named-demand"}
	if !wakeDemandOverridesSleepSuppression(relabeled, eval, interactive, nil, "olivia", false, true) {
		t.Fatal("explicit wake must override regardless of the awake-set reason label")
	}
	if wakeDemandOverridesSleepSuppression(explicit, eval, interactive, nil, "olivia", true, true) {
		t.Fatal("explicit sleep intent should still win over an explicit wake")
	}

	scaled := AwakeDecision{ShouldWake: true, Reason: "scaled:demand"}
	if wakeDemandOverridesSleepSuppression(scaled, eval, interactive, map[string]int{"olivia": 1}, "olivia", false, false) {
		t.Fatal("ordinary interactive pool demand should still honor sleep suppression")
	}
}

// explicitWakePendingInfo bounds the explicit-wake override: a wake request is
// served once the session is up, or once it has slept since the request.
func TestExplicitWakePendingInfo(t *testing.T) {
	const (
		before = "2026-09-22T20:00:00Z"
		after  = "2026-09-22T22:00:00Z"
	)
	for _, tc := range []struct {
		name string
		info sessionpkg.Info
		want bool
	}{
		{name: "no request", info: sessionpkg.Info{MetadataState: "asleep"}, want: false},
		{name: "non-explicit request", info: sessionpkg.Info{MetadataState: "asleep", WakeRequest: "work"}, want: false},
		{name: "asleep, requested after sleep", info: sessionpkg.Info{MetadataState: "asleep", WakeRequest: "explicit", WakeRequestedAt: after, SleptAt: before}, want: true},
		{name: "asleep, requested before sleep", info: sessionpkg.Info{MetadataState: "asleep", WakeRequest: "explicit", WakeRequestedAt: before, SleptAt: after}, want: false},
		{name: "asleep, requested same second as sleep", info: sessionpkg.Info{MetadataState: "asleep", WakeRequest: "explicit", WakeRequestedAt: after, SleptAt: after}, want: true},
		{name: "asleep, no timestamps", info: sessionpkg.Info{MetadataState: "asleep", WakeRequest: "explicit"}, want: true},
		{name: "running active", info: sessionpkg.Info{MetadataState: "active", WakeRequest: "explicit", WakeRequestedAt: after, SleptAt: before}, want: false},
		{name: "running awake", info: sessionpkg.Info{MetadataState: "awake", WakeRequest: " explicit ", WakeRequestedAt: after}, want: false},
		{name: "drained", info: sessionpkg.Info{MetadataState: "drained", WakeRequest: "explicit", WakeRequestedAt: after, SleptAt: before}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := explicitWakePendingInfo(tc.info); got != tc.want {
				t.Fatalf("explicitWakePendingInfo(%+v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}
