package main

import (
	"bufio"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// binaryNameViolation records a single hardcoded binary-name reference.
type binaryNameViolation struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// binaryNameReport is the JSON-serializable output of gc lint binary-name.
type binaryNameReport struct {
	OK         bool                  `json:"ok"`
	Count      int                   `json:"count"`
	Violations []binaryNameViolation `json:"violations,omitempty"`
}

// knownSubcommands are gc subcommands that indicate a hardcoded binary
// reference when preceded by bare "gc ".
var knownSubcommands = []string{
	"bd", "mail", "session", "runtime", "sling", "events", "hook",
	"nudge", "workflow", "doctor", "agent", "agent-script", "convoy",
	"rig", "wait", "start", "stop", "status", "config", "register",
	"reload", "restart", "suspend", "shell", "skill", "version",
	"init", "prime", "prompt", "lint", "mcp", "order", "converge",
	"beads", "supervisor", "pack", "trace", "build-image",
	"dolt-state", "dolt-cleanup", "dolt-config", "handoff", "event",
	"analyze", "patrol", "api", "graph", "formula", "unregister",
	"import", "migrate", "cities", "completion", "resume",
}

// gcSubcmdPattern matches "gc <subcommand>" where subcommand is one of
// the known gc CLI subcommands.
var gcSubcmdPattern *regexp.Regexp

func init() {
	escaped := make([]string, len(knownSubcommands))
	for i, sc := range knownSubcommands {
		escaped[i] = regexp.QuoteMeta(sc)
	}
	gcSubcmdPattern = regexp.MustCompile(`\bgc\s+(` + strings.Join(escaped, "|") + `)(?:\s|$)`)
}

func newLintBinaryNameCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		jsonOut    bool
		typeFilter string
		fix        bool
	)
	cmd := &cobra.Command{
		Use:   "binary-name [directory]",
		Short: "Scan for hardcoded binary name references",
		Long: strings.TrimSpace(`Scan a directory tree for hardcoded "gc" binary name references that should
use configurable alternatives (prog()/cmdName()/cmdErr() in Go, {{binary}} in
formula TOML, {{ cmd }} in prompt templates, ${GC_BIN:-gc} in shell).

Use --fix to automatically rewrite violations to their configurable form.`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			if fix {
				return exitForCode(doFixBinaryName(dir, typeFilter, stdout, stderr))
			}
			return exitForCode(doLintBinaryName(dir, jsonOut, typeFilter, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON output")
	cmd.Flags().StringVar(&typeFilter, "type", "", "filter to file type: go, toml, template, sh")
	cmd.Flags().BoolVar(&fix, "fix", false, "automatically rewrite violations to configurable form")
	return cmd
}

func doLintBinaryName(dir string, jsonOut bool, typeFilter string, stdout, stderr io.Writer) int {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName("lint binary-name"), err) //nolint:errcheck
		return 1
	}

	violations, err := scanBinaryNameViolations(absDir, typeFilter)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName("lint binary-name"), err) //nolint:errcheck
		return 1
	}

	report := binaryNameReport{
		OK:         len(violations) == 0,
		Count:      len(violations),
		Violations: violations,
	}

	if jsonOut {
		if err := writeCLIJSONLine(stdout, report); err != nil {
			fmt.Fprintf(stderr, "%s: encode JSON: %v\n", cmdName("lint binary-name"), err) //nolint:errcheck
			return 1
		}
	} else {
		for _, v := range violations {
			fmt.Fprintf(stdout, "%s:%d: %s\n", v.File, v.Line, v.Text) //nolint:errcheck
		}
		if len(violations) == 0 {
			fmt.Fprintf(stdout, "%s: %s: ok\n", cmdName("lint binary-name"), absDir) //nolint:errcheck
		} else {
			fmt.Fprintf(stderr, "%s: found %d violation(s)\n", cmdName("lint binary-name"), len(violations)) //nolint:errcheck
		}
	}

	if len(violations) > 0 {
		return 1
	}
	return 0
}

func scanBinaryNameViolations(root, typeFilter string) ([]binaryNameViolation, error) {
	var violations []binaryNameViolation

	err := filepath.WalkDir(root, func(path string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && lintBinaryNameSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		name := entry.Name()
		fileType := classifyFileType(name)
		if fileType == "" {
			return nil
		}
		if typeFilter != "" && fileType != typeFilter {
			return nil
		}

		fileViolations, err := scanFileForBinaryName(path, fileType)
		if err != nil {
			return fmt.Errorf("scanning %s: %w", path, err)
		}
		violations = append(violations, fileViolations...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].File == violations[j].File {
			return violations[i].Line < violations[j].Line
		}
		return violations[i].File < violations[j].File
	})
	return violations, nil
}

func lintBinaryNameSkipDir(name string) bool {
	switch name {
	case ".git", ".gc", ".beads", "node_modules", "vendor", "testdata":
		return true
	default:
		return false
	}
}

// classifyFileType returns the file type key for scanning, or empty if the
// file should be skipped.
func classifyFileType(name string) string {
	switch {
	case strings.HasSuffix(name, "_test.go"):
		return "" // skip test files
	case strings.HasSuffix(name, ".go"):
		return "go"
	case strings.HasSuffix(name, ".toml"):
		return "toml"
	case strings.HasSuffix(name, ".template.md"):
		return "template"
	case strings.HasSuffix(name, ".sh"):
		return "sh"
	default:
		return ""
	}
}

func scanFileForBinaryName(path, fileType string) ([]binaryNameViolation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file scan

	var violations []binaryNameViolation
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		var hit bool
		switch fileType {
		case "go":
			hit = checkGoLine(line, trimmed, filepath.Base(path), lineNo)
		case "toml":
			hit = checkTOMLLine(line, trimmed)
		case "template":
			hit = checkTemplateLine(line, trimmed)
		case "sh":
			hit = checkShellLine(line, trimmed)
		}

		if hit {
			violations = append(violations, binaryNameViolation{
				File: path,
				Line: lineNo,
				Text: trimmed,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return violations, nil
}

// checkGoLine reuses the same logic as the existing binary_name_lint_test.go
// ratchet test: containsHardcodedGC for detection, isAllowlisted for exceptions.
func checkGoLine(line, trimmed, filename string, lineNo int) bool {
	if strings.HasPrefix(trimmed, "//") {
		return false
	}
	if !containsHardcodedGC(line) {
		return false
	}
	if isAllowlisted(line, filename, lineNo) {
		return false
	}
	return true
}

// checkTOMLLine detects "gc <subcommand>" in TOML formula files, allowing
// lines that use {{binary}} or ${GC_BIN:-gc} substitution, and comment lines.
func checkTOMLLine(line, trimmed string) bool {
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	// start_command and nudge are raw config fields, not template-rendered —
	// {{binary}} would not be substituted there.
	if strings.HasPrefix(trimmed, "start_command") || strings.HasPrefix(trimmed, "nudge") {
		return false
	}
	if !gcSubcmdPattern.MatchString(line) {
		return false
	}
	if strings.Contains(line, "{{binary}}") {
		return false
	}
	if strings.Contains(line, "${GC_BIN:-gc}") {
		return false
	}
	return true
}

// checkTemplateLine detects "gc <subcommand>" in prompt template files,
// allowing lines with {{ cmd }} / {{cmd}} template actions.
func checkTemplateLine(line, trimmed string) bool {
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	if !gcSubcmdPattern.MatchString(line) {
		return false
	}
	if strings.Contains(line, "{{ cmd }}") || strings.Contains(line, "{{cmd}}") {
		return false
	}
	// Allow lines inside Go template actions (e.g. {{ ... gc ... }})
	if strings.Contains(line, "{{") && strings.Contains(line, "}}") {
		// If the gc reference is inside a template action, allow it.
		// Simple heuristic: if there's a {{ before the "gc " and a }} after.
		gcIdx := strings.Index(line, "gc ")
		openIdx := strings.Index(line, "{{")
		closeIdx := strings.LastIndex(line, "}}")
		if openIdx < gcIdx && closeIdx > gcIdx {
			return false
		}
	}
	if strings.Contains(line, "{{binary}}") {
		return false
	}
	return true
}

// checkShellLine detects bare "gc <subcommand>" in shell scripts, allowing
// ${GC_BIN:-gc} variable usage and comment lines.
func checkShellLine(line, trimmed string) bool {
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	if !gcSubcmdPattern.MatchString(line) {
		return false
	}
	if strings.Contains(line, "${GC_BIN:-gc}") {
		return false
	}
	return true
}

// containsHardcodedGC checks whether a Go source line contains patterns
// indicating a hardcoded "gc" binary name reference. Shared between the
// ratchet test (binary_name_lint_test.go) and the gc lint binary-name
// subcommand.
func containsHardcodedGC(line string) bool {
	// "gc " — gc followed by space (command reference in strings)
	if strings.Contains(line, `"gc `) {
		return true
	}
	// Strings starting with "gc: (error prefix)
	if strings.Contains(line, `"gc:`) {
		return true
	}
	// Short/Long field with "gc" as binary name mid-string
	if (strings.Contains(line, "Short:") || strings.Contains(line, "Long:")) && strings.Contains(line, " gc ") {
		return true
	}
	return false
}

// isAllowlisted returns true if the Go source line matches a known-safe
// pattern that is NOT a hardcoded binary name reference. Shared between
// the ratchet test and the gc lint binary-name subcommand.
func isAllowlisted(line, filename string, _ int) bool {
	// .gc/ runtime directory references
	if strings.Contains(line, `".gc`) {
		return true
	}
	// gc- bead ID prefixes
	if strings.Contains(line, `"gc-`) {
		return true
	}
	// GC_ environment variable prefix
	if strings.Contains(line, `"GC_`) {
		return true
	}
	// Import paths and module names
	if strings.Contains(line, `"gascity`) || strings.Contains(line, `"gastownhall`) {
		return true
	}
	if strings.Contains(line, `gascity`) || strings.Contains(line, `gastownhall`) {
		return true
	}
	// The binaryName default in progname.go itself
	if filename == "progname.go" && strings.Contains(line, `binaryName`) {
		return true
	}
	// gc.test — test binary detection
	if strings.Contains(line, `"gc.test`) {
		return true
	}
	// gc: durable label namespace — bead labels like "gc:session",
	// "gc:extmsg-slack" must remain constant regardless of binary name
	// so renamed binaries can still discover existing beads.
	if strings.Contains(line, `"gc:`) {
		return true
	}
	// The lint/fix tool itself references "gc " in detection logic and
	// regex patterns — those are infrastructure, not binary name refs.
	if filename == "cmd_lint_binary_name.go" {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// doFixBinaryName — auto-rewrite hardcoded binary name references
// ---------------------------------------------------------------------------

// doFixBinaryName walks the directory tree and rewrites hardcoded "gc <subcmd>"
// references to their configurable form, in place.
func doFixBinaryName(dir, typeFilter string, stdout, stderr io.Writer) int {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName("lint binary-name --fix"), err) //nolint:errcheck
		return 1
	}

	var totalFixed int
	var filesFixed int

	walkErr := filepath.WalkDir(absDir, func(path string, entry iofs.DirEntry, wErr error) error {
		if wErr != nil {
			return wErr
		}
		if entry.IsDir() {
			if path != absDir && lintBinaryNameSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		name := entry.Name()
		fileType := classifyFileType(name)
		if fileType == "" {
			return nil
		}
		if typeFilter != "" && fileType != typeFilter {
			return nil
		}

		n, fixErr := fixFileInPlace(path, fileType)
		if fixErr != nil {
			return fmt.Errorf("fixing %s: %w", path, fixErr)
		}
		if n > 0 {
			totalFixed += n
			filesFixed++
			fmt.Fprintf(stdout, "fixed: %s (%d replacements)\n", path, n) //nolint:errcheck
		}
		return nil
	})
	if walkErr != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName("lint binary-name --fix"), walkErr) //nolint:errcheck
		return 1
	}

	if totalFixed == 0 {
		fmt.Fprintf(stdout, "%s: no violations found\n", cmdName("lint binary-name --fix")) //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "%s: fixed %d violations in %d files\n", //nolint:errcheck
			cmdName("lint binary-name --fix"), totalFixed, filesFixed)
	}
	return 0
}

// fixFileInPlace reads the file, rewrites violation lines, and writes back
// atomically via temp-file + rename. Returns the number of replacements made.
func fixFileInPlace(path, fileType string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	lines := strings.Split(string(data), "\n")
	totalReplacements := 0
	filename := filepath.Base(path)

	// For Go files, detect whether cmdName is locally shadowed anywhere in
	// the file. If so, use prog()+" subcmd" instead of cmdName("subcmd").
	cmdNameShadowed := false
	if fileType == "go" {
		for _, l := range lines {
			trimmed := strings.TrimSpace(l)
			if strings.HasPrefix(trimmed, "const cmdName") ||
				strings.HasPrefix(trimmed, "var cmdName") ||
				strings.Contains(trimmed, "cmdName :=") {
				cmdNameShadowed = true
				break
			}
		}
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		var isViolation bool
		switch fileType {
		case "go":
			isViolation = checkGoLine(line, trimmed, filename, i+1)
		case "toml":
			isViolation = checkTOMLLine(line, trimmed)
		case "template":
			isViolation = checkTemplateLine(line, trimmed)
		case "sh":
			isViolation = checkShellLine(line, trimmed)
		}

		if !isViolation {
			continue
		}

		var fixed string
		var n int
		switch fileType {
		case "go":
			fixed, n = fixGoLine(line, cmdNameShadowed)
		case "toml":
			fixed, n = fixTOMLLine(line)
		case "template":
			fixed, n = fixTemplateLine(line)
		case "sh":
			fixed, n = fixShellLine(line)
		}

		if n > 0 {
			lines[i] = fixed
			totalReplacements += n
		}
	}

	if totalReplacements == 0 {
		return 0, nil
	}

	// Atomic write: temp file in same directory, then rename.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gc-fix-*")
	if err != nil {
		return 0, fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()

	content := strings.Join(lines, "\n")
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return 0, fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return 0, fmt.Errorf("closing temp file: %w", err)
	}

	// Preserve original file permissions.
	info, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}
	if err := os.Chmod(tmpName, info.Mode()); err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}

	return totalReplacements, nil
}

// ---------------------------------------------------------------------------
// Fix patterns for each file type
// ---------------------------------------------------------------------------

// goShortLongGCPattern matches bare "gc <subcmd>" in Short:/Long: cobra fields.
var goShortLongGCPattern *regexp.Regexp

// goGCStringLiteralPattern matches "gc <subcmd>" at the start of a
// double-quoted Go string literal. It captures:
//
//	Group 1: the full subcommand path (e.g. "stop", "agent add", "supervisor install")
//	Group 2: the separator and rest after the subcommand path (": ...", " ...", or empty)
//
// The match includes the opening quote but NOT the closing quote (the rest
// of the string continues after group 2).
var goGCStringLiteralPattern *regexp.Regexp

func init() {
	// Sort longest-first so "agent-script" matches before "agent" in the
	// regex alternation (leftmost-match semantics).
	sorted := make([]string, len(knownSubcommands))
	copy(sorted, knownSubcommands)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i]) > len(sorted[j])
	})
	escaped := make([]string, len(sorted))
	for i, sc := range sorted {
		escaped[i] = regexp.QuoteMeta(sc)
	}
	subcmdAlt := strings.Join(escaped, "|")

	// Bare "gc <subcmd>" in cobra Short/Long fields (may be in backtick strings).
	goShortLongGCPattern = regexp.MustCompile(
		`\bgc (` + subcmdAlt + `)\b`,
	)

	// Match "gc <subcmd> in a double-quoted string. The subcommand is the
	// first known word; additional hyphenated/dotted words are included as
	// part of the subcmd path (e.g. "agent-script", "dolt-state reset-probe",
	// "convoy control --serve").
	//
	// Group 1: subcmd path (e.g. "supervisor install")
	// The match ends right after the subcmd path. The caller inspects
	// what follows (colon, space, quote, plus) to decide the rewrite.
	goGCStringLiteralPattern = regexp.MustCompile(
		`"gc ((?:` + subcmdAlt + `)(?:[\s]+[\w./-]+(?:\s+[\w./-]+)*)?)`,
	)
}

// fixGoLine rewrites a single Go source line, replacing hardcoded "gc <subcmd>"
// patterns with cmdName()/prog() calls using string concatenation. Returns the
// fixed line and number of replacements.
//
// The key design choice: ALWAYS use string concatenation, never inject %s into
// format strings. This is safe because cmdName("agent")+": %v\n" evaluates to
// "gc agent: %v\n" at runtime — format verbs in the rest of the string work
// unchanged. This avoids the fragile and error-prone approach of finding
// format-string argument positions for %s injection.
func fixGoLine(line string, useProg bool) (string, int) {
	replacements := 0

	// --- Short:/Long: cobra fields (backtick or quoted strings) ---
	// These are display text, not executable, so use prog() directly.
	if strings.Contains(line, "Short:") || strings.Contains(line, "Long:") {
		if goShortLongGCPattern.MatchString(line) {
			line = goShortLongGCPattern.ReplaceAllStringFunc(line, func(match string) string {
				groups := goShortLongGCPattern.FindStringSubmatch(match)
				replacements++
				return prog() + " " + groups[1]
			})
			return line, replacements
		}
	}

	// Helper: generate replacement for "gc subcmd" based on whether cmdName
	// is locally shadowed. When shadowed, use prog()+" subcmd" to avoid
	// name collision with the local const. Both forms return a complete
	// expression (all quotes closed).
	cmdRepl := func(subcmd string) string {
		if useProg {
			return `prog()+" ` + subcmd + `"`
		}
		return `cmdName("` + subcmd + `")`
	}

	// --- All string literal contexts: always use string concatenation ---
	for {
		match := goGCStringLiteralPattern.FindStringSubmatchIndex(line)
		if match == nil {
			break
		}
		subcmd := line[match[2]:match[3]]
		afterIdx := match[1]
		rest := line[afterIdx:]

		var old, repl string
		switch {
		case strings.HasPrefix(rest, `:`):
			// "gc subcmd: rest..." or "gc subcmd:\n..." → cmdRepl+": rest..."
			old = `"gc ` + subcmd + `:`
			repl = cmdRepl(subcmd) + `+":`
		case strings.HasPrefix(rest, `"`):
			// "gc subcmd" — entire string is just the command name.
			old = `"gc ` + subcmd + `"`
			repl = cmdRepl(subcmd)
		case strings.HasPrefix(rest, `"+`), strings.HasPrefix(rest, `" +`):
			// "gc subcmd"+ concatenation.
			old = `"gc ` + subcmd + `"`
			repl = cmdRepl(subcmd)
		case strings.HasPrefix(rest, ` `):
			// "gc subcmd rest..." → cmdRepl+" rest..."
			old = `"gc ` + subcmd + ` `
			repl = cmdRepl(subcmd) + `+" `
		}

		if old == "" {
			break
		}
		idx := strings.Index(line, old)
		if idx < 0 {
			break
		}
		line = line[:idx] + repl + line[idx+len(old):]
		replacements++
	}

	// --- Fallback for bare "gc:" error prefix (no known subcommand) ---
	if replacements == 0 && strings.Contains(line, `"gc:`) {
		old := `"gc:`
		repl := `"+prog()+":`
		if strings.Contains(line, old) {
			line = strings.Replace(line, old, repl, 1)
			replacements++
		}
	}

	return line, replacements
}

// fixTOMLLine replaces "gc <subcmd>" with "{{binary}} <subcmd>" in TOML lines.
func fixTOMLLine(line string) (string, int) {
	n := 0
	fixed := gcSubcmdPattern.ReplaceAllStringFunc(line, func(match string) string {
		groups := gcSubcmdPattern.FindStringSubmatch(match)
		n++
		// Preserve trailing character (space or end-of-string).
		suffix := ""
		if len(match) > 0 {
			last := match[len(match)-1]
			if last == ' ' || last == '\t' || last == '\n' {
				suffix = string(last)
			}
		}
		return "{{binary}} " + groups[1] + suffix
	})
	return fixed, n
}

// fixTemplateLine replaces "gc <subcmd>" with "{{ cmd }} <subcmd>" in template lines.
func fixTemplateLine(line string) (string, int) {
	n := 0
	fixed := gcSubcmdPattern.ReplaceAllStringFunc(line, func(match string) string {
		groups := gcSubcmdPattern.FindStringSubmatch(match)
		n++
		suffix := ""
		if len(match) > 0 {
			last := match[len(match)-1]
			if last == ' ' || last == '\t' || last == '\n' {
				suffix = string(last)
			}
		}
		return "{{ cmd }} " + groups[1] + suffix
	})
	return fixed, n
}

// fixShellLine replaces "gc <subcmd>" with "${GC_BIN:-gc} <subcmd>" in shell lines.
func fixShellLine(line string) (string, int) {
	n := 0
	fixed := gcSubcmdPattern.ReplaceAllStringFunc(line, func(match string) string {
		groups := gcSubcmdPattern.FindStringSubmatch(match)
		n++
		suffix := ""
		if len(match) > 0 {
			last := match[len(match)-1]
			if last == ' ' || last == '\t' || last == '\n' {
				suffix = string(last)
			}
		}
		return "${GC_BIN:-gc} " + groups[1] + suffix
	})
	return fixed, n
}
