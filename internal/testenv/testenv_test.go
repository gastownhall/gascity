package testenv_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testenv"
)

// TestInitScrubsLeakVectors verifies init() unsets every var in
// LeakVectorVars. Done by re-execing this test binary with the leak vars
// pre-set in env, then asking the child to report what it sees.
func TestInitScrubsLeakVectors(t *testing.T) {
	if os.Getenv("GC_TESTENV_CHILD") == "1" {
		// Child: report current values of leak-vector vars (init() should have
		// scrubbed them) plus a known-allowed var (should survive).
		var lines []string
		for _, name := range testenv.LeakVectorVars {
			lines = append(lines, name+"="+os.Getenv(name))
		}
		lines = append(lines, "GC_FAST_UNIT="+os.Getenv("GC_FAST_UNIT"))
		os.Stdout.WriteString(strings.Join(lines, "\n") + "\n") //nolint:errcheck
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestInitScrubsLeakVectors$", "-test.v")
	cmd.Env = []string{
		"GC_TESTENV_CHILD=1",
		"GC_FAST_UNIT=should-survive",
	}
	for _, name := range testenv.LeakVectorVars {
		cmd.Env = append(cmd.Env, name+"=leaked-"+name)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("re-exec: %v\nstderr: %s", err, exitStderr(err))
	}
	got := string(out)
	for _, name := range testenv.LeakVectorVars {
		if strings.Contains(got, name+"=leaked-"+name) {
			t.Errorf("%s not scrubbed; child output:\n%s", name, got)
		}
	}
	if !strings.Contains(got, "GC_FAST_UNIT=should-survive") {
		t.Errorf("GC_FAST_UNIT was scrubbed but should not be; child output:\n%s", got)
	}
}

// TestInitPassthroughPreservesNamed verifies that GC_TESTENV_PASSTHROUGH
// preserves the named leak-vector vars, scrubs the rest, and unsets itself.
func TestInitPassthroughPreservesNamed(t *testing.T) {
	if os.Getenv("GC_TESTENV_CHILD") == "1" {
		// Child: report current values of leak-vector vars plus the passthrough
		// var itself (which init() should have unset).
		var lines []string
		for _, name := range testenv.LeakVectorVars {
			lines = append(lines, name+"="+os.Getenv(name))
		}
		lines = append(lines, testenv.PassthroughVar+"="+os.Getenv(testenv.PassthroughVar))
		os.Stdout.WriteString(strings.Join(lines, "\n") + "\n") //nolint:errcheck
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	keep := []string{"GC_CITY", "GC_CITY_PATH"}
	cmd := exec.Command(exe, "-test.run=^TestInitPassthroughPreservesNamed$", "-test.v")
	cmd.Env = []string{
		"GC_TESTENV_CHILD=1",
		testenv.PassthroughVar + "=" + strings.Join(keep, ","),
	}
	for _, name := range testenv.LeakVectorVars {
		cmd.Env = append(cmd.Env, name+"=seeded-"+name)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("re-exec: %v\nstderr: %s", err, exitStderr(err))
	}
	got := string(out)
	kept := map[string]bool{}
	for _, name := range keep {
		kept[name] = true
		if !strings.Contains(got, name+"=seeded-"+name) {
			t.Errorf("%s not preserved by passthrough; child output:\n%s", name, got)
		}
	}
	for _, name := range testenv.LeakVectorVars {
		if kept[name] {
			continue
		}
		if strings.Contains(got, name+"=seeded-"+name) {
			t.Errorf("%s survived scrub despite not being in passthrough; child output:\n%s", name, got)
		}
	}
	if !strings.Contains(got, testenv.PassthroughVar+"=\n") {
		t.Errorf("%s not unset by init(); child output:\n%s", testenv.PassthroughVar, got)
	}
}

// TestInitSkipsScrubInTestscriptSubcommandMode verifies init() does NOT scrub
// when the binary is invoked under a non-`.test` name, simulating the
// testscript.Main subcommand re-invocation (e.g. binary copied to $PATH/bin/gc).
// Done by copying the test binary to a non-`.test` name then re-execing it.
func TestInitSkipsScrubInTestscriptSubcommandMode(t *testing.T) {
	if os.Getenv("GC_TESTENV_CHILD") == "1" {
		var lines []string
		for _, name := range testenv.LeakVectorVars {
			lines = append(lines, name+"="+os.Getenv(name))
		}
		os.Stdout.WriteString(strings.Join(lines, "\n") + "\n") //nolint:errcheck
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	// Copy the test binary to a non-`.test` name in a temp dir, so
	// filepath.Base(os.Args[0]) lacks the `.test` suffix that triggers scrub.
	fakeGC := filepath.Join(t.TempDir(), "gc")
	if err := copyFile(exe, fakeGC); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	cmd := exec.Command(fakeGC, "-test.run=^TestInitSkipsScrubInTestscriptSubcommandMode$", "-test.v")
	cmd.Env = []string{
		"GC_TESTENV_CHILD=1",
	}
	for _, name := range testenv.LeakVectorVars {
		cmd.Env = append(cmd.Env, name+"=kept-"+name)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("re-exec: %v\nstderr: %s", err, exitStderr(err))
	}
	got := string(out)
	for _, name := range testenv.LeakVectorVars {
		if !strings.Contains(got, name+"=kept-"+name) {
			t.Errorf("%s was scrubbed but should survive in subcommand mode; child output:\n%s", name, got)
		}
	}
}

// TestInitRefusesProdDoltPort verifies init() refuses to let a Dolt port var
// pointing at the production Dolt server (local host, port 3307) survive into
// a test process. The guard fires only for values that would outlive the
// scrub — passthrough-preserved vars in go-test mode, and all vars in
// testscript subcommand mode (where the scrub is skipped) — and only when the
// effective Dolt host is local (unset, scrubbed, localhost, loopback, or
// unspecified, including bracketed IPv6 forms like "[::1]"). Port values are
// matched numerically the way consumers parse them, so "03307" and "+3307"
// also refuse while unparsable values pass through. BEADS_DOLT_PORT feeds
// multiple beads code paths — a legacy server-port alias as well as
// local/auto-start inputs — so no env pairing proves its effective host; the
// guard fails closed, treats it as implicitly local, and no surviving host
// value disarms it. External hosts on 3307 (Dolt's default port) are
// legitimate fixtures. Setting GC_ALLOW_PROD_DOLT_PORT_IN_TESTS=1 opts out
// for the rare legitimate case.
func TestInitRefusesProdDoltPort(t *testing.T) {
	if os.Getenv("GC_TESTENV_CHILD") == "1" {
		// Child: report the Dolt host/port vars as the parent's env shaped them.
		var lines []string
		for _, name := range []string{"BEADS_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT"} {
			lines = append(lines, name+"="+os.Getenv(name))
		}
		os.Stdout.WriteString(strings.Join(lines, "\n") + "\n") //nolint:errcheck
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	// Copy of the test binary under a non-`.test` name, simulating the
	// testscript.Main subcommand re-invocation where the scrub is skipped.
	fakeGC := filepath.Join(t.TempDir(), "gc")
	if err := copyFile(exe, fakeGC); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	cases := []struct {
		name       string
		bin        string
		env        []string
		wantPanic  bool
		wantOutput []string
	}{
		{
			name: "passthrough BEADS_DOLT_SERVER_PORT prod port panics",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough GC_DOLT_PORT prod port panics",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=GC_DOLT_PORT",
				"GC_DOLT_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough zero-padded prod port panics",
			bin:  exe,
			env: []string{
				// Consumers parse the port with strconv.Atoi, so "03307"
				// reaches the production server just like "3307".
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_PORT=03307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough plus-signed prod port panics",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=GC_DOLT_PORT",
				"GC_DOLT_PORT=+3307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough non-prod port survives",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_PORT=3308",
			},
			wantOutput: []string{"BEADS_DOLT_SERVER_PORT=3308\n"},
		},
		{
			name: "passthrough unparsable port value survives",
			bin:  exe,
			env: []string{
				// strconv.Atoi rejects "3307x", so consumers never use it to
				// reach a server and the guard must not refuse it.
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_PORT=3307x",
			},
			wantOutput: []string{"BEADS_DOLT_SERVER_PORT=3307x\n"},
		},
		{
			name: "passthrough external host allows prod port",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_HOST,BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_HOST=city-db.example.com",
				"BEADS_DOLT_SERVER_PORT=3307",
			},
			wantOutput: []string{
				"BEADS_DOLT_SERVER_HOST=city-db.example.com\n",
				"BEADS_DOLT_SERVER_PORT=3307\n",
			},
		},
		{
			name: "passthrough loopback host refuses prod port",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_HOST,BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_HOST=127.0.0.1",
				"BEADS_DOLT_SERVER_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough bracketed IPv6 loopback host refuses prod port",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_HOST,BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_HOST=[::1]",
				"BEADS_DOLT_SERVER_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough bracketed unspecified host refuses prod port",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_HOST,BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_HOST=[::]",
				"BEADS_DOLT_SERVER_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough GC bracketed IPv6 loopback host refuses prod port",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=GC_DOLT_HOST,GC_DOLT_PORT",
				"GC_DOLT_HOST=[::1]",
				"GC_DOLT_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough GC bracketed unspecified host refuses prod port",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=GC_DOLT_HOST,GC_DOLT_PORT",
				"GC_DOLT_HOST=[::]",
				"GC_DOLT_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough port with scrubbed external host refuses prod port",
			bin:  exe,
			env: []string{
				// Host is set but not passthrough-listed: it will be
				// scrubbed, so the surviving client defaults to localhost.
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_HOST=city-db.example.com",
				"BEADS_DOLT_SERVER_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough external GC_DOLT_HOST allows prod GC_DOLT_PORT",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=GC_DOLT_HOST,GC_DOLT_PORT",
				"GC_DOLT_HOST=city-db.example.com",
				"GC_DOLT_PORT=3307",
			},
			wantOutput: []string{
				"GC_DOLT_HOST=city-db.example.com\n",
				"GC_DOLT_PORT=3307\n",
			},
		},
		{
			name: "passthrough BEADS_DOLT_PORT prod port panics",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_PORT",
				"BEADS_DOLT_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "passthrough BEADS_DOLT_PORT non-prod port survives",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_PORT",
				"BEADS_DOLT_PORT=3308",
			},
			wantOutput: []string{"BEADS_DOLT_PORT=3308\n"},
		},
		{
			name: "passthrough external host does not disarm BEADS_DOLT_PORT",
			bin:  exe,
			env: []string{
				// BEADS_DOLT_PORT feeds multiple beads code paths — a legacy
				// server-port alias as well as local/auto-start inputs — so no
				// env pairing proves its effective host; a surviving external
				// server host must not disarm its guard.
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_HOST,BEADS_DOLT_PORT",
				"BEADS_DOLT_SERVER_HOST=city-db.example.com",
				"BEADS_DOLT_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "opt-out allows prod port through passthrough",
			bin:  exe,
			env: []string{
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_PORT=3307",
				"GC_ALLOW_PROD_DOLT_PORT_IN_TESTS=1",
			},
			wantOutput: []string{"BEADS_DOLT_SERVER_PORT=3307\n"},
		},
		{
			name: "scrubbed prod port without passthrough does not panic",
			bin:  exe,
			env: []string{
				"BEADS_DOLT_SERVER_PORT=3307",
				"GC_DOLT_PORT=3307",
			},
			wantOutput: []string{"BEADS_DOLT_SERVER_PORT=\n", "GC_DOLT_PORT=\n"},
		},
		{
			name:      "subcommand mode refuses prod port",
			bin:       fakeGC,
			env:       []string{"BEADS_DOLT_SERVER_PORT=3307"},
			wantPanic: true,
		},
		{
			name:       "subcommand mode keeps non-prod port",
			bin:        fakeGC,
			env:        []string{"BEADS_DOLT_SERVER_PORT=3309"},
			wantOutput: []string{"BEADS_DOLT_SERVER_PORT=3309\n"},
		},
		{
			name: "subcommand mode external host keeps prod port",
			bin:  fakeGC,
			env: []string{
				"BEADS_DOLT_SERVER_HOST=city-db.example.com",
				"BEADS_DOLT_SERVER_PORT=3307",
			},
			wantOutput: []string{
				"BEADS_DOLT_SERVER_HOST=city-db.example.com\n",
				"BEADS_DOLT_SERVER_PORT=3307\n",
			},
		},
		{
			name: "subcommand mode localhost host refuses prod port",
			bin:  fakeGC,
			env: []string{
				"BEADS_DOLT_SERVER_HOST=localhost",
				"BEADS_DOLT_SERVER_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name: "subcommand mode bracketed IPv6 loopback host refuses prod port",
			bin:  fakeGC,
			env: []string{
				"BEADS_DOLT_SERVER_HOST=[::1]",
				"BEADS_DOLT_SERVER_PORT=3307",
			},
			wantPanic: true,
		},
		{
			name:      "subcommand mode BEADS_DOLT_PORT refuses prod port",
			bin:       fakeGC,
			env:       []string{"BEADS_DOLT_PORT=3307"},
			wantPanic: true,
		},
		{
			name:      "subcommand mode zero-padded BEADS_DOLT_PORT refuses prod port",
			bin:       fakeGC,
			env:       []string{"BEADS_DOLT_PORT=03307"},
			wantPanic: true,
		},
		{
			name: "subcommand mode opt-out allows prod port",
			bin:  fakeGC,
			env: []string{
				"BEADS_DOLT_SERVER_PORT=3307",
				"GC_ALLOW_PROD_DOLT_PORT_IN_TESTS=1",
			},
			wantOutput: []string{"BEADS_DOLT_SERVER_PORT=3307\n"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := append([]string{"GC_TESTENV_CHILD=1"}, tc.env...)
			assertRefusesDoltPort(t, tc.bin, "TestInitRefusesProdDoltPort", "", env, tc.wantPanic, tc.wantOutput)
		})
	}
}

// assertRefusesDoltPort re-execs bin under testName's -test.run pattern (with
// dir as its working directory, unless empty) and asserts either a panic —
// whose stderr must contain both diagnostic substrings — or, when wantPanic
// is false, that stdout contains every string in wantOutput. Shared by
// TestInitRefusesProdDoltPort and TestInitRefusesAmbientCityDoltPort so the
// hardcoded-port and ambient-city detection arms exercise a single re-exec
// call site instead of two, keeping this package's tracked subprocess census
// unchanged (see internal/testpolicy/resourcecensus and test/test-resources.toml).
func assertRefusesDoltPort(t *testing.T, bin, testName, dir string, env []string, wantPanic bool, wantOutput []string) {
	t.Helper()
	cmd := exec.Command(bin, "-test.run=^"+testName+"$", "-test.v")
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	out, err := cmd.Output()
	if wantPanic {
		if err == nil {
			t.Fatalf("child succeeded but should have refused the Dolt port; output:\n%s", out)
		}
		stderr := exitStderr(err)
		for _, want := range []string{"production Dolt server", "GC_ALLOW_PROD_DOLT_PORT_IN_TESTS"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("panic message missing %q; stderr:\n%s", want, stderr)
			}
		}
		return
	}
	if err != nil {
		t.Fatalf("re-exec: %v\nstderr: %s", err, exitStderr(err))
	}
	for _, want := range wantOutput {
		if !strings.Contains(string(out), want) {
			t.Errorf("child output missing %q; got:\n%s", want, out)
		}
	}
}

// buildSyntheticCity creates a t.TempDir() city tree — a city.toml marker at
// the root and a managed-Dolt runtime state file recording port — and
// returns a directory nested three levels below the root, so pointing
// cmd.Dir at it exercises the ambient arm's upward walk rather than a
// same-directory check. The synthetic root is a fresh temp dir, so the walk
// finds this city.toml before it could ever reach any real ambient city
// further up the real filesystem tree.
func buildSyntheticCity(t *testing.T, port int) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "city.toml"), []byte("# synthetic city\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	stateDir := filepath.Join(root, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir dolt state dir: %v", err)
	}
	state := fmt.Sprintf(`{"running":true,"pid":1,"port":%d,"data_dir":"x"}`, port)
	if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(state), 0o644); err != nil {
		t.Fatalf("write dolt-state.json: %v", err)
	}
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested dir: %v", err)
	}
	return nested
}

// TestInitRefusesAmbientCityDoltPort proves the ambient-city detection arm in
// refuseProdDoltPort is actually wired into init() end-to-end — not just
// correct as a pure function, which TestAmbientCityDoltPort in
// testenv_internal_test.go already covers against synthetic trees directly.
// The child's cmd.Dir is pointed at a synthetic city tree whose port is a
// neutral synthetic value — neither the hardcoded ProdDoltPort (3307) nor
// the fleet's real managed ambient port (28231) — so a panic here can only
// be caused by the ambient arm consulting that synthetic city's own state,
// never by a coincidental match against this process's real environment.
func TestInitRefusesAmbientCityDoltPort(t *testing.T) {
	if os.Getenv("GC_TESTENV_CHILD") == "1" {
		os.Stdout.WriteString("BEADS_DOLT_SERVER_PORT=" + os.Getenv("BEADS_DOLT_SERVER_PORT") + "\n") //nolint:errcheck
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	const ambientPort = 19999 // neither ProdDoltPort (3307) nor the fleet's real ambient port (28231)

	cases := []struct {
		name       string
		port       string
		extraEnv   []string
		wantPanic  bool
		wantOutput []string
	}{
		{
			name:      "surviving port matching ambient city port panics",
			port:      "19999",
			wantPanic: true,
		},
		{
			name:       "surviving port not matching ambient city port survives",
			port:       "20000",
			wantOutput: []string{"BEADS_DOLT_SERVER_PORT=20000\n"},
		},
		{
			name:       "opt-out allows ambient-matching port through",
			port:       "19999",
			extraEnv:   []string{"GC_ALLOW_PROD_DOLT_PORT_IN_TESTS=1"},
			wantOutput: []string{"BEADS_DOLT_SERVER_PORT=19999\n"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := buildSyntheticCity(t, ambientPort)
			env := append([]string{
				"GC_TESTENV_CHILD=1",
				"GC_TESTENV_PASSTHROUGH=BEADS_DOLT_SERVER_PORT",
				"BEADS_DOLT_SERVER_PORT=" + tc.port,
			}, tc.extraEnv...)
			assertRefusesDoltPort(t, exe, "TestInitRefusesAmbientCityDoltPort", dir, env, tc.wantPanic, tc.wantOutput)
		})
	}
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

func exitStderr(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(ee.Stderr)
	}
	return ""
}
