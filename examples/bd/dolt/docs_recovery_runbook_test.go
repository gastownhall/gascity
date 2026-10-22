package dolt_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// TestRecoveryRunbooksKeepRecoveryPerDatabase pins AC-5 (#1979): the two
// runbooks that describe manual journal-corruption recovery must keep every
// instruction per-database. The managed dolt data dir is shared by every
// database in the city, and the incident shape — moving the whole data dir
// aside and rebuilding it from one database — is what lost the databases no
// scope is bound to. For every command in a fenced shell block (commented
// commands included: they are what operators copy):
//
//   - nothing under the data dir may be deleted;
//   - the data dir itself may not be moved, deleted, cloned over or
//     re-created, and a move of <data_dir>/<dbname> must land outside the
//     data dir;
//   - every `gc dolt <verb>` named must be a command the dolt pack provides
//     (pack groups reject unknown verbs).
func TestRecoveryRunbooksKeepRecoveryPerDatabase(t *testing.T) {
	verbs := doltPackCommandVerbs(t)
	for label, path := range recoveryRunbookPaths(t) {
		t.Run(label, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read runbook: %v", err)
			}
			var problems []string
			for _, block := range fencedBlockLines(string(data)) {
				for _, line := range block {
					problems = append(problems, checkRecoveryRunbookLine(line, verbs)...)
				}
			}
			if len(problems) > 0 {
				t.Fatalf("%s has recovery commands that are not per-database or do not exist:\n%s", path, strings.Join(problems, "\n"))
			}
		})
	}
}

func recoveryRunbookPaths(t *testing.T) map[string]string {
	t.Helper()
	packDir := repoRoot(t)
	var repo string
	if root := bazeltest.OverrideRoot(); root != "" {
		repo = root
	} else {
		_, filename, _, _ := runtime.Caller(0)
		// <repo>/examples/bd/dolt/docs_recovery_runbook_test.go -> <repo>
		repo = filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filename))))
	}
	return map[string]string{
		"pack runbook":        filepath.Join(packDir, "docs", "journal-corruption-recovery.md"),
		"maintenance runbook": filepath.Join(repo, "engdocs", "contributors", "dolt-maintenance.md"),
	}
}

// doltPackCommandVerbs lists the verbs the dolt pack provides, from its
// commands directory: every directory under commands/ is one `gc dolt <verb>`.
func doltPackCommandVerbs(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "commands"))
	if err != nil {
		t.Fatalf("read pack commands dir: %v", err)
	}
	verbs := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			verbs[entry.Name()] = true
		}
	}
	if len(verbs) == 0 {
		t.Fatal("no pack commands found")
	}
	return verbs
}

// fencedBlockLines returns the lines of every fenced code block in a
// markdown document, fence markers excluded.
func fencedBlockLines(doc string) [][]string {
	var blocks [][]string
	var current []string
	inFence := false
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inFence {
			if strings.HasPrefix(trimmed, "```") {
				inFence = true
				current = nil
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			blocks = append(blocks, current)
			inFence = false
			continue
		}
		current = append(current, line)
	}
	return blocks
}

var gcDoltVerbRe = regexp.MustCompile(`(?:^|\s|;)gc dolt ([a-z][a-z-]*)`)

// checkRecoveryRunbookLine reports per-line violations of the per-database
// recovery contract. Commented commands count: a commented `rm -rf` is
// exactly what a stressed operator copies during an incident.
func checkRecoveryRunbookLine(line string, verbs map[string]bool) []string {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return nil
	}
	var problems []string
	switch fields[0] {
	case "rm":
		for _, arg := range nonFlagArgs(fields[1:]) {
			if isManagedDoltDataDir(arg) {
				problems = append(problems, trimmed+": deletes the shared data dir itself")
			} else if isUnderManagedDoltDataDir(arg) {
				problems = append(problems, trimmed+": deletes a database store under the data dir (move it aside instead)")
			}
		}
	case "mv":
		args := nonFlagArgs(fields[1:])
		if len(args) >= 1 && isManagedDoltDataDir(args[0]) {
			problems = append(problems, trimmed+": moves the shared data dir itself (move <data_dir>/<dbname> aside instead)")
		} else if len(args) >= 2 && isUnderManagedDoltDataDir(args[0]) &&
			(isManagedDoltDataDir(args[1]) || isUnderManagedDoltDataDir(args[1])) {
			problems = append(problems, trimmed+": move of a database store lands inside the data dir (land it outside)")
		}
	case "dolt":
		if len(fields) > 1 && fields[1] == "clone" {
			args := nonFlagArgs(fields[2:])
			if len(args) > 0 && isManagedDoltDataDir(args[len(args)-1]) {
				problems = append(problems, trimmed+": clones over the shared data dir itself")
			}
		}
	}
	if m := gcDoltVerbRe.FindStringSubmatch(trimmed); m != nil && !verbs[m[1]] {
		problems = append(problems, trimmed+": 'gc dolt "+m[1]+"' is not a command the dolt pack provides")
	}
	return problems
}

func nonFlagArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func stripShellQuotes(arg string) string {
	if len(arg) >= 2 {
		if (arg[0] == '"' && arg[len(arg)-1] == '"') || (arg[0] == '\'' && arg[len(arg)-1] == '\'') {
			return arg[1 : len(arg)-1]
		}
	}
	return arg
}

// isManagedDoltDataDir reports whether a shell argument names the managed
// dolt data dir itself: the absolute or .beads-relative layouts the docs use
// (the docs run moves from <city>/.beads, where the dir is the bare `dolt`),
// the <data_dir> placeholder, and the legacy .dolt-data layout.
func isManagedDoltDataDir(arg string) bool {
	arg = stripShellQuotes(arg)
	switch arg {
	case "dolt", "<data_dir>", ".dolt-data", ".beads/dolt":
		return true
	}
	return strings.HasSuffix(arg, "/.beads/dolt") || strings.HasSuffix(arg, "/.dolt-data")
}

// isUnderManagedDoltDataDir reports whether a shell argument names a path
// inside the managed dolt data dir (a database store), in the same layouts
// isManagedDoltDataDir recognizes.
func isUnderManagedDoltDataDir(arg string) bool {
	arg = stripShellQuotes(arg)
	return strings.HasPrefix(arg, "<data_dir>/") ||
		strings.HasPrefix(arg, "dolt/") ||
		strings.HasPrefix(arg, ".dolt-data/") ||
		strings.Contains(arg, ".beads/dolt/")
}
