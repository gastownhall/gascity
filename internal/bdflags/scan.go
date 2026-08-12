package bdflags

import "strings"

// Finding describes an unrecognized flag found in a bd or "gc bd" invocation
// inside raw template source text.
type Finding struct {
	Line       int    // 1-indexed line number in the source
	Subcommand string // matched bd subcommand key, e.g. "mol pour"
	Flag       string // the unrecognized flag token, e.g. "--asignee"
}

// flagTokenCutset is trimmed from both ends of every whitespace-split token
// before classification, so flags embedded in markdown inline-code spans,
// sentence punctuation, or quoted example strings compare correctly (e.g.
// "`--claim`," becomes "--claim").
const flagTokenCutset = "`*_(),:;.\"'"

// gcScopeValueFlags are owned by "gc bd" rather than by bd itself.
// cmd/gc/cmd_bd.go extractBdScopeFlags strips them from the raw argument
// list and resolves the target store before forwarding what remains to bd,
// so they are valid on any "gc bd <sub>" invocation and invalid on a bare
// "bd <sub>" one, which rejects them with "unknown flag".
var gcScopeValueFlags = map[string]bool{
	"--city": true, "--rig": true,
}

// ScanUnknownFlags scans raw template source text for bd and "gc bd"
// invocations of subcommands known to this package, and reports any flag
// token that is not a recognized value or boolean flag for that
// subcommand's manifest (see Known/ValueFlags/BoolFlags). Invocations of
// subcommands outside this package's manifest (e.g. "bd formula show") are
// silently skipped — there is no ground truth to validate them against.
//
// Each line is first split into shell segments on unquoted separators, so a
// flag belonging to a downstream command ("bd show <id> --json | jq -r .x")
// or to a neighboring markdown table cell is never attributed to bd.
//
// Scanning is line-oriented and does not join backslash-continued shell
// lines; every bd invocation seen in prompt templates today is a single
// physical line, so this is an accepted scope boundary rather than a gap.
// Flag names that only exist behind a template variable (e.g.
// "--{{.FlagName}}") are likewise invisible to this raw-text scan.
func ScanUnknownFlags(source []byte) []Finding {
	var findings []Finding
	lines := strings.Split(string(source), "\n")
	for idx, rawLine := range lines {
		for _, segment := range splitShellSegments(rawLine) {
			findings = append(findings, scanLineForUnknownFlags(tokenize(segment), idx+1)...)
		}
	}
	return findings
}

// shellSeparators are the unquoted characters that end one command and begin
// another, so flag scanning must not carry across them. Markdown table cell
// pipes land here too, which is the same boundary for the same reason.
//
// Redirection ("<", ">") is deliberately excluded: prompt templates write
// argument placeholders as "<id>" far more often than they redirect, and
// treating those as separators would split an invocation in half and hide
// the typo after it. What follows a redirection is a filename rather than a
// flag, so the missed boundary costs nothing.
const shellSeparators = "|;&"

// splitShellSegments splits a line on unquoted shell separators. Quoted
// spans are preserved intact, so a separator inside an argument value (a jq
// program, a --title string) does not split the invocation that owns it.
func splitShellSegments(line string) []string {
	var segments []string
	var current strings.Builder
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			current.WriteRune(r)
		case strings.ContainsRune(shellSeparators, r):
			segments = append(segments, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	return append(segments, current.String())
}

// tokenize splits a line on whitespace and trims flagTokenCutset from each
// resulting token, dropping any token that becomes empty.
func tokenize(line string) []string {
	fields := strings.Fields(line)
	tokens := make([]string, 0, len(fields))
	for _, f := range fields {
		if t := strings.Trim(f, flagTokenCutset); t != "" {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

// scanLineForUnknownFlags walks tokens looking for bd/"gc bd" invocations of
// known subcommands and reports unrecognized flags within each one.
func scanLineForUnknownFlags(tokens []string, lineNo int) []Finding {
	var findings []Finding
	i := 0
	for i < len(tokens) {
		subStart, viaGC := bdSubcommandStart(tokens, i)
		if subStart < 0 {
			i++
			continue
		}
		key, consumed, ok := matchSubcommand(tokens, subStart)
		if !ok {
			// Not a subcommand this package has a manifest for (e.g.
			// "formula show"); resume scanning right after "bd"/"gc bd" so
			// we don't loop on the same trigger forever.
			i = subStart
			continue
		}
		valueFlags := ValueFlags(key)
		boolFlags := BoolFlags(key)
		if viaGC {
			for flag := range gcScopeValueFlags {
				valueFlags[flag] = true
			}
		}

		j := subStart + consumed
		for j < len(tokens) {
			tok := tokens[j]
			if tok == "--" {
				j++
				break
			}
			if next, _ := bdSubcommandStart(tokens, j); next >= 0 {
				break // a new bd invocation begins; let the outer loop handle it
			}
			if !strings.HasPrefix(tok, "-") || tok == "-" {
				j++
				continue
			}
			name := tok
			hasInlineValue := false
			if eq := strings.IndexByte(tok, '='); eq >= 0 {
				name = tok[:eq]
				hasInlineValue = true
			}
			switch {
			case boolFlags[name]:
				j++
			case valueFlags[name]:
				if hasInlineValue {
					j++
				} else {
					j += 2
				}
			default:
				findings = append(findings, Finding{Line: lineNo, Subcommand: key, Flag: name})
				j++
			}
		}
		i = j
	}
	return findings
}

// bdSubcommandStart returns the token index where a bd subcommand begins if
// tokens[i] opens a bd invocation ("bd", or "gc" immediately followed by
// "bd"), along with whether it opened via the "gc bd" form, which accepts
// the gc-owned scope flags. It returns -1 if tokens[i] does not open one.
//
// "gc bd --rig <name> list" places the scope flags before the subcommand,
// which this scanner skips over as it searches for a known subcommand key;
// only the trailing form ("gc bd list --rig <name>") reaches flag scanning.
func bdSubcommandStart(tokens []string, i int) (start int, viaGC bool) {
	switch {
	case tokens[i] == "bd":
		return i + 1, false
	case tokens[i] == "gc" && i+1 < len(tokens) && tokens[i+1] == "bd":
		return i + 2, true
	default:
		return -1, false
	}
}

// matchSubcommand returns the longest known subcommand key starting at
// token index i, preferring the two-token compound form (e.g. "mol pour")
// over the single-token form (e.g. "update").
func matchSubcommand(tokens []string, i int) (key string, consumed int, ok bool) {
	if i >= len(tokens) {
		return "", 0, false
	}
	if i+1 < len(tokens) {
		if two := tokens[i] + " " + tokens[i+1]; Known(two) {
			return two, 2, true
		}
	}
	if Known(tokens[i]) {
		return tokens[i], 1, true
	}
	return "", 0, false
}
