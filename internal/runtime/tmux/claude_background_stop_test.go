package tmux

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Pane rows from Claude Code 2.1.296 in the GC tmux runtime (80 columns),
// taken live: a Claude session asked to start `sleep 90 && touch <file>` with
// run_in_background and then run `sleep 40` in the foreground, stopped through
// POST /v0/city/{city}/session/{id}/stop. The interrupt ended the turn while
// the background shells kept running. Rows are verbatim with their escape
// sequences removed; the transcript row is the live one.
const (
	claudeInterruptedRow   = "  ⎿  Interrupted · What should Claude do instead?"
	claudeRule             = "────────────────────────────────────────────────────────────────────────────────"
	claudeBusyRow          = "✽ Puttering… (5s · ↓ 398 tokens · thinking)"
	claudeShellsFooter     = "  ⏵⏵ bypass permissions on · %d %s · ← for agents · ↓ to manage"
	claudeBusyShellsFooter = "  ⏵⏵ bypass permissions on · %d %s · esc to interrupt · ← for agents · ↓ t…"
	claudeSelectedFooter   = "  ⏵⏵ bypass permissions on · %d %s · Enter to view tasks"
	claudeIdleFooter       = "  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents"
	claudeListHint         = "  ↑/↓ to select · Enter to view · x to stop · Esc to close"
	claudeEmptyListHint    = "  ↑/↓ to select · Enter to view · Esc to close"
	claudeDetailsHint      = "  ← to go back · Esc/Enter/Space to close · x to stop"
)

func shellsWord(n int) string {
	if n == 1 {
		return "shell"
	}
	return "shells"
}

// fakeClaudeTaskPane is a tmux executor that plays an interrupted Claude Code
// 2.1.296 pane with background shells running, and its background-task view:
// Down on an empty input box selects the footer's shells entry, Enter then
// opens the view (the one shell's details, or the list), x stops the shell
// under the cursor (and closes the details view), and Escape closes the view.
// Any other key reaches the input box.
type fakeClaudeTaskPane struct {
	mu sync.Mutex

	provider string
	// busyCaptures is how many captures still show the turn running.
	busyCaptures int
	// shells is how many background shells are running.
	shells int
	// draft is the text in the input box.
	draft string
	// dropX is how many x presses the view drops before it takes one.
	dropX int
	// ignoreX makes the view take no x at all.
	ignoreX bool
	// noTaskView makes Down select nothing.
	noTaskView bool
	// longCommand makes the details view taller than the pane, as a command
	// that wraps to a second row did live: its key hints are cut off.
	longCommand bool

	selected bool
	view     string // "", "list" or "details"
	captures int
	// events is every key ("key:<key>") and literal text ("type:<text>")
	// that reached the pane, and every pause the runtime took
	// ("sleep:<duration>"), in order.
	events []string
	// stray is every key that reached the input box instead of the view.
	stray []string
}

func newFakeClaudeTaskPane(shells int) *fakeClaudeTaskPane {
	return &fakeClaudeTaskPane{provider: "claude", shells: shells}
}

func (f *fakeClaudeTaskPane) render() []string {
	lines := []string{"  Ran 3 shell commands", claudeInterruptedRow, ""}
	busy := f.captures <= f.busyCaptures
	if busy {
		lines = append(lines, claudeBusyRow)
	}
	switch f.view {
	case "list":
		lines = append(lines, claudeRule, "  Background")
		if f.shells == 0 {
			return append(lines, "  No tasks currently running", "", claudeEmptyListHint)
		}
		lines = append(lines, fmt.Sprintf("  %d active %s", f.shells, shellsWord(f.shells)), "")
		for i := f.shells - 1; i >= 0; i-- {
			cursor := "  "
			if i == f.shells-1 {
				cursor = "❯ "
			}
			lines = append(lines, fmt.Sprintf("  %s● sleep 90 && touch /tmp/cbg-%d   running", cursor, i))
		}
		return append(lines, claudeListHint)
	case "details":
		if f.longCommand {
			return append(lines, claudeRule, "  Shell details", "", "  Status:   running", "  Runtime:  2s",
				"  Command:  sleep 25 && printf finished > /home/tester/staging/qualification",
				"            /lifecycle-claude-interrupt-e6e8ad4f/artifact.txt", "", "  Output:", "  No output available")
		}
		return append(lines, claudeRule, "  Shell details", "", "  Status:   running", "  Runtime:  6s",
			"  Command:  sleep 90 && touch /tmp/cbg-0", "", "  Output:", "  No output available", "", claudeDetailsHint)
	}
	lines = append(lines, claudeRule, "❯ "+f.draft, claudeRule)
	switch {
	case f.shells > 0 && busy:
		return append(lines, fmt.Sprintf(claudeBusyShellsFooter, f.shells, shellsWord(f.shells)))
	case f.shells > 0 && f.selected:
		return append(lines, fmt.Sprintf(claudeSelectedFooter, f.shells, shellsWord(f.shells)))
	case f.shells > 0:
		return append(lines, fmt.Sprintf(claudeShellsFooter, f.shells, shellsWord(f.shells)))
	}
	return append(lines, claudeIdleFooter)
}

func (f *fakeClaudeTaskPane) execute(args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	switch tmuxSubcommand(args) {
	case "show-environment":
		if slices.Contains(args, "GC_PROVIDER") {
			return "GC_PROVIDER=" + f.provider, nil
		}
		return "", fmt.Errorf("unknown variable")
	case "display-message":
		if slices.Contains(args, "#{pane_in_mode}") {
			return "0", nil
		}
		return "", nil
	case "capture-pane":
		f.captures++
		return strings.Join(f.render(), "\n"), nil
	case "send-keys":
		f.sendKeys(args)
	}
	return "", nil
}

func (f *fakeClaudeTaskPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func (f *fakeClaudeTaskPane) sendKeys(args []string) {
	i := slices.Index(args, "send-keys")
	literal := false
	var keys []string
	for i++; i < len(args); i++ {
		switch args[i] {
		case "-t":
			i++
		case "-l":
			literal = true
		case "-X":
			return // copy-mode command, not input
		default:
			keys = append(keys, args[i])
		}
	}
	if literal {
		text := strings.Join(keys, " ")
		f.events = append(f.events, "type:"+text)
		f.stray = append(f.stray, text)
		f.draft += text
		return
	}
	for _, key := range keys {
		f.events = append(f.events, "key:"+key)
		f.press(key)
	}
}

func (f *fakeClaudeTaskPane) press(key string) {
	if f.view != "" {
		switch key {
		case "x":
			if f.ignoreX || f.shells == 0 {
				return
			}
			if f.dropX > 0 {
				f.dropX--
				return
			}
			f.shells--
			if f.view == "details" {
				f.view, f.selected = "", false
			}
		case "Escape":
			f.view, f.selected = "", false
		}
		return
	}
	switch {
	case key == "Down" && f.draft == "" && f.shells > 0 && !f.noTaskView:
		f.selected = true
	case key == "Enter" && f.selected:
		f.view = "list"
		if f.shells == 1 {
			f.view = "details"
		}
	default:
		f.stray = append(f.stray, key)
	}
}

func (f *fakeClaudeTaskPane) sleep(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "sleep:"+d.String())
	return nil
}

// keys returns what was pressed or typed, without the pauses.
func (f *fakeClaudeTaskPane) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var got []string
	for _, event := range f.events {
		if !strings.HasPrefix(event, "sleep:") {
			got = append(got, event)
		}
	}
	return got
}

func newClaudeStopTmux(pane *fakeClaudeTaskPane) *Tmux {
	tm := NewTmux()
	tm.exec = pane
	tm.backgroundStopSleep = pane.sleep
	return tm
}

// TestClaudeStopBackgroundWorkStopsBackgroundShells is the live defect: the
// stop ended the Claude turn, but the commands it had started in the
// background kept running, and a completion notice started another turn.
// StopBackgroundWork opens Claude's background-task view from the idle input
// box, stops each listed shell, and closes the view.
func TestClaudeStopBackgroundWorkStopsBackgroundShells(t *testing.T) {
	pane := newFakeClaudeTaskPane(2)
	tm := newClaudeStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "claude-bg-stop-a"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got, want := pane.keys(), []string{"key:Down", "key:Enter", "key:x", "key:x", "key:Escape"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the interrupted Claude pane = %q, want %q: the interrupt left its background shells running", got, want)
	}
	if pane.shells != 0 || pane.view != "" || len(pane.stray) != 0 {
		t.Fatalf("after the stop: shells %d, view %q, keys in the input box %q; want 0, closed, none", pane.shells, pane.view, pane.stray)
	}
}

// TestClaudeStopBackgroundWorkStopsTheOneShellFromItsDetails: with one shell
// Enter opens its details, and x there stops it and closes the view, so no
// Escape follows.
func TestClaudeStopBackgroundWorkStopsTheOneShellFromItsDetails(t *testing.T) {
	pane := newFakeClaudeTaskPane(1)
	tm := newClaudeStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "claude-bg-stop-b"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got, want := pane.keys(), []string{"key:Down", "key:Enter", "key:x"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Claude pane = %q, want %q", got, want)
	}
	if pane.shells != 0 || pane.view != "" || len(pane.stray) != 0 {
		t.Fatalf("after the stop: shells %d, view %q, keys in the input box %q; want 0, closed, none", pane.shells, pane.view, pane.stray)
	}
}

// TestClaudeStopBackgroundWorkStopsAShellWhoseDetailsOverflowThePane is the
// live failure of the first version: in GC's 24-row pane the details of a
// command that wrapped to two rows pushed the view's key hints off screen,
// the view was read as not open, and the shell kept running. The view is
// read from its heading.
func TestClaudeStopBackgroundWorkStopsAShellWhoseDetailsOverflowThePane(t *testing.T) {
	pane := newFakeClaudeTaskPane(1)
	pane.longCommand = true
	tm := newClaudeStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "claude-bg-stop-i"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got, want := pane.keys(), []string{"key:Down", "key:Enter", "key:x"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Claude pane = %q, want %q", got, want)
	}
	if pane.shells != 0 || pane.view != "" || len(pane.stray) != 0 {
		t.Fatalf("after the stop: shells %d, view %q, keys in the input box %q; want 0, closed, none", pane.shells, pane.view, pane.stray)
	}
}

// TestClaudeStopBackgroundWorkPressesAgainForADroppedStop: live, the view did
// not take an x that arrived while it redrew after the previous stop. A shell
// still listed after the settle gets another x.
func TestClaudeStopBackgroundWorkPressesAgainForADroppedStop(t *testing.T) {
	pane := newFakeClaudeTaskPane(2)
	pane.dropX = 1
	tm := newClaudeStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "claude-bg-stop-c"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got, want := pane.keys(), []string{"key:Down", "key:Enter", "key:x", "key:x", "key:x", "key:Escape"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Claude pane = %q, want %q", got, want)
	}
	if pane.shells != 0 || len(pane.stray) != 0 {
		t.Fatalf("after the stop: shells %d, keys in the input box %q; want 0, none", pane.shells, pane.stray)
	}
}

// TestClaudeStopBackgroundWorkSendsNothingWithoutBackgroundShells: an
// interrupted turn that left no background shell running gets no keys.
func TestClaudeStopBackgroundWorkSendsNothingWithoutBackgroundShells(t *testing.T) {
	pane := newFakeClaudeTaskPane(0)
	tm := newClaudeStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "claude-bg-stop-d"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got := pane.keys(); len(got) != 0 {
		t.Fatalf("keys reaching a Claude pane with no background shell = %q, want none", got)
	}
}

// TestClaudeStopBackgroundWorkLeavesADraftAlone: Down in an input box that
// holds text moves through the text instead of selecting the footer, so a
// draft gets no keys and the background shells keep running.
func TestClaudeStopBackgroundWorkLeavesADraftAlone(t *testing.T) {
	pane := newFakeClaudeTaskPane(1)
	pane.draft = "a note someone is typing"
	tm := newClaudeStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "claude-bg-stop-e"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got := pane.keys(); len(got) != 0 {
		t.Fatalf("keys reaching a Claude input box holding a draft = %q, want none", got)
	}
}

// TestClaudeStopBackgroundWorkWaitsOutTheBusyTurn: the footer reports the
// shells while the turn still runs, but the view is opened only once the pane
// reads idle.
func TestClaudeStopBackgroundWorkWaitsOutTheBusyTurn(t *testing.T) {
	pane := newFakeClaudeTaskPane(1)
	pane.busyCaptures = 2
	tm := newClaudeStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "claude-bg-stop-f"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got, want := pane.keys(), []string{"key:Down", "key:Enter", "key:x"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Claude pane = %q, want %q", got, want)
	}
	if pane.captures <= pane.busyCaptures+1 {
		t.Fatalf("captures = %d, want the busy captures waited out first", pane.captures)
	}
}

// TestClaudeStopBackgroundWorkSendsNoStopWithoutTheTaskView: when Down does
// not select the footer's shells entry, no Enter or x follows: either would
// reach the input box.
func TestClaudeStopBackgroundWorkSendsNoStopWithoutTheTaskView(t *testing.T) {
	pane := newFakeClaudeTaskPane(2)
	pane.noTaskView = true
	tm := newClaudeStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "claude-bg-stop-g"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got, want := pane.keys(), []string{"key:Down"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Claude pane = %q, want %q", got, want)
	}
	if pane.shells != 2 {
		t.Fatalf("shells = %d, want the fake's 2 still running", pane.shells)
	}
}

// TestClaudeStopBackgroundWorkBoundsTheStopsOfShellsThatKeepRunning: a view
// that takes no x gets a bounded number of them, is closed, and the stop
// still succeeds (the turn is over; the runtime logs what is left).
func TestClaudeStopBackgroundWorkBoundsTheStopsOfShellsThatKeepRunning(t *testing.T) {
	pane := newFakeClaudeTaskPane(2)
	pane.ignoreX = true
	tm := newClaudeStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "claude-bg-stop-h"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	keys := pane.keys()
	if got, want := slices.Index(keys, "key:Escape"), len(keys)-1; got != want {
		t.Fatalf("keys = %q, want the view closed last", keys)
	}
	if got, want := strings.Count(strings.Join(keys, " "), "key:x"), 2+claudeTaskStopExtraPresses; got != want {
		t.Fatalf("x presses = %d, want %d (the shells plus the bound)", got, want)
	}
	if pane.shells != 2 || pane.view != "" || len(pane.stray) != 0 {
		t.Fatalf("after the stop: shells %d, view %q, keys in the input box %q; want 2, closed, none", pane.shells, pane.view, pane.stray)
	}
}

// TestClaudePaneReportsBackgroundShells pins where the count is read: in the
// footer under the live input box, not in the transcript.
func TestClaudePaneReportsBackgroundShells(t *testing.T) {
	box := []string{claudeRule, "❯ ", claudeRule}
	withFooter := func(footer string) []string {
		return append(append([]string{"  Ran 3 shell commands", claudeInterruptedRow, ""}, box...), footer)
	}
	for _, tc := range []struct {
		name     string
		lines    []string
		shells   int
		readable bool
	}{
		{"live interrupted pane", withFooter(fmt.Sprintf(claudeShellsFooter, 2, "shells")), 2, true},
		{"one shell", withFooter(fmt.Sprintf(claudeShellsFooter, 1, "shell")), 1, true},
		{"entry selected", withFooter(fmt.Sprintf(claudeSelectedFooter, 2, "shells")), 2, true},
		{"busy footer cut to the pane width", withFooter(fmt.Sprintf(claudeBusyShellsFooter, 1, "shell")), 1, true},
		{"no permission mode before the entry", withFooter("  3 shells · ? for shortcuts"), 3, true},
		{"after the shells stopped", withFooter(claudeIdleFooter), 0, true},
		{"transcript mentions shells", append([]string{"● 2 shells · still running, as you asked"}, withFooter(claudeIdleFooter)...), 0, true},
		{"no input box on screen", []string{"  Ran 3 shell commands", claudeInterruptedRow}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shells, readable := claudePaneReportsBackgroundShells(tc.lines)
			if shells != tc.shells || readable != tc.readable {
				t.Fatalf("claudePaneReportsBackgroundShells = (%d, %t), want (%d, %t)", shells, readable, tc.shells, tc.readable)
			}
		})
	}
}

// TestReadClaudeTaskView pins the view's reading: open only while its heading
// is below any input box, x offered when the hints say so or are cut off
// while a shell runs, and the running count from the list heading or the
// details' status.
func TestReadClaudeTaskView(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shell int
		view  string
		long  bool
		want  claudeTaskView
	}{
		{"list of two", 2, "list", false, claudeTaskView{open: true, canStop: true, running: 2}},
		{"details of one", 1, "details", false, claudeTaskView{open: true, canStop: true, running: 1}},
		{"details with the hints below the pane", 1, "details", true, claudeTaskView{open: true, canStop: true, running: 1}},
		{"list emptied", 0, "list", false, claudeTaskView{open: true}},
		{"closed", 2, "", false, claudeTaskView{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := newFakeClaudeTaskPane(tc.shell)
			pane.captures = 1
			pane.view = tc.view
			pane.longCommand = tc.long
			if got := readClaudeTaskView(pane.render()); got != tc.want {
				t.Fatalf("readClaudeTaskView = %+v, want %+v", got, tc.want)
			}
		})
	}
}
