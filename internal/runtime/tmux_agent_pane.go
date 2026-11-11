package runtime

import (
	"errors"
	"strings"
)

// TmuxAgentPaneOption is the tmux pane user option that marks a session's
// agent pane, shared by the local tmux and ssh providers. Its value is the
// owning session's id ("$3"), so a gc agent pane joined into another session,
// or an option set at window, session or global scope (which format lookup
// would inherit onto every pane), never reads as that session's agent. Every create path sets it on the session's only pane, then sets
// TmuxAgentSessionOption on the session. A pane option belongs to the
// pane, not its position: it survives respawn-pane, split-window -b,
// swap-pane, rotate-window, break-pane and new-window -b, and a pane a human
// adds does not inherit it.
//
// list-panes rows lead with TmuxAgentPaneMarkerFormat, which expands to a
// word rather than the raw option (command output is trimmed, so an empty
// leading field would vanish from the first row):
//   - "agent": the session's marked agent pane;
//   - "orphan": any other pane of a marked session;
//   - "pane": a pane of a session created before the marker.
const (
	TmuxAgentPaneOption       = "@gc_agent"
	TmuxAgentSessionOption    = "@gc_agent_session"
	TmuxAgentPaneMarkerFormat = "#{?#{==:#{" + TmuxAgentPaneOption + "},#{session_id}},agent,#{?#{" + TmuxAgentSessionOption + "},orphan,pane}}"
	TmuxAgentPaneMarked       = "agent"
	TmuxAgentPaneOrphan       = "orphan"
)

// ErrTmuxAgentPaneGone reports a marked session whose agent pane no longer exists
// (a human killed it and the session lives on in their panes). Callers fail
// closed: writes refuse, IsPaneDead reports dead, the kill plan is empty.
var ErrTmuxAgentPaneGone = errors.New("agent pane is gone")

// ErrTmuxAgentPaneAmbiguous reports more than one pane marked as a session's
// agent pane. Callers fail closed rather than guess.
var ErrTmuxAgentPaneAmbiguous = errors.New("agent pane is ambiguous")

// PickTmuxAgentPaneRow returns the fields of the one "agent" row of list-panes
// output whose rows are "<agent|orphan|pane>\t<fields>". With no agent row,
// a marked session (any "orphan" row) has lost its agent pane
// (ErrTmuxAgentPaneGone), and an unmarked one falls back to its first row. Two
// agent rows are ErrTmuxAgentPaneAmbiguous. Empty output yields "", which each
// reader reports as its own empty-answer error.
func PickTmuxAgentPaneRow(out string) (string, error) {
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	agent, marked := -1, false
	for i, row := range rows {
		switch mark, _, _ := strings.Cut(row, "\t"); mark {
		case TmuxAgentPaneMarked:
			if agent >= 0 {
				return "", ErrTmuxAgentPaneAmbiguous
			}
			agent = i
		case TmuxAgentPaneOrphan:
			marked = true
		}
	}
	switch {
	case agent >= 0:
		_, fields, _ := strings.Cut(rows[agent], "\t")
		return fields, nil
	case marked:
		return "", ErrTmuxAgentPaneGone
	}
	_, fields, _ := strings.Cut(rows[0], "\t")
	return fields, nil
}
