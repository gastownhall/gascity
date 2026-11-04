package bdflags

import (
	"fmt"
	"strings"
)

// SplitGlobalFlags splits a bd argv into its subcommand and the arguments that
// follow it, skipping the values of global value-flags.
//
// Taking the first non-dash token as the subcommand reads a global flag's VALUE
// instead: `bd --actor bob update <id> ...` yields "bob". Anything keyed off the
// subcommand — a mutation guard, a routing decision — is then bypassed by a
// token the caller never meant as a verb.
func SplitGlobalFlags(args []string) (string, []string) {
	globals := GlobalValueFlags()
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:]
		}
		// An inline --flag=value consumes nothing further.
		if strings.IndexByte(a, '=') < 0 && globals[a] && i+1 < len(args) {
			i++
		}
	}
	return "", nil
}

// SplitGlobalFlagsStrict is SplitGlobalFlags for a caller that must know the
// subcommand bd runs rather than guess it: ok is false when the argv does not
// say which one that is.
//
// cobra finds the subcommand before it parses a single flag, and while it
// looks it knows only the root command's flags: it takes the token after any
// other flag as that flag's value, so `bd --reason show close <id>` runs close
// with the reason "show", where SplitGlobalFlags reads show. Before the
// subcommand only the global flags are read, then. The help flag is not one of
// them yet (cobra adds it to the command it has found), so a separate -h or
// --help takes the next token too. Any other dash token, a "--" (cobra stops
// looking there) or an empty token (which it skips) leaves ok false. An argv
// with no subcommand returns "" and true.
func SplitGlobalFlagsStrict(args []string) (verb string, rest []string, ok bool) {
	at, ok := commandPathIndex(args, true)
	if !ok || at == len(args) {
		return "", nil, ok
	}
	return args[at], args[at+1:], true
}

// SplitSubcommandStrict reads the sub-verb of a compound subcommand (the "add"
// of `comments add`) the way SplitGlobalFlagsStrict reads the subcommand.
// args is the whole argv without the subcommand: cobra looks for the sub-verb
// in every other token, the flags before the subcommand included, and it
// looks with the subcommand's flags, which inherit the persistent global
// flags but not the root's own -V/--version, so only those may come before
// it. subArgs is the argv the sub-verb's command parses: args without the
// sub-verb. With no sub-verb, sub is "" and subArgs is args.
func SplitSubcommandStrict(args []string) (sub string, subArgs []string, ok bool) {
	at, ok := commandPathIndex(args, false)
	if !ok || at == len(args) {
		return "", args, ok
	}
	subArgs = append(append(make([]string, 0, len(args)-1), args[:at]...), args[at+1:]...)
	return args[at], subArgs, true
}

// rootOnlyFlags are the global bool flags only bd's root command declares; a
// subcommand looking for its sub-verb does not know them.
var rootOnlyFlags = map[string]bool{"-V": true, "--version": true}

// helpFlags are the spellings of the help flag, which cobra declares on a
// command only once it has found it.
var helpFlags = map[string]bool{"-h": true, "--help": true}

// commandPathIndex returns the index of the first token of args cobra takes
// as a command name, past the flags it reads while it resolves the command
// path, or len(args) when there is none. ok is false when a token before it
// is one cobra may read otherwise than this scan does. root reports the root
// command's own resolution, which also knows -V/--version.
func commandPathIndex(args []string, root bool) (int, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a != "" && !strings.HasPrefix(a, "-") {
			return i, true
		}
		name, _, inline := strings.Cut(a, "=")
		switch {
		case globalValueFlags[name] || helpFlags[name]:
			if !inline {
				i++ // the value, whatever it looks like
			}
		case globalBoolFlags[name] && (root || !rootOnlyFlags[name]):
		default:
			return i, false
		}
	}
	return len(args), true
}

// Positionals returns the positional arguments of a bd subcommand's argv,
// skipping every token consumed as a flag's value.
//
// It needs the FULL value-flag set for the subcommand, not a subset: with a
// partial set the value of any omitted flag is read as a positional. That is how
// `update <id> --add-label role=worker` came to look like a stray key=value
// token rather than a flag value.
func Positionals(sub string, args []string) []string {
	needsValue := ValueFlags(sub)
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positionals = append(positionals, a)
			continue
		}
		hasInlineValue := strings.IndexByte(a, '=') >= 0
		name := a
		if hasInlineValue {
			name = a[:strings.IndexByte(a, '=')]
		}
		if !hasInlineValue && needsValue[name] && i+1 < len(args) {
			i++
		}
	}
	return positionals
}

// DroppedMetadataPairs returns the bare key=value tokens sitting in `bd update`'s
// positional issue-id slot — the --set-metadata pairs bd is about to discard.
//
// --set-metadata is a repeatable flag taking ONE pair per occurrence, so in
// `--set-metadata a=1 b=2 c=3` only a=1 is the flag's value; b=2 and c=3 become
// positional issue ids. bd fails to resolve them, reports the failures on
// stderr, prints its success line for the id that did resolve, and EXITS 0 —
// so a caller cannot distinguish a full write from a 1-of-N write, and no
// exit-code check in any script can see it.
//
// No bead id contains '=', so such a positional never resolved under bd either:
// reporting one cannot condemn an invocation that previously worked.
func DroppedMetadataPairs(args []string) []string {
	var dropped []string
	for _, p := range Positionals("update", args) {
		if strings.IndexByte(p, '=') >= 0 {
			dropped = append(dropped, p)
		}
	}
	return dropped
}

// DroppedMetadataRefusal builds the refusal message for a `bd update` whose
// metadata pairs would be silently dropped, or reports false for any other verb
// and every well-formed invocation. prefix names the entry point (e.g. "gc bd").
//
// The message names each dropped pair and the form that works, because the
// caller's own output gives it nothing: bd prints success and exits 0 however
// many pairs it discarded.
func DroppedMetadataRefusal(prefix, verb string, args []string) (string, bool) {
	if verb != "update" {
		return "", false
	}
	dropped := DroppedMetadataPairs(args)
	if len(dropped) == 0 {
		return "", false
	}
	corrected := make([]string, 0, len(dropped))
	for _, pair := range dropped {
		corrected = append(corrected, "--set-metadata "+pair)
	}
	subject, object := "it", "an issue id"
	if len(dropped) > 1 {
		subject, object = "them", "issue ids"
	}
	return fmt.Sprintf(
		"%s: refusing update: %s would be dropped. --set-metadata takes ONE key=value per flag, so bd reads %s as %s, fails to resolve %s, and still exits 0 after writing only the first pair. Repeat the flag instead: %s\n",
		prefix,
		strings.Join(dropped, ", "),
		strings.Join(dropped, ", "),
		object,
		subject,
		strings.Join(corrected, " "),
	), true
}
