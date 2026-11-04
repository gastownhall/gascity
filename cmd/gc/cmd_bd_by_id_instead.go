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

// bdByIDReasonFileStdinInstead answers `close --reason-file -`: stdin is the
// one reason source the in-process close does not read.
const bdByIDReasonFileStdinInstead = "the in-process close does not read stdin; pass the reason in a file (`--reason-file <path>`) or inline (`gc bd close %s --reason <text>`)"

// bdByIDCommentsReadInstead and bdByIDCommentsWriteInstead answer a comment
// read or write of a bead the binding holds: the class store contract carries
// no comments, so there are none to list and nowhere to add one.
const (
	bdByIDCommentsReadInstead  = "the class store contract carries no comments, so the bead has none to list; `gc bd show %s --json` reads the whole record"
	bdByIDCommentsWriteInstead = "the class store contract carries no comments; keep the text as metadata instead: `gc bd update %s --set-metadata <key>=<text>`"
)

// bdByIDUnservedAlternative returns what to run instead of an unserved
// invocation that addresses id, a bead the class binding holds, or "" when
// there is nothing to name. flag is the spelling the served parser rejected
// (bdByIDUnservedFlag), or "".
func bdByIDUnservedAlternative(bdArgs []string, id, flag string) string {
	verb := bdByIDRefusedVerb(bdArgs, id)
	switch {
	case verb == "heartbeat":
		return fmt.Sprintf(bdByIDHeartbeatInstead, id)
	case verb == "close" && flag == "--reason-file "+bdByIDReasonFileStdin:
		return fmt.Sprintf(bdByIDReasonFileStdinInstead, id)
	case verb == "label add" || verb == "label remove":
		return bdByIDLabelInstead(verb, bdArgs, id)
	case verb == "comments" || verb == "comments list":
		return fmt.Sprintf(bdByIDCommentsReadInstead, id)
	case verb == "comment" || verb == "comments add":
		return fmt.Sprintf(bdByIDCommentsWriteInstead, id)
	}
	droppable := bdByIDDroppableFlags[verb]
	if droppable == nil {
		return ""
	}
	rest, dropped := bdArgvWithout(bdArgs, droppable)
	if len(dropped) == 0 {
		return ""
	}
	var losses, thens, keeps []string
	for _, flag := range dropped {
		spec := droppable[flag]
		losses = appendUniqueString(losses, spec.lost)
		if spec.then != "" {
			thens = appendUniqueString(thens, spec.then)
		}
		if spec.keep != "" {
			keeps = appendUniqueString(keeps, fmt.Sprintf(spec.keep, id))
		}
	}
	var parts []string
	if _, served := parseBdByIDOp(rest); served {
		run := fmt.Sprintf("run it without %s (%s): %s", strings.Join(dropped, " and "), strings.Join(losses, "; "), bdByIDCommandText(rest))
		for _, then := range thens {
			run += fmt.Sprintf(", then `%s`", then)
		}
		parts = append(parts, run)
	}
	parts = append(parts, keeps...)
	return strings.Join(parts, "; ")
}

// bdByIDLabelInstead spells `label add|remove <ids...> <label>` as the served
// update of id: --add-label or --remove-label with the same label. bd takes the
// last positional as the label. bdflags carries no manifest for the label
// verbs, so the argv after the verb's own words is read directly.
func bdByIDLabelInstead(verb string, bdArgs []string, id string) string {
	var positionals []string
	for _, arg := range bdArgsAfterWords(bdArgs, strings.Fields(verb)) {
		if !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) < 2 {
		return ""
	}
	flag := "--add-label"
	if verb == "label remove" {
		flag = "--remove-label"
	}
	// bd's label verbs take a comma list; update takes one label per flag.
	argv := []string{"update", id}
	for _, label := range strings.Split(positionals[len(positionals)-1], ",") {
		if label = strings.TrimSpace(label); label != "" {
			argv = append(argv, flag, label)
		}
	}
	if len(argv) == 2 {
		return ""
	}
	return "the served update takes the same labels: " + bdByIDCommandText(argv)
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
// `--flag=value` one — and the flags it removed, in the order first seen.
// Everything after `--` is positional and kept.
func bdArgvWithout(argv []string, drop map[string]bdByIDDroppableFlag) (rest, dropped []string) {
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
		if spec.takesValue && !inline {
			i++
		}
	}
	return rest, dropped
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
	return "`" + shellquote.Join(append([]string{"gc", "bd"}, argv...)) + "`"
}
