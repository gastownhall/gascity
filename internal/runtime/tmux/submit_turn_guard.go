package tmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// A submit whose keys would interrupt a running turn (Codex's Escape-led
// submit, see submitSequenceInterrupts) must reach the pane only when the
// agent is waiting for input, and must be judged delivered only on evidence
// that the agent took the message. The live failure this guards against
// (healthy-direct, 2026-10-15, codex-cli 0.162.1 on a contended host): the
// first Escape+Enter started the turn, the pane drew no busy row within the
// confirm window, the confirm loop sent Escape+Enter again, and that Escape
// aborted the turn 1609 ms in, before Codex recorded the message. The drained
// composer then had the submit reported delivered.

// awaitIdleComposerBeforeTyping waits, up to the nudge ready timeout, until
// target shows an idle composer: no busy indicator, a ready prompt (the
// session's configured prefix or its family's composer prompt) on
// IdleRequiredObservations consecutive looks, and, for Codex, a startup that
// has finished (runtime.CodexStartupPending). A pane still running a turn when
// the wait runs out gets nothing typed: the error wraps runtime.ErrSessionBusy
// so the caller delivers later. A pane the looks cannot read as either keeps
// the historical behavior and is typed into once the wait runs out.
func (t *Tmux) awaitIdleComposerBeforeTyping(session, target string, submitKeys []string) error {
	family := t.nudgeSubmitFamily(target)
	prefixes := []string{t.resolveIdlePromptPrefix(session)}
	if marker, ok := t.stagedDraftMarkerFor(target); ok {
		prefixes = append(prefixes, marker.promptPrefix)
	}
	deadline := time.Now().Add(t.cfg.NudgeReadyTimeout)
	idle, busy := 0, false
	for {
		lines, err := t.CapturePaneLines(target, promptObservationLines)
		switch {
		case err != nil:
			if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer) {
				return err
			}
			idle, busy = 0, false
		case paneShowsRunningTurn(lines, family):
			idle, busy = 0, true
		case composerReadyForInput(lines, prefixes):
			idle, busy = idle+1, false
			if idle >= IdleRequiredObservations {
				return nil
			}
		default:
			idle, busy = 0, false
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(IdlePollInterval)
	}
	if busy {
		return fmt.Errorf("%w: session %q is still running a turn and its submit keys (%s) would interrupt it",
			runtime.ErrSessionBusy, session, strings.Join(submitKeys, " "))
	}
	return nil
}

// turnRunning reports whether target's agent, of provider family, is running
// a turn, read the way a submit whose keys would interrupt one must read it
// (see paneShowsRunningTurn).
func (t *Tmux) turnRunning(target, family string) (bool, error) {
	lines, err := t.CapturePaneLines(target, promptObservationLines)
	if err != nil {
		return false, err
	}
	return paneShowsRunningTurn(lines, family), nil
}

// TurnRunning reports whether session's agent is running a turn now, read
// from its agent pane the way the nudge's turn guard reads it before typing
// (paneShowsRunningTurn): for Codex the status rows right above its live
// composer, so a Working row an earlier turn left in the scrollback is not a
// running turn; for every other family a busy indicator anywhere in the
// capture, as SnapshotIdle reads it. A pane that cannot be resolved or
// captured is an error, never a reading.
func (t *Tmux) TurnRunning(session string) (bool, error) {
	target, err := t.agentInputTarget(session)
	if err != nil {
		return false, err
	}
	return t.turnRunning(target, t.nudgeSubmitFamily(target))
}

// codexTurnStatusRows bounds the rows read above Codex's live composer for
// its status: the "• Working (… esc to interrupt)" row and the queued
// follow-ups Codex lists under it.
const codexTurnStatusRows = 6

// paneShowsRunningTurn reports whether lines, the bottom of a pane running
// family, show a turn in progress now.
//
// For Codex it reads the live screen relative to the composer: the turn's
// status row sits in the block of rows right above the live composer (the
// last composer prompt), and only Codex's own "esc to interrupt" there
// counts. The rest of the capture is history. Codex leaves an earlier bottom
// pane, Working row and composer included, in the scrollback when an approval
// overlay redraws the screen (approvals.repeated, codex-cli 0.162.1): read
// anywhere in the capture, that stale row held every later Escape-led submit
// back as busy for good, and a transcript row shaped like another provider's
// indicator (a tool's "(2s · 3.1 MB)" reads as Claude's spinner) did the same.
// A Codex pane with no composer on screen cannot be read that way and falls
// back to Codex's indicator anywhere in lines. Every other family keeps
// paneContainsBusyIndicator.
func paneShowsRunningTurn(lines []string, family string) bool {
	if family != "codex" {
		return paneContainsBusyIndicator(lines)
	}
	const codexBusy = "esc to interrupt"
	composer := -1
	for i, line := range lines {
		if matchesPromptPrefix(line, codexPromptPrefix) {
			composer = i
		}
	}
	if composer < 0 {
		return slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, codexBusy) })
	}
	row := composer - 1
	for row >= 0 && strings.TrimSpace(lines[row]) == "" {
		row--
	}
	for n := 0; row >= 0 && n < codexTurnStatusRows && strings.TrimSpace(lines[row]) != ""; row, n = row-1, n+1 {
		if strings.Contains(lines[row], codexBusy) {
			return true
		}
	}
	return false
}

// composerReadyForInput reports whether lines show an idle ready prompt for
// one of prefixes, from a Codex that has finished starting.
func composerReadyForInput(lines []string, prefixes []string) bool {
	if runtime.CodexStartupPending(strings.Join(lines, "\n")) {
		return false
	}
	for _, prefix := range prefixes {
		if PaneShowsIdlePrompt(lines, prefix) {
			return true
		}
	}
	return false
}

// submitStillWaiting reports whether target is idle with a message still
// sitting unsubmitted in its composer: the one state in which sending an
// interrupting submit sequence again is safe and needed. Busy is read from a
// plain capture, the composer from one with dim placeholder text removed.
func (t *Tmux) submitStillWaiting(target, family string) (bool, error) {
	busy, err := t.turnRunning(target, family)
	if err != nil || busy {
		return false, err
	}
	lines, err := t.captureInputLines(target)
	if err != nil {
		return false, err
	}
	return paneShowsUnsubmittedInput(lines), nil
}

// paneShowsUnsubmittedInput reports whether the live composer of a family
// with a known composer prompt (stagedDraftMarkers) holds input: typed text or
// a staged-paste placeholder. A pane with no such composer on screen cannot be
// read, so it reports false. Dim placeholder text must already be removed
// (see captureInputLines).
func paneShowsUnsubmittedInput(lines []string) bool {
	for _, line := range lines {
		for _, marker := range stagedDraftMarkers {
			if matchesPromptPrefix(line, marker.promptPrefix) {
				return !paneShowsDrainedComposer(lines)
			}
		}
	}
	return false
}

// codexSubmitRecordWait bounds how long a Codex submit whose turn was never
// seen busy waits for Codex's rollout to say whether the message landed.
// Codex records the message only after its UserPromptSubmit hooks have run,
// which can take seconds on a loaded host.
const codexSubmitRecordWait = 10 * time.Second

// codexSubmitRecordTailBytes is how much of the rollout's end is read for the
// submit's record: enough for a large pasted prompt and the rows around it.
const codexSubmitRecordTailBytes = 512 * 1024

// unobservedSubmitVerdict judges a submit whose turn was never seen busy but
// whose composer drained. For a Codex session whose own rollout it can name
// (see codexRolloutOf), that rollout decides, within codexSubmitRecordWait:
// the message recorded as a user turn is a delivery (nil); the turn aborted
// before the message was recorded is a loss (ErrNudgeSubmitLost). Without
// that rollout, or without a verdict in time, the drained composer stands as
// delivery that was not observed (ErrNudgeSubmitDeliveredUnobserved).
func (t *Tmux) unobservedSubmitVerdict(session string, typedAt time.Time, message string) error {
	unobserved := fmt.Errorf("%w: session %q", ErrNudgeSubmitDeliveredUnobserved, session)
	if sessionlog.ProviderFamily(t.providerEnv(session)) != "codex" {
		return unobserved
	}
	deadline := time.Now().Add(codexSubmitRecordWait)
	rollout := ""
	for {
		// Codex creates its rollout lazily, on the first turn it records, so
		// a fresh session's file can appear only after the submit.
		if rollout == "" {
			rollout = t.codexRolloutOf(session)
		}
		if rollout != "" {
			switch codexSubmitRecordIn(rollout, typedAt, message) {
			case codexSubmitLanded:
				return nil
			case codexSubmitLost:
				return fmt.Errorf("%w: session %q: Codex's rollout records the turn aborted and never the message", ErrNudgeSubmitLost, session)
			}
		}
		if !time.Now().Before(deadline) {
			return unobserved
		}
		time.Sleep(IdlePollInterval)
	}
}

// codexRolloutOf returns the rollout the session's own Codex process holds
// open, or "" when that cannot be told. The process is the identity: Codex
// sessions commonly share a home (no CODEX_HOME, so ~/.codex) and a working
// directory, so the newest rollout under the home, or one matched by its
// recorded cwd, can be another session's.
func (t *Tmux) codexRolloutOf(session string) string {
	out, err := t.GetPanePID(session)
	if err != nil {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid <= 1 {
		return ""
	}
	held := t.codexRolloutHeld
	if held == nil {
		held = heldCodexRollout
	}
	return held(pid)
}

// heldCodexRollout returns the one Codex rollout held open by the process
// tree rooted at panePID, read from the live process table.
func heldCodexRollout(panePID int) string {
	records, err := proctable.SnapshotProcesses()
	if err != nil {
		return ""
	}
	return selectHeldCodexRollout(records, panePID, proctable.OpenFiles)
}

// selectHeldCodexRollout returns the one Codex rollout that root or any of
// its descendants in records holds open, per openFiles. Two different
// rollouts held in the tree, or a process whose open files cannot be read,
// return "": a guess could judge this submit by another conversation's
// record.
func selectHeldCodexRollout(records []proctable.ProcessRecord, root int, openFiles func(int) ([]string, error)) string {
	children := make(map[int][]int, len(records))
	for _, r := range records {
		if r.PID != root {
			children[r.PPID] = append(children[r.PPID], r.PID)
		}
	}
	held := ""
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		files, err := openFiles(pid)
		if err != nil {
			return ""
		}
		for _, file := range files {
			if !isCodexRolloutPath(file) || file == held {
				continue
			}
			if held != "" {
				return ""
			}
			held = file
		}
		for _, child := range children[pid] {
			if !seen[child] {
				seen[child] = true
				queue = append(queue, child)
			}
		}
	}
	return held
}

// isCodexRolloutPath reports whether path names a Codex rollout,
// <home>/sessions/YYYY/MM/DD/rollout-<stamp>-<id>.jsonl.
func isCodexRolloutPath(path string) bool {
	name := filepath.Base(path)
	return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") &&
		strings.Contains(filepath.ToSlash(path), "/sessions/")
}

// codexSubmitRecord is what Codex's rollout says about a submitted message.
type codexSubmitRecord int

const (
	// codexSubmitUnknown: nothing since the submit decides it yet.
	codexSubmitUnknown codexSubmitRecord = iota
	// codexSubmitLanded: Codex recorded the message as a user turn.
	codexSubmitLanded
	// codexSubmitLost: a turn aborted since the submit, and the message was
	// never recorded.
	codexSubmitLost
)

func (r codexSubmitRecord) String() string {
	switch r {
	case codexSubmitLanded:
		return "landed"
	case codexSubmitLost:
		return "lost"
	default:
		return "unknown"
	}
}

// codexSubmitRecordIn reads the rollout at path for the message submitted at
// since.
func codexSubmitRecordIn(path string, since time.Time, message string) codexSubmitRecord {
	info, err := os.Stat(path)
	if err != nil || info.ModTime().Before(since.Truncate(time.Second)) {
		return codexSubmitUnknown
	}
	tail, err := readFileTail(path, codexSubmitRecordTailBytes)
	if err != nil {
		return codexSubmitUnknown
	}
	return readCodexSubmitRecord(tail, since, message)
}

// readCodexSubmitRecord reads a rollout tail for the message submitted at
// since. Only rows Codex stamped at or after since count, so an earlier
// turn's abort says nothing about this submit. The message is matched with
// its whitespace collapsed, inside a user response item or a user_message
// event. Rows that do not parse (the tail's first, torn line) are skipped.
func readCodexSubmitRecord(tail string, since time.Time, message string) codexSubmitRecord {
	want := collapseSpace(message)
	if want == "" {
		return codexSubmitUnknown
	}
	cutoff := since.Truncate(time.Millisecond)
	landed, aborted := false, false
	for _, line := range strings.Split(tail, "\n") {
		var row struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &row) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil || at.Before(cutoff) {
			continue
		}
		var payload struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Message string `json:"message"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(row.Payload, &payload) != nil {
			continue
		}
		switch {
		case row.Type == "response_item" && payload.Type == "message" && payload.Role == "user":
			var text strings.Builder
			for _, part := range payload.Content {
				text.WriteString(part.Text)
				text.WriteString("\n")
			}
			switch {
			case strings.Contains(text.String(), "<turn_aborted>"):
				aborted = true
			case strings.Contains(collapseSpace(text.String()), want):
				landed = true
			}
		case row.Type == "event_msg" && payload.Type == "user_message":
			if strings.Contains(collapseSpace(payload.Message), want) {
				landed = true
			}
		case row.Type == "event_msg" && payload.Type == "turn_aborted":
			aborted = true
		}
	}
	switch {
	case landed:
		return codexSubmitLanded
	case aborted:
		return codexSubmitLost
	default:
		return codexSubmitUnknown
	}
}

// collapseSpace folds every run of whitespace in s to one space.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
