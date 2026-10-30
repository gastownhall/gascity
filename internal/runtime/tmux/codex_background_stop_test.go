package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Pane captures from codex-cli 0.162.1 in the GC tmux runtime, taken live: a
// Codex session asked to run `sh -c "sleep 90; touch /tmp/marker"` in the
// foreground, then stopped through POST /v0/city/{city}/session/{id}/stop.
// Codex ran the command in a background terminal, so the interrupt ended the
// turn while the command kept running and later wrote the marker.
var (
	// The turn's status row before the stop. The rows around it are this
	// test's; the status row is verbatim.
	codexWorkingWithBackgroundTerminalPane = []string{
		"› Run sh -c \"sleep 90; touch /tmp/marker\" in the foreground",
		"",
		"• Working (11s • esc to interrupt) · 1 background terminal running · /ps to vie…",
		"› Ask Codex to do anything",
		"  GPT-6-Luna medium · ~/staging/stock-lab/fresh1 · Run foreground shell command",
	}
	// The bottom of the pane right after the stop returned, verbatim.
	codexInterruptedWithBackgroundTerminalPane = []string{
		"■ Conversation interrupted - use /feedback if something went wrong",
		"  1 background terminal running · /ps to view · /stop to close",
		"› Ask Codex to do anything",
		"  GPT-6-Luna medium · ~/staging/stock-lab/fresh1 · Run foreground shell command",
		"  ? for shortcuts                                      ⚠ 1 warning · f2 to view",
	}
)

// fakeCodexPane is a tmux executor that plays an interrupted codex-cli 0.162.1
// pane. It renders the live screens above: the Working status row while the
// turn is still busy, then the interrupted pane, with the background-terminal
// row while a terminal runs. Typing /stop and pressing Enter in the composer
// stops the background terminals the way Codex does, after
// stopAfterCaptures more captures (never, with ignoreStop). Its Codex holds
// open a rollout (under ~/.codex, with no CODEX_HOME, as a stock deployment
// has) whose tail already records the turn as aborted, so the interrupt
// boundary is reached on the first look.
type fakeCodexPane struct {
	mu sync.Mutex

	provider string
	// rollout is the file the pane's Codex holds open (heldRollout).
	rollout string

	// busyCaptures is how many captures still show the Working row.
	busyCaptures int
	// background is how many background terminals are running.
	background int
	// draft is the text in the composer; empty shows the dim placeholder.
	draft string
	// stopAfterCaptures is how many captures after /stop is submitted still
	// report the background terminals.
	stopAfterCaptures int
	ignoreStop        bool

	stopSubmitted bool
	stopCaptures  int
	captures      int
	// events is every literal text ("type:<text>") and key ("key:<key>")
	// that reached the pane, and every pause the runtime took
	// ("sleep:<duration>"), in order.
	events []string
}

func newFakeCodexPane(t *testing.T) *fakeCodexPane {
	t.Helper()
	home := t.TempDir()
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "10", "12", "rollout-2026-10-12T19-46-33-01a1d9e2-7c41-7a10-9d55-3b6a1f0c2e41.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	aborted := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<turn_aborted>\nThe user interrupted the previous turn on purpose.\n</turn_aborted>"}]}}` + "\n"
	if err := os.WriteFile(rollout, []byte(aborted), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return &fakeCodexPane{provider: "codex", rollout: rollout, background: 1}
}

// heldRollout answers the open-file lookup for the pane's process tree
// (fakeAgentPaneRow's pane PID 4242): the rollout its Codex holds open.
func (f *fakeCodexPane) heldRollout(panePID int) string {
	if panePID != 4242 {
		return ""
	}
	return f.rollout
}

func (f *fakeCodexPane) render(escapes bool) []string {
	if f.captures <= f.busyCaptures {
		return append([]string(nil), codexWorkingWithBackgroundTerminalPane...)
	}
	lines := []string{codexInterruptedWithBackgroundTerminalPane[0]}
	if f.stopSubmitted {
		lines = append(lines, "", "• Stopping all background terminals.", "")
	}
	if f.background > 0 {
		lines = append(lines, codexInterruptedWithBackgroundTerminalPane[1])
	}
	composer := "› " + f.draft
	if f.draft == "" {
		placeholder := "Ask Codex to do anything"
		if escapes {
			placeholder = "\x1b[2m" + placeholder + "\x1b[22m"
		}
		composer = "› " + placeholder
	}
	lines = append(lines, composer)
	return append(lines, codexInterruptedWithBackgroundTerminalPane[3:]...)
}

func (f *fakeCodexPane) execute(args []string) (string, error) {
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
		return "", errors.New("unknown variable")
	case "display-message":
		if slices.Contains(args, "#{pane_in_mode}") {
			return "0", nil
		}
		return "", nil
	case "capture-pane":
		f.captures++
		if f.stopSubmitted && !f.ignoreStop && f.background > 0 {
			f.stopCaptures++
			if f.stopCaptures > f.stopAfterCaptures {
				f.background = 0
			}
		}
		return strings.Join(f.render(slices.Contains(args, "-e")), "\n"), nil
	case "send-keys":
		f.sendKeys(args)
		return "", nil
	}
	return "", nil
}

func (f *fakeCodexPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func (f *fakeCodexPane) sendKeys(args []string) {
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
		f.draft += text
		return
	}
	for _, key := range keys {
		f.events = append(f.events, "key:"+key)
		if key == "Enter" {
			if f.draft == "/stop" {
				f.stopSubmitted = true
			}
			f.draft = ""
		}
	}
}

// sleep records a pause the runtime took, without taking it.
func (f *fakeCodexPane) sleep(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "sleep:"+d.String())
	return nil
}

// allEvents returns every event, pauses included.
func (f *fakeCodexPane) allEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// typedOrPressed returns what was typed and pressed, without the pauses.
func (f *fakeCodexPane) typedOrPressed() []string {
	var got []string
	for _, event := range f.allEvents() {
		if !strings.HasPrefix(event, "sleep:") {
			got = append(got, event)
		}
	}
	return got
}

// pausedAfter returns the total of the pauses taken after the first event
// equal to marker.
func (f *fakeCodexPane) pausedAfter(marker string) time.Duration {
	var total time.Duration
	seen := false
	for _, event := range f.allEvents() {
		if event == marker {
			seen = true
			continue
		}
		if d, ok := strings.CutPrefix(event, "sleep:"); ok && seen {
			parsed, err := time.ParseDuration(d)
			if err == nil {
				total += parsed
			}
		}
	}
	return total
}

// newCodexStopTmux returns a Tmux driving pane, whose pauses pane records
// instead of taking.
func newCodexStopTmux(pane *fakeCodexPane) *Tmux {
	tm := NewTmux()
	tm.exec = pane
	tm.backgroundStopSleep = pane.sleep
	tm.codexRolloutHeld = pane.heldRollout
	return tm
}

// TestCodexStopBackgroundWorkStopsBackgroundTerminals is the live defect: the
// stop ended the Codex turn, but the command it was running kept going in a
// background terminal. StopBackgroundWork types Codex's own /stop into the
// idle composer, submits it with a plain Enter, and waits until the pane no
// longer reports running background terminals.
func TestCodexStopBackgroundWorkStopsBackgroundTerminals(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.stopAfterCaptures = 2
	tm := newCodexStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-a"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}

	if got, want := pane.typedOrPressed(), []string{"type:/stop", "key:Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the interrupted Codex pane = %q, want %q: the interrupt left its background terminal running", got, want)
	}
	// Codex takes an Enter inside a fast burst of keys for a newline, so the
	// Enter follows a settle pause, and is plain: no Escape, which would close
	// the /stop popup.
	events := pane.allEvents()
	typed := slices.Index(events, "type:/stop")
	if got, want := events[typed:typed+3], []string{"type:/stop", "sleep:" + codexSlashCommandSettle.String(), "key:Enter"}; !slices.Equal(got, want) {
		t.Fatalf("submit of /stop = %q, want %q", got, want)
	}
	if pane.background != 0 {
		t.Fatalf("background terminals = %d after the wait returned, want 0", pane.background)
	}
	// The wait ends on the first capture that no longer reports them.
	if got, want := pane.stopCaptures, pane.stopAfterCaptures+1; got != want {
		t.Fatalf("captures after /stop = %d, want %d (the wait must end as the row clears)", got, want)
	}
}

// TestCodexStopBackgroundWorkSendsNothingWithoutBackgroundTerminals: an
// interrupted turn that left no background terminal running gets no keys.
func TestCodexStopBackgroundWorkSendsNothingWithoutBackgroundTerminals(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.background = 0
	tm := newCodexStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-b"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got := pane.typedOrPressed(); len(got) != 0 {
		t.Fatalf("keys reaching a Codex pane with no background terminal = %q, want none", got)
	}
}

// TestCodexStopBackgroundWorkLeavesADraftAlone: /stop typed behind text in the
// composer would submit that text with /stop glued to it, so a composer that
// is not empty gets no keys and the background terminal keeps running.
func TestCodexStopBackgroundWorkLeavesADraftAlone(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.draft = "a note someone is typing"
	tm := newCodexStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-c"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got := pane.typedOrPressed(); len(got) != 0 {
		t.Fatalf("keys reaching a Codex composer holding a draft = %q, want none", got)
	}
	if pane.draft != "a note someone is typing" {
		t.Fatalf("composer = %q, want the draft untouched", pane.draft)
	}
}

// TestCodexStopBackgroundWorkWaitsOutTheWorkingRow: the Working row also names
// the background terminal, but a busy composer is no place for /stop. It is
// typed only once the pane reads idle, and only once.
func TestCodexStopBackgroundWorkWaitsOutTheWorkingRow(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.busyCaptures = 2
	tm := newCodexStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-d"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got, want := pane.typedOrPressed(), []string{"type:/stop", "key:Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Codex pane = %q, want %q", got, want)
	}
	if pane.background != 0 {
		t.Fatalf("background terminals = %d, want 0", pane.background)
	}
}

// TestCodexStopBackgroundWorkReportsBackgroundTerminalsThatKeepRunning: when
// the row does not clear, /stop is sent once, the wait is bounded, and the
// stop still succeeds (the turn is over; the runtime logs what is left).
func TestCodexStopBackgroundWorkReportsBackgroundTerminalsThatKeepRunning(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.ignoreStop = true
	tm := newCodexStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-e"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got, want := pane.typedOrPressed(), []string{"type:/stop", "key:Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Codex pane = %q, want %q (one /stop, no retries)", got, want)
	}
	if pane.background != 1 {
		t.Fatalf("background terminals = %d, want the fake's 1 still running", pane.background)
	}
	if got := pane.pausedAfter("key:Enter"); got != codexBackgroundStopWait {
		t.Fatalf("waited %v for the background terminals to stop, want the %v bound", got, codexBackgroundStopWait)
	}
}

// TestCodexPaneReportsBackgroundTerminals pins where the report is read: in
// Codex's status area around the live composer, not in the transcript.
func TestCodexPaneReportsBackgroundTerminals(t *testing.T) {
	afterStop := []string{
		codexInterruptedWithBackgroundTerminalPane[0],
		"",
		"• Stopping all background terminals.",
		"",
		"› Ask Codex to do anything",
		"  GPT-6-Luna medium · ~/staging/stock-lab/fresh1 · Run foreground shell command",
	}
	for _, tc := range []struct {
		name              string
		lines             []string
		running, readable bool
	}{
		{"live interrupted pane", codexInterruptedWithBackgroundTerminalPane, true, true},
		{"live Working row", codexWorkingWithBackgroundTerminalPane, true, true},
		{"after /stop", afterStop, false, true},
		{"plural", []string{"  2 background terminals running · /ps to view · /stop to close", "› ", "  gpt-5.5 low · /tmp/probe"}, true, true},
		{"transcript above the status area", append([]string{
			"• The footer said 1 background terminal running earlier.",
			"",
			"• Done.",
			"",
		}, afterStop...), false, true},
		{"an earlier message, not the live composer", []string{
			"› 1 background terminal running",
			"",
			"• Noted.",
			"",
			"• Stopping all background terminals.",
			"› Ask Codex to do anything",
		}, false, true},
		{"no composer on screen", codexInterruptedWithBackgroundTerminalPane[:2], false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			running, readable := codexPaneReportsBackgroundTerminals(tc.lines)
			if running != tc.running || readable != tc.readable {
				t.Fatalf("codexPaneReportsBackgroundTerminals = (%t, %t), want (%t, %t)", running, readable, tc.running, tc.readable)
			}
		})
	}
}

// TestOtherProvidersStopBackgroundWorkIsUnsupported: a session that runs
// neither Codex nor Claude has no background work for the runtime to stop; no
// keys reach its pane.
func TestOtherProvidersStopBackgroundWorkIsUnsupported(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.provider = "gemini"
	tm := newCodexStopTmux(pane)

	err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-f")
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("StopBackgroundWork(gemini) = %v, want ErrInteractionUnsupported", err)
	}
	if got := pane.typedOrPressed(); len(got) != 0 {
		t.Fatalf("keys reaching a Gemini pane = %q, want none", got)
	}
}

// codexAbortedRow is the rollout row Codex writes when an interrupt lands.
const codexAbortedRow = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<turn_aborted>\nThe user interrupted the previous turn on purpose.\n</turn_aborted>"}]}}` + "\n"

// codexLifecycleRow is the rollout row Codex writes for a turn's lifecycle
// event: kind is task_started, task_complete or turn_aborted.
func codexLifecycleRow(kind, turnID string) string {
	return `{"timestamp":"2026-10-16T02:43:26.424Z","type":"event_msg","payload":{"type":"` + kind + `","turn_id":"` + turnID + `"}}` + "\n"
}

// codexTurnAbortedRows are the rows codex-cli 0.162.1 writes when an
// interrupt aborts turnID: a <turn_aborted> developer message, then the
// turn_aborted event.
func codexTurnAbortedRows(turnID string) []string {
	return []string{
		`{"timestamp":"2026-10-16T02:43:26.418Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<turn_aborted>\nThe previous turn was interrupted on purpose.\n</turn_aborted>"}]}}` + "\n",
		codexLifecycleRow("turn_aborted", turnID),
	}
}

// writeCodexRollout replaces the rollout at path with rows, last written at
// modTime.
func writeCodexRollout(t *testing.T, path string, modTime time.Time, rows ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(rows, "")), 0o644); err != nil {
		t.Fatalf("writing rollout: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("stamping rollout: %v", err)
	}
}

// appendCodexRollout appends rows to the rollout at path, as Codex records
// them now.
func appendCodexRollout(t *testing.T, path string, rows ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("opening rollout: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(strings.Join(rows, "")); err != nil {
		t.Fatalf("writing rollout: %v", err)
	}
	// Stamp the write time explicitly. Linux filesystems take file times from
	// the kernel's coarse clock, which can trail time.Now() by a few
	// milliseconds, so a write made microseconds after the interrupt began can
	// read as older than it. Real Codex writes these rows long after GC's
	// interrupt keys, well clear of that granularity.
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("stamping rollout: %v", err)
	}
}

// TestCodexInterruptBoundaryReadsTheSessionsOwnRollout: Codex sessions
// commonly share one home (~/.codex, no CODEX_HOME), so the newest rollout
// there can be another session's. The boundary an interrupt_now submit waits
// for is read from the rollout this session's Codex holds open: another
// session's newer turn_aborted does not end the wait, and this session's does.
func TestCodexInterruptBoundaryReadsTheSessionsOwnRollout(t *testing.T) {
	pane := newFakeCodexPane(t)
	if err := os.WriteFile(pane.rollout, []byte(`{"type":"event_msg","payload":{"type":"task_started"}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	other := filepath.Join(filepath.Dir(pane.rollout), "rollout-2026-10-12T19-46-40-01a1d9e2-0000-7000-8000-000000000002.jsonl")
	if err := os.WriteFile(other, []byte(codexAbortedRow), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	newest := time.Now().Add(time.Hour)
	if err := os.Chtimes(other, newest, newest); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	tm := newCodexStopTmux(pane)
	since := time.Now().Add(-time.Minute)

	if err := tm.WaitForInterruptBoundary(context.Background(), "codex-bg-stop-g", since, 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("WaitForInterruptBoundary = %v, want ErrIdleTimeout: only another session's rollout records an abort", err)
	}

	f, err := os.OpenFile(pane.rollout, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(codexAbortedRow); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	_ = f.Close()
	if err := tm.WaitForInterruptBoundary(context.Background(), "codex-bg-stop-g", since, time.Second); err != nil {
		t.Fatalf("WaitForInterruptBoundary = %v, want nil: this session's rollout records the abort", err)
	}
}

// TestCodexInterruptBoundaryWithoutItsOwnRolloutIsUnsupported: a session
// whose rollout cannot be named (none held open, two held in its process
// tree, or open files that cannot be read) has no boundary to wait for; the
// wait reports runtime.ErrInteractionUnsupported at once rather than reading
// another session's rollout.
func TestCodexInterruptBoundaryWithoutItsOwnRolloutIsUnsupported(t *testing.T) {
	pane := newFakeCodexPane(t)
	tm := newCodexStopTmux(pane)
	tm.codexRolloutHeld = func(int) string { return "" }

	if err := tm.WaitForInterruptBoundary(context.Background(), "codex-bg-stop-g", time.Now().Add(-time.Minute), time.Minute); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("WaitForInterruptBoundary = %v, want runtime.ErrInteractionUnsupported", err)
	}
}

// TestCodexInterruptBoundaryWaitsOnlyForARunningTurn: the boundary an
// interrupt waits for is the end of the turn it interrupted. An idle Codex
// records nothing when Escape reaches it, so a session whose own rollout
// records no running turn has already reached the boundary: the wait returns
// at once, rather than timing out and failing the stop or restarting the
// session under an interrupt_now. A running turn is waited for until the
// rollout records its end, whether the interrupt aborted it or it completed
// before the interrupt landed.
func TestCodexInterruptBoundaryWaitsOnlyForARunningTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// rollout is the session's rollout when the interrupt is sent, last
		// written an hour before.
		rollout []string
		// ended is what Codex records after the interrupt; nil when no turn
		// was running, so Codex records nothing.
		ended []string
	}{
		{
			name:    "idle after a completed turn",
			rollout: []string{codexLifecycleRow("task_started", "t1"), codexLifecycleRow("task_complete", "t1")},
		},
		{
			name:    "idle after an earlier interrupt",
			rollout: append([]string{codexLifecycleRow("task_started", "t1")}, codexTurnAbortedRows("t1")...),
		},
		{
			name:    "a running turn the interrupt aborts",
			rollout: []string{codexLifecycleRow("task_started", "t1")},
			ended:   codexTurnAbortedRows("t1"),
		},
		{
			name:    "a running turn that completes before the interrupt lands",
			rollout: []string{codexLifecycleRow("task_started", "t1")},
			ended:   []string{codexLifecycleRow("task_complete", "t1")},
		},
		{
			name:    "a later turn running after an earlier one ended",
			rollout: []string{codexLifecycleRow("task_started", "t1"), codexLifecycleRow("task_complete", "t1"), codexLifecycleRow("task_started", "t2")},
			ended:   codexTurnAbortedRows("t2"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := newFakeCodexPane(t)
			writeCodexRollout(t, pane.rollout, time.Now().Add(-time.Hour), tc.rollout...)
			tm := newCodexStopTmux(pane)
			since := time.Now()

			if tc.ended == nil {
				if err := tm.WaitForInterruptBoundary(context.Background(), "codex-bg-stop-h", since, 2*time.Second); err != nil {
					t.Fatalf("WaitForInterruptBoundary = %v, want nil at once: no turn was running, so there is no turn end to wait for", err)
				}
				return
			}
			if err := tm.WaitForInterruptBoundary(context.Background(), "codex-bg-stop-h", since, 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
				t.Fatalf("WaitForInterruptBoundary = %v, want ErrIdleTimeout while the turn still runs", err)
			}
			appendCodexRollout(t, pane.rollout, tc.ended...)
			if err := tm.WaitForInterruptBoundary(context.Background(), "codex-bg-stop-h", since, time.Second); err != nil {
				t.Fatalf("WaitForInterruptBoundary = %v, want nil once the rollout records the turn's end", err)
			}
		})
	}
}

// codex-cli 0.162.1 records an interrupted turn as a <turn_aborted>
// developer message and a turn_aborted event (rows from a live rollout,
// 2026-10-16). Missing them, the interrupt_now boundary wait ran its full
// timeout on every interrupt, and the submit then restarted Codex.
func TestCodexTranscriptTailRecognizesCodex0162TurnAborted(t *testing.T) {
	developer := `{"timestamp":"2026-10-16T02:43:26.418Z","ordinal":17,"type":"response_item","payload":{"type":"message","id":"msg_1","role":"developer","content":[{"type":"input_text","text":"<turn_aborted>\nThe previous turn was interrupted on purpose.\n</turn_aborted>"}]}}`
	event := `{"timestamp":"2026-10-16T02:43:26.424Z","ordinal":18,"type":"event_msg","payload":{"type":"turn_aborted","root_turn_id":"t1","turn_id":"t1","reason":"interrupted"}}`
	tokens := `{"timestamp":"2026-10-16T02:43:26.404Z","ordinal":16,"type":"event_msg","payload":{"type":"token_count"}}`
	for name, tail := range map[string]string{
		"developer message and event": tokens + "\n" + developer + "\n" + event,
		"event alone":                 tokens + "\n" + event,
		"developer message alone":     tokens + "\n" + developer,
	} {
		if !codexTranscriptTailContainsTurnAborted(tail) {
			t.Errorf("%s: codexTranscriptTailContainsTurnAborted() = false, want true", name)
		}
	}
	if codexTranscriptTailContainsTurnAborted(tokens) {
		t.Error("codexTranscriptTailContainsTurnAborted() = true for a tail with no abort, want false")
	}
}
