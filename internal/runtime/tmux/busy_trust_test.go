package tmux

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// familyStaticPane is a tmux executor for an agent pane of provider family
// (its GC_PROVIDER; none when family is empty) whose capture is always screen.
type familyStaticPane struct {
	family string
	screen string
}

func (f *familyStaticPane) execute(args []string) (string, error) {
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	switch tmuxSubcommand(args) {
	case "show-environment":
		if f.family != "" && len(args) > 0 && args[len(args)-1] == "GC_PROVIDER" {
			return "GC_PROVIDER=" + f.family, nil
		}
		return "", errors.New("unknown variable")
	case "capture-pane":
		return f.screen, nil
	}
	return "", nil
}

func (f *familyStaticPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

const (
	busyTrustBusyPane    = "✻ Thinking… (12s · esc to interrupt)"
	busyTrustIdlePane    = "● Done.\n\n❯ \n  ? for shortcuts"
	busyTrustNeitherPane = "Broadcast message from root@host: DISK ALERT"
)

// The busy reading of the pane scan is policy the leaf owns: only the
// families whose indicator the scan reads (submitVerifyEligibleFamilies:
// claude, codex) may report it. Every other family, and a pane whose family
// cannot be told, reports the busy-indicator string as
// runtime.ErrIdleIndeterminate, so a caller that holds on busy holds nothing
// there without knowing which families are trusted. The idle reading and the
// indeterminate one are untouched for every family.
func TestSnapshotIdleUntrustedFamilyIsIndeterminate(t *testing.T) {
	for _, tc := range []struct {
		name              string
		family            string
		screen            string
		wantIdle          bool
		wantIndeterminate bool
	}{
		{name: "claude busy", family: "claude", screen: busyTrustBusyPane},
		{name: "codex busy", family: "codex", screen: busyTrustBusyPane},
		{name: "gemini busy", family: "gemini", screen: busyTrustBusyPane, wantIndeterminate: true},
		{name: "opencode busy", family: "opencode", screen: busyTrustBusyPane, wantIndeterminate: true},
		{name: "unidentified pane busy", screen: busyTrustBusyPane, wantIndeterminate: true},
		{name: "gemini idle", family: "gemini", screen: busyTrustIdlePane, wantIdle: true},
		{name: "claude idle", family: "claude", screen: busyTrustIdlePane, wantIdle: true},
		{name: "gemini neither", family: "gemini", screen: busyTrustNeitherPane, wantIndeterminate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = &familyStaticPane{family: tc.family, screen: tc.screen}

			idle, err := tm.SnapshotIdle("lab")

			if idle != tc.wantIdle {
				t.Errorf("idle = %t, want %t", idle, tc.wantIdle)
			}
			if got := errors.Is(err, runtime.ErrIdleIndeterminate); got != tc.wantIndeterminate {
				t.Errorf("errors.Is(err, ErrIdleIndeterminate) = %t, want %t (err = %v)", got, tc.wantIndeterminate, err)
			}
			if !tc.wantIndeterminate && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

// TurnRunning answers only for a family whose turn the leaf can read. For
// every other family it reports runtime.ErrIdleIndeterminate, whatever the
// screen shows: neither a busy-indicator string nor its absence says whether
// such a family's turn runs.
func TestTurnRunningUntrustedFamilyIsIndeterminate(t *testing.T) {
	for _, tc := range []struct {
		name              string
		family            string
		screen            string
		wantRunning       bool
		wantIndeterminate bool
	}{
		{name: "claude busy", family: "claude", screen: busyTrustBusyPane, wantRunning: true},
		{name: "claude idle", family: "claude", screen: busyTrustIdlePane},
		{name: "gemini busy", family: "gemini", screen: busyTrustBusyPane, wantIndeterminate: true},
		{name: "gemini idle", family: "gemini", screen: busyTrustIdlePane, wantIndeterminate: true},
		{name: "unidentified pane busy", screen: busyTrustBusyPane, wantIndeterminate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = &familyStaticPane{family: tc.family, screen: tc.screen}

			running, err := (&Provider{tm: tm}).TurnRunning("lab")

			if running != tc.wantRunning {
				t.Errorf("running = %t, want %t", running, tc.wantRunning)
			}
			if got := errors.Is(err, runtime.ErrIdleIndeterminate); got != tc.wantIndeterminate {
				t.Errorf("errors.Is(err, ErrIdleIndeterminate) = %t, want %t (err = %v)", got, tc.wantIndeterminate, err)
			}
			if !tc.wantIndeterminate && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}
