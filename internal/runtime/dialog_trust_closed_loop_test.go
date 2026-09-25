package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// steppedDialogClock is a virtual clock for the startup-dialog helpers
// (installed as dialogClock). Time only moves when the handler sleeps: Sleep
// runs every event due by the end of the sleep, in (time, schedule order), then
// returns. So the ordering of a fake pane's late keys, lagged frames and
// re-renders against the handler's peeks is fixed by the scenario, not raced
// on real timers. Tie rule: an event due at the instant a sleep ends has
// happened by the next peek.
type steppedDialogClock struct {
	mu     sync.Mutex
	now    time.Time
	seq    int
	events []steppedDialogEvent
}

type steppedDialogEvent struct {
	at  time.Time
	seq int
	f   func()
}

// useSteppedDialogClock installs a stepped virtual clock as dialogClock for
// the rest of the test.
func useSteppedDialogClock(t *testing.T) *steppedDialogClock {
	t.Helper()
	c := &steppedDialogClock{now: time.Unix(0, 0)}
	old := dialogClock
	dialogClock = c
	t.Cleanup(func() { dialogClock = old })
	return c
}

func (c *steppedDialogClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppedDialogClock) Sleep(_ context.Context, d time.Duration) {
	c.advance(d)
}

// after schedules f to run d after the current virtual time.
func (c *steppedDialogClock) after(d time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	c.events = append(c.events, steppedDialogEvent{at: c.now.Add(d), seq: c.seq, f: f})
}

// advance moves virtual time forward by d, running each event due by then
// at its own time (an event may schedule further events).
func (c *steppedDialogClock) advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for c.runNext(target, false) {
	}
	c.mu.Lock()
	if c.now.Before(target) {
		c.now = target
	}
	c.mu.Unlock()
}

// drain runs every pending event, however far in the future: the in-flight
// keys and frames land.
func (c *steppedDialogClock) drain() {
	for c.runNext(time.Time{}, true) {
	}
}

func (c *steppedDialogClock) runNext(target time.Time, all bool) bool {
	c.mu.Lock()
	next := -1
	for i, ev := range c.events {
		if next < 0 || ev.at.Before(c.events[next].at) ||
			(ev.at.Equal(c.events[next].at) && ev.seq < c.events[next].seq) {
			next = i
		}
	}
	if next < 0 || (!all && c.events[next].at.After(target)) {
		c.mu.Unlock()
		return false
	}
	ev := c.events[next]
	c.events = append(c.events[:next], c.events[next+1:]...)
	if ev.at.After(c.now) {
		c.now = ev.at
	}
	c.mu.Unlock()
	ev.f()
	return true
}

// fakeClaudeTrustPane models Claude Code's workspace-trust dialog as observed
// live on Claude 2.1.278-2.1.281 (#6531/#6532):
//   - two rows, with the cursor defaulting to "No, exit";
//   - the cursor WRAPS (Down, Down goes No -> Yes -> No; Up wraps too);
//   - keys can be dropped right after the first render (dropDowns);
//   - keys are applied in order, but optionally late (keyLag);
//   - a snapshot stream can deliver a changed frame late (frameLag);
//   - shortly after first paint the dialog re-renders, snapping the cursor
//     back to "No, exit" (resetAt; seen live on 2.1.281).
//
// Enter confirms whichever row the cursor is on when Enter is applied.
// Lags and the re-render run on a steppedDialogClock, so they are ordered
// against the handler's peeks by virtual time, not by goroutine scheduling.
type fakeClaudeTrustPane struct {
	mu sync.Mutex
	// cursor is 0 on "No, exit", 1 on "Yes, I trust this folder".
	cursor int
	// dropDowns is how many upcoming movement keys are swallowed; <0 drops all.
	dropDowns int
	sent      []string
	confirmed string // "" until Enter is applied; then "no" or "trust"
	// onChange receives every changed frame (after frameLag), so the pane
	// can drive a change-driven snapshot stream.
	onChange func(string)

	clock            *steppedDialogClock
	keyLag, frameLag time.Duration
}

type fakePaneOpts struct {
	cursor    int
	dropDowns int
	keyLag    time.Duration
	frameLag  time.Duration
	// resetAt, when set, re-renders the dialog that long after the pane is
	// created, moving the cursor back to "No, exit".
	resetAt time.Duration
	// clock times keyLag, frameLag and resetAt; required when any is set.
	clock *steppedDialogClock
}

func newFakeClaudeTrustPane(t *testing.T, o fakePaneOpts) *fakeClaudeTrustPane {
	t.Helper()
	if o.clock == nil && (o.keyLag > 0 || o.frameLag > 0 || o.resetAt > 0) {
		t.Fatal("fake pane timing needs a stepped clock (useSteppedDialogClock)")
	}
	p := &fakeClaudeTrustPane{
		cursor: o.cursor, dropDowns: o.dropDowns,
		clock: o.clock, keyLag: o.keyLag, frameLag: o.frameLag,
	}
	if o.resetAt > 0 {
		o.clock.after(o.resetAt, func() {
			p.mu.Lock()
			if p.confirmed != "" || p.cursor == 0 {
				p.mu.Unlock()
				return
			}
			p.cursor = 0
			f := p.frameLocked()
			onChange := p.onChange
			p.mu.Unlock()
			if onChange != nil {
				p.later(p.frameLag, func() { onChange(f) })
			}
		})
	}
	return p
}

// later runs f after lag of virtual time, in FIFO order among equal lags
// (immediately when lag is 0).
func (p *fakeClaudeTrustPane) later(lag time.Duration, f func()) {
	if lag <= 0 {
		f()
		return
	}
	p.clock.after(lag, f)
}

// flush applies every in-flight key and delivers every in-flight frame.
func (p *fakeClaudeTrustPane) flush() {
	if p.clock != nil {
		p.clock.drain()
	}
}

func (p *fakeClaudeTrustPane) frameLocked() string {
	switch p.confirmed {
	case "trust":
		return "❯ "
	case "no":
		return "user@host $"
	}
	if p.cursor == 1 {
		return strings.Replace(
			strings.Replace(realTrustDialogNoExitSelected, "❯ No, exit", "  No, exit", 1),
			"  Yes, I trust this folder", "❯ Yes, I trust this folder", 1)
	}
	return realTrustDialogNoExitSelected
}

func (p *fakeClaudeTrustPane) frame() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frameLocked()
}

// peek is a synchronous capture of the screen as Claude has rendered it so far.
func (p *fakeClaudeTrustPane) peek(int) (string, error) {
	return p.frame(), nil
}

func (p *fakeClaudeTrustPane) sendKeys(keys ...string) error {
	p.mu.Lock()
	p.sent = append(p.sent, keys...)
	p.mu.Unlock()
	for _, k := range keys {
		p.later(p.keyLag, func() { p.apply(k) })
	}
	return nil
}

func (p *fakeClaudeTrustPane) apply(k string) {
	p.mu.Lock()
	if p.confirmed != "" {
		p.mu.Unlock()
		return
	}
	switch k {
	case "Down", "Up":
		if p.dropDowns != 0 {
			if p.dropDowns > 0 {
				p.dropDowns--
			}
			p.mu.Unlock()
			return // dropped: no state change, no re-render
		}
		p.cursor = (p.cursor + 1) % 2 // two rows; both directions wrap
	case "Enter":
		if p.cursor == 1 {
			p.confirmed = "trust"
		} else {
			p.confirmed = "no"
		}
	default:
		p.mu.Unlock()
		return
	}
	f := p.frameLocked()
	onChange := p.onChange
	p.mu.Unlock()
	if onChange != nil {
		p.later(p.frameLag, func() { onChange(f) })
	}
}

func (p *fakeClaudeTrustPane) result() ([]string, string) {
	p.flush()
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sent...), p.confirmed
}

func assertNeverConfirmedNoExit(t *testing.T, pane *fakeClaudeTrustPane) {
	t.Helper()
	sent, confirmed := pane.result()
	if confirmed == "no" {
		t.Fatalf("handler confirmed %q; sent=%v", "No, exit", sent)
	}
}

func TestFakeClaudeTrustPaneWraps(t *testing.T) {
	pane := newFakeClaudeTrustPane(t, fakePaneOpts{})
	_ = pane.sendKeys("Down", "Down", "Enter")
	if _, confirmed := pane.result(); confirmed != "no" {
		t.Fatalf("Down,Down,Enter confirmed %q, want no (cursor wraps like real Claude)", confirmed)
	}
}

func TestAcceptWorkspaceTrustDialogClosedLoop(t *testing.T) {
	tests := []struct {
		name      string
		cursor    int
		dropDowns int
		wantSent  []string
		wantErr   bool
	}{
		{name: "down lands", cursor: 0, wantSent: []string{"Down", "Enter"}},
		{name: "first down dropped", cursor: 0, dropDowns: 1, wantSent: []string{"Down", "Down", "Enter"}},
		{name: "two downs dropped", cursor: 0, dropDowns: 2, wantSent: []string{"Down", "Down", "Down", "Enter"}},
		{name: "trust preselected", cursor: 1, wantSent: []string{"Enter"}},
		{
			name: "cursor never reaches trust row", cursor: 0, dropDowns: -1,
			wantSent: []string{"Down", "Down", "Down"}, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withZeroDialogTimings(t)
			pane := newFakeClaudeTrustPane(t, fakePaneOpts{cursor: tt.cursor, dropDowns: tt.dropDowns})

			err := acceptWorkspaceTrustDialog(context.Background(), newStartupDialogBudget(time.Second), pane.peek, pane.sendKeys)

			assertNeverConfirmedNoExit(t, pane)
			sent, confirmed := pane.result()
			if !reflect.DeepEqual(sent, tt.wantSent) {
				t.Fatalf("sent = %v, want %v", sent, tt.wantSent)
			}
			if tt.wantErr {
				if !errors.Is(err, ErrWorkspaceTrustUnconfirmed) {
					t.Fatalf("error = %v, want ErrWorkspaceTrustUnconfirmed", err)
				}
				if confirmed != "" {
					t.Fatalf("dialog confirmed %q, want left unconfirmed", confirmed)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if confirmed != "trust" {
				t.Fatalf("confirmed = %q, want trust", confirmed)
			}
		})
	}
}

// TestAcceptWorkspaceTrustDialogLateKeys covers Claude applying keys late
// on the polling path: a Down still in flight when the pane is re-read gets
// followed by a second Down, and the wrapping cursor ends back on "No, exit"
// after briefly showing the trust row. Requiring the trust row on two
// consecutive frames before Enter keeps Enter off it.
//
// Every key lags by the same amount and the handler polls on a fixed cadence
// (stepped clock), so each lag fixes the order of key arrivals against peeks;
// the sweep covers every order, including lags that land exactly on a peek.
// Out of scope, and part of the input-lag residual accepted for #6531: the
// trust row shows between two queued Downs for the gap between their sends
// (one handler cycle) plus any extra lag of the second. If that window
// outlasts the handler's next cycle (the second key lags the first by more,
// or a later cycle runs shorter than the one that sent it), two peeks a
// cycle apart can both land in it, and nothing on screen tells that apart
// from a settled trust row. In production this needs Claude input lag of
// about two settle delays (~1s) plus that much drift. Under CPU load the old
// real-timer version of this test drifted by a full 20ms cycle and hit it.
func TestAcceptWorkspaceTrustDialogLateKeys(t *testing.T) {
	for keyLag := time.Millisecond; keyLag <= 100*time.Millisecond; keyLag += time.Millisecond {
		t.Run(keyLag.String(), func(t *testing.T) {
			withZeroDialogTimings(t)
			startupDialogAcceptDelay = 20 * time.Millisecond
			dialogPollInterval = 5 * time.Millisecond
			pane := newFakeClaudeTrustPane(t, fakePaneOpts{keyLag: keyLag, clock: useSteppedDialogClock(t)})

			err := acceptWorkspaceTrustDialog(context.Background(), newStartupDialogBudget(2*time.Second), pane.peek, pane.sendKeys)

			assertNeverConfirmedNoExit(t, pane)
			sent, confirmed := pane.result()
			switch {
			case err == nil && confirmed == "trust":
			case errors.Is(err, ErrWorkspaceTrustUnconfirmed) && confirmed == "":
			default:
				t.Fatalf("err = %v confirmed = %q sent = %v; want trust, or unconfirmed with ErrWorkspaceTrustUnconfirmed", err, confirmed, sent)
			}
		})
	}
}

// TestAcceptWorkspaceTrustDialogLaterPassWithKeysInFlight covers a second
// pass (the tmux post-readiness pass, or a deferred dismiss) starting while
// the first pass's movement keys are still in flight. The second pass sent
// no move itself, so it must still require the trust row on two consecutive
// frames, or it Enters on a "Yes" frame that a queued Down is about to move
// back to "No, exit".
//
// Stepped timeline (settle 20ms, key lag 45ms, first Down dropped): pass 1
// sends Downs at 0, 20 and 40 (landing at 45 dropped, 65, 85), still sees
// "No" at 60 and gives up. Pass 2 starts at 70 on the "Yes" frame from the
// 65 Down, with the 85 Down still queued.
func TestAcceptWorkspaceTrustDialogLaterPassWithKeysInFlight(t *testing.T) {
	withZeroDialogTimings(t)
	startupDialogAcceptDelay = 20 * time.Millisecond
	dialogPollInterval = 5 * time.Millisecond
	clock := useSteppedDialogClock(t)
	pane := newFakeClaudeTrustPane(t, fakePaneOpts{dropDowns: 1, keyLag: 45 * time.Millisecond, clock: clock})

	err1 := acceptWorkspaceTrustDialog(context.Background(), newStartupDialogBudget(2*time.Second), pane.peek, pane.sendKeys)
	if !errors.Is(err1, ErrWorkspaceTrustUnconfirmed) {
		t.Fatalf("pass 1 error = %v, want ErrWorkspaceTrustUnconfirmed (keys still in flight)", err1)
	}
	// Pass 2 starts 10ms after pass 1 gives up, with its keys still queued.
	clock.advance(10 * time.Millisecond)
	if got := pane.frame(); !strings.Contains(got, "❯ Yes") {
		t.Fatalf("pass 2 should start on a trust-row frame with a Down queued; frame:\n%s", got)
	}
	err2 := acceptWorkspaceTrustDialog(context.Background(), newStartupDialogBudget(2*time.Second), pane.peek, pane.sendKeys)

	assertNeverConfirmedNoExit(t, pane)
	sent, confirmed := pane.result()
	switch {
	case err2 == nil && confirmed == "trust":
	case errors.Is(err2, ErrWorkspaceTrustUnconfirmed) && confirmed == "":
	default:
		t.Fatalf("pass 2 err = %v confirmed = %q sent = %v; want trust, or unconfirmed with ErrWorkspaceTrustUnconfirmed", err2, confirmed, sent)
	}
}

// TestAcceptStartupDialogsTrustDialogSurvivesDroppedDown drives the whole
// polling sequence (as the tmux provider does) through a dropped first Down.
func TestAcceptStartupDialogsTrustDialogSurvivesDroppedDown(t *testing.T) {
	withZeroDialogTimings(t)
	pane := newFakeClaudeTrustPane(t, fakePaneOpts{dropDowns: 1})

	if err := AcceptStartupDialogsWithTimeout(context.Background(), time.Second, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout() error = %v", err)
	}
	assertNeverConfirmedNoExit(t, pane)
	if sent, confirmed := pane.result(); confirmed != "trust" || !reflect.DeepEqual(sent, []string{"Down", "Down", "Enter"}) {
		t.Fatalf("sent = %v confirmed = %q, want [Down Down Enter] trust", sent, confirmed)
	}
}

// TestAcceptWorkspaceTrustDialogCursorResetByRerender covers the re-render
// Claude does shortly after the trust dialog first paints (observed live on
// 2.1.281): the dialog remounts, the cursor snaps back to "No, exit", and
// keys in flight are lost. A frame showing the trust row just before the
// reset must not be enough to confirm, or a late Enter lands on "No".
//
// The two-frame rule covers a reset up to two settle delays after the move
// (1s in production, against a re-render seen ~100-200ms after first paint);
// here the second trust frame is read at ~40ms, so resets land before it.
func TestAcceptWorkspaceTrustDialogCursorResetByRerender(t *testing.T) {
	// Down lands at 10ms; trust frames are read at 20 and 40ms. Resets at
	// 5 (before the Down: no-op), 10 (same instant, before the Down), 15
	// (between the Down and the first trust frame), 25 and 35 (between the
	// two trust frames).
	for _, resetAt := range []time.Duration{5, 10, 15, 25, 35} {
		resetAt *= time.Millisecond
		t.Run(resetAt.String(), func(t *testing.T) {
			withZeroDialogTimings(t)
			startupDialogAcceptDelay = 20 * time.Millisecond
			dialogPollInterval = 5 * time.Millisecond
			pane := newFakeClaudeTrustPane(t, fakePaneOpts{keyLag: 10 * time.Millisecond, resetAt: resetAt, clock: useSteppedDialogClock(t)})

			err := acceptWorkspaceTrustDialog(context.Background(), newStartupDialogBudget(2*time.Second), pane.peek, pane.sendKeys)

			assertNeverConfirmedNoExit(t, pane)
			sent, confirmed := pane.result()
			if err != nil || confirmed != "trust" {
				t.Fatalf("err = %v confirmed = %q sent = %v; want trust", err, confirmed, sent)
			}
		})
	}
}

// newChangeDrivenTrustStream wires pane to a snapshot stream that, like a
// change-driven watch-startup op, publishes only when the screen changes.
func newChangeDrivenTrustStream(pane *fakeClaudeTrustPane, initialCopies int) *replayableSnapshotStream {
	stream := &replayableSnapshotStream{update: make(chan struct{})}
	for i := 0; i < initialCopies; i++ {
		stream.publish(pane.frame())
	}
	pane.mu.Lock()
	pane.onChange = stream.publish
	pane.mu.Unlock()
	return stream
}

// TestAcceptWorkspaceTrustDialogFromStreamNeverMoves pins the stream half: a
// snapshot stream cannot re-read the screen, so when the cursor is off the
// trust row the stream handler sends nothing and reports the stream
// inconclusive; only a frame already showing the trust row is confirmed.
func TestAcceptWorkspaceTrustDialogFromStreamNeverMoves(t *testing.T) {
	tests := []struct {
		name             string
		opts             fakePaneOpts
		staleCopies      int
		wantSent         []string
		wantInconclusive bool
	}{
		{name: "cursor on No", staleCopies: 1, wantInconclusive: true},
		{name: "cursor on No, queued frames", staleCopies: 4, wantInconclusive: true},
		{name: "cursor on No, lagged frames", opts: fakePaneOpts{frameLag: 15 * time.Millisecond, keyLag: 5 * time.Millisecond}, staleCopies: 2, wantInconclusive: true},
		{name: "trust preselected", opts: fakePaneOpts{cursor: 1}, staleCopies: 1, wantSent: []string{"Enter"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withZeroDialogTimings(t)
			startupDialogAcceptDelay = 5 * time.Millisecond
			tt.opts.clock = useSteppedDialogClock(t)
			pane := newFakeClaudeTrustPane(t, tt.opts)
			stream := newChangeDrivenTrustStream(pane, tt.staleCopies)

			observed, err := acceptWorkspaceTrustDialogFromStream(
				context.Background(), 5*time.Second, newReplayableSnapshotCursorFromStream(stream), pane.sendKeys)

			assertNeverConfirmedNoExit(t, pane)
			sent, confirmed := pane.result()
			if !reflect.DeepEqual(sent, tt.wantSent) {
				t.Fatalf("sent = %v, want %v (err=%v)", sent, tt.wantSent, err)
			}
			if !observed {
				t.Fatalf("observed = false, want true")
			}
			if tt.wantInconclusive {
				if !errors.Is(err, errStartupDialogStreamInconclusive) {
					t.Fatalf("error = %v, want errStartupDialogStreamInconclusive", err)
				}
				if confirmed != "" {
					t.Fatalf("dialog confirmed %q, want left unconfirmed", confirmed)
				}
				return
			}
			if err != nil || confirmed != "trust" {
				t.Fatalf("err = %v confirmed = %q, want trust", err, confirmed)
			}
		})
	}
}

// TestAcceptStartupDialogsFromStreamTrustDialogFallsBackToPeeks drives the
// exec provider's sequence (exec.go dismissStartupDialogs): the stream path
// first, then, when it reports the stream inconclusive, the synchronous
// polling path. Across dropped keys, late keys, lagged frames and the
// post-paint cursor reset, the result must be trusted, never "No, exit".
func TestAcceptStartupDialogsFromStreamTrustDialogFallsBackToPeeks(t *testing.T) {
	for _, tt := range []struct {
		name         string
		opts         fakePaneOpts
		wantObserved bool
		wantSent     []string
	}{
		{name: "trust preselected", opts: fakePaneOpts{cursor: 1}, wantObserved: true, wantSent: []string{"Enter"}},
		{name: "down lands", wantSent: []string{"Down", "Enter"}},
		{name: "down dropped", opts: fakePaneOpts{dropDowns: 1}, wantSent: []string{"Down", "Down", "Enter"}},
		// The reviewer's repro shape: 15ms frame lag, 5ms delay.
		{name: "lagged frames", opts: fakePaneOpts{frameLag: 15 * time.Millisecond}, wantSent: []string{"Down", "Enter"}},
		{name: "late keys and lagged frames", opts: fakePaneOpts{keyLag: 8 * time.Millisecond, frameLag: 15 * time.Millisecond}, wantSent: []string{"Down", "Enter"}},
		{name: "cursor reset after first paint", opts: fakePaneOpts{keyLag: 5 * time.Millisecond, resetAt: 12 * time.Millisecond}, wantSent: []string{"Down", "Down", "Enter"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withZeroDialogTimings(t)
			// Keys apply well within the settle delay, as with real Claude
			// (500ms); key lag past it is covered by the LateKeys test.
			startupDialogAcceptDelay = 20 * time.Millisecond
			dialogPollInterval = 2 * time.Millisecond
			tt.opts.clock = useSteppedDialogClock(t)
			pane := newFakeClaudeTrustPane(t, tt.opts)
			snapshots := make(chan string, 64)
			snapshots <- pane.frame()
			var streamMu sync.Mutex
			streamOpen := true
			closeStream := func() {
				streamMu.Lock()
				defer streamMu.Unlock()
				if streamOpen {
					streamOpen = false
					close(snapshots)
				}
			}
			t.Cleanup(closeStream)
			pane.mu.Lock()
			pane.onChange = func(f string) {
				streamMu.Lock()
				defer streamMu.Unlock()
				if !streamOpen {
					return
				}
				snapshots <- f
				if strings.HasPrefix(f, "❯") {
					streamOpen = false
					close(snapshots)
				}
			}
			pane.mu.Unlock()

			observed, err := AcceptStartupDialogsFromStreamWithStatus(context.Background(), 2*time.Second, snapshots, pane.sendKeys)
			if err != nil {
				t.Fatalf("stream error = %v", err)
			}
			if observed != tt.wantObserved {
				t.Fatalf("observed = %v, want %v", observed, tt.wantObserved)
			}
			if !observed {
				// Like exec.go: close the watch, then fall back to peeks.
				closeStream()
				if err := AcceptStartupDialogsWithTimeout(context.Background(), 2*time.Second, pane.peek, pane.sendKeys); err != nil {
					t.Fatalf("polling fallback error = %v", err)
				}
			}

			assertNeverConfirmedNoExit(t, pane)
			sent, confirmed := pane.result()
			if confirmed != "trust" {
				t.Fatalf("confirmed = %q sent = %v, want trust", confirmed, sent)
			}
			if !reflect.DeepEqual(sent, tt.wantSent) {
				t.Fatalf("sent = %v, want %v", sent, tt.wantSent)
			}
		})
	}
}
