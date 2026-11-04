package bdflags

import (
	"strconv"
	"strings"
)

// ClaimFlag reports the flag that makes a bd subcommand claim work, given
// the argv after the subcommand: `update --claim`, `ready --claim`,
// `close --claim-next`, and `close --continue` unless `--no-auto` turns its
// auto-claim of the next molecule step off.
//
// The argv is read the way pflag reads it: a value flag of the subcommand
// takes the next token whatever it looks like ("--reason --no-auto" sets no
// --no-auto), the last occurrence of a flag wins, and an inline value is
// parsed with strconv.ParseBool. It fails closed: an inline value that does
// not parse counts as SET for a claim flag and UNSET for --no-auto.
//
// It is the one place callers learn which argv claims, so a guard that must
// vet claims (the rig namespace guard of `gc bd`) cannot miss a spelling.
func ClaimFlag(sub string, args []string) (string, bool) {
	var claimFlags []string
	switch sub {
	case "update", "ready":
		claimFlags = []string{"--claim"}
	case "close":
		claimFlags = []string{"--claim-next", "--continue"}
	default:
		return "", false
	}
	values := ValueFlags(sub)
	noAuto := false
	if present, on, valid := BoolFlagValue(args, values, "--no-auto"); present {
		noAuto = on && valid
	}
	for _, f := range claimFlags {
		present, on, valid := BoolFlagValue(args, values, f)
		if !present || (valid && !on) {
			continue
		}
		if f == "--continue" && noAuto {
			continue
		}
		return f, true
	}
	return "", false
}

// BoolFlagValue reads a boolean flag (any of names, the long and short
// spellings) from an argv the way pflag does: tokens after "--" are
// positional, a flag in valueFlags consumes the next token, and the last
// occurrence wins. present reports whether the flag occurs at all; on is
// its value (a bare flag is true); valid is false when the last occurrence
// carries an inline value strconv.ParseBool rejects.
func BoolFlagValue(args []string, valueFlags map[string]bool, names ...string) (present, on, valid bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, value, inline := strings.Cut(a, "=")
		if matchesName(name, names) {
			present = true
			if !inline {
				on, valid = true, true
				continue
			}
			b, err := strconv.ParseBool(strings.TrimSpace(value))
			on, valid = b, err == nil
			continue
		}
		if !inline && valueFlags[name] && i+1 < len(args) {
			i++
		}
	}
	return present, on, valid
}

// FlagValues returns the values given to any of names in an argv read the
// way pflag reads it (see BoolFlagValue), in order, in the separate and the
// inline spelling. A flag whose value is missing at the end of the argv
// yields nothing.
//
// A shorthand name ("-C") is read in every spelling pflag accepts: "-C dir",
// "-C=dir" and "-Cdir", and inside a run of shorthands, "-qC dir", "-qC=dir"
// and "-qCdir" (shorthandRunValue).
func FlagValues(args []string, valueFlags map[string]bool, names ...string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if len(a) < 2 || a[0] != '-' {
			continue // a lone "-" is a positional too
		}
		if a[1] != '-' {
			value, found, next := shorthandRunValue(a[1:], valueFlags, names)
			if next {
				if i+1 == len(args) {
					break
				}
				i++
				value = args[i]
			}
			if found {
				out = append(out, value)
			}
			continue
		}
		name, value, inline := strings.Cut(a, "=")
		if matchesName(name, names) {
			if inline {
				out = append(out, value)
			} else if i+1 < len(args) {
				out = append(out, args[i+1])
				i++
			}
			continue
		}
		if !inline && valueFlags[name] && i+1 < len(args) {
			i++
		}
	}
	return out
}

// shorthandRunValue reads a single-dash token without its dash ("C", "qCdir")
// as pflag reads a run of shorthands, for the value it gives any of names.
// Each shorthand takes the rest after an "=" as its value ("-q=x", "-qC=dir");
// one that valueFlags lists, or one of names, takes the rest of the run
// ("-Cdir"), or the next token when it ends the run (next). Any other
// shorthand is read as a bool, and the run goes on: found is false when the
// value went to another flag ("-aC" gives -a the value "C").
func shorthandRunValue(run string, valueFlags map[string]bool, names []string) (value string, found, next bool) {
	for i := 0; i < len(run); i++ {
		flag, rest := "-"+run[i:i+1], run[i+1:]
		named := matchesName(flag, names)
		if len(rest) > 1 && rest[0] == '=' {
			return rest[1:], named, false
		}
		if !named && !valueFlags[flag] {
			continue
		}
		return rest, named, rest == ""
	}
	return "", false, false
}

func matchesName(name string, names []string) bool {
	for _, n := range names {
		if name == n {
			return true
		}
	}
	return false
}
