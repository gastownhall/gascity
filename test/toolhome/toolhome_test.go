package toolhome

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestEnvironReplacesHomeAndKeepsOnlyExplicitBeadsVars(t *testing.T) {
	home := "/var/tmp/tool-home"
	got := Environ([]string{
		"PATH=/usr/bin",
		"HOME=/home/operator",
		"XDG_CONFIG_HOME=/home/operator/.config",
		"BEADS_DIR=/work/.beads",
		"BEADS_DOLT_SHARED_SERVER=1",
		"BEADS_DOLT_SERVER_PORT=3307",
		"BD_DOLT_SHARED_SERVER=true",
		"BD_ALLOW_REMOTE_MIGRATE=1",
	}, home, "BEADS_DIR", "HOME")
	want := []string{
		"BD_DISABLE_METRICS=1",
		"BD_DOLT_SHARED_SERVER=false",
		"BEADS_DIR=/work/.beads",
		"HOME=" + home,
		"PATH=/usr/bin",
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Environ =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestEnvironKeepsAnExplicitlyEmptySharedServerSwitch(t *testing.T) {
	got := Environ([]string{"BD_DOLT_SHARED_SERVER=", "XDG_CONFIG_HOME=/seeded"}, "/h", Explicit([]string{"BD_DOLT_SHARED_SERVER=", "XDG_CONFIG_HOME=/seeded", "HOME=/real"})...)
	vars := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	if v, ok := vars[SharedServerConfigEnv]; !ok || v != "" {
		t.Errorf("%s = %q (set=%v), want kept as empty", SharedServerConfigEnv, v, ok)
	}
	if vars["XDG_CONFIG_HOME"] != "/seeded" || vars["HOME"] != "/h" {
		t.Errorf("got %v, want the seeded XDG_CONFIG_HOME kept and HOME replaced", vars)
	}
}

// The wrapper is what gc actually forks, so check what bd receives through it.
func TestWrapperScriptReHomesBdAndHonoursExplicitVars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper is POSIX-only")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "real bd")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	home := filepath.Join(dir, "it's home")
	script, err := WrapperScript(home, stub)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "bd")
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil { //nolint:gosec // wrapper must be executable
		t.Fatal(err)
	}

	run := func(env ...string) map[string]string {
		t.Helper()
		cmd := exec.Command(wrapper)
		cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("wrapper: %v\n%s", err, out)
		}
		vars := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				vars[k] = v
			}
		}
		return vars
	}

	defaulted := run("HOME=/home/operator")
	for k, want := range defaults(home) {
		if defaulted[k] != want {
			t.Errorf("%s = %q, want %q", k, defaulted[k], want)
		}
	}
	if defaulted[SharedServerConfigEnv] != "false" || defaulted[DisableMetricsEnv] != "1" {
		t.Errorf("%s = %q, %s = %q; want false and 1", SharedServerConfigEnv, defaulted[SharedServerConfigEnv], DisableMetricsEnv, defaulted[DisableMetricsEnv])
	}

	explicit := run("HOME=/home/operator", "XDG_CONFIG_HOME=/seeded", SharedServerConfigEnv+"=", "BEADS_DIR=/work/.beads")
	if explicit["HOME"] != home {
		t.Errorf("HOME = %q, want %q even when the caller set one", explicit["HOME"], home)
	}
	if explicit["XDG_CONFIG_HOME"] != "/seeded" || explicit["BEADS_DIR"] != "/work/.beads" {
		t.Errorf("explicit variables not honored: %v", explicit)
	}
	if v, ok := explicit[SharedServerConfigEnv]; !ok || v != "" {
		t.Errorf("%s = %q (set=%v), want kept as empty", SharedServerConfigEnv, v, ok)
	}
}

func TestScrubProcessEnvLeavesOnlyExplicitValues(t *testing.T) {
	t.Setenv("HOME", "/home/operator")
	t.Setenv("XDG_CONFIG_HOME", "/home/operator/.config")
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
	t.Setenv("BEADS_SHARED_SERVER_DIR", "/home/operator/.beads/shared-server")
	t.Setenv("BD_DOLT_SHARED_SERVER", "true")
	t.Setenv("GC_UNRELATED", "kept")
	if err := ScrubProcessEnv(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"XDG_CONFIG_HOME", "BEADS_DOLT_SHARED_SERVER", "BEADS_SHARED_SERVER_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			t.Errorf("%s=%q survived the scrub", name, v)
		}
	}
	if got := os.Getenv(SharedServerConfigEnv); got != "false" {
		t.Errorf("%s = %q, want false", SharedServerConfigEnv, got)
	}
	if os.Getenv("HOME") != "/home/operator" || os.Getenv("GC_UNRELATED") != "kept" {
		t.Errorf("scrub touched HOME or an unrelated variable")
	}
}

// The wrapper execs realBD; written over realBD it would exec itself forever
// and the real binary would be gone. Refuse both the direct path and a symlink
// that resolves to it.
func TestWriteWrapperRefusesToOverwriteTheRealBd(t *testing.T) {
	dir := t.TempDir()
	realBD := filepath.Join(dir, "bd-real")
	original := []byte("#!/bin/sh\necho real\n")
	if err := os.WriteFile(realBD, original, 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bd")
	if err := os.Symlink(realBD, link); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{realBD, link, filepath.Join(dir, ".", "bd-real")} {
		if err := WriteWrapper(target, filepath.Join(dir, "home"), realBD); err == nil {
			t.Errorf("WriteWrapper(%s) over the real bd succeeded", target)
		}
	}
	if got, err := os.ReadFile(realBD); err != nil || string(got) != string(original) {
		t.Fatalf("real bd changed: %q, %v", got, err)
	}
	if err := WriteWrapper(filepath.Join(dir, "wrapped", "bd"), filepath.Join(dir, "home"), realBD); err != nil {
		t.Fatalf("WriteWrapper to a separate path: %v", err)
	}
}
