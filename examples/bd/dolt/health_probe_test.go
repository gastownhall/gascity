package dolt_test

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// probeFakeDolt is a fake `dolt` client for the health probe tests. SELECT 1
// fails for the first $FAKE_DOLT_PING_FAILURES calls ("always" = every call),
// printing an error that carries JSON-hostile bytes (quotes, a backslash, a
// newline, a tab) so the report's escaping is exercised on every failure. Each
// SELECT 1 call is counted in $FAKE_DOLT_PING_COUNT. The per-database count
// queries answer 810 commits / 7 open beads unless told to fail or to return
// no number.
const probeFakeDolt = `#!/bin/sh
q=""
prev=""
for a in "$@"; do
  [ "$prev" = "-q" ] && q="$a"
  prev="$a"
done
case "$q" in
  "SELECT 1")
    n=0
    [ -f "$FAKE_DOLT_PING_COUNT" ] && n=$(cat "$FAKE_DOLT_PING_COUNT")
    n=$((n + 1))
    printf '%s\n' "$n" > "$FAKE_DOLT_PING_COUNT"
    if [ "${FAKE_DOLT_PING_FAILURES:-0}" = "always" ] || [ "$n" -le "${FAKE_DOLT_PING_FAILURES:-0}" ]; then
      if [ -z "${FAKE_DOLT_PING_SILENT:-}" ]; then
        printf 'fake ping %s: dial tcp: "connection refused" \\ retry\n\tsecond line\n' "$n" >&2
      fi
      exit "${FAKE_DOLT_PING_EXIT:-1}"
    fi
    printf '+---+\n| 1 |\n+---+\n'
    ;;
  *"FROM dolt_log"*)
    if [ -n "${FAKE_DOLT_COMMITS_FAIL:-}" ]; then
      printf 'fake commits probe: %s\n' "$FAKE_DOLT_COMMITS_FAIL" >&2
      exit 1
    fi
    if [ -n "${FAKE_DOLT_COMMITS_EMPTY:-}" ]; then
      printf 'COUNT(*)\n'
      exit 0
    fi
    printf 'COUNT(*)\n810\n'
    ;;
  *"FROM issues"*)
    printf 'COUNT(*)\n7\n'
    ;;
esac
exit 0
`

// probeFixture is a local managed-Dolt fixture: a live TCP listener that the
// lsof/nc fakes report as the managed server, one on-disk database ("hq"), and
// probeFakeDolt standing in for the SQL client.
type probeFixture struct {
	root      string
	env       []string
	pingCount string
}

func newProbeFixture(t *testing.T) probeFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	writeExecutable(t, filepath.Join(fakeBin, "gc"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "lsof"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "nc"), `#!/bin/sh
if [ "$1" = "-z" ] && [ "$2" = "127.0.0.1" ] && [ "$3" = "`+port+`" ]; then
  exit 0
fi
exit 1
`)
	writeExecutable(t, filepath.Join(fakeBin, "dolt"), probeFakeDolt)

	root := repoRoot(t)
	pingCount := filepath.Join(t.TempDir(), "ping-count")
	env := append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH", "GC_DOLT_DATA_DIR",
		"FAKE_DOLT_PING_COUNT", "FAKE_DOLT_PING_FAILURES", "FAKE_DOLT_PING_SILENT", "FAKE_DOLT_PING_EXIT",
		"FAKE_DOLT_COMMITS_FAIL", "FAKE_DOLT_COMMITS_EMPTY"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+port,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
		// Keep the suite fast; the pause between attempts is not under test.
		"GC_DOLT_HEALTH_PROBE_RETRY_SECS=0",
		"FAKE_DOLT_PING_COUNT="+pingCount,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	return probeFixture{root: root, env: env, pingCount: pingCount}
}

// run invokes the health script with extra env entries (later entries win).
func (f probeFixture) run(t *testing.T, extraEnv []string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := newHealthScriptCmd(f.root, append(append([]string{}, f.env...), extraEnv...), args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if stderr.Len() > 0 {
		t.Logf("health.sh stderr:\n%s", stderr.String())
	}
	return out, err
}

func (f probeFixture) pings(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.pingCount)
	if err != nil {
		t.Fatalf("fake dolt never answered a SELECT 1: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("ping count %q: %v", data, err)
	}
	return n
}

type probeReport struct {
	Server struct {
		Running       bool    `json:"running"`
		Reachable     bool    `json:"reachable"`
		ProbeAttempts *int    `json:"probe_attempts"`
		ProbeError    *string `json:"probe_error"`
	} `json:"server"`
	Databases []map[string]json.RawMessage `json:"databases"`
}

func decodeProbeReport(t *testing.T, out []byte) probeReport {
	t.Helper()
	var r probeReport
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("health.sh --json returned invalid JSON: %v\n%s", err, out)
	}
	return r
}

func (r probeReport) attempts(t *testing.T, out []byte) int {
	t.Helper()
	if r.Server.ProbeAttempts == nil {
		t.Fatalf("server.probe_attempts missing from report\n%s", out)
	}
	return *r.Server.ProbeAttempts
}

func (r probeReport) probeError(t *testing.T, out []byte) string {
	t.Helper()
	if r.Server.ProbeError == nil {
		t.Fatalf("server.probe_error missing from report; the client's error must not be discarded\n%s", out)
	}
	return *r.Server.ProbeError
}

func (r probeReport) database(t *testing.T, name string, out []byte) map[string]json.RawMessage {
	t.Helper()
	for _, db := range r.Databases {
		if string(db["name"]) == strconv.Quote(name) {
			return db
		}
	}
	t.Fatalf("database %q missing from report\n%s", name, out)
	return nil
}

func rawString(t *testing.T, raw json.RawMessage, field string, out []byte) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s = %s, want a JSON string: %v\n%s", field, raw, err, out)
	}
	return s
}

// TestHealthScriptRetriesTransientSelectOneFailure is the positive arm for
// m37-5lo: ONE failed SELECT 1 must not read as a downed server. Patrol
// automation escalates server.reachable=false as CRITICAL and the runbook's
// next step is a restart, so a single transient client failure used to page
// (and invite a restart of) a healthy server. The report must stay reachable,
// scan the databases, and still surface what the failed attempt said.
func TestHealthScriptRetriesTransientSelectOneFailure(t *testing.T) {
	f := newProbeFixture(t)
	out, err := f.run(t, []string{"FAKE_DOLT_PING_FAILURES=1"}, "--json")
	if err != nil {
		t.Fatalf("health.sh --json failed: %v\n%s", err, out)
	}
	r := decodeProbeReport(t, out)
	if !r.Server.Reachable {
		t.Fatalf("server.reachable = false after one transient SELECT 1 failure; want true\n%s", out)
	}
	if got := r.attempts(t, out); got != 2 {
		t.Fatalf("server.probe_attempts = %d, want 2 (one failure, one success)\n%s", got, out)
	}
	if got := f.pings(t); got != 2 {
		t.Fatalf("fake dolt saw %d SELECT 1 calls, want 2\n%s", got, out)
	}
	if !strings.Contains(r.probeError(t, out), "fake ping 1") {
		t.Fatalf("server.probe_error = %q, want the failed attempt's client error\n%s", r.probeError(t, out), out)
	}
	hq := r.database(t, "hq", out)
	if string(hq["commits"]) != "810" || string(hq["open_beads"]) != "7" {
		t.Fatalf("hq counts = commits %s open_beads %s, want 810/7 (database scan must run once reachable)\n%s",
			hq["commits"], hq["open_beads"], out)
	}
}

// TestHealthScriptReportsClientErrorWhenSelectOneNeverAnswers is the negative
// arm: a server that fails every attempt must still read as unreachable, with
// the client's own error in the report instead of discarded.
func TestHealthScriptReportsClientErrorWhenSelectOneNeverAnswers(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		f := newProbeFixture(t)
		out, err := f.run(t, []string{"FAKE_DOLT_PING_FAILURES=always"}, "--json")
		if err != nil {
			t.Fatalf("health.sh --json must exit 0 even when unreachable: %v\n%s", err, out)
		}
		r := decodeProbeReport(t, out)
		if r.Server.Reachable {
			t.Fatalf("server.reachable = true for a server that never answered\n%s", out)
		}
		if !r.Server.Running {
			t.Fatalf("server.running = false; the listener is up, only SQL fails\n%s", out)
		}
		if got := r.attempts(t, out); got != 3 {
			t.Fatalf("server.probe_attempts = %d, want the default 3\n%s", got, out)
		}
		if got := f.pings(t); got != 3 {
			t.Fatalf("fake dolt saw %d SELECT 1 calls, want 3\n%s", got, out)
		}
		for _, want := range []string{"fake ping 3", `"connection refused"`, `\ retry`, "second line"} {
			if !strings.Contains(r.probeError(t, out), want) {
				t.Fatalf("server.probe_error = %q, want it to carry %q from the last attempt\n%s", r.probeError(t, out), want, out)
			}
		}
		if len(r.Databases) != 0 {
			t.Fatalf("databases = %d entries; an unreachable server must not report counts\n%s", len(r.Databases), out)
		}
	})

	t.Run("human", func(t *testing.T) {
		f := newProbeFixture(t)
		out, err := f.run(t, []string{"FAKE_DOLT_PING_FAILURES=always"})
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("human mode exit = %v, want exit 1 for an unreachable server\n%s", err, out)
		}
		for _, want := range []string{"not answering SQL", "3 attempt", "fake ping 3"} {
			if !strings.Contains(string(out), want) {
				t.Fatalf("human output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("single attempt when configured", func(t *testing.T) {
		f := newProbeFixture(t)
		out, err := f.run(t, []string{"FAKE_DOLT_PING_FAILURES=1", "GC_DOLT_HEALTH_PROBE_ATTEMPTS=1"}, "--json")
		if err != nil {
			t.Fatalf("health.sh --json failed: %v\n%s", err, out)
		}
		r := decodeProbeReport(t, out)
		if r.Server.Reachable || r.attempts(t, out) != 1 || f.pings(t) != 1 {
			t.Fatalf("GC_DOLT_HEALTH_PROBE_ATTEMPTS=1: reachable=%v attempts=%d pings=%d, want false/1/1\n%s",
				r.Server.Reachable, r.attempts(t, out), f.pings(t), out)
		}
	})

	t.Run("timeout is named when the client prints nothing", func(t *testing.T) {
		f := newProbeFixture(t)
		out, err := f.run(t, []string{"FAKE_DOLT_PING_FAILURES=always", "FAKE_DOLT_PING_SILENT=1", "FAKE_DOLT_PING_EXIT=124"}, "--json")
		if err != nil {
			t.Fatalf("health.sh --json failed: %v\n%s", err, out)
		}
		r := decodeProbeReport(t, out)
		if !strings.Contains(r.probeError(t, out), "timed out after 5s") {
			t.Fatalf("server.probe_error = %q, want the run_bounded timeout named\n%s", r.probeError(t, out), out)
		}
	})
}

// TestHealthScriptFailedCountProbeReportsNullNotZero pins the count half of
// m37-5lo: a commits probe that fails (or returns no number) is unknown, not a
// measured zero. A 0 there is indistinguishable from an empty history and
// silently disarms the commit-bloat threshold for that sample.
func TestHealthScriptFailedCountProbeReportsNullNotZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		wantErr string
	}{
		{name: "client error", env: "FAKE_DOLT_COMMITS_FAIL=table not found: dolt_log", wantErr: "table not found: dolt_log"},
		{name: "no numeric result", env: "FAKE_DOLT_COMMITS_EMPTY=1", wantErr: "no numeric result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProbeFixture(t)
			out, err := f.run(t, []string{tc.env}, "--json")
			if err != nil {
				t.Fatalf("health.sh --json failed: %v\n%s", err, out)
			}
			r := decodeProbeReport(t, out)
			if !r.Server.Reachable || r.attempts(t, out) != 1 || r.probeError(t, out) != "" {
				t.Fatalf("server = reachable %v attempts %d error %q, want true/1/\"\" (only the count probe fails)\n%s",
					r.Server.Reachable, r.attempts(t, out), r.probeError(t, out), out)
			}
			hq := r.database(t, "hq", out)
			if got := string(hq["commits"]); got != "null" {
				t.Fatalf("hq commits = %s after a failed probe, want null (unknown), never a measured 0\n%s", got, out)
			}
			if got := string(hq["open_beads"]); got != "7" {
				t.Fatalf("hq open_beads = %s, want 7 (its own probe succeeded)\n%s", got, out)
			}
			raw, ok := hq["probe_error"]
			if !ok {
				t.Fatalf("hq probe_error missing; a null count must say why\n%s", out)
			}
			if msg := rawString(t, raw, "hq probe_error", out); !strings.Contains(msg, "commits") || !strings.Contains(msg, tc.wantErr) {
				t.Fatalf("hq probe_error = %q, want it to name the commits probe and %q\n%s", msg, tc.wantErr, out)
			}
		})
	}

	t.Run("healthy counts carry an empty probe_error", func(t *testing.T) {
		f := newProbeFixture(t)
		out, err := f.run(t, nil, "--json")
		if err != nil {
			t.Fatalf("health.sh --json failed: %v\n%s", err, out)
		}
		r := decodeProbeReport(t, out)
		hq := r.database(t, "hq", out)
		if string(hq["commits"]) != "810" || string(hq["open_beads"]) != "7" {
			t.Fatalf("hq counts = %s/%s, want 810/7\n%s", hq["commits"], hq["open_beads"], out)
		}
		if msg := rawString(t, hq["probe_error"], "hq probe_error", out); msg != "" {
			t.Fatalf("hq probe_error = %q, want empty when both probes succeeded\n%s", msg, out)
		}
	})

	t.Run("human output says unknown", func(t *testing.T) {
		f := newProbeFixture(t)
		out, err := f.run(t, []string{"FAKE_DOLT_COMMITS_FAIL=table not found: dolt_log"})
		if err != nil {
			t.Fatalf("human mode failed for a reachable server: %v\n%s", err, out)
		}
		for _, want := range []string{"hq: unknown commits", "table not found: dolt_log"} {
			if !strings.Contains(string(out), want) {
				t.Fatalf("human output missing %q:\n%s", want, out)
			}
		}
	})
}

// TestHealthScriptProbeReportsMatchResultSchema validates every probe outcome
// against the published result schema, so the nullable counts and the probe
// fields cannot drift from the contract `--json-schema=result` serves.
func TestHealthScriptProbeReportsMatchResultSchema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "commands", "health", "schemas", "result.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schemaDoc any
	if err := json.Unmarshal(data, &schemaDoc); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	const uri = "gc://schemas/dolt/health/result.schema.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(uri, schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := compiler.Compile(uri)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "healthy"},
		{name: "transient ping failure", env: []string{"FAKE_DOLT_PING_FAILURES=1"}},
		{name: "never answers", env: []string{"FAKE_DOLT_PING_FAILURES=always"}},
		{name: "failed count", env: []string{"FAKE_DOLT_COMMITS_FAIL=table not found: dolt_log"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProbeFixture(t)
			out, err := f.run(t, tc.env, "--json")
			if err != nil {
				t.Fatalf("health.sh --json failed: %v\n%s", err, out)
			}
			var payload any
			if err := json.Unmarshal(out, &payload); err != nil {
				t.Fatalf("health.sh --json returned invalid JSON: %v\n%s", err, out)
			}
			if err := schema.Validate(payload); err != nil {
				t.Fatalf("report does not match result.schema.json: %v\n%s", err, out)
			}
		})
	}
}

// TestDoltHealthCheckCarriesProbeError keeps the client's error attached where
// the dolt-health order records its failure message.
func TestDoltHealthCheckCarriesProbeError(t *testing.T) {
	script := filepath.Join(repoRoot(t), "commands", "health-check", "run.sh")
	input := `{
  "server": {
    "running": true,
    "reachable": false,
    "external": false,
    "pid": 123,
    "port": 3311,
    "latency_ms": 0,
    "probe_attempts": 3,
    "probe_error": "exit 1: dial tcp: \"connection refused\""
  }
}`
	cmd := exec.Command("sh", script)
	cmd.Stdin = strings.NewReader(input)
	// Assert on stderr alone: health-check echoes the whole report to stdout,
	// so a combined-output match would find the error text in the echo even if
	// the failure message dropped it.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("health-check unexpectedly succeeded:\n%s", stderr.String())
	}
	for _, want := range []string{"Dolt server unreachable", "attempts=3", `error=exit 1: dial tcp: "connection refused"`} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("health-check failure message missing %q:\n%s", want, stderr.String())
		}
	}
}
