package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// newLaneKindsSQLiteStore is a SQLite sessions store, the topology an unset
// [ops] authoritative runs the lane on.
func newLaneKindsSQLiteStore(t *testing.T) beads.Store {
	t.Helper()
	st, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = st.(interface{ CloseStore() error }).CloseStore() })
	return st
}

// TestEffectiveLaneKindsDefaultsOnSQLiteOnly pins OJ-21 21a's default: an
// unset [ops] authoritative runs message, interrupt and respond on the lane
// over a SQLite sessions store (cached or not), and none over native Dolt,
// the file provider, bd or an exec provider; an explicit list, [] included,
// wins everywhere.
func TestEffectiveLaneKindsDefaultsOnSQLiteOnly(t *testing.T) {
	sqlite := newLaneKindsSQLiteStore(t)
	file, err := beads.OpenFileStore(fsys.NewFake(), filepath.Join("/city", "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	stores := []struct {
		name     string
		store    beads.Store
		defaults bool
		fit      laneStoreFit
	}{
		{"sqlite", sqlite, true, laneStoreFitDefault},
		{"cached sqlite", beads.NewCachingStoreForTest(sqlite, nil), true, laneStoreFitDefault},
		{"native dolt", beads.NewCachingStoreForTest(&beads.NativeDoltStore{}, nil), false, laneStoreFitListed},
		{"file", file, false, laneStoreFitListed},
		{"bd", &beads.BdStore{}, false, laneStoreFitNone},
		{"exec", beadsexec.NewStore("/bin/false"), false, laneStoreFitNone},
		{"no store", nil, false, laneStoreFitNone},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			var sessions beads.SessionStore
			if tc.store != nil {
				sessions = sessionFrontDoor(tc.store).Store()
			}
			if got := laneStoreFitFor(sessions); got != tc.fit {
				t.Fatalf("laneStoreFitFor = %v, want %v", got, tc.fit)
			}
			kinds, defaulted := cliLaneKinds(&config.City{}, tc.store)
			if tc.defaults {
				if !defaulted || !slices.Equal(kinds, []string{"message", "interrupt", "respond"}) {
					t.Fatalf("unset: kinds %v, defaulted %v; want all three by default", kinds, defaulted)
				}
			} else if defaulted || len(kinds) != 0 {
				t.Fatalf("unset: kinds %v, defaulted %v; want none (legacy delivery)", kinds, defaulted)
			}
			for _, explicit := range [][]string{{}, {"message"}} {
				cfg := &config.City{Ops: config.OpsConfig{Authoritative: config.OpsKinds(explicit...)}}
				kinds, defaulted := cliLaneKinds(cfg, tc.store)
				if defaulted || !slices.Equal(kinds, explicit) {
					t.Fatalf("explicit %v: kinds %v, defaulted %v; want the list as written", explicit, kinds, defaulted)
				}
			}
		})
	}
	if kinds, defaulted := cliLaneKinds(nil, sqlite); defaulted || kinds != nil {
		t.Fatalf("nil config: kinds %v, defaulted %v; want none", kinds, defaulted)
	}
}

// TestLegacyDeliveryWarnings pins where the deprecation warnings appear: a
// legacy-delivery key only where the lane runs the kinds it governs, and an
// explicit [] only where the default would have run the lane.
func TestLegacyDeliveryWarnings(t *testing.T) {
	keys := func() *config.City {
		cfg := &config.City{}
		cfg.Daemon.NudgeDispatcher = "supervisor"
		cfg.Session.NudgePollInterval = "2s"
		cfg.Ops.ContentMode = "auto"
		cfg.Ops.ControlMode = "auto"
		return cfg
	}
	if got := laneLegacyDeliveryWarnings(keys(), nil, false); len(got) != 0 {
		t.Fatalf("no lane: warnings %q, want none", got)
	}
	all := laneLegacyDeliveryWarnings(keys(), laneDefaultKinds, true)
	for _, key := range []string{"[daemon] nudge_dispatcher", "[session] nudge_poll_interval", "[ops] content", "[ops] control"} {
		if !slices.ContainsFunc(all, func(w string) bool { return strings.HasPrefix(w, key+" is deprecated") }) {
			t.Fatalf("lane on: warnings %q, want one for %s", all, key)
		}
	}
	// The keys still govern the legacy deliverers in this release (queue
	// leftovers, sessions without a row); removing [daemon] nudge_dispatcher
	// would re-enable the per-session pollers. No warning may ask for removal.
	// Legacy delivery is removed one release after gc init splits storage by
	// default (D-NOLANE), so no warning may promise the next release.
	for _, w := range all {
		if strings.Contains(w, "remove it") || !strings.Contains(w, "leave it set until then") {
			t.Fatalf("lane on: warning %q asks for removal; want it to say to leave the key set until legacy delivery is removed", w)
		}
		if strings.Contains(w, "next release") || !strings.Contains(w, "a future release") {
			t.Fatalf("lane on: warning %q, want legacy delivery's removal in a future release", w)
		}
	}
	interruptOnly := laneLegacyDeliveryWarnings(keys(), []string{"interrupt"}, false)
	if len(interruptOnly) != 1 || !strings.HasPrefix(interruptOnly[0], "[ops] control") {
		t.Fatalf("interrupt only: warnings %q, want only [ops] control's", interruptOnly)
	}
	rollback := &config.City{Ops: config.OpsConfig{Authoritative: config.OpsKinds()}}
	if got := laneLegacyDeliveryWarnings(rollback, nil, true); len(got) != 1 || !strings.Contains(got[0], "authoritative = []") || strings.Contains(got[0], "next") {
		t.Fatalf("[] on a capable city: warnings %q, want the rollback warning, promising no next-release removal", got)
	}
	if got := laneLegacyDeliveryWarnings(rollback, nil, false); len(got) != 0 {
		t.Fatalf("[] on an incapable city: warnings %q, want none", got)
	}
}

// TestSessionNudgeDefaultsToTheLaneOnSQLite: with [ops] authoritative unset,
// gc session nudge to a session in a SQLite sessions store is a lane op,
// delivered by the inline pass, with no queue item.
func TestSessionNudgeDefaultsToTheLaneOnSQLite(t *testing.T) {
	c := newCLILaneCityOver(t, newLaneKindsSQLiteStore(t))
	c.cfg.Ops.Authoritative = nil
	code, _, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
	if code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	op := c.only(t)
	if op.Executor != opjournal.ExecutorLane || op.Payload != cliLaneTestMessage {
		t.Fatalf("op = %+v, want the lane op", op)
	}
	if n := len(c.nudges()); n != 1 || c.queuedItems(t) != 0 {
		t.Fatalf("%d deliveries, %d queued; want the inline delivery and no queue item", n, c.queuedItems(t))
	}
}

// TestSessionNudgeStaysLegacyOffSQLiteByDefault: with [ops] authoritative
// unset, a sessions store the default does not cover (here the in-memory
// test store; bd, native Dolt, file and exec stores alike,
// TestEffectiveLaneKindsDefaultsOnSQLiteOnly) keeps gc session nudge on its
// legacy path: no lane op.
func TestSessionNudgeStaysLegacyOffSQLiteByDefault(t *testing.T) {
	c := newCLILaneCity(t)
	c.cfg.Ops.Authoritative = nil
	prevPoller := startNudgePoller
	startNudgePoller = func(_, _, _ string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prevPoller })
	if code, _, stderr := c.nudge(nudgeDeliveryQueue, "", false, cliLaneTestMessage); code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	for _, op := range c.ops(t) {
		if op.Executor == opjournal.ExecutorLane {
			t.Fatalf("lane op %+v off SQLite with the list unset", op)
		}
	}
	if c.queuedItems(t) != 1 {
		t.Fatalf("queued = %d, want the legacy queue item", c.queuedItems(t))
	}
}

// TestMailNotifyDefaultsToTheLaneOnSQLite: with [ops] authoritative unset,
// a mail notice to a session in a SQLite sessions store is a lane notice.
func TestMailNotifyDefaultsToTheLaneOnSQLite(t *testing.T) {
	c := newCLILaneCityOver(t, newLaneKindsSQLiteStore(t))
	c.cfg.Ops.Authoritative = nil
	if _, err := sendMailNotifyWithOp(c.target, c.store, c.sp, "alice", "msg-1"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	c.noticeBy(t, mailNoticePrincipal, "msg-1")
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}
