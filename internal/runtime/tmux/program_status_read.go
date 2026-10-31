package tmux

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// The read half of program-status capture (design §7.1). While the city
// switch is shadow or on, the state cache's list-panes and the runtime
// inventory batch append each pane's capture fields to their rows; a
// session's reading is judged from its agent pane's row alone, with the
// tap's liveness taken from the process table the state cache already
// scans. While the switch is off nothing here runs: the read formats are
// today's byte for byte and every reading is Unread.

// programReadSanitizer is the read format's filter for a capture option:
// tmux removes every byte outside the codec's alphabet before printing the
// value, so no option value can forge a field (a tab) or a row (a newline).
// The codec's values pass it unchanged (programstatus
// TestEncodeSurvivesServerSanitizerUnchanged).
const programReadSanitizer = "[^-+A-Za-z0-9=_.|%]"

// programPaneFormat is a pane's capture fields, appended to a list-panes -a
// row only while the switch is not off: the agent-pane marker, the pane id,
// whether a pipe-pane reader is attached, and the sanitized tap nonce and
// record. The row's own pane_dead field completes the agent-pane read. An
// option set at no scope expands empty; one set at window, session or global
// scope is inherited, which the judgement's pane-bound nonce compare tells
// apart.
const programPaneFormat = agentPaneMarkerFormat + "\t#{pane_id}\t#{pane_pipe}\t" +
	"#{s/" + programReadSanitizer + "//:" + programTapOption + "}\t" +
	"#{s/" + programReadSanitizer + "//:" + programStatusOption + "}"

// programPaneFields is the field count of programPaneFormat.
const programPaneFields = 5

// programPane is one pane's capture fields as list-panes printed them.
type programPane struct {
	mark   string // the agent-pane marker word: agent, orphan or pane
	id     string // the pane id ("%3")
	dead   bool
	pipe   bool   // a pipe-pane reader is attached
	tap    string // the tap nonce as the pane reads it, inherited values included
	record string // the published record, sanitized
}

// parseProgramPane parses programPaneFields fields of a row whose pane_dead
// field read dead. It reports false for a field tmux would never print: an
// unknown marker word, a malformed pane id, or a pane_pipe that is not 0 or 1.
func parseProgramPane(fields []string, dead bool) (programPane, bool) {
	if len(fields) != programPaneFields {
		return programPane{}, false
	}
	pane := programPane{mark: fields[0], id: fields[1], dead: dead, tap: fields[3], record: fields[4]}
	switch pane.mark {
	case runtime.TmuxAgentPaneMarked, runtime.TmuxAgentPaneOrphan, "pane":
	default:
		return programPane{}, false
	}
	if !wellFormedTmuxID(pane.id, '%') {
		return programPane{}, false
	}
	switch fields[2] {
	case "0":
	case "1":
		pane.pipe = true
	default:
		return programPane{}, false
	}
	return pane, true
}

// foldProgramPane returns a session's agent pane among its rows: the pane
// marked as the session's agent pane ([runtime.PickTmuxAgentPaneRow]), else
// the session's only pane. It reports false when no pane can be named: a
// marked session that lost its agent pane, two marked panes, or several
// unmarked panes. It never guesses by position.
func foldProgramPane(rows []programPane) (programPane, bool) {
	if len(rows) == 0 {
		return programPane{}, false
	}
	marked := slices.ContainsFunc(rows, func(p programPane) bool { return p.mark != "pane" })
	if !marked && len(rows) > 1 {
		return programPane{}, false
	}
	lines := make([]string, len(rows))
	for i, p := range rows {
		lines[i] = p.mark + "\t" + strconv.Itoa(i)
	}
	picked, err := runtime.PickTmuxAgentPaneRow(strings.Join(lines, "\n"))
	if err != nil {
		return programPane{}, false
	}
	i, err := strconv.Atoi(picked)
	if err != nil || i < 0 || i >= len(rows) {
		return programPane{}, false
	}
	return rows[i], true
}

// programTaps is the process table's account of capture readers: the nonce
// ("<rand>.<pane_id>") every tap process carries. known is false when the
// table was not read, and then no tap's liveness is known.
type programTaps struct {
	known  bool
	nonces map[string]struct{}
}

// liveness is the tap-liveness tristate for nonce.
func (t programTaps) liveness(nonce string) runtime.ProgramTapLiveness {
	if !t.known {
		return runtime.ProgramTapUnknown
	}
	if _, ok := t.nonces[nonce]; ok {
		return runtime.ProgramTapAlive
	}
	return runtime.ProgramTapGone
}

// tapNonces indexes the nonce of every tap process in s.
func tapNonces(s processSnapshot) map[string]struct{} {
	nonces := make(map[string]struct{})
	for _, process := range s.byPID {
		if nonce, ok := tapNonce(process.Args); ok {
			nonces[nonce] = struct{}{}
		}
	}
	return nonces
}

// tapNonce returns the nonce a tap's argv carries: after its "tap" word,
// "-n <rand>" and "-t <pane_id>" (the arm's tapCommand). The shell tmux runs
// the command under carries the same words until it execs the tap, so a tap
// being started counts as alive.
func tapNonce(args string) (string, bool) {
	fields := strings.Fields(args)
	at := slices.Index(fields, "tap")
	if at < 0 {
		return "", false
	}
	var rand, pane string
	for i := at + 1; i+1 < len(fields); i++ {
		switch fields[i] {
		case "-n":
			rand = fields[i+1]
		case "-t":
			pane = fields[i+1]
		}
	}
	nonce := rand + "." + pane
	if rand == "" || !wellFormedTmuxID(pane, '%') || !programstatus.ValidNonce(nonce) {
		return "", false
	}
	return nonce, true
}

// judgeProgramPane judges a session's reading under mode. named is false
// when the session's agent pane could not be named (see foldProgramPane),
// which reads not_armed: no nonce of ours is bound to a pane the reading
// could name.
func judgeProgramPane(mode runtime.ProgramStatusMode, gate bool, pane programPane, named bool, taps programTaps) runtime.ProgramStatus {
	row := runtime.ProgramPaneRow{Mode: mode, CaptureGate: gate}
	if named {
		row.PaneID, row.PaneDead, row.PanePipe, row.Tap = pane.id, pane.dead, pane.pipe, pane.tap
		row.TapLiveness = taps.liveness(pane.tap)
		record, err := programstatus.Decode(pane.record)
		row.RecordForm, row.Record = programstatus.RecordForm(err), record
	}
	return runtime.JudgeProgramStatus(row)
}

// programStatusMode is the switch this Tmux reads under: shadow or on, and
// off for anything else.
func (t *Tmux) programStatusMode() runtime.ProgramStatusMode {
	switch mode := t.cfg.ProgramStatus; mode {
	case runtime.ProgramStatusShadow, runtime.ProgramStatusOn:
		return mode
	default:
		return runtime.ProgramStatusOff
	}
}

// programReadEnabled reports whether the read formats carry capture fields.
func (t *Tmux) programReadEnabled() bool {
	return t.programStatusMode() != runtime.ProgramStatusOff
}

// ProgramStatus implements [runtime.ProgramStatusProvider]: the session's
// agent-pane reading from the state cache's snapshot (the same 2 s
// list-panes and process-table read liveness takes), judged by
// [runtime.JudgeProgramStatus]. With the switch off it reads nothing and
// returns an Unread reading. A session the snapshot does not hold is an error
// wrapping [runtime.ErrSessionNotFound]; a snapshot the cache cannot vouch
// for, one wrapping [runtime.ErrRuntimeUnavailable].
func (p *Provider) ProgramStatus(name string) (runtime.ProgramStatus, error) {
	mode := p.tm.programStatusMode()
	if mode == runtime.ProgramStatusOff {
		return runtime.ProgramStatus{Mode: runtime.ProgramStatusOff}, nil
	}
	obs := p.cache.observe()
	switch classifyCacheObservation(obs, name, p.cache.clock(), p.cache.staleTTL, p.tm.serverConfirmedDead) {
	case cacheAnswerAbsent:
		return runtime.ProgramStatus{}, fmt.Errorf("program status of %q: %w", name, runtime.ErrSessionNotFound)
	case cacheAnswerUnknown:
		return runtime.ProgramStatus{}, cacheUnknownError(name, obs.lastErr)
	}
	session, ok := obs.state.Sessions[name]
	if !ok {
		return runtime.ProgramStatus{}, fmt.Errorf("program status of %q: %w", name, runtime.ErrSessionNotFound)
	}
	if !obs.state.ProgramRead {
		// A snapshot fetched before this switch was read carries no capture
		// fields: there is nothing to judge.
		return runtime.ProgramStatus{Mode: mode}, nil
	}
	return judgeProgramPane(mode, p.tm.captureGatePassed(), session.Program, session.ProgramNamed, obs.state.programTaps()), nil
}

// judgeInventoryPrograms fills each entry's Program from its session's agent
// pane, judged under mode with the tap liveness of the state cache's process
// table. That table can predate the inventory's listing by up to the cache's
// TTL, so a pane armed in between reads reader_lost until the next refresh:
// a reading that withholds state, never one that grants it.
func (p *Provider) judgeInventoryPrograms(mode runtime.ProgramStatusMode, entries map[string]runtime.InventoryEntry, panes map[string]programPane) {
	gate := p.tm.captureGatePassed()
	taps := p.cache.currentState().programTaps()
	for name, e := range entries {
		pane, named := panes[name]
		e.Program = judgeProgramPane(mode, gate, pane, named, taps)
		entries[name] = e
	}
}

var _ runtime.ProgramStatusProvider = (*Provider)(nil)
