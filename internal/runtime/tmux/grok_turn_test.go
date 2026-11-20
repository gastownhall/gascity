package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// grokSpinnerShapedRow is a grok transcript row shaped like another
	// family's indicator: a tool's "(2s · 3.1 MB)" reads as Claude's spinner
	// to the shared scan. grokIdleBoxUnderSpinnerShapedRow is an idle grok
	// pane whose transcript carries it.
	grokSpinnerShapedRow             = "  ● bash: make test (2s · 3.1 MB)"
	grokIdleBoxUnderSpinnerShapedRow = grokSpinnerShapedRow + "\n" + grokIdleBox
)

// The busy reading is grok's own footer read relative to its live composer:
// the hint below the composer is a turn in progress, the same hint above it is
// transcript text an earlier turn printed, and the reader is consulted only
// for a grok pane — the hint's text in another family's pane, and every near
// miss in grok's own chrome, is never busy. A grok pane reads that footer
// alone: another family's indicator in its transcript is not its turn, while
// the same row in a claude pane still is claude's.
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
		{name: "another family's indicator in a grok pane is not grok's turn", screen: busyTrustBusyPane, family: "grok"},
		{name: "spinner-shaped transcript row above an idle grok composer", screen: grokIdleBoxUnderSpinnerShapedRow, family: "grok"},
		{name: "spinner-shaped transcript row above a working grok composer", screen: grokSpinnerShapedRow + "\n" + grokBusyBox, family: "grok", want: true},
		{name: "the same spinner-shaped row in a claude pane", screen: grokIdleBoxUnderSpinnerShapedRow, family: "claude", want: true},
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
// as busy, and the same pane without the footer is idle (AC-3). That footer is
// a grok pane's only busy reading: a transcript row shaped like another
// family's indicator keeps neither its idle composer from reading idle nor a
// question dialog from reading as no idle boundary, while the same row in a
// claude pane is claude's turn.
func TestSnapshotIdleGrokFooter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		family   string
		screen   string
		wantIdle bool
		wantErr  error
	}{
		{name: "busy", family: "grok", screen: grokBusyBox},
		{name: "idle", family: "grok", screen: grokIdleBox, wantIdle: true},
		{name: "hint above the composer", family: "grok", screen: grokHintAboveBox, wantIdle: true},
		{name: "spinner-shaped transcript row above an idle composer", family: "grok", screen: grokIdleBoxUnderSpinnerShapedRow, wantIdle: true},
		{name: "spinner-shaped transcript row above a working composer", family: "grok", screen: grokSpinnerShapedRow + "\n" + grokBusyBox},
		{name: "spinner-shaped transcript row above a question dialog", family: "grok", screen: grokSpinnerShapedRow + "\n" + askUserQuestionPane, wantErr: runtime.ErrIdleIndeterminate},
		{name: "the same spinner-shaped row in a claude pane", family: "claude", screen: grokIdleBoxUnderSpinnerShapedRow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = &familyStaticPane{family: tc.family, screen: tc.screen}

			idle, err := tm.SnapshotIdle("lab")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("SnapshotIdle err = %v, want %v", err, tc.wantErr)
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
// unobserved one — and an idle grok pane settles (AC-3), also under a
// transcript row shaped like another family's indicator, which in a claude
// pane is claude's turn.
func TestWaitForIdleGrokPane(t *testing.T) {
	familyPane := func(family, screen string) *Tmux {
		tm := NewTmux()
		tm.exec = &paneScriptExecutor{
			captures: []scriptedCapture{{out: screen}},
			env:      map[string]string{"GC_PROVIDER": family},
		}
		return tm
	}
	grokPane := func(screen string) *Tmux { return familyPane("grok", screen) }
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
	t.Run("spinner-shaped transcript row above the composer", func(t *testing.T) {
		if err := grokPane(grokIdleBoxUnderSpinnerShapedRow).WaitForIdle(context.Background(), "w", 2*time.Second); err != nil {
			t.Fatalf("WaitForIdle = %v, want nil: the row is transcript text, grok's footer is its only busy reading", err)
		}
	})
	t.Run("the same spinner-shaped row in a claude pane", func(t *testing.T) {
		err := familyPane("claude", grokIdleBoxUnderSpinnerShapedRow).WaitForIdle(context.Background(), "w", 500*time.Millisecond)
		if !errors.Is(err, ErrIdleTimeout) || errors.Is(err, runtime.ErrWaitUnobserved) {
			t.Fatalf("WaitForIdle = %v, want ErrIdleTimeout not wrapping ErrWaitUnobserved: the row is claude's spinner", err)
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
// reading WaitForIdle would report idle mid-turn. A working pane whose
// composer has scrolled away still shows the footer, and the footer's text
// in the transcript above an idle composer is not a turn.
func TestGrokPaneReadsItsFooter(t *testing.T) {
	for _, tc := range []struct {
		name string
		pane string
		busy bool
	}{
		{name: "busy", pane: "busy.txt", busy: true},
		{name: "idle", pane: "idle.txt"},
		{name: "busy, composer scrolled away", pane: "busy-no-composer.txt", busy: true},
		{name: "footer text in the transcript", pane: "hint-in-transcript.txt"},
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

// A claude pane carrying grok's footer text — the hint above the ready
// prompt, below it, or as the whole screen — never reads busy: the hint is
// grok's own indicator, and no reader this leaf consults (the busy scan,
// TurnRunning, the verified submit's busy reader) treats it as another
// family's (AC-4).
const (
	claudeIdleWithGrokHintAbove = "● Done.\n" +
		"\n" +
		grokHintRow + "\n" +
		"❯ \n" +
		"  ? for shortcuts"
	claudeIdleWithGrokHintBelow = "● Done.\n" +
		"\n" +
		"❯ \n" +
		grokHintRow + "\n" +
		"  ? for shortcuts"
)

// Guard: grok's hint text alone never produces a busy reading outside a grok
// pane. The grok-side placement rules (the hint below the composer is busy,
// above it is transcript) are pinned by the tests above; this pins the other
// half of the scoping — the reader is consulted only for a grok pane, so the
// same text in a claude pane is inert (AC-4).
func TestGrokHintTextIsNotBusyOutsideGrokPanes(t *testing.T) {
	screens := []struct {
		name   string
		screen string
	}{
		{name: "hint above the prompt", screen: claudeIdleWithGrokHintAbove},
		{name: "hint below the prompt", screen: claudeIdleWithGrokHintBelow},
		{name: "hint with no prompt", screen: grokBusyNoComposer},
	}
	t.Run("paneBusyReading", func(t *testing.T) {
		for _, tc := range screens {
			if paneBusyReading(strings.Split(tc.screen, "\n"), "claude") {
				t.Errorf("%s: paneBusyReading(claude) = true, want false: grok's hint is not claude's indicator", tc.name)
			}
		}
	})
	t.Run("TurnRunning", func(t *testing.T) {
		for _, tc := range screens {
			tm := NewTmux()
			tm.exec = &familyStaticPane{family: "claude", screen: tc.screen}

			running, err := tm.TurnRunning("lab")
			if err != nil {
				t.Fatalf("TurnRunning(%s) err = %v, want nil", tc.name, err)
			}
			if running {
				t.Errorf("%s: TurnRunning = true, want false: the hint text alone is not a claude turn", tc.name)
			}
		}
	})
	t.Run("paneBusyFor", func(t *testing.T) {
		for _, tc := range screens {
			tm := NewTmux()
			tm.exec = &familyStaticPane{family: "claude", screen: tc.screen}

			busy, err := tm.paneBusyFor("lab", "claude")
			if err != nil {
				t.Fatalf("paneBusyFor(%s) err = %v, want nil", tc.name, err)
			}
			if busy {
				t.Errorf("%s: paneBusyFor(claude) = true, want false: the verified submit's busy reader must not fire on grok's hint", tc.name)
			}
		}
	})
}

// The same claude pane stays idle in the snapshot scan: grok's hint text in
// the transcript, or in the status row under the prompt, does not push a
// prompt-visible pane off idle (AC-4).
func TestSnapshotIdleIgnoresGrokHintTextInAClaudePane(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
	}{
		{name: "hint above the prompt", screen: claudeIdleWithGrokHintAbove},
		{name: "hint below the prompt", screen: claudeIdleWithGrokHintBelow},
		{name: "hint in the status row under the prompt", screen: grokFixture(t, "claude-with-hint.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = &familyStaticPane{family: "claude", screen: tc.screen}

			idle, err := tm.SnapshotIdle("lab")
			if err != nil {
				t.Fatalf("SnapshotIdle err = %v, want nil", err)
			}
			if !idle {
				t.Errorf("SnapshotIdle = false, want true: the pane is at its ready prompt")
			}
		})
	}
}

// familyReadCountingPane is familyStaticPane whose first failures GC_PROVIDER
// reads fail (every read when failures is negative); reads counts them all.
type familyReadCountingPane struct {
	familyStaticPane
	failures int

	mu    sync.Mutex
	reads int
}

func (f *familyReadCountingPane) execute(args []string) (string, error) {
	if tmuxSubcommand(args) == "show-environment" && args[len(args)-1] == "GC_PROVIDER" {
		f.mu.Lock()
		f.reads++
		fail := f.failures < 0 || f.reads <= f.failures
		f.mu.Unlock()
		if fail {
			return "", errProviderReadFailed
		}
	}
	return f.familyStaticPane.execute(args)
}

func (f *familyReadCountingPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func (f *familyReadCountingPane) familyReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// SnapshotIdle reads the pane's family only for a capture whose reading the
// family decides: a busy-indicator string, or grok's footer below a composer.
// A capture that reads the same for every family costs no family read.
func TestSnapshotIdleReadsTheFamilyOnlyWhereItDecides(t *testing.T) {
	for _, tc := range []struct {
		name      string
		family    string
		screen    string
		wantReads int
	}{
		{name: "idle claude pane", family: "claude", screen: busyTrustIdlePane},
		{name: "idle grok pane", family: "grok", screen: grokIdleBox},
		{name: "grok footer text in the transcript", family: "grok", screen: grokHintAboveBox},
		{name: "neither prompt nor indicator", family: "claude", screen: busyTrustNeitherPane},
		{name: "busy claude pane", family: "claude", screen: busyTrustBusyPane, wantReads: 1},
		{name: "busy grok pane", family: "grok", screen: grokBusyBox, wantReads: 1},
		{name: "grok footer text under a claude prompt", family: "claude", screen: claudeIdleWithGrokHintBelow, wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := &familyReadCountingPane{familyStaticPane: familyStaticPane{family: tc.family, screen: tc.screen}}
			tm := NewTmux()
			tm.exec = pane

			_, _ = tm.SnapshotIdle("lab")

			if got := pane.familyReads(); got != tc.wantReads {
				t.Errorf("GC_PROVIDER reads = %d, want %d", got, tc.wantReads)
			}
		})
	}
}

// The idle wait never counts an idle reading its family decides while that
// family cannot be read: the shared scan alone reads a working grok pane as
// idle, so settling on it would type a wait-idle nudge into the running turn.
// Each capture that needs the family tries the read again; once it succeeds,
// the wait reads the pane as it is. A busy reading the shared scan takes
// without the family stands, and a capture no family changes settles without
// any read (RUNTIME-CONTRACT-016).
func TestWaitForIdleNeverSettlesOnAReadingItsFamilyDecidesUnread(t *testing.T) {
	newFamilyPane := func(family, screen string, failures int) (*Tmux, *familyReadCountingPane) {
		pane := &familyReadCountingPane{familyStaticPane: familyStaticPane{family: family, screen: screen}, failures: failures}
		tm := NewTmux()
		tm.exec = pane
		return tm, pane
	}
	newPane := func(screen string, failures int) (*Tmux, *familyReadCountingPane) {
		return newFamilyPane("grok", screen, failures)
	}
	t.Run("busy grok pane, family never read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, pane := newPane(grokBusyBox, -1)
			err := tm.WaitForIdle(context.Background(), "lab", 2*time.Second)
			if !errors.Is(err, ErrIdleTimeout) || !errors.Is(err, runtime.ErrWaitUnobserved) {
				t.Fatalf("WaitForIdle = %v, want ErrIdleTimeout wrapping ErrWaitUnobserved: no capture could be read without the family", err)
			}
			if got := pane.familyReads(); got < 2 {
				t.Fatalf("GC_PROVIDER reads = %d, want one per capture that needed it: a failed read is tried again", got)
			}
		})
	})
	t.Run("busy grok pane, family read on the third try", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, pane := newPane(grokBusyBox, 2)
			err := tm.WaitForIdle(context.Background(), "lab", 2*time.Second)
			if !errors.Is(err, ErrIdleTimeout) || errors.Is(err, runtime.ErrWaitUnobserved) {
				t.Fatalf("WaitForIdle = %v, want ErrIdleTimeout not wrapping ErrWaitUnobserved: the footer read busy once the family was read", err)
			}
			if got := pane.familyReads(); got != 3 {
				t.Fatalf("GC_PROVIDER reads = %d, want 3: two failures, then one read kept for the rest of the wait", got)
			}
		})
	})
	t.Run("busy claude pane, family never read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, _ := newFamilyPane("claude", busyTrustBusyPane, -1)
			err := tm.WaitForIdle(context.Background(), "lab", 2*time.Second)
			if !errors.Is(err, ErrIdleTimeout) || errors.Is(err, runtime.ErrWaitUnobserved) {
				t.Fatalf("WaitForIdle = %v, want ErrIdleTimeout not wrapping ErrWaitUnobserved: the shared scan read the session busy", err)
			}
		})
	})
	t.Run("idle grok pane, family never read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, pane := newPane(grokIdleBox, -1)
			if err := tm.WaitForIdle(context.Background(), "lab", 2*time.Second); err != nil {
				t.Fatalf("WaitForIdle = %v, want nil: an idle composer with no footer reads the same for every family", err)
			}
			if got := pane.familyReads(); got != 0 {
				t.Fatalf("GC_PROVIDER reads = %d, want 0", got)
			}
		})
	})
}
