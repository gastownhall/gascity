package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// newLegacyDeliveryCheckForTest builds the check over a canned queue, poller
// count and lane-status reply.
func newLegacyDeliveryCheckForTest(cfg *config.City, state nudgequeue.State, pollers int, st conversationLaneStatus, queryErr error) *legacyDeliveryDoctorCheck {
	c := newLegacyDeliveryDoctorCheck(cfg)
	c.queue = func(string) (nudgequeue.State, error) { return state, nil }
	c.pollers = func(string) (int, error) { return pollers, nil }
	c.query = func(string) (conversationLaneStatus, error) { return st, queryErr }
	return c
}

// TestLegacyDeliveryDoctor pins the legacy-delivery check (OJ-21 21a): it
// counts the queue and live pollers, says whether the lane or legacy
// delivery serves the city, and warns, advisory, of each deprecation that
// applies where the lane runs or the default is held back.
func TestLegacyDeliveryDoctor(t *testing.T) {
	queued := nudgequeue.State{Pending: []nudgequeue.Item{{ID: "n-1"}, {ID: "n-2"}}, Dead: []nudgequeue.Item{{ID: "n-3"}}}
	lane := conversationLaneStatus{State: laneHostStateRunning, Kinds: laneDefaultKinds, KindsDefault: true, DefaultApplies: true}
	dispatcher := &config.City{}
	dispatcher.Daemon.NudgeDispatcher = "supervisor"
	rollback := &config.City{Ops: config.OpsConfig{Authoritative: config.OpsKinds()}}
	for _, tc := range []struct {
		name     string
		cfg      *config.City
		st       conversationLaneStatus
		queryErr error
		status   doctor.CheckStatus
		want     []string
	}{
		{name: "lane runs, nothing deprecated", cfg: &config.City{}, st: lane, status: doctor.StatusOK, want: []string{"the conversation lane delivers message, interrupt, respond"}},
		{name: "lane runs, dispatcher key set", cfg: dispatcher, st: lane, status: doctor.StatusWarning, want: []string{"[daemon] nudge_dispatcher is deprecated"}},
		{name: "rollback on a SQLite city", cfg: rollback, st: conversationLaneStatus{State: laneHostStateRunning, DefaultApplies: true}, status: doctor.StatusWarning, want: []string{"legacy delivery serves this city", "authoritative = [] keeps message, interrupt and respond on legacy delivery"}},
		{name: "default city keeps legacy", cfg: dispatcher, st: conversationLaneStatus{State: laneHostStateOff}, status: doctor.StatusOK, want: []string{"legacy delivery serves this city"}},
		{name: "controller not answering", cfg: &config.City{}, queryErr: errors.New("dial: no such file"), status: doctor.StatusOK, want: []string{"did not answer lane-status"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newLegacyDeliveryCheckForTest(tc.cfg, queued, 3, tc.st, tc.queryErr)
			r := c.Run(&doctor.CheckContext{CityPath: "/city"})
			if r.Status != tc.status {
				t.Fatalf("status = %v (%q), want %v", r.Status, r.Message, tc.status)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.Message, w) {
					t.Fatalf("message %q, want it to contain %q", r.Message, w)
				}
			}
			if tc.status == doctor.StatusWarning && (r.Severity != doctor.SeverityAdvisory || r.FixHint == "") {
				t.Fatalf("warning severity %v hint %q, want advisory with a remedy", r.Severity, r.FixHint)
			}
			if strings.Contains(r.FixHint, "remove the deprecated keys") {
				t.Fatalf("hint %q asks to remove keys that still govern legacy delivery in this release", r.FixHint)
			}
			if !slices.Contains(r.Details, "nudge queue: 2 pending, 0 in flight, 1 dead") || !slices.Contains(r.Details, "live nudge pollers: 3") {
				t.Fatalf("details = %q, want the queue depth and pollers", r.Details)
			}
		})
	}
	c := newLegacyDeliveryDoctorCheck(&config.City{})
	if c.Name() != "legacy-delivery" || c.CanFix() || c.WarmupEligible() {
		t.Fatalf("check = %s fix %v warmup %v", c.Name(), c.CanFix(), c.WarmupEligible())
	}
}

// TestLiveCityNudgePollersCountsNone: a city with no pollers directory has
// no live poller, and PID files naming a dead process, garbage or a live
// process that is not this city's poller count none.
func TestLiveCityNudgePollersCountsNone(t *testing.T) {
	city := t.TempDir()
	if n, err := liveCityNudgePollers(city); err != nil || n != 0 {
		t.Fatalf("no pollers dir: %d, %v; want 0", n, err)
	}
	for i, content := range []string{fmt.Sprintf("%d\n", deadPID(t)), "not-a-pid\n", fmt.Sprintf("%d\n", os.Getpid())} {
		pidPath := nudgePollerPIDPath(city, fmt.Sprintf("sess-%d", i), "session-id")
		if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(pidPath, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	if n, err := liveCityNudgePollers(city); err != nil || n != 0 {
		t.Fatalf("no live poller: %d, %v; want 0", n, err)
	}
}
