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
// value is skipped together with its continuation lines, up to and including
// the next line containing the matching quote, and reported as one error, so
// a continuation line (for example "export CODEX_HOME=..." inside a quoted
// script) never becomes a top-level key of its own. If no later line closes
// the quote, only the opening line is skipped. A value whose opening quote is
// closed on the same line but followed by more text (KEY="v" # c) is kept
// literally, as before.
//
// A line missing '=' or with an empty key is malformed. Malformed lines are
// skipped and reported individually rather than aborting the whole parse: the
// returned map holds every successfully-parsed entry, and the returned error
// slice holds one error per malformed line or skipped block (nil/empty when
// the parse is clean), so a single bad line in a secrets file no longer
// silently drops every other credential in it. Errors name only line numbers
// and keys, never raw line content or values, because the input holds
// secrets. The last assignment wins when a key repeats.
func ParseEnvFile(content string) (map[string]string, []error) {
	out := make(map[string]string)
	var errs []error
	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			errs = append(errs, fmt.Errorf("line %d: missing '='", i+1))
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			errs = append(errs, fmt.Errorf("line %d: empty key", i+1))
			continue
		}
		val = strings.TrimSpace(val)
		if q, open := unclosedQuote(val); open {
			end := -1
			for j := i + 1; j < len(lines); j++ {
				if strings.IndexByte(lines[j], q) >= 0 {
					end = j
					break
				}
			}
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
