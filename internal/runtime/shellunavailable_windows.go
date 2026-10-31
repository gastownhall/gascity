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
// statements, pipeline elements and ordinary control-flow script blocks, and
// refuses literal program names there. It does not try to resolve dynamic
// forms such as Start-Process arguments, Invoke-Expression text, functions,
// aliases or a program name supplied solely by a variable; a command-position
// token whose literal text names a .sh path is still refused without resolving
// its variable prefix.
//
// Reading literal text makes it conservative both ways. It refuses what would
// never run, such as a POSIX shell named in a branch never taken or a function
// the script itself names sh, and it does not see a POSIX shell another
// program starts, as cmd /c bash, wsl -e bash or pwsh -c bash do.
func ShellCommandUnavailable(command string) string {
	for _, program := range commandPrograms(command) {
		if reason := shellProgramUnavailable(program); reason != "" {
			return reason
		}
	}
	return ""
}

func shellProgramUnavailable(program string) string {
	base := strings.ToLower(filepath.Base(program))
	switch {
	case strings.HasSuffix(base, ".sh"):
		return fmt.Sprintf("its program %s is a POSIX shell script, and Gas City runs no POSIX shell on Windows", program)
	case posixShells[strings.TrimSuffix(base, ".exe")]:
		return fmt.Sprintf("its program %s is a POSIX shell, and Gas City runs no POSIX shell on Windows", program)
	}
	return ""
}

// commandProgram is the first statically named program command may run.
func commandProgram(command string) string {
	programs := commandPrograms(command)
	if len(programs) == 0 {
		return ""
	}
	return programs[0]
}

// commandPrograms returns the literal program names PowerShell can see in
// command position. The scanner follows the subset the gate needs rather than
// evaluating PowerShell: dynamic invocations are deliberately absent.
func commandPrograms(command string) []string {
	scanner := powerShellCommandScanner{command: normalizePowerShellCommandScannerInput(command)}
	scanner.scanStatements(0)
	return scanner.programs
}

// ShellCommandPrograms returns literal program names in PowerShell command
// positions, or nil if any command position cannot be resolved statically.
// Programs launched inside scripts still require configured process names and
// interpreter hosts.
func ShellCommandPrograms(command string) []string {
	scanner := powerShellCommandScanner{command: normalizePowerShellCommandScannerInput(command)}
	scanner.scanStatements(0)
	if scanner.unresolved {
		return nil
	}
	return scanner.programs
}

type powerShellCommandScanner struct {
	command    string
	position   int
	programs   []string
	unresolved bool
}

func (s *powerShellCommandScanner) scanStatements(stop byte) {
	atCommand := true
	afterCall := false
	for s.position < len(s.command) {
		if stop != 0 && s.command[s.position] == stop {
			s.position++
			return
		}
		if s.skipSpaceAndComments(&atCommand, &afterCall) {
			continue
		}
		if s.position >= len(s.command) {
			return
		}
		if stop != 0 && s.command[s.position] == stop {
			s.position++
			return
		}
		if s.consumeStatementSeparator(&atCommand, &afterCall) {
			continue
		}
		if !atCommand {
			s.skipNonCommandToken(&atCommand, &afterCall)
			continue
		}
		switch s.command[s.position] {
		case '&':
			afterCall = true
			s.position++

		case '.':
			if s.dotStartsCallOperator() {
				afterCall = true
				s.position++
				continue
			}
			program := s.readBareWord()
			s.addProgramIfCommand(program)
			atCommand = false
			afterCall = false

		case '\'', '"':
			program, static, ok := s.readQuotedString(s.command[s.position])
			if afterCall && ok && static {
				s.addProgramIfCommand(program)
			} else {
				s.unresolved = true
			}
			atCommand = false
			afterCall = false

		case '(':
			if afterCall {
				if program, ok := s.readParenthesizedConstantString(); ok {
					s.addProgramIfCommand(program)
				} else {
					s.unresolved = true
				}
			} else {
				s.position++
				s.scanStatements(')')
			}
			atCommand = false
			afterCall = false

		case '@':
			s.unresolved = true
			if s.position+1 < len(s.command) && (s.command[s.position+1] == '(' || s.command[s.position+1] == '{') {
				s.position++
				if s.command[s.position] == '(' {
					s.position++
					s.scanStatements(')')
				} else {
					s.skipBalanced('{', '}')
				}
			} else {
				s.position++
			}
			atCommand = false
			afterCall = false

		case '[':
			s.unresolved = true
			s.skipBalanced('[', ']')
			atCommand = false
			afterCall = false

		case '{':
			s.position++
			if afterCall {
				s.scanStatements('}')
			} else {
				s.unresolved = true
				s.position--
				s.skipBalanced('{', '}')
			}
			atCommand = false
			afterCall = false

		case '$':
			s.unresolved = true
			program := s.readVariableCommandToken()
			if strings.ContainsAny(program, `/\`) {
				s.addProgramIfCommand(program)
			}
			if s.consumeAssignmentOperatorAfterTrivia() {
				atCommand = true
				afterCall = false
				continue
			}
			atCommand = false
			afterCall = false

		default:
			program := s.readBareWord()
			if program == "" {
				s.position++
				continue
			}
			switch strings.ToLower(program) {
			case "return", "throw":
				atCommand = true
				afterCall = false
				continue
			case "function", "filter", "workflow", "configuration":
				s.unresolved = true
				s.scanFunctionDefinition()
				atCommand = true
				afterCall = false
				continue
			case "begin", "process", "end", "dynamicparam", "clean":
				if s.scanFollowingScriptBlock() {
					atCommand = true
					afterCall = false
					continue
				}
			}
			switch {
			case s.scanControlKeyword(program):
				atCommand = true
				afterCall = false
				continue
			case powerShellNonCommandKeyword(program):
			default:
				s.addProgramIfCommand(program)
			}
			atCommand = false
			afterCall = false
		}
	}
}

func (s *powerShellCommandScanner) addProgramIfCommand(program string) {
	switch strings.ToLower(filepath.Base(program)) {
	case "invoke-expression", "iex", "start-process", "start", "saps", "invoke-item", "ii",
		"invoke-command", "icm", "set-alias", "new-alias": // icm: built-in alias of Invoke-Command
		s.unresolved = true
	}
	if powerShellCommandNameCanBeLiteral(program) {
		s.programs = append(s.programs, program)
	} else {
		s.unresolved = true
	}
}

func (s *powerShellCommandScanner) skipSpaceAndComments(atCommand *bool, afterCall *bool) bool {
	consumed := false
	for s.position < len(s.command) {
		switch s.command[s.position] {
		case ' ', '\t', '\v', '\f':
			s.position++
			consumed = true

		case '\r', '\n':
			s.skipNewline()
			*atCommand = true
			*afterCall = false
			consumed = true

		case '#':
			s.skipLineComment()
			consumed = true

		case '<':
			if s.position+1 >= len(s.command) || s.command[s.position+1] != '#' {
				return consumed
			}
			if s.skipBlockComment() {
				*atCommand = true
				*afterCall = false
			}
			consumed = true

		default:
			return consumed
		}
	}
	return consumed
}

func (s *powerShellCommandScanner) consumeStatementSeparator(atCommand *bool, afterCall *bool) bool {
	switch s.command[s.position] {
	case ';':
		s.position++
	case '|':
		if s.position+1 < len(s.command) && s.command[s.position+1] == '|' {
			s.position += 2
		} else {
			s.position++
		}
	case '&':
		if s.position+1 >= len(s.command) || s.command[s.position+1] != '&' {
			return false
		}
		s.position += 2
	case '}':
		s.position++
	default:
		return false
	}
	*atCommand = true
	*afterCall = false
	return true
}

func (s *powerShellCommandScanner) skipNonCommandToken(atCommand *bool, afterCall *bool) {
	if s.consumeStatementSeparator(atCommand, afterCall) {
		return
	}
	if s.command[s.position] == '&' && (s.position == 0 || s.command[s.position-1] != '>') {
		s.unresolved = true
	}
	s.skipExpressionToken()
}

func (s *powerShellCommandScanner) skipExpressionToken() {
	if s.position >= len(s.command) {
		return
	}
	switch s.command[s.position] {
	case '\'', '"':
		_, _, _ = s.readQuotedString(s.command[s.position])
	case '(':
		s.position++
		s.scanStatements(')')
	case '[':
		s.skipBalanced('[', ']')
	case '{':
		s.position++
		s.scanStatements('}')
	case '@':
		if s.position+1 < len(s.command) && (s.command[s.position+1] == '(' || s.command[s.position+1] == '{') {
			s.position++
			if s.command[s.position] == '(' {
				s.position++
				s.scanStatements(')')
			} else {
				s.unresolved = true
				s.skipBalanced('{', '}')
			}
		} else {
			s.position++
		}
	case '`':
		_ = s.readBacktickEscape()
	default:
		s.position++
	}
}

func (s *powerShellCommandScanner) readBareWord() string {
	var word strings.Builder
	for s.position < len(s.command) {
		ch := s.command[s.position]
		if bareWordTerminator(ch) {
			break
		}
		switch ch {
		case '`':
			word.WriteString(s.readBacktickEscape())

		case '\'', '"':
			part, static, ok := s.readQuotedString(ch)
			if !ok || !static {
				return word.String()
			}
			word.WriteString(part)

		default:
			word.WriteByte(ch)
			s.position++
		}
	}
	return word.String()
}

func bareWordTerminator(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\v' || ch == '\f' ||
		ch == '\r' || ch == '\n' || ch == ';' || ch == '|' ||
		ch == '&' || ch == '(' || ch == ')' || ch == '{' || ch == '}' ||
		ch == '>'
}

func (s *powerShellCommandScanner) readQuotedString(quote byte) (string, bool, bool) {
	if quote == '\'' {
		return s.readSingleQuotedString()
	}
	return s.readDoubleQuotedString()
}

func (s *powerShellCommandScanner) readSingleQuotedString() (string, bool, bool) {
	s.position++
	var value strings.Builder
	for s.position < len(s.command) {
		ch := s.command[s.position]
		if ch == '\'' {
			if s.position+1 < len(s.command) && s.command[s.position+1] == '\'' {
				value.WriteByte('\'')
				s.position += 2
				continue
			}
			s.position++
			return value.String(), true, true
		}
		value.WriteByte(ch)
		s.position++
	}
	return value.String(), true, false
}

func (s *powerShellCommandScanner) readDoubleQuotedString() (string, bool, bool) {
	s.position++
	static := true
	var value strings.Builder
	for s.position < len(s.command) {
		ch := s.command[s.position]
		switch ch {
		case '"':
			s.position++
			return value.String(), static, true
		case '`':
			value.WriteString(s.readBacktickEscape())
		case '$':
			if s.position+1 < len(s.command) && s.command[s.position+1] == '(' {
				static = false
				s.position += 2
				s.scanStatements(')')
				continue
			}
			static = false
			value.WriteByte(ch)
			s.position++
		default:
			value.WriteByte(ch)
			s.position++
		}
	}
	return value.String(), static, false
}

func (s *powerShellCommandScanner) readBacktickEscape() string {
	s.position++
	if s.position >= len(s.command) {
		return ""
	}
	ch := s.command[s.position]
	s.position++
	switch ch {
	case '0':
		return "\x00"
	case 'a':
		return "\a"
	case 'b':
		return "\b"
	case 'f':
		return "\f"
	case 'n':
		return "\n"
	case 'r':
		return "\r"
	case 't':
		return "\t"
	case 'v':
		return "\v"
	case '\r':
		if s.position < len(s.command) && s.command[s.position] == '\n' {
			s.position++
		}
		return ""
	case '\n':
		return ""
	default:
		return string(ch)
	}
}

func (s *powerShellCommandScanner) readParenthesizedConstantString() (string, bool) {
	start := s.position
	s.position++
	s.skipParenTrivia()
	if s.position >= len(s.command) || (s.command[s.position] != '\'' && s.command[s.position] != '"') {
		s.position = start
		s.skipBalanced('(', ')')
		return "", false
	}
	value, static, ok := s.readQuotedString(s.command[s.position])
	s.skipParenTrivia()
	if !ok || !static || s.position >= len(s.command) || s.command[s.position] != ')' {
		s.position = start
		s.skipBalanced('(', ')')
		return "", false
	}
	s.position++
	return value, true
}

func (s *powerShellCommandScanner) skipParenTrivia() {
	for s.position < len(s.command) {
		switch s.command[s.position] {
		case ' ', '\t', '\v', '\f', '\r', '\n':
			if s.command[s.position] == '\r' || s.command[s.position] == '\n' {
				s.skipNewline()
			} else {
				s.position++
			}
		case '#':
			s.skipLineComment()
		case '<':
			if s.position+1 >= len(s.command) || s.command[s.position+1] != '#' {
				return
			}
			s.skipBlockComment()
		default:
			return
		}
	}
}

func (s *powerShellCommandScanner) skipBalanced(open byte, close byte) {
	depth := 0
	for s.position < len(s.command) {
		switch s.command[s.position] {
		case '\'', '"':
			_, _, _ = s.readQuotedString(s.command[s.position])
		case '#':
			s.skipLineComment()
		case '<':
			if s.position+1 < len(s.command) && s.command[s.position+1] == '#' {
				s.skipBlockComment()
			} else {
				s.position++
			}
		default:
			ch := s.command[s.position]
			s.position++
			if ch == open {
				depth++
			}
			if ch == close {
				depth--
				if depth == 0 {
					return
				}
			}
		}
	}
}

func (s *powerShellCommandScanner) skipLineComment() {
	for s.position < len(s.command) && s.command[s.position] != '\r' && s.command[s.position] != '\n' {
		s.position++
	}
}

func (s *powerShellCommandScanner) skipBlockComment() bool {
	s.position += 2
	sawNewline := false
	for s.position < len(s.command) {
		if s.command[s.position] == '\r' || s.command[s.position] == '\n' {
			sawNewline = true
			s.skipNewline()
			continue
		}
		if s.command[s.position] == '#' && s.position+1 < len(s.command) && s.command[s.position+1] == '>' {
			s.position += 2
			return sawNewline
		}
		s.position++
	}
	return true
}

func (s *powerShellCommandScanner) skipNewline() {
	if s.command[s.position] == '\r' && s.position+1 < len(s.command) && s.command[s.position+1] == '\n' {
		s.position += 2
		return
	}
	s.position++
}

func (s *powerShellCommandScanner) dotStartsCallOperator() bool {
	position := s.position + 1
	if position >= len(s.command) {
		return false
	}
	switch s.command[position] {
	case ' ', '\t', '\v', '\f', '\r', '\n', '{', '(', '\'', '"':
		return true
	default:
		return false
	}
}

func (s *powerShellCommandScanner) readVariableCommandToken() string {
	start := s.position
	s.position++
	if s.position < len(s.command) && s.command[s.position] == '{' {
		s.position++
		for s.position < len(s.command) && s.command[s.position] != '}' {
			s.position++
		}
		if s.position < len(s.command) {
			s.position++
		}
	} else {
		for s.position < len(s.command) {
			ch := s.command[s.position]
			if ch == ':' || ch == '_' || ch == '?' || unicode.IsLetter(rune(ch)) || unicode.IsDigit(rune(ch)) {
				s.position++
				continue
			}
			break
		}
	}
	if s.position < len(s.command) && (s.command[s.position] == '\\' || s.command[s.position] == '/') {
		for s.position < len(s.command) && !bareWordTerminator(s.command[s.position]) {
			s.position++
		}
	}
	return s.command[start:s.position]
}

func (s *powerShellCommandScanner) consumeAssignmentOperatorAfterTrivia() bool {
	start := s.position
	for s.position < len(s.command) {
		switch s.command[s.position] {
		case ' ', '\t', '\v', '\f':
			s.position++
		default:
			goto operator
		}
	}

operator:
	if s.position >= len(s.command) {
		s.position = start
		return false
	}
	if s.command[s.position] == '=' {
		s.position++
		return true
	}
	if s.position+1 < len(s.command) && s.command[s.position+1] == '=' {
		switch s.command[s.position] {
		case '+', '-', '*', '/', '%':
			s.position += 2
			return true
		}
	}
	s.position = start
	return false
}

func (s *powerShellCommandScanner) scanFunctionDefinition() {
	for s.position < len(s.command) {
		s.skipTriviaInExpression()
		if s.position >= len(s.command) {
			return
		}
		if s.command[s.position] == '{' {
			s.position++
			s.scanStatements('}')
			return
		}
		s.skipExpressionToken()
	}
}

func (s *powerShellCommandScanner) scanControlKeyword(word string) bool {
	switch strings.ToLower(word) {
	case "if", "elseif", "while", "for", "foreach", "catch", "trap", "data":
		s.scanUntilControlBlock()
		return true
	case "else", "try", "finally", "do":
		s.scanFollowingScriptBlock()
		return true
	case "switch":
		s.scanSwitchStatement()
		return true
	default:
		return false
	}
}

func (s *powerShellCommandScanner) scanUntilControlBlock() {
	for s.position < len(s.command) {
		s.skipTriviaInExpression()
		if s.position >= len(s.command) {
			return
		}
		if s.command[s.position] == '{' {
			s.position++
			s.scanStatements('}')
			return
		}
		s.skipExpressionToken()
	}
}

func (s *powerShellCommandScanner) scanFollowingScriptBlock() bool {
	for s.position < len(s.command) {
		s.skipTriviaInExpression()
		if s.position >= len(s.command) || s.command[s.position] != '{' {
			return false
		}
		s.position++
		s.scanStatements('}')
		return true
	}
	return false
}

func (s *powerShellCommandScanner) scanSwitchStatement() {
	for s.position < len(s.command) {
		s.skipTriviaInExpression()
		if s.position >= len(s.command) {
			return
		}
		if s.command[s.position] == '{' {
			s.position++
			s.scanSwitchClauses()
			return
		}
		s.skipExpressionToken()
	}
}

func (s *powerShellCommandScanner) scanSwitchClauses() {
	for s.position < len(s.command) {
		s.skipTriviaInExpression()
		if s.position >= len(s.command) {
			return
		}
		if s.command[s.position] == '}' {
			s.position++
			return
		}
		for s.position < len(s.command) {
			s.skipTriviaInExpression()
			if s.position >= len(s.command) || s.command[s.position] == '}' {
				break
			}
			if s.command[s.position] == '{' {
				s.position++
				s.scanStatements('}')
				break
			}
			s.skipExpressionToken()
		}
	}
}

func (s *powerShellCommandScanner) skipTriviaInExpression() {
	for s.position < len(s.command) {
		switch s.command[s.position] {
		case ' ', '\t', '\v', '\f', '\r', '\n':
			if s.command[s.position] == '\r' || s.command[s.position] == '\n' {
				s.skipNewline()
			} else {
				s.position++
			}
		case '#':
			s.skipLineComment()
		case '<':
			if s.position+1 >= len(s.command) || s.command[s.position+1] != '#' {
				return
			}
			s.skipBlockComment()
		default:
			return
		}
	}
}

func powerShellCommandNameCanBeLiteral(program string) bool {
	for _, first := range program {
		return !unicode.IsDigit(first)
	}
	return false
}

func normalizePowerShellCommandScannerInput(command string) string {
	var b strings.Builder
	changed := false
	for _, r := range command {
		switch r {
		case '\u2018', '\u2019', '\u201A', '\u201B':
			b.WriteByte('\'')
			changed = true
		case '\u201C', '\u201D', '\u201E', '\u201F':
			b.WriteByte('"')
			changed = true
		default:
			b.WriteRune(r)
		}
	}
	if !changed {
		return command
	}
	return b.String()
}

func powerShellNonCommandKeyword(word string) bool {
	switch strings.ToLower(word) {
	case "using", "param", "function", "filter", "workflow", "configuration", "class", "enum", "return", "throw", "break", "continue", "exit":
		return true
	default:
		return false
	}
}
