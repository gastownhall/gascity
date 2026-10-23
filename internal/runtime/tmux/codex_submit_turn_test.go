package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// codexTurnPane is a tmux executor that plays a codex-cli 0.162.1 pane and
// its rollout across a submitted turn, on the clock of the synctest bubble it
// runs in:
//
//   - literal text goes into the composer; C-u empties it;
//   - Enter on an idle pane with text in the composer starts a turn: the
//     composer empties and the rollout records task_started (created on the
//     first turn, as Codex materializes its rollout lazily);
//   - the turn records the submitted text as a user message only recordAfter
//     into the turn (Codex runs the UserPromptSubmit hook GC installs first);
//   - its "Working (… esc to interrupt)" row shows only busyHiddenFor into the
//     turn (a contended host draws it late, or never within the window);
//   - Escape on a running turn aborts it ("turn_aborted", reason interrupted):
//     a submitted text not yet recorded is lost, as it was live.
//
// earlierTurnFor > 0 starts the pane inside an earlier turn that shows its
// Working row from the start and ends by itself after that long.
// selfAbortAfter > 0 makes a submitted turn abort by itself that long in, an
// abort GC did not cause.
type codexTurnPane struct {
	mu sync.Mutex

	session string
	// rollout is the file this pane's Codex holds open once its first turn
	// creates it. The pane serves no CODEX_HOME: Codex defaults to ~/.codex,
	// as a stock deployment does.
	rollout string
	// held, when set, replaces heldRollout as the open-file lookup.
	held func(panePID int) string

	busyHiddenFor  time.Duration
	recordAfter    time.Duration
	selfAbortAfter time.Duration
	// swallowEnters is how many Enters the composer takes as a newline instead
	// of a submit (#4706: codex reads a lone Enter inside a paste burst so).
	swallowEnters int
	// burstEatsEnter plays codex-cli 0.162.1 (live Mayor frames, 2026-10-16):
	// typed multi-line text is a paste burst the composer does not show yet,
	// and the Enter after it is one more newline, not a submit. The burst
	// appears in the composer codexBurstFlush after that Enter. A bracketed
	// paste is not a burst.
	burstEatsEnter bool
	burst          string
	// attached makes the pane report one attached client.
	attached bool
	// ctrlUKillsLine plays Codex's own Ctrl-U: it deletes back to the start
	// of the draft's last line, or the line break before an empty last line,
	// rather than the whole draft. ctrlUIgnored makes Ctrl-U delete nothing.
	ctrlUKillsLine bool
	ctrlUIgnored   bool
	// scrollback is history above the pane's live screen, as a 120-row
	// capture returns it.
	scrollback []string
	// buffers holds tmux paste buffers loaded with load-buffer.
	buffers map[string]string

	draft     string
	running   bool
	turnStart time.Time
	turnSeq   int
	timers    []*time.Timer

	// recorded is every text the rollout recorded as a user message.
	recorded []string
	// aborted counts turns an Escape cut short.
	aborted int
	// events is every literal text ("type:<text>") and key ("key:<key>")
	// that reached the pane, in order.
	events []string
}

func newCodexTurnPane(t *testing.T, session string) *codexTurnPane {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "10", "15")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return &codexTurnPane{
		session:       session,
		rollout:       filepath.Join(dir, "rollout-2026-10-15T08-42-16-01a13eb9-f318-7c32-87de-d469fe118a8b.jsonl"),
		busyHiddenFor: 300 * time.Millisecond,
		recordAfter:   50 * time.Millisecond,
	}
}

// startEarlierTurn puts the pane inside a turn that ends by itself after d.
func (f *codexTurnPane) startEarlierTurn(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
	f.turnStart = time.Now()
	f.busyHiddenFor = 0
	f.turnSeq++
	seq := f.turnSeq
	f.timers = append(f.timers, time.AfterFunc(d, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.running && f.turnSeq == seq {
			f.running = false
		}
	}))
}

// heldRollout answers the open-file lookup for the pane's process tree
// (fakeAgentPaneRow's pane PID 4242): its rollout, once the first turn has
// created it.
func (f *codexTurnPane) heldRollout(panePID int) string {
	if f.held != nil {
		return f.held(panePID)
	}
	if panePID != 4242 {
		return ""
	}
	if _, err := os.Stat(f.rollout); err != nil {
		return ""
	}
	return f.rollout
}

// otherSessionWrites appends rows to another Codex session's rollout in the
// same home and sessions directory, after d, and leaves that file the newest
// there. Its mtime is set outright: the bubble's clock does not reach the
// filesystem, and a coarse mtime could tie the two files.
func (f *codexTurnPane) otherSessionWrites(d time.Duration, rows ...map[string]any) {
	other := filepath.Join(filepath.Dir(f.rollout), "rollout-2026-10-15T08-42-17-01a13eba-0000-7000-8000-000000000002.jsonl")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timers = append(f.timers, time.AfterFunc(d, func() {
		file, err := os.OpenFile(other, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			panic(err)
		}
		defer file.Close() //nolint:errcheck
		for _, row := range rows {
			line, _ := json.Marshal(map[string]any{
				"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
				"type":      row["type"],
				"payload":   row["payload"],
			})
			if _, err := file.Write(append(line, '\n')); err != nil {
				panic(err)
			}
		}
		newest := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(other, newest, newest); err != nil {
			panic(err)
		}
	}))
}

// stop cancels the pane's pending turn events so the bubble can end.
func (f *codexTurnPane) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, timer := range f.timers {
		timer.Stop()
	}
}

func (f *codexTurnPane) writeRollout(kind string, payload map[string]any) {
	line, _ := json.Marshal(map[string]any{
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"type":      kind,
		"payload":   payload,
	})
	file, err := os.OpenFile(f.rollout, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		panic(err)
	}
	defer file.Close() //nolint:errcheck
	if _, err := file.Write(append(line, '\n')); err != nil {
		panic(err)
	}
}

// startTurn begins the turn that carries text. The caller holds mu.
func (f *codexTurnPane) startTurn(text string) {
	if _, err := os.Stat(f.rollout); errors.Is(err, os.ErrNotExist) {
		f.writeRollout("session_meta", map[string]any{"id": "01a13eb9-f318-7c32-87de-d469fe118a8b", "cli_version": "0.162.1", "originator": "codex-tui"})
	}
	f.running = true
	f.turnStart = time.Now()
	f.turnSeq++
	seq := f.turnSeq
	f.writeRollout("event_msg", map[string]any{"type": "task_started", "turn_id": "turn-" + time.Now().Format("150405.000")})
	f.writeRollout("response_item", map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "<environment_context>\n  <cwd>/w</cwd>\n</environment_context>"}}})
	f.timers = append(f.timers, time.AfterFunc(f.recordAfter, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.running || f.turnSeq != seq {
			return
		}
		f.recorded = append(f.recorded, text)
		f.writeRollout("response_item", map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": text}}})
		f.writeRollout("event_msg", map[string]any{"type": "user_message", "message": text})
	}))
	if f.selfAbortAfter > 0 {
		f.timers = append(f.timers, time.AfterFunc(f.selfAbortAfter, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.running && f.turnSeq == seq {
				f.abortTurn()
			}
		}))
	}
}

// abortTurn ends the running turn as Codex does on Escape. The caller holds mu.
func (f *codexTurnPane) abortTurn() {
	f.running = false
	f.turnSeq++
	f.writeRollout("event_msg", map[string]any{"type": "turn_aborted", "reason": "interrupted"})
}

func (f *codexTurnPane) render(escapes bool) []string {
	lines := append(slices.Clone(f.scrollback), ">_ OpenAI Codex (v0.162.1)", "")
	if f.running && time.Since(f.turnStart) >= f.busyHiddenFor {
		lines = append(lines, "• Working (1s • esc to interrupt)", "")
	}
	if rows := strings.Split(f.draft, "\n"); len(rows) > codexComposerRows {
		// A draft taller than the composer scrolls, as live: the prompt row
		// can show a blank line of the draft, with the scroll arrow.
		lines = append(lines, codexPromptPrefix+strings.Repeat(" ", 70)+"↑")
		lines = append(lines, rows[len(rows)-codexComposerRows+1:]...)
		return append(lines, "", "  gpt-6.1-sol low · ~/w")
	}
	composer := codexPromptPrefix + f.draft
	if f.draft == "" {
		placeholder := "Ask Codex to do anything"
		if escapes {
			placeholder = "\x1b[2m" + placeholder + "\x1b[22m"
		}
		composer = codexPromptPrefix + placeholder
	}
	return append(lines, composer, "", "  gpt-6.1-sol low · ~/w")
}

func (f *codexTurnPane) execute(args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	switch tmuxSubcommand(args) {
	case "show-environment":
		switch {
		case slices.Contains(args, "GC_PROVIDER"):
			return "GC_PROVIDER=codex", nil
		case slices.Contains(args, sessionReadyPromptEnvKey):
			return sessionReadyPromptEnvKey + "=" + codexPromptPrefix, nil
		}
		return "", errors.New("unknown variable")
	case "display-message":
		switch {
		case slices.Contains(args, "#{pane_in_mode}"):
			return "0", nil
		case slices.Contains(args, "#{session_name}|#{session_attached}"):
			if f.attached {
				return f.session + "|1", nil
			}
			return f.session + "|0", nil
		}
		return "", nil
	case "capture-pane":
		return strings.Join(f.render(slices.Contains(args, "-e")), "\n"), nil
	case "send-keys":
		f.sendKeys(args)
	case "load-buffer":
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return "", err
		}
		if f.buffers == nil {
			f.buffers = map[string]string{}
		}
		f.buffers[args[slices.Index(args, "-b")+1]] = string(data)
	case "paste-buffer":
		text := f.buffers[args[slices.Index(args, "-b")+1]]
		f.events = append(f.events, "paste:"+text)
		f.draft += text
	}
	return "", nil
}

func (f *codexTurnPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func (f *codexTurnPane) sendKeys(args []string) {
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
		if f.burstEatsEnter && strings.Contains(text, "\n") {
			f.burst += text
			return
		}
		f.draft += text
		return
	}
	for _, key := range keys {
		f.events = append(f.events, "key:"+key)
		switch key {
		case "C-u":
			switch {
			case f.ctrlUIgnored:
			case f.ctrlUKillsLine:
				f.draft = killToLineStart(f.draft)
			default:
				f.draft = ""
			}
		case "Escape":
			if f.running {
				f.aborted++
				f.abortTurn()
			}
		case "Enter":
			switch {
			case f.swallowEnters > 0:
				f.swallowEnters--
			case f.burst != "":
				flushed := f.burst + "\n"
				f.burst = ""
				f.timers = append(f.timers, time.AfterFunc(codexBurstFlush, func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					f.draft += flushed
				}))
			case !f.running && f.draft != "":
				text := f.draft
				f.draft = ""
				f.startTurn(text)
			}
		}
	}
}

// killToLineStart is draft after one of Codex's Ctrl-U presses with the
// cursor at its end: the last line's text goes, or, when that line is empty,
// the line break before it.
func killToLineStart(draft string) string {
	i := strings.LastIndex(draft, "\n")
	switch {
	case i < 0:
		return ""
	case i == len(draft)-1:
		return draft[:i]
	default:
		return draft[:i+1]
	}
}

// composer returns what sits typed in the pane's composer.
func (f *codexTurnPane) composer() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.draft
}

// keysPressed returns the named keys the pane received, in order.
func (f *codexTurnPane) keysPressed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for _, event := range f.events {
		if key, ok := strings.CutPrefix(event, "key:"); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

func (f *codexTurnPane) snapshot() (recorded []string, aborted int, events []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.recorded), f.aborted, slices.Clone(f.events)
}

// runCodexTurnNudge delivers message through NudgeSession to a pane set up by
// setup, inside a synctest bubble so every wait takes virtual time only.
func runCodexTurnNudge(t *testing.T, session string, setup func(*codexTurnPane)) (*codexTurnPane, error) {
	t.Helper()
	return runCodexTurnNudgeMessage(t, session, codexTurnMarker, setup)
}

// runCodexTurnNudgeMessage is runCodexTurnNudge for a given message.
func runCodexTurnNudgeMessage(t *testing.T, session, message string, setup func(*codexTurnPane)) (*codexTurnPane, error) {
	t.Helper()
	// The bubble creates this session's nudge lock channel; drop it so
	// nothing outside the bubble ever touches it.
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	var pane *codexTurnPane
	var err error
	synctest.Test(t, func(t *testing.T) {
		pane = newCodexTurnPane(t, session)
		setup(pane)
		tm := &Tmux{cfg: DefaultConfig(), exec: pane, codexRolloutHeld: pane.heldRollout}
		err = tm.NudgeSession(session, message)
		// Let the turn the submit started play out on the bubble's clock (no
		// wall time passes), then cancel what is left of it.
		<-time.After(time.Minute)
		pane.stop()
	})
	return pane, err
}

const codexTurnMarker = "Reply with exactly FIRST_SUBMIT_MARKER. Do not use tools or change files."

// codexComposerRows is how many draft rows the fake Codex composer shows
// before it scrolls.
const codexComposerRows = 8

// codexBurstFlush is how long after the Enter a typed burst takes to appear
// in the composer: on the loaded live host it came after the confirm loop had
// already read the composer as drained.
const codexBurstFlush = 3 * time.Second

// codexMayorPrompt is shaped like the stock Mayor's startup prompt: a
// multi-line Markdown message far taller than the composer.
var codexMayorPrompt = "[city] mayor • 2026-10-16T02:02:32\n# Mayor\n" + strings.Repeat("You are the mayor of this Gas City workspace.\n\n## Commands\nUse `/gc-work` to load command reference.\n", 6) + "Reply with exactly MAYOR_STARTUP_MARKER."

// TestCodexMultilineMessageArrivesAsOnePaste is the Mayor startup defect
// (codex-cli 0.162.1, every stock Mayor start since 2026-10-13): the
// multi-line startup prompt was typed with send-keys, Codex read it as a paste
// burst and took the submit Enter as one more newline, and the composer,
// still empty while Codex held the burst, was what the confirm loop
// read as drained before the burst appeared. The submit was credited and the
// prompt sat drafted. A multi-line message to Codex arrives as one bracketed
// paste, and the Enter after it submits.
func TestCodexMultilineMessageArrivesAsOnePaste(t *testing.T) {
	pane, err := runCodexTurnNudgeMessage(t, "codex-multiline-submit", codexMayorPrompt, func(p *codexTurnPane) {
		p.burstEatsEnter = true
	})
	if err != nil {
		t.Fatalf("NudgeSession = %v, want nil: Codex recorded the prompt", err)
	}
	recorded, _, events := pane.snapshot()
	if !slices.Equal(recorded, []string{codexMayorPrompt}) {
		t.Fatalf("Codex recorded %q, want the prompt once; events %q", recorded, events)
	}
	if !slices.Contains(events, "paste:"+codexMayorPrompt) {
		t.Fatalf("events %q, want the prompt as one bracketed paste", events)
	}
}

// TestCodexSingleLineMessageIsStillTyped: a one-line message is not a paste
// burst, so it is still typed.
func TestCodexSingleLineMessageIsStillTyped(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-single-line-submit", func(p *codexTurnPane) {
		p.burstEatsEnter = true
	})
	if err != nil {
		t.Fatalf("NudgeSession = %v, want nil", err)
	}
	if _, _, events := pane.snapshot(); !slices.Contains(events, "type:"+codexTurnMarker) {
		t.Fatalf("events %q, want the one-line message typed", events)
	}
}

// A Codex draft taller than the composer scrolls, and the prompt row can show
// a blank line of it next to the scroll arrow (rows captured from the live
// Mayor pane, 2026-10-16T02:02:54Z). That is a held draft, not a drained
// composer: the arrow is the row's remainder.
func TestCodexScrolledDraftIsNotADrainedComposer(t *testing.T) {
	scrolled := []string{
		"• I’m ready to manage the Gas City workspace. Tell me what you’d like me to",
		"  plan, dispatch, or monitor.",
		"  Worked for 21s • Oct 14 at 03:37",
		"›                                                                              ↑",
		"  Note: those `/gc-*` entries name skills exposed by your provider's command",
		"  palette, not bash commands.",
		"  GPT-6-Luna medium · ~/city · Plan Gas City workspace",
		"                                                       ⚠ 1 warning · f2 to view",
	}
	if paneShowsDrainedComposer(scrolled) {
		t.Fatal("paneShowsDrainedComposer = true for a scrolled draft, want false")
	}
	bare := append(append([]string(nil), scrolled[:3]...), "› ", "  GPT-6-Luna medium · ~/city · Plan Gas City workspace")
	if !paneShowsDrainedComposer(bare) {
		t.Fatal("paneShowsDrainedComposer = false for a bare composer, want true")
	}
}

// TestCodexSubmitLeavesTheTurnItStartedRunning is the live defect
// (healthy-direct, 2026-10-15): GC's Escape+Enter started the Codex turn
// (turn 01a13eba-340a at 08:42:32.842Z), but the pane showed no busy row
// within the confirm window, so the confirm loop re-sent Escape+Enter. The
// Escape aborted the turn 1609 ms in (turn_aborted, reason interrupted),
// before Codex recorded the message, and the submit still reported success
// because the composer had drained. A drained composer is the evidence the
// submit landed: the Escape-led sequence must not be sent again into it.
func TestCodexSubmitLeavesTheTurnItStartedRunning(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-submit-hidden-busy", func(p *codexTurnPane) {
		p.busyHiddenFor = time.Hour // the Working row never shows in the window
		p.recordAfter = 3 * time.Second
	})

	recorded, aborted, events := pane.snapshot()
	if aborted != 0 {
		t.Fatalf("the submit aborted %d Codex turn(s); events %q", aborted, events)
	}
	if got, want := pane.keysPressed(), []string{"C-u", "Escape", "Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys = %q, want %q: one Escape+Enter submit, never repeated into the turn it started", got, want)
	}
	if !slices.Equal(recorded, []string{codexTurnMarker}) {
		t.Fatalf("Codex recorded %q, want the submitted message once", recorded)
	}
	if err != nil {
		t.Fatalf("NudgeSession = %v, want nil: Codex's rollout recorded the message", err)
	}
}

// TestCodexSubmitAbortedBeforeItWasRecordedIsNotDelivered: when the turn the
// submit started ends before Codex records the message (here an abort GC did
// not cause), the message never landed. A drained composer is not proof of
// delivery then, and the submit must not report success. The session has no
// CODEX_HOME (Codex's default ~/.codex, as in a stock deployment): its rollout
// is found as the file its own Codex holds open.
func TestCodexSubmitAbortedBeforeItWasRecordedIsNotDelivered(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-submit-lost", func(p *codexTurnPane) {
		p.busyHiddenFor = time.Hour
		p.recordAfter = 3 * time.Second
		p.selfAbortAfter = time.Second
	})

	if recorded, _, _ := pane.snapshot(); len(recorded) != 0 {
		t.Fatalf("Codex recorded %q, want nothing (the fixture aborts first)", recorded)
	}
	if err == nil || errors.Is(err, runtime.ErrNudgeDeliveredUnobserved) {
		t.Fatalf("NudgeSession = %v, want a not-delivered error", err)
	}
	if !errors.Is(err, ErrNudgeSubmitLost) {
		t.Fatalf("NudgeSession = %v, want ErrNudgeSubmitLost", err)
	}
	if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("NudgeSession = %v, want it to wrap ErrNudgeSubmitUnconfirmed so retrying callers retry it", err)
	}
}

// TestCodexSubmitVerdictReadsTheSessionsOwnRollout: Codex sessions commonly
// share one home (~/.codex) and one working directory, so the newest rollout
// there, or one matched by its recorded cwd, can be another session's. The
// verdict reads only the rollout this session's Codex holds open: another
// session's newer abort does not lose this submit, and another session
// recording the same text does not deliver it.
func TestCodexSubmitVerdictReadsTheSessionsOwnRollout(t *testing.T) {
	envContext := map[string]any{"type": "response_item", "payload": map[string]any{
		"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": "<environment_context>\n  <cwd>/w</cwd>\n</environment_context>"}},
	}}
	taskStarted := map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "turn-other"}}
	otherAborted := map[string]any{"type": "event_msg", "payload": map[string]any{"type": "turn_aborted", "reason": "interrupted"}}
	otherRecorded := map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": codexTurnMarker}}

	t.Run("another session's newer abort does not lose a recorded submit", func(t *testing.T) {
		pane, err := runCodexTurnNudge(t, "codex-verdict-own-landed", func(p *codexTurnPane) {
			p.busyHiddenFor = time.Hour
			p.otherSessionWrites(200*time.Millisecond, envContext, taskStarted, otherAborted)
		})
		if recorded, _, _ := pane.snapshot(); !slices.Equal(recorded, []string{codexTurnMarker}) {
			t.Fatalf("Codex recorded %q, want the message once", recorded)
		}
		if err != nil {
			t.Fatalf("NudgeSession = %v, want nil: this session's rollout recorded the message", err)
		}
	})

	t.Run("another session recording the same text does not deliver a lost submit", func(t *testing.T) {
		pane, err := runCodexTurnNudge(t, "codex-verdict-own-lost", func(p *codexTurnPane) {
			p.busyHiddenFor = time.Hour
			p.recordAfter = 3 * time.Second
			p.selfAbortAfter = time.Second
			p.otherSessionWrites(1500*time.Millisecond, envContext, taskStarted, otherRecorded)
		})
		if recorded, _, _ := pane.snapshot(); len(recorded) != 0 {
			t.Fatalf("Codex recorded %q, want nothing (the fixture aborts first)", recorded)
		}
		if !errors.Is(err, ErrNudgeSubmitLost) {
			t.Fatalf("NudgeSession = %v, want ErrNudgeSubmitLost: this session's rollout records the abort", err)
		}
	})
}

// TestCodexSubmitVerdictWithoutItsOwnRolloutStandsUnobserved: when no single
// rollout can be tied to the session's Codex (none held open, two held in its
// process tree, or open files that cannot be read), the verdict does not
// guess from other rollouts: the drained composer stands as delivery that was
// not observed, as it did before the rollout was read.
func TestCodexSubmitVerdictWithoutItsOwnRolloutStandsUnobserved(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-verdict-unattributed", func(p *codexTurnPane) {
		p.busyHiddenFor = time.Hour
		p.recordAfter = 3 * time.Second
		p.selfAbortAfter = time.Second
		p.held = func(int) string { return "" }
	})
	if recorded, _, _ := pane.snapshot(); len(recorded) != 0 {
		t.Fatalf("Codex recorded %q, want nothing (the fixture aborts first)", recorded)
	}
	if !errors.Is(err, runtime.ErrNudgeDeliveredUnobserved) || errors.Is(err, ErrNudgeSubmitLost) {
		t.Fatalf("NudgeSession = %v, want ErrNudgeDeliveredUnobserved and not ErrNudgeSubmitLost", err)
	}
}

// selectHeldCodexRollout names a session's rollout by the process tree under
// its pane: the one rollout held open there, whichever process holds it.
func TestSelectHeldCodexRollout(t *testing.T) {
	const (
		own   = "/home/user/.codex/sessions/2026/10/15/rollout-2026-10-15T08-42-16-01a13eb9-f318-7c32-87de-d469fe118a8b.jsonl"
		other = "/home/user/.codex/sessions/2026/10/15/rollout-2026-10-15T08-42-17-01a13eba-0000-7000-8000-000000000002.jsonl"
	)
	// 4242 is the pane's shell, 4300 the codex launcher, 4301 the codex
	// binary; 5000 is another session's codex.
	records := []proctable.ProcessRecord{
		{PID: 4242, PPID: 1}, {PID: 4300, PPID: 4242}, {PID: 4301, PPID: 4300}, {PID: 5000, PPID: 1},
	}
	for _, tt := range []struct {
		name  string
		files map[int][]string
		err   map[int]bool
		want  string
	}{
		{"held by a descendant", map[int][]string{4242: {"/dev/pts/3"}, 4301: {"/home/user/city", own}, 5000: {other}}, nil, own},
		{"the same rollout held twice", map[int][]string{4300: {own}, 4301: {own, own}}, nil, own},
		{"none held yet", map[int][]string{4301: {"/home/user/city"}, 5000: {other}}, nil, ""},
		{"two rollouts in the tree", map[int][]string{4300: {other}, 4301: {own}}, nil, ""},
		{"a descendant whose files cannot be read", map[int][]string{4301: {own}}, map[int]bool{4300: true}, ""},
		{"a rollout-named file outside sessions", map[int][]string{4301: {"/tmp/rollout-x.jsonl"}}, nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			openFiles := func(pid int) ([]string, error) {
				if tt.err[pid] {
					return nil, errors.New("permission denied")
				}
				return tt.files[pid], nil
			}
			if got := selectHeldCodexRollout(records, 4242, openFiles); got != tt.want {
				t.Fatalf("selectHeldCodexRollout = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCodexSubmitWaitsOutARunningTurn: Codex reads Escape as "interrupt the
// running turn", so a submit that finds a turn running types nothing and
// presses nothing until the turn ends, then submits once.
func TestCodexSubmitWaitsOutARunningTurn(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-submit-after-turn", func(p *codexTurnPane) {
		p.startEarlierTurn(3 * time.Second)
	})
	if err != nil {
		t.Fatalf("NudgeSession = %v, want nil", err)
	}
	recorded, aborted, events := pane.snapshot()
	if aborted != 0 {
		t.Fatalf("the submit aborted %d Codex turn(s); events %q", aborted, events)
	}
	if !slices.Equal(recorded, []string{codexTurnMarker}) {
		t.Fatalf("Codex recorded %q, want the submitted message once", recorded)
	}
}

// TestCodexSubmitDefersWhileATurnOutlastsTheReadyWait: a turn still running
// when the wait for an idle composer runs out gets no keys at all, and the
// caller is told to deliver later (runtime.ErrSessionBusy), which the session
// layer turns into a queued submit.
func TestCodexSubmitDefersWhileATurnOutlastsTheReadyWait(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-submit-busy", func(p *codexTurnPane) {
		p.startEarlierTurn(time.Hour)
	})
	if !errors.Is(err, runtime.ErrSessionBusy) {
		t.Fatalf("NudgeSession = %v, want runtime.ErrSessionBusy", err)
	}
	if _, aborted, events := pane.snapshot(); aborted != 0 || len(events) != 0 {
		t.Fatalf("the busy Codex pane received %q (aborted %d), want nothing typed or pressed", events, aborted)
	}
}

// TestCodexSubmitResendsWhileTheDraftWaits keeps the #4706 recovery: when the
// first Enter is taken as a newline, the message still sits in the idle
// composer, and only then is the Escape+Enter submit sent again.
func TestCodexSubmitResendsWhileTheDraftWaits(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-submit-swallowed-enter", func(p *codexTurnPane) {
		p.swallowEnters = 1
	})
	if err != nil {
		t.Fatalf("NudgeSession = %v, want nil", err)
	}
	if got, want := pane.keysPressed(), []string{"C-u", "Escape", "Enter", "Escape", "Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys = %q, want %q", got, want)
	}
	if recorded, aborted, _ := pane.snapshot(); aborted != 0 || !slices.Equal(recorded, []string{codexTurnMarker}) {
		t.Fatalf("recorded %q, aborted %d; want the message once and no abort", recorded, aborted)
	}
}

// The rollout reading behind the Codex delivery verdict, on the rows the live
// healthy-direct rollout held after GC's submit (no pane frames survive): the
// environment context, task_started, then turn_aborted, and never the marker.
func TestCodexSubmitRecordReadsTheRollout(t *testing.T) {
	since := time.Date(2026, 10, 15, 8, 42, 30, 490_000_000, time.UTC)
	const head = `{"timestamp":"2026-10-15T08:42:32.878Z","type":"session_meta","payload":{"id":"01a13eb9-f318-7c32-87de-d469fe118a8b","cli_version":"0.162.1"}}
{"timestamp":"2026-10-15T08:42:32.879Z","type":"event_msg","payload":{"type":"task_started","turn_id":"01a13eba-340a-7fd3-8533-2aa0a7bd4e68"}}
{"timestamp":"2026-10-15T08:42:32.880Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/home/user/city</cwd>\n</environment_context>"}]}}
`
	const aborted = `{"timestamp":"2026-10-15T08:42:34.479Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"01a13eba-340a-7fd3-8533-2aa0a7bd4e68","reason":"interrupted","duration_ms":1609}}
`
	const recorded = `{"timestamp":"2026-10-15T08:42:33.500Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with exactly FIRST_SUBMIT_MARKER.\nDo not use tools or change files."}]}}
`
	const earlierAbort = `{"timestamp":"2026-10-15T08:42:29.000Z","type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}
`
	for _, tt := range []struct {
		name string
		tail string
		want codexSubmitRecord
	}{
		{"live: aborted before the message was recorded", head + aborted, codexSubmitLost},
		{"recorded, then aborted", head + recorded + aborted, codexSubmitLanded},
		{"recorded", head + recorded, codexSubmitLanded},
		{"turn still starting", head, codexSubmitUnknown},
		{"an abort before the submit is not this submit's", earlierAbort + head, codexSubmitUnknown},
		{"torn first line", `ab"}` + "\n" + head + aborted, codexSubmitLost},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := readCodexSubmitRecord(tt.tail, since, codexTurnMarker); got != tt.want {
				t.Fatalf("readCodexSubmitRecord = %v, want %v", got, tt.want)
			}
		})
	}
}

// A confirm loop for an Escape-led submit sends again only when the look
// before the send shows the message still waiting; never on a pane whose
// composer drained, and never when the look fails.
func TestSubmitAndConfirmWithholdsAnInterruptingResend(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mayResend func() (bool, error)
		wantSends int
	}{
		{"draft still waiting", func() (bool, error) { return true, nil }, submitEnterMaxSends},
		{"composer drained", func() (bool, error) { return false, nil }, 1},
		{"look failed", func() (bool, error) { return false, errors.New("capture-pane: timed out") }, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sends := 0
			confirmed, _, sent, err := submitAndConfirm(func() error { sends++; return nil }, func() {},
				func() (bool, error) { return false, nil }, tt.mayResend, noSleep)
			if err != nil || confirmed {
				t.Fatalf("submitAndConfirm = %t, %v; want unconfirmed, nil", confirmed, err)
			}
			if sends != tt.wantSends || sent != tt.wantSends {
				t.Fatalf("sends = %d (reported %d), want %d", sends, sent, tt.wantSends)
			}
		})
	}
}

// The first submit of an Escape-led sequence waits for an idle reading: a
// pane busy at the baseline gets its submit only after it reads idle.
func TestSubmitAndConfirmHoldsTheFirstInterruptingSubmitWhileBusy(t *testing.T) {
	reads, sends := 0, 0
	busy := func() (bool, error) {
		reads++
		if sends > 0 {
			return true, nil // the submit started a turn
		}
		return reads <= 3, nil // an earlier turn ends after three reads
	}
	confirmed, _, sent, err := submitAndConfirm(func() error { sends++; return nil }, func() {}, busy,
		func() (bool, error) { return true, nil }, noSleep)
	if err != nil || !confirmed {
		t.Fatalf("submitAndConfirm = %t, %v; want confirmed", confirmed, err)
	}
	if sent != 1 {
		t.Fatalf("sends = %d, want 1, after the earlier turn ended", sent)
	}
}

// runCodexStartupNudge delivers message through nudgeStartupSession, which
// takes no ready wait, to a pane running a turn that outlasts the delivery
// and set up further by setup, inside a synctest bubble.
func runCodexStartupNudge(t *testing.T, session, message string, setup func(*codexTurnPane)) (*codexTurnPane, error) {
	t.Helper()
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	var pane *codexTurnPane
	var err error
	synctest.Test(t, func(t *testing.T) {
		pane = newCodexTurnPane(t, session)
		pane.startEarlierTurn(time.Hour)
		setup(pane)
		tm := &Tmux{cfg: DefaultConfig(), exec: pane, codexRolloutHeld: pane.heldRollout}
		err = tm.nudgeStartupSession(session, message)
		pane.stop()
	})
	return pane, err
}

// assertHeldSubmitDeferred checks a delivery whose Escape-led submit a
// running turn held back for the whole confirm window: no submit key reached
// the turn, the typed message was cleared from the composer, and the error is
// a deferral (runtime.ErrSessionBusy), never an unconfirmed submit, because
// no submit was sent.
func assertHeldSubmitDeferred(t *testing.T, pane *codexTurnPane, err error) {
	t.Helper()
	if !errors.Is(err, runtime.ErrSessionBusy) {
		t.Fatalf("delivery = %v, want runtime.ErrSessionBusy", err)
	}
	if errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) {
		t.Fatalf("delivery = %v, want no unconfirmed-submit sentinel: no submit was sent", err)
	}
	if keys := pane.keysPressed(); slices.Contains(keys, "Escape") || slices.Contains(keys, "Enter") {
		t.Fatalf("keys = %q, want no submit into the running turn", keys)
	}
	if _, aborted, _ := pane.snapshot(); aborted != 0 {
		t.Fatalf("aborted %d turn(s), want none", aborted)
	}
	if draft := pane.composer(); draft != "" {
		t.Fatalf("composer holds %q, want the typed message cleared", draft)
	}
}

// TestCodexStartupNudgeHoldsItsSubmitWhileATurnRuns: the startup nudge takes
// no ready wait, so it can type into a pane already running a turn. Its
// Escape-led submit is then held back for the whole confirm window, so the
// message it typed is cleared and the nudge is deferred: a deferring caller
// queues it, and the startup ladder delivers it again later.
func TestCodexStartupNudgeHoldsItsSubmitWhileATurnRuns(t *testing.T) {
	pane, err := runCodexStartupNudge(t, "codex-startup-busy", codexTurnMarker, func(*codexTurnPane) {})
	assertHeldSubmitDeferred(t, pane, err)
}

// TestCodexHeldSubmitClearsItsDraftWithAClientAttached: the clear before the
// paste is skipped while a client is attached, since a human may be typing,
// but the message a held submit leaves typed is gc's own and is cleared
// anyway: a deferral must leave nothing typed behind.
func TestCodexHeldSubmitClearsItsDraftWithAClientAttached(t *testing.T) {
	pane, err := runCodexStartupNudge(t, "codex-startup-busy-attached", codexTurnMarker, func(p *codexTurnPane) {
		p.attached = true
	})
	assertHeldSubmitDeferred(t, pane, err)
}

// TestCodexHeldSubmitClearsAMultilineDraft: Codex's Ctrl-U deletes one line
// of a multi-line draft at a time, so the clear presses it until the
// composer reads empty.
func TestCodexHeldSubmitClearsAMultilineDraft(t *testing.T) {
	const message = "first line of the nudge\nsecond line\nthird line"
	pane, err := runCodexStartupNudge(t, "codex-startup-busy-multiline", message, func(p *codexTurnPane) {
		p.ctrlUKillsLine = true
	})
	assertHeldSubmitDeferred(t, pane, err)
	if _, _, events := pane.snapshot(); !slices.Contains(events, "paste:"+message) {
		t.Fatalf("events = %q, want the message pasted before it was cleared", events)
	}
}

// TestCodexHeldSubmitWhoseDraftWillNotClearIsNotADeferral: when the composer
// still holds the message after the clear, the nudge is neither a deferral,
// which promises nothing typed is left, nor an unconfirmed submit, since none
// was sent: callers count the attempt as failed.
func TestCodexHeldSubmitWhoseDraftWillNotClearIsNotADeferral(t *testing.T) {
	pane, err := runCodexStartupNudge(t, "codex-startup-busy-stuck-draft", codexTurnMarker, func(p *codexTurnPane) {
		p.ctrlUIgnored = true
	})
	if err == nil || errors.Is(err, runtime.ErrSessionBusy) || errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) {
		t.Fatalf("nudgeStartupSession = %v, want an error that is neither runtime.ErrSessionBusy nor unconfirmed", err)
	}
	if !errors.Is(err, errNudgeSubmitHeld) {
		t.Fatalf("nudgeStartupSession = %v, want errNudgeSubmitHeld", err)
	}
	if draft := pane.composer(); draft != codexTurnMarker {
		t.Fatalf("composer holds %q, want the message still typed", draft)
	}
	if keys := pane.keysPressed(); slices.Contains(keys, "Escape") || slices.Contains(keys, "Enter") {
		t.Fatalf("keys = %q, want no submit into the running turn", keys)
	}
}

// TestCodexStartupLadderDeliversAHeldNudgeOnceTheTurnEnds drives the startup
// retry ladder over a real delivery into a pane whose startup turn is still
// running: each held attempt is deferred with its draft cleared, the busy
// pane is never taken for a delivery, and the nudge lands once, after the
// turn ends.
func TestCodexStartupLadderDeliversAHeldNudgeOnceTheTurnEnds(t *testing.T) {
	const session = "codex-startup-ladder"
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	synctest.Test(t, func(t *testing.T) {
		pane := newCodexTurnPane(t, session)
		pane.startEarlierTurn(10 * time.Second)
		ops := &tmuxStartOps{tm: &Tmux{cfg: DefaultConfig(), exec: pane, codexRolloutHeld: pane.heldRollout}}

		err := sendStartupNudgeWithRetry(context.Background(),
			func() error { return ops.sendKeys(session, codexTurnMarker) },
			func(d time.Duration) { <-time.After(d) },
			func() (bool, error) { return ops.paneBusy(session) })
		<-time.After(time.Minute)
		pane.stop()

		if err != nil {
			t.Fatalf("sendStartupNudgeWithRetry = %v, want nil once the turn ended", err)
		}
		recorded, aborted, events := pane.snapshot()
		if aborted != 0 || !slices.Equal(recorded, []string{codexTurnMarker}) {
			t.Fatalf("recorded %q, aborted %d; want the nudge once and no abort; events %q", recorded, aborted, events)
		}
	})
}

// TestCodexStartupLadderNeverTakesAHeldNudgeForDelivered: a startup turn that
// outlasts the whole ladder keeps every attempt's submit held. Its busy pane
// is the turn that held the submit, not the nudge landing, so the ladder
// returns the deferral, with nothing left typed, instead of nil.
func TestCodexStartupLadderNeverTakesAHeldNudgeForDelivered(t *testing.T) {
	const session = "codex-startup-ladder-busy"
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	synctest.Test(t, func(t *testing.T) {
		pane := newCodexTurnPane(t, session)
		pane.startEarlierTurn(time.Hour)
		ops := &tmuxStartOps{tm: &Tmux{cfg: DefaultConfig(), exec: pane, codexRolloutHeld: pane.heldRollout}}

		err := sendStartupNudgeWithRetry(context.Background(),
			func() error { return ops.sendKeys(session, codexTurnMarker) },
			func(d time.Duration) { <-time.After(d) },
			func() (bool, error) { return ops.paneBusy(session) })
		pane.stop()

		assertHeldSubmitDeferred(t, pane, err)
		if recorded, _, _ := pane.snapshot(); len(recorded) != 0 {
			t.Fatalf("Codex recorded %q, want nothing while the startup turn runs", recorded)
		}
	})
}
