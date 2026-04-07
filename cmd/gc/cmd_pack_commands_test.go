package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// setupPackCity creates a temp city with a pack that has [[commands]].
// Returns cityPath, packDir.
func setupPackCity(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()

	// Create city structure.
	cityPath := filepath.Join(dir, "testcity")
	gcDir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create pack directory.
	packDir := filepath.Join(dir, "packs", "mypack")
	if err := os.MkdirAll(filepath.Join(packDir, "commands"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write pack.toml with a [[commands]] entry.
	packTOML := `[pack]
name = "mypack"
schema = 1

[[commands]]
name = "hello"
description = "Say hello"
long_description = "commands/hello-help.txt"
script = "commands/hello.sh"

[[commands]]
name = "info"
description = "Show info"
long_description = "commands/info-help.txt"
script = "commands/info.sh"
`
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(packTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write long description.
	if err := os.WriteFile(filepath.Join(packDir, "commands", "hello-help.txt"),
		[]byte("Say hello to the world.\n\nThis command greets everyone."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "commands", "info-help.txt"),
		[]byte("Show pack info."), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write hello.sh script.
	helloScript := `#!/bin/sh
echo "hello from $GC_PACK_NAME"
echo "cityctx=$GC_CITY"
echo "city=$GC_CITY_PATH"
echo "runtime=$GC_CITY_RUNTIME_DIR"
echo "packdir=$GC_PACK_DIR"
echo "packstate=$GC_PACK_STATE_DIR"
echo "cityname=$GC_CITY_NAME"
echo "args=$*"
`
	scriptPath := filepath.Join(packDir, "commands", "hello.sh")
	if err := os.WriteFile(scriptPath, []byte(helloScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write info.sh script.
	infoScript := `#!/bin/sh
echo "info output"
`
	if err := os.WriteFile(filepath.Join(packDir, "commands", "info.sh"), []byte(infoScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write city.toml referencing the pack.
	cityTOML := `[workspace]
name = "testcity"

[workspace.pack]
path = "` + packDir + `"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	return cityPath, packDir
}

func TestLoadPackCommandEntries(t *testing.T) {
	_, packDir := setupPackCity(t)

	entries := config.LoadPackCommandEntries(fsys.OSFS{}, []string{packDir})

	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	hello := entries[0]
	if hello.PackName != "mypack" {
		t.Errorf("PackName = %q, want %q", hello.PackName, "mypack")
	}
	if hello.Entry.Name != "hello" {
		t.Errorf("Entry.Name = %q, want %q", hello.Entry.Name, "hello")
	}
	if hello.Entry.Description != "Say hello" {
		t.Errorf("Entry.Description = %q, want %q", hello.Entry.Description, "Say hello")
	}
	if hello.Entry.Script != "commands/hello.sh" {
		t.Errorf("Entry.Script = %q, want %q", hello.Entry.Script, "commands/hello.sh")
	}
	if hello.PackDir != packDir {
		t.Errorf("PackDir = %q, want %q", hello.PackDir, packDir)
	}
}

func TestLoadPackCommandEntriesDedup(t *testing.T) {
	_, packDir := setupPackCity(t)

	// Pass the same dir twice — should dedup.
	entries := config.LoadPackCommandEntries(fsys.OSFS{}, []string{packDir, packDir})
	if len(entries) != 2 {
		t.Fatalf("got %d entries after dedup, want 2", len(entries))
	}
}

func TestLoadPackCommandEntriesBadDir(t *testing.T) {
	entries := config.LoadPackCommandEntries(fsys.OSFS{}, []string{"/nonexistent"})
	if len(entries) != 0 {
		t.Fatalf("got %d entries for nonexistent dir, want 0", len(entries))
	}
}

func TestLoadPackCommandEntriesNilDirs(t *testing.T) {
	entries := config.LoadPackCommandEntries(fsys.OSFS{}, nil)
	if len(entries) != 0 {
		t.Fatalf("got %d entries for nil dirs, want 0", len(entries))
	}
}

func TestPackCommandRegistration(t *testing.T) {
	cityPath, packDir := setupPackCity(t)

	entries := config.LoadPackCommandEntries(fsys.OSFS{}, []string{packDir})

	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "start"})
	root.AddCommand(&cobra.Command{Use: "stop"})

	var stdout, stderr bytes.Buffer
	addPackCommandsToRoot(root, entries, cityPath, "testcity", &stdout, &stderr)

	// The pack namespace command should be registered.
	found := false
	for _, c := range root.Commands() {
		if c.Name() == "mypack" {
			found = true
			// Should have 2 subcommands.
			if len(c.Commands()) != 2 {
				t.Errorf("mypack has %d subcommands, want 2", len(c.Commands()))
			}
			// Check subcommand names.
			names := make(map[string]bool)
			for _, sub := range c.Commands() {
				names[sub.Name()] = true
			}
			if !names["hello"] {
				t.Error("missing 'hello' subcommand")
			}
			if !names["info"] {
				t.Error("missing 'info' subcommand")
			}
		}
	}
	if !found {
		t.Error("mypack namespace command not registered")
	}
}

func TestPackCommandCollisionProtection(t *testing.T) {
	cityPath, _ := setupPackCity(t)

	// Create an entry with pack name that shadows a core command.
	entries := []config.PackCommandInfo{
		{
			PackName: "start", // shadows core "start"
			Entry: config.PackCommandEntry{
				Name:        "foo",
				Description: "do foo",
				Script:      "foo.sh",
			},
			PackDir: "/tmp",
		},
	}

	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "start"})

	var stdout, stderr bytes.Buffer
	addPackCommandsToRoot(root, entries, cityPath, "testcity", &stdout, &stderr)

	// Should have warned.
	if !strings.Contains(stderr.String(), "shadows core command") {
		t.Errorf("expected collision warning, got stderr: %q", stderr.String())
	}

	// Should NOT have added a second "start" command.
	startCount := 0
	for _, c := range root.Commands() {
		if c.Name() == "start" {
			startCount++
		}
	}
	if startCount != 1 {
		t.Errorf("got %d 'start' commands, want 1", startCount)
	}
}

func TestPackCommandExecution(t *testing.T) {
	cityPath, packDir := setupPackCity(t)

	entries := config.LoadPackCommandEntries(fsys.OSFS{}, []string{packDir})

	// Find the "hello" entry.
	var hello config.PackCommandInfo
	for _, e := range entries {
		if e.Entry.Name == "hello" {
			hello = e
			break
		}
	}

	var stdout, stderr bytes.Buffer
	code := runPackCommand(hello, cityPath, "testcity", []string{"world", "42"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "hello from mypack") {
		t.Errorf("stdout missing 'hello from mypack', got:\n%s", out)
	}
	if !strings.Contains(out, "city="+cityPath) {
		t.Errorf("stdout missing city path, got:\n%s", out)
	}
	if !strings.Contains(out, "cityctx="+cityPath) {
		t.Errorf("stdout missing city context, got:\n%s", out)
	}
	if !strings.Contains(out, "runtime="+filepath.Join(cityPath, ".gc", "runtime")) {
		t.Errorf("stdout missing runtime dir, got:\n%s", out)
	}
	if !strings.Contains(out, "packdir="+packDir) {
		t.Errorf("stdout missing pack dir, got:\n%s", out)
	}
	if !strings.Contains(out, "packstate="+filepath.Join(cityPath, ".gc", "runtime", "packs", "mypack")) {
		t.Errorf("stdout missing pack state dir, got:\n%s", out)
	}
	if !strings.Contains(out, "cityname=testcity") {
		t.Errorf("stdout missing city name, got:\n%s", out)
	}
	if !strings.Contains(out, "args=world 42") {
		t.Errorf("stdout missing args, got:\n%s", out)
	}
}

func TestPackCommandExitCode(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a script that exits non-zero.
	script := `#!/bin/sh
exit 42
`
	if err := os.WriteFile(filepath.Join(packDir, "fail.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	info := config.PackCommandInfo{
		PackName: "test",
		Entry: config.PackCommandEntry{
			Name:   "fail",
			Script: "fail.sh",
		},
		PackDir: packDir,
	}

	var stdout, stderr bytes.Buffer
	code := runPackCommand(info, dir, "city", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 42 {
		t.Errorf("exit code = %d, want 42", code)
	}
}

func TestPackCommandTemplateExpansion(t *testing.T) {
	result := expandScriptTemplate("{{.CityRoot}}/bin/run.sh", "/home/user/city", "mytown", "/packs/p1")
	if result != "/home/user/city/bin/run.sh" {
		t.Errorf("expanded = %q, want %q", result, "/home/user/city/bin/run.sh")
	}
}

func TestPackCommandTemplateExpansionConfigDir(t *testing.T) {
	result := expandScriptTemplate("{{.ConfigDir}}/scripts/run.sh", "/city", "mytown", "/packs/p1")
	if result != "/packs/p1/scripts/run.sh" {
		t.Errorf("expanded = %q, want %q", result, "/packs/p1/scripts/run.sh")
	}
}

func TestPackCommandTemplateNoTemplate(t *testing.T) {
	result := expandScriptTemplate("commands/run.sh", "/city", "mytown", "/packs/p1")
	if result != "commands/run.sh" {
		t.Errorf("expanded = %q, want %q", result, "commands/run.sh")
	}
}

func TestPackCommandTemplateBadTemplate(t *testing.T) {
	result := expandScriptTemplate("{{.Bad", "/city", "mytown", "/packs/p1")
	if result != "{{.Bad" {
		t.Errorf("expected graceful fallback, got %q", result)
	}
}

func TestPackCommandLongDescription(t *testing.T) {
	_, packDir := setupPackCity(t)

	entries := config.LoadPackCommandEntries(fsys.OSFS{}, []string{packDir})

	var hello config.PackCommandInfo
	for _, e := range entries {
		if e.Entry.Name == "hello" {
			hello = e
			break
		}
	}

	long := readLongDescription(hello)
	if !strings.Contains(long, "Say hello to the world") {
		t.Errorf("long description missing expected text, got: %q", long)
	}
	if !strings.Contains(long, "This command greets everyone") {
		t.Errorf("long description missing second paragraph, got: %q", long)
	}
}

func TestPackCommandLongDescriptionMissing(t *testing.T) {
	info := config.PackCommandInfo{
		Entry: config.PackCommandEntry{
			LongDescription: "nonexistent.txt",
		},
		PackDir: "/tmp",
	}
	long := readLongDescription(info)
	if long != "" {
		t.Errorf("expected empty string for missing file, got %q", long)
	}
}

func TestPackCommandLongDescriptionEmpty(t *testing.T) {
	info := config.PackCommandInfo{
		Entry: config.PackCommandEntry{
			LongDescription: "",
		},
		PackDir: "/tmp",
	}
	long := readLongDescription(info)
	if long != "" {
		t.Errorf("expected empty string for empty path, got %q", long)
	}
}

func TestPackCommandPassthroughArgs(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Script that echoes all args, including flags.
	script := `#!/bin/sh
for arg in "$@"; do
    echo "arg:$arg"
done
`
	if err := os.WriteFile(filepath.Join(packDir, "echo.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	info := config.PackCommandInfo{
		PackName: "test",
		Entry: config.PackCommandEntry{
			Name:   "echo",
			Script: "echo.sh",
		},
		PackDir: packDir,
	}

	var stdout, stderr bytes.Buffer
	code := runPackCommand(info, dir, "city", []string{"--verbose", "-n", "3", "hello world"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "arg:--verbose") {
		t.Errorf("missing --verbose flag passthrough, got:\n%s", out)
	}
	if !strings.Contains(out, "arg:-n") {
		t.Errorf("missing -n flag passthrough, got:\n%s", out)
	}
	if !strings.Contains(out, "arg:3") {
		t.Errorf("missing positional arg passthrough, got:\n%s", out)
	}
	if !strings.Contains(out, "arg:hello world") {
		t.Errorf("missing quoted arg passthrough, got:\n%s", out)
	}
}

func TestRegisterPackCommands_UncachedPacksNoLogNoise(t *testing.T) {
	cityPath := t.TempDir()

	cityTOML := `[workspace]
name = "test"
includes = ["mypk"]

[packs.mypk]
source = "https://example.com/repo.git"
ref = "main"
path = "packs/mypk"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	_, _ = quietLoadCityConfig(cityPath)

	if bytes.Contains(logBuf.Bytes(), []byte("not found, skipping")) {
		t.Fatalf("quietLoadCityConfig produced log noise: %s", logBuf.String())
	}
}

func TestCoreCommandNames(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "start", Aliases: []string{"up"}})
	root.AddCommand(&cobra.Command{Use: "stop"})
	root.AddCommand(&cobra.Command{Use: "doctor"})

	names := coreCommandNames(root)
	for _, want := range []string{"start", "up", "stop", "doctor", "help", "completion"} {
		if !names[want] {
			t.Fatalf("core names missing %q", want)
		}
	}
	if names["nonexistent"] {
		t.Fatal("core names should not contain nonexistent")
	}
}

func TestPackCommandTemplateExpansion(t *testing.T) {
	result := expandScriptTemplate("{{.CityRoot}}/bin/run.sh", "/home/user/city", "mytown", "/packs/p1")
	if result != "/home/user/city/bin/run.sh" {
		t.Fatalf("expanded = %q, want %q", result, "/home/user/city/bin/run.sh")
	}
}

func TestPackCommandTemplateExpansionConfigDir(t *testing.T) {
	result := expandScriptTemplate("{{.ConfigDir}}/scripts/run.sh", "/city", "mytown", "/packs/p1")
	if result != "/packs/p1/scripts/run.sh" {
		t.Fatalf("expanded = %q, want %q", result, "/packs/p1/scripts/run.sh")
	}
}

func TestPackCommandTemplateNoTemplate(t *testing.T) {
	result := expandScriptTemplate("commands/run.sh", "/city", "mytown", "/packs/p1")
	if result != "commands/run.sh" {
		t.Fatalf("expanded = %q, want %q", result, "commands/run.sh")
	}
}

func TestPackCommandTemplateBadTemplate(t *testing.T) {
	result := expandScriptTemplate("{{.Bad", "/city", "mytown", "/packs/p1")
	if result != "{{.Bad" {
		t.Fatalf("expected graceful fallback, got %q", result)
	}
}

func TestNewRootCmdExposesRootPackCommands(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(filepath.Join(cityDir, "commands", "hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "commands", "hello", "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	backstage := findSubcommand(root, "backstage")
	if backstage == nil {
		t.Fatal("missing root pack namespace command")
	}
	if findSubcommand(backstage, "hello") == nil {
		t.Fatal("missing root pack hello command")
	}
}
