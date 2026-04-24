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
