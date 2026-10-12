package main

import (
	"slices"
	"testing"
	"time"
)

// Arm A3's alerts (C4c2 review): once per episode, re-armed when the
// condition clears.

// identityAlertWorld is a World at now whose census holds row gc-2.
func identityAlertWorld(t *testing.T, now time.Time) *World {
	t.Helper()
	w, _ := rowWorld(t, sessionRow("gc-2", "template", "worker", "session_name", "s-gc-2", "state", "active"))
	w.Now = now
	return w
}

func countAlerts(t *testing.T, f *observeFixture, kind string) int {
	t.Helper()
	n := 0
	for _, a := range f.alerts(t) {
		if a == kind {
			n++
		}
	}
	return n
}

// Kills a refusal alert raised before the fifth consecutive refusal, raised
// again within one episode, or never re-armed (a landed rekey, or a pass in
// which the row decides anything but a rekey, ends the streak); and failed
// rekeys left out of the streak.
func TestRekeyRefusalsAlertOncePerEpisode(t *testing.T) {
	f := newObserveFixture(t)
	k := rowKeyOf("gc-2")
	refuse := func(n int) {
		for range n {
			f.p.observeRekeySettlement(settlement{Key: k, Kind: intentRekey, Outcome: settledRefused, Cause: causeIdentityChanged})
		}
	}
	refuse(rekeyRefusalsAlert - 1)
	if n := countAlerts(t, f, alertRekeyRefused); n != 0 {
		t.Fatalf("%d alerts after %d refusals, want none", n, rekeyRefusalsAlert-1)
	}
	refuse(3)
	if n := countAlerts(t, f, alertRekeyRefused); n != 1 {
		t.Fatalf("%d alerts in one streak, want exactly one", n)
	}

	// A failure (a write-error) counts in the streak like a refusal.
	f.p.observeRekeySettlement(settlement{Key: k, Kind: intentRekey, Outcome: settledLanded})
	for range rekeyRefusalsAlert {
		f.p.observeRekeySettlement(settlement{Key: k, Kind: intentRekey, Outcome: settledFailed, Cause: causeWrite})
	}
	if n := countAlerts(t, f, alertRekeyRefused); n != 2 {
		t.Fatalf("%d alerts after %d failed rekeys, want a second", n, rekeyRefusalsAlert)
	}

	f.p.observeRekeySettlement(settlement{Key: k, Kind: intentRekey, Outcome: settledLanded})
	refuse(rekeyRefusalsAlert)
	if n := countAlerts(t, f, alertRekeyRefused); n != 3 {
		t.Fatalf("%d alerts after a landing and a new streak, want 3 (re-armed)", n)
	}

	w := identityAlertWorld(t, gatherNow)
	f.p.observeRekeySettlement(settlement{Key: k, Kind: intentRekey, Outcome: settledLanded})
	refuse(rekeyRefusalsAlert - 1)
	f.p.observeIdentityHolds(w, map[rowKey]string{k: decideNoAction})
	refuse(rekeyRefusalsAlert - 1)
	if n := countAlerts(t, f, alertRekeyRefused); n != 3 {
		t.Fatalf("%d alerts across a pass that decided no rekey, want the streak ended", n)
	}
}

// Kills a NewerSelf alert raised at or before the dwell, raised again in one
// hold, or never re-armed: a row held NewerSelf alerts once past
// newerSelfHeldAlert, an in-flight pass (no reason) keeps the hold, and a
// pass that decides anything else ends it.
func TestNewerSelfHeldAlertsOncePastDwell(t *testing.T) {
	f := newObserveFixture(t)
	k := rowKeyOf("gc-2")
	held := map[rowKey]string{k: decideNewerSelf}
	pass := func(at time.Duration, reasons map[rowKey]string) {
		f.p.observeIdentityHolds(identityAlertWorld(t, gatherNow.Add(at)), reasons)
	}
	pass(0, held)
	pass(newerSelfHeldAlert, held)
	pass(newerSelfHeldAlert+time.Second, map[rowKey]string{}) // in flight: no reason
	if n := countAlerts(t, f, alertNewerSelfHeld); n != 0 {
		t.Fatalf("%d alerts at the dwell, want none", n)
	}
	pass(newerSelfHeldAlert+2*time.Second, held)
	pass(newerSelfHeldAlert+time.Minute, held)
	if got := f.alerts(t); countAlerts(t, f, alertNewerSelfHeld) != 1 || !slices.Contains(got, alertNewerSelfHeld) {
		t.Fatalf("alerts %v, want one newer-self-held", got)
	}

	pass(newerSelfHeldAlert+2*time.Minute, map[rowKey]string{k: decideNoAction})
	pass(newerSelfHeldAlert+3*time.Minute, held)
	pass(2*newerSelfHeldAlert+3*time.Minute, held)
	if n := countAlerts(t, f, alertNewerSelfHeld); n != 1 {
		t.Fatalf("%d alerts, want the re-armed hold still inside its dwell", n)
	}
	pass(2*newerSelfHeldAlert+4*time.Minute, held)
	if n := countAlerts(t, f, alertNewerSelfHeld); n != 2 {
		t.Fatalf("%d alerts, want a second after the re-armed hold passed its dwell", n)
	}
}

// Kills A3's alerts left unwired: refused rekeys drained through the
// planner's settlement queue count toward the streak, and a real pass in
// which the row decides no rekey ends it, so four refusals on either side
// of that pass raise nothing while five in a row alert.
func TestRekeyRefusalsAlertWiredThroughThePlanner(t *testing.T) {
	f := newObserveFixture(t, poolRow("gc-1", "worker", 1, "active"))
	refused := settlement{Key: rowKeyOf("gc-1"), Kind: intentRekey, Outcome: settledRefused, Cause: causeNameBusy}
	post := func(n int) {
		for range n {
			f.p.settlements.post(refused)
		}
	}
	post(rekeyRefusalsAlert - 1)
	f.runPass(time.Second) // drains the refusals; the row decides no rekey
	post(rekeyRefusalsAlert - 1)
	f.runPass(time.Second)
	if n := countAlerts(t, f, alertRekeyRefused); n != 0 {
		t.Fatalf("%d alerts across a pass that decided no rekey, want none", n)
	}
	post(rekeyRefusalsAlert)
	f.p.drainSettlements(f.clk.Now())
	if n := countAlerts(t, f, alertRekeyRefused); n != 1 {
		t.Fatalf("%d alerts after %d drained refusals, want one", n, rekeyRefusalsAlert)
	}
}
