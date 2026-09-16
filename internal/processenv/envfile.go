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
// A line missing '=' or with an empty key is malformed. Malformed lines are
// skipped and reported individually rather than aborting the whole parse: the
// returned map holds every successfully-parsed entry, and the returned error
// slice holds one error per malformed line (nil/empty when the parse is
// clean), so a single bad line in a secrets file no longer silently drops
// every other credential in it. The last assignment wins when a key repeats.
func ParseEnvFile(content string) (map[string]string, []error) {
	out := make(map[string]string)
	var errs []error
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			errs = append(errs, fmt.Errorf("line %d: missing '=' in %q", i+1, raw))
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			errs = append(errs, fmt.Errorf("line %d: empty key in %q", i+1, raw))
			continue
		}
		out[key] = unquoteEnvValue(strings.TrimSpace(val))
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
