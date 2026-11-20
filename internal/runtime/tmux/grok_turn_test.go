package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Grok Build's live screen: a boxed composer ("│ ❯ … │") that stays on screen
// while a turn runs, and a footer carrying grokBusyHint strictly below it while
// it works. grokIdleBox is the pane waiting for input; grokBusyBox is the same
// pane mid-turn; grokHintAboveBox carries the hint in the transcript above a
// fresh composer; grokBusyNoComposer is a working pane whose composer has
// scrolled away.
const (
	grokBoxTop    = "┌──────────────────────────────────────┐"
	grokBoxBottom = "└──────────────────────────────────────┘"
	grokComposer  = "│ ❯ │"
	grokHintRow   = "│ grok-4.7 · Ctrl+c:cancel │"

	grokIdleBox = "  ● wrote 3 files\n" +
		"\n" +
		grokBoxTop + "\n" +
		grokComposer + "\n" +
		grokBoxBottom
	grokBusyBox = "  ● reading the worktree\n" +
		"\n" +
		grokBoxTop + "\n" +
		grokComposer + "\n" +
		grokBoxBottom + "\n" +
		grokHintRow
	grokHintAboveBox = "  ● reading the worktree\n" +
		grokHintRow + "\n" +
		grokBoxTop + "\n" +
		grokComposer + "\n" +
		grokBoxBottom
	grokBusyNoComposer = "  ● reading the worktree\n" +
		"\n" +
		grokHintRow
)

// The busy reading is grok's own footer read relative to its live composer:
// the hint below the composer is a turn in progress, the same hint above it is
// transcript text an earlier turn printed, and the reader is consulted only
// for a grok pane — the hint's text in another family's pane, and every near
// miss in grok's own chrome, is never busy.
func TestPaneBusyReadingGrokFooter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
		family string
		want   bool
	}{
		{name: "grok idle", screen: grokIdleBox, family: "grok"},
		{name: "grok busy, footer below the composer", screen: grokBusyBox, family: "grok", want: true},
		{name: "grok hint above the composer is transcript", screen: grokHintAboveBox, family: "grok"},
		{name: "grok busy, composer scrolled away", screen: grokBusyNoComposer, family: "grok", want: true},
		{name: "claude pane carrying grok's footer text", screen: grokBusyNoComposer, family: "claude"},
		{name: "unidentified pane carrying grok's footer text", screen: grokBusyNoComposer},
		{name: "near miss lowercase", screen: "│ ctrl+c:cancel │", family: "grok"},
		{name: "near miss uppercase", screen: "│ CTRL+C:CANCEL │", family: "grok"},
		{name: "near miss phrase", screen: "│ Ctrl+C to cancel │", family: "grok"},
		{name: "generic indicator still reads through the shared scan", screen: busyTrustBusyPane, family: "grok", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneBusyReading(strings.Split(tc.screen, "\n"), tc.family); got != tc.want {
				t.Errorf("paneBusyReading(family %q) = %t, want %t", tc.family, got, tc.want)
			}
		})
	}
}

// A working grok pane keeps its boxed composer on screen, so the shared scan
// alone would read it idle: SnapshotIdle carries grok's footer reading through
// as busy, and the same pane without the footer is idle (AC-3).
func TestSnapshotIdleGrokFooter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		screen   string
		wantIdle bool
	}{
		{name: "busy", screen: grokBusyBox},
		{name: "idle", screen: grokIdleBox, wantIdle: true},
		{name: "hint above the composer", screen: grokHintAboveBox, wantIdle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = &familyStaticPane{family: "grok", screen: tc.screen}

			idle, err := tm.SnapshotIdle("lab")
			if err != nil {
				t.Fatalf("SnapshotIdle err = %v, want nil", err)
			}
			if idle != tc.wantIdle {
				t.Errorf("SnapshotIdle = %t, want %t", idle, tc.wantIdle)
			}
		})
	}
}

// TurnRunning reads the same grok pane the same way: the footer below the
// composer is a running turn, and without it the pane is not running one
// (AC-3).
func TestTurnRunningGrokFooter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		screen      string
		wantRunning bool
	}{
		{name: "busy", screen: grokBusyBox, wantRunning: true},
		{name: "idle", screen: grokIdleBox},
		{name: "hint above the composer", screen: grokHintAboveBox},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = &familyStaticPane{family: "grok", screen: tc.screen}

			running, err := tm.TurnRunning("lab")
			if err != nil {
				t.Fatalf("TurnRunning err = %v, want nil", err)
			}
			if running != tc.wantRunning {
				t.Errorf("TurnRunning = %t, want %t", running, tc.wantRunning)
			}
		})
	}
}

// The idle wait reads grok's footer too: a working grok pane times out as an
// observed timeout — the caller knows a turn runs, it is not left holding an
// unobserved one — and an idle grok pane settles (AC-3).
func TestWaitForIdleGrokPane(t *testing.T) {
	grokPane := func(screen string) *Tmux {
		tm := NewTmux()
		tm.exec = &paneScriptExecutor{
			captures: []scriptedCapture{{out: screen}},
			env:      map[string]string{"GC_PROVIDER": "grok"},
		}
		return tm
	}
	t.Run("busy", func(t *testing.T) {
		err := grokPane(grokBusyBox).WaitForIdle(context.Background(), "w", 500*time.Millisecond)
		if !errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("WaitForIdle = %v, want ErrIdleTimeout", err)
		}
		if errors.Is(err, runtime.ErrWaitUnobserved) {
			t.Fatalf("WaitForIdle = %v, want the bare timeout: the grok footer showed the session busy", err)
		}
	})
	t.Run("idle", func(t *testing.T) {
		if err := grokPane(grokIdleBox).WaitForIdle(context.Background(), "w", 2*time.Second); err != nil {
			t.Fatalf("WaitForIdle = %v, want nil", err)
		}
	})
	t.Run("hint above the composer", func(t *testing.T) {
		if err := grokPane(grokHintAboveBox).WaitForIdle(context.Background(), "w", 2*time.Second); err != nil {
			t.Fatalf("WaitForIdle = %v, want nil: the hint is transcript text, the pane is idle", err)
		}
	})
}

// grokFixture reads a Grok Build pane capture from testdata/grok.
func grokFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "grok", name))
	if err != nil {
		t.Fatalf("reading grok fixture %s: %v", name, err)
	}
	return strings.TrimRight(string(data), "\n")
}

// A grok pane (GC_PROVIDER=grok) whose footer shows Ctrl+c:cancel below its
// live composer box is running a turn for every leaf reading: SnapshotIdle is
// (false, nil), TurnRunning (true, nil), and WaitForIdle times out having
// seen the session busy. Without the footer the same box is idle. Grok's
// boxed composer stays on screen through a turn, so without the footer
// reading WaitForIdle would report idle mid-turn.
func TestGrokPaneReadsItsFooter(t *testing.T) {
	for _, tc := range []struct {
		name string
		pane string
		busy bool
	}{
		{name: "busy", pane: "busy.txt", busy: true},
		{name: "idle", pane: "idle.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newTmux := func() *Tmux {
				tm := NewTmux()
				tm.exec = &familyStaticPane{family: "grok", screen: grokFixture(t, tc.pane)}
				return tm
			}

			idle, err := newTmux().SnapshotIdle("lab")
			if err != nil || idle == tc.busy {
				t.Errorf("SnapshotIdle = (%t, %v), want (%t, nil)", idle, err, !tc.busy)
			}

			running, err := (&Provider{tm: newTmux()}).TurnRunning("lab")
			if err != nil || running != tc.busy {
				t.Errorf("TurnRunning = (%t, %v), want (%t, nil)", running, err, tc.busy)
			}

			synctest.Test(t, func(t *testing.T) {
				err := newTmux().WaitForIdle(context.Background(), "lab", 2*time.Second)
				switch {
				case tc.busy:
					if !errors.Is(err, ErrIdleTimeout) || errors.Is(err, runtime.ErrWaitUnobserved) {
						t.Errorf("WaitForIdle = %v, want ErrIdleTimeout not wrapping ErrWaitUnobserved", err)
					}
				case err != nil:
					t.Errorf("WaitForIdle = %v, want nil", err)
				}
			})
		})
	}
}
