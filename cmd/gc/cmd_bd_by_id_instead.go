package main

// What to run instead when the by-ID door refuses.
//
// A refusal that names no alternative leaves its reader holding a command that
// does not work and no command that does (bdRelocatedClassMintPaths makes the
// same point for creates). So every refusal the door writes about a bead the
// class binding holds ends with the command that does the job, as far as the
// class store contract can carry it: the same command without the spelling the
// contract has no place for, when what is left is a command the door serves,
// and the gc command that covers the dropped part, when there is one.
//
// Nothing here decides anything about the bead. The alternatives are argv
// rewrites and fixed sentences; whether to take one is the caller's call.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// bdByIDDroppableFlag is a flag the class store contract has no place for,
// whose absence still leaves the rest of the command meaningful.
type bdByIDDroppableFlag struct {
	// takesValue reports whether the flag's separated spelling consumes the
	// next token.
	takesValue bool
	// lost says what dropping the flag gives up.
	lost string
	// then is the gc command that does the dropped part, run after the
	// command without the flag, or "".
	then string
	// keep is a sentence, with a %s for the bead id, naming where the text
	// the flag carried can go instead, or "".
	keep string
}

// bdByIDDroppableFlags are, per verb, the flags an alternative drops. They are
// the flags the served parsers reject although bd accepts them; any other
// rejected flag has no alternative this file can name.
var bdByIDDroppableFlags = map[string]map[string]bdByIDDroppableFlag{
	"close": {
		"--actor":        {takesValue: true, lost: "the class store records no close actor"},
		"--session":      {takesValue: true, lost: "the class store records no closing session"},
		"--force":        {lost: "the class store's close runs none of the pinned-issue or gate checks it waives"},
		"-f":             {lost: "the class store's close runs none of the pinned-issue or gate checks it waives"},
		"--claim-next":   {lost: "the in-process close claims nothing", then: "gc hook --claim"},
		"--continue":     {lost: "the in-process close advances no molecule", then: "gc hook --claim"},
		"--no-auto":      {lost: "it only qualifies --continue"},
		"--suggest-next": {lost: "the in-process close lists nothing it unblocked", then: "gc ready"},
	},
	"reopen": {
		"--actor":  {takesValue: true, lost: "the class store records no reopen actor"},
		"--reason": {takesValue: true, lost: "the class store's reopen records no reason"},
		"-r":       {takesValue: true, lost: "the class store's reopen records no reason"},
	},
	"update": {
		"--actor":        {takesValue: true, lost: "the class store records no update actor"},
		"--notes":        {takesValue: true, lost: "a bead in the class store has no notes field", keep: bdByIDNotesKeep},
		"--append-notes": {takesValue: true, lost: "a bead in the class store has no notes field", keep: bdByIDNotesKeep},
	},
}

// bdByIDNotesKeep names where a note goes on a bead with no notes field: the
// bead's metadata, the way the packs record a report on a relocated wisp
// (mol-dog-stale-db's record_report).
const bdByIDNotesKeep = "keep the text as metadata instead: `gc bd update %s --set-metadata <key>=<text>`"

// bdByIDHeartbeatInstead answers a heartbeat of a bead the binding holds. bd's
// heartbeat pushes its claim lease forward; a claim the class store holds has
// no lease, so there is nothing to push.
const bdByIDHeartbeatInstead = "a claim the class store holds carries no lease, so there is nothing to refresh: it stays held until it is released or the bead closes, and `gc bd show %s --json` shows the holder"

// bdByIDCommentsReadInstead and bdByIDCommentsWriteInstead answer a comment
// read or write of a bead the binding holds: the class store contract carries
// no comments, so there are none to list and nowhere to add one.
const (
	bdByIDCommentsReadInstead  = "the class store contract carries no comments, so the bead has none to list; `gc bd show %s --json` reads the whole record"
	bdByIDCommentsWriteInstead = "the class store contract carries no comments; keep the text as metadata instead: `gc bd update %s --set-metadata <key>=<text>`"
)

// bdByIDResidence is what an alternative needs to know about where the ids of
// a refused command live: the binding's name for the sentence, and whether it
// holds a given id. An error from holds is a residence the door could not
// decide, and the alternative then names nothing rather than guess.
type bdByIDResidence struct {
	binding string
	holds   func(id string) (bool, error)
}

// bdByIDNoAutoThen is what follows a close whose --continue carried --no-auto:
// bd then shows the next step without claiming it, which is a look at the
// frontier, not a claim.
const bdByIDNoAutoThen = "gc ready"

// bdByIDSplitRest joins the class half of a split alternative to its work half.
const bdByIDSplitRest = ", and the rest as typed, which bd answers from the work store: "

// bdByIDUnservedAlternative returns what to run instead of an unserved
// invocation that addresses id, a bead the class binding holds, or "" when
// there is nothing to name. flag is the spelling the served parser rejected
// (bdByIDUnservedFlag), or "".
//
// An alternative is a command the reader can run as it stands. A close batch
// that also names ids the binding does not hold is spelled as two commands:
// the ids the binding holds, which the door serves, and the rest exactly as
// typed, which bd serves from the work store — dropping a flag from the work
// half would change a command bd can run, and naming the whole batch would
// name a command the door refuses.
func bdByIDUnservedAlternative(bdArgs []string, id, flag string, res bdByIDResidence) string {
	verb := bdByIDRefusedVerb(bdArgs, id)
	switch {
	case verb == "heartbeat":
		return fmt.Sprintf(bdByIDHeartbeatInstead, id)
	case verb == "close" && flag == bdByIDReasonFileStdinFlag:
		return bdByIDCloseStdinInstead(bdArgs, res)
	case verb == "label add" || verb == "label remove":
		return bdByIDLabelInstead(verb, bdArgs, res)
	case verb == "comments" || verb == "comments list":
		return fmt.Sprintf(bdByIDCommentsReadInstead, id)
	case verb == "comment" || verb == "comments add":
		return fmt.Sprintf(bdByIDCommentsWriteInstead, id)
	}
	droppable := bdByIDDroppableFlags[verb]
	if droppable == nil {
		return ""
	}
	rest, dropped, removed := bdArgvWithout(bdArgs, droppable)
	if len(dropped) == 0 {
		return ""
	}
	var losses, thens, keeps []string
	for _, flag := range dropped {
		spec := droppable[flag]
		losses = appendUniqueString(losses, spec.lost)
		then := spec.then
		if flag == "--continue" && slices.Contains(dropped, "--no-auto") {
			then = bdByIDNoAutoThen
		}
		if then != "" {
			thens = appendUniqueString(thens, then)
		}
		if spec.keep != "" {
			keeps = appendUniqueString(keeps, fmt.Sprintf(spec.keep, id))
		}
	}
	var parts []string
	without := fmt.Sprintf("without %s (%s)", strings.Join(dropped, " and "), strings.Join(losses, "; "))
	if run := bdByIDRunWithout(rest, removed, without, thens, res); run != "" {
		parts = append(parts, run)
	}
	parts = append(parts, keeps...)
	return strings.Join(parts, "; ")
}

// bdByIDRunWithout spells rest — the refused argv without the dropped flags,
// which removed holds as typed — as the command to run, or "" when there is
// none: rest is not a command the door serves, it is a reason shape bd itself
// refuses, or it is a batch carrying a flag bd takes on one id only. without
// says what was dropped and what that gives up, and thens are the gc commands
// that do the dropped part, run after.
func bdByIDRunWithout(rest, removed []string, without string, thens []string, res bdByIDResidence) string {
	op, served := parseBdByIDOp(rest)
	if !served || op.Close.Invalid != "" {
		return ""
	}
	whole := "run it " + without + ": " + bdByIDCommandText(rest)
	for _, then := range thens {
		whole += fmt.Sprintf(", then `%s`", then)
	}
	if op.Verb != bdByIDClose {
		return whole
	}
	if len(op.Close.IDs) > 1 && bdArgvHasAny(removed, bdByIDSingleIDCloseFlags) {
		return ""
	}
	held, notHeld, ok := bdByIDCloseResidence(op, res)
	switch {
	case !ok || len(held) == 0:
		return ""
	case len(notHeld) == 0:
		return whole
	}
	// The work half keeps every flag it was typed with, so it does the
	// dropped part itself and no gc command follows.
	return fmt.Sprintf("run the ids %s holds %s: %s%s%s", res.binding, without,
		bdByIDCommandText(bdByIDCloseArgv(op, held)), bdByIDSplitRest,
		bdByIDCommandText(append(bdByIDCloseArgv(op, notHeld), removed...)))
}

// bdByIDSingleIDCloseFlags are the close flags bd refuses on more than one id
// ("--continue only works when closing a single issue").
var bdByIDSingleIDCloseFlags = map[string]bool{"--continue": true, "--suggest-next": true}

// bdArgvHasAny reports whether argv carries any of the named flags, in either
// spelling.
func bdArgvHasAny(argv []string, names map[string]bool) bool {
	for _, arg := range argv {
		if name, _, _ := strings.Cut(arg, "="); names[name] {
			return true
		}
	}
	return false
}

// bdByIDCloseResidence splits a close's ids by where they live: the indices
// the binding holds and the rest. ok is false when a residence could not be
// decided.
func bdByIDCloseResidence(op bdByIDOp, res bdByIDResidence) (held, notHeld []int, ok bool) {
	known := make(map[string]bool, len(op.Close.IDs))
	for i, id := range op.Close.IDs {
		holds, seen := known[id]
		if !seen {
			var err error
			if holds, err = res.holds(id); err != nil {
				return nil, nil, false
			}
			known[id] = holds
		}
		if holds {
			held = append(held, i)
		} else {
			notHeld = append(notHeld, i)
		}
	}
	return held, notHeld, true
}

// bdByIDCloseStdinInstead answers `close --reason-file -`: stdin is the one
// reason source the in-process close does not read. The ids the binding holds
// take the reason from a file or inline; the rest keep `--reason-file -`,
// which bd reads.
func bdByIDCloseStdinInstead(bdArgs []string, res bdByIDResidence) string {
	rest, removed := bdArgvWithoutReasonFileStdin(bdArgs)
	op, served := parseBdByIDOp(rest)
	if !served || op.Verb != bdByIDClose || op.Close.Invalid != "" || len(op.Close.Reasons) > 0 {
		// A reason beside the file is bd's own refusal of the command.
		return ""
	}
	held, notHeld, ok := bdByIDCloseResidence(op, res)
	if !ok || len(held) == 0 {
		return ""
	}
	class := bdByIDCloseArgv(op, held)
	text := "the in-process close does not read stdin; pass the reason in a file or inline: " +
		bdByIDCommandTextWith(class, "--reason-file <path>") + " or " + bdByIDCommandTextWith(class, "--reason <text>")
	if len(notHeld) > 0 {
		text += bdByIDSplitRest + bdByIDCommandText(append(bdByIDCloseArgv(op, notHeld), removed...))
	}
	return text
}

// bdByIDReasonFileStdinFlag is the rejected spelling parseBdByIDCloseArgs
// reports for a reason file read from stdin.
const bdByIDReasonFileStdinFlag = "--reason-file " + bdByIDReasonFileStdin

// bdArgvWithoutReasonFileStdin returns argv without `--reason-file -` (either
// spelling), and the tokens it removed, as typed.
func bdArgvWithoutReasonFileStdin(argv []string) (rest, removed []string) {
	rest = make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			return append(rest, argv[i:]...), removed
		case arg == "--reason-file="+bdByIDReasonFileStdin:
			removed = append(removed, arg)
		case arg == "--reason-file" && i+1 < len(argv) && argv[i+1] == bdByIDReasonFileStdin:
			removed = append(removed, arg, argv[i+1])
			i++
		default:
			rest = append(rest, arg)
		}
	}
	return rest, removed
}

// bdByIDLabelInstead spells `label add|remove <ids...> <labels>` as the served
// update of each id the binding holds, one --add-label or --remove-label per
// label, and the ids it does not hold as the label write as typed. bd takes
// the last positional as a comma list of labels. bdflags carries no manifest
// for the label verbs, so the argv after the verb's own words is read
// directly.
func bdByIDLabelInstead(verb string, bdArgs []string, res bdByIDResidence) string {
	var positionals []string
	for _, arg := range bdArgsAfterWords(bdArgs, strings.Fields(verb)) {
		if !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) < 2 {
		return ""
	}
	ids, labels := positionals[:len(positionals)-1], positionals[len(positionals)-1]
	flag := "--add-label"
	if verb == "label remove" {
		flag = "--remove-label"
	}
	var updates, notHeld []string
	for _, id := range ids {
		holds, err := res.holds(id)
		if err != nil {
			return ""
		}
		if !holds {
			notHeld = append(notHeld, id)
			continue
		}
		// bd's label verbs take a comma list; update takes one label per flag.
		argv := []string{"update", id}
		for _, label := range strings.Split(labels, ",") {
			if label = strings.TrimSpace(label); label != "" {
				argv = append(argv, flag, label)
			}
		}
		if len(argv) == 2 {
			return ""
		}
		updates = append(updates, bdByIDCommandText(argv))
	}
	if len(updates) == 0 {
		return ""
	}
	text := "the served update takes the same labels: " + strings.Join(updates, ", ")
	if len(notHeld) > 0 {
		argv := append(strings.Fields(verb), notHeld...)
		text += bdByIDSplitRest + bdByIDCommandText(append(argv, labels))
	}
	return text
}

// bdArgsAfterWords returns the argv after the first run of words, in order,
// that spells the verb, or nil when argv does not spell it.
func bdArgsAfterWords(bdArgs, words []string) []string {
	for i := 0; i+len(words) <= len(bdArgs); i++ {
		match := true
		for j, word := range words {
			if bdArgs[i+j] != word {
				match = false
				break
			}
		}
		if match {
			return bdArgs[i+len(words):]
		}
	}
	return nil
}

// bdArgvWithout returns argv with every flag named in drop removed — both its
// separated spelling, with the value a value flag consumes, and its inline
// `--flag=value` one — the flags it removed, in the order first seen, and the
// removed tokens as typed. Everything after `--` is positional and kept.
func bdArgvWithout(argv []string, drop map[string]bdByIDDroppableFlag) (rest, dropped, removed []string) {
	rest = make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			rest = append(rest, argv[i:]...)
			break
		}
		name, _, inline := strings.Cut(arg, "=")
		spec, ok := drop[name]
		if !ok || !strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			continue
		}
		dropped = appendUniqueString(dropped, name)
		removed = append(removed, arg)
		if spec.takesValue && !inline && i+1 < len(argv) {
			i++
			removed = append(removed, argv[i])
		}
	}
	return rest, dropped, removed
}

// bdByIDReadInstead returns what to run instead of an unserved by-ID read of a
// bead the class binding holds: the served spelling for each subject, or, for
// a flag bd's own read lacks, the served command that asks for what it meant.
func bdByIDReadInstead(manifest bdByIDReadManifest, flag string, subjects []string) string {
	if hint := bdReadFlagInstead(manifest.verb, flag, subjects); hint != "" {
		return hint
	}
	options := ""
	if flag != "" {
		options = bdByIDServedReadOptions[manifest.verb]
	}
	commands := make([]string, 0, len(subjects))
	for _, id := range subjects {
		commands = append(commands, fmt.Sprintf("`gc bd %s %s%s`", manifest.verb, id, options))
	}
	if flag == "" {
		return "read each id on its own: " + strings.Join(commands, ", ")
	}
	return "the served spelling is " + strings.Join(commands, ", ")
}

// bdByIDServedReadOptions are the options each by-ID read's served parser
// accepts, in the form a refusal prints after the id.
var bdByIDServedReadOptions = map[string]string{
	"show":     " [--json] [--include-comments]",
	"dep list": " [--direction=up|down] [--type=<type>] [--json]",
	"dep tree": " [--direction=up|down] [--max-depth=<n>] [--json]",
}

// bdReadFlagHint names what a flag bd's own read lacks was asking for, and the
// command, with a %s for the id, that asks for it in a spelling bd takes.
type bdReadFlagHint struct {
	what    string
	command string
}

// bdReadFlagHints are, per read verb, flags bd itself rejects for which gc
// knows the spelling that was meant.
var bdReadFlagHints = map[string]map[string]bdReadFlagHint{
	"show": {"--comments": {what: "comments", command: "`gc bd show %s --json --include-comments`"}},
}

// bdReadFlagInstead returns the sentence naming the spelling bd takes for a
// flag its read verb does not have, for ids, or "" when gc knows none. The by-ID
// door's refusal and the split allowlist's both say it.
func bdReadFlagInstead(verb, flag string, ids []string) string {
	hint, ok := bdReadFlagHints[verb][flag]
	if !ok {
		return ""
	}
	commands := make([]string, 0, len(ids))
	for _, id := range ids {
		commands = append(commands, fmt.Sprintf(hint.command, id))
	}
	return fmt.Sprintf("bd's %s has no %s flag; ask for %s with %s", verb, flag, hint.what, strings.Join(commands, " and "))
}

// bdByIDCommandText spells argv as the `gc bd` command a reader can paste.
func bdByIDCommandText(argv []string) string {
	return bdByIDCommandTextWith(argv, "")
}

// bdByIDCommandTextWith spells argv as bdByIDCommandText does, followed by
// placeholder, a fragment like `--reason <text>` the reader fills in, which
// is left unquoted.
func bdByIDCommandTextWith(argv []string, placeholder string) string {
	text := shellquote.Join(append([]string{"gc", "bd"}, argv...))
	if placeholder != "" {
		text += " " + placeholder
	}
	return "`" + text + "`"
}
