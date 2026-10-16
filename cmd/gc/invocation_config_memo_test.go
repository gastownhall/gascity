package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// The memo is process-wide state, so none of these tests run in parallel.

func writeConfigMemoTestCity(t *testing.T, name string) string {
	t.Helper()
	cityDir := t.TempDir()
	writeConfigMemoTestCityTOML(t, cityDir, name)
	return cityDir
}

func writeConfigMemoTestCityTOML(t *testing.T, cityDir, name string) {
	t.Helper()
	toml := "[workspace]\nname = \"" + name + "\"\nprefix = \"mc\"\n\n[[agent]]\nname = \"worker\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// beginConfigMemoForTest activates the memo for the rest of the test, the way
// run() activates it for one invocation.
func beginConfigMemoForTest(t *testing.T) {
	t.Helper()
	t.Cleanup(beginInvocationConfigMemo())
}

func configLoadsDuring(t *testing.T, fn func()) int64 {
	t.Helper()
	before := composedCityConfigLoads.Load()
	fn()
	return composedCityConfigLoads.Load() - before
}

func mustLoadMemoTestCity(t *testing.T, cityDir string) {
	t.Helper()
	if _, err := loadCityConfig(cityDir, io.Discard); err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
}

func memoTestCityName(t *testing.T, cityDir string) string {
	t.Helper()
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	return cfg.Workspace.Name
}

// Every loader on a one-shot command's path shares one composition: the
// command's own load, the prematerialized load the store open and bd binary
// pinning take, and the hosted-credential probe. Before the memo each composed
// the city again. The storage-routing funnel reads config on its own by design
// (see invocationConfigMemo).
func TestInvocationConfigMemoComposesOncePerInvocation(t *testing.T) {
	cityDir := writeConfigMemoTestCity(t, "memo")
	resetHostedCredentialProbeCache()
	t.Cleanup(resetHostedCredentialProbeCache)
	beginConfigMemoForTest(t)

	loads := configLoadsDuring(t, func() {
		for i := 0; i < 3; i++ {
			mustLoadMemoTestCity(t, cityDir)
		}
		if _, err := loadCityConfigWithoutBuiltinPackRefresh(cityDir, io.Discard); err != nil {
			t.Fatalf("loadCityConfigWithoutBuiltinPackRefresh: %v", err)
		}
		if _, err := citySelectsHostedBeadsCredentialProvider(cityDir); err != nil {
			t.Fatalf("citySelectsHostedBeadsCredentialProvider: %v", err)
		}
	})
	if loads != 1 {
		t.Fatalf("one invocation composed the city config %d times, want 1", loads)
	}
}

// With no memo active, every load composes: package tests that call loaders
// directly, and long-lived processes, keep the old behavior.
func TestInvocationConfigMemoInactiveComposesEveryLoad(t *testing.T) {
	cityDir := writeConfigMemoTestCity(t, "memo")
	loads := configLoadsDuring(t, func() {
		mustLoadMemoTestCity(t, cityDir)
		mustLoadMemoTestCity(t, cityDir)
	})
	if loads != 2 {
		t.Fatalf("loads without a memo composed %d times, want 2", loads)
	}
}

// Different cities and different load options never share an entry.
func TestInvocationConfigMemoKeysOnCityAndOptions(t *testing.T) {
	first := writeConfigMemoTestCity(t, "first")
	second := writeConfigMemoTestCity(t, "second")
	beginConfigMemoForTest(t)

	if got := memoTestCityName(t, first); got != "first" {
		t.Fatalf("first city name = %q", got)
	}
	if got := memoTestCityName(t, second); got != "second" {
		t.Fatalf("second city name = %q, want second: cities must not share an entry", got)
	}
	loads := configLoadsDuring(t, func() {
		if _, err := loadCityConfigAdvisory(first); err != nil {
			t.Fatalf("loadCityConfigAdvisory: %v", err)
		}
	})
	if loads != 1 {
		t.Fatalf("advisory load composed %d times, want 1: other load options must not share an entry", loads)
	}
}

// The builtin-pack refresh may rewrite builtin packs, so the first refreshing
// load in an invocation does not take an entry composed before the refresh.
// After that, every loader shares the post-refresh entry.
func TestInvocationConfigMemoRecomposesAfterFirstBuiltinRefresh(t *testing.T) {
	cityDir := writeConfigMemoTestCity(t, "memo")
	beginConfigMemoForTest(t)

	prematerialized := func() {
		if _, err := loadCityConfigWithoutBuiltinPackRefresh(cityDir, io.Discard); err != nil {
			t.Fatalf("loadCityConfigWithoutBuiltinPackRefresh: %v", err)
		}
	}
	if loads := configLoadsDuring(t, prematerialized); loads != 1 {
		t.Fatalf("first prematerialized load composed %d times, want 1", loads)
	}
	if loads := configLoadsDuring(t, func() { mustLoadMemoTestCity(t, cityDir) }); loads != 1 {
		t.Fatalf("first refreshing load composed %d times, want 1: it must not take the pre-refresh entry", loads)
	}
	if loads := configLoadsDuring(t, func() { mustLoadMemoTestCity(t, cityDir) }); loads != 0 {
		t.Fatalf("second refreshing load composed %d times, want 0", loads)
	}
	if loads := configLoadsDuring(t, prematerialized); loads != 0 {
		t.Fatalf("prematerialized load after the refresh composed %d times, want 0", loads)
	}
}

// A command that rewrites city.toml and then loads again sees the new file,
// even through a writer that does not call the invalidation hook.
func TestInvocationConfigMemoRereadsRewrittenCityTOML(t *testing.T) {
	cityDir := writeConfigMemoTestCity(t, "before")
	beginConfigMemoForTest(t)

	mustLoadMemoTestCity(t, cityDir)
	writeConfigMemoTestCityTOML(t, cityDir, "after-rewrite")
	if got := memoTestCityName(t, cityDir); got != "after-rewrite" {
		t.Fatalf("Workspace.Name = %q, want after-rewrite: a rewritten city.toml must be re-read", got)
	}
}

// packs.lock and .gc/site.toml shape the load without being reported as
// sources; one appearing after a load drops the entry.
func TestInvocationConfigMemoRereadsWhenLockOrSiteBindingsAppear(t *testing.T) {
	for _, rel := range []string{"packs.lock", filepath.Join(".gc", "site.toml")} {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			cityDir := writeConfigMemoTestCity(t, "memo")
			beginConfigMemoForTest(t)
			mustLoadMemoTestCity(t, cityDir)

			path := filepath.Join(cityDir, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if loads := configLoadsDuring(t, func() { mustLoadMemoTestCity(t, cityDir) }); loads != 1 {
				t.Fatalf("load after %s appeared composed %d times, want 1", rel, loads)
			}
		})
	}
}

// The config write helper invalidates explicitly, so a write-then-reread sees
// the new value even when the rewrite keeps city.toml's size and mtime.
func TestInvocationConfigMemoWriteHelperInvalidates(t *testing.T) {
	cityDir := writeConfigMemoTestCity(t, "aaaa")
	tomlPath := filepath.Join(cityDir, "city.toml")
	beginConfigMemoForTest(t)

	if got := memoTestCityName(t, cityDir); got != "aaaa" {
		t.Fatalf("fixture name = %q", got)
	}
	edit, err := loadCityConfigForEditFS(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("loadCityConfigForEditFS: %v", err)
	}
	fi, err := os.Stat(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	edit.Workspace.Name = "bbbb"
	if err := writeCityConfigForEditFS(fsys.OSFS{}, tomlPath, edit); err != nil {
		t.Fatalf("writeCityConfigForEditFS: %v", err)
	}
	if err := os.Chtimes(tomlPath, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := memoTestCityName(t, cityDir); got != "bbbb" {
		t.Fatalf("Workspace.Name = %q after a write-helper write, want bbbb", got)
	}
}

// Every config writer in cmd/gc invalidates the memo, so a write-then-reread in
// one invocation never sees the config from before the write.
func TestConfigWritersInvalidateInvocationConfigMemo(t *testing.T) {
	for _, tc := range []struct{ file, fn string }{
		{"cmd_agent.go", "func writeCityConfigForEditFS("},
		{"cmd_import.go", "func writeCityPackManifest("},
		{"cmd_import.go", "validateComposedConfigAfterInstall = func("},
		{"cmd_rig.go", "config.WriteCityAndRigSiteBindingsForEditRemovingRigs("},
		{"cmd_pack.go", "config.WriteLock("},
		{"doctor_v2_checks.go", "config.WriteCityAndRigSiteBindingsForEdit("},
		{"cmd_agent.go", "config.WriteCityAgentSuspendedForEdit("},
	} {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		body := string(src)
		at := strings.Index(body, tc.fn)
		if at < 0 {
			t.Fatalf("%s: %q not found", tc.file, tc.fn)
		}
		// The hook sits in the same function, before or just after the write.
		window := body[max(0, at-400):min(len(body), at+2500)]
		if !strings.Contains(window, "invalidateInvocationConfigMemo()") {
			t.Errorf("%s: no invalidateInvocationConfigMemo() near %q", tc.file, tc.fn)
		}
	}
	// gc init writes city.toml through the helper, never around it.
	src, err := os.ReadFile("cmd_init.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "config.WriteCityAndRigSiteBindingsForEdit(") {
		t.Error("cmd_init.go writes city.toml around writeCityConfigForEditFS, skipping the memo invalidation")
	}
}

// The post-install validation composes what `gc import install` just
// materialized, not an entry from before it.
func TestValidateComposedConfigAfterInstallComposesFresh(t *testing.T) {
	cityDir := writeConfigMemoTestCity(t, "memo")
	beginConfigMemoForTest(t)
	mustLoadMemoTestCity(t, cityDir)

	loads := configLoadsDuring(t, func() {
		if err := validateComposedConfigAfterInstall(cityDir); err != nil {
			t.Fatalf("validateComposedConfigAfterInstall: %v", err)
		}
	})
	if loads != 1 {
		t.Fatalf("post-install validation composed %d times, want 1 fresh composition", loads)
	}
}

// A failed load is not memoized: fixing the file is seen at once.
func TestInvocationConfigMemoDoesNotMemoizeErrors(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beginConfigMemoForTest(t)
	if _, err := loadCityConfig(cityDir, io.Discard); err == nil {
		t.Fatal("malformed city.toml loaded")
	}
	writeConfigMemoTestCityTOML(t, cityDir, "fixed")
	if got := memoTestCityName(t, cityDir); got != "fixed" {
		t.Fatalf("Workspace.Name = %q, want fixed", got)
	}
}

// Only real-filesystem loads are memoized; a fake filesystem is a test's own
// world and composes every time.
func TestInvocationConfigMemoSkipsNonOSFilesystems(t *testing.T) {
	fake := fsys.NewFake()
	fake.Files["/city/city.toml"] = []byte("[workspace]\nname = \"fake\"\n")
	beginConfigMemoForTest(t)

	loads := configLoadsDuring(t, func() {
		for i := 0; i < 2; i++ {
			if _, err := loadCityConfigFS(fake, "/city/city.toml", io.Discard); err != nil {
				t.Fatalf("loadCityConfigFS(fake): %v", err)
			}
		}
	})
	if loads != 2 {
		t.Fatalf("fake-filesystem loads composed %d times, want 2", loads)
	}
}

// run() activates the memo for one-shot commands only; the long-lived entry
// points reload config in their loops and never see it.
func TestInvocationConfigMemoCoversOneShotCommandsOnly(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"mail", "inbox"}, true},
		{[]string{"mail", "check", "--inject"}, true},
		{[]string{"nudge", "drain", "--inject"}, true},
		{[]string{"hook", "run", "--", "nudge", "drain"}, true},
		{[]string{"bd", "list"}, true},
		{[]string{"session", "list"}, true},
		{[]string{"prime", "--hook"}, true},
		{[]string{"nudge", "poll"}, false},
		{[]string{"supervisor", "run"}, false},
		{[]string{"start"}, false},
		{[]string{"dashboard", "serve"}, false},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			cmd, _, err := root.Find(tc.args)
			if err != nil {
				t.Fatalf("Find(%v): %v", tc.args, err)
			}
			if got := invocationConfigMemoEligible(cmd); got != tc.want {
				t.Fatalf("invocationConfigMemoEligible(%q) = %v, want %v", cmd.CommandPath(), got, tc.want)
			}
		})
	}
	for path := range invocationConfigMemoLongLivedCommands {
		args := strings.Fields(strings.TrimPrefix(path, "gc "))
		cmd, _, err := root.Find(args)
		if err != nil || cmd.CommandPath() != path {
			t.Errorf("long-lived command %q does not name a command (found %v, err %v)", path, cmd, err)
		}
	}
}

// A long-lived host leaves the memo before its loop, so even when it was
// reached with a memo active, every reload composes from disk.
func TestInvocationConfigMemoNeverServesLongLivedHosts(t *testing.T) {
	cityDir := writeConfigMemoTestCity(t, "memo")
	beginConfigMemoForTest(t)
	mustLoadMemoTestCity(t, cityDir)

	leaveInvocationConfigMemo()
	if loads := configLoadsDuring(t, func() {
		mustLoadMemoTestCity(t, cityDir)
		mustLoadMemoTestCity(t, cityDir)
	}); loads != 2 {
		t.Fatalf("reloads after leaving the memo composed %d times, want 2", loads)
	}

	for _, tc := range []struct{ file, fn string }{
		{"cmd_supervisor.go", "func runSupervisor("},
		{"controller.go", "func runController("},
		{"city_runtime.go", "func newCityRuntime("},
		{"cmd_nudge.go", "func cmdNudgePoll("},
	} {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		body := string(src)
		start := strings.Index(body, tc.fn)
		if start < 0 {
			t.Fatalf("%s: %s not found", tc.file, tc.fn)
		}
		end := strings.Index(body[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("%s: end of %s not found", tc.file, tc.fn)
		}
		if !strings.Contains(body[start:start+end], "leaveInvocationConfigMemo()") {
			t.Errorf("%s: %s does not call leaveInvocationConfigMemo()", tc.file, tc.fn)
		}
	}
}

// run() ends the memo with the invocation.
func TestRunEndsInvocationConfigMemo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc version exited %d: %s", code, stderr.String())
	}
	if currentInvocationConfigMemo() != nil {
		t.Fatal("the config memo is still active after run returned")
	}
}

// The two hottest hook and patrol commands compose the city config once per
// invocation. Without the memo they composed it once per loader on their path:
// 8 times for mail inbox and 3 for nudge drain on this file-store fixture, and
// 16 for mail inbox on a bd-backed city with 16 packs.
func TestOneShotHookCommandsComposeCityConfigOnce(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		alias string
	}{
		{args: []string{"mail", "inbox", "--json"}},
		// Drain resolves its session from GC_ALIAS; without one it exits
		// before reading config.
		{args: []string{"nudge", "drain", "--inject"}, alias: "worker"},
	} {
		args := tc.args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cityDir := writeConfigMemoTestCity(t, "memo")
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_CITY_PATH", cityDir)
			t.Setenv("GC_ALIAS", tc.alias)
			t.Setenv("GC_SESSION_ID", "")
			t.Chdir(cityDir)

			var stdout, stderr bytes.Buffer
			var code int
			loads := configLoadsDuring(t, func() { code = run(args, &stdout, &stderr) })
			if code != 0 {
				t.Fatalf("gc %s exited %d: %s", strings.Join(args, " "), code, stderr.String())
			}
			if loads != 1 {
				t.Fatalf("gc %s composed the city config %d times, want 1", strings.Join(args, " "), loads)
			}
		})
	}
}
