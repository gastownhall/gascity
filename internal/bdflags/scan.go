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
		// "gc bd" may place the gc-owned scope flags before the subcommand
		// ("gc bd --rig <name> create ..."); consume them so the subcommand
		// that follows is still validated. Bare "bd" does not accept scope
		// flags, so its leading forms are left for matchSubcommand to reject.
		verbStart := subStart
		if viaGC {
			verbStart = skipLeadingScopeFlags(tokens, subStart)
		}
		key, consumed, ok := matchSubcommand(tokens, verbStart)
		if !ok {
			// Not a subcommand this package has a manifest for (e.g.
			// "formula show"); resume scanning right after "bd"/"gc bd" so
			// we don't loop on the same trigger forever.
			i = subStart
			continue
		}
		flagFindings, resume := scanInvocationFlags(tokens, verbStart+consumed, key, viaGC, lineNo)
		findings = append(findings, flagFindings...)
		i = resume
	}
	return findings
}

// skipLeadingScopeFlags advances past any gc-owned scope flags that a "gc bd"
// invocation places before its subcommand ("--rig <name>", "--city <path>",
// or their inline "--rig=<name>" forms), returning the index of the first
// token that is not one. This lets "gc bd --rig <name> create ..." reach and
// validate the subcommand after the scope flags instead of skipping the whole
// invocation.
func skipLeadingScopeFlags(tokens []string, i int) int {
	for i < len(tokens) {
		name, hasInlineValue := splitFlagToken(tokens[i])
		if !gcScopeValueFlags[name] {
			return i
		}
		if hasInlineValue {
			i++ // "--rig=<name>" carries its value in the same token
		} else {
			i += 2 // "--rig <name>" also consumes the following value token
		}
	}
	return i
}

// scanInvocationFlags scans the flag tokens of a single bd invocation whose
// subcommand resolved to key and whose flags begin at flagStart. It reports
// any unrecognized flag and returns the token index at which the outer scan
// should resume. viaGC additionally allows the gc-owned scope flags in the
// trailing position ("gc bd list --rig <name>").
func scanInvocationFlags(tokens []string, flagStart int, key string, viaGC bool, lineNo int) (findings []Finding, resume int) {
	valueFlags := ValueFlagsWithDiscovery(key)
	boolFlags := BoolFlagsWithDiscovery(key)
	if viaGC {
		// Safe to mutate in place: ValueFlags returns a freshly allocated map
		// per call (mergeFlagSets), so this never leaks scope flags into
		// another invocation's or consumer's manifest.
		for flag := range gcScopeValueFlags {
			valueFlags[flag] = true
		}
	}
	j := flagStart
	for j < len(tokens) {
		tok := tokens[j]
		if tok == "--" {
			return findings, j + 1 // positional args follow; stop flag scanning
		}
		if next, _ := bdSubcommandStart(tokens, j); next >= 0 {
			return findings, j // a new bd invocation begins; the outer loop takes it
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			j++
			continue
		}
		name, advance, unknown := classifyFlag(tok, valueFlags, boolFlags)
		if unknown {
			findings = append(findings, Finding{Line: lineNo, Subcommand: key, Flag: name})
		}
		j += advance
	}
	return findings, j
}

// splitFlagToken splits a flag token into its name and whether it carried an
// inline "=value": "--rig=x" -> ("--rig", true); "--rig" -> ("--rig", false).
func splitFlagToken(tok string) (name string, hasInlineValue bool) {
	if eq := strings.IndexByte(tok, '='); eq >= 0 {
		return tok[:eq], true
	}
	return tok, false
}

// classifyFlag resolves a "-"-prefixed flag token against a subcommand's value
// and boolean flag sets. It returns the flag name, how many tokens the flag
// consumes (2 when a value flag takes the following token, otherwise 1), and
// whether the flag is unrecognized.
func classifyFlag(tok string, valueFlags, boolFlags map[string]bool) (name string, advance int, unknown bool) {
	name, hasInlineValue := splitFlagToken(tok)
	switch {
	case boolFlags[name]:
		return name, 1, false
	case valueFlags[name]:
		if hasInlineValue {
			return name, 1, false
		}
		return name, 2, false
	default:
		return name, 1, true
	}
}

// bdSubcommandStart returns the token index immediately after "bd"/"gc bd"
// when tokens[i] opens a bd invocation ("bd", or "gc" immediately followed by
// "bd"), along with whether it opened via the "gc bd" form, which accepts the
// gc-owned scope flags. It returns -1 if tokens[i] does not open one.
//
// The returned index is where the subcommand search begins. For the "gc bd"
// form the caller first consumes any leading scope flags
// (skipLeadingScopeFlags), so both "gc bd --rig <name> list" and the trailing
// "gc bd list --rig <name>" flow through subcommand and flag validation.
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

// matchSubcommand returns the longest known subcommand key starting at token
// index i. Keys may be nested to any depth (for example "mol wisp gc"), so
// matching is derived from the manifest rather than capped at two tokens.
func matchSubcommand(tokens []string, i int) (key string, consumed int, ok bool) {
	if i >= len(tokens) {
		return "", 0, false
	}
	for _, candidate := range Subcommands() {
		parts := strings.Fields(candidate)
		if len(parts) <= consumed || i+len(parts) > len(tokens) {
			continue
		}
		matches := true
		for offset, part := range parts {
			if tokens[i+offset] != part {
				matches = false
				break
			}
		}
		if matches {
			key, consumed, ok = candidate, len(parts), true
		}
	}
	return key, consumed, ok
}
