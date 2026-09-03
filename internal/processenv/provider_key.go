package processenv

import (
	"os"
	"regexp"
)

// envNameRe matches a legal environment variable name. os.Expand's grammar is
// looser than this — "$$" parses as a reference named "$" — so a name it
// yields still has to be checked before it can be treated as a variable.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvName reports whether key is a legal environment variable name.
func ValidEnvName(key string) bool { return envNameRe.MatchString(key) }

// SoleReferencedEnvVar reports the one environment variable that value
// interpolates, when value references exactly that variable and no other.
//
// It reads value with os.Expand — the same grammar [ExpandSessionEnvValue]
// uses at session start — so it honors both ${VAR} and bare $VAR. A narrower
// grammar would silently disagree with the process it describes: a provider
// declaring ANTHROPIC_API_KEY = "$ACME_KEY" would look like it referenced
// nothing at all.
//
// Literal text around the reference is accepted, and so is repeating the same
// variable: "Bearer ${GW_KEY}" references exactly GW_KEY, so changing GW_KEY
// changes what the harness receives and the "Bearer " prefix survives
// expansion untouched.
//
// That acceptance is a decision, not an oversight. Refusing such a value would
// protect nothing — a caller acts on the NAMED VARIABLE, never on the value,
// so the literal cannot be lost — while pushing an operator to edit the
// credential source by hand, which is the one path with no checks at all. The
// refusal would only make sense for a caller that overwrote the whole value,
// and none does.
//
// What is not accepted is a value referencing two different variables: no
// single variable then determines the credential, and naming one of them would
// describe a change that leaves the other in place.
//
// ok is false for a static literal (there is no variable to change) and for a
// value referencing more than one distinct variable.
func SoleReferencedEnvVar(value string) (name string, ok bool) {
	seen := make(map[string]bool, 2)
	var first string
	os.Expand(value, func(key string) string {
		if !seen[key] {
			seen[key] = true
			if first == "" {
				first = key
			}
		}
		return ""
	})
	if len(seen) != 1 || !envNameRe.MatchString(first) {
		return "", false
	}
	return first, true
}

// HasEnvRef reports whether value interpolates any environment variable, under
// the same os.Expand grammar [SoleReferencedEnvVar] and
// [ExpandSessionEnvValue] use.
//
// It exists so a caller can tell "this value is a literal" apart from "this
// value references several variables" — two situations that need different,
// actionable answers rather than one vague refusal.
func HasEnvRef(value string) bool {
	referenced := false
	os.Expand(value, func(string) string {
		referenced = true
		return ""
	})
	return referenced
}
