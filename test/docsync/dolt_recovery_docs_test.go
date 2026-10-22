package docsync

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// doltRecoveryDocs are the operator pages that tell someone how to recover a
// managed Dolt store. The data dir they act on is shared by every database of
// the city, so a step that replaces the whole dir loses the databases it does
// not name (#1979).
var doltRecoveryDocs = []string{
	"examples/bd/dolt/docs/journal-corruption-recovery.md",
	"engdocs/contributors/dolt-maintenance.md",
}

var (
	shellFenceRE   = regexp.MustCompile("^```(bash|sh|shell|zsh|console)\\s*$")
	gcDoltVerbRE   = regexp.MustCompile(`\bgc dolt ([a-z][a-z0-9-]*)`)
	shellCommandRE = regexp.MustCompile(`&&|\|\||[;|]`)
)

// shellLine is one line of a fenced shell block, with its 1-based line number.
type shellLine struct {
	n    int
	text string
}

// fencedShellLines returns the lines inside fenced shell blocks of a Markdown
// page. Comment lines are returned without their leading '#', so a commented
// command counts like a live one.
func fencedShellLines(content string) []shellLine {
	var lines []shellLine
	inShell, inFence := false, false
	for i, raw := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "```") {
			if inFence {
				inFence, inShell = false, false
			} else {
				inFence, inShell = true, shellFenceRE.MatchString(trimmed)
			}
			continue
		}
		if inShell {
			lines = append(lines, shellLine{n: i + 1, text: strings.TrimSpace(strings.TrimLeft(trimmed, "#"))})
		}
	}
	return lines
}

// isDataDirRoot reports whether a shell operand names the managed Dolt data
// dir itself.
func isDataDirRoot(operand string) bool {
	op := strings.TrimRight(operand, "/")
	switch op {
	case "dolt", ".beads/dolt", "<data_dir>", "$DATA_DIR", "${DATA_DIR}":
		return true
	}
	return strings.HasSuffix(op, "/.beads/dolt")
}

// isUnderDataDir reports whether a shell operand names a path inside the
// managed Dolt data dir.
func isUnderDataDir(operand string) bool {
	for _, prefix := range []string{"dolt/", ".beads/dolt/", "<data_dir>/", "$DATA_DIR/", "${DATA_DIR}/"} {
		if strings.HasPrefix(operand, prefix) {
			return true
		}
	}
	return strings.Contains(operand, "/.beads/dolt/")
}

// dataDirViolation returns why a single shell command replaces or deletes
// managed Dolt data, or "" when it does not.
func dataDirViolation(command string) string {
	fields := strings.Fields(command)
	for len(fields) > 0 && (fields[0] == "sudo" || strings.Contains(fields[0], "=")) {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return ""
	}
	name, args := fields[0], fields[1:]
	if name == "dolt" {
		if len(args) == 0 || args[0] != "clone" {
			return ""
		}
		name, args = "dolt clone", args[1:]
	}
	recursive := false
	var operands []string
	for _, arg := range args {
		arg = strings.Trim(arg, `"'`)
		if strings.HasPrefix(arg, "-") {
			recursive = recursive || strings.ContainsAny(arg, "rRa")
			continue
		}
		operands = append(operands, arg)
	}
	for _, op := range operands {
		switch {
		case name == "rm" && (isDataDirRoot(op) || isUnderDataDir(op)):
			return "deletes " + op
		case (name == "mv" || name == "mkdir" || name == "dolt clone") && isDataDirRoot(op):
			return name + " on the data-dir root " + op
		case name == "cp" && recursive && isDataDirRoot(op):
			return "cp -r on the data-dir root " + op
		}
	}
	if name == "mv" && len(operands) > 1 && isUnderDataDir(operands[len(operands)-1]) {
		return "moves into the data dir " + operands[len(operands)-1]
	}
	return ""
}

func readDoltRecoveryDoc(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(data)
}

// TestDoltRecoveryDocsNeverReplaceTheDataDir fails when a recovery page tells
// the operator to delete a database store, or to move, delete, clone over or
// re-create the shared data dir. Recovery is per database: a store moves
// aside to a path outside the data dir and the siblings stay in place.
func TestDoltRecoveryDocsNeverReplaceTheDataDir(t *testing.T) {
	for _, rel := range doltRecoveryDocs {
		for _, line := range fencedShellLines(readDoltRecoveryDoc(t, rel)) {
			for _, command := range shellCommandRE.Split(line.text, -1) {
				if why := dataDirViolation(command); why != "" {
					t.Errorf("%s:%d: %s: %q", rel, line.n, why, line.text)
				}
			}
		}
	}
}

// TestDoltRecoveryDocsNameExistingDoltPackCommands fails when a recovery page
// names a `gc dolt <verb>` the dolt pack does not provide; gc rejects an
// unknown verb, so the step cannot be followed.
func TestDoltRecoveryDocsNameExistingDoltPackCommands(t *testing.T) {
	commandsDir := filepath.Join(repoRoot(), "examples", "bd", "dolt", "commands")
	for _, rel := range doltRecoveryDocs {
		content := readDoltRecoveryDoc(t, rel)
		for i, line := range strings.Split(content, "\n") {
			for _, m := range gcDoltVerbRE.FindAllStringSubmatch(line, -1) {
				_, err := os.Stat(filepath.Join(commandsDir, m[1], "command.toml"))
				if os.IsNotExist(err) {
					t.Errorf("%s:%d: gc dolt %s is not a dolt pack command: %q", rel, i+1, m[1], strings.TrimSpace(line))
					continue
				}
				if err != nil {
					t.Fatalf("checking dolt pack command %q: %v", m[1], err)
				}
			}
		}
	}
}
