package processenv

import (
	"fmt"
	"strings"
)

// ParseEnvFile parses dotenv-style content into a key/value map. The format is
// the common EnvironmentFile / docker --env-file subset:
//
//   - one KEY=VALUE assignment per line;
//   - blank lines and lines whose first non-space character is '#' are ignored;
//   - an optional leading "export " prefix on the key is stripped;
//   - surrounding whitespace around the key and value is trimmed;
//   - a value fully wrapped in matching single or double quotes is unquoted,
//     preserving '=' and '#' characters inside the quotes.
//
// Values are treated literally otherwise: there is no variable interpolation
// and no inline-comment stripping on unquoted values (a '#' mid-value is kept).
// A line missing '=', a line with an empty key, or a value that opens a quote
// without closing it on the same line (multi-line quoted values are not
// supported) is a parse error, so a malformed secrets file fails loudly
// rather than silently dropping a credential or splitting a quoted value
// across invented top-level keys. The last assignment wins when a key
// repeats.
func ParseEnvFile(content string) (map[string]string, error) {
	out := make(map[string]string)
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: missing '=' in %q", i+1, raw)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key in %q", i+1, raw)
		}
		val = strings.TrimSpace(val)
		if hasUnterminatedQuote(val) {
			return nil, fmt.Errorf("line %d: unterminated quote in value for %q; multi-line values are not supported", i+1, key)
		}
		out[key] = unquoteEnvValue(val)
	}
	return out, nil
}

// hasUnterminatedQuote reports whether a trimmed value opens with a single or
// double quote but does not close with that same quote character on this
// line. That pattern is the signature of a value that was meant to span
// multiple lines (e.g. a multi-line quoted shell script), which this
// line-oriented parser cannot represent correctly: it would otherwise
// silently truncate the value and may misparse the continuation lines as
// unrelated top-level assignments. A single-character value that is just a
// lone quote (e.g. val == `"`) is not treated as unterminated, since its
// first and last byte are the same quote character.
func hasUnterminatedQuote(val string) bool {
	if len(val) < 2 {
		return false
	}
	first, last := val[0], val[len(val)-1]
	return (first == '"' || first == '\'') && last != first
}

// unquoteEnvValue strips one layer of matching surrounding single or double
// quotes from a trimmed value; otherwise it returns the value unchanged.
func unquoteEnvValue(val string) string {
	if len(val) < 2 {
		return val
	}
	first, last := val[0], val[len(val)-1]
	if (first == '"' || first == '\'') && last == first {
		return val[1 : len(val)-1]
	}
	return val
}
