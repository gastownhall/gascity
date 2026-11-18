package scripts_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

// The early-exit-reader guard: a reader at the end of a pipe that can exit
// before its input ends (grep -q, head, sed ...q, awk ... exit) lets the
// producer die of SIGPIPE (141). Under `set -o pipefail` that turns a MATCH
// into a miss, but only when the producer's output is larger than a few pipe
// writes, so small inputs pass and large ones silently flip. Two incidents in
// one week (upstream #6714, #6866) and a third fixed earlier (#3796) were all
// this one shape. Mechanism, measured flip rates, the scope rule and the
// rewrite table: engdocs/contributors/shell-pipefail-early-exit.md.
//
//	shell_pipefail_early_exit_test.go        this file: the finding type, the
//	                                         discovery and analyzer entry points,
//	                                         and the whole-repo test
//	shell_pipefail_early_exit_rules_test.go  table-driven fixtures: what is
//	                                         flagged, what is not, and the
//	                                         historical regressions verbatim
//
// The guard parses every tracked shell source with mvdan.cc/sh/v3/syntax (parse
// only, never interp) and has no exemption mechanism of any kind: no allow
// comment, no baseline file, no directory exclusion list. The remedy is always
// a one-token rewrite, and an escape hatch is what rots.

// shellDialect is the dialect a source is written for. It decides both whether
// the source is checked (a POSIX sh file is only in scope when pipefail is on
// for it) and which rewrite the finding recommends (here-strings and process
// substitution are bash-only).
type shellDialect int

const (
	// dialectBash is bash (shebang bash or env bash, *.bash, or a *.sh with no
	// shebang). It is ALWAYS checked, whether or not it sets pipefail: a library
	// inherits its sourcer's options and removing pipefail must never silence
	// the guard.
	dialectBash shellDialect = iota
	// dialectPOSIX is sh, dash, ash or ksh. It is checked only when it sets
	// pipefail itself or is sourced, transitively, from an in-scope file.
	dialectPOSIX
)

// shellSource is one parseable unit of shell: a whole script file, or one
// run: block of a GitHub Actions workflow.
type shellSource struct {
	// Path is the repo-relative, slash-separated path of the file.
	Path string
	// Dialect is the dialect the source is written for.
	Dialect shellDialect
	// Text is the shell text exactly as the parser should see it.
	Text string
	// LineOffset is added to a parsed line to give the file line. It is 0 for a
	// script file and, for a workflow run: block, the YAML line before the
	// block's first line, so a finding names the YAML line.
	LineOffset int
	// Workflow marks a run: block of a workflow file.
	Workflow bool
	// WorkflowShell is the step's explicit shell: value ("" when unspecified).
	// GitHub documents an unspecified shell as `bash -e {0}`, which has no
	// pipefail, and an explicit `shell: bash` as `bash -eo pipefail {0}`.
	WorkflowShell string
}

// pipefailFinding is one reader that can exit before its input ends.
type pipefailFinding struct {
	// File is the repo-relative path.
	File string
	// Line is the file line on which the reader command word starts.
	Line int
	// Reader is the reader command as written (grep, head, sed, awk, rg, ...).
	Reader string
	// Producer is the command word of the stage that feeds the reader.
	Producer string
	// Rewrite is the one-token rewrite for this reader and dialect.
	Rewrite string
}

// shellInterpreters maps the interpreter named on a #! line to its dialect.
// Anything else (python, zsh, node, ...) is not a source this guard checks.
var shellInterpreters = map[string]shellDialect{
	"bash": dialectBash,
	"sh":   dialectPOSIX,
	"dash": dialectPOSIX,
	"ash":  dialectPOSIX,
	"ksh":  dialectPOSIX,
}

// shebangInterpreter returns the interpreter a first line names, directly or
// through env, or "" when the line is not a #! line.
func shebangInterpreter(text string) string {
	if !strings.HasPrefix(text, "#!") {
		return ""
	}
	line, _, _ := strings.Cut(text, "\n")
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	for i := 0; i < len(fields); i++ {
		base := path.Base(fields[i])
		if i == 0 && base != "env" {
			return base
		}
		if i > 0 && !strings.HasPrefix(fields[i], "-") && !strings.Contains(fields[i], "=") {
			return base
		}
	}
	return ""
}

// newShellSource decides whether one tracked file is a shell source and, if
// so, which dialect. Discovery by content: a first line `#!` naming sh, bash,
// dash, ash or ksh (directly or through env), or a *.sh/*.bash name (no
// shebang means bash). ok is false for any other file.
func newShellSource(file string, data []byte) (src shellSource, ok bool) {
	text := string(data)
	if interp := shebangInterpreter(text); interp != "" {
		dialect, known := shellInterpreters[interp]
		if !known {
			return shellSource{}, false
		}
		return shellSource{Path: file, Dialect: dialect, Text: text}, true
	}
	if strings.HasSuffix(file, ".sh") || strings.HasSuffix(file, ".bash") {
		return shellSource{Path: file, Dialect: dialectBash, Text: text}, true
	}
	return shellSource{}, false
}

// githubExpression matches a ${{ ... }} expression, which is not shell.
var githubExpression = regexp.MustCompile(`(?s)\$\{\{.*?\}\}`)

// workflowRunSources returns one source per run: block of a workflow file,
// with ${{ ... }} replaced by a placeholder so the block parses, and with the
// line offset and the step's shell recorded so findings name the YAML line.
func workflowRunSources(file string, data []byte) ([]shellSource, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: parsing workflow YAML: %w", file, err)
	}
	var out []shellSource
	var walk func(n *yaml.Node, inherited string)
	walk = func(n *yaml.Node, inherited string) {
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c, inherited)
			}
		case yaml.MappingNode:
			shell := inherited
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "defaults" {
					if v := workflowDefaultShell(n.Content[i+1]); v != "" {
						shell = v
					}
				}
			}
			stepShell := shell
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "shell" && n.Content[i+1].Kind == yaml.ScalarNode {
					stepShell = n.Content[i+1].Value
				}
			}
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, val := n.Content[i], n.Content[i+1]
				if key.Value == "run" && val.Kind == yaml.ScalarNode {
					if src, ok := workflowRunSource(file, val, stepShell); ok {
						out = append(out, src)
					}
					continue
				}
				walk(val, shell)
			}
		}
	}
	walk(&doc, "")
	return out, nil
}

// workflowDefaultShell returns defaults.run.shell from a defaults: mapping.
func workflowDefaultShell(defaults *yaml.Node) string {
	if defaults.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(defaults.Content); i += 2 {
		run := defaults.Content[i+1]
		if defaults.Content[i].Value != "run" || run.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(run.Content); j += 2 {
			if run.Content[j].Value == "shell" {
				return run.Content[j+1].Value
			}
		}
	}
	return ""
}

// workflowRunSource builds the source for one scalar run: value. ok is false
// when the step's shell is not a bash-family shell (pwsh, python, cmd, ...).
func workflowRunSource(file string, run *yaml.Node, shell string) (shellSource, bool) {
	if shell != "" && !strings.HasPrefix(shell, "bash") && !strings.HasPrefix(shell, "sh") {
		return shellSource{}, false
	}
	text := githubExpression.ReplaceAllStringFunc(run.Value, func(m string) string {
		return "GHEXPR" + strings.Repeat("\n", strings.Count(m, "\n"))
	})
	// A block scalar's body starts on the line after the `|` or `>` indicator;
	// a plain or quoted scalar starts on the line of the run: key itself.
	offset := run.Line - 1
	if run.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		offset = run.Line
	}
	return shellSource{
		Path: file, Dialect: dialectBash, Text: text,
		LineOffset: offset, Workflow: true, WorkflowShell: shell,
	}, true
}

// argUnknown stands in for any part of a word whose value is not a literal.
const argUnknown = "\x00"

// staticWord is a word's literal text, with argUnknown for every expansion.
func staticWord(w *syntax.Word) string {
	var sb strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			sb.WriteString(p.Value)
		case *syntax.SglQuoted:
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				if l, ok := q.(*syntax.Lit); ok {
					sb.WriteString(l.Value)
				} else {
					sb.WriteString(argUnknown)
				}
			}
		default:
			sb.WriteString(argUnknown)
		}
	}
	return sb.String()
}

// stageCommand is one pipeline stage reduced to the command it runs.
type stageCommand struct {
	// name is the command word with its directory and wrappers stripped, or ""
	// when the stage is not a simple command with a literal name.
	name string
	// args are the arguments after the command name.
	args []string
	// readsStdin is false when the stage redirects its own input.
	readsStdin bool
	// compound names a non-simple stage (subshell, loop, ...) for the report.
	compound string
}

// unwrapCommand strips command, builtin, exec, env and timeout wrappers.
func unwrapCommand(words []string) (name string, args []string) {
	i := 0
	for i < len(words) {
		w := words[i]
		if strings.Contains(w, argUnknown) {
			return "", nil
		}
		switch path.Base(w) {
		case "command", "builtin", "exec":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				if words[i] == "-v" || words[i] == "-V" {
					return "", nil
				}
				i++
			}
		case "env":
			i++
			for i < len(words) {
				switch {
				case words[i] == "-u" || words[i] == "-C" || words[i] == "-S":
					i += 2
				case strings.HasPrefix(words[i], "-") || strings.Contains(words[i], "="):
					i++
				default:
					goto next
				}
			}
		case "timeout":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				if words[i] == "-s" || words[i] == "-k" {
					i++
				}
				i++
			}
			i++ // the duration
		default:
			return path.Base(w), words[i+1:]
		}
	next:
	}
	return "", nil
}

// stageOf reduces a pipeline stage to the command it runs.
func stageOf(stmt *syntax.Stmt) stageCommand {
	st := stageCommand{readsStdin: true}
	for _, r := range stmt.Redirs {
		switch r.Op {
		case syntax.RdrIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc, syntax.RdrInOut:
			if r.N == nil || r.N.Value == "0" {
				st.readsStdin = false
			}
		}
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok {
		switch stmt.Cmd.(type) {
		case *syntax.Subshell:
			st.compound = "subshell"
		case *syntax.Block:
			st.compound = "command group"
		default:
			st.compound = "compound command"
		}
		return st
	}
	words := make([]string, len(call.Args))
	for i, w := range call.Args {
		words[i] = staticWord(w)
	}
	st.name, st.args = unwrapCommand(words)
	return st
}

// optionCluster walks the leading-dash arguments of a command, calling short
// for each short-option character and long for each --name[=value]. A short or
// long option that takes a value consumes it from the cluster tail or the next
// argument and passes it to the callback. It returns the positional arguments.
type optionSpec struct {
	// shortValue lists the short options that take a value.
	shortValue string
	// longValue lists the long options that take a value.
	longValue []string
	// shortStop lists short options after which the rest of the cluster is not
	// an option cluster (sed -i takes an optional attached suffix).
	shortStop string
}

func scanOptions(args []string, spec optionSpec, short func(c byte, val string), long func(name, val string)) (positionals []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(positionals, args[i+1:]...)
		case strings.HasPrefix(a, "--"):
			name, val, has := strings.Cut(a[2:], "=")
			takes := false
			for _, l := range spec.longValue {
				if l == name {
					takes = true
				}
			}
			if takes && !has && i+1 < len(args) {
				i++
				val = args[i]
			}
			long(name, val)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j := 1; j < len(a); j++ {
				c := a[j]
				if strings.IndexByte(spec.shortValue, c) >= 0 {
					val := a[j+1:]
					if val == "" && i+1 < len(args) {
						i++
						val = args[i]
					}
					short(c, val)
					break
				}
				short(c, "")
				if strings.IndexByte(spec.shortStop, c) >= 0 {
					break
				}
			}
		default:
			positionals = append(positionals, a)
		}
	}
	return positionals
}

// readsOnlyStdin reports whether the positional arguments name no input file
// ("-" is stdin).
func readsOnlyStdin(files []string) bool {
	for _, f := range files {
		if f != "-" {
			return false
		}
	}
	return true
}

var grepFamily = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ugrep": true, "ag": true}

// grepExitsEarly reports whether grep stops reading before end of input:
// -q/--quiet/--silent, -m/--max-count, -l/--files-with-matches.
func grepExitsEarly(args []string) bool {
	early, patternGiven := false, false
	spec := optionSpec{
		shortValue: "efmABCdD",
		longValue: []string{
			"regexp", "file", "max-count", "after-context", "before-context",
			"context", "devices", "directories", "exclude", "exclude-from", "exclude-dir",
			"include", "label", "binary-files", "group-separator",
		},
	}
	positionals := scanOptions(args, spec,
		func(c byte, _ string) {
			switch c {
			case 'q', 'l', 'm':
				early = true
			case 'e', 'f':
				patternGiven = true
			}
		},
		func(name, _ string) {
			switch name {
			case "quiet", "silent", "files-with-matches", "max-count":
				early = true
			case "regexp", "file":
				patternGiven = true
			}
		})
	if !patternGiven && len(positionals) > 0 {
		positionals = positionals[1:]
	}
	return early && readsOnlyStdin(positionals)
}

// headExitsEarly reports whether head stops before end of input: every form
// except a negative count (-n -N, -c -N), which drops the tail and reads all.
func headExitsEarly(args []string) bool {
	negative := false
	check := func(val string) {
		if strings.HasPrefix(val, "-") {
			negative = true
		}
	}
	positionals := scanOptions(args, optionSpec{shortValue: "nc", longValue: []string{"lines", "bytes"}},
		func(c byte, val string) {
			if c == 'n' || c == 'c' {
				check(val)
			}
		},
		func(name, val string) {
			if name == "lines" || name == "bytes" {
				check(val)
			}
		})
	return !negative && readsOnlyStdin(positionals)
}

// sedExitsEarly reports whether a sed script contains a q or Q command.
func sedExitsEarly(args []string) bool {
	var scripts []string
	haveFile := false
	positionals := scanOptions(args, optionSpec{shortValue: "efl", longValue: []string{"expression", "file", "line-length"}, shortStop: "i"},
		func(c byte, val string) {
			switch c {
			case 'e':
				scripts = append(scripts, val)
			case 'f':
				haveFile = true
			}
		},
		func(name, val string) {
			switch name {
			case "expression":
				scripts = append(scripts, val)
			case "file":
				haveFile = true
			}
		})
	if len(scripts) == 0 && !haveFile && len(positionals) > 0 {
		scripts = []string{positionals[0]}
		positionals = positionals[1:]
	}
	if len(scripts) == 0 || !readsOnlyStdin(positionals) {
		return false
	}
	return sedScriptQuits(strings.Join(scripts, "\n"))
}

// sedScriptQuits scans a sed script command by command, so a q inside a
// regex, a replacement or a y/// table is not mistaken for the quit command.
func sedScriptQuits(s string) bool {
	i, n := 0, len(s)
	skipSpaces := func() {
		for i < n && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	skipLine := func() {
		for i < n && s[i] != '\n' {
			i++
		}
	}
	// skipDelim consumes up to and including the next unescaped delimiter.
	skipDelim := func(d byte) {
		for i < n {
			c := s[i]
			i++
			switch c {
			case '\\':
				i++
			case d:
				return
			}
		}
	}
	isAddrChar := func(c byte) bool { return c >= '0' && c <= '9' || c == '$' || c == argUnknown[0] }
	parseAddr := func() bool {
		if i >= n {
			return false
		}
		switch c := s[i]; {
		case isAddrChar(c):
			for i < n && isAddrChar(s[i]) {
				i++
			}
			if i < n && s[i] == '~' {
				i++
				for i < n && isAddrChar(s[i]) {
					i++
				}
			}
		case c == '/':
			i++
			skipDelim('/')
			for i < n && (s[i] == 'I' || s[i] == 'M') {
				i++
			}
		case c == '\\' && i+1 < n:
			d := s[i+1]
			i += 2
			skipDelim(d)
			for i < n && (s[i] == 'I' || s[i] == 'M') {
				i++
			}
		default:
			return false
		}
		return true
	}
	for i < n {
		switch s[i] {
		case ' ', '\t', '\n', ';', '{', '}':
			i++
			continue
		case '#':
			skipLine()
			continue
		}
		if parseAddr() {
			skipSpaces()
			if i < n && s[i] == ',' {
				i++
				skipSpaces()
				if i < n && (s[i] == '+' || s[i] == '~') {
					i++
				}
				parseAddr()
			}
		}
		skipSpaces()
		for i < n && s[i] == '!' {
			i++
			skipSpaces()
		}
		if i >= n {
			break
		}
		cmd := s[i]
		i++
		switch cmd {
		case 'q', 'Q':
			return true
		case 's', 'y':
			if i < n {
				d := s[i]
				i++
				skipDelim(d)
				skipDelim(d)
			}
			for i < n && s[i] != ';' && s[i] != '}' && s[i] != '\n' {
				if s[i] == 'w' {
					skipLine()
					break
				}
				i++
			}
		case 'a', 'i', 'c':
			for i < n {
				skipLine()
				if i > 0 && s[i-1] == '\\' && i < n {
					i++
					continue
				}
				break
			}
		case 'r', 'R', 'w', 'W', 'e', '#':
			skipLine()
		case ':', 'b', 't', 'T':
			for i < n && s[i] != ';' && s[i] != '\n' && s[i] != '}' {
				i++
			}
		}
	}
	return false
}

var awkFamily = map[string]bool{"awk": true, "gawk": true, "mawk": true, "nawk": true}

// awkExitsEarly reports whether an awk program has an exit outside an END
// block. An exit inside END runs after all input is read.
func awkExitsEarly(args []string) bool {
	var programs []string
	haveFile := false
	positionals := scanOptions(args, optionSpec{shortValue: "Fvfe", longValue: []string{"field-separator", "assign", "file", "source"}},
		func(c byte, val string) {
			switch c {
			case 'f':
				haveFile = true
			case 'e':
				programs = append(programs, val)
			}
		},
		func(name, val string) {
			switch name {
			case "file":
				haveFile = true
			case "source":
				programs = append(programs, val)
			}
		})
	if len(programs) == 0 && !haveFile && len(positionals) > 0 {
		programs = []string{positionals[0]}
		positionals = positionals[1:]
	}
	var files []string
	for _, p := range positionals {
		if name, _, ok := strings.Cut(p, "="); !ok || strings.ContainsAny(name, "/.") {
			files = append(files, p)
		}
	}
	if len(programs) == 0 || !readsOnlyStdin(files) {
		return false
	}
	return awkProgramExitsOutsideEnd(strings.Join(programs, "\n"))
}

// awkProgramExitsOutsideEnd tokenizes just enough awk (strings, regexes,
// comments, braces, identifiers) to find an exit statement outside END { }.
func awkProgramExitsOutsideEnd(prog string) bool {
	depth, endDepth := 0, -1 // endDepth is the depth an END block opened at, or -1
	pendingEnd := false
	prevOperand := false
	isIdent := func(c byte) bool {
		return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	for i := 0; i < len(prog); {
		c := prog[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '\n' || c == ';':
			prevOperand = false
			i++
		case c == '#':
			for i < len(prog) && prog[i] != '\n' {
				i++
			}
		case c == '"':
			i++
			for i < len(prog) && prog[i] != '"' {
				if prog[i] == '\\' {
					i++
				}
				i++
			}
			i++
			prevOperand, pendingEnd = true, false
		case c == '/' && !prevOperand:
			i++
			for i < len(prog) && prog[i] != '/' {
				if prog[i] == '\\' {
					i++
				}
				i++
			}
			i++
			prevOperand, pendingEnd = true, false
		case c == '{':
			if pendingEnd && endDepth < 0 {
				endDepth = depth
			}
			depth++
			prevOperand, pendingEnd = false, false
			i++
		case c == '}':
			depth--
			if endDepth >= 0 && depth <= endDepth {
				endDepth = -1
			}
			prevOperand, pendingEnd = false, false
			i++
		case isIdent(c):
			j := i
			for j < len(prog) && isIdent(prog[j]) {
				j++
			}
			word := prog[i:j]
			afterDollar := i > 0 && prog[i-1] == '$'
			if word == "exit" && !afterDollar && endDepth < 0 {
				return true
			}
			pendingEnd = word == "END"
			prevOperand = word != "print" && word != "printf" && word != "return" && word != "in"
			i = j
		case c == ')' || c == ']' || c == argUnknown[0]:
			prevOperand, pendingEnd = true, false
			i++
		default:
			prevOperand, pendingEnd = false, false
			i++
		}
	}
	return false
}

// earlyExitReader reports whether the command is a reader that can exit before
// its input ends.
func earlyExitReader(name string, args []string) bool {
	switch {
	case grepFamily[name]:
		return grepExitsEarly(args)
	case name == "head":
		return headExitsEarly(args)
	case name == "sed":
		return sedExitsEarly(args)
	case awkFamily[name]:
		return awkExitsEarly(args)
	}
	return false
}

// pipefailRewrite is the one-token rewrite recommended for a reader.
func pipefailRewrite(reader, producer string, args []string, dialect shellDialect) string {
	bash := dialect == dialectBash
	variableFed := producer == "printf" || producer == "echo"
	switch {
	case reader == "head":
		bytesMode := false
		for _, a := range args {
			if strings.HasPrefix(a, "-c") || strings.HasPrefix(a, "--bytes") {
				bytesMode = true
			}
		}
		switch {
		case bytesMode && bash:
			return "head -c N < <(" + producer + " ...) (the producer status is deliberately unobserved)"
		case bytesMode:
			return "write the producer to a temp file first, then head -c N < file"
		}
		return producer + " ... | sed -n '1,Np' (sed reads to the end of its input)"
	case grepFamily[reader] && variableFed && bash:
		return reader + ` ... <<<"$v" (a here-string has no producer process to SIGPIPE)`
	case grepFamily[reader]:
		for _, a := range args {
			if strings.HasPrefix(a, "-m") || strings.HasPrefix(a, "--max-count") {
				return producer + " ... | awk '!seen && /P/ { print; seen=1 }' (a program that reads to the end of its input)"
			}
		}
		if bash {
			return producer + " ... | " + reader + " P >/dev/null (drain: pipefail still reports a real " + producer + " failure), or capture first: out=$(" + producer + " ...) || handle; " + reader + ` -q P <<<"$out"`
		}
		return producer + " ... | " + reader + " P >/dev/null (drain: drop -q so the reader reads to the end of its input)"
	case variableFed && bash:
		return reader + ` ... <<<"$v" (a here-string has no producer process to SIGPIPE)`
	}
	return producer + " ... | awk '!seen && P { print $1; seen=1 }' (a program that reads to the end of its input)"
}

// parsedShell is one source with its syntax tree.
type parsedShell struct {
	src  shellSource
	file *syntax.File
}

// parseShellSource parses one source as bash. Bash is the most permissive
// dialect, so a POSIX script with a bashism still parses and is checked.
func parseShellSource(src shellSource) (*syntax.File, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src.Text), src.Path)
	if err != nil {
		var pe syntax.ParseError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("%s:%d: parsing shell: %s", src.Path, int(pe.Pos.Line())+src.LineOffset, pe.Text)
		}
		return nil, fmt.Errorf("%s: parsing shell: %w", src.Path, err)
	}
	return f, nil
}

// callWords returns the literal words of a simple command.
func callWords(call *syntax.CallExpr) []string {
	words := make([]string, len(call.Args))
	for i, w := range call.Args {
		words[i] = staticWord(w)
	}
	return words
}

// setsPipefail reports whether a `set` command turns pipefail on (sign '-') or
// off (sign '+'): `set -o pipefail`, or an option cluster ending in o and
// followed by pipefail (`set -eo pipefail`).
func setsPipefail(words []string, sign byte) bool {
	if len(words) < 3 || words[0] != "set" {
		return false
	}
	for i := 1; i+1 < len(words); i++ {
		w := words[i]
		if len(w) >= 2 && w[0] == sign && w[len(w)-1] == 'o' && words[i+1] == "pipefail" {
			return true
		}
	}
	return false
}

// sourcedPaths returns the literal trailing path of every `source X` / `. X`.
func sourcedPaths(f *syntax.File) []string {
	var out []string
	syntax.Walk(f, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		if name := staticWord(call.Args[0]); name != "source" && name != "." {
			return true
		}
		target := staticWord(call.Args[1])
		if i := strings.LastIndex(target, argUnknown); i >= 0 {
			target = target[i+1:]
		}
		target = strings.TrimLeft(strings.TrimPrefix(target, "./"), "/")
		if target != "" {
			out = append(out, target)
		}
		return true
	})
	return out
}

// pipefailOffRanges returns the byte ranges of subshells and command
// substitutions that turn pipefail off for the statements after the set.
func pipefailOffRanges(f *syntax.File) [][2]uint {
	var ranges [][2]uint
	scan := func(n syntax.Node, stmts []*syntax.Stmt) {
		for _, st := range stmts {
			if call, ok := st.Cmd.(*syntax.CallExpr); ok && setsPipefail(callWords(call), '+') {
				ranges = append(ranges, [2]uint{st.End().Offset(), n.End().Offset()})
				return
			}
		}
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Subshell:
			scan(n, n.Stmts)
		case *syntax.CmdSubst:
			scan(n, n.Stmts)
		}
		return true
	})
	return ranges
}

// pipeStages flattens a pipeline into its stages, left to right.
func pipeStages(stmt *syntax.Stmt, seen map[*syntax.BinaryCmd]bool) []*syntax.Stmt {
	bin, ok := stmt.Cmd.(*syntax.BinaryCmd)
	if !ok || (bin.Op != syntax.Pipe && bin.Op != syntax.PipeAll) {
		return []*syntax.Stmt{stmt}
	}
	seen[bin] = true
	return append(pipeStages(bin.X, seen), pipeStages(bin.Y, seen)...)
}

// pipelineFindings returns the early-exit readers in one parsed source.
func pipelineFindings(ps parsedShell) []pipefailFinding {
	var findings []pipefailFinding
	off := pipefailOffRanges(ps.file)
	seen := map[*syntax.BinaryCmd]bool{}
	syntax.Walk(ps.file, func(n syntax.Node) bool {
		bin, ok := n.(*syntax.BinaryCmd)
		if !ok || seen[bin] || (bin.Op != syntax.Pipe && bin.Op != syntax.PipeAll) {
			return true
		}
		stages := pipeStages(&syntax.Stmt{Cmd: bin}, seen)
		for i := 1; i < len(stages); i++ {
			reader := stageOf(stages[i])
			if reader.name == "" || !reader.readsStdin || !earlyExitReader(reader.name, reader.args) {
				continue
			}
			offset := stages[i].Pos().Offset()
			silenced := false
			for _, r := range off {
				if offset >= r[0] && offset < r[1] {
					silenced = true
				}
			}
			if silenced {
				continue
			}
			producer := stageOf(stages[i-1])
			producerName := producer.name
			if producerName == "" {
				producerName = producer.compound
			}
			if producerName == "" {
				producerName = "the producer"
			}
			findings = append(findings, pipefailFinding{
				File:     ps.src.Path,
				Line:     int(stages[i].Pos().Line()) + ps.src.LineOffset,
				Reader:   reader.name,
				Producer: producerName,
				Rewrite:  pipefailRewrite(reader.name, producerName, reader.args, ps.src.Dialect),
			})
		}
		return true
	})
	return findings
}

// initiallyInScope applies the scope rule that needs only the source itself.
func initiallyInScope(ps parsedShell) bool {
	sets := false
	syntax.Walk(ps.file, func(n syntax.Node) bool {
		if call, ok := n.(*syntax.CallExpr); ok && setsPipefail(callWords(call), '-') {
			sets = true
		}
		return true
	})
	if ps.src.Workflow {
		// GitHub runs an explicit `shell: bash` as `bash -eo pipefail {0}`; an
		// unspecified shell is `bash -e {0}`, which has no pipefail.
		return ps.src.WorkflowShell == "bash" || strings.Contains(ps.src.WorkflowShell, "pipefail") || sets
	}
	// Bash is always checked: a library inherits its sourcer's options, and
	// removing pipefail must never silence the guard.
	return ps.src.Dialect == dialectBash || sets
}

// checkShellSources applies the scope rule across the whole source set (a
// POSIX file sourced from an in-scope file is itself in scope, so scope needs
// every source at once) and returns every finding sorted by file then line.
// A source that fails to parse is an error naming the file and line; it is
// never skipped.
func checkShellSources(sources []shellSource) ([]pipefailFinding, error) {
	parsed := make([]parsedShell, 0, len(sources))
	var parseErrs []error
	for _, src := range sources {
		f, err := parseShellSource(src)
		if err != nil {
			parseErrs = append(parseErrs, err)
			continue
		}
		parsed = append(parsed, parsedShell{src: src, file: f})
	}
	if len(parseErrs) > 0 {
		return nil, errors.Join(parseErrs...)
	}

	inScope := make([]bool, len(parsed))
	var queue []int
	for i, ps := range parsed {
		if initiallyInScope(ps) {
			inScope[i] = true
			queue = append(queue, i)
		}
	}
	// A POSIX source that an in-scope source sources, however indirectly, runs
	// with that source's options. Resolved by the literal trailing path of the
	// source command's argument.
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, target := range sourcedPaths(parsed[cur].file) {
			for j, other := range parsed {
				if inScope[j] || other.src.Workflow {
					continue
				}
				if other.src.Path == target || strings.HasSuffix(other.src.Path, "/"+target) {
					inScope[j] = true
					queue = append(queue, j)
				}
			}
		}
	}

	var findings []pipefailFinding
	for i, ps := range parsed {
		if inScope[i] {
			findings = append(findings, pipelineFindings(ps)...)
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

// isWorkflowFile reports whether rel is a GitHub Actions workflow.
func isWorkflowFile(rel string) bool {
	return strings.HasPrefix(rel, ".github/workflows/") &&
		(strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml"))
}

// loadShellSources reads one repo file and returns the shell sources in it. It
// reads the whole file only when its name or its first two bytes say shell.
func loadShellSources(root, rel string) ([]shellSource, error) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // tracked but deleted in the work tree
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	shellName := strings.HasSuffix(rel, ".sh") || strings.HasSuffix(rel, ".bash")
	if !shellName && !isWorkflowFile(rel) {
		f, err := os.Open(abs)
		if err != nil {
			return nil, err
		}
		head := make([]byte, 2)
		n, _ := f.Read(head)
		_ = f.Close()
		if !bytes.Equal(head[:n], []byte("#!")) {
			return nil, nil
		}
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if isWorkflowFile(rel) {
		return workflowRunSources(rel, data)
	}
	if src, ok := newShellSource(rel, data); ok {
		return []shellSource{src}, nil
	}
	return nil, nil
}

// discoverShellSources returns every shell source under root: the tracked files
// from `git ls-files -z` when root is a work tree, otherwise a walk of the
// tree with the residency prune rules. Both must give the same set.
func discoverShellSources(root string) ([]shellSource, error) {
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		return gitShellSources(root)
	}
	return walkShellSources(root)
}

// walkShellSources is the non-git discovery: a walk of root that prunes the
// directory names residencyPruned prunes.
func walkShellSources(root string) ([]shellSource, error) {
	var out []shellSource
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if p != root && (entry.Name() == ".git" || residencyPruned(entry.Name())) {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		found, err := loadShellSources(root, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		out = append(out, found...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s for shell sources: %w", root, err)
	}
	return out, nil
}

// gitShellSources is the git discovery: git ls-files -z under root.
func gitShellSources(root string) ([]shellSource, error) {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR":
			continue
		}
		env = append(env, entry)
	}
	listing, err := testutil.RunGitWithEnv(root, env, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w: %s", root, err, listing)
	}
	var out []shellSource
	for _, rel := range strings.Split(string(listing), "\x00") {
		if rel == "" {
			continue
		}
		found, err := loadShellSources(root, rel)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// formatPipefailFinding renders the one report line for a finding:
// file:line: <reader> can exit before its input ends; <producer> may die of
// SIGPIPE (141) under pipefail and turn a match into a miss. Rewrite: ... See
// engdocs/contributors/shell-pipefail-early-exit.md.
func formatPipefailFinding(f pipefailFinding) string {
	return fmt.Sprintf("%s:%d: %s can exit before its input ends; %s may die of SIGPIPE (141) under pipefail and turn a match into a miss. Rewrite: %s. See %s",
		f.File, f.Line, f.Reader, f.Producer, f.Rewrite, pipefailGuardDoc)
}

// pipefailGuardDoc is the contributor document every finding points at.
const pipefailGuardDoc = "engdocs/contributors/shell-pipefail-early-exit.md"

// pipefailSourceFloor is the shrink alarm: roughly 75% of the 395 sources the
// guard parsed when it was written. A scan set that quietly shrinks below it
// means the guard is checking less than it claims.
const pipefailSourceFloor = 296

// pipefailCanaryDirs each need at least one discovered source, so a scan that
// loses a whole area (a Bazel data gap, a moved directory, a broken prune)
// fails loudly instead of passing on what is left.
var pipefailCanaryDirs = []string{
	".githooks/",
	".github/workflows/",
	"internal/bootstrap/packs/core/",
	"scripts/",
	"contrib/",
	"examples/",
	"schemas/",
	"test/agents/",
	"tools/rbe/",
}

// TestShellNoEarlyExitReaderUnderPipefail is the whole-repo guard. It lists
// EVERY finding, sorted, then fails once.
func TestShellNoEarlyExitReaderUnderPipefail(t *testing.T) {
	root := repoRoot(t)
	sources, err := discoverShellSources(root)
	if err != nil {
		t.Fatalf("discovering shell sources under %s: %v", root, err)
	}

	if len(sources) < pipefailSourceFloor {
		t.Errorf("scan set shrank: %d shell sources, floor %d", len(sources), pipefailSourceFloor)
	}
	for _, dir := range pipefailCanaryDirs {
		found := false
		for _, src := range sources {
			if strings.HasPrefix(src.Path, dir) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("canary: no shell source discovered under %s", dir)
		}
	}

	findings, err := checkShellSources(sources)
	if err != nil {
		t.Fatalf("analyzing shell sources: %v", err)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	if len(findings) == 0 {
		return
	}
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		lines = append(lines, formatPipefailFinding(f))
	}
	t.Errorf("%d early-exit reader(s) at the end of a pipe under pipefail:\n%s",
		len(findings), strings.Join(lines, "\n"))
}

// lineOfNeedle returns the 1-based line of text containing needle, failing the
// test when there is none or more than one, so a fixture expectation names its
// line by content instead of by a hand-counted number that rots.
func lineOfNeedle(t *testing.T, text, needle string) int {
	t.Helper()
	found := 0
	at := 0
	for i, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			found++
			at = i + 1
		}
	}
	if found != 1 {
		t.Fatalf("needle %q appears on %d lines of the fixture, want exactly 1:\n%s", needle, found, text)
	}
	return at
}

// describeFindings renders findings for a failure message.
func describeFindings(findings []pipefailFinding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, fmt.Sprintf("%s:%d %s<-%s", f.File, f.Line, f.Reader, f.Producer))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}
