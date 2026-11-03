//go:build windows

package runtime

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// posixShells are the programs ShellCommandUnavailable refuses by name, with
// or without .exe.
var posixShells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "ash": true}

// ShellCommandUnavailable reports why Windows refuses to run command through
// [ShellCommandContext], or "" when it does not.
//
// Windows runs a command string through PowerShell, and a command written for
// another interpreter fails there loudly, which is its author's to fix. Two
// programs are refused before anything starts, because PowerShell would not
// fail on them: a POSIX shell script (a .sh file), which PowerShell hands to
// the file's Windows association, and a POSIX shell, which resolves to
// whatever PATH offers, Git's sh.exe or the WSL launcher in System32. Gas City
// runs no POSIX shell on Windows.
//
// The gate is static. It scans PowerShell command positions, including later
// statements, pipeline elements, script blocks and subexpressions, and refuses
// literal program names there. It does not try to resolve dynamic forms such
// as Start-Process arguments, Invoke-Expression text, functions, aliases or a
// program name supplied solely by a variable; a command-position token whose
// literal text names a path, such as $env:PACK_DIR\x.sh or
// "$env:GIT\bin\bash.exe", is still refused without resolving its variables.
//
// Reading literal text makes it conservative both ways. It refuses what would
// never run, such as a POSIX shell named in a branch never taken or a function
// the script itself names sh, and it does not see a POSIX shell another
// program starts, as cmd /c bash, wsl -e bash or pwsh -c bash do.
func ShellCommandUnavailable(command string) string {
	for _, program := range scanPowerShellCommand(command).programs {
		base := strings.ToLower(filepath.Base(program))
		switch {
		case strings.HasSuffix(base, ".sh"):
			return fmt.Sprintf("its program %s is a POSIX shell script, and Gas City runs no POSIX shell on Windows", program)
		case posixShells[strings.TrimSuffix(base, ".exe")]:
			return fmt.Sprintf("its program %s is a POSIX shell, and Gas City runs no POSIX shell on Windows", program)
		}
	}
	return ""
}

// ShellCommandPrograms returns literal program names in PowerShell command
// positions, or nil if any command position cannot be resolved statically.
// Programs launched inside scripts still require configured process names and
// interpreter hosts.
func ShellCommandPrograms(command string) []string {
	scan := scanPowerShellCommand(command)
	if scan.unresolved {
		return nil
	}
	return scan.programs
}

// commandPrograms returns the literal program names in command position.
func commandPrograms(command string) []string {
	return scanPowerShellCommand(command).programs
}

// psScan is one left-to-right pass over a command. It does not parse
// PowerShell: every statement separator, pipe, (, $(, @(, script block and
// assignment opens a command position, the first word there is a program
// unless it is a keyword, and a stack of open brackets says where each one
// closes. Double-quoted strings are frames too, so their $( ) subexpressions
// are scanned and the string resumes after them.
type psScan struct {
	s          string
	i          int
	programs   []string
	unresolved bool
	frames     []byte // '(' paren, 'P' param block, '{' block, 's' switch body, 't' hashtable, '"' string, 'h' here-string
	switchAt   int    // frame depth whose next { is a switch body, or -1
	paramAt    int    // frame depth whose next ( is a param block, or -1
}

func scanPowerShellCommand(command string) *psScan {
	p := &psScan{s: normalizePowerShellQuotes(command), switchAt: -1, paramAt: -1}
	atCmd, call := true, false
	for p.i < len(p.s) {
		if top := p.top(); top == '"' || top == 'h' {
			if p.scanStringBody(top) {
				atCmd, call = true, false
			}
			continue
		}
		c := p.s[p.i]
		wasCall := call
		call = false
		switch {
		case c == ' ' || c == '\t' || c == '\v' || c == '\f':
			p.i++
			call = wasCall
		case c == '\r' || c == '\n' || c == ';':
			p.i++
			atCmd = p.top() != 't'
		case c == '=' && p.top() == 't':
			p.i++
			atCmd = true // a hashtable value is a statement
		case c == '|':
			p.i++
			if p.peek(0) == '|' {
				p.i++
			}
			atCmd = true
		case c == '&':
			p.i++
			switch {
			case p.peek(0) == '&':
				p.i++
				atCmd = true
			case atCmd:
				call = true
			case p.i < 2 || p.s[p.i-2] != '>':
				p.unresolved, atCmd = true, true // PowerShell 7 background operator
			}
		case c == '#':
			for p.i < len(p.s) && p.s[p.i] != '\n' && p.s[p.i] != '\r' {
				p.i++ // the newline still ends the statement
			}
		case c == '<' && p.peek(1) == '#':
			if strings.ContainsAny(p.skipTo("#>"), "\r\n") {
				atCmd = true
			}
		case c == '`' && (p.peek(1) == '\n' || p.peek(1) == '\r'):
			p.i += 2 // line continuation
			call = wasCall
		case c == ')' || c == '}':
			p.i++
			closed := p.top()
			if len(p.frames) > 0 {
				p.frames = p.frames[:len(p.frames)-1]
			}
			atCmd = closed == 'P' || (closed == '{' || closed == 's') && p.top() != 's' && p.top() != 't'
		case c == '(':
			if wasCall && p.constantCallTarget() {
				atCmd = false
				continue
			}
			p.unresolved = p.unresolved || wasCall
			p.i++
			if p.paramAt == len(p.frames) {
				p.paramAt = -1
				p.push('P')
			} else {
				p.push('(')
			}
			atCmd = true
		case c == '{':
			p.i++
			switch {
			case p.switchAt == len(p.frames):
				p.switchAt = -1
				p.push('s')
				atCmd = false
			case atCmd && !wasCall && p.top() != 's' && p.top() != 't':
				p.unresolved = true // a script block that is never invoked
				p.i--
				p.skipBalanced('{', '}')
				atCmd = false
			default:
				p.push('{')
				atCmd = true
			}
		case (c == '$' || c == '@') && p.peek(1) == '(':
			p.unresolved = p.unresolved || atCmd
			p.i += 2
			p.push('(')
			atCmd = true
		case c == '@' && p.peek(1) == '{':
			p.unresolved = true // a hashtable may hold a script block run later
			p.i += 2
			p.push('t')
			atCmd = false
		case c == '@' && (p.peek(1) == '\'' || p.peek(1) == '"') && (p.peek(2) == '\n' || p.peek(2) == '\r'):
			p.unresolved = p.unresolved || atCmd
			if p.peek(1) == '\'' {
				p.i += 2
				p.skipHereStringEnd('\'')
			} else {
				p.i += 2
				p.push('h')
			}
			atCmd = false
		case c == '[':
			p.unresolved = p.unresolved || atCmd
			p.skipBalanced('[', ']')
			p.skipSpace()
			atCmd = atCmd && p.peek(0) == '$' // [type]$x = ... is an assignment
		case c == '$':
			word := p.readVariable()
			if atCmd || wasCall {
				p.dynamicProgram(word)
				if atCmd && !wasCall && p.assignment() {
					continue
				}
			}
			atCmd = false
		case c == '\'' || c == '"':
			value, static, sub := p.peekString()
			if sub {
				p.unresolved = p.unresolved || atCmd || wasCall
				p.i++
				p.push('"')
			} else if wasCall && static {
				p.addProgram(value)
			} else if wasCall {
				p.dynamicProgram(value)
			} else {
				p.unresolved = p.unresolved || atCmd
			}
			atCmd = false
		case c == '.' && atCmd && strings.IndexByte(" \t\v\f\r\n{('\"", p.peek(1)) >= 0 && p.peek(1) != 0:
			p.i++
			call = true
		default:
			word := p.readBareWord()
			if word == "" {
				p.i++
				atCmd = false
				continue
			}
			if !atCmd && !wasCall || p.top() == 's' {
				atCmd = false
				continue
			}
			if !wasCall {
				if next, keyword := p.keyword(word); keyword {
					atCmd = next
					continue
				}
			}
			p.addProgram(word)
			atCmd = false
		}
	}
	return p
}

// keyword reports whether word is a PowerShell keyword in command position
// and, if so, whether a command position follows it directly.
func (p *psScan) keyword(word string) (atCmd bool, ok bool) {
	switch strings.ToLower(word) {
	case "return", "throw":
		return true, true
	case "function", "filter", "workflow", "configuration":
		p.unresolved = true // the script may shadow any program name
		p.skipSpace()
		p.readBareWord()
		return false, true
	case "begin", "process", "end", "dynamicparam", "clean":
		p.skipSpace()
		return false, p.peek(0) == '{'
	case "switch":
		p.switchAt = len(p.frames)
		return false, true
	case "param":
		p.paramAt = len(p.frames)
		return false, true
	case "if", "elseif", "else", "while", "for", "foreach", "do", "until", "try", "catch", "finally",
		"trap", "data", "using", "class", "enum", "break", "continue", "exit":
		return false, true
	}
	return false, false
}

func (p *psScan) addProgram(program string) {
	switch strings.ToLower(filepath.Base(program)) {
	case "invoke-expression", "iex", "start-process", "start", "saps", "invoke-item", "ii",
		"invoke-command", "icm", "set-alias", "new-alias":
		p.unresolved = true
	}
	if r := []rune(program); len(r) == 0 || unicode.IsDigit(r[0]) {
		p.unresolved = true
		return
	}
	p.programs = append(p.programs, program)
}

// dynamicProgram records a program whose name depends on a variable. Its
// literal text is kept when it names a path, so $env:DIR\x.sh is refused.
func (p *psScan) dynamicProgram(text string) {
	p.unresolved = true
	if strings.ContainsAny(text, `/\`) {
		p.programs = append(p.programs, text)
	}
}

// constantCallTarget consumes ('literal') after a call operator and records
// the literal as the program.
func (p *psScan) constantCallTarget() bool {
	start := p.i
	p.i++
	p.skipSpace()
	if q := p.peek(0); q == '\'' || q == '"' {
		if value, static, sub := p.peekString(); static && !sub {
			p.skipSpace()
			if p.peek(0) == ')' {
				p.i++
				p.addProgram(value)
				return true
			}
		}
	}
	p.i = start
	return false
}

// scanStringBody advances through the open string frame to its end or to a
// $( subexpression, which it opens. It reports whether one was opened.
func (p *psScan) scanStringBody(kind byte) bool {
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '`':
			p.i += 2
		case c == '$' && p.peek(1) == '(':
			p.i += 2
			p.push('(')
			return true
		case kind == '"' && c == '"' && p.peek(1) == '"':
			p.i += 2
		case kind == '"' && c == '"':
			p.i++
			p.frames = p.frames[:len(p.frames)-1]
			return false
		case kind == 'h' && (c == '\n' || c == '\r') && p.hereStringEndsAt(p.i, '"'):
			p.skipHereStringEnd('"')
			p.frames = p.frames[:len(p.frames)-1]
			return false
		default:
			p.i++
		}
	}
	return false
}

// peekString reads the quoted string at the cursor and returns its value,
// whether it has no variables, and whether it holds a $( subexpression. It
// consumes the string unless it holds a subexpression.
func (p *psScan) peekString() (value string, static bool, sub bool) {
	quote := p.s[p.i]
	var b strings.Builder
	static = true
	for j := p.i + 1; j < len(p.s); j++ {
		c := p.s[j]
		switch {
		case c == quote && j+1 < len(p.s) && p.s[j+1] == quote:
			b.WriteByte(c)
			j++
		case c == quote:
			p.i = j + 1
			return b.String(), static, false
		case quote == '\'':
			b.WriteByte(c)
		case c == '`' && j+1 < len(p.s):
			j++
			b.WriteString(backtickEscape(p.s[j]))
		case c == '$' && j+1 < len(p.s) && p.s[j+1] == '(':
			return "", false, true
		case c == '$' && j+1 < len(p.s) && (isVariableChar(p.s[j+1]) || strings.IndexByte("{$?^", p.s[j+1]) >= 0):
			static = false
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	p.i = len(p.s)
	return b.String(), static, false
}

// readBareWord reads an unquoted command word, resolving backtick escapes and
// joining embedded static quoted parts as PowerShell does.
func (p *psScan) readBareWord() string {
	var b strings.Builder
	for p.i < len(p.s) && strings.IndexByte(" \t\v\f\r\n;|&(){}>", p.s[p.i]) < 0 {
		switch c := p.s[p.i]; {
		case c == '`' && p.i+1 < len(p.s):
			b.WriteString(backtickEscape(p.s[p.i+1]))
			p.i += 2
		case c == '\'' || c == '"':
			value, static, sub := p.peekString()
			if !static || sub {
				return b.String()
			}
			b.WriteString(value)
		default:
			b.WriteByte(c)
			p.i++
		}
	}
	return b.String()
}

// readVariable reads $name, ${name} or $scope:name, plus any path text that
// follows it directly, such as $env:PACK_DIR\x.sh.
func (p *psScan) readVariable() string {
	start := p.i
	p.i++
	if p.peek(0) == '{' {
		p.skipTo("}")
	} else {
		for p.i < len(p.s) && (isVariableChar(p.s[p.i]) || p.s[p.i] == ':' || p.s[p.i] == '?') {
			p.i++
		}
	}
	if c := p.peek(0); c == '\\' || c == '/' {
		for p.i < len(p.s) && strings.IndexByte(" \t\v\f\r\n;|&(){}>", p.s[p.i]) < 0 {
			p.i++
		}
	}
	return p.s[start:p.i]
}

// assignment consumes an assignment operator after a variable.
func (p *psScan) assignment() bool {
	start := p.i
	p.skipSpace()
	if p.peek(0) == '=' && p.peek(1) != '=' {
		p.i++
		return true
	}
	if strings.IndexByte("+-*/%", p.peek(0)) >= 0 && p.peek(0) != 0 && p.peek(1) == '=' {
		p.i += 2
		return true
	}
	p.i = start
	return false
}

func (p *psScan) skipBalanced(open, close byte) {
	depth := 0
	for p.i < len(p.s) {
		switch c := p.s[p.i]; c {
		case '\'', '"':
			if _, _, sub := p.peekString(); sub {
				p.i++
			}
		default:
			p.i++
			if c == open {
				depth++
			} else if c == close {
				if depth--; depth == 0 {
					return
				}
			}
		}
	}
}

func (p *psScan) hereStringEndsAt(j int, quote byte) bool {
	if p.s[j] == '\r' && j+1 < len(p.s) && p.s[j+1] == '\n' {
		j++
	}
	return j+2 < len(p.s) && p.s[j+1] == quote && p.s[j+2] == '@'
}

// skipHereStringEnd moves past the newline-quote-@ that closes a here-string.
func (p *psScan) skipHereStringEnd(quote byte) {
	for ; p.i < len(p.s); p.i++ {
		if (p.s[p.i] == '\n' || p.s[p.i] == '\r') && p.hereStringEndsAt(p.i, quote) {
			if p.s[p.i] == '\r' && p.peek(1) == '\n' {
				p.i++
			}
			p.i += 3
			return
		}
	}
}

// skipTo moves past the next end marker, or to the end, and returns what it skipped.
func (p *psScan) skipTo(end string) string {
	start := p.i
	if k := strings.Index(p.s[p.i:], end); k >= 0 {
		p.i += k + len(end)
	} else {
		p.i = len(p.s)
	}
	return p.s[start:p.i]
}

func (p *psScan) skipSpace() {
	for p.i < len(p.s) && strings.IndexByte(" \t\v\f", p.s[p.i]) >= 0 {
		p.i++
	}
}

func (p *psScan) peek(offset int) byte {
	if p.i+offset < len(p.s) {
		return p.s[p.i+offset]
	}
	return 0
}

func (p *psScan) push(frame byte) { p.frames = append(p.frames, frame) }

func (p *psScan) top() byte {
	if len(p.frames) == 0 {
		return 0
	}
	return p.frames[len(p.frames)-1]
}

func isVariableChar(c byte) bool {
	return c == '_' || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))
}

func backtickEscape(c byte) string {
	if k := strings.IndexByte("0abfnrtv", c); k >= 0 {
		return string("\x00\a\b\f\n\r\t\v"[k])
	}
	if c == '\r' || c == '\n' {
		return ""
	}
	return string(c)
}

// normalizePowerShellQuotes maps the typographic quotes PowerShell accepts to
// their ASCII forms.
func normalizePowerShellQuotes(command string) string {
	return strings.NewReplacer(
		"\u2018", "'", "\u2019", "'", "\u201A", "'", "\u201B", "'",
		"\u201C", `"`, "\u201D", `"`, "\u201E", `"`, "\u201F", `"`,
	).Replace(command)
}
