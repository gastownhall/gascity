//go:build windows

package config

import "strings"

// openOptionValueFits reports whether value may be substituted into an open
// option's FlagTemplate. The rendered args are joined into the launch command
// with POSIX quoting (shellquote.Join), the command is re-tokenized by POSIX
// rules, and on Windows PowerShell runs the result. PowerShell ends a string at
// typographic quotes that POSIX quoting leaves alone and reads POSIX's escaped
// quote as code; a bare comma makes an array, which a native program receives
// as separate arguments; a leading @ or - and whitespace mean something else
// to it; and Windows PowerShell 5.1 splits a native argument at an embedded
// double quote.
//
// A value is honored only when it is one literal word under every one of those
// readings: it starts with an ASCII letter or digit and holds only those and
// . _ : / @ + = [ ] - , which covers model ids and effort tiers. A comma is
// allowed only beside a [, as in claude-opus-4-8[context=1m,effort=high]: the
// bracket makes shellquote.Join single-quote the whole value, and PowerShell
// reads a comma inside a string literally.
func openOptionValueFits(value string) bool {
	for i, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && strings.ContainsRune("._:/@+=[]-,", r):
		default:
			return false
		}
	}
	if strings.ContainsRune(value, ',') && !strings.ContainsRune(value, '[') {
		return false
	}
	return value != ""
}

// openOptionValueRule states the rule openOptionValueFits enforces, for the
// message that refuses a value under it.
const openOptionValueRule = "on Windows an open option value must be a plain word: an ASCII letter or digit, then only letters, digits and . _ : / @ + = [ ] - (a comma only beside a [), because PowerShell runs the launch command and would read anything else as more than one literal word"
