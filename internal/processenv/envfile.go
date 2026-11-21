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
// A value that opens with a quote which is not closed anywhere on the same line
// is not a single-line value; multi-line values are not supported. Such a
// value is skipped together with its continuation lines, through the closing
// line, and reported as one error, so a continuation line (for example
// "export CODEX_HOME=..." inside a quoted script) never becomes a top-level
// key of its own. The closing line is the first later line whose trimmed text
// ends with the matching quote and holds an odd number of that quote. The
// odd count keeps a JSON field line such as `"k": "v"` from closing the block
// and, because an identifier key holds no quotes, means a complete balanced
// one-line assignment (KEY="v") never closes it either, so a typo'd unclosed
// quote does not swallow the valid assignments after it. If no later line
// qualifies, only the opening line is skipped. A value whose opening quote is closed on the same
// line but followed by more text (KEY="v" # c) is kept literally, as before.
//
// A line missing '=', or whose key is empty or not a shell identifier
// ([A-Za-z_][A-Za-z0-9_]*), is malformed. Malformed lines are skipped and
// reported individually rather than aborting the whole parse: the returned
// map holds every successfully-parsed entry, and the returned error slice
// holds one error per malformed line or skipped block (nil/empty when the
// parse is clean), so a single bad line in a secrets file no longer silently
// drops every other credential in it. Errors name only line numbers and
// valid identifier keys, never raw line content, values, or the text of an
// invalid key, because the input holds secrets. The last assignment wins when
// a key repeats.
func ParseEnvFile(content string) (map[string]string, []error) {
	out := make(map[string]string)
	var errs []error
	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, err := parseEnvAssignment(line)
		if err != "" {
			errs = append(errs, fmt.Errorf("line %d: %s", i+1, err))
			continue
		}
		if q, open := unclosedQuote(val); open {
			end := multiLineQuoteEnd(lines, i+1, q)
			if end < 0 {
				errs = append(errs, fmt.Errorf("line %d: unterminated quote in value for %q; skipped", i+1, key))
				continue
			}
			errs = append(errs, fmt.Errorf("lines %d-%d: multi-line quoted value for %q is not supported; skipped", i+1, end+1, key))
			i = end
			continue
		}
		out[key] = unquoteEnvValue(val)
	}
	return out, errs
}

// parseEnvAssignment splits a trimmed, non-comment line into an identifier key
// and a trimmed raw value. On failure it returns a short reason that contains
// no text from the line.
func parseEnvAssignment(line string) (key, val, reason string) {
	line = strings.TrimPrefix(line, "export ")
	key, val, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", "missing '='"
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", "empty key"
	}
	if !isEnvIdentifier(key) {
		return "", "", "invalid key"
	}
	return key, strings.TrimSpace(val), ""
}

// isEnvIdentifier reports whether key matches [A-Za-z_][A-Za-z0-9_]*.
func isEnvIdentifier(key string) bool {
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return key != ""
}

// multiLineQuoteEnd returns the index of the line that closes a multi-line
// value opened with quote q, searching from index from, or -1 if none does.
// See ParseEnvFile for the closing-line rule.
func multiLineQuoteEnd(lines []string, from int, q byte) int {
	for j := from; j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if t != "" && t[len(t)-1] == q && strings.Count(t, string(q))%2 == 1 {
			return j
		}
	}
	return -1
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

// unclosedQuote reports whether val opens with a single or double quote that
// does not appear again anywhere in val, i.e. the quoted value continues past
// the end of the line. It returns the opening quote byte.
func unclosedQuote(val string) (byte, bool) {
	if val == "" || (val[0] != '"' && val[0] != '\'') {
		return 0, false
	}
	return val[0], strings.IndexByte(val[1:], val[0]) < 0
}
