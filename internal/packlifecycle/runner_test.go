package packlifecycle

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func requireShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts require a POSIX shell")
	}
}

func TestRunExecutesHooksInOrderAndCapturesOutput(t *testing.T) {
	requireShell(t)
	dir := t.TempDir()
	first := writeScript(t, dir, "first.sh", "#!/bin/sh\necho first\n")
	second := writeScript(t, dir, "second.sh", "#!/bin/sh\necho second\n")

	results := Run(context.Background(), t.TempDir(), []Hook{
		{Event: "city-stop", Script: first, PackDir: dir, PackName: "alpha"},
		{Event: "city-stop", Script: second, PackDir: dir, PackName: "beta"},
	}, time.Minute)

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Name != "alpha:city-stop" || results[1].Name != "beta:city-stop" {
		t.Fatalf("names = %q, %q", results[0].Name, results[1].Name)
	}
	for i, want := range []string{"first", "second"} {
		if results[i].Err != nil {
			t.Errorf("results[%d].Err = %v, want nil", i, results[i].Err)
		}
		if strings.TrimSpace(results[i].Output) != want {
			t.Errorf("results[%d].Output = %q, want %q", i, results[i].Output, want)
		}
	}
}

func TestRunContinuesAfterFailingHook(t *testing.T) {
	requireShell(t)
	dir := t.TempDir()
	failing := writeScript(t, dir, "fail.sh", "#!/bin/sh\necho boom >&2\nexit 3\n")
	ok := writeScript(t, dir, "ok.sh", "#!/bin/sh\nexit 0\n")

	results := Run(context.Background(), t.TempDir(), []Hook{
		{Event: "city-stop", Script: failing, PackDir: dir, PackName: "alpha"},
		{Event: "city-stop", Script: ok, PackDir: dir, PackName: "beta"},
	}, time.Minute)

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("failing hook: Err = nil, want exit-status error")
	}
	if !strings.Contains(results[0].Err.Error(), "3") {
		t.Errorf("Err = %v, want the exit status", results[0].Err)
	}
	if !strings.Contains(results[0].Output, "boom") {
		t.Errorf("Output = %q, want captured stderr", results[0].Output)
	}
	if results[1].Err != nil {
		t.Errorf("second hook did not run cleanly: %v", results[1].Err)
	}
}

func TestRunInjectsPackAndCityEnv(t *testing.T) {
	requireShell(t)
	dir := t.TempDir()
	script := writeScript(t, dir, "env.sh", "#!/bin/sh\necho \"$GC_CITY_PATH|$GC_PACK_DIR|$GC_LIFECYCLE_EVENT|$PWD\"\n")
	cityPath := t.TempDir()

	results := Run(context.Background(), cityPath, []Hook{
		{Event: "city-stop", Script: script, PackDir: dir, PackName: "alpha"},
	}, time.Minute)

	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	fields := strings.Split(strings.TrimSpace(results[0].Output), "|")
	if len(fields) != 4 {
		t.Fatalf("output = %q", results[0].Output)
	}
	if fields[0] != cityPath {
		t.Errorf("GC_CITY_PATH = %q, want %q", fields[0], cityPath)
	}
	if fields[1] != dir {
		t.Errorf("GC_PACK_DIR = %q, want %q", fields[1], dir)
	}
	if fields[2] != "city-stop" {
		t.Errorf("GC_LIFECYCLE_EVENT = %q, want city-stop", fields[2])
	}
	if evalSymlinks(t, fields[3]) != evalSymlinks(t, dir) {
		t.Errorf("working dir = %q, want pack dir %q", fields[3], dir)
	}
}

func evalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

func TestRunTimesOutHangingHook(t *testing.T) {
	requireShell(t)
	dir := t.TempDir()
	script := writeScript(t, dir, "hang.sh", "#!/bin/sh\nsleep 30\n")

	start := time.Now()
	results := Run(context.Background(), t.TempDir(), []Hook{
		{Event: "city-stop", Script: script, PackDir: dir, PackName: "alpha"},
	}, 200*time.Millisecond)

	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("Err = nil, want timeout error")
	}
	if !strings.Contains(results[0].Err.Error(), "timed out") {
		t.Errorf("Err = %v, want a timeout error", results[0].Err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("elapsed = %s, want the hook to be killed at the timeout", elapsed)
	}
}

func TestRunReportsMissingScript(t *testing.T) {
	results := Run(context.Background(), t.TempDir(), []Hook{
		{Event: "city-stop", Script: filepath.Join(t.TempDir(), "absent.sh"), PackDir: t.TempDir(), PackName: "alpha"},
	}, time.Minute)

	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want an error for a missing script", results)
	}
}

func TestRunNoHooks(t *testing.T) {
	if results := Run(context.Background(), t.TempDir(), nil, time.Minute); results != nil {
		t.Fatalf("results = %+v, want nil", results)
	}
}
