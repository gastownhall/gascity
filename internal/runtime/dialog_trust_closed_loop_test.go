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

// fakeClaudeTrustPane models Claude Code's workspace-trust dialog: two rows,
// the cursor defaulting to "No, exit", and a window right after the first
// render in which keystrokes are dropped (observed live on Claude 2.1.278-281;
// see #6531/#6532). Enter confirms whichever row the cursor is on.
type fakeClaudeTrustPane struct {
	mu sync.Mutex
	// cursor is 0 on "No, exit", 1 on "Yes, I trust this folder".
	cursor int
	// dropDowns is how many upcoming Down keys are swallowed; <0 drops all.
	dropDowns int
	sent      []string
	confirmed string // "" until Enter; then "no" or "trust"
	// onChange is called with the new frame whenever the screen changes, so
	// the pane can drive a change-driven snapshot stream.
	onChange func(string)
}

func (p *fakeClaudeTrustPane) frame() string {
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

func (p *fakeClaudeTrustPane) peek(int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frame(), nil
}

func (p *fakeClaudeTrustPane) sendKeys(keys ...string) error {
	p.mu.Lock()
	var changed []string
	for _, k := range keys {
		p.sent = append(p.sent, k)
		if p.confirmed != "" {
			continue
		}
		switch k {
		case "Down", "Up":
			if p.dropDowns != 0 {
				if p.dropDowns > 0 {
					p.dropDowns--
				}
				continue // dropped: no state change, no re-render
			}
			switch {
			case k == "Down" && p.cursor == 0:
				p.cursor = 1
			case k == "Up" && p.cursor == 1:
				p.cursor = 0
			default:
				continue
			}
		case "Enter":
			if p.cursor == 1 {
				p.confirmed = "trust"
			} else {
				p.confirmed = "no"
			}
		default:
			continue
		}
		changed = append(changed, p.frame())
	}
	onChange := p.onChange
	p.mu.Unlock()
	if onChange != nil {
		for _, f := range changed {
			onChange(f)
		}
	}
	return nil
}

func (p *fakeClaudeTrustPane) result() ([]string, string) {
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
			pane := &fakeClaudeTrustPane{cursor: tt.cursor, dropDowns: tt.dropDowns}

			err := acceptWorkspaceTrustDialog(context.Background(), newStartupDialogBudget(time.Second), pane.peek, pane.sendKeys)

			assertNeverConfirmedNoExit(t, pane)
			sent, confirmed := pane.result()
			if !reflect.DeepEqual(sent, tt.wantSent) {
				t.Fatalf("sent = %v, want %v", sent, tt.wantSent)
			}
			if tt.wantErr {
				if !errors.Is(err, errWorkspaceTrustUnconfirmed) {
					t.Fatalf("error = %v, want errWorkspaceTrustUnconfirmed", err)
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

// TestAcceptStartupDialogsTrustDialogSurvivesDroppedDown drives the whole
// polling sequence (as the tmux provider does) through a dropped first Down.
func TestAcceptStartupDialogsTrustDialogSurvivesDroppedDown(t *testing.T) {
	withZeroDialogTimings(t)
	pane := &fakeClaudeTrustPane{dropDowns: 1}

	if err := AcceptStartupDialogsWithTimeout(context.Background(), time.Second, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout() error = %v", err)
	}
	assertNeverConfirmedNoExit(t, pane)
	if sent, confirmed := pane.result(); confirmed != "trust" || !reflect.DeepEqual(sent, []string{"Down", "Down", "Enter"}) {
		t.Fatalf("sent = %v confirmed = %q, want [Down Down Enter] trust", sent, confirmed)
	}
}

// newChangeDrivenTrustStream wires pane to a snapshot stream that, like a
// change-driven watch-startup op, publishes only when the screen changes: a
// dropped keystroke produces no snapshot at all.
func newChangeDrivenTrustStream(pane *fakeClaudeTrustPane, initialCopies int) *replayableSnapshotStream {
	stream := &replayableSnapshotStream{update: make(chan struct{})}
	for i := 0; i < initialCopies; i++ {
		stream.publish(pane.frame())
	}
	pane.onChange = stream.publish
	return stream
}

func TestAcceptWorkspaceTrustDialogFromStreamClosedLoop(t *testing.T) {
	tests := []struct {
		name      string
		cursor    int
		dropDowns int
		// staleCopies of the initial frame are queued before any key is sent,
		// as a stream that emits several frames of the same render would.
		staleCopies int
		settle      time.Duration
		wantSent    []string
		wantErr     bool
	}{
		{name: "down lands", staleCopies: 1, wantSent: []string{"Down", "Enter"}},
		{name: "stale queued frames are not re-used", staleCopies: 4, wantSent: []string{"Down", "Enter"}},
		{name: "first down dropped, quiet stream", staleCopies: 1, dropDowns: 1, settle: 5 * time.Millisecond, wantSent: []string{"Down", "Down", "Enter"}},
		{name: "first down dropped, stale frames queued", staleCopies: 3, dropDowns: 1, wantSent: []string{"Down", "Down", "Enter"}},
		{name: "trust preselected", cursor: 1, staleCopies: 1, wantSent: []string{"Enter"}},
		{
			name: "cursor never reaches trust row", staleCopies: 2, dropDowns: -1, settle: 5 * time.Millisecond,
			wantSent: []string{"Down", "Down", "Down"}, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withZeroDialogTimings(t)
			startupDialogAcceptDelay = tt.settle
			pane := &fakeClaudeTrustPane{cursor: tt.cursor, dropDowns: tt.dropDowns}
			stream := newChangeDrivenTrustStream(pane, tt.staleCopies)

			observed, err := acceptWorkspaceTrustDialogFromStream(
				context.Background(), 2*time.Second, newReplayableSnapshotCursorFromStream(stream), pane.sendKeys)

			assertNeverConfirmedNoExit(t, pane)
			sent, confirmed := pane.result()
			if !reflect.DeepEqual(sent, tt.wantSent) {
				t.Fatalf("sent = %v, want %v", sent, tt.wantSent)
			}
			if !observed {
				t.Fatalf("observed = false, want true")
			}
			if tt.wantErr {
				if !errors.Is(err, errWorkspaceTrustUnconfirmed) {
					t.Fatalf("error = %v, want errWorkspaceTrustUnconfirmed", err)
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

// TestAcceptStartupDialogsFromStreamTrustDialogSurvivesDroppedDown drives the
// whole stream sequence (as the exec provider does) through a dropped Down,
// and checks the unconfirmed case surfaces as an error from the public API.
func TestAcceptStartupDialogsFromStreamTrustDialogSurvivesDroppedDown(t *testing.T) {
	for _, tt := range []struct {
		name      string
		dropDowns int
		wantErr   bool
	}{
		{name: "dropped once", dropDowns: 1},
		{name: "always dropped", dropDowns: -1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withZeroDialogTimings(t)
			startupDialogAcceptDelay = 5 * time.Millisecond
			pane := &fakeClaudeTrustPane{dropDowns: tt.dropDowns}
			snapshots := make(chan string, 16)
			snapshots <- pane.frame()
			var once sync.Once
			pane.onChange = func(f string) {
				snapshots <- f
				if strings.HasPrefix(f, "❯") {
					once.Do(func() { close(snapshots) })
				}
			}

			observed, err := AcceptStartupDialogsFromStreamWithStatus(context.Background(), 2*time.Second, snapshots, pane.sendKeys)

			assertNeverConfirmedNoExit(t, pane)
			sent, confirmed := pane.result()
			if tt.wantErr {
				if !errors.Is(err, errWorkspaceTrustUnconfirmed) {
					t.Fatalf("error = %v, want errWorkspaceTrustUnconfirmed (sent=%v)", err, sent)
				}
				return
			}
			if err != nil || !observed {
				t.Fatalf("observed = %v error = %v", observed, err)
			}
			if confirmed != "trust" || !reflect.DeepEqual(sent, []string{"Down", "Down", "Enter"}) {
				t.Fatalf("sent = %v confirmed = %q, want [Down Down Enter] trust", sent, confirmed)
			}
		})
	}
}
