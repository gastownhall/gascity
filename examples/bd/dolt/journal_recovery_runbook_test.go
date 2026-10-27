package dolt_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const journalCorruptionRunbook = "docs/journal-corruption-recovery.md"

// gcDoltVerbPattern matches a `gc dolt <verb>` invocation in the runbook.
var gcDoltVerbPattern = regexp.MustCompile(`\bgc dolt ([a-z][a-z0-9-]*)`)

// fencedBlockPattern matches one fenced code block's body.
var fencedBlockPattern = regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")

func readJournalCorruptionRunbook(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), journalCorruptionRunbook))
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	return string(data)
}

// TestJournalCorruptionRunbookNamesShippedDoltCommands pins the operator-facing
// journal corruption runbook to commands and paths that exist: every
// `gc dolt <verb>` it names is a shipped dolt pack command, the gc-managed
// restore reads bd's "default" backup (what mol-dog-backup registers), the
// automatic start-time recovery and its corrupt-aside quarantine are
// described, and the JSONL import comes from the core jsonl-archive and is
// labeled a lossy last resort.
func TestJournalCorruptionRunbookNamesShippedDoltCommands(t *testing.T) {
	runbook := readJournalCorruptionRunbook(t)

	if strings.Contains(runbook, "gc dolt backup") {
		t.Errorf("runbook names `gc dolt backup`, which the dolt pack does not ship")
	}
	for _, match := range gcDoltVerbPattern.FindAllStringSubmatch(runbook, -1) {
		verb := match[1]
		info, err := os.Stat(filepath.Join(repoRoot(t), "commands", verb))
		if err != nil || !info.IsDir() {
			t.Errorf("runbook names `gc dolt %s`, but commands/%s/ is not shipped (stat err: %v)", verb, verb, err)
		}
	}

	restoreReadsDefault := false
	for _, block := range fencedBlockPattern.FindAllStringSubmatch(runbook, -1) {
		if strings.Contains(block[1], "dolt backup restore") && strings.Contains(block[1], ".backups.default.url") {
			restoreReadsDefault = true
		}
	}
	if !restoreReadsDefault {
		t.Errorf("runbook has no restore snippet that reads the registered default backup URL (.backups.default.url) and runs `dolt backup restore`")
	}

	for _, want := range []string{
		".gc/runtime/packs/dolt/corrupt-aside/",
		"gc start",
		"jsonl-archive",
		"JSONL recovery loses all non-issue history",
	} {
		if !strings.Contains(runbook, want) {
			t.Errorf("runbook missing %q", want)
		}
	}
	if !strings.Contains(strings.ToLower(runbook), "automatic") {
		t.Errorf("runbook does not describe the automatic start-time recovery")
	}
	if strings.Contains(runbook, ".beads/issues.jsonl") {
		t.Errorf("runbook still snapshots .beads/issues.jsonl, which gc no longer writes (bd auto-export is off)")
	}
}
