package systemdscope

import (
	"errors"
	"strings"
	"testing"
)

func TestArgvWrapsCommand(t *testing.T) {
	got := Argv("gcdolt.slice", []string{"dolt", "sql-server", "--config", "/tmp/c.yaml"})
	want := []string{
		"systemd-run", "--user", "--scope", "--slice=gcdolt.slice", "--collect", "--quiet", "--",
		"dolt", "sql-server", "--config", "/tmp/c.yaml",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Argv = %q, want %q", got, want)
	}
}

func TestArgvPassesThroughWhenSliceOrCommandEmpty(t *testing.T) {
	cmd := []string{"dolt", "sql-server"}
	if got := Argv("", cmd); strings.Join(got, "\x00") != strings.Join(cmd, "\x00") {
		t.Fatalf("empty slice: Argv = %q, want %q", got, cmd)
	}
	if got := Argv("gcdolt.slice", nil); len(got) != 0 {
		t.Fatalf("empty command: Argv = %q, want empty", got)
	}
}

// TestArgvPreservesPIDContract pins the flag set that makes wrapping safe for
// PID-tracked children: `--scope` makes systemd-run exec in place (the wrapped
// process keeps the PID the caller observed), and `--quiet` keeps systemd-run
// off the child's stdout, where the managed-dolt watchdog handshake lives.
func TestArgvPreservesPIDContract(t *testing.T) {
	got := strings.Join(Argv("s.slice", []string{"true"}), " ")
	for _, flag := range []string{"--scope", "--quiet", "--collect", "--user"} {
		if !strings.Contains(got, flag) {
			t.Errorf("Argv missing %s: %q", flag, got)
		}
	}
}

func TestWrapperFallsBackWhenProbeFails(t *testing.T) {
	warn := &strings.Builder{}
	w := &Wrapper{
		Probe: func(string) error { return errors.New("no user bus") },
		Warn:  warn,
		Label: "GC_DOLT_SLICE",
	}
	cmd := []string{"dolt", "sql-server"}
	got := w.Wrap("gcdolt.slice", cmd)
	if strings.Join(got, "\x00") != strings.Join(cmd, "\x00") {
		t.Fatalf("Wrap on probe failure = %q, want unwrapped %q", got, cmd)
	}
	if !strings.Contains(warn.String(), "GC_DOLT_SLICE") {
		t.Errorf("warning does not name the knob: %q", warn.String())
	}
	if !strings.Contains(warn.String(), "no user bus") {
		t.Errorf("warning drops the probe cause: %q", warn.String())
	}
}

func TestWrapperReprobesAfterAFailureWindow(t *testing.T) {
	calls := 0
	w := &Wrapper{
		Probe: func(string) error { calls++; return errors.New("bus down") },
		Warn:  &strings.Builder{}, Label: "GC_DOLT_SLICE",
	}

	if w.Available("s.slice") {
		t.Fatal("probe failed but Available reported true")
	}
	if w.Available("s.slice"); calls != 1 {
		t.Fatalf("re-probed inside the failure window: %d calls, want 1", calls)
	}

	// A long-lived process must not be stuck with a stale failure verdict.
	previous := failureRetryInterval
	failureRetryInterval = 0
	t.Cleanup(func() { failureRetryInterval = previous })
	if w.Available("s.slice"); calls != 2 {
		t.Fatalf("did not re-probe after the failure window: %d calls, want 2", calls)
	}
}

func TestWrapperCachesPerSlice(t *testing.T) {
	probed := []string{}
	w := &Wrapper{
		Probe: func(slice string) error { probed = append(probed, slice); return nil },
		Warn:  &strings.Builder{}, Label: "GC_DOLT_SLICE",
	}

	w.Available("a.slice")
	w.Available("b.slice")
	w.Available("a.slice")

	if strings.Join(probed, ",") != "a.slice,b.slice" {
		t.Fatalf("probed %v, want each slice probed exactly once", probed)
	}
}

func TestWrapperProbesAtMostOnce(t *testing.T) {
	calls := 0
	w := &Wrapper{
		Probe: func(string) error { calls++; return nil },
		Warn:  &strings.Builder{},
		Label: "GC_DOLT_SLICE",
	}
	for range 3 {
		if got := w.Wrap("gcdolt.slice", []string{"true"}); got[0] != "systemd-run" {
			t.Fatalf("Wrap did not wrap: %q", got)
		}
	}
	if calls != 1 {
		t.Fatalf("probe ran %d times, want 1", calls)
	}
}

func TestWrapperSkipsProbeWhenSliceEmpty(t *testing.T) {
	calls := 0
	w := &Wrapper{Probe: func(string) error { calls++; return nil }, Warn: &strings.Builder{}}
	if got := w.Wrap("", []string{"true"}); len(got) != 1 || got[0] != "true" {
		t.Fatalf("Wrap = %q, want passthrough", got)
	}
	if calls != 0 {
		t.Fatalf("probe ran %d times for an empty slice, want 0", calls)
	}
}
