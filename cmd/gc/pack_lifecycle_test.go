package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func writeLifecyclePack(t *testing.T, root, packName, event, body string) string {
	t.Helper()
	packDir := filepath.Join(root, packName)
	lifecycleDir := filepath.Join(packDir, "lifecycle")
	if err := os.MkdirAll(lifecycleDir, 0o755); err != nil {
		t.Fatalf("mkdir pack: %v", err)
	}
	manifest := "[pack]\nname = \"" + packName + "\"\n"
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write pack.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lifecycleDir, event+".sh"), []byte(body), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	return packDir
}

func TestPackLifecycleHookDirsOrdersCityPacksBeforeRigPacks(t *testing.T) {
	cfg := &config.City{
		PackDirs: []string{"/packs/city-a", "/packs/city-b"},
		RigPackDirs: map[string][]string{
			"zeta":  {"/packs/zeta"},
			"alpha": {"/packs/alpha"},
		},
	}
	got := packLifecycleHookDirs(cfg)
	want := []string{"/packs/city-a", "/packs/city-b", "/packs/alpha", "/packs/zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("dirs = %v, want %v", got, want)
	}
}

func TestPackLifecycleHooksFindsCityAndRigPacks(t *testing.T) {
	root := t.TempDir()
	cityPack := writeLifecyclePack(t, root, "citypack", config.LifecycleEventCityStop, "#!/bin/sh\nexit 0\n")
	rigPack := writeLifecyclePack(t, root, "rigpack", config.LifecycleEventCityStop, "#!/bin/sh\nexit 0\n")
	writeLifecyclePack(t, root, "startonly", config.LifecycleEventCityStart, "#!/bin/sh\nexit 0\n")

	cfg := &config.City{
		PackDirs:    []string{cityPack, filepath.Join(root, "startonly")},
		RigPackDirs: map[string][]string{"rig": {rigPack}},
	}

	hooks := packLifecycleHooks(cfg, config.LifecycleEventCityStop)
	if len(hooks) != 2 {
		t.Fatalf("hooks = %+v, want the two city-stop hooks", hooks)
	}
	if hooks[0].PackName != "citypack" || hooks[1].PackName != "rigpack" {
		t.Errorf("hook packs = %q, %q; want citypack, rigpack", hooks[0].PackName, hooks[1].PackName)
	}
}

func TestRunPackLifecycleHooksRunsHookAndReportsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts require a POSIX shell")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "ran")
	okPack := writeLifecyclePack(t, root, "okpack", config.LifecycleEventCityStop,
		"#!/bin/sh\ntouch \""+marker+"\"\nexit 0\n")
	badPack := writeLifecyclePack(t, root, "badpack", config.LifecycleEventCityStop,
		"#!/bin/sh\necho hub down failed >&2\nexit 1\n")

	cfg := &config.City{PackDirs: []string{okPack, badPack}}
	var stdout, stderr bytes.Buffer
	runPackLifecycleHooks(t.TempDir(), cfg, config.LifecycleEventCityStop, &stdout, &stderr)

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("hook did not run: %v", err)
	}
	if !strings.Contains(stderr.String(), "gc stop: pack hook badpack:city-stop") {
		t.Errorf("stderr = %q, want a warning for the failing hook", stderr.String())
	}
	if !strings.Contains(stderr.String(), "hub down failed") {
		t.Errorf("stderr = %q, want the hook output", stderr.String())
	}
}

func TestRunPackLifecycleHooksNoPacksIsSilent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runPackLifecycleHooks(t.TempDir(), &config.City{}, config.LifecycleEventCityStop, &stdout, &stderr)
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q; want silence", stdout.String(), stderr.String())
	}
}

func TestRunPackLifecycleHooksEchoesHookOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts require a POSIX shell")
	}
	root := t.TempDir()
	pack := writeLifecyclePack(t, root, "chatty", config.LifecycleEventCityStart,
		"#!/bin/sh\necho federation hub started\n")

	var stdout, stderr bytes.Buffer
	runPackLifecycleHooks(t.TempDir(), &config.City{PackDirs: []string{pack}}, config.LifecycleEventCityStart, &stdout, &stderr)

	if !strings.Contains(stdout.String(), "pack hook chatty:city-start: federation hub started") {
		t.Errorf("stdout = %q, want the hook output echoed", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want silence", stderr.String())
	}
}
