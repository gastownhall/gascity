package dolt_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestJournalCorruptionRunbookNamesShippedDoltCommands pins the
// journal-corruption runbook to what actually ships (#1980): every
// `gc dolt <verb>` it names must have a commands/<verb>/ entry in this pack
// (there is no `gc dolt backup` — the manual restore reads the registered
// backup URL and runs `dolt backup restore` itself), the automatic start-time
// recovery and its corrupt-aside quarantine are described, and the JSONL
// fallback is sourced from the core jsonl-archive and labeled a lossy last
// resort rather than a snapshot of the no-longer-written .beads/issues.jsonl.
func TestJournalCorruptionRunbookNamesShippedDoltCommands(t *testing.T) {
	runbookPath := filepath.Join(repoRoot(t), "docs", "journal-corruption-recovery.md")
	data, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	runbook := string(data)

	t.Run("gc dolt verbs ship", func(t *testing.T) {
		verb := regexp.MustCompile(`gc dolt ([a-z-]+)`)
		seen := map[string]bool{}
		for _, m := range verb.FindAllStringSubmatch(runbook, -1) {
			seen[m[1]] = true
		}
		if len(seen) == 0 {
			t.Fatal("runbook names no `gc dolt <verb>` commands; the check found nothing to verify")
		}
		for name := range seen {
			if _, err := os.Stat(filepath.Join(repoRoot(t), "commands", name)); err != nil {
				t.Errorf("runbook names `gc dolt %s` but commands/%s/ does not ship it", name, name)
			}
		}
		if strings.Contains(runbook, "gc dolt backup") {
			t.Error("runbook must not name `gc dolt backup` — no backup verb ships; the manual restore runs `dolt backup restore` from the registered URL")
		}
	})

	t.Run("manual restore reads the registered backup", func(t *testing.T) {
		if !strings.Contains(runbook, ".backups.default") {
			t.Error("manual restore must read the registered default backup URL from repo_state.json")
		}
		if !strings.Contains(runbook, "dolt backup restore") {
			t.Error("manual restore must use `dolt backup restore`")
		}
		if !strings.Contains(runbook, "gc bd backup status --json") {
			t.Error("backup freshness must be checked with `gc bd backup status --json`")
		}
	})

	t.Run("automatic recovery described", func(t *testing.T) {
		if !strings.Contains(runbook, "corrupt-aside") {
			t.Error("runbook must describe the automatic start-time recovery's corrupt-aside quarantine")
		}
		if !strings.Contains(runbook, "gc start") || !strings.Contains(runbook, "automatic") {
			t.Error("runbook must describe the automatic gc start recovery path")
		}
	})

	t.Run("jsonl fallback is a lossy last resort from the archive", func(t *testing.T) {
		if !strings.Contains(runbook, "jsonl-archive") {
			t.Error("JSONL fallback must be sourced from the core jsonl-archive")
		}
		if !strings.Contains(runbook, "lossy") || !strings.Contains(runbook, "last resort") {
			t.Error("JSONL fallback must be labeled a lossy last resort")
		}
		if !strings.Contains(runbook, "loses all non-issue history") {
			t.Error("JSONL fallback must warn that non-issue history is lost")
		}
		if strings.Contains(runbook, ".beads/issues.jsonl") {
			t.Error("runbook must not snapshot .beads/issues.jsonl — gc suppresses the auto-export that writes it")
		}
	})
}
