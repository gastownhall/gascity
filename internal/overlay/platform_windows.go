//go:build windows

package overlay

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
)

// copilotHooksPath is GitHub Copilot CLI's repository hook file.
var copilotHooksPath = filepath.Join(".github", "hooks", "gascity.json")

// copilotInstructionsPath is GitHub Copilot CLI's repository instruction file.
var copilotInstructionsPath = filepath.Join(".github", "copilot-instructions.md")

// posixGCHookPrefix starts every command in the bundled Copilot hook file: it
// appends the usual go-install directories to PATH, then runs the gc that
// GC_BIN names, or gc.
const posixGCHookPrefix = `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && "${GC_BIN:-gc}" `

// powerShellGCHookPrefix is posixGCHookPrefix in PowerShell. Its fallback is
// gc.exe, because PowerShell resolves a bare gc to its Get-Content alias.
const powerShellGCHookPrefix = `$env:PATH = "$env:PATH;$HOME\go\bin;$HOME\.local\bin"; & $(if ($env:GC_BIN) { $env:GC_BIN } else { 'gc.exe' }) `

// AdaptSettingsForPlatform gives each bash command in Copilot CLI's hook file
// a powershell twin. On Windows, Copilot CLI runs a hook's powershell field and
// falls back to its bash field only when there is none; that bash is whatever
// PATH offers, typically WSL's, which has no gc.
//
// Only a command of the bundled shape is translated: posixGCHookPrefix
// followed by arguments that mean the same to PowerShell as to bash. An entry
// that already has a powershell field keeps it, and any other command is left
// as it is. Every other file is returned unchanged, as is a document that does
// not parse.
func AdaptSettingsForPlatform(relPath string, data []byte) []byte {
	switch {
	case isCopilotHooksPath(relPath):
		return adaptCopilotHooks(data)
	case isInstructionMarkdownPath(relPath):
		return adaptInstructionMarkdown(data)
	default:
		return data
	}
}

func platformAdaptsPath(relPath string) bool {
	return isCopilotHooksPath(relPath) || isInstructionMarkdownPath(relPath)
}

func platformAdaptsDirectoryCopies() bool {
	return true
}

// ManagedFileNeedsPlatformUpgrade returns a predicate that identifies the
// released Windows-managed bytes this build should silently upgrade. It is nil
// when relPath has no Windows-only managed-byte adaptation.
func ManagedFileNeedsPlatformUpgrade(relPath string, legacyData, desiredData []byte) func([]byte) bool {
	if !isInstructionMarkdownPath(relPath) || bytes.Equal(legacyData, desiredData) {
		return nil
	}
	legacy := append([]byte(nil), legacyData...)
	return func(existing []byte) bool {
		return bytes.Equal(existing, legacy)
	}
}

func preservePlatformManagedFile(relPath string, existing, legacyData, desiredData []byte) bool {
	if !isInstructionMarkdownPath(relPath) || bytes.Equal(legacyData, desiredData) {
		return false
	}
	return !bytes.Equal(existing, legacyData)
}

func platformPreserveExistingPath(relPath string) bool {
	return isRootAgentsMarkdownPath(relPath)
}

func isCopilotHooksPath(relPath string) bool {
	return strings.EqualFold(cleanPlatformRelPath(relPath), filepath.ToSlash(copilotHooksPath))
}

func isInstructionMarkdownPath(relPath string) bool {
	clean := cleanPlatformRelPath(relPath)
	return strings.EqualFold(clean, filepath.ToSlash(copilotInstructionsPath)) ||
		isRootAgentsMarkdownPath(clean)
}

func isRootAgentsMarkdownPath(relPath string) bool {
	return strings.EqualFold(cleanPlatformRelPath(relPath), "AGENTS.md")
}

func cleanPlatformRelPath(relPath string) string {
	return filepath.ToSlash(filepath.Clean(relPath))
}

func adaptCopilotHooks(data []byte) []byte {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return data
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return data
	}
	changed := false
	for _, event := range hooks {
		entries, ok := event.([]any)
		if !ok {
			continue
		}
		for _, item := range entries {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if _, has := entry["powershell"]; has {
				continue
			}
			bash, ok := entry["bash"].(string)
			if !ok {
				continue
			}
			if ps, ok := powerShellGCHook(bash); ok {
				entry["powershell"] = ps
				changed = true
			}
		}
	}
	if !changed {
		return data
	}
	out, err := MarshalCanonicalJSON(doc)
	if err != nil {
		return data
	}
	return out
}

func adaptInstructionMarkdown(data []byte) []byte {
	text := string(data)
	var out strings.Builder
	changed := false
	last := 0
	inFence := false
	var fenceChar byte
	fenceLen := 0
	fenceShell := false
	var fenceCtx fenceContext
	for i := 0; i < len(text); {
		if atLineStart(text, i) {
			lineEnd, nextLine := lineBounds(text, i)
			line := text[i:lineEnd]
			if inFence {
				if !fenceContainerContinues(line, fenceCtx) {
					inFence = false
					fenceShell = false
					fenceCtx = fenceContext{}
					continue
				}
				if fenceShell {
					replacement := adaptFencedCommandLine(line, fenceCtx)
					if replacement != line {
						if !changed {
							out.Grow(len(text) + 4)
							changed = true
						}
						out.WriteString(text[last:i])
						out.WriteString(replacement)
						last = lineEnd
					}
				}
				if isClosingFenceLine(line, fenceChar, fenceLen, fenceCtx) {
					inFence = false
					fenceShell = false
					fenceCtx = fenceContext{}
				}
				i = nextLine
				continue
			}
			if ch, n, shell, ctx, ok := openingFenceLine(line); ok {
				inFence = true
				fenceChar = ch
				fenceLen = n
				fenceShell = shell
				fenceCtx = ctx
				i = nextLine
				continue
			}
		}
		if text[i] != '`' || escapedBacktick(text, i) {
			i++
			continue
		}
		j := i
		for j < len(text) && text[j] == '`' {
			j++
		}
		runLen := j - i
		close := findClosingBacktickRun(text, j, runLen)
		if close < 0 {
			i = j
			continue
		}
		code := text[j:close]
		replacement := adaptCodeSpanCommands(code)
		if replacement != code {
			if !changed {
				out.Grow(len(text) + 4)
				changed = true
			}
			out.WriteString(text[last:j])
			out.WriteString(replacement)
			last = close
		}
		i = close + runLen
	}
	if !changed {
		return data
	}
	out.WriteString(text[last:])
	return []byte(out.String())
}

func adaptCodeSpanCommands(code string) string {
	start := 0
	end := len(code)
	if len(code) >= 2 && code[0] == ' ' && code[len(code)-1] == ' ' {
		start = 1
		end--
	}
	replacement := adaptInstructionCommands(code[start:end])
	if replacement == code[start:end] {
		return code
	}
	return code[:start] + replacement + code[end:]
}

func adaptFencedCommandLine(line string, ctx fenceContext) string {
	prefixLen, ok := fencedCommandPrefixLen(line, ctx)
	if !ok {
		return line
	}
	if prefixLen > len(line) {
		return line
	}
	replacement := adaptInstructionCommands(line[prefixLen:])
	if replacement == line[prefixLen:] {
		return line
	}
	return line[:prefixLen] + replacement
}

func fencedCommandPrefixLen(line string, ctx fenceContext) (int, bool) {
	i, ok := fencedLineContentOffset(line, ctx)
	if !ok {
		return 0, false
	}
	i += shellPromptLen(line[i:])
	return i, true
}

type fenceContext struct {
	containers []fenceContainer
}

type fenceContainer struct {
	kind   fenceContainerKind
	indent int
}

type fenceContainerKind int

const (
	fenceContainerBlockquote fenceContainerKind = iota + 1
	fenceContainerList
)

func shellPromptLen(line string) int {
	switch {
	case strings.HasPrefix(line, "$ "), strings.HasPrefix(line, "> "):
		return 2
	case strings.HasPrefix(line, "PS> "):
		return len("PS> ")
	case strings.HasPrefix(line, "PS "):
		if end := strings.Index(line, "> "); end >= len("PS ") {
			return end + len("> ")
		}
	}
	return 0
}

func atLineStart(text string, i int) bool {
	return i == 0 || text[i-1] == '\n'
}

func lineBounds(text string, start int) (int, int) {
	end := start
	for end < len(text) && text[end] != '\n' {
		end++
	}
	contentEnd := end
	if contentEnd > start && text[contentEnd-1] == '\r' {
		contentEnd--
	}
	if end < len(text) {
		end++
	}
	return contentEnd, end
}

func openingFenceLine(line string) (byte, int, bool, fenceContext, bool) {
	i, ctx := openingFenceContentOffset(line)
	if i >= len(line) || (line[i] != '`' && line[i] != '~') {
		return 0, 0, false, fenceContext{}, false
	}
	ch := line[i]
	j := i
	for j < len(line) && line[j] == ch {
		j++
	}
	if j-i < 3 {
		return 0, 0, false, fenceContext{}, false
	}
	info := strings.TrimSpace(line[j:])
	if ch == '`' && strings.Contains(info, "`") {
		return 0, 0, false, fenceContext{}, false
	}
	return ch, j - i, shellFenceInfo(info), ctx, true
}

func isClosingFenceLine(line string, ch byte, n int, ctx fenceContext) bool {
	i, ok := fencedLineContentOffset(line, ctx)
	if !ok {
		return false
	}
	j := i
	for j < len(line) && line[j] == ch {
		j++
	}
	if j-i < n {
		return false
	}
	for j < len(line) {
		if line[j] != ' ' && line[j] != '\t' {
			return false
		}
		j++
	}
	return true
}

func openingFenceContentOffset(line string) (int, fenceContext) {
	i := 0
	ctx := fenceContext{}
	for {
		containerStart := i
		spaces := upToThreeSpaces(line[i:])
		j := i + spaces
		if j < len(line) && line[j] == '>' {
			ctx.containers = append(ctx.containers, fenceContainer{kind: fenceContainerBlockquote})
			i = j + 1
			if i < len(line) && (line[i] == ' ' || line[i] == '\t') {
				i++
			}
			continue
		}
		if n := listMarkerLen(line[j:]); n > 0 {
			i = j + n
			for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
				i++
			}
			ctx.containers = append(ctx.containers, fenceContainer{
				kind:   fenceContainerList,
				indent: i - containerStart,
			})
			continue
		}
		i = j
		break
	}
	return i, ctx
}

func fenceContainerContinues(line string, ctx fenceContext) bool {
	_, ok := fencedLineContentOffset(line, ctx)
	return ok
}

func fencedLineContentOffset(line string, ctx fenceContext) (int, bool) {
	i := 0
	for _, container := range ctx.containers {
		switch container.kind {
		case fenceContainerBlockquote:
			i += upToThreeSpaces(line[i:])
			if i >= len(line) || line[i] != '>' {
				return 0, false
			}
			i++
			if i < len(line) && (line[i] == ' ' || line[i] == '\t') {
				i++
			}
		case fenceContainerList:
			if strings.TrimSpace(line[i:]) == "" {
				return len(line), fenceContextListsOnly(ctx)
			}
			indent := 0
			for i+indent < len(line) && (line[i+indent] == ' ' || line[i+indent] == '\t') {
				indent++
			}
			if indent < container.indent {
				return 0, false
			}
			i += container.indent
		}
	}
	i += upToThreeSpaces(line[i:])
	return i, true
}

func fenceContextListsOnly(ctx fenceContext) bool {
	if len(ctx.containers) == 0 {
		return false
	}
	for _, container := range ctx.containers {
		if container.kind != fenceContainerList {
			return false
		}
	}
	return true
}

func upToThreeSpaces(s string) int {
	n := 0
	for n < len(s) && n < 3 && s[n] == ' ' {
		n++
	}
	return n
}

func listMarkerLen(s string) int {
	if len(s) >= 2 && (s[0] == '-' || s[0] == '+' || s[0] == '*') && (s[1] == ' ' || s[1] == '\t') {
		return 1
	}
	i := 0
	for i < len(s) && i < 9 && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(s) || (s[i] != '.' && s[i] != ')') {
		return 0
	}
	if i+1 >= len(s) || (s[i+1] != ' ' && s[i+1] != '\t') {
		return 0
	}
	return i + 1
}

func shellFenceInfo(info string) bool {
	if info == "" {
		return true
	}
	fields := strings.Fields(info)
	if len(fields) == 0 {
		return true
	}
	switch strings.ToLower(fields[0]) {
	case "sh", "bash", "shell", "console", "powershell", "pwsh", "ps1", "ps", "cmd", "bat", "text":
		return true
	default:
		return false
	}
}

func escapedBacktick(text string, i int) bool {
	backslashes := 0
	for j := i - 1; j >= 0 && text[j] == '\\'; j-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func findClosingBacktickRun(text string, start, runLen int) int {
	for i := start; i < len(text); {
		if atLineStart(text, i) {
			lineEnd, _ := lineBounds(text, i)
			line := text[i:lineEnd]
			if strings.TrimSpace(line) == "" {
				return -1
			}
			if _, _, _, _, ok := openingFenceLine(line); ok {
				return -1
			}
		}
		if text[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(text) && text[j] == '`' {
			j++
		}
		if j-i == runLen {
			return i
		}
		i = j
	}
	return -1
}

func adaptInstructionCommands(code string) string {
	var out strings.Builder
	changed := false
	last := 0
	commandPosition := true
	inSingle := false
	inDouble := false
	for i := 0; i < len(code); {
		if commandPosition {
			for i < len(code) && isCommandWhitespace(code[i]) {
				i++
			}
			if instructionGCCommandAt(code, i) && !instructionGCIsGetContent(code, i+len("gc")) {
				if !changed {
					out.Grow(len(code) + 4)
					changed = true
				}
				out.WriteString(code[last:i])
				out.WriteString("gc.exe")
				last = i + len("gc")
				i += len("gc")
				commandPosition = false
				continue
			}
		}
		if i >= len(code) {
			break
		}
		if inSingle {
			if code[i] == '\'' {
				if i+1 < len(code) && code[i+1] == '\'' {
					i += 2
					continue
				}
				inSingle = false
			}
			i++
			continue
		}
		if inDouble {
			if code[i] == '`' {
				i += 2
				continue
			}
			if code[i] == '"' {
				inDouble = false
			}
			i++
			continue
		}
		switch code[i] {
		case '\'':
			inSingle = true
			commandPosition = false
			i++
		case '"':
			inDouble = true
			commandPosition = false
			i++
		case '`':
			commandPosition = false
			i += 2
		case ';':
			commandPosition = true
			i++
		case '\r':
			commandPosition = true
			i++
			if i < len(code) && code[i] == '\n' {
				i++
			}
		case '\n':
			commandPosition = true
			i++
		case '(':
			commandPosition = true
			i++
		case '|':
			commandPosition = true
			i++
			if i < len(code) && code[i] == '|' {
				i++
			}
		case '&':
			if i+1 < len(code) && code[i+1] == '&' {
				commandPosition = true
				i += 2
			} else {
				commandPosition = true
				i++
			}
		default:
			commandPosition = false
			i++
		}
	}
	if !changed {
		return code
	}
	out.WriteString(code[last:])
	return out.String()
}

func instructionGCCommandAt(code string, i int) bool {
	if i < 0 || i+len("gc") > len(code) || code[i:i+len("gc")] != "gc" {
		return false
	}
	j := i + len("gc")
	return j == len(code) || isCommandWhitespace(code[j]) || isCommandSeparator(code[j])
}

func instructionGCIsGetContent(code string, argStart int) bool {
	for argStart < len(code) && isCommandHorizontalWhitespace(code[argStart]) {
		argStart++
	}
	if argStart >= len(code) || isCommandSeparator(code[argStart]) {
		return false
	}
	if code[argStart] == '\r' || code[argStart] == '\n' {
		return false
	}
	if code[argStart] == '\'' || code[argStart] == '"' {
		return true
	}
	argEnd := argStart
	for argEnd < len(code) && !isCommandWhitespace(code[argEnd]) && !isCommandSeparator(code[argEnd]) {
		if code[argEnd] == '`' && argEnd+1 < len(code) {
			argEnd += 2
			continue
		}
		argEnd++
	}
	arg := code[argStart:argEnd]
	if arg == "" {
		return false
	}
	if strings.HasPrefix(arg, "--") {
		return false
	}
	if strings.HasPrefix(arg, "-") {
		return isGetContentParameter(arg)
	}
	return strings.ContainsAny(arg, `.\/:*?$~`)
}

func isGetContentParameter(arg string) bool {
	name := strings.TrimPrefix(arg, "-")
	if strings.EqualFold(name, "h") || strings.EqualFold(name, "help") {
		return false
	}
	for _, param := range getContentParameters {
		if len(name) > 0 && len(name) <= len(param) && strings.EqualFold(name, param[:len(name)]) {
			return true
		}
	}
	return false
}

func isCommandWhitespace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n'
}

func isCommandHorizontalWhitespace(ch byte) bool {
	return ch == ' ' || ch == '\t'
}

func isCommandSeparator(ch byte) bool {
	return ch == ';' || ch == '|' || ch == '&' || ch == '(' || ch == ')'
}

var getContentParameters = []string{
	"Path",
	"LiteralPath",
	"Tail",
	"TotalCount",
	"Head",
	"First",
	"Last",
	"Raw",
	"Encoding",
	"Wait",
	"ReadCount",
	"Delimiter",
	"Filter",
	"Include",
	"Exclude",
	"Stream",
	"AsByteStream",
	"Force",
	"Credential",
}

func powerShellGCHook(bash string) (string, bool) {
	args, ok := strings.CutPrefix(bash, posixGCHookPrefix)
	if !ok || !portableHookArgs(args) {
		return "", false
	}
	return powerShellGCHookPrefix + args, true
}

// portableHookArgs reports whether args reach gc as the same argument list
// from PowerShell as from bash: words separated by single spaces, each either
// bare or wholly enclosed in double quotes, made of letters, digits and the
// characters -._/=: (and spaces, inside quotes). A word of hyphens such as --
// passes through both shells unchanged (measured on pwsh 7.6 and Windows
// PowerShell 5.1).
func portableHookArgs(args string) bool {
	words := 0
	for i := 0; i < len(args); {
		switch {
		case args[i] == ' ':
			i++
		case args[i] == '"':
			end := strings.IndexByte(args[i+1:], '"')
			if end <= 0 || !portableHookWord(args[i+1:i+1+end], true) {
				return false
			}
			i += end + 2
			if i < len(args) && args[i] != ' ' {
				return false
			}
			words++
		default:
			end := strings.IndexByte(args[i:], ' ')
			if end < 0 {
				end = len(args) - i
			}
			if !portableHookWord(args[i:i+end], false) {
				return false
			}
			i += end
			words++
		}
	}
	return words > 0
}

func portableHookWord(word string, quoted bool) bool {
	for _, r := range word {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-._/=:", r):
		case quoted && r == ' ':
		default:
			return false
		}
	}
	return true
}
