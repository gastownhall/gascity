//go:build integration

package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRecoveryRunbooksNameOnlyBdFlagsThePinnedBdHas fails when a recovery
// runbook tells the operator to run a bd command (as `gc bd ...` or `bd ...`,
// commented examples included) whose subcommand or flags the pinned bd does
// not have. The JSONL fallback once named `gc bd init --recreate-missing`, a
// flag no released bd carries, so an operator following it had already moved
// the corrupt store aside when bd refused the command (#1979). The
// integration build puts the pinned bd first on PATH (GC_TEST_TOOL_PATHS);
// the per-database rules for these runbooks live in test/docsync.
func TestRecoveryRunbooksNameOnlyBdFlagsThePinnedBdHas(t *testing.T) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		t.Skipf("bd not found: %v", err)
	}
	helps := map[string]string{}
	checked := 0
	for _, runbook := range recoveryRunbooks(t) {
		data, err := os.ReadFile(runbook)
		if err != nil {
			t.Fatalf("read runbook: %v", err)
		}
		for _, line := range fencedCommandLines(string(data)) {
			for _, inv := range bdInvocations(line) {
				command := strings.Join(inv.command, " ")
				help, ok := helps[command]
				if !ok {
					help = bdHelp(t, bdPath, inv.command)
					helps[command] = help
				}
				if !strings.Contains(help, "bd "+command+" ") {
					t.Errorf("%s: %q: the pinned bd has no command 'bd %s'", runbook, line, command)
					continue
				}
				for _, flag := range inv.flags {
					if !helpListsFlag(help, flag) {
						t.Errorf("%s: %q: 'bd %s' of the pinned bd has no flag %s", runbook, line, command, flag)
					}
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no bd command in the recovery runbooks")
	}
}

// recoveryRunbooks returns the runbooks that tell an operator how to recover
// a managed Dolt store: the dolt pack's own and the maintenance runbook
// (//:engdocs_dolt_maintenance in this target's data).
func recoveryRunbooks(t *testing.T) []string {
	t.Helper()
	packDir := repoRoot(t)
	repo := filepath.Dir(filepath.Dir(filepath.Dir(packDir)))
	return []string{
		filepath.Join(packDir, "docs", "journal-corruption-recovery.md"),
		filepath.Join(repo, "engdocs", "contributors", "dolt-maintenance.md"),
	}
}

// fencedCommandLines returns the command text of every line inside a fenced
// block, with a commented command's leading '#' and any trailing comment
// removed: an operator copies a commented example as readily as a live one.
func fencedCommandLines(doc string) []string {
	var lines []string
	inFence := false
	for _, raw := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			continue
		}
		text := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		if i := strings.Index(text, " #"); i >= 0 {
			text = text[:i]
		}
		lines = append(lines, text)
	}
	return lines
}

// bdInvocation is one bd command a runbook line runs: its subcommand path
// (e.g. backup restore) and the flags it passes.
type bdInvocation struct {
	command []string
	flags   []string
}

var (
	shellSegmentRE  = regexp.MustCompile(`&&|\|\||[;|]`)
	bdCommandWordRE = regexp.MustCompile(`^[a-z][a-z-]*$`)
)

// bdInvocations returns the bd commands a shell line runs, through gc's
// passthrough (`gc bd`, whose --city and --rig flags gc consumes) or directly.
// The subcommand path is the run of command words right after bd; it ends at
// the first flag or argument.
func bdInvocations(line string) []bdInvocation {
	var invocations []bdInvocation
	for _, segment := range shellSegmentRE.Split(line, -1) {
		fields := strings.Fields(strings.TrimLeft(strings.TrimSpace(segment), "("))
		switch {
		case len(fields) >= 2 && fields[0] == "gc" && fields[1] == "bd":
			fields = withoutGCScopeFlags(fields[2:])
		case len(fields) >= 1 && fields[0] == "bd":
			fields = fields[1:]
		default:
			continue
		}
		var inv bdInvocation
		inCommand := true
		for _, field := range fields {
			switch {
			case strings.HasPrefix(field, "-"):
				inv.flags = append(inv.flags, field)
				inCommand = false
			case inCommand && bdCommandWordRE.MatchString(field):
				inv.command = append(inv.command, field)
			default:
				inCommand = false
			}
		}
		if len(inv.command) > 0 {
			invocations = append(invocations, inv)
		}
	}
	return invocations
}

// withoutGCScopeFlags drops the --city and --rig flags gc's bd passthrough
// takes for itself before handing the rest to bd.
func withoutGCScopeFlags(fields []string) []string {
	var rest []string
	for i := 0; i < len(fields); i++ {
		switch {
		case (fields[i] == "--city" || fields[i] == "--rig") && i+1 < len(fields):
			i++
		case strings.HasPrefix(fields[i], "--city="), strings.HasPrefix(fields[i], "--rig="):
		default:
			rest = append(rest, fields[i])
		}
	}
	return rest
}

// bdHelp returns the pinned bd's help for a subcommand path.
func bdHelp(t *testing.T, bdPath string, command []string) string {
	t.Helper()
	cmd := exec.Command(bdPath, append(append([]string{}, command...), "--help")...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd %s --help: %v\n%s", strings.Join(command, " "), err, out)
	}
	return string(out)
}

// helpListsFlag reports whether cobra help text lists a flag, in either its
// long form ("      --json") or its short form ("  -p, --prefix").
func helpListsFlag(help, flag string) bool {
	name, _, _ := strings.Cut(flag, "=")
	pattern := `(?m)^\s+` + regexp.QuoteMeta(name) + `, --`
	if strings.HasPrefix(name, "--") {
		pattern = `(?m)^\s+(?:-\w, )?` + regexp.QuoteMeta(name) + `(?:\s|$)`
	}
	return regexp.MustCompile(pattern).MatchString(help)
}
