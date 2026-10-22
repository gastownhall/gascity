package bdflags

import "strings"

// ClaimFlag reports the flag that makes a bd subcommand claim work, given
// the argv after the subcommand: `update --claim`, `ready --claim`,
// `close --claim-next`, and `close --continue` unless `--no-auto` turns its
// auto-claim of the next molecule step off. A "--" terminator ends the scan;
// an explicit "=false" value does not claim.
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
	noAuto := boolFlagSet(args, "--no-auto")
	for _, f := range claimFlags {
		if !boolFlagSet(args, f) {
			continue
		}
		if f == "--continue" && noAuto {
			continue
		}
		return f, true
	}
	return "", false
}

// boolFlagSet reports whether a boolean flag is set before "--": bare, or
// with an inline value other than "false".
func boolFlagSet(args []string, flag string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == flag {
			return true
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok && !strings.EqualFold(strings.TrimSpace(v), "false") {
			return true
		}
	}
	return false
}
