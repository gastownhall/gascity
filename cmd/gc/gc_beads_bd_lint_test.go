package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestGcBeadsBdNoBdConfigSet enforces the perf-fix from ga-5mym: the
// gc-beads-bd init script must never invoke `bd config set` (directly or
// through the run_bd_* wrappers). bd >= 1.0.3 makes that call cost 18-50s
// per invocation due to auto-migrate; combined cost overruns the 30s
// providerOpTimeout and the supervisor wedges in starting_bead_store.
//
// The replacement path is ensure_bd_runtime_config_value (direct SQL into
// the bd config table). Any future regression must use that helper, not
// the slow bd CLI subcommand.
func TestGcBeadsBdNoBdConfigSet(t *testing.T) {
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	f, err := os.Open(scriptPath)
	if err != nil {
		t.Fatalf("open script: %v", err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // test cleanup

	// Match `bd <args> config set` and `run_bd_<wrapper> <args> config set`.
	// The script invokes bd both directly and through helpers like
	// run_bd_pinned, which always end up calling `bd config set` — both
	// shapes hit the slow auto-migrate path.
	pattern := regexp.MustCompile(`bd[a-zA-Z_]*[[:space:]]+.*config[[:space:]]+set`)
	commentLine := regexp.MustCompile(`^[[:space:]]*#`)

	var offenders []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if commentLine.MatchString(line) {
			continue
		}
		if pattern.MatchString(line) {
			offenders = append(offenders, formatOffender(scriptPath, lineNum, line))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan script: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("ERROR: bd config set re-introduced in gc-beads-bd.sh.\n"+
			"See ga-5mym; use ensure_bd_runtime_config_value (direct SQL) instead.\n"+
			"Offending lines:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func formatOffender(path string, line int, content string) string {
	return path + ":" + strconv.Itoa(line) + ": " + strings.TrimSpace(content)
}

func repoRootForLint(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root")
		}
		dir = parent
	}
}
