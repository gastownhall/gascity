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
	// mandateRE matches a gc command that claims work or acknowledges a
	// drain. The first match on a line is the mandate for that line.
	mandateRE = longest(`gc [a-z][a-z0-9 -]*(claim|drain-ack)[a-z0-9 -]*`)

	// demandKeyRE matches reserved gc.* metadata keys a pack requires.
	demandKeyRE = longest(`gc\.(output_json[a-z_]*|run_target|kind|scope_[a-z]+|on_fail|continuation_group)`)

	// demandSchemaRE matches a quoted, versioned schema id such as "name.v1".
	demandSchemaRE = longest(`"[a-z][a-z0-9_.-]*\.v[0-9]+"`)

	// judgmentSurfaceRE matches pack-relative paths whose prose tells an
	// agent how to decide: role prompts and template fragments.
	judgmentSurfaceRE = longest(`(^|/)(roles/agents|agents|template-fragments|prompts)/`)

	// normRE matches a lower-cased line that states a rule.
	normRE = longest(`(^|[^a-z])(must not|must never|must|never|always|do not|only permitted|is the only|may not|shall)([^a-z]|$)`)

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
// run, in order of appearance, one per line at most, without duplicates.
// A mandate is a gc command that claims work or acknowledges a drain, for
// example "gc hook --claim --json" or "gc runtime drain-ack". Markdown
// quoting, list markers and backticks are removed and whitespace is
// collapsed.
//
// ExtractMandates reads text only; callers decide which files are prompts.
func ExtractMandates(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range splitLines(text) {
		m, ok := mandateOnLine(line)
		if !ok || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

func mandateOnLine(line string) (string, bool) {
	loc := mandateRE.FindStringIndex(line)
	if loc == nil {
		return "", false
	}
	m := strings.TrimRight(normalize(line[loc[0]:loc[1]]), " ")
	return m, m != ""
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
	if len(s) <= normsMinLength || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "//") {
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
// them: a trailing newline does not start an extra empty line.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
