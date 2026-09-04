package processenv

import (
	"os"
	"regexp"
)

// envNameRe matches a legal environment variable name. os.Expand's grammar is
// looser than this — "$$" parses as a reference named "$" — so a name it
// yields still has to be checked before it can be treated as a variable.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ReferencedEnvVars returns the distinct environment variables a
// config-authored env value interpolates, in first-appearance order.
//
// It reads value with os.Expand — the same grammar [ExpandSessionEnvValue]
// uses at session start — so it honors both ${VAR} and bare $VAR. A narrower
// grammar would silently disagree with the process it describes: a provider
// declaring ANTHROPIC_API_KEY = "$ACME_KEY" would look like it referenced
// nothing at all.
//
// The count is what callers reason about:
//
//   - none: the value is a literal, so there is no variable to change;
//   - exactly one: that variable determines the value even when literal text
//     surrounds it, because a caller acts on the VARIABLE — changing GW_KEY in
//     "Bearer ${GW_KEY}" moves the secret and leaves "Bearer " untouched
//     through expansion, so refusing such a value would protect nothing while
//     pushing an operator toward editing the source by hand;
//   - more than one: no single variable determines the value, and naming one
//     of them would describe a change that leaves the others in place.
//
// Names os.Expand yields that are not legal variable names are skipped, so
// "$$" reads as a literal rather than a reference to a variable named "$".
func ReferencedEnvVars(value string) []string {
	var names []string
	seen := make(map[string]bool, 2)
	os.Expand(value, func(key string) string {
		if envNameRe.MatchString(key) && !seen[key] {
			seen[key] = true
			names = append(names, key)
		}
		return ""
	})
	return names
}
