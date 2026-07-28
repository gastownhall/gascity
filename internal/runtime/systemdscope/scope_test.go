package systemdscope

import (
	"context"
	"errors"
	"reflect"
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

func TestWrapperWrapRequiredFailsClosedWhenProbeFails(t *testing.T) {
	w := &Wrapper{
		Probe: func(string) error { return errors.New("user bus unavailable") },
		Warn:  &strings.Builder{},
		Label: "GC_DOLT_SLICE",
	}

	got, err := w.WrapRequired("gcdolt.slice", []string{"dolt", "sql-server"})
	if err == nil {
		t.Fatalf("WrapRequired error = nil, got %q", got)
	}
	if len(got) != 0 {
		t.Fatalf("WrapRequired returned fallback argv %q, want none", got)
	}
	if !strings.Contains(err.Error(), "user bus unavailable") {
		t.Fatalf("WrapRequired error = %q, want probe cause", err)
	}
}

func TestManagerEnsureSliceAppliesAndVerifiesRequiredPolicy(t *testing.T) {
	var calls [][]string
	manager := Manager{
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			if name == "systemctl" && len(args) > 1 && args[1] == "show" {
				return []byte("MemoryMax=12884901888\nMemoryLow=2147483648\nManagedOOMPreference=avoid\n"), nil
			}
			return nil, nil
		},
	}
	policy := SlicePolicy{
		MemoryMax:            "12884901888",
		MemoryLow:            "2147483648",
		ManagedOOMPreference: "avoid",
	}

	if err := manager.EnsureSlice(context.Background(), "gcdolt.slice", policy); err != nil {
		t.Fatalf("EnsureSlice: %v", err)
	}

	want := [][]string{
		{
			"systemctl", "--user", "set-property", "--runtime", "gcdolt.slice",
			"MemoryMax=12884901888", "MemoryLow=2147483648", "ManagedOOMPreference=avoid",
		},
		{
			"systemctl", "--user", "show", "gcdolt.slice",
			"--property=MemoryMax", "--property=MemoryLow", "--property=ManagedOOMPreference",
		},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestManagerEnsureSliceRejectsIneffectivePolicy(t *testing.T) {
	manager := Manager{
		Run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "systemctl" {
				return []byte("MemoryMax=infinity\nMemoryLow=0\nManagedOOMPreference=none\n"), nil
			}
			return nil, nil
		},
	}
	err := manager.EnsureSlice(context.Background(), "gcdolt.slice", SlicePolicy{
		MemoryMax:            "12884901888",
		MemoryLow:            "2147483648",
		ManagedOOMPreference: "avoid",
	})
	if err == nil {
		t.Fatal("EnsureSlice error = nil for ineffective policy")
	}
	for _, want := range []string{"MemoryMax=infinity", "ManagedOOMPreference=none"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("EnsureSlice error = %q, want %q", err, want)
		}
	}
}

func TestManagerStartScopeWithProcessesUsesOneAtomicTransientUnitCall(t *testing.T) {
	var gotName string
	var gotArgs []string
	manager := Manager{
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			gotName = name
			gotArgs = append([]string(nil), args...)
			return []byte(`o "/org/freedesktop/systemd1/job/42"`), nil
		},
	}

	if err := manager.StartScopeWithProcesses(
		context.Background(),
		"gcdolt-adopt-123.scope",
		"gcdolt.slice",
		"Gas City managed Dolt",
		[]int{101, 102},
	); err != nil {
		t.Fatalf("StartScopeWithProcesses: %v", err)
	}

	if gotName != "busctl" {
		t.Fatalf("runner name = %q, want busctl", gotName)
	}
	wantArgs := []string{
		"--user", "call",
		"org.freedesktop.systemd1",
		"/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager",
		"StartTransientUnit",
		"ssa(sv)a(sa(sv))",
		"gcdolt-adopt-123.scope",
		"fail",
		"4",
		"PIDs", "au", "2", "101", "102",
		"Slice", "s", "gcdolt.slice",
		"Description", "s", "Gas City managed Dolt",
		"CollectMode", "s", "inactive-or-failed",
		"0",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("busctl args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestManagerStartScopeWithProcessesRejectsUnsafeInputsBeforeCallingBus(t *testing.T) {
	calls := 0
	manager := Manager{
		Run: func(context.Context, string, ...string) ([]byte, error) {
			calls++
			return nil, nil
		},
	}
	tests := []struct {
		name  string
		unit  string
		slice string
		pids  []int
	}{
		{name: "non-scope unit", unit: "gcdolt.service", slice: "gcdolt.slice", pids: []int{101}},
		{name: "non-slice parent", unit: "gcdolt.scope", slice: "gcdolt.service", pids: []int{101}},
		{name: "pid one", unit: "gcdolt.scope", slice: "gcdolt.slice", pids: []int{1}},
		{name: "duplicate pid", unit: "gcdolt.scope", slice: "gcdolt.slice", pids: []int{101, 101}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := manager.StartScopeWithProcesses(
				context.Background(), tc.unit, tc.slice, "test", tc.pids,
			); err == nil {
				t.Fatal("StartScopeWithProcesses error = nil")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("runner called %d times for invalid inputs, want 0", calls)
	}
}
