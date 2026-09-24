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

	keyQ, frameQ *delayedQueue
	inFlight     *sync.WaitGroup
}

type delayedQueue struct {
	lag time.Duration
	ch  chan delayedFunc
	wg  *sync.WaitGroup
}

type delayedFunc struct {
	at time.Time
	f  func()
}

func newDelayedQueue(lag time.Duration, wg *sync.WaitGroup) *delayedQueue {
	q := &delayedQueue{lag: lag, ch: make(chan delayedFunc, 64), wg: wg}
	if lag > 0 {
		go func() {
			for d := range q.ch {
				// Simulated lag: run f at its scheduled time.
				<-time.NewTimer(time.Until(d.at)).C
				d.f()
				q.wg.Done()
			}
		}()
	}
	return q
}

// push runs f after the queue's lag, in FIFO order (immediately when lag is 0).
func (q *delayedQueue) push(f func()) {
	if q.lag <= 0 {
		f()
		return
	}
	q.wg.Add(1)
	q.ch <- delayedFunc{at: time.Now().Add(q.lag), f: f}
}

type fakePaneOpts struct {
	cursor    int
	dropDowns int
	keyLag    time.Duration
	frameLag  time.Duration
	// resetAt, when set, re-renders the dialog that long after the pane is
	// created, moving the cursor back to "No, exit".
	resetAt time.Duration
}

func newFakeClaudeTrustPane(t *testing.T, o fakePaneOpts) *fakeClaudeTrustPane {
	t.Helper()
	wg := &sync.WaitGroup{}
	p := &fakeClaudeTrustPane{cursor: o.cursor, dropDowns: o.dropDowns, inFlight: wg}
	p.keyQ = newDelayedQueue(o.keyLag, wg)
	p.frameQ = newDelayedQueue(o.frameLag, wg)
	// Let in-flight keys and frames drain (they may feed each other) before
	// the queues close.
	t.Cleanup(func() {
		wg.Wait()
		for _, q := range []*delayedQueue{p.keyQ, p.frameQ} {
			if q.lag > 0 {
				close(q.ch)
			}
		}
	})
	if o.resetAt > 0 {
		wg.Add(1)
		timer := time.AfterFunc(o.resetAt, func() {
			defer wg.Done()
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
				p.frameQ.push(func() { onChange(f) })
			}
		})
		t.Cleanup(func() {
			if timer.Stop() {
				wg.Done()
			}
		})
	}
	return p
}

// flush waits until every in-flight key and frame has been applied/delivered.
func (p *fakeClaudeTrustPane) flush() {
	p.inFlight.Wait()
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
		p.keyQ.push(func() { p.apply(k) })
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
		p.frameQ.push(func() { onChange(f) })
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
// consecutive frames once more than one move was needed keeps Enter off it.
func TestAcceptWorkspaceTrustDialogLateKeys(t *testing.T) {
	for _, keyLag := range []time.Duration{5, 15, 25, 35, 45, 60} {
		keyLag *= time.Millisecond
		t.Run(keyLag.String(), func(t *testing.T) {
			withZeroDialogTimings(t)
			startupDialogAcceptDelay = 20 * time.Millisecond
			dialogPollInterval = 5 * time.Millisecond
			pane := newFakeClaudeTrustPane(t, fakePaneOpts{keyLag: keyLag})

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
	for _, resetAt := range []time.Duration{10, 25, 35} {
		resetAt *= time.Millisecond
		t.Run(resetAt.String(), func(t *testing.T) {
			withZeroDialogTimings(t)
			startupDialogAcceptDelay = 20 * time.Millisecond
			dialogPollInterval = 5 * time.Millisecond
			pane := newFakeClaudeTrustPane(t, fakePaneOpts{keyLag: 10 * time.Millisecond, resetAt: resetAt})

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
		wantSent     []string // nil: only check the outcome (timing-dependent)
	}{
		{name: "trust preselected", opts: fakePaneOpts{cursor: 1}, wantObserved: true, wantSent: []string{"Enter"}},
		{name: "down lands", wantSent: []string{"Down", "Enter"}},
		{name: "down dropped", opts: fakePaneOpts{dropDowns: 1}, wantSent: []string{"Down", "Down", "Enter"}},
		// The reviewer's repro shape: 15ms frame lag, 5ms delay.
		{name: "lagged frames", opts: fakePaneOpts{frameLag: 15 * time.Millisecond}, wantSent: []string{"Down", "Enter"}},
		{name: "late keys and lagged frames", opts: fakePaneOpts{keyLag: 8 * time.Millisecond, frameLag: 15 * time.Millisecond}},
		{name: "cursor reset after first paint", opts: fakePaneOpts{keyLag: 5 * time.Millisecond, resetAt: 12 * time.Millisecond}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withZeroDialogTimings(t)
			// Keys apply well within the settle delay, as with real Claude
			// (500ms); key lag past it is covered by the LateKeys test.
			startupDialogAcceptDelay = 20 * time.Millisecond
			dialogPollInterval = 2 * time.Millisecond
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
			if tt.wantSent != nil && !reflect.DeepEqual(sent, tt.wantSent) {
				t.Fatalf("sent = %v, want %v", sent, tt.wantSent)
			}
		})
	}
}
