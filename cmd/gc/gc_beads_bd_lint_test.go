package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var bdConfigSetPattern = regexp.MustCompile(`bd[a-zA-Z_]*[[:space:]]+.*config[[:space:]]+set`)

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

	offenders, err := bdConfigSetOffenders(scriptPath, f)
	if err != nil {
		t.Fatalf("scan script: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("ERROR: bd config set re-introduced in gc-beads-bd.sh.\n"+
			"See ga-5mym; use ensure_bd_runtime_config_value (direct SQL) instead.\n"+
			"Offending lines:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func TestGcBeadsBdConfigSetLintCases(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		wantHit bool
	}{
		{
			name:    "direct bd config set",
			script:  `bd config set issue_prefix "$prefix"`,
			wantHit: true,
		},
		{
			name:    "wrapper bd config set",
			script:  `run_bd_pinned "$dir" config set issue_prefix "$prefix"`,
			wantHit: true,
		},
		{
			name: "wrapper continuation bd config set",
			script: "run_bd_pinned \"$dir\" config \\\n" +
				"  set issue_prefix \"$prefix\"",
			wantHit: true,
		},
		{
			name: "direct continuation bd config set",
			script: "bd \\\n" +
				"  config set issue_prefix \"$prefix\"",
			wantHit: true,
		},
		{
			name:    "bd config get is safe",
			script:  `bd config get issue_prefix`,
			wantHit: false,
		},
		{
			name:    "runtime config helper is safe",
			script:  `ensure_bd_runtime_config_value "$db" "issue_prefix" "$prefix"`,
			wantHit: false,
		},
		{
			name:    "full line comment is safe",
			script:  `# bd config set issue_prefix "$prefix"`,
			wantHit: false,
		},
		{
			name:    "inline comment is safe",
			script:  `ensure_bd_runtime_config_value "$db" "issue_prefix" "$prefix" # replaces bd config set`,
			wantHit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offenders, err := bdConfigSetOffenders("test-script.sh", strings.NewReader(tt.script))
			if err != nil {
				t.Fatalf("scan script: %v", err)
			}
			gotHit := len(offenders) > 0
			if gotHit != tt.wantHit {
				t.Fatalf("bdConfigSetOffenders hit = %v, want %v; offenders: %v", gotHit, tt.wantHit, offenders)
			}
		})
	}
}

func TestNoBashCleanupProjectIDGuard(t *testing.T) {
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	script := string(data)

	for _, forbidden := range []string{"metadata_has_project_id", "backfill_project_id_if_missing"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("gc-beads-bd.sh must not contain %q", forbidden)
		}
	}
	for _, fn := range []string{"ensure_project_identity", "identity_toml_present"} {
		if got := countShellFunctionDefinitions(script, fn); got != 1 {
			t.Fatalf("%s definitions = %d, want 1", fn, got)
		}
	}

	ensureFn := extractShellFunction(t, script, "ensure_project_identity")
	if strings.Contains(ensureFn, "identity_toml_present") {
		t.Fatalf("ensure_project_identity must not guard on identity_toml_present:\n%s", ensureFn)
	}
	if got := strings.Count(ensureFn, "dolt-state ensure-project-id"); got != 1 {
		t.Fatalf("ensure_project_identity dolt-state ensure-project-id count = %d, want 1:\n%s", got, ensureFn)
	}
	if !strings.Contains(ensureFn, `--city "$GC_CITY_PATH"`) {
		t.Fatalf("ensure_project_identity missing --city \"$GC_CITY_PATH\":\n%s", ensureFn)
	}
}

func countShellFunctionDefinitions(script, name string) int {
	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\(\) \{`)
	return len(pattern.FindAllStringIndex(script, -1))
}

func bdConfigSetOffenders(path string, r io.Reader) ([]string, error) {
	var offenders []string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNum := 0
	continued := ""
	continuedLine := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimRight(stripShellComment(scanner.Text()), " \t")
		if line == "" && continued == "" {
			continue
		}
		if strings.HasSuffix(line, `\`) {
			if continued == "" {
				continuedLine = lineNum
			}
			continued = joinContinuedShellLine(continued, strings.TrimSuffix(line, `\`))
			continue
		}

		lineToCheck := line
		offenderLine := lineNum
		if continued != "" {
			lineToCheck = joinContinuedShellLine(continued, line)
			offenderLine = continuedLine
			continued = ""
			continuedLine = 0
		}
		if bdConfigSetPattern.MatchString(lineToCheck) {
			offenders = append(offenders, formatOffender(path, offenderLine, lineToCheck))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if continued != "" && bdConfigSetPattern.MatchString(continued) {
		offenders = append(offenders, formatOffender(path, continuedLine, continued))
	}
	return offenders, nil
}

func stripShellComment(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		return line[:i]
	}
	return line
}

func joinContinuedShellLine(prefix, line string) string {
	prefix = strings.TrimSpace(prefix)
	line = strings.TrimSpace(line)
	if prefix == "" {
		return line
	}
	if line == "" {
		return prefix
	}
	return prefix + " " + line
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
