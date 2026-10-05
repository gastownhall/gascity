package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Pane captures from Claude Code 2.1.288 in an 80-column tmux pane, taken in
// the lost-turn repro (a /stop that lands before Claude's first response
// chunk). Claude puts the interrupted prompt back into the input box, here
// three wrapped rows. The live input row is "❯" plus NBSP; transcript rows
// above it use a plain space.
var (
	// The interrupted prompt A, restored into the input box after /stop.
	claudeRestoredDraftPane = []string{
		"  tools.",
		"⏺ BANANA t0",
		"✻ Brewed for 20s · done 10:22 AM",
		strings.Repeat("─", 80),
		"❯ d1.0-A: write a detailed essay of about 400 words on the history of",
		"  lighthouses. Do not use any tools. Start your answer with the words ESSAY",
		"  d1.0.",
		strings.Repeat("─", 80),
		"  ⏵⏵ don't ask on (shift+tab to cycle)",
	}
	// The same draft text the restored rows hold, as Claude stores it.
	claudeRestoredDraft = "d1.0-A: write a detailed essay of about 400 words on the history of " +
		"lighthouses. Do not use any tools. Start your answer with the words ESSAY d1.0."

	// After the next submit cleared one row and pasted B behind the rest: the
	// merged draft sits unsent in the input box (trial d3.0).
	claudeMergedUnsentPane = []string{
		"⏺ BANANA d2.0",
		"✻ Crunched for 16s · done 10:26 AM",
		strings.Repeat("─", 80),
		"❯ d3.0-A: write a detailed essay of about 400 words on the history of",
		"  lighthouses. Do not use any tools. Start your answer with the words ESSAY",
		"  d3.0.d3.0-B: reply with exactly the words BANANA d3.0 and nothing else. Do",
		"  not use any tools.",
		strings.Repeat("─", 80),
		"  ⏵⏵ don't ask on (shift+tab to cycle)",
	}
	claudeMergedUnsentB = "d3.0-B: reply with exactly the words BANANA d3.0 and nothing else. Do not use any tools."

	// Right after /stop returned, before the prompt was restored.
	claudeEmptyAfterStopPane = []string{
		"❯ t0-B: reply with exactly the words BANANA t0 and nothing else. Do not use any",
		"  tools.",
		"⏺ BANANA t0",
		"✻ Brewed for 20s · done 10:22 AM",
		"                Removed 1 invisible character · review and press Enter to send",
		strings.Repeat("─", 80),
		"❯ ",
		strings.Repeat("─", 80),
		"  ⏵⏵ don't ask on (shift+tab to cycle) · ← for agents",
	}

	// composer-keys.sh: a typed three-row draft, then the input box after each
	// of three Ctrl-U presses. Each Ctrl-U removes one wrapped row.
	claudeCtrlUSequence = [][]string{
		{
			"❯ EXP alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi",
			"  omicron pi rho sigma tau upsilon phi chi psi omega one two three four five",
			"  six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen END",
		},
		{
			"❯ EXP alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi",
			"  omicron pi rho sigma tau upsilon phi chi psi omega one two three four five",
		},
		{
			"❯ EXP alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi",
		},
		{
			"❯ ",
		},
	}
	// keys.sh on Claude Code 2.1.289: the same draft with the cursor moved to
	// its start, then the input box after each Ctrl-U+Ctrl-K pair sent in one
	// send-keys. A Ctrl-U alone there deletes nothing; Ctrl-K deletes the rest
	// of the cursor's wrapped row.
	claudeCtrlUCtrlKFromStartSequence = [][]string{
		{
			"❯  omicron pi rho sigma tau upsilon phi chi psi omega one two three four five",
			"  six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen END",
		},
		{
			"❯ six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen END",
		},
		{
			"❯ ",
		},
	}
	claudeCtrlUDraft = "EXP alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi " +
		"omicron pi rho sigma tau upsilon phi chi psi omega one two three four five " +
		"six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen END"
)

// fakeClaudePane is a tmux executor that plays a Claude Code 2.1.288 pane: an
// 80-column transcript above an input box framed by rules. It models what the
// repro measured: the input box word-wraps at 77 columns, Ctrl-U deletes from
// the cursor back to the start of the cursor's wrapped row (so one Ctrl-U at
// the end of a draft removes only the last row, and a Ctrl-U with the cursor at
// the start of the draft removes nothing), Ctrl-K deletes from the cursor to
// the end of its wrapped row, Enter sends the whole draft as one message, and
// a Ctrl-C on an empty input box arms "press Ctrl-C again to exit".
type fakeClaudePane struct {
	mu sync.Mutex

	header   []string
	draft    string
	attached bool
	busy     bool
	// cursor is the byte offset of the input cursor in draft; draftEnd
	// (the default) keeps it at the end, where typing leaves it.
	cursor int
	// attachProbeErr makes the attachment probe fail, so whether a human is
	// at the pane cannot be told.
	attachProbeErr error
	// attachOnCapture attaches a client on the attachOnCapture'th
	// capture-pane (1-based), the way a human may attach mid-clear.
	attachOnCapture int

	// restoreDraft appears in the input box on the restoreOnCapture'th
	// capture-pane (1-based), the way Claude restores an interrupted prompt
	// after /stop has already settled.
	restoreDraft     string
	restoreOnCapture int
	// ignoreCtrlU makes Ctrl-U and Ctrl-K no-ops (an input box that will
	// not clear).
	ignoreCtrlU bool
	// placeholder is drawn dim in an empty input box, as Claude draws
	// "Try \"write a test for <filepath>\"" at startup.
	placeholder string

	captures    int
	keys        []string // every non-literal send-keys key, in order
	submitted   []string // every message Enter sent
	exitArmed   bool     // Ctrl-C landed on an empty input box
	sessionName string
}

const fakeClaudeInputWidth = 77

// draftEnd is fakeClaudePane.cursor for "at the end of the draft".
const draftEnd = -1

func newFakeClaudePane(draft string) *fakeClaudePane {
	return &fakeClaudePane{
		header:      append([]string(nil), claudeRestoredDraftPane[:3]...),
		draft:       draft,
		cursor:      draftEnd,
		sessionName: "lab",
	}
}

// cur returns the cursor's byte offset in the draft.
func (f *fakeClaudePane) cur() int {
	if f.cursor < 0 || f.cursor > len(f.draft) {
		return len(f.draft)
	}
	return f.cursor
}

// cursorRow returns the start and end offsets of the wrapped row holding the
// cursor. A cursor at a row start belongs to that row.
func (f *fakeClaudePane) cursorRow() (start, end int) {
	starts := f.rowStarts()
	c := f.cur()
	r := 0
	for i, st := range starts {
		if st <= c {
			r = i
		}
	}
	end = len(f.draft)
	if r+1 < len(starts) {
		end = starts[r+1]
	}
	return starts[r], end
}

// setCursor stores c, keeping "at the end" sticky so later typing appends.
func (f *fakeClaudePane) setCursor(c int) {
	if c >= len(f.draft) {
		f.cursor = draftEnd
		return
	}
	f.cursor = c
}

// rowStarts word-wraps the draft the way the repro's captures show it and
// returns the byte offset where each row starts. The space at a wrap point
// stays at the end of the row before it.
func (f *fakeClaudePane) rowStarts() []int {
	if f.draft == "" {
		return []int{0}
	}
	starts := []int{0}
	rowLen := 0
	for i := 0; i < len(f.draft); {
		next := strings.IndexByte(f.draft[i:], ' ')
		word := f.draft[i:]
		if next >= 0 {
			word = f.draft[i : i+next]
		}
		if rowLen > 0 && rowLen+1+len(word) > fakeClaudeInputWidth {
			starts = append(starts, i)
			rowLen = 0
		}
		if rowLen > 0 {
			rowLen++
		}
		rowLen += len(word)
		if next < 0 {
			break
		}
		i += next + 1
	}
	return starts
}

func (f *fakeClaudePane) inputRows() []string {
	starts := f.rowStarts()
	rows := make([]string, len(starts))
	for i, start := range starts {
		end := len(f.draft)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		rows[i] = strings.TrimRight(f.draft[start:end], " ")
	}
	return rows
}

func (f *fakeClaudePane) render() []string {
	return f.renderStyled(false)
}

// renderStyled renders the pane; escapes adds the SGR sequences
// capture-pane -e would include around the dim placeholder.
func (f *fakeClaudePane) renderStyled(escapes bool) []string {
	lines := append([]string(nil), f.header...)
	lines = append(lines, strings.Repeat("─", 80))
	for i, row := range f.inputRows() {
		if i == 0 {
			if f.draft == "" && f.placeholder != "" {
				if escapes {
					row = "\x1b[2m" + f.placeholder + "\x1b[0m"
				} else {
					row = f.placeholder
				}
			}
			lines = append(lines, "❯ "+row)
			continue
		}
		lines = append(lines, "  "+row)
	}
	lines = append(lines, strings.Repeat("─", 80))
	footer := "  ⏵⏵ don't ask on (shift+tab to cycle)"
	if f.busy {
		footer += " · esc to interrupt"
	}
	return append(lines, footer)
}

func (f *fakeClaudePane) ctrlU() {
	if f.ignoreCtrlU {
		return
	}
	start, _ := f.cursorRow()
	c := f.cur()
	f.draft = f.draft[:start] + f.draft[c:]
	f.setCursor(start)
}

func (f *fakeClaudePane) ctrlK() {
	if f.ignoreCtrlU {
		return
	}
	_, end := f.cursorRow()
	c := f.cur()
	f.draft = f.draft[:c] + f.draft[end:]
	f.setCursor(c)
}

func (f *fakeClaudePane) execute(args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Drop the global flags run() adds (-u, -L <socket>).
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if args[0] == "-L" {
			args = args[1:]
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "list-panes":
		return "%1\tclaude\t4242", nil
	case "display-message":
		if slices.Contains(args, "#{session_name}|#{session_attached}") {
			if f.attachProbeErr != nil {
				return "", f.attachProbeErr
			}
			if f.attached {
				return f.sessionName + "|1", nil
			}
			return f.sessionName + "|0", nil
		}
		return "", nil
	case "show-environment":
		if slices.Contains(args, "GC_PROVIDER") {
			return "GC_PROVIDER=claude", nil
		}
		return "", errors.New("unknown variable")
	case "capture-pane":
		f.captures++
		if f.attachOnCapture > 0 && f.captures >= f.attachOnCapture {
			f.attached = true
		}
		if f.restoreDraft != "" && f.captures >= f.restoreOnCapture {
			f.draft, f.restoreDraft = f.restoreDraft, ""
			f.cursor = draftEnd
		}
		return strings.TrimSpace(strings.Join(f.renderStyled(slices.Contains(args, "-e")), "\n")), nil
	case "send-keys":
		f.sendKeys(args[1:])
		return "", nil
	default:
		return "", nil
	}
}

func (f *fakeClaudePane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func (f *fakeClaudePane) sendKeys(args []string) {
	literal := false
	var keys []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t":
			i++
		case "-l":
			literal = true
		default:
			keys = append(keys, args[i])
		}
	}
	if literal {
		text := strings.Join(keys, " ")
		c := f.cur()
		f.draft = f.draft[:c] + text + f.draft[c:]
		f.setCursor(c + len(text))
		return
	}
	for _, key := range keys {
		f.keys = append(f.keys, key)
		switch key {
		case "C-u":
			f.ctrlU()
		case "C-k":
			f.ctrlK()
		case "C-c":
			if f.draft == "" {
				f.exitArmed = true
			}
			f.draft = ""
			f.cursor = draftEnd
		case "Enter":
			if f.draft != "" {
				f.submitted = append(f.submitted, f.draft)
				f.draft = ""
				f.cursor = draftEnd
				f.busy = true
			}
		}
	}
}

func (f *fakeClaudePane) keyCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, k := range f.keys {
		if k == key {
			n++
		}
	}
	return n
}

func newFakeClaudeTmux(pane *fakeClaudePane) *Tmux {
	tm := NewTmux()
	tm.exec = pane
	return tm
}

// The fake must reproduce the real captures, or the tests below prove
// nothing about Claude Code.
func TestFakeClaudePaneMatchesRealCaptures(t *testing.T) {
	pane := newFakeClaudePane(claudeRestoredDraft)
	if got := pane.render(); !slices.Equal(got, claudeRestoredDraftPane) {
		t.Fatalf("restored draft renders as\n%s\nwant the real capture\n%s", strings.Join(got, "\n"), strings.Join(claudeRestoredDraftPane, "\n"))
	}

	pane = newFakeClaudePane(claudeCtrlUDraft)
	for i, want := range claudeCtrlUSequence {
		if i > 0 {
			pane.ctrlU()
		}
		got := pane.render()
		got = got[len(pane.header)+1 : len(got)-2]
		if !slices.Equal(got, want) {
			t.Fatalf("after %d Ctrl-U the input box reads\n%s\nwant the real capture\n%s", i, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// The fake must also clear a draft whose cursor is not at its end the way
// Claude Code 2.1.289 does: Ctrl-U alone stalls at the start of the draft, and
// each Ctrl-U+Ctrl-K pair removes one wrapped row (rows compared with spaces
// collapsed: the real input box kept the space at the first wrap point).
func TestFakeClaudePaneMatchesRealCursorCaptures(t *testing.T) {
	pane := newFakeClaudePane(claudeCtrlUDraft)
	pane.cursor = 0
	pane.ctrlU()
	if pane.draft != claudeCtrlUDraft {
		t.Fatalf("Ctrl-U at the start of the draft left %q, want the draft unchanged", pane.draft)
	}
	for i, want := range claudeCtrlUCtrlKFromStartSequence {
		pane.ctrlU()
		pane.ctrlK()
		got := pane.render()
		got = got[len(pane.header)+1 : len(got)-2]
		if !slices.EqualFunc(got, want, func(a, b string) bool { return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ") }) {
			t.Fatalf("after %d Ctrl-U+Ctrl-K the input box reads\n%s\nwant the real capture\n%s", i+1, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// The lost-turn bug: the next submit cleared the restored draft with one
// Ctrl-U, which in Claude Code 2.1.288 removes one wrapped row, so the new
// message was pasted behind the rest of the old prompt and both went out as
// one message. The submit must clear the whole draft first.
func TestNudgeSessionClearsWholeRestoredClaudeDraft(t *testing.T) {
	pane := newFakeClaudePane(claudeRestoredDraft)
	tm := newFakeClaudeTmux(pane)
	const b = "d1.0-B: reply with exactly the words BANANA d1.0 and nothing else."

	if err := tm.NudgeSession(pane.sessionName, b); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	if !slices.Equal(pane.submitted, []string{b}) {
		t.Fatalf("Claude received %q, want only %q", pane.submitted, b)
	}
	if pane.exitArmed || pane.keyCount("C-c") != 0 {
		t.Fatalf("keys = %q, want no Ctrl-C", pane.keys)
	}
}

// A client is attached: a human may be typing, so the submit still must not
// touch the input box (#5192).
func TestNudgeSessionLeavesAttachedClaudeDraftAlone(t *testing.T) {
	pane := newFakeClaudePane("human typing")
	pane.attached = true
	tm := newFakeClaudeTmux(pane)

	_ = tm.NudgeSession(pane.sessionName, "nudge")

	if n := pane.keyCount("C-u"); n != 0 {
		t.Fatalf("sent %d Ctrl-U to an attached session, want none (keys %q)", n, pane.keys)
	}
}

// A stop leaves the input box empty even when Claude restores the interrupted
// prompt after the interrupt has settled.
func TestClearInputClearsDraftRestoredAfterStop(t *testing.T) {
	pane := newFakeClaudePane("")
	pane.header = append([]string(nil), claudeEmptyAfterStopPane[:5]...)
	pane.restoreDraft = claudeRestoredDraft
	pane.restoreOnCapture = 3
	tm := newFakeClaudeTmux(pane)

	if err := tm.ClearInput(context.Background(), pane.sessionName, 2*time.Second); err != nil {
		t.Fatalf("ClearInput: %v", err)
	}

	if pane.restoreDraft != "" {
		t.Fatal("ClearInput returned before the restored draft appeared")
	}
	if pane.draft != "" {
		t.Fatalf("input box still holds %q", pane.draft)
	}
	if n := pane.keyCount("C-c"); n != 0 || pane.exitArmed {
		t.Fatalf("keys = %q, want Ctrl-U only", pane.keys)
	}
}

// Nothing to clear: no keys at all. In particular no Ctrl-C, which on an empty
// input box arms "press Ctrl-C again to exit".
func TestClearInputSendsNoKeysToEmptyInput(t *testing.T) {
	pane := newFakeClaudePane("")
	tm := newFakeClaudeTmux(pane)

	if err := tm.ClearInput(context.Background(), pane.sessionName, 300*time.Millisecond); err != nil {
		t.Fatalf("ClearInput: %v", err)
	}

	if len(pane.keys) != 0 {
		t.Fatalf("keys = %q, want none for an empty input box", pane.keys)
	}
}

// A draft that will not clear is an error, not a silent success: the next
// message would be merged into it.
func TestClearInputFailsLoudlyWhenDraftWillNotClear(t *testing.T) {
	pane := newFakeClaudePane(claudeRestoredDraft)
	pane.ignoreCtrlU = true
	tm := newFakeClaudeTmux(pane)

	err := tm.ClearInput(context.Background(), pane.sessionName, 0)
	if err == nil {
		t.Fatal("ClearInput = nil, want an error for an input box that will not clear")
	}
	if !strings.Contains(err.Error(), "d1.0-A") {
		t.Fatalf("error %q does not name the draft left in the input box", err)
	}
	if n := pane.keyCount("C-u"); n == 0 || n > 8 {
		t.Fatalf("sent %d Ctrl-U, want a few and a bounded number", n)
	}
	if pane.keyCount("C-c") != 0 {
		t.Fatalf("keys = %q, want no Ctrl-C", pane.keys)
	}
}

// The merged draft from trial d3.0 starts with the old prompt, not with the
// message just sent. It is still sitting unsent, so the input box has not
// drained and the delivery must not be reported as proven.
func TestPaneShowsDrainedComposerRejectsMergedClaudeDraft(t *testing.T) {
	if paneShowsDrainedComposer(claudeMergedUnsentPane) {
		t.Fatalf("paneShowsDrainedComposer = true for a merged draft still in the input box (sent %q), want false", claudeMergedUnsentB)
	}
	if paneShowsDrainedComposer(claudeRestoredDraftPane) {
		t.Fatal("paneShowsDrainedComposer = true for an input box holding another draft, want false")
	}
	if !paneShowsDrainedComposer(claudeEmptyAfterStopPane) {
		t.Fatal("paneShowsDrainedComposer = false for an empty input box, want true")
	}
	// A draft whose first row is blank still counts.
	multiline := append(append([]string(nil), claudeRestoredDraftPane[:4]...), "❯ ", "  second row", strings.Repeat("─", 80))
	if paneShowsDrainedComposer(multiline) {
		t.Fatal("paneShowsDrainedComposer = true for a draft with a blank first row, want false")
	}
}

// Claude Code draws its placeholder dim, and a capture taken with escapes
// (capture-pane -e) is how gc tells it from a draft. Captured from Claude Code
// 2.1.288 at startup, then after typing.
func TestClaudeInputIgnoresDimPlaceholder(t *testing.T) {
	placeholder := "\x1b[39m❯ \x1b[2mTry \"write a test for <filepath>\"\x1b[0m"
	typed := "\x1b[39m❯ hello world typed"
	rule := "\x1b[38;5;244m" + strings.Repeat("─", 80) + "\x1b[39m"

	in, ok := readClaudeInput([]string{stripDimText(rule), stripDimText(placeholder), stripDimText(rule)})
	if !ok || !in.empty() {
		t.Fatalf("placeholder input box = %q (found %v), want an empty input box", in.rows, ok)
	}
	in, ok = readClaudeInput([]string{stripDimText(rule), stripDimText(typed), stripDimText(rule)})
	if !ok || in.text() != "hello world typed" {
		t.Fatalf("typed input box = %q (found %v), want \"hello world typed\"", in.rows, ok)
	}

	for raw, want := range map[string]string{
		"\x1b[38;5;2mgreen":                          "green", // color 2 is not the dim attribute
		"\x1b[2;38;5;246mdim\x1b[22mnormal":          "normal",
		"a\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\b": "alinkb", // OSC 8 hyperlink
		"\x1b[2mdim\x1b[mback":                       "back",
	} {
		if got := stripDimText(raw); got != want {
			t.Errorf("stripDimText(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Before Claude's placeholder is the input box, the nudge must not mistake it
// for a draft: no Ctrl-U at all, and the message goes out alone.
func TestNudgeSessionSendsNoCtrlUOverClaudePlaceholder(t *testing.T) {
	pane := newFakeClaudePane("")
	pane.placeholder = "Try \"write a test for <filepath>\""
	tm := newFakeClaudeTmux(pane)

	if err := tm.NudgeSession(pane.sessionName, "hello"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	if !slices.Equal(pane.submitted, []string{"hello"}) {
		t.Fatalf("Claude received %q, want only \"hello\"", pane.submitted)
	}
	if n := pane.keyCount("C-u"); n != 0 {
		t.Fatalf("sent %d Ctrl-U over the placeholder, want none", n)
	}
}

// A permission menu's "❯ 1. Yes" cursor row is not the input box.
func TestClaudeInputIgnoresPermissionMenuCursor(t *testing.T) {
	lines := []string{
		strings.Repeat("─", 80),
		" Bash command",
		"   ls",
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. No",
	}
	if in, ok := readClaudeInput(lines); ok {
		t.Fatalf("read input box %q from a permission menu, want none", in.rows)
	}
}

// A human attached to the session may be typing: the stop-time clear must not
// wipe their draft (#5192). It sends no keys and says why.
func TestClearInputLeavesAttachedClaudeDraftAlone(t *testing.T) {
	pane := newFakeClaudePane("human draft in progress")
	pane.attached = true
	tm := newFakeClaudeTmux(pane)

	err := tm.ClearInput(context.Background(), pane.sessionName, 300*time.Millisecond)
	if !errors.Is(err, runtime.ErrInputClearSkipped) {
		t.Fatalf("ClearInput = %v, want runtime.ErrInputClearSkipped", err)
	}
	if len(pane.keys) != 0 || pane.draft != "human draft in progress" {
		t.Fatalf("keys = %q, draft = %q; want no keys and the draft intact", pane.keys, pane.draft)
	}
}

// When the attachment probe cannot answer, a human may be there: leave the
// input box alone, as the paste path does.
func TestClearInputLeavesDraftAloneWhenAttachProbeFails(t *testing.T) {
	pane := newFakeClaudePane("human draft in progress")
	pane.attachProbeErr = errors.New("server exited unexpectedly")
	tm := newFakeClaudeTmux(pane)

	err := tm.ClearInput(context.Background(), pane.sessionName, 0)
	if !errors.Is(err, runtime.ErrInputClearSkipped) {
		t.Fatalf("ClearInput = %v, want runtime.ErrInputClearSkipped", err)
	}
	if len(pane.keys) != 0 || pane.draft != "human draft in progress" {
		t.Fatalf("keys = %q, draft = %q; want no keys and the draft intact", pane.keys, pane.draft)
	}
}

// A client that attaches while ClearInput waits for Claude to restore the
// interrupted prompt also stops the clear before any key is sent.
func TestClearInputStopsWhenClientAttachesDuringRestoreWindow(t *testing.T) {
	pane := newFakeClaudePane("")
	pane.restoreDraft = claudeRestoredDraft
	pane.restoreOnCapture = 3
	pane.attachOnCapture = 2
	tm := newFakeClaudeTmux(pane)

	err := tm.ClearInput(context.Background(), pane.sessionName, 2*time.Second)
	if !errors.Is(err, runtime.ErrInputClearSkipped) {
		t.Fatalf("ClearInput = %v, want runtime.ErrInputClearSkipped", err)
	}
	if len(pane.keys) != 0 {
		t.Fatalf("keys = %q, want none once a client attached", pane.keys)
	}
}

// A draft whose cursor is not at its end (a human moved it, or Claude
// restored the prompt with the cursor at the start) still clears: Ctrl-U
// alone deletes nothing before a cursor at the start of the draft.
func TestClearInputClearsDraftWithCursorAtStart(t *testing.T) {
	pane := newFakeClaudePane(claudeRestoredDraft)
	pane.cursor = 0
	tm := newFakeClaudeTmux(pane)

	if err := tm.ClearInput(context.Background(), pane.sessionName, 0); err != nil {
		t.Fatalf("ClearInput: %v", err)
	}
	if pane.draft != "" {
		t.Fatalf("input box still holds %q", pane.draft)
	}
	if n := pane.keyCount("C-c"); n != 0 || pane.exitArmed {
		t.Fatalf("keys = %q, want no Ctrl-C", pane.keys)
	}
}

// The submit path's clear handles a cursor in the middle of the draft too, so
// the message still goes out alone.
func TestNudgeSessionClearsDraftWithCursorInTheMiddle(t *testing.T) {
	pane := newFakeClaudePane(claudeRestoredDraft)
	pane.cursor = strings.Index(claudeRestoredDraft, "lighthouses")
	tm := newFakeClaudeTmux(pane)
	const b = "d1.0-B: reply with exactly the words BANANA d1.0 and nothing else."

	if err := tm.NudgeSession(pane.sessionName, b); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	if !slices.Equal(pane.submitted, []string{b}) {
		t.Fatalf("Claude received %q, want only %q", pane.submitted, b)
	}
}
