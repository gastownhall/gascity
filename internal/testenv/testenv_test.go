package testenv_test

import (
	"errors"
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
			cmd := exec.Command(tc.bin, "-test.run=^TestInitRefusesProdDoltPort$", "-test.v")
			cmd.Env = append([]string{"GC_TESTENV_CHILD=1"}, tc.env...)
			out, err := cmd.Output()
			if tc.wantPanic {
				if err == nil {
					t.Fatalf("child succeeded but should have refused the prod Dolt port; output:\n%s", out)
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
			for _, want := range tc.wantOutput {
				if !strings.Contains(string(out), want) {
					t.Errorf("child output missing %q; got:\n%s", want, out)
				}
			}
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
