package beadstest

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testpolicy/waiverclock"
)

// TestLedgeredSkipsAreValidAndUnexpired is the guard that keeps every
// conformance opt-out honest: each entry must name a bead, carry an expiry, and
// not be parked further than maxSkipHorizon out, and the waiver clock must
// tolerate its date. A skip that outlives its fix warns through the grace
// window and then turns red here, forcing the defect to be fixed or the
// escalation to be renewed.
func TestLedgeredSkipsAreValidAndUnexpired(t *testing.T) {
	now := time.Now()
	mode, err := waiverclock.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ledgeredSkips {
		if s.Subtest == "" || s.Reason == "" || s.BeadID == "" || s.Expiry.IsZero() {
			t.Errorf("incomplete ledger entry (Subtest, Reason, BeadID, Expiry all required): %+v", s)
			continue
		}
		clock := skipClock(s, now, mode)
		for _, fatal := range clock.Fatal {
			t.Error(fatal)
		}
		for _, warning := range clock.Warnings {
			t.Log(warning)
		}
		if s.Expiry.After(now.Add(maxSkipHorizon)) {
			t.Errorf("ledger skip %q expiry %s is more than 90 days out; opt-outs must be short-lived",
				s.Subtest, s.Expiry.Format("2006-01-02"))
		}
	}
}

// TestUnledgeredSubtestHasNoSkip proves the lookup that backs requireLedgeredSkip
// returns nil for any subtest not in the ledger — the condition that makes an
// unledgered opt-out hard-fail instead of silently skipping.
func TestUnledgeredSubtestHasNoSkip(t *testing.T) {
	if got := lookupSkip("NoSuchSubtest"); got != nil {
		t.Fatalf("lookupSkip returned %+v for an unledgered subtest; want nil", got)
	}
	// Every ledger entry must be findable by its own Subtest name.
	for _, s := range ledgeredSkips {
		if lookupSkip(s.Subtest) == nil {
			t.Errorf("ledger entry %q is not findable via lookupSkip", s.Subtest)
		}
	}
}

// TestSkipClockGraceWarnsBeforeItFails proves a skip's expiry goes through the
// fleet waiver clock: quiet while far off, a warning from WarnAhead before its
// date and through the grace window after it, fatal once the grace runs out,
// and fatal on the day after expiry only in strict mode.
func TestSkipClockGraceWarnsBeforeItFails(t *testing.T) {
	skip := ConformanceSkip{
		Subtest: "Example",
		Reason:  "example",
		BeadID:  "ga-example",
		Expiry:  time.Date(2026, time.December, 15, 0, 0, 0, 0, time.UTC),
	}
	day := func(offset int) time.Time { return skip.Expiry.AddDate(0, 0, offset).Add(12 * time.Hour) }
	for _, tc := range []struct {
		name         string
		now          time.Time
		mode         waiverclock.Mode
		fatal, warns bool
	}{
		{name: "far ahead", now: day(-30), mode: waiverclock.ModeGrace},
		{name: "inside the warning window", now: day(-3), mode: waiverclock.ModeGrace, warns: true},
		{name: "expiry day is still valid", now: day(0), mode: waiverclock.ModeGrace, warns: true},
		{name: "lapsed, inside grace", now: day(1), mode: waiverclock.ModeGrace, warns: true},
		{name: "lapsed, last grace day", now: day(14), mode: waiverclock.ModeGrace, warns: true},
		{name: "lapsed, past grace", now: day(15), mode: waiverclock.ModeGrace, fatal: true},
		{name: "lapsed, strict", now: day(1), mode: waiverclock.ModeStrict, fatal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := skipClock(skip, tc.now, tc.mode)
			if got := len(clock.Fatal) > 0; got != tc.fatal {
				t.Fatalf("fatal = %v (%q), want %v", got, clock.Fatal, tc.fatal)
			}
			if got := len(clock.Warnings) > 0; got != tc.warns {
				t.Fatalf("warns = %v (%q), want %v", got, clock.Warnings, tc.warns)
			}
			for _, msg := range append(clock.Fatal, clock.Warnings...) {
				if !strings.Contains(msg, skip.BeadID) || !strings.Contains(msg, skip.Subtest) {
					t.Fatalf("clock message %q does not name the skip and its owner", msg)
				}
			}
		})
	}
}
