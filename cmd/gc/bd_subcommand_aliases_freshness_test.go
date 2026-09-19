//go:build integration

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// realBdPath resolves the actually-installed bd binary, skipping the
// testscript-provisioned "bin" directory TestMain (main_test.go) puts ahead
// of it on PATH for this package's txtar fixtures — that directory holds a
// wrapper that re-execs THIS test binary as a fake "bd" (bdTestCmd), not the
// real CLI this guard needs to shell out to. plain exec.LookPath("bd") in
// this package finds the fake one first and silently checks against it
// instead of a live release, which is worse than not checking at all.
func realBdPath(t *testing.T) (string, bool) {
	t.Helper()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if strings.Contains(dir, "testscript-main") {
			continue
		}
		candidate := filepath.Join(dir, "bd")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}

// aliasesLineRE matches a cobra help transcript's "Aliases:" section, e.g.
//
//	Aliases:
//	  create, new
//
// capturing the comma-separated list on the line that follows.
var aliasesLineRE = regexp.MustCompile(`(?m)^Aliases:\n\s*(.+)$`)

// parseHelpAliases extracts the alias set (including the canonical verb
// itself) declared in a bd `<verb> --help` transcript. A transcript with no
// "Aliases:" section (the verb has no aliases) yields an empty set.
func parseHelpAliases(help string) map[string]bool {
	m := aliasesLineRE.FindStringSubmatch(help)
	if m == nil {
		return nil
	}
	names := make(map[string]bool)
	for _, part := range strings.Split(m[1], ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			names[part] = true
		}
	}
	return names
}

// TestBdSubcommandAliasesCurrent guards bdSubcommandAliases (cmd_bd_by_id.go)
// against bd growing or renaming an alias out from under it. gc-0nf9kp round
// 4 found this table missing three of the four registered aliases in scope
// here (done/close, view/show, protomolecule/mol); bd registers seven pairs
// in all, and the other three are out of scope because bdflags keys none of
// their canonical verbs and none accepts --assignee. It shipped stale with every
// other test green because nothing compared it against the installed bd.
//
// This mirrors internal/bdflags.TestBdFlagManifestCurrent's posture: shell
// the real installed bd's --help per canonical verb, fail closed if the live
// CLI declares an alias the table doesn't know (that alias would walk
// straight past checkBdAssigneeArgs the way "protomolecule pour" did), and
// skip rather than fail if bd is not in PATH, since alias currency can't be
// checked without a bd binary to check it against.
func TestBdSubcommandAliasesCurrent(t *testing.T) {
	bdPath, ok := realBdPath(t)
	if !ok {
		t.Skip("bd not found in PATH; skipping subcommand-alias freshness check")
	}

	// canonical -> every alias this table maps to it, itself included.
	byCanonical := map[string]map[string]bool{}
	for alias, canonical := range bdSubcommandAliases {
		if byCanonical[canonical] == nil {
			byCanonical[canonical] = map[string]bool{canonical: true}
		}
		byCanonical[canonical][alias] = true
	}

	for canonical, known := range byCanonical {
		t.Run(canonical, func(t *testing.T) {
			// Routed through the shared runShellCommand body (pool.go) rather
			// than a fresh os/exec call site, per the untagged-subprocess
			// census ratchet in test/test-resources.toml.
			command := shellQuotePath(bdPath) + " " + shellQuotePath(canonical) + " --help"
			out, _ := shellCommand(command, "", 10*time.Second, nil)
			live := parseHelpAliases(out)
			if live == nil {
				t.Fatalf("parsed no \"Aliases:\" section from `bd %s --help`; output format may have changed:\n%s", canonical, out)
			}

			var missing, extra []string
			for a := range live {
				if !known[a] {
					missing = append(missing, a)
				}
			}
			for a := range known {
				if !live[a] {
					extra = append(extra, a)
				}
			}
			sort.Strings(missing)
			sort.Strings(extra)

			if len(missing) > 0 {
				t.Errorf("bd %s: live aliases %v not in bdSubcommandAliases; a caller spelling one of them walks past checkBdAssigneeArgs unresolved. Add it to the table in cmd/gc/cmd_bd_by_id.go.", canonical, missing)
			}
			if len(extra) > 0 {
				t.Errorf("bd %s: bdSubcommandAliases claims alias(es) %v this bd does not declare; stale entry, remove it.", canonical, extra)
			}
		})
	}
}
