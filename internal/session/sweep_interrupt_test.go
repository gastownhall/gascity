package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSweepInterruptTouchesNothing: a stop sweep's interrupt-now
// submit should reach no provider call. It takes no lease, so any Interrupt
// or Stop it makes runs under another holder's lease.
func TestSweepInterruptTouchesNothing(t *testing.T) {
	m := newManagerLeaseFixture(t)
	if err := m.start(m.operator(nil)); err != nil {
		t.Fatal(err)
	}
	held := m.hold(t)
	defer held.Release()
	m.sp.WaitForIdleErrors = map[string]error{m.info.SessionName: errors.New("not idle yet")} // forces the restart fallback
	before := m.sp.CountCalls("Interrupt", m.info.SessionName) + m.sp.CountCalls("SendKeys", m.info.SessionName)
	_, err := m.mgr.Submit(context.Background(), m.as(ActorSweep), m.info.ID, "replace the turn", BuildResumeCommand(m.info), runtime.Config{WorkDir: m.info.WorkDir}, SubmitIntentInterruptNow)
	after := m.sp.CountCalls("Interrupt", m.info.SessionName) + m.sp.CountCalls("SendKeys", m.info.SessionName)
	t.Logf("sweep interrupt-now submit under another holder's lease: err=%v, interrupt/keys calls=%d, stops=%d, running=%v", err, after-before, len(m.sp.stops), m.sp.IsRunning(m.info.SessionName))
	if after != before || len(m.sp.stops) != 0 {
		t.Fatalf("a stop sweep's submit reached the provider under another holder's lease (interrupts %d, stops %q, running %v, err %v)", after-before, m.sp.stops, m.sp.IsRunning(m.info.SessionName), err)
	}
}

// TestSweepIsRefusedAtEveryStartersEntry: every verb that may start, resume
// or interrupt refuses a stop sweep at its entry, before it reads the row:
// on a row that does not exist it answers ErrSweepStarts, not not-found.
func TestSweepIsRefusedAtEveryStartersEntry(t *testing.T) {
	m := newManagerLeaseFixture(t)
	sweep, hints := m.as(ActorSweep), runtime.Config{}
	const missing = "no-such-session"
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Start":  func() error { return m.mgr.Start(ctx, sweep, missing, "cmd", hints) },
		"Attach": func() error { return m.mgr.Attach(ctx, sweep, missing, "cmd", hints) },
		"Send": func() error {
			_, err := m.mgr.Send(ctx, sweep, missing, "hi", "cmd", hints)
			return err
		},
		"SendImmediate": func() error {
			_, err := m.mgr.SendImmediate(ctx, sweep, missing, "hi", "cmd", hints)
			return err
		},
		"Submit": func() error {
			_, err := m.mgr.Submit(ctx, sweep, missing, "hi", "cmd", hints, SubmitIntentDefault)
			return err
		},
		"TryWaitIdleNudge": func() error {
			_, err := m.mgr.TryWaitIdleNudge(ctx, sweep, missing, "test", "hi", "cmd", hints)
			return err
		},
		"StartRuntimeOnly": func() error { return m.mgr.StartRuntimeOnly(ctx, sweep, missing, "cmd", hints) },
	} {
		if err := call(); !errors.Is(err, ErrSweepStarts) {
			t.Errorf("%s by a sweep on a missing row = %v, want ErrSweepStarts at entry", name, err)
		}
	}
}
