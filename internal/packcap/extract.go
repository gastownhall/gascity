package packcap

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// normsLineLimit bounds a NORMS value, in bytes, after normalization.
const normsLineLimit = 160

// normsMinLength is the shortest normalized line, in bytes, that counts as
// a norm. Shorter lines are headings or fragments, not rules.
const normsMinLength = 12

var (
	// demandKeyRE matches reserved gc.* metadata keys a pack requires. The
	// boundaries keep gc.kindred from reading as gc.kind.
	demandKeyRE = longest(`\bgc\.(output_json[a-z_]*|run_target|kind|scope_[a-z]+|on_fail|continuation_group)\b`)

	// demandSchemaRE matches a quoted, versioned schema id such as "name.v1".
	demandSchemaRE = longest(`"[a-z][a-z0-9_.-]*\.v[0-9]+"`)

	// judgmentSurfaceRE matches pack-relative paths whose prose tells an
	// agent how to decide: role prompts and template fragments.
	judgmentSurfaceRE = longest(`(^|/)(roles/agents|agents|template-fragments|prompts)/`)

	// normRE matches a lower-cased line that states a rule.
	normRE = longest(`(^|[^a-z])(must not|must never|must|never|always|do not|only permitted|is the only|may not|shall)([^a-z]|$)`)

	// mandateWordRE matches a gc subcommand word such as "hook" or
	// "drain-ack".
	mandateWordRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

	// mandateFlagRE matches a flag, optionally with an attached value.
	mandateFlagRE = regexp.MustCompile(`^--?([a-z][a-z0-9-]*)(=\S+)?$`)

	// mandateArgRE matches an argument of a known shape: a <placeholder>, a
	// shell variable, optionally double-quoted, or a {{ template }} value.
	mandateArgRE = regexp.MustCompile(`^(<[^<>\s]+>|"?\$\{?[A-Za-z_][A-Za-z0-9_]*\}?"?|\{\{[^{}]*\}\}\S*)$`)

	leadingMarkupRE = longest(`^[ \t>*-]+`)
	trailingSpaceRE = longest(`[ \t]+$`)
	innerSpaceRE    = longest(`[ \t]+`)
)

// longest compiles a pattern with leftmost-longest matching, the POSIX
// semantics the original awk extractor used.
func longest(pattern string) *regexp.Regexp {
	re := regexp.MustCompile(pattern)
	re.Longest()
	return re
}

// ExtractMandates returns the commands a piece of prose tells an agent to
// run, in order of appearance, without duplicates. A mandate is a gc command
// that claims work or acknowledges a drain, for example
// "gc hook --claim --json" or "gc runtime drain-ack".
//
// A mandate is "gc" followed by its subcommand words, then its flags and
// arguments of a known shape (<placeholder>, $VAR, {{ template }}). It ends
// at the first other token: prose, a shell operator or redirection, a
// comment, a backtick, or a token ending in punctuation (kept without the
// punctuation). Subcommand words end at "claim" or "drain-ack". A line may
// hold several mandates. A command counts when a subcommand word is claim or
// drain-ack, or a flag is --claim or --drain-ack.
//
// ExtractMandates reads text only; callers decide which files are prompts.
func ExtractMandates(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range splitLines(text) {
		for _, m := range mandatesOnLine(line) {
			if seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// mandatesOnLine returns the mandates on one line, in order.
func mandatesOnLine(line string) []string {
	var out []string
	for i := 0; i+3 <= len(line); i++ {
		if line[i:i+2] != "gc" || !gcBoundaryBefore(line, i) || (line[i+2] != ' ' && line[i+2] != '\t') {
			continue
		}
		if m := mandateAt(line[i+2:]); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// gcBoundaryBefore reports whether "gc" at line[i] starts a word: it is at
// the start of the line or follows a character that cannot belong to a
// longer name.
func gcBoundaryBefore(line string, i int) bool {
	if i == 0 {
		return true
	}
	c := line[i-1]
	isNameChar := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '-' || c == '.' || c == '/' || c == '$'
	return !isNameChar
}

// mandateAt reads the command whose "gc" precedes rest and returns it, or
// "" when the command neither claims work nor acknowledges a drain.
func mandateAt(rest string) string {
	if j := strings.IndexByte(rest, '`'); j >= 0 {
		rest = rest[:j]
	}
	tokens := []string{"gc"}
	claims, wordsDone := false, false
	for _, tok := range strings.Fields(rest) {
		final := false
		if !mandateArgRE.MatchString(tok) {
			if trimmed := strings.TrimRight(tok, ".,;:!?)'\""); trimmed != tok {
				tok, final = trimmed, true
			}
		}
		switch {
		case !wordsDone && mandateWordRE.MatchString(tok):
			if tok == "claim" || tok == "drain-ack" {
				claims, wordsDone = true, true
			}
		case mandateFlagRE.MatchString(tok):
			wordsDone = true
			if name := mandateFlagRE.FindStringSubmatch(tok)[1]; name == "claim" || name == "drain-ack" {
				claims = true
			}
		case mandateArgRE.MatchString(tok):
			wordsDone = true
		default:
			tok, final = "", true
		}
		if tok != "" {
			tokens = append(tokens, tok)
		}
		if final {
			break
		}
	}
	if !claims {
		return ""
	}
	return strings.Join(tokens, " ")
}

// demandsOnLine returns the reserved metadata keys and schema ids a line
// requires, in order of appearance.
func demandsOnLine(line string) []string {
	var out []string
	out = append(out, demandKeyRE.FindAllString(line, -1)...)
	for _, s := range demandSchemaRE.FindAllString(line, -1) {
		out = append(out, "schema:"+s[1:len(s)-1])
	}
	return out
}

// isJudgmentSurface reports whether a pack-relative, slash-separated path
// holds prose whose normative lines count as NORMS.
func isJudgmentSurface(rel string) bool {
	return judgmentSurfaceRE.MatchString(rel)
}

// normOnLine returns the normalized normative statement on a line of a
// judgment-surface file, truncated to normsLineLimit bytes.
func normOnLine(line string) (string, bool) {
	if !normRE.MatchString(strings.ToLower(line)) {
		return "", false
	}
	s := normalize(line)
	if len(s) < normsMinLength || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "//") {
		return "", false
	}
	return truncateBytes(s, normsLineLimit), true
}

// normalize strips leading quote and list markup, trailing whitespace and
// backticks, and collapses runs of spaces and tabs to one space.
func normalize(s string) string {
	s = leadingMarkupRE.ReplaceAllString(s, "")
	s = trailingSpaceRE.ReplaceAllString(s, "")
	s = innerSpaceRE.ReplaceAllString(s, " ")
	return strings.ReplaceAll(s, "`", "")
}

// truncateBytes cuts s to at most n bytes without splitting a UTF-8 rune.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// splitLines splits text into lines the way a line-oriented reader sees
// them: a trailing newline does not start an extra empty line, and a
// carriage return before a newline is not part of the line.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}
