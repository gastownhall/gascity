//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

// observationFixture uses a real CLI and store factory with hostile process
// substitutes. Even a transient start, failed recovery, or proxied bd read
// leaves a log entry; checking only survivors would miss those attempts.
type observationFixture struct {
	city, bin, provider, log string
	env                      []string
}

func newObservationFixture(t *testing.T, state string) observationFixture {
	t.Helper()
	city := t.TempDir()
	bin := filepath.Join(city, "bin")
	for _, dir := range []string{bin, filepath.Join(city, ".gc"), filepath.Join(city, ".beads", "dolt"), filepath.Join(city, "home"), filepath.Join(city, "runtime")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(city, "attempts")
	// No stub starts a real service. Failing health is intentionally enough to
	// trigger the currently recovering path and expose the forbidden attempt.
	body := "#!/bin/sh\nprintf '%s %s\\n' \"${0##*/}\" \"$*\" >> \"" + logPath + "\"\nprintf '%s\\n' 'dolt server unreachable: observation tripwire' >&2\nexit 1\n"
	provider := filepath.Join(bin, "gc-beads-bd.sh")
	for _, name := range []string{"bd", "dolt", "gc-beads-bd.sh", "tmux"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeCityFile(t, city, "city.toml", fmt.Sprintf("[workspace]\nname = \"observe-test\"\n[beads]\nprovider = %q\n[[agent]]\nname = \"observer\"\n", "exec:"+provider))
	mode := "server"
	if state == "proxied-stopped" {
		mode = "proxied-server"
	}
	writeScopeBeadsMetadata(t, city, fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":%q,"dolt_database":"hq"}`, mode))
	if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte("issue_prefix: hq\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state == "stale-publication" {
		if err := writeDoltState(city, doltRuntimeState{Running: true, PID: 2147483647, Port: 1, DataDir: filepath.Join(city, ".beads", "dolt")}); err != nil {
			t.Fatal(err)
		}
	}
	if state == "proxied-stopped" {
		if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := markProviderScopeOwnershipReady(city, city); err != nil {
			t.Fatal(err)
		}
	}
	f := observationFixture{city: city, bin: bin, provider: provider, log: logPath}
	usage := `{"type":"assistant","message":{"usage":{"input_tokens":10000,"cache_read_input_tokens":70000,"cache_creation_input_tokens":10000}}}` + "\n"
	writeCityFile(t, city, "usage.jsonl", usage)
	// TestMain's CLI re-exec defaults must not turn this into a FileStore or
	// skip Dolt. Everything that can locate a user's city is replaced here.
	f.env = sanitizedBaseEnv(
		"HOME="+filepath.Join(city, "home"), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BD_BIN="+filepath.Join(bin, "bd"),
		"GC_CITY="+city, "GC_HOME="+filepath.Join(city, "home"), "XDG_RUNTIME_DIR="+filepath.Join(city, "runtime"),
		"GC_BEADS=exec:"+provider, "GC_DOLT=enabled", "GC_BOOTSTRAP=skip", "GC_SESSION=fake",
		"GC_AGENT=observer", "GC_ALIAS=observer", "GC_TEMPLATE=observer", "GC_SESSION_NAME=observe-test--observer", "GC_SESSION_ID=hq-observer",
		"GC_BEADS_BD_SCRIPT="+provider, "BEADS_DOLT_AUTO_START=0", "BEADS_ROLE=maintainer",
		"GC_TEST_OBSERVATION_CITY="+city,
	)
	return f
}

func (f observationFixture) command(t *testing.T, gc string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	if err := os.Symlink(gc, filepath.Join(f.bin, "gc")); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*testutil.ExecRaceTimeout)
	defer cancel()
	cmd := testCommandContext(ctx, gc, append([]string{"--city", f.city}, args...)...)
	cmd.Dir, cmd.Env = f.city, f.env
	prepareProviderOpCommand(cmd)
	cmd.WaitDelay = 2 * time.Second
	if stdin == "usage-sample" {
		stdin = fmt.Sprintf(`{"hook_event_name":"UserPromptSubmit","transcript_path":%q}`, filepath.Join(f.city, "usage.jsonl"))
	}
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("command exceeded hang guard: %s: %v; stderr=%s", strings.Join(args, " "), ctx.Err(), stderr.String())
	}
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("execute gc: %v", err)
	}
	return exit.ExitCode(), stdout.String(), stderr.String()
}

func (f observationFixture) attempts(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Scope by a unique temp-city path, and keep PID plus command identity. A
// same-count replacement is a leak too. No process is signalled or cleaned up
// by this census; the hostile substitutes cannot start a server in RED.
func observationProcessCensus(t *testing.T, city string) []string {
	t.Helper()
	if _, err := os.Stat("/proc"); os.IsNotExist(err) {
		return observationPortableProcessCensus(t, city)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	var published doltRuntimeState
	if data, err := os.ReadFile(filepath.Join(city, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); err == nil {
		if err := json.Unmarshal(data, &published); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil { // Processes may exit between directory and cmdline reads.
			continue
		}
		command := strings.ReplaceAll(string(data), "\x00", " ")
		if strings.Contains(command, city) && (entry.Name() == strconv.Itoa(published.PID) || strings.Contains(command, "sql-server") || strings.Contains(command, "watchdog")) {
			found = append(found, entry.Name()+" "+command)
		}
	}
	slices.Sort(found)
	return found
}

func (f observationFixture) makeAvailable(t *testing.T) int {
	t.Helper()
	port := reserveRandomTCPPort(t)
	server := startTCPListenerProcessInDir(t, port, filepath.Join(f.city, ".beads", "dolt"))
	if err := writeDoltState(f.city, doltRuntimeState{Running: true, PID: server.Process.Pid, Port: port, DataDir: filepath.Join(f.city, ".beads", "dolt")}); err != nil {
		t.Fatal(err)
	}
	if currentManagedDoltPort(f.city) == "" {
		t.Fatal("fixture has no valid live managed publication; healthy control would be vacuous")
	}
	return server.Process.Pid
}

func (f observationFixture) applyEnv(t *testing.T) {
	t.Helper()
	clearGCEnv(t)
	for _, item := range f.env {
		key, value, ok := strings.Cut(item, "=")
		if ok && (strings.HasPrefix(key, "GC_") || strings.HasPrefix(key, "BEADS_") || key == "BD_BIN" || key == "PATH") {
			t.Setenv(key, value)
		}
	}
	oldHookRunExecutable := hookRunExecutable
	t.Cleanup(func() { hookRunExecutable = oldHookRunExecutable })
	configureObservationCLIForTests()
}

func TestObservationAvailableStoreControls(t *testing.T) {
	gc := reexecGCTestBinaryForTests(t)
	f := newObservationFixture(t, "stopped")
	f.applyEnv(t)
	f.makeAvailable(t)
	// Reads return an empty healthy store; any lifecycle verb remains a
	// failing tripwire. The listener proves the publication/ownership branch,
	// while the existing store conformance suites own bd's JSON protocol.
	body := "#!/bin/sh\nprintf '%s %s\\n' \"${0##*/}\" \"$*\" >> \"" + f.log + "\"\ncase \"$1\" in\nlist|ready|query|show) printf '[]\\n';;\ncount) printf '{\"count\":0}\\n';;\nversion) printf 'bd version 1.1.0\\n';;\n*) printf 'dolt server unreachable: unexpected control operation %s\\n' \"$*\" >&2; exit 1;;\nesac\n"
	if err := os.WriteFile(filepath.Join(f.bin, "bd"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	// BdStore, with the already-running managed server, avoids an exec-store
	// protocol double and exercises the native-off fallback explicitly.
	writeCityFile(t, f.city, "city.toml", "[workspace]\nname = \"observe-test\"\n[beads]\nprovider = \"bd\"\nnative_transport = \"off\"\n[[agent]]\nname = \"observer\"\n")
	for i, item := range f.env {
		if strings.HasPrefix(item, "GC_BEADS=") {
			f.env[i] = "GC_BEADS=bd"
		}
	}
	for _, tc := range []struct {
		args []string
		exit int
	}{
		{[]string{"status"}, 0},
		{[]string{"hook", "observer"}, 1},
		{[]string{"mail", "check", "human"}, 1},
		{[]string{"mail", "inbox", "human"}, 0},
	} {
		t.Run(strings.Join(tc.args, "-"), func(t *testing.T) {
			if err := os.Remove(f.log); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			before := observationProcessCensus(t, f.city)
			code, out, errOut := f.command(t, gc, "", tc.args...)
			after := observationProcessCensus(t, f.city)
			t.Logf("healthy empty store: code=%d stdout=%q stderr=%q attempts=%q", code, out, errOut, f.attempts(t))
			if code != tc.exit || strings.Contains(errOut, "store unavailable:") {
				t.Errorf("healthy empty store: exit=%d want=%d out=%q stderr=%q", code, tc.exit, out, errOut)
			}
			for _, line := range strings.Split(strings.TrimSpace(f.attempts(t)), "\n") {
				if line != "" && !strings.HasPrefix(line, "bd ") {
					t.Errorf("available observation ran lifecycle tripwire: %s", line)
				}
				for _, verb := range []string{"bd create ", "bd update ", "bd close ", "bd init "} {
					if strings.HasPrefix(line, verb) {
						t.Errorf("available observation mutated the store: %s", line)
					}
				}
			}
			if len(before) == 0 || !slices.Equal(before, after) {
				t.Errorf("available observation stopped or replaced another owner's server: before=%v after=%v", before, after)
			}
		})
	}
}

func TestObservationFederatedHookReportsOmittedScopes(t *testing.T) {
	gc := reexecGCTestBinaryForTests(t)
	for _, hasWork := range []bool{false, true} {
		t.Run(fmt.Sprintf("city-has-work=%t", hasWork), func(t *testing.T) {
			f := newObservationFixture(t, "stopped")
			f.applyEnv(t)
			f.makeAvailable(t)
			rig := filepath.Join(f.city, "rigs", "cold")
			if err := os.MkdirAll(filepath.Join(rig, ".beads", "dolt"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"cold"}`)
			if err := persistProviderScopeOwnership(f.city, rig, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
				t.Fatal(err)
			}
			if err := markProviderScopeOwnershipReady(f.city, rig); err != nil {
				t.Fatal(err)
			}
			writeCityFile(t, f.city, "city.toml", fmt.Sprintf("[workspace]\nname = \"observe-test\"\n[beads]\nprovider = \"bd\"\nnative_transport = \"off\"\n[[agent]]\nname = \"observer\"\nscope = \"city\"\n[[rigs]]\nname = \"cold\"\npath = %q\n", rig))
			for i, item := range f.env {
				if strings.HasPrefix(item, "GC_BEADS=") {
					f.env[i] = "GC_BEADS=bd"
				}
			}
			work := "[]"
			if hasWork {
				work = `[{"id":"hq-work","title":"available city work","type":"task","status":"open","priority":2}]`
			}
			writeCityFile(t, f.city, "work.json", work+"\n")
			// Cold-scope calls are visible even if the default query swallows
			// their error and substitutes []. The available city can still
			// contribute real work, without a running rig store.
			body := fmt.Sprintf("#!/bin/sh\nprintf 'bd %%s %%s\\n' \"${BEADS_DIR:-$PWD}\" \"$*\" >> %q\ncase \"${BEADS_DIR:-$PWD}\" in\n%q*) echo 'dolt server unreachable: cold rig tripwire' >&2; exit 1;;\nesac\ncase \"$1\" in\nready) cat %q;;\nlist|query|show) printf '[]\\n';;\nversion) printf 'bd version 1.1.0\\n';;\n*) echo 'dolt server unreachable: unexpected operation' >&2; exit 1;;\nesac\n", f.log, rig, filepath.Join(f.city, "work.json"))
			if err := os.WriteFile(filepath.Join(f.bin, "bd"), []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			before := observationProcessCensus(t, f.city)
			code, out, errOut := f.command(t, gc, "", "hook", "observer")
			want := 69
			if hasWork {
				want = 0
			}
			if code != want || !strings.Contains(errOut, "store unavailable: stopped scope=cold") {
				t.Errorf("partial federation: code=%d want=%d stdout=%q stderr=%q", code, want, out, errOut)
			}
			if hasWork && !strings.Contains(out, "hq-work") || !hasWork && out != "" {
				t.Errorf("partial federation work answer=%q, city has work=%t", out, hasWork)
			}
			if attempts := f.attempts(t); strings.Contains(attempts, rig) || strings.Contains(attempts, "gc-beads-bd.sh") {
				t.Errorf("federation entered a stopped scope or recovered the live scope: %s", attempts)
			}
			if after := observationProcessCensus(t, f.city); !slices.Equal(before, after) {
				t.Errorf("federation changed scoped process identities: before=%v after=%v", before, after)
			}
		})
	}
}

func TestObservationCustomHookQueryRunsWithRecoveryDisabled(t *testing.T) {
	gc := reexecGCTestBinaryForTests(t)
	f := newObservationFixture(t, "stopped")
	query := filepath.Join(f.bin, "custom-query")
	marker := filepath.Join(f.city, "custom-query-env")
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s %%s\\n' \"$BEADS_DOLT_AUTO_START\" \"$GC_STORE_SCOPE\" > %q\nprintf '[]\\n'\n", marker)
	if err := os.WriteFile(query, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCityFile(t, f.city, "city.toml", fmt.Sprintf("[workspace]\nname = \"observe-test\"\n[beads]\nprovider = %q\n[[agent]]\nname = \"observer\"\nwork_query = %q\n", "exec:"+f.provider, query))
	code, out, errOut := f.command(t, gc, "", "hook", "observer")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("operator's custom query was skipped on a stopped scope: %v; code=%d stdout=%q stderr=%q", err, code, out, errOut)
	}
	if string(data) != "0 city\n" || f.attempts(t) != "" {
		t.Errorf("custom-query environment=%q, want auto-start 0 and city; forbidden attempts=%q", data, f.attempts(t))
	}
}

func TestObservationHookRunPreservesUnavailableAndTimeoutContracts(t *testing.T) {
	old := hookRunExecutable
	hookRunExecutable = func() (string, error) { return exec.LookPath("sh") }
	t.Cleanup(func() { hookRunExecutable = old })
	for _, tc := range []struct {
		name, script string
		timeout      time.Duration
		want         int
	}{
		{"unavailable", "echo 'store unavailable: stopped scope=city' >&2; exit 69", testutil.ExecRaceTimeout, 69},
		{"timeout-mid-child", "printf partial; exec sleep 10", 100 * time.Millisecond, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := cmdHookRun([]string{"-c", tc.script}, hookRunOptions{Timeout: tc.timeout, TimeoutExitCode: 0}, strings.NewReader(`{"hook_event_name":"UserPromptSubmit"}`), &out, &errOut)
			if code != tc.want || out.Len() != 0 {
				t.Errorf("managed hook contract: code=%d want=%d stdout=%q stderr=%q", code, tc.want, out.String(), errOut.String())
			}
			if tc.want == 69 && !strings.Contains(errOut.String(), "store unavailable: stopped scope=city") || tc.want == 0 && !strings.Contains(errOut.String(), "timed out after") {
				t.Errorf("managed hook lost child-unavailable or wrapper-timeout diagnostic: %q", errOut.String())
			}
		})
	}
}

func TestObservationHookDeadlineReportsUnreachable(t *testing.T) {
	f := newObservationFixture(t, "stopped")
	f.applyEnv(t)
	f.makeAvailable(t)
	// A custom query must run even when a scope is unavailable. Its deadline
	// is the behavior under test; lowering the package var is the architect's
	// explicit alternative to waiting the production 150-second budget.
	writeCityFile(t, f.city, "city.toml", fmt.Sprintf("[workspace]\nname = \"observe-test\"\n[beads]\nprovider = %q\n[[agent]]\nname = \"observer\"\nwork_query = \"exec tail -f /dev/null\"\n", "exec:"+f.provider))
	oldTimeout := hookWorkQueryTimeout
	hookWorkQueryTimeout = 100 * time.Millisecond
	t.Cleanup(func() { hookWorkQueryTimeout = oldTimeout })
	var out, errOut bytes.Buffer
	code := cmdHookWithOptions([]string{"observer"}, hookCommandOptions{}, &out, &errOut)
	if code != 69 || out.Len() != 0 || !strings.Contains(errOut.String(), "store unavailable: unreachable scope=city") {
		t.Errorf("deadline was mistaken for no work: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if attempts := f.attempts(t); attempts != "" {
		t.Errorf("work-query timeout recovered a live endpoint: %s", attempts)
	}
}

func TestObservationExternalEndpointNeedsNoManagedPublication(t *testing.T) {
	gc := reexecGCTestBinaryForTests(t)
	f := newObservationFixture(t, "stopped")
	writeCityFile(t, f.city, "city.toml", "[workspace]\nname = \"observe-test\"\n[beads]\nprovider = \"bd\"\nnative_transport = \"off\"\n[dolt]\nhost = \"external-db.example.invalid\"\nport = 4406\n[[agent]]\nname = \"observer\"\n")
	writeCityFile(t, f.city, ".beads/config.yaml", "issue_prefix: hq\ngc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\ndolt.auto-start: false\ndolt.host: external-db.example.invalid\ndolt.port: 4406\n")
	for i, item := range f.env {
		if strings.HasPrefix(item, "GC_BEADS=") {
			f.env[i] = "GC_BEADS=bd"
		}
	}
	body := fmt.Sprintf("#!/bin/sh\nprintf 'bd %%s\\n' \"$*\" >> %q\ncase \"$1\" in\nlist|ready|query|show) printf '[]\\n';;\nversion) echo 'bd version 1.1.0';;\n*) echo 'unexpected external read' >&2; exit 1;;\nesac\n", f.log)
	if err := os.WriteFile(filepath.Join(f.bin, "bd"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := f.command(t, gc, "", "status")
	if code != 0 || strings.Contains(out+errOut, "store unavailable:") {
		t.Errorf("external store misclassified by missing managed publication: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if attempts := f.attempts(t); !strings.Contains(attempts, "bd list") || strings.Contains(attempts, "gc-beads-bd.sh") || strings.Contains(attempts, "dolt ") {
		t.Errorf("external control did not read normally without managed lifecycle: %q", attempts)
	}
}

func TestObservationStatusUnavailableWhileControllerRunningIsDegraded(t *testing.T) {
	for _, state := range []string{"stopped", "stale-publication"} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", state, jsonOut), func(t *testing.T) {
				f := newObservationFixture(t, state)
				f.applyEnv(t)
				old := controllerIdentityHook
				controllerIdentityHook = func(string) controllerIdentityReply {
					return controllerIdentityReply{PID: os.Getpid(), HostingMode: controllerHostingStandalone}
				}
				t.Cleanup(func() { controllerIdentityHook = old })
				cfg, err := loadCityConfig(f.city, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				var out, errOut bytes.Buffer
				code := cmdCityStatusLocalFallback(cfg, f.city, jsonOut, &out, &errOut)
				if code != 1 {
					t.Errorf("running controller with unavailable store must remain degraded: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
				}
				if jsonOut && (!strings.Contains(out.String(), "store_"+strings.ReplaceAll(state, "-", "_")) || strings.Contains(out.String(), "controller_not_running")) {
					t.Errorf("running-controller status has wrong health signals: %s", out.String())
				}
				if attempts := f.attempts(t); attempts != "" {
					t.Errorf("running-controller status recovered an unavailable store: %s", attempts)
				}
			})
		}
	}
}

func observationPortableProcessCensus(t *testing.T, city string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	defer cancel()
	data, err := testCommandContext(ctx, "ps", "-axo", "pid=,args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	var published doltRuntimeState
	if data, err := os.ReadFile(filepath.Join(city, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); err == nil {
		if err := json.Unmarshal(data, &published); err != nil {
			t.Fatal(err)
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.Contains(line, city) && (fields[0] == strconv.Itoa(published.PID) || strings.Contains(line, "sql-server") || strings.Contains(line, "watchdog")) {
			found = append(found, strings.TrimSpace(line))
		}
	}
	slices.Sort(found)
	return found
}

func assertObservationUnavailableJSON(t *testing.T, output, state string) {
	t.Helper()
	var got jsonSchemaErrorPayload
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Errorf("unavailable JSON is not an error envelope: %v; output=%q", err, output)
		return
	}
	if got.SchemaVersion != "1" || got.OK || got.Error.Code != "store_unavailable" || got.Error.ExitCode != 69 || !strings.Contains(got.Error.Message, state) {
		t.Errorf("unavailable JSON = %+v, want schema 1, ok=false, store_unavailable, exit 69 and %s", got, state)
	}
}

func TestObservationCLILeavesStoppedStoresCold(t *testing.T) {
	gc := reexecGCTestBinaryForTests(t)
	cases := []struct {
		name      string
		args      []string
		stdin     string
		exit      int
		jsonError bool
	}{
		{"status", []string{"status"}, "", 0, false},
		{"status-json", []string{"status", "--json"}, "", 0, false},
		{"doctor", []string{"doctor", "--check", "controller,agent-sessions,order-firing-current,skill-dangling-sink,dolt-server"}, "", 0, false},
		{"hook", []string{"hook", "observer"}, "", 69, false},
		{"hook-json", []string{"hook", "observer", "--json"}, "", 69, true},
		{"hook-current", []string{"hook", "current", "--id-only"}, "", 69, false},
		{"mail-check", []string{"mail", "check"}, "", 69, false},
		{"mail-check-inject", []string{"mail", "check", "--inject"}, "", 0, false},
		{"mail-inbox", []string{"mail", "inbox"}, "", 69, false},
		{"mail-inbox-json", []string{"mail", "inbox", "--json"}, "", 69, true},
		{"mail-peek", []string{"mail", "peek", "hq-message"}, "", 69, false},
		{"mail-peek-json", []string{"mail", "peek", "hq-message", "--json"}, "", 69, true},
		{"nudge-drain", []string{"nudge", "drain", "--inject"}, "", 0, false},
		{"nudge-drain-usage", []string{"nudge", "drain", "--inject"}, "usage-sample", 0, false},
		{"nudge-status", []string{"nudge", "status", "observer"}, "", 69, false},
		{"nudge-poll", []string{"nudge", "poll", "observer"}, "", 69, false},
		{"prime-hook", []string{"prime", "--hook"}, "", 0, false},
		{"hook-run-exit", []string{"hook", "run", "--", "mail", "check"}, "", 69, false},
	}
	for _, topology := range []string{"stopped", "stale-publication", "proxied-stopped", "unreachable"} {
		for _, tc := range cases {
			t.Run(topology+"/"+tc.name, func(t *testing.T) {
				f := newObservationFixture(t, topology)
				args, wantExit := slices.Clone(tc.args), tc.exit
				if topology == "unreachable" {
					f.applyEnv(t)
					f.makeAvailable(t)
					if strings.HasPrefix(tc.name, "status") {
						wantExit = 1
					}
					if tc.name == "doctor" {
						args[2] += ",bead-store-preflight"
						wantExit = 1
					}
				}
				before := observationProcessCensus(t, f.city)
				publicationPath := filepath.Join(f.city, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
				publicationBefore, _ := os.ReadFile(publicationPath)
				code, out, errOut := f.command(t, gc, tc.stdin, args...)
				attempts := f.attempts(t)
				after := observationProcessCensus(t, f.city)
				t.Logf("state=%s command=%v rc=%d processes_before=%v processes_after=%v attempts=%q stdout=%q stderr=%q", topology, tc.args, code, before, after, attempts, out, errOut)
				if topology != "unreachable" && attempts != "" {
					t.Errorf("observation invoked a lifecycle/provider/bd process on an unavailable store:\n%s", attempts)
				}
				if topology == "unreachable" {
					for _, line := range strings.Split(attempts, "\n") {
						if strings.HasPrefix(line, "dolt ") || strings.HasPrefix(line, "tmux ") || strings.HasPrefix(line, "gc-beads-bd.sh health") || strings.HasPrefix(line, "gc-beads-bd.sh recover") {
							t.Errorf("unreachable read ran lifecycle helper: %s", line)
						}
					}
				}
				if !slices.Equal(before, after) {
					t.Errorf("scoped server/watchdog identities changed: before=%v after=%v", before, after)
				}
				publicationAfter, _ := os.ReadFile(publicationPath)
				if !bytes.Equal(publicationBefore, publicationAfter) {
					t.Errorf("observation rewrote its runtime publication: before=%s after=%s", publicationBefore, publicationAfter)
				}
				if code != wantExit {
					t.Errorf("exit=%d, want %d", code, wantExit)
				}
				state := strings.TrimPrefix(topology, "proxied-")
				if tc.name == "status-json" {
					if !strings.Contains(out, "store_"+strings.ReplaceAll(state, "-", "_")) || !strings.Contains(out, "controller_not_running") {
						t.Errorf("status JSON lacks unavailable and controller health signals: %s", out)
					}
				} else if tc.name == "status" {
					if !strings.Contains(out+errOut, state) || state != "unreachable" && !strings.Contains(out, "store not running") {
						t.Errorf("status does not disclose store state and unavailable named-session count: %s%s", out, errOut)
					}
				} else if tc.name == "doctor" {
					if state != "unreachable" && !strings.Contains(out, "not checked: store") || !strings.Contains(out, "controller") || state != "unreachable" && strings.Contains(out, "reachable on") {
						t.Errorf("doctor failed store omission or independent controller diagnostic: %s", out)
					}
				} else if !strings.Contains(errOut, "store unavailable: "+state+" scope=city") {
					t.Errorf("stderr lacks typed unavailable diagnostic: %q", errOut)
				}
				if state == "stale-publication" && !strings.Contains(out+errOut, "2147483647") {
					t.Errorf("stale publication diagnostic omitted the dead published pid")
				}
				if tc.jsonError {
					assertObservationUnavailableJSON(t, out, state)
				}
				if tc.name == "hook" || tc.name == "hook-current" || strings.HasPrefix(tc.name, "nudge-drain") {
					if out != "" {
						t.Errorf("unavailable observation fabricated hook context/work: %q", out)
					}
				}
				if tc.name == "mail-check-inject" && (!strings.Contains(out, "<system-reminder>") || !strings.Contains(out, "degraded") || !strings.Contains(out, state)) {
					t.Errorf("inject did not disclose degraded unavailable read: %q", out)
				}
			})
		}
	}
}

func TestObservationIdentitylessHookControls(t *testing.T) {
	gc := reexecGCTestBinaryForTests(t)
	for _, args := range [][]string{{"mail", "check", "--inject"}, {"nudge", "drain", "--inject"}, {"hook", "--inject"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			f := newObservationFixture(t, "stopped")
			var env []string
			for _, item := range f.env {
				if strings.HasPrefix(item, "GC_AGENT=") || strings.HasPrefix(item, "GC_ALIAS=") || strings.HasPrefix(item, "GC_TEMPLATE=") || strings.HasPrefix(item, "GC_SESSION_ID=") || strings.HasPrefix(item, "GC_SESSION_NAME=") {
					continue
				}
				env = append(env, item)
			}
			f.env = env
			code, out, errOut := f.command(t, gc, "", args...)
			if code != 0 || out != "" || f.attempts(t) != "" {
				t.Fatalf("identity-less control: code=%d out=%q stderr=%q attempts=%q", code, out, errOut, f.attempts(t))
			}
		})
	}
}

func TestObservationRejectsLiveButUnownedOrMismatchedPublication(t *testing.T) {
	gc := reexecGCTestBinaryForTests(t)
	for _, state := range []string{"unowned-pid", "mismatched-data-dir"} {
		t.Run(state, func(t *testing.T) {
			f := newObservationFixture(t, "stopped")
			f.applyEnv(t)
			f.makeAvailable(t)
			publication, err := readDoltRuntimeStateFile(filepath.Join(f.city, ".gc", "runtime", "packs", "dolt", "dolt-state.json"))
			if err != nil {
				t.Fatalf("read healthy fixture publication: %v", err)
			}
			if state == "unowned-pid" {
				publication.PID = os.Getpid()
			} else {
				publication.DataDir = filepath.Join(f.city, "different-data")
			}
			if err := writeDoltState(f.city, publication); err != nil {
				t.Fatal(err)
			}
			before := observationProcessCensus(t, f.city)
			code, out, errOut := f.command(t, gc, "", "hook", "observer")
			if code != 69 || out != "" || !strings.Contains(errOut, "store unavailable: stale-publication scope=city") {
				t.Errorf("invalid live publication trusted: code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			if attempts := f.attempts(t); attempts != "" {
				t.Errorf("invalid live publication reached store/lifecycle: %s", attempts)
			}
			if after := observationProcessCensus(t, f.city); !slices.Equal(before, after) {
				t.Errorf("invalid publication changed another process: before=%v after=%v", before, after)
			}
		})
	}
}

func TestObservationDoctorGatesManagedStoppedAndStale(t *testing.T) {
	for _, state := range []string{"stopped", "stale-publication"} {
		for _, controllerRunning := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/controller=%t", state, controllerRunning), func(t *testing.T) {
				f := newObservationFixture(t, state)
				gate := newDoctorStoreGate(controllerRunning, nil)
				opened := false
				_, err := gate.StoreFactory(func(string) (beads.Store, error) { opened = true; return beads.NewMemStore(), nil })(f.city)
				if err == nil || opened {
					t.Errorf("doctor attempted to open unavailable managed store: opened=%t err=%v", opened, err)
				}
				check := gate.StandIn("agent-sessions", []string{f.city}, []string{"city"})
				if check == nil {
					t.Fatal("doctor omitted no stand-in for an unavailable managed store")
				}
				result := check.Run(&doctor.CheckContext{})
				wantStatus := doctor.StatusOK
				if controllerRunning || state == "stale-publication" {
					wantStatus = doctor.StatusWarning
				}
				if result.Status != wantStatus || !strings.Contains(result.Message, "not checked: store") {
					t.Errorf("stand-in=%+v, want status=%v and store omission", result, wantStatus)
				}
			})
		}
	}
}

func TestObservationHookEnvironmentNeverRecovers(t *testing.T) {
	f := newObservationFixture(t, "stopped")
	f.applyEnv(t)
	old := recoverManagedBDCommand
	t.Cleanup(func() { recoverManagedBDCommand = old })
	recoverManagedBDCommand = func(string) error {
		t.Error("observation reached the managed retry recovery callback")
		return errors.New("forbidden observation recovery")
	}
	cfg, err := loadCityConfig(f.city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, err = hookQueryEnv(f.city, cfg, &config.Agent{Name: "observer"})
	if attempts := f.attempts(t); attempts != "" {
		t.Errorf("hook environment recovered an unavailable scope: %s", attempts)
	}
	if err == nil || !strings.Contains(err.Error(), "store unavailable: stopped scope=city") {
		t.Errorf("hook environment error=%v, want typed stopped diagnostic", err)
	}
}

func TestObservationNativeAndFallbackOpenStayCold(t *testing.T) {
	for _, native := range []string{"auto", "off"} {
		t.Run(native, func(t *testing.T) {
			f := newObservationFixture(t, "stopped")
			f.applyEnv(t)
			t.Setenv("GC_BEADS", "bd")
			t.Setenv("GC_BEADS_ALLOW_SCHEMA_BEHIND_MIGRATE", "1")
			writeCityFile(t, f.city, "city.toml", fmt.Sprintf("[workspace]\nname = \"observe-test\"\n[beads]\nprovider = \"bd\"\nnative_transport = %q\n", native))
			oldFactory := openStoreFactoryForCity
			t.Cleanup(func() { openStoreFactoryForCity = oldFactory })
			opened := 0
			openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
				opened++
				// If the classifier is misplaced, force both lanes instead of
				// letting the host version gate hide a recovering native open.
				for _, open := range []func() (beads.Store, error){opts.OpenNativeStore, opts.OpenBdStore, opts.OpenExecStore} {
					if open != nil {
						_, _ = open()
					}
				}
				return beads.StoreOpenResult{}, errors.New("unavailable fixture")
			}
			_, _, _ = openCityStatusStore(f.city, io.Discard)
			if opened != 0 || f.attempts(t) != "" {
				t.Errorf("native/fallback factory reached before stopped classification: opens=%d attempts=%q", opened, f.attempts(t))
			}
		})
	}
}

func TestObservationPollerDoesNotRecoverAfterSuccessfulResolve(t *testing.T) {
	f := newObservationFixture(t, "stopped")
	f.applyEnv(t)
	f.makeAvailable(t)
	store := beads.NewMemStore()
	info, err := store.Create(beads.Bead{
		Type: session.BeadType, Labels: []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "observe-test--observer", "agent_name": "observer", "template": "observer", "provider": "claude", "work_dir": f.city},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldOpen := openNudgeBeadStore
	t.Cleanup(func() { openNudgeBeadStore = oldOpen })
	resolved := 0
	openNudgeBeadStore = func(string) beads.NudgesStore {
		resolved++
		// Resolution succeeds on its existing handle, while the next open
		// sees the stopped publication. Never signal a server we do not own.
		if err := writeDoltState(f.city, doltRuntimeState{Running: false}); err != nil {
			t.Fatal(err)
		}
		return beads.NudgesStore{Store: store}
	}
	var out, errOut bytes.Buffer
	code := cmdNudgePoll([]string{info.ID}, "", time.Second, time.Second, true, &out, &errOut)
	if resolved == 0 {
		t.Fatal("fixture never exercised successful target resolution")
	}
	if code != 69 || !strings.Contains(errOut.String(), "store unavailable: stopped scope=city") || f.attempts(t) != "" {
		t.Errorf("post-resolve poller recovered: code=%d stderr=%q attempts=%q", code, errOut.String(), f.attempts(t))
	}
}

func TestObservationDoctorOutageGatesConstructorAndLazyReads(t *testing.T) {
	f := newObservationFixture(t, "stopped")
	f.applyEnv(t)
	f.makeAvailable(t)
	if err := os.MkdirAll(filepath.Join(f.city, "orders"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCityFile(t, f.city, "orders/observe-history.toml", "[order]\nexec = \"true\"\ntrigger = \"cron\"\nschedule = \"0 */4 * * *\"\n")
	cfg, err := loadCityConfig(f.city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	oldPreflight, oldOpen := doctorBeadStorePreflight, openSessionProviderStore
	t.Cleanup(func() { doctorBeadStorePreflight, openSessionProviderStore = oldPreflight, oldOpen })
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error {
		return errors.New("dolt server unreachable: bounded preflight deadline exceeded")
	}
	providerReads := 0
	openSessionProviderStore = func(string) (beads.Store, error) {
		providerReads++
		return nil, errors.New("dolt server unreachable: forbidden post-outage snapshot")
	}
	checks := buildDoctorChecks(f.city, cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck: true, SkipManagedDoltCheck: true, SkipRigDoltChecks: true,
	})
	if r := runDoctorCheckNamed(t, checks, "bead-store-preflight"); r.Status != doctor.StatusError {
		t.Errorf("unreachable preflight was hidden: %+v", r)
	}
	// Static sink inspection and the controller diagnostic remain usable, but
	// neither may query live sinks after the shared preflight has failed.
	_ = runDoctorCheckNamed(t, checks, "controller")
	_ = runDoctorCheckNamed(t, checks, "skill-dangling-sink")
	if providerReads != 0 {
		t.Errorf("doctor opened %d constructor/lazy session snapshots despite unavailable preflight; those reads can outlive the preflight/check deadline", providerReads)
	}
	for _, check := range checks {
		if check.Name() == "order-firing-current" {
			result := check.Run(&doctor.CheckContext{CityPath: f.city})
			if !strings.Contains(result.Message, "not checked") {
				t.Errorf("outage left the order-history read enabled: %+v", result)
			}
		}
	}
}

func TestObservationDoctorSnapshotCannotOutliveReadBudgets(t *testing.T) {
	gc := reexecGCTestBinaryForTests(t)
	f := newObservationFixture(t, "stopped")
	f.applyEnv(t)
	f.makeAvailable(t)
	writeCityFile(t, f.city, "city.toml", "[workspace]\nname = \"observe-test\"\n[beads]\nprovider = \"bd\"\nnative_transport = \"off\"\n[[agent]]\nname = \"observer\"\n")
	for i, item := range f.env {
		if strings.HasPrefix(item, "GC_BEADS=") {
			f.env[i] = "GC_BEADS=bd"
		}
	}
	// The published process remains alive and its TCP port accepts connects,
	// but a store read stalls: the same observation shape as a wedged server.
	// The delay exceeds the 5s preflight and 3s snapshot budgets; a constructor
	// read outside those budgets must not hide behind a check's own timeout.
	body := fmt.Sprintf("#!/bin/sh\nprintf 'bd %%s\\n' \"$*\" >> %q\ncase \"$1\" in\nversion) echo 'bd version 1.1.0'; exit 0;;\ncontext) echo 'dolt server unreachable' >&2; exit 1;;\nesac\nsleep 10\necho 'dolt server unreachable: stalled read' >&2\nexit 1\n", f.log)
	if err := os.WriteFile(filepath.Join(f.bin, "bd"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	before := observationProcessCensus(t, f.city)
	started := time.Now()
	code, out, errOut := f.command(t, gc, "", "doctor", "--check", "controller,bead-store-preflight,skill-dangling-sink", "--check-timeout", "100ms")
	elapsed := time.Since(started)
	if elapsed > 10*time.Second {
		t.Errorf("doctor constructor/lazy reads escaped existing budgets: elapsed=%s, preflight=5s and snapshot=3s", elapsed)
	}
	if code != 1 || !strings.Contains(out+errOut, "unreachable") || !strings.Contains(out, "controller") {
		t.Errorf("bounded outage lost preflight failure or independent checks: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if attempts := f.attempts(t); strings.Contains(attempts, "gc-beads-bd.sh") || strings.Contains(attempts, "dolt ") {
		t.Errorf("stalled read attempted recovery: %s", attempts)
	}
	if after := observationProcessCensus(t, f.city); !slices.Equal(before, after) {
		t.Errorf("doctor replaced a wedged live server: before=%v after=%v", before, after)
	}
}
