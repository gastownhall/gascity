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
