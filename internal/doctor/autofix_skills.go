package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DeprecatedAttachmentFieldsCheck scans user-editable city TOML files
// for the v0.15.0 attachment-list tombstone fields (`skills`, `mcp`,
// `skills_append`, `mcp_append`) and offers a `--fix` rule that strips
// them in place.
//
// The fields parse cleanly in v0.15.1 but are ignored by the new
// materializer; they are scheduled to become a hard parse error in
// v0.16. This check is the migration helper that pairs with the
// load-time deprecation warning emitted by
// config.WarnDeprecatedAttachmentFields.
//
// Scope: only the city's own TOML files
// (`<cityPath>/city.toml` and `<cityPath>/pack.toml` when present).
// Pack-vendored files under `<gcHome>/cache/` and external includes
// are out of scope — the user owns the fix on those surfaces.
type DeprecatedAttachmentFieldsCheck struct{}

// deprecatedAttachmentKeys lists the array-of-string TOML keys that the
// v0.15.1 tombstone covers. Order matters only for deterministic output.
var deprecatedAttachmentKeys = []string{
	"skills",
	"mcp",
	"skills_append",
	"mcp_append",
}

// Name returns the check identifier.
func (c *DeprecatedAttachmentFieldsCheck) Name() string { return "deprecated-attachment-fields" }

// CanFix reports that the check supports automatic remediation.
func (c *DeprecatedAttachmentFieldsCheck) CanFix() bool { return true }

// Run reports a warning when any city TOML file still references the
// tombstone attachment-list fields. Returns OK when none are found.
func (c *DeprecatedAttachmentFieldsCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if ctx == nil || ctx.CityPath == "" {
		r.Status = StatusOK
		r.Message = "no city path provided"
		return r
	}

	hits, err := scanCityForDeprecatedAttachmentFields(ctx.CityPath)
	if err != nil {
		r.Status = StatusError
		r.Message = err.Error()
		return r
	}
	if len(hits) == 0 {
		r.Status = StatusOK
		r.Message = "no deprecated attachment-list fields found"
		return r
	}

	totalLines := 0
	for _, h := range hits {
		totalLines += len(h.Lines)
		r.Details = append(r.Details, formatHit(h))
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf(
		"found deprecated attachment-list field(s) in %d file(s) (%d line range(s))",
		len(hits), totalLines,
	)
	r.FixHint = `run "gc doctor --fix" to strip the deprecated fields`
	return r
}

// Fix strips the tombstone fields from each affected file. Each file
// is rewritten atomically (tmp + rename). Pre-existing comments,
// formatting, and unrelated content are preserved.
func (c *DeprecatedAttachmentFieldsCheck) Fix(ctx *CheckContext) error {
	if ctx == nil || ctx.CityPath == "" {
		return nil
	}
	hits, err := scanCityForDeprecatedAttachmentFields(ctx.CityPath)
	if err != nil {
		return err
	}
	for _, h := range hits {
		if err := rewriteWithoutDeprecatedAttachmentFields(h.Path); err != nil {
			return fmt.Errorf("rewriting %s: %w", h.Path, err)
		}
	}
	return nil
}

// deprecatedAttachmentHit describes the deprecated-field occurrences
// found in a single TOML file.
type deprecatedAttachmentHit struct {
	// Path is the absolute filesystem path to the affected file.
	Path string
	// Lines records each occurrence as a (key, line-number) pair. Line
	// numbers are 1-indexed and point at the assignment line.
	Lines []deprecatedAttachmentLine
}

type deprecatedAttachmentLine struct {
	Key  string
	Line int
}

// scanCityForDeprecatedAttachmentFields walks the well-known city
// TOML files and returns the set of files with stale tombstone fields.
// Files are ordered by path so the report is deterministic.
func scanCityForDeprecatedAttachmentFields(cityPath string) ([]deprecatedAttachmentHit, error) {
	candidates := []string{
		filepath.Join(cityPath, "city.toml"),
		filepath.Join(cityPath, "pack.toml"),
	}
	var hits []deprecatedAttachmentHit
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if info.IsDir() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		matches := findDeprecatedAttachmentFieldLines(string(data))
		if len(matches) == 0 {
			continue
		}
		hits = append(hits, deprecatedAttachmentHit{Path: path, Lines: matches})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Path < hits[j].Path })
	return hits, nil
}

// findDeprecatedAttachmentFieldLines locates each occurrence of a
// tombstone key assignment in the TOML source and returns one
// (key, line) pair per occurrence. Line numbers are 1-indexed and
// point at the assignment line; subsequent lines belonging to the
// same multi-line array are not separately listed.
//
// Lines that fall inside a TOML multi-line string (`"""..."""` or
// `”'...”'`) are opaque content and never match — this prevents
// `gc doctor --fix` from corrupting a `description = """..."""`
// field whose body happens to embed an illustrative `skills = [...]`
// line.
func findDeprecatedAttachmentFieldLines(source string) []deprecatedAttachmentLine {
	var hits []deprecatedAttachmentLine
	lines := splitLinesPreserving(source)
	state := tomlStringState{}
	for i := 0; i < len(lines); i++ {
		if state.inMultiline() {
			state = state.update(lines[i])
			continue
		}
		key, isAssign := matchedDeprecatedKey(lines[i])
		if !isAssign {
			state = state.update(lines[i])
			continue
		}
		hits = append(hits, deprecatedAttachmentLine{Key: key, Line: i + 1})
		consumed := arrayLineSpan(lines, i)
		for j := 0; j < consumed; j++ {
			state = state.update(lines[i+j])
		}
		if consumed > 1 {
			i += consumed - 1
		}
	}
	return hits
}

// rewriteWithoutDeprecatedAttachmentFields rewrites the file at path,
// removing every assignment whose key is one of the tombstone names.
// Multi-line arrays are removed in full. Surrounding lines, comments,
// and section headers are preserved verbatim. Trailing-newline shape
// is preserved when present.
//
// Mirrors findDeprecatedAttachmentFieldLines's multi-line string
// state tracking: lines inside a `"""..."""` or `”'...”'` block
// are never stripped, even if their content syntactically resembles
// a deprecated assignment.
func rewriteWithoutDeprecatedAttachmentFields(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	source := string(data)
	hadTrailingNewline := strings.HasSuffix(source, "\n")
	lines := splitLinesPreserving(source)

	out := make([]string, 0, len(lines))
	state := tomlStringState{}
	for i := 0; i < len(lines); i++ {
		if state.inMultiline() {
			out = append(out, lines[i])
			state = state.update(lines[i])
			continue
		}
		if _, ok := matchedDeprecatedKey(lines[i]); ok {
			consumed := arrayLineSpan(lines, i)
			for j := 0; j < consumed; j++ {
				state = state.update(lines[i+j])
			}
			if consumed > 1 {
				i += consumed - 1
			}
			continue
		}
		out = append(out, lines[i])
		state = state.update(lines[i])
	}

	rendered := strings.Join(out, "\n")
	if hadTrailingNewline && !strings.HasSuffix(rendered, "\n") {
		rendered += "\n"
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
	if _, err := tmp.WriteString(rendered); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// tomlStringState tracks whether the scanner is currently inside an
// open TOML multi-line string. Two flavors: basic (`"""..."""` —
// escape sequences apply) and literal (`”'...”'` — raw content).
//
// The scanner only needs to find the closing triple-quote token; it
// does not need full TOML grammar fidelity. Per-line update is
// recursive so a line that opens AND closes the same flavor
// (`description = """one-liner"""`) leaves the state unchanged.
type tomlStringState struct {
	inBasic   bool
	inLiteral bool
}

// inMultiline reports whether the scanner is mid-multi-line-string at
// the start of the next line.
func (s tomlStringState) inMultiline() bool {
	return s.inBasic || s.inLiteral
}

// update returns the new state after walking line, looking for the
// triple-quote tokens that toggle multi-line state. Operates only on
// the input flavor at most: when inside a basic string only `"""`
// can close it; same for literal `”'`. When outside both, the first
// triple-quote token (whichever flavor) opens its kind and the rest
// of the line is rescanned from inside that state.
func (s tomlStringState) update(line string) tomlStringState {
	if s.inBasic {
		idx := strings.Index(line, `"""`)
		if idx < 0 {
			return s
		}
		s.inBasic = false
		return s.update(line[idx+3:])
	}
	if s.inLiteral {
		idx := strings.Index(line, `'''`)
		if idx < 0 {
			return s
		}
		s.inLiteral = false
		return s.update(line[idx+3:])
	}
	basicIdx := strings.Index(line, `"""`)
	literalIdx := strings.Index(line, `'''`)
	if basicIdx < 0 && literalIdx < 0 {
		return s
	}
	if basicIdx >= 0 && (literalIdx < 0 || basicIdx < literalIdx) {
		s.inBasic = true
		return s.update(line[basicIdx+3:])
	}
	s.inLiteral = true
	return s.update(line[literalIdx+3:])
}

// matchedDeprecatedKey reports whether line is a key assignment for one
// of the tombstone keys, returning the matched key and true. The match
// is anchored: leading whitespace is ignored, the key must be followed
// by `=` (with optional surrounding whitespace), and the line must not
// be a comment.
func matchedDeprecatedKey(line string) (string, bool) {
	stripped := strings.TrimLeft(line, " \t")
	if stripped == "" || strings.HasPrefix(stripped, "#") {
		return "", false
	}
	for _, key := range deprecatedAttachmentKeys {
		if !strings.HasPrefix(stripped, key) {
			continue
		}
		rest := strings.TrimLeft(stripped[len(key):], " \t")
		if strings.HasPrefix(rest, "=") {
			return key, true
		}
	}
	return "", false
}

// arrayLineSpan returns the number of source lines occupied by the
// assignment starting at lines[start]. Returns 1 for a single-line
// value. Returns >1 when the value spans multiple lines via either a
// bracketed array or a multi-line TOML string (`"""..."""` or
// `”'..”'`); the span ends at the line where bracket depth returns
// to 0 and no multi-line string is open.
//
// The scanner tracks all four TOML string flavors so values like
// `skills = [”'contains ] bracket”']` are not prematurely closed
// by a literal `]` inside a string body.
func arrayLineSpan(lines []string, start int) int {
	if start < 0 || start >= len(lines) {
		return 1
	}
	first := lines[start]
	eqIdx := strings.Index(first, "=")
	if eqIdx < 0 {
		return 1
	}
	state := scanBrackets(first[eqIdx+1:], scanState{})
	if state.settled() {
		return 1
	}
	for i := start + 1; i < len(lines); i++ {
		state = scanBrackets(lines[i], state)
		if state.settled() {
			return i - start + 1
		}
	}
	// Unclosed value — give up and treat as single-line so we don't
	// mass-delete the rest of the file. The TOML parser would reject
	// this file anyway.
	return 1
}

// scanState carries the bracket-depth and TOML-string state across a
// scanBrackets call. settled() reports the natural stopping point
// (no open brackets, no open multi-line string).
type scanState struct {
	depth           int
	inBasicSingle   bool // "..."  (single-line basic string, escapes apply)
	inBasicMulti    bool // """..."""  (multi-line basic string, escapes apply)
	inLiteralSingle bool // '...' (single-line literal string, raw)
	inLiteralMulti  bool // '''..'''  (multi-line literal string, raw)
	escape          bool // last byte was `\` inside a basic string
}

// settled reports whether the state represents a closed value: bracket
// depth is zero and no multi-line string is currently open. Single-line
// strings are not allowed to span lines per TOML spec, so they don't
// keep the value open across lines.
func (s scanState) settled() bool {
	return s.depth == 0 && !s.inBasicMulti && !s.inLiteralMulti
}

// scanBrackets walks segment byte-by-byte, updating bracket depth and
// TOML-string state. Triple-quote tokens (`"""`, `”'`) take precedence
// over single-quote tokens — a literal `”'` opens a multi-line literal
// string even though `'` would otherwise open a single-line literal
// string. Comments (`#` outside any string) terminate the line scan
// without altering depth or string state.
func scanBrackets(segment string, state scanState) scanState {
	i := 0
	for i < len(segment) {
		b := segment[i]

		switch {
		case state.inBasicMulti:
			if state.escape {
				state.escape = false
				i++
				continue
			}
			if isTripleQuote(segment, i, '"') {
				state.inBasicMulti = false
				i += 3
				continue
			}
			if b == '\\' {
				state.escape = true
			}
			i++

		case state.inLiteralMulti:
			if isTripleQuote(segment, i, '\'') {
				state.inLiteralMulti = false
				i += 3
				continue
			}
			i++

		case state.inBasicSingle:
			if state.escape {
				state.escape = false
				i++
				continue
			}
			if b == '\\' {
				state.escape = true
				i++
				continue
			}
			if b == '"' {
				state.inBasicSingle = false
			}
			i++

		case state.inLiteralSingle:
			if b == '\'' {
				state.inLiteralSingle = false
			}
			i++

		default:
			// Not currently in a string. Triple-quote tokens checked
			// first so the single-quote branch doesn't grab them.
			switch {
			case isTripleQuote(segment, i, '"'):
				state.inBasicMulti = true
				i += 3
			case isTripleQuote(segment, i, '\''):
				state.inLiteralMulti = true
				i += 3
			case b == '"':
				state.inBasicSingle = true
				i++
			case b == '\'':
				state.inLiteralSingle = true
				i++
			case b == '[':
				state.depth++
				i++
			case b == ']':
				if state.depth > 0 {
					state.depth--
				}
				i++
			case b == '#':
				// Rest of line is a comment outside any string.
				return state
			default:
				i++
			}
		}
	}
	// Single-line strings cannot span lines per TOML; reset their
	// state at end-of-line so a malformed/unclosed `"foo` on one line
	// does not poison the next.
	state.inBasicSingle = false
	state.inLiteralSingle = false
	state.escape = false
	return state
}

// isTripleQuote reports whether segment[i..i+3] is the triple-quote
// token `quote` repeated three times.
func isTripleQuote(segment string, i int, quote byte) bool {
	if i+2 >= len(segment) {
		return false
	}
	return segment[i] == quote && segment[i+1] == quote && segment[i+2] == quote
}

// splitLinesPreserving splits source into lines without consuming a
// trailing empty token from a final newline. Each element is the line
// text without its terminating newline.
func splitLinesPreserving(source string) []string {
	if source == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(source, "\n")
	if trimmed == "" {
		// File was just a newline — preserve as a single empty line so
		// rewriters can re-add the trailing newline cleanly.
		return []string{""}
	}
	return strings.Split(trimmed, "\n")
}

// formatHit renders a hit for inclusion in CheckResult.Details. Each
// rendered line follows the "<path>:<line> <key>=" convention so the
// output is greppable and matches typical compiler output.
func formatHit(h deprecatedAttachmentHit) string {
	var b strings.Builder
	for i, ln := range h.Lines {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s:%d %s=", h.Path, ln.Line, ln.Key)
	}
	return b.String()
}
