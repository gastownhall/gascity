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

// ScanUnknownFlags scans raw template source text for bd and "gc bd"
// invocations of subcommands known to this package, and reports any flag
// token that is not a recognized value or boolean flag for that
// subcommand's manifest (see Known/ValueFlags/BoolFlags). Invocations of
// subcommands outside this package's manifest (e.g. "bd formula show") are
// silently skipped — there is no ground truth to validate them against.
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
		findings = append(findings, scanLineForUnknownFlags(tokenize(rawLine), idx+1)...)
	}
	return findings
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
		subStart := bdSubcommandStart(tokens, i)
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

		j := subStart + consumed
		for j < len(tokens) {
			tok := tokens[j]
			if tok == "--" {
				j++
				break
			}
			if bdSubcommandStart(tokens, j) >= 0 {
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
// "bd"). It returns -1 if tokens[i] does not open one.
func bdSubcommandStart(tokens []string, i int) int {
	switch {
	case tokens[i] == "bd":
		return i + 1
	case tokens[i] == "gc" && i+1 < len(tokens) && tokens[i+1] == "bd":
		return i + 2
	default:
		return -1
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
