package orders

import (
	"fmt"
	"testing"
	"time"
)

var streakControllerStart = time.Date(2030, 8, 21, 9, 0, 0, 0, time.UTC)

// pastGrace is a time well after the startup grace, so failures count.
func pastGrace() time.Time {
	return streakControllerStart.Add(StartupFailureGrace + time.Minute)
}

func TestFailureStreaksAlertsOnceAtThresholdThenHourly(t *testing.T) {
	s := NewFailureStreaks(streakControllerStart)
	const name = "gate-sweep"
	first := pastGrace()

	for i := 1; i < FailureAlertAfter; i++ {
		if a, ok := s.ObserveFailure(name, fmt.Sprintf("fail %d", i), first.Add(time.Duration(i-1)*30*time.Second)); ok {
			t.Fatalf("failure %d alerted early: %+v", i, a)
		}
	}
	third := first.Add(time.Minute)
	a, ok := s.ObserveFailure(name, "fail 3", third)
	if !ok {
		t.Fatalf("failure %d did not alert", FailureAlertAfter)
	}
	want := OrderFailingAlert{OrderName: name, Consecutive: 3, FirstFailed: first, FailingFor: time.Minute, LastError: "fail 3"}
	if a != want {
		t.Fatalf("alert = %+v, want %+v", a, want)
	}

	if a, ok := s.ObserveFailure(name, "fail 4", third.Add(FailureAlertRepeat-time.Second)); ok {
		t.Fatalf("a 4th failure inside the repeat window alerted again: %+v", a)
	}
	a, ok = s.ObserveFailure(name, "fail 5", third.Add(FailureAlertRepeat))
	if !ok {
		t.Fatal("a failure a full repeat window after the alert did not alert again")
	}
	if a.Consecutive != 5 || a.FirstFailed != first || a.LastError != "fail 5" {
		t.Errorf("repeat alert = %+v, want consecutive 5 anchored at %s with the latest error", a, first)
	}
}

// Failures right after a controller start are the #3898 spurious burst: they
// are skipped, neither counted nor resetting a streak.
func TestFailureStreaksSkipFailuresInsideTheStartupGrace(t *testing.T) {
	s := NewFailureStreaks(streakControllerStart)
	const name = "gate-sweep"
	for i := 0; i < 5; i++ {
		at := streakControllerStart.Add(time.Duration(i) * time.Minute)
		if a, ok := s.ObserveFailure(name, "spurious", at); ok {
			t.Fatalf("failure inside the startup grace alerted: %+v", a)
		}
	}
	first := pastGrace()
	for i := 0; i < FailureAlertAfter-1; i++ {
		if a, ok := s.ObserveFailure(name, "real", first.Add(time.Duration(i)*time.Minute)); ok {
			t.Fatalf("grace-window failures were counted toward the streak: %+v", a)
		}
	}
	a, ok := s.ObserveFailure(name, "real", first.Add(time.Duration(FailureAlertAfter-1)*time.Minute))
	if !ok || a.Consecutive != FailureAlertAfter || a.FirstFailed != first {
		t.Fatalf("alert = %+v (ok=%v), want consecutive %d anchored at the first post-grace failure", a, ok, FailureAlertAfter)
	}
}

func TestFailureStreaksAreIndependentPerOrder(t *testing.T) {
	s := NewFailureStreaks(streakControllerStart)
	at := pastGrace()
	for i := 0; i < FailureAlertAfter-1; i++ {
		s.ObserveFailure("a", "x", at)
		s.ObserveFailure("b", "x", at)
	}
	if _, ok := s.ObserveFailure("a", "x", at); !ok {
		t.Fatal("order a did not alert at its own threshold")
	}
	if a, ok := s.ObserveFailure("c", "x", at); ok {
		t.Fatalf("order c alerted on its first failure: %+v", a)
	}
}

// The thresholds are operator-facing contract: gc doctor's
// order-outcome check reports at the same count and grace.
func TestFailureStreakConstantsMatchTheirDocumentedContract(t *testing.T) {
	if FailureAlertAfter != 3 {
		t.Errorf("FailureAlertAfter = %d, want 3", FailureAlertAfter)
	}
	if StartupFailureGrace != 10*time.Minute {
		t.Errorf("StartupFailureGrace = %s, want 10m", StartupFailureGrace)
	}
	if FailureAlertRepeat != time.Hour {
		t.Errorf("FailureAlertRepeat = %s, want 1h", FailureAlertRepeat)
	}
}

func TestFailureStreaksRecoverOnlyAnAlertedStreakThenRestartAtOne(t *testing.T) {
	s := NewFailureStreaks(streakControllerStart)
	const name = "gate-sweep"
	first := pastGrace()
	for i := 0; i < FailureAlertAfter; i++ {
		s.ObserveFailure(name, "x", first.Add(time.Duration(i)*time.Minute))
	}
	recoveredAt := first.Add(time.Hour)
	r, ok := s.ObserveSuccess(name, recoveredAt)
	if !ok {
		t.Fatal("success after an alerted streak did not report a recovery")
	}
	want := OrderRecovery{OrderName: name, Consecutive: FailureAlertAfter, FirstFailed: first, FailedFor: time.Hour}
	if r != want {
		t.Fatalf("recovery = %+v, want %+v", r, want)
	}
	if r, ok := s.ObserveSuccess(name, recoveredAt.Add(time.Minute)); ok {
		t.Fatalf("a second success reported another recovery: %+v", r)
	}

	restart := recoveredAt.Add(time.Hour)
	for i := 0; i < FailureAlertAfter-1; i++ {
		if a, ok := s.ObserveFailure(name, "y", restart.Add(time.Duration(i)*time.Minute)); ok {
			t.Fatalf("the streak did not restart at 1 after recovery: %+v", a)
		}
	}
	a, ok := s.ObserveFailure(name, "y", restart.Add(time.Duration(FailureAlertAfter-1)*time.Minute))
	if !ok || a.Consecutive != FailureAlertAfter || a.FirstFailed != restart {
		t.Fatalf("new streak alert = %+v (ok=%v), want consecutive %d anchored at %s", a, ok, FailureAlertAfter, restart)
	}
}

// A success that ends a streak which never alerted announces nothing, but
// still resets the count.
func TestFailureStreaksSuccessResetsAnUnalertedStreakSilently(t *testing.T) {
	s := NewFailureStreaks(streakControllerStart)
	const name = "gate-sweep"
	at := pastGrace()
	for i := 0; i < FailureAlertAfter-1; i++ {
		s.ObserveFailure(name, "x", at)
	}
	if r, ok := s.ObserveSuccess(name, at); ok {
		t.Fatalf("recovery reported for a streak that never alerted: %+v", r)
	}
	for i := 0; i < FailureAlertAfter-1; i++ {
		if a, ok := s.ObserveFailure(name, "x", at); ok {
			t.Fatalf("failures before the success were still counted: %+v", a)
		}
	}
}

// A dispatcher rebuild keeps the table and prunes it to the orders still
// configured: a kept order's streak continues from its first failure, and a
// removed one's is dropped, so the order would start a new streak at 1.
func TestFailureStreaksPruneDropsOnlyTheRejectedOrders(t *testing.T) {
	s := NewFailureStreaks(streakControllerStart)
	at := pastGrace()
	for i := 0; i < FailureAlertAfter-1; i++ {
		s.ObserveFailure("kept", "x", at)
		s.ObserveFailure("removed", "x", at)
	}
	s.Prune(func(name string) bool { return name == "kept" })

	a, ok := s.ObserveFailure("kept", "x", at.Add(time.Minute))
	if !ok || a.Consecutive != FailureAlertAfter || a.FirstFailed != at {
		t.Fatalf("kept order's streak did not survive the prune: %+v (ok=%v)", a, ok)
	}
	for i := 0; i < FailureAlertAfter-1; i++ {
		if a, ok := s.ObserveFailure("removed", "x", at.Add(time.Minute)); ok {
			t.Fatalf("removed order's streak survived the prune: %+v", a)
		}
	}
}
