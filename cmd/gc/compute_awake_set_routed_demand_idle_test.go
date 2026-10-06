package main

import (
	"testing"
	"time"
)

func TestPR4644_RoutedDemandWakesAsleepNamedHolder(t *testing.T) {
	const (
		template    = "gascity/reviewer"
		identity    = "gascity/reviewer"
		sessionName = "gascity--reviewer"
	)

	build := func(idleSince time.Time) AwakeInput {
		return AwakeInput{
			Agents: []AwakeAgent{{
				QualifiedName:  template,
				SleepAfterIdle: 10 * time.Minute, // idle_timeout = "10m"
			}},
			NamedSessions: []AwakeNamedSession{{
				Identity:    identity,
				Template:    template,
				Mode:        "on_demand",
				RuntimeName: sessionName,
			}},
			SessionBeads: []AwakeSessionBead{{
				ID:                     "gm-gjmwz2",
				SessionName:            sessionName,
				Template:               template,
				State:                  "asleep",
				SleepReason:            "idle-timeout",
				NamedIdentity:          identity,
				ConfiguredNamedSession: true,
				IdleSince:              idleSince,
			}},
			ScaleCheckCounts:         map[string]int{template: 1},
			NamedSessionRoutedDemand: map[string]bool{identity: true},
			Now:                      time.Now().UTC(),
		}
	}

	t.Run("A_no_idle_reference", func(t *testing.T) {
		d := ComputeAwakeSet(build(time.Time{}))[sessionName]
		if !d.ShouldWake {
			t.Fatalf("want wake, got ShouldWake=false reason=%q", d.Reason)
		}
		if d.Reason != "routed-demand" {
			t.Fatalf("want reason routed-demand, got %q", d.Reason)
		}
	})

	t.Run("B_live_shape_stale_idle_reference", func(t *testing.T) {
		d := ComputeAwakeSet(build(time.Now().UTC().Add(-56 * 24 * time.Hour)))[sessionName]
		if !d.ShouldWake {
			t.Fatalf("routed-demand wake was canceled by idle-sleep (final reason=%q); "+
				"add \"routed-demand\" to the idle-sleep exemption list", d.Reason)
		}
	})
}
