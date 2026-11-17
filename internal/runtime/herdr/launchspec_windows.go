//go:build windows

package herdr

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// launchSyntaxPortable reports whether command reads as the same words under
// the POSIX splitting that finds the program and its arguments as under
// PowerShell, which runs a raw command on Windows. A kind launch hands herdr the
// POSIX words, so only then does it run what the command says. This is a
// conservative grammar, not a PowerShell parser: a command outside it launches
// raw, which runs it exactly as written.
//
// Each exclusion is a measured disagreement between the POSIX split and what
// pwsh 7.6 or Windows PowerShell 5.1 passed a program, and follows from where
// the PowerShell tokenizer starts a new word:
//   - a backslash, which the split reads as an escape and drops;
//   - `{` and `}`, a script block, and `~`, which pwsh expands to the home
//     directory;
//   - the typographic quotes, which PowerShell reads as quotes, and whitespace
//     other than a space or tab, which it reads as a separator;
//   - a word opening with `@` (splatting) or `#` (a comment), and `--%`
//     (stop-parsing) whether quoted or not, none of which PowerShell passes on;
//   - a word opening with `]` and holding more: PowerShell passes the `]` as a
//     word of its own and reads the rest afresh, so `]a` is `]` and `a`;
//   - a comma in an unquoted word, which PowerShell reads as building an
//     array: a leading, trailing or doubled comma is a parse error, and
//     Windows PowerShell passes a number in the list as its value (`a,0x10` as
//     `a,16`) and splits the words around a comma beside a space;
//   - a `.`, `[` or `:` in a parameter (see isParameter). PowerShell ends the
//     parameter at a `.` or `[` and reads the rest as a new word, so `-a.b` is
//     `-a` and `.b`; after a `:` it reads the rest afresh as the parameter's
//     argument, so `-a:` is a parse error and `-a:]x` is `-a:]` and `x`;
//   - a quote anywhere but around a whole, non-empty word, and a double quote
//     inside one: PowerShell splits `"a"b` in two, reads a doubled quote as
//     one, and Windows PowerShell drops an empty argument and strips the
//     double quotes inside one;
//   - a quoted program, which PowerShell reads as a string rather than a
//     command.
func launchSyntaxPortable(command string) bool {
	if strings.ContainsAny(command, "\\{}~\u2018\u2019\u201a\u201b\u201c\u201d\u201e") {
		return false
	}
	rest := strings.TrimLeft(command, " \t")
	if strings.HasPrefix(rest, `"`) || strings.HasPrefix(rest, "'") {
		return false
	}
	for {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			return true
		}
		if quote := rest[0]; quote == '"' || quote == '\'' {
			end := strings.IndexByte(rest[1:], quote)
			if end <= 0 {
				return false
			}
			content := rest[1 : 1+end]
			rest = rest[end+2:]
			if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
				return false
			}
			if strings.Contains(content, `"`) || content == stopParsing || !portableRunes(content) {
				return false
			}
			continue
		}
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		word := rest[:end]
		rest = rest[end:]
		if !portableBareWord(word) {
			return false
		}
	}
}

// stopParsing is PowerShell's stop-parsing token, which passes the rest of the
// line to the program verbatim.
const stopParsing = "--%"

// portableBareWord reports whether an unquoted word reads the same both ways.
func portableBareWord(word string) bool {
	if strings.ContainsAny(word, `"',`) || word == stopParsing || word[0] == '@' || word[0] == '#' {
		return false
	}
	if word[0] == ']' && len(word) > 1 {
		return false
	}
	if isParameter(word) && strings.ContainsAny(word, ".[:") {
		return false
	}
	return portableRunes(word)
}

// isParameter reports whether PowerShell may read word as a parameter: a dash
// followed by anything but another dash, a digit or a `.`. PowerShell reads a
// dash followed by a letter, `_` or `?` as a parameter; with a digit or a `.`
// after it, the word is a number or a plain word, and with a second dash an
// operator or a plain word.
func isParameter(word string) bool {
	first, size := utf8.DecodeRuneInString(word)
	if !isDash(first) || size == len(word) {
		return false
	}
	second, _ := utf8.DecodeRuneInString(word[size:])
	return !isDash(second) && second != '.' && (second < '0' || second > '9')
}

// isDash reports whether PowerShell reads r as a dash: the hyphen-minus, and
// the en dash, em dash and horizontal bar.
func isDash(r rune) bool {
	return r == '-' || r == '\u2013' || r == '\u2014' || r == '\u2015'
}

// portableRunes reports whether s holds no control character and no
// whitespace but spaces and tabs.
func portableRunes(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) && r != '\t' {
			return false
		}
		if unicode.IsSpace(r) && r != ' ' && r != '\t' {
			return false
		}
	}
	return true
}

// agentKindOf names the agent kind program would be, or "" when program names
// a particular file rather than a command.
//
// A kind launch runs the kind's canonical command as the pane resolves it:
// herdr types `Get-Command <kind>`, then `& <kind>` or `Start-Process -FilePath
// <kind>` (measured). A bare name resolves the same way whichever launch runs
// it, so only a bare name is a kind. Anything more specific can name another
// program: `maki.cmd` beside a `maki.exe` ran the .exe through the kind launch
// (measured), and a path picks one file among those the name could resolve to.
// Windows command names ignore case, so CLAUDE is the claude kind.
func agentKindOf(program string) string {
	if strings.ContainsAny(program, `/\:.`) {
		return ""
	}
	return strings.ToLower(program)
}
