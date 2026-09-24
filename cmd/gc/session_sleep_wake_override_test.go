package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
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
