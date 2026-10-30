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

	"github.com/gastownhall/gascity/internal/bazeltest"
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

func TestDoltliteRuntimeConfigUsesSQLiteParameters(t *testing.T) {
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fn := extractShellFunction(t, string(data), "ensure_doltlite_runtime_config_value")

	for _, want := range []string{
		".parameter set @gc_config_key",
		".parameter set @gc_config_value",
		`VALUES (@gc_config_key, @gc_config_value)`,
	} {
		if !strings.Contains(fn, want) {
			t.Fatalf("ensure_doltlite_runtime_config_value missing %q:\n%s", want, fn)
		}
	}
	for _, forbidden := range []string{
		"VALUES ('$key_sql', '$value_sql')",
		"VALUES ('$key', '$value')",
	} {
		if strings.Contains(fn, forbidden) {
			t.Fatalf("ensure_doltlite_runtime_config_value interpolates config values into SQL via %q:\n%s", forbidden, fn)
		}
	}
}

func TestDoltliteMaintenanceDueUsesPortableStatFallback(t *testing.T) {
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fn := extractShellFunction(t, string(data), "doltlite_maintenance_due")

	want := `stat -c %Y "$stamp" 2>/dev/null || stat -f %m "$stamp" 2>/dev/null || echo 0`
	if !strings.Contains(fn, want) {
		t.Fatalf("doltlite_maintenance_due missing portable GNU/BSD stat fallback %q:\n%s", want, fn)
	}
}

// TestDoltliteReindexUsesInProcessGc pins the ga-7hei maintenance-path heal to
// an in-process, SQLite-capable REINDEX. `bd flatten`/`bd gc` rewrite the
// DoltLite store and leave its physical SQLite secondary indexes stale, so
// index-path reads (count/status/list) silently return wrong results until a
// REINDEX. REINDEX is SQLite-specific DDL: it must run against the
// .beads/doltlite/<db>.db file through gc's own SQLite driver
// (gc dolt-config doltlite-reindex), resolved via resolve_gc_helper_bin. It
// must NOT route through `bd sql`, which speaks Dolt/MySQL and cannot execute
// REINDEX (and is refused in the embedded mode run_bd_doltlite forces), so that
// path is an inert no-op that leaves the stale-index corruption live.
func TestDoltliteReindexUsesInProcessGc(t *testing.T) {
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fn := extractShellFunction(t, string(data), "run_doltlite_reindex")

	if !strings.Contains(fn, "resolve_gc_helper_bin") {
		t.Fatalf("run_doltlite_reindex must resolve the gc helper via resolve_gc_helper_bin so REINDEX "+
			"runs in gc's SQLite engine (ga-7hei):\n%s", fn)
	}
	if !strings.Contains(fn, "dolt-config doltlite-reindex --dir") {
		t.Fatalf("run_doltlite_reindex must reindex via `gc dolt-config doltlite-reindex --dir`, which "+
			"runs SQLite REINDEX against the physical .db (ga-7hei):\n%s", fn)
	}
	// `bd sql` speaks Dolt/MySQL and cannot execute SQLite REINDEX (and is
	// refused in embedded mode), so it must never be the reindex mechanism.
	if strings.Contains(fn, `sql 'REINDEX'`) || strings.Contains(fn, `sql "REINDEX"`) {
		t.Fatalf("run_doltlite_reindex must not reindex through `bd sql`: the Dolt/MySQL dialect cannot "+
			"execute SQLite REINDEX and is refused in embedded mode, making it an inert no-op (ga-7hei):\n%s", fn)
	}
}

// TestDoltliteMaintenanceGatesStampOnReindex pins the ga-7hei maintenance
// contract: `bd flatten`/`bd gc` CREATE the stale-index condition, so the
// maintenance path must (1) skip them entirely when no reindex-capable gc
// helper is available — never manufacturing corruption it cannot heal — and
// (2) advance the .gc-maintenance.stamp only when the reindex actually
// succeeds, so a failed reindex stays visible and retryable instead of being
// suppressed for the whole maintenance interval. A best-effort reindex that
// falls through to an unconditional stamp is the exact regression this guards.
func TestDoltliteMaintenanceGatesStampOnReindex(t *testing.T) {
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fn := extractShellFunction(t, string(data), "run_doltlite_existing_db_maintenance")

	// Anchor on statement forms, not bare words, so prose in the function's
	// comments (which necessarily mention "flatten"/"reindex") cannot satisfy or
	// falsely trip these ordering checks.
	gateIdx := strings.Index(fn, "if ! doltlite_reindex_supported")
	if gateIdx < 0 {
		t.Fatalf("run_doltlite_existing_db_maintenance must probe doltlite_reindex_supported so it skips the "+
			"stale-index-producing flatten/gc when reindex is unavailable, rather than manufacturing index "+
			"corruption it cannot heal (ga-7hei):\n%s", fn)
	}
	flattenIdx := strings.Index(fn, `run_bd_doltlite "$dir" flatten`)
	if flattenIdx < 0 {
		t.Fatalf("run_doltlite_existing_db_maintenance missing the flatten maintenance step:\n%s", fn)
	}
	if gateIdx > flattenIdx {
		t.Fatalf("run_doltlite_existing_db_maintenance must probe doltlite_reindex_supported BEFORE running "+
			"flatten/gc; otherwise the stale-index-producing compaction runs even when it cannot be healed "+
			"(ga-7hei):\n%s", fn)
	}

	// The stamp must be gated on reindex success. A bare
	// `run_doltlite_reindex ... || echo warning` that falls through to the
	// stamp is the exact bug: it latches "maintenance done" while the indexes
	// are still stale, suppressing retries for the interval.
	reindexGateIdx := strings.Index(fn, "if ! run_doltlite_reindex")
	if reindexGateIdx < 0 {
		t.Fatalf("run_doltlite_existing_db_maintenance must guard the maintenance stamp on reindex success "+
			"(if ! run_doltlite_reindex ...; then ... return), not advance it unconditionally after a "+
			"best-effort reindex (ga-7hei):\n%s", fn)
	}
	stampIdx := strings.Index(fn, `date +%s > "$stamp"`)
	if stampIdx < 0 {
		t.Fatalf("run_doltlite_existing_db_maintenance missing the maintenance stamp write:\n%s", fn)
	}
	if reindexGateIdx > stampIdx {
		t.Fatalf("run_doltlite_existing_db_maintenance must run the guarded reindex before writing the "+
			"maintenance stamp (ga-7hei):\n%s", fn)
	}
}

// ambientSocketUnsetPattern requires BEADS_DOLT_SERVER_SOCKET to appear in an
// `unset` statement rather than merely somewhere in the helper body. Anchoring
// on the statement form is what makes this a scrub assertion instead of a
// spelling assertion: a bare substring match is equally satisfied by a line
// that *sets* the socket, which is the exact regression being guarded. The
// name must be a bare operand and every operand before it a plain variable
// name: `unset $BEADS_DOLT_SERVER_SOCKET` unsets whatever variable the socket
// path names, and `unset -f` removes a function, so neither is a scrub.
var ambientSocketUnsetPattern = regexp.MustCompile(
	`(?m)^[ \t]*unset(?:[ \t]+-v)?(?:[ \t]+[A-Za-z_]\w*)*[ \t]+BEADS_DOLT_SERVER_SOCKET\b`)

// ambientSocketAssignPattern is the inverse guard: assigning the socket inside
// these helpers re-establishes the ambient topology the unset exists to drop.
// The assignment counts wherever a shell word can start (line start,
// whitespace, `;`, `&`, `|`, `(`), which covers plain and exported
// assignments, a prefix on the bd command itself, and env's operands, which
// may also be quoted.
var ambientSocketAssignPattern = regexp.MustCompile(
	`(?m)(?:^|[\s;&|(])BEADS_DOLT_SERVER_SOCKET=|\benv\b[^\n]*\bBEADS_DOLT_SERVER_SOCKET=`)

// ambientSocketBdInvocations lists the call-site spellings the ordering check
// recognizes. Each carries the argument expansion so an anchor cannot land on
// `bd_bin="${BD_BIN:-bd}"`, which only computes the binary name and can be
// hoisted without moving either the scrub or the invocation.
var ambientSocketBdInvocations = []string{`"$bd_bin" "$@"`, `"${BD_BIN:-bd}" "$@"`}

// TestBeadsHelpersClearAmbientSocket guards the socket scrub in the proxied and
// DoltLite helpers: each must unset BEADS_DOLT_SERVER_SOCKET before invoking
// bd. An inherited socket must not override the explicit proxied or DoltLite
// selection: gc's own env projection (cmd/gc/bd_env.go) drops host/port
// whenever a socket is present, so a leaked socket silently redirects an
// explicitly selected topology.
//
// The check is textual: it orders the unset statement against the bd call in
// the helper body, so it assumes both run in the helper's own subshell. An
// unset confined to a nested subshell or a conditional branch still reads as
// a scrub.
//
// The remaining bd fork sites are deliberately not asserted. run_bd_pinned and
// op_init's proxied `bd context` probe do not scrub the socket today; whether
// they should depends on beads' own connection-resolution order, which is
// tracked separately (ga-kbcwc). run_provider_owned_bd scrubs conditionally:
// an external-target provider init keeps the socket, which
// op_provider_owned_init forwards to a direct init as --server-socket.
// bd_version_output runs `bd version`, which opens no store.
func TestBeadsHelpersClearAmbientSocket(t *testing.T) {
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	script := string(data)

	for _, name := range []string{"run_bd_init_proxied", "run_bd_doltlite"} {
		// Shell binds the last definition of a name, while extractShellFunction
		// grades the first. A duplicate definition appended without the scrub
		// would leave this guard green while the executed copy leaks the socket.
		if got := countShellFunctionDefinitions(script, name); got != 1 {
			t.Errorf("%s definitions = %d, want 1; the guard grades the first definition "+
				"but shell executes the last", name, got)
			continue
		}
		fn := extractShellFunction(t, script, name)

		if ambientSocketAssignPattern.MatchString(fn) {
			t.Errorf("%s assigns BEADS_DOLT_SERVER_SOCKET; the helper must drop the ambient socket, "+
				"not establish one:\n%s", name, fn)
		}
		unsetLoc := ambientSocketUnsetPattern.FindStringIndex(fn)
		if unsetLoc == nil {
			t.Errorf("%s does not unset BEADS_DOLT_SERVER_SOCKET:\n%s", name, fn)
			continue
		}

		invokeAt := -1
		for _, anchor := range ambientSocketBdInvocations {
			if at := strings.Index(fn, anchor); at >= 0 && (invokeAt < 0 || at < invokeAt) {
				invokeAt = at
			}
		}
		if invokeAt < 0 {
			t.Errorf("%s: no recognized bd invocation to order against. Add the new call-site "+
				"spelling to ambientSocketBdInvocations rather than leaving the ordering check "+
				"silently unenforced:\n%s", name, fn)
			continue
		}
		if unsetLoc[0] > invokeAt {
			t.Errorf("%s clears ambient socket after invoking bd:\n%s", name, fn)
		}
	}
}

// countShellFunctionDefinitions counts every header that defines name in a
// spelling the shell accepts: indented (a definition nested in an if/fi still
// binds), with space around or inside the parens, with the brace on the next
// line, or through bash's `function` keyword. Counting only the canonical
// `name() {` would miss exactly the duplicate a last-definition-wins shadow
// needs.
func countShellFunctionDefinitions(script, name string) int {
	q := regexp.QuoteMeta(name)
	pattern := regexp.MustCompile(`(?m)^[ \t]*(?:function[ \t]+` + q + `\b|` + q + `[ \t]*\([ \t]*\))`)
	return len(pattern.FindAllStringIndex(script, -1))
}

func TestCountShellFunctionDefinitionsMatchesHeaderSpellings(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   int
	}{
		{name: "canonical", script: "helper() {\n}\n", want: 1},
		{name: "space before parens", script: "helper () {\n}\n", want: 1},
		{name: "space inside parens", script: "helper ( ) {\n}\n", want: 1},
		{name: "no space before brace", script: "helper(){\n}\n", want: 1},
		{name: "brace on next line", script: "helper()\n{\n}\n", want: 1},
		{name: "indented in conditional", script: "if true; then\n    helper() {\n    }\nfi\n", want: 1},
		{name: "function keyword", script: "function helper {\n}\n", want: 1},
		{name: "function keyword with parens", script: "function helper() {\n}\n", want: 1},
		{name: "shadowing duplicate", script: "helper() {\n}\nhelper () {\n}\n", want: 2},
		{name: "longer name", script: "helper_extra() {\n}\n", want: 0},
		{name: "prefixed name", script: "my_helper() {\n}\n", want: 0},
		{name: "function keyword longer name", script: "function helper_extra {\n}\n", want: 0},
		{name: "call site", script: "helper \"$dir\"\n", want: 0},
		{name: "comment", script: "# helper() {\n", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countShellFunctionDefinitions(tt.script, "helper"); got != tt.want {
				t.Fatalf("countShellFunctionDefinitions(%q) = %d, want %d", tt.script, got, tt.want)
			}
		})
	}
}

func TestAmbientSocketPatternsClassifyShellForms(t *testing.T) {
	tests := []struct {
		line       string
		wantUnset  bool
		wantAssign bool
	}{
		{line: "unset BEADS_DOLT_SERVER_SOCKET", wantUnset: true},
		{line: "        unset BEADS_DOLT_SERVER_HOST BEADS_DOLT_SERVER_SOCKET BEADS_DOLT_SERVER_USER", wantUnset: true},
		{line: "unset -v BEADS_DOLT_SERVER_SOCKET", wantUnset: true},
		{line: "unset $BEADS_DOLT_SERVER_SOCKET"},
		{line: "unset ${BEADS_DOLT_SERVER_SOCKET}"},
		{line: "unset -f BEADS_DOLT_SERVER_SOCKET"},
		{line: "unset BEADS_DOLT_SERVER_SOCKET_PATH"},
		{line: "# unset BEADS_DOLT_SERVER_SOCKET"},
		{line: "echo unset BEADS_DOLT_SERVER_SOCKET"},
		{line: "BEADS_DOLT_SERVER_SOCKET=\"$sock\"", wantAssign: true},
		{line: "    export BEADS_DOLT_SERVER_SOCKET=\"$sock\"", wantAssign: true},
		{line: "export BEADS_DIR=\"$dir\" BEADS_DOLT_SERVER_SOCKET=\"$sock\"", wantAssign: true},
		{line: "BEADS_DIR=\"$dir\" BEADS_DOLT_SERVER_SOCKET=\"$sock\" \"$bd_bin\" \"$@\"", wantAssign: true},
		{line: "cd \"$dir\"; BEADS_DOLT_SERVER_SOCKET=\"$sock\" \"$bd_bin\" \"$@\"", wantAssign: true},
		{line: "true && BEADS_DOLT_SERVER_SOCKET=\"$sock\" \"$bd_bin\" \"$@\"", wantAssign: true},
		{line: "(BEADS_DOLT_SERVER_SOCKET=\"$sock\" \"$bd_bin\" \"$@\")", wantAssign: true},
		{line: "env BEADS_DOLT_SERVER_SOCKET=\"$sock\" \"${BD_BIN:-bd}\" \"$@\"", wantAssign: true},
		{line: "env \"BEADS_DOLT_SERVER_SOCKET=$sock\" \"${BD_BIN:-bd}\" \"$@\"", wantAssign: true},
		{line: "if [ -n \"${BEADS_DOLT_SERVER_SOCKET:-}\" ]; then"},
		{line: "set -- \"$@\" --server-socket \"$BEADS_DOLT_SERVER_SOCKET\""},
		{line: "GC_BEADS_DOLT_SERVER_SOCKET=\"$sock\""},
		{line: "echo \"BEADS_DOLT_SERVER_SOCKET=$sock\""},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			if got := ambientSocketUnsetPattern.MatchString(tt.line); got != tt.wantUnset {
				t.Errorf("unset match = %v, want %v", got, tt.wantUnset)
			}
			if got := ambientSocketAssignPattern.MatchString(tt.line); got != tt.wantAssign {
				t.Errorf("assign match = %v, want %v", got, tt.wantAssign)
			}
		})
	}
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
	if root := bazeltest.OverrideRoot(); root != "" {
		return root
	}
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

func TestGCBeadsBDScriptPinsWitnessAndDirectInitToBDBin(t *testing.T) {
	scriptPath := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, want := range []string{
		`raw=$("${BD_BIN:-bd}" version 2>/dev/null)`,
		`"${BD_BIN:-bd}" "$@"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("gc-beads-bd.sh must use BD_BIN for direct managed initialization boundary: missing %q", want)
		}
	}
}
