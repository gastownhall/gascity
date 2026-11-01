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
	gcDoltVerbRE   = regexp.MustCompile(`\bgc dolt ([a-z][a-z0-9-]*)`)
	shellCommandRE = regexp.MustCompile(`&&|\|\||[;|]`)
)

// shellLine is one line of a fenced block, with its 1-based line number.
type shellLine struct {
	n    int
	text string
}

// fencedShellLines returns the lines inside every fenced block of a Markdown
// page, whatever its language: an operator copies from an untagged fence as
// readily as from a bash one. Comment lines are returned without their
// leading '#', so a commented command counts like a live one.
func fencedShellLines(content string) []shellLine {
	var lines []shellLine
	inFence := false
	for i, raw := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			lines = append(lines, shellLine{n: i + 1, text: strings.TrimSpace(strings.TrimLeft(trimmed, "#"))})
		}
	}
	return lines
}

// isDataDirRoot reports whether a shell operand names the managed Dolt data
// dir itself, in the layouts the recovery pages use: the .beads/dolt dir
// (bare `dolt` from inside .beads), the <data_dir> placeholder, $DATA_DIR,
// and the legacy .dolt-data dir.
func isDataDirRoot(operand string) bool {
	op := strings.TrimRight(operand, "/")
	switch op {
	case "dolt", ".beads/dolt", ".dolt-data", "<data_dir>", "$DATA_DIR", "${DATA_DIR}":
		return true
	}
	return strings.HasSuffix(op, "/.beads/dolt") || strings.HasSuffix(op, "/.dolt-data")
}

// isUnderDataDir reports whether a shell operand names a path inside the
// managed Dolt data dir, in the layouts isDataDirRoot recognizes.
func isUnderDataDir(operand string) bool {
	for _, prefix := range []string{"dolt/", ".beads/dolt/", ".dolt-data/", "<data_dir>/", "$DATA_DIR/", "${DATA_DIR}/"} {
		if strings.HasPrefix(operand, prefix) {
			return true
		}
	}
	return strings.Contains(operand, "/.beads/dolt/") || strings.Contains(operand, "/.dolt-data/")
}

// isDatabaseDir reports whether a shell operand names one database's own
// directory: a single name right under the data dir, in the layouts
// isDataDirRoot recognizes. A database name has no dot, so an entry such as
// <dbname>.old is not one.
func isDatabaseDir(operand string) bool {
	op := strings.TrimRight(operand, "/")
	i := strings.LastIndex(op, "/")
	if i < 0 {
		return false
	}
	name := op[i+1:]
	return name != "" && !strings.Contains(name, ".") && isDataDirRoot(op[:i])
}

// movesStoreIntoItsOwnDir reports whether a move puts one store from outside
// the data dir into its own database directory, as a restore's swap and the
// move back of a corrupt store do. Any other move into the data dir leaves an
// extra entry there or moves one database over another.
func movesStoreIntoItsOwnDir(sources []string, dest string) bool {
	if len(sources) != 1 || !isDatabaseDir(dest) {
		return false
	}
	return !isDataDirRoot(sources[0]) && !isUnderDataDir(sources[0])
}

// leadingShellWords are the words that can come before the command a shell
// segment runs: `if ! mv ...` and `elif mv ...` run mv like a bare line does.
var leadingShellWords = map[string]bool{
	"sudo": true, "if": true, "elif": true, "then": true, "else": true,
	"do": true, "while": true, "until": true, "!": true, "{": true,
}

// dataDirViolation returns why a single shell command replaces or deletes
// managed Dolt data, or "" when it does not.
func dataDirViolation(command string) string {
	fields := strings.Fields(command)
	for len(fields) > 0 && (leadingShellWords[fields[0]] || strings.Contains(fields[0], "=")) {
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
	if name == "mv" && len(operands) > 1 {
		dest, sources := operands[len(operands)-1], operands[:len(operands)-1]
		if isUnderDataDir(dest) && !movesStoreIntoItsOwnDir(sources, dest) {
			return "moves into the data dir " + dest
		}
	}
	return ""
}

// lineViolation returns why a line of a fenced block replaces or deletes
// managed Dolt data, or "" when none of the commands it runs does.
func lineViolation(line string) string {
	for _, command := range shellCommandRE.Split(line, -1) {
		if why := dataDirViolation(command); why != "" {
			return why
		}
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
			if why := lineViolation(line.text); why != "" {
				t.Errorf("%s:%d: %s: %q", rel, line.n, why, line.text)
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

// TestDataDirViolationRecognizesTheIncidentShapes pins what the page scan
// rejects, so the rule cannot pass vacuously: the whole-dir move and clone of
// #1979, deletes, and moves that land inside the data dir, in each data-dir
// layout the pages use and after the shell keywords of an if-block, while the
// per-database steps stay allowed, a store moved back into its own database
// directory among them.
func TestDataDirViolationRecognizesTheIncidentShapes(t *testing.T) {
	for _, tc := range []struct {
		command string
		bad     bool
	}{
		{command: "mv dolt dolt.broken-$(date -u +%Y%m%dT%H%M%SZ)", bad: true},
		{command: "dolt clone file://$(pwd)/dolt-backups/success/<timestamp> dolt", bad: true},
		{command: "rm -rf /path/to/city/.beads/dolt/<dbname>", bad: true},
		{command: "mv .dolt-data .dolt-data.old", bad: true},
		{command: "rm -rf .dolt-data/<dbname>", bad: true},
		{command: "mv <data_dir>/<dbname> <data_dir>/<dbname>.old", bad: true},
		{command: "mv <data_dir>/<dbname> <data_dir>/<other>", bad: true},
		{command: "mv <somewhere-safe> <data_dir>/<dbname> <data_dir>/<other>", bad: true},
		{command: "mkdir -p <data_dir>", bad: true},
		{command: "if ! mv <data_dir> <data_dir>.old; then", bad: true},
		{command: `elif mv "$staging" <data_dir>; then`, bad: true},
		{command: "mv <data_dir>/<dbname> <city>/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>", bad: false},
		{command: "mv .dolt-data/<dbname> <city>/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>", bad: false},
		{command: "mv <somewhere-safe> <data_dir>/<dbname>", bad: false},
		{command: "if ! mv <data_dir>/<dbname> <somewhere-safe>; then", bad: false},
		{command: `elif mv "$staging/<dbname>" <data_dir>/<dbname>; then`, bad: false},
		{command: "dolt backup restore <backup-url> <dbname>", bad: false},
		{command: "mkdir -p <city>/.gc/runtime/packs/dolt/corrupt-aside", bad: false},
	} {
		if got := lineViolation(tc.command) != ""; got != tc.bad {
			t.Errorf("lineViolation(%q) reports a violation = %v, want %v", tc.command, got, tc.bad)
		}
	}
}

// TestFencedShellLinesScansEveryFence: commands in an untagged fence, and
// commented ones, are scanned like those in a bash fence.
func TestFencedShellLinesScansEveryFence(t *testing.T) {
	doc := "intro\n```\nmv dolt dolt.old\n```\n\n```bash\n# rm -rf .beads/dolt\n```\n"
	got := fencedShellLines(doc)
	want := []shellLine{{n: 3, text: "mv dolt dolt.old"}, {n: 7, text: "rm -rf .beads/dolt"}}
	if len(got) != len(want) {
		t.Fatalf("fencedShellLines = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fencedShellLines[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// pinnedBdReleaseRE matches where a recovery page names the bd release its
// steps were checked against, across a Markdown line wrap.
var pinnedBdReleaseRE = regexp.MustCompile(`pinned\s+bd\s+release\s+\((v[^)\s]+)\)`)

// TestJournalRecoveryRunbookNamesThePinnedBd fails when the JSONL fallback
// names a bd release other than the one deps.env pins (BD_VERSION). How that
// step re-creates a database depends on the release: bd through v1.3.2 does
// it with --reinit-local and has no --recreate-missing, while beads main
// refuses --reinit-local for a missing database and wants --recreate-missing.
// The page went on naming v1.3.1 after the pin moved (#1979), so a pin bump
// must re-check that step, and its split-store stop, against the new release.
func TestJournalRecoveryRunbookNamesThePinnedBd(t *testing.T) {
	const rel = "examples/bd/dolt/docs/journal-corruption-recovery.md"
	pinned := depsEnvValue(t, "BD_VERSION")
	named := pinnedBdReleaseRE.FindAllStringSubmatch(readDoltRecoveryDoc(t, rel), -1)
	if len(named) == 0 {
		t.Fatalf("%s names no pinned bd release: Step 4 (fallback) must say which release its re-create step holds for", rel)
	}
	for _, m := range named {
		if m[1] != pinned {
			t.Errorf("%s names the pinned bd release %s, but deps.env pins BD_VERSION=%s: re-check Step 4 (fallback) against bd %s (does --reinit-local still re-create a missing database, and does `bd init --help` list --recreate-missing?) and its split-store stop, then name %s", rel, m[1], pinned, pinned, pinned)
		}
	}
}

// depsEnvValue returns the value deps.env assigns to key.
func depsEnvValue(t *testing.T, key string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), "deps.env"))
	if err != nil {
		t.Fatalf("reading deps.env: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	t.Fatalf("deps.env does not set %s", key)
	return ""
}
