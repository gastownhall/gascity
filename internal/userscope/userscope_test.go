package userscope

import (
	"errors"
	"slices"
	"testing"
)

func TestWrapArgv(t *testing.T) {
	w := &Wrapper{Probe: func(string) error { return nil }}
	got := w.Wrap("gascity-infra.slice", []string{"dolt", "sql-server", "--config", "/c/config.yaml"})
	want := []string{
		"systemd-run", "--user", "--scope", "--slice=gascity-infra.slice", "--collect", "--quiet", "--",
		"dolt", "sql-server", "--config", "/c/config.yaml",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Wrap argv = %q, want %q", got, want)
	}
	if empty := w.Wrap("gascity-infra.slice", nil); len(empty) != 0 {
		t.Fatalf("Wrap(nil) = %q, want empty", empty)
	}
}

func TestWrapLeavesArgvUnchangedWithoutSlice(t *testing.T) {
	probeCalls := 0
	w := &Wrapper{Probe: func(string) error { probeCalls++; return nil }}
	argv := []string{"gc", "supervisor", "run"}
	if got := w.Wrap("", argv); !slices.Equal(got, argv) {
		t.Fatalf("Wrap with empty slice = %q, want %q", got, argv)
	}
	if probeCalls != 0 {
		t.Fatalf("probe called %d times for an empty slice, want 0", probeCalls)
	}
}

func TestWrapFallsBackWithoutUserManager(t *testing.T) {
	probeCalls := 0
	var reported []error
	w := &Wrapper{
		Probe: func(string) error {
			probeCalls++
			return errors.New("Failed to connect to bus")
		},
		Unavailable: func(slice string, err error) {
			if slice != "gascity-infra.slice" {
				t.Errorf("Unavailable slice = %q, want gascity-infra.slice", slice)
			}
			reported = append(reported, err)
		},
	}
	argv := []string{"gc", "supervisor", "run"}
	for i := 0; i < 3; i++ {
		if got := w.Wrap("gascity-infra.slice", argv); !slices.Equal(got, argv) {
			t.Fatalf("Wrap call %d = %q, want unwrapped %q", i, got, argv)
		}
	}
	if probeCalls != 1 {
		t.Fatalf("probe called %d times, want 1 (result must be cached)", probeCalls)
	}
	if len(reported) != 1 {
		t.Fatalf("Unavailable called %d times, want 1", len(reported))
	}
}

func TestWrapDoesNotAliasCallerArgv(t *testing.T) {
	w := &Wrapper{Probe: func(string) error { return nil }}
	argv := make([]string, 2, 16)
	argv[0], argv[1] = "a", "b"
	got := w.Wrap("s.slice", argv)
	got[len(got)-1] = "mutated"
	if argv[1] != "b" {
		t.Fatalf("Wrap result aliases caller argv: %q", argv)
	}
}

func TestProbeFailsWithoutSystemdRun(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := Probe("gascity-infra.slice"); err == nil {
		t.Fatal("Probe succeeded with no systemd-run on PATH, want error")
	}
}
