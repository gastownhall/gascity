package runtime

import (
	"fmt"
	"regexp"
	"strings"
)

// The tmux input fence, shared by the local tmux provider and the box tmux
// carrier ([NewTmuxCarrier]), which cannot import the tmux package. A pane's
// fence is the pane user option TmuxInputFenceOption holding
// "<digest>.<pane_id>": the fence's digest ([InputFence.Digest]) and the
// pane's own id. The id makes a value inherited from window, session or
// global scope (which format lookup would show on every pane without its
// own) read as no fence at all. Like any pane option it survives
// respawn-pane, and a new pane starts without it. A new digest format needs a
// new option name, since a pane never re-adopts an own value.
const (
	// TmuxInputFenceOption is the pane user option holding a pane's fence.
	TmuxInputFenceOption = "@gc_fence"
	// TmuxFenceReadFormat reads a pane's fence value, id and death.
	TmuxFenceReadFormat = "#{" + TmuxInputFenceOption + "}|#{pane_id}|#{pane_dead}"
	// TmuxFenceMissPrefix leads the output of a guarded command's else
	// branch, followed by TmuxFenceReadFormat's expansion.
	TmuxFenceMissPrefix = "gc-fence-miss:"
)

// tmuxKeyNameRe is a key name a guarded send-keys may carry: tmux key names
// (Enter, C-c, Up, a digit), never text. Fence digests match it too.
var tmuxKeyNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+_-]{0,31}$`)

// TmuxKeyName reports whether k may be sent by a guarded send-keys: a tmux
// key name, never text.
func TmuxKeyName(k string) bool { return tmuxKeyNameRe.MatchString(k) }

// TmuxFenceValue is the set-option -F value that makes digest the fence of
// the pane the option is set on.
func TmuxFenceValue(digest string) string { return digest + ".#{pane_id}" }

// TmuxPublishInputFenceArgs is the tmux command (no leading "tmux") that sets
// f as the fence of target's pane. It carries the digest, never the token.
func TmuxPublishInputFenceArgs(target string, f InputFence) []string {
	return []string{"set-option", "-p", "-F", "-t", target, TmuxInputFenceOption, TmuxFenceValue(f.Digest())}
}

// TmuxClearInputFenceArgs is the tmux command (no leading "tmux") that
// removes target's pane fence, for a pane respawned as an incarnation with no
// identity: its predecessor's fence must not name it.
func TmuxClearInputFenceArgs(target string) []string {
	return []string{"set-option", "-p", "-u", "-t", target, TmuxInputFenceOption}
}

// TmuxFenceRead is one read of a pane's fence (TmuxFenceReadFormat). A read
// with no pane id found no session: tmux answers display-message -t and
// if-shell -F -t for a session that no longer exists (while the server
// lives) with exit 0 and every field empty.
type TmuxFenceRead struct {
	Value, Pane string
	Dead        bool
}

// ParseTmuxFenceRead parses TmuxFenceReadFormat's expansion; false when out
// is not one.
func ParseTmuxFenceRead(out string) (TmuxFenceRead, bool) {
	parts := strings.Split(strings.TrimSpace(out), "|")
	switch {
	case len(parts) != 3:
		return TmuxFenceRead{}, false
	case parts[1] == "":
		return TmuxFenceRead{}, true
	case !TmuxPaneID(parts[1]):
		return TmuxFenceRead{}, false
	}
	return TmuxFenceRead{Value: parts[0], Pane: parts[1], Dead: parts[2] == "1"}, true
}

// Absent reports whether the read found no session.
func (r TmuxFenceRead) Absent() bool { return r.Pane == "" }

// Own is the digest the pane holds as its own fence, "" when it holds none
// (no value, or one inherited from another scope or another pane).
func (r TmuxFenceRead) Own() string {
	digest, ok := strings.CutSuffix(r.Value, "."+r.Pane)
	if !ok {
		return ""
	}
	return digest
}

// Found is what a fence miss on r reports ([FenceMismatchError.Found]).
func (r TmuxFenceRead) Found() string {
	switch {
	case r.Absent():
		return FenceFoundAbsent
	case r.Dead:
		return FenceFoundDead
	case r.Own() != "":
		return r.Own()
	}
	return FenceFoundUnproven
}

// TmuxPaneID reports whether s is a tmux pane id ("%" and digits).
func TmuxPaneID(s string) bool {
	if len(s) < 2 || s[0] != '%' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// TmuxGuardedInputArgs is the tmux command (no leading "tmux") that runs
// inner only while target is live pane pane holding digest as its own fence;
// otherwise it deletes buffer (when set) and prints TmuxFenceMissPrefix and
// what it found. The condition names pane's id too: tmux evaluates it in the
// window's active pane when target's pane is gone, and that context must
// miss. Every argument of inner must pass [TmuxCommandString] with only the
// guard's own formats.
func TmuxGuardedInputArgs(target, pane, digest string, inner []string, buffer string) ([]string, error) {
	if !TmuxKeyName(digest) {
		return nil, fmt.Errorf("fenced input: invalid digest %q", digest)
	}
	if !TmuxPaneID(pane) {
		return nil, fmt.Errorf("fenced input: target %q names no pane", target)
	}
	then, err := TmuxCommandString(TmuxGuardFormatArg, inner...)
	if err != nil {
		return nil, err
	}
	show, err := TmuxCommandString(TmuxGuardFormatArg, "display-message", "-p", "-t", target, TmuxFenceMissPrefix+TmuxFenceReadFormat)
	if err != nil {
		return nil, err
	}
	elseCmd := show
	if buffer != "" {
		del, err := TmuxCommandString(TmuxGuardFormatArg, "delete-buffer", "-b", buffer)
		if err != nil {
			return nil, err
		}
		elseCmd = del + " ; " + show
	}
	cond := "#{&&:#{==:#{pane_id}," + pane + "},#{&&:#{==:#{" + TmuxInputFenceOption + "}," + digest + ".#{pane_id}},#{!=:#{pane_dead},1}}}"
	return []string{"if-shell", "-F", "-t", target, cond, then, elseCmd}, nil
}

// TmuxCommandString renders args as one tmux command for if-shell, each
// argument single-quoted. Arguments tmux would parse or expand (a quote, a
// control byte, or a format ownFormat does not accept) are refused: only a
// fenced operation's own targets, key names, buffer names and formats reach
// it.
func TmuxCommandString(ownFormat func(string) bool, args ...string) (string, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		if a == "" {
			return "", fmt.Errorf("fenced tmux command: empty argument")
		}
		for _, r := range a {
			if r == '\'' || r < 0x20 || r == 0x7f {
				return "", fmt.Errorf("fenced tmux command: argument %q is unsafe", a)
			}
		}
		if strings.Contains(a, "#{") && !ownFormat(a) {
			return "", fmt.Errorf("fenced tmux command: argument %q holds a format", a)
		}
		quoted[i] = "'" + a + "'"
	}
	return strings.Join(quoted, " "), nil
}

// TmuxGuardFormatArg reports whether a is a format the guard writes itself:
// a fence value or the miss report.
func TmuxGuardFormatArg(a string) bool {
	switch {
	case strings.HasSuffix(a, ".#{pane_id}") && TmuxKeyName(strings.TrimSuffix(a, ".#{pane_id}")):
		return true
	case a == TmuxFenceMissPrefix+TmuxFenceReadFormat:
		return true
	}
	return false
}
