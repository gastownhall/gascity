package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func TestAddDiscoveredCommandsToRoot_BuildsBindingScopedNestedTree(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "start"})

	entries := []config.DiscoveredCommand{
		{
			BindingName: "gs",
			Command:     []string{"status"},
			Description: "Show status",
		},
		{
			BindingName: "gs",
			Command:     []string{"repo", "sync"},
			Description: "Sync repo",
		},
	}

	addDiscoveredCommandsToRoot(root, entries, "/city", "testcity", os.Stdout, os.Stderr, true)

	gs := findSubcommand(root, "gs")
	if gs == nil {
		t.Fatal("missing binding namespace command")
	}
	if findSubcommand(gs, "status") == nil {
		t.Fatal("missing status leaf under binding namespace")
	}
	repo := findSubcommand(gs, "repo")
	if repo == nil {
		t.Fatal("missing nested repo namespace")
	}
	sync := findSubcommand(repo, "sync")
	if sync == nil {
		t.Fatal("missing nested sync leaf")
	}
	if !sync.DisableFlagParsing {
		t.Fatal("sync leaf DisableFlagParsing = false, want true")
	}
}

func TestRunDiscoveredCommand_UsesPackContext(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "pack")
	sourceDir := filepath.Join(packDir, "commands", "status")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(sourceDir, "run.sh")
	script := `#!/bin/sh
echo "packdir=$GC_PACK_DIR"
echo "packname=$GC_PACK_NAME"
echo "cityname=$GC_CITY_NAME"
echo "args=$*"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		BindingName: "gs",
		PackName:    "mypack",
		Command:     []string{"status"},
		RunScript:   scriptPath,
		PackDir:     packDir,
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", []string{"hello", "world"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "packdir="+packDir) {
		t.Fatalf("stdout missing pack dir, got:\n%s", out)
	}
	if !strings.Contains(out, "packname=mypack") {
		t.Fatalf("stdout missing pack name, got:\n%s", out)
	}
	if !strings.Contains(out, "cityname=testcity") {
		t.Fatalf("stdout missing city name, got:\n%s", out)
	}
	if !strings.Contains(out, "args=hello world") {
		t.Fatalf("stdout missing args, got:\n%s", out)
	}
}

func TestRunDiscoveredCommand_PrefersEntryPackDir(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "actual-pack")
	sourceDir := filepath.Join(dir, "somewhere", "else", "commands", "status")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(sourceDir, "run.sh")
	script := `#!/bin/sh
echo "packdir=$GC_PACK_DIR"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		BindingName: "gs",
		PackName:    "mypack",
		Command:     []string{"status"},
		RunScript:   scriptPath,
		PackDir:     packDir,
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "packdir="+packDir) {
		t.Fatalf("stdout missing composed pack dir, got:\n%s", stdout.String())
	}
}

func TestPackRootFromEntryDir_UsesLastTopLevelSegment(t *testing.T) {
	sourceDir := filepath.Join("/workspace", "commands", "mypk", "commands", "status")
	got := packRootFromEntryDir(sourceDir, "commands")
	want := filepath.Join("/workspace", "commands", "mypk")
	if got != want {
		t.Fatalf("packRootFromEntryDir(%q) = %q, want %q", sourceDir, got, want)
	}
}

func TestPackRootFromEntryDir_FallsBackToParent(t *testing.T) {
	sourceDir := filepath.Join("/workspace", "misc", "status")
	got := packRootFromEntryDir(sourceDir, "commands")
	want := filepath.Dir(sourceDir)
	if got != want {
		t.Fatalf("packRootFromEntryDir(%q) = %q, want %q", sourceDir, got, want)
	}
}

func TestRunDiscoveredCommand_ExitCodePropagates(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "commands", "fail")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		PackName:  "mypack",
		Command:   []string{"fail"},
		RunScript: scriptPath,
		SourceDir: sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
}

func TestRunDiscoveredCommand_MissingScriptFails(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "commands", "missing")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		BindingName: "gs",
		PackName:    "mypack",
		Command:     []string{"missing"},
		RunScript:   filepath.Join(sourceDir, "run.sh"),
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no such file") {
		t.Fatalf("stderr missing missing-file message, got:\n%s", stderr.String())
	}
}

func TestRunDiscoveredCommand_NonExecutableFails(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "commands", "nonexec")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		BindingName: "gs",
		PackName:    "mypack",
		Command:     []string{"nonexec"},
		RunScript:   scriptPath,
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Fatalf("stderr missing permission error, got:\n%s", stderr.String())
	}
}

func TestRunDiscoveredCommand_PassthroughArgs(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "commands", "echo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	script := `#!/bin/sh
for arg in "$@"; do
	echo "arg:$arg"
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		PackName:  "mypack",
		Command:   []string{"echo"},
		RunScript: scriptPath,
		SourceDir: sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", []string{"--verbose", "-n", "3", "hello world"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"arg:--verbose", "arg:-n", "arg:3", "arg:hello world"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q, got:\n%s", want, out)
		}
	}
}

func TestAddDiscoveredCommandsToRoot_HelpFlagShowsBuiltInHelp(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "pack")
	sourceDir := filepath.Join(packDir, "commands", "status")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho should-not-run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	helpPath := filepath.Join(sourceDir, "help.md")
	if err := os.WriteFile(helpPath, []byte("Long discovered help.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := &cobra.Command{Use: "gc"}
	addDiscoveredCommandsToRoot(root, []config.DiscoveredCommand{{
		BindingName: "ops",
		PackName:    "ops",
		Command:     []string{"status"},
		Description: "Show status",
		RunScript:   scriptPath,
		HelpFile:    helpPath,
		SourceDir:   sourceDir,
		PackDir:     packDir,
	}}, dir, "testcity", &bytes.Buffer{}, &bytes.Buffer{}, true)

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"ops", "status", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr=%s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Long discovered help.") {
		t.Fatalf("stdout missing built-in help text, got:\n%s", out)
	}
	if strings.Contains(out, "should-not-run") {
		t.Fatalf("help should not execute the discovered command, got:\n%s", out)
	}
}

func TestAddDiscoveredCommandsToRoot_CollisionProtection(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "start"})

	entries := []config.DiscoveredCommand{
		{
			BindingName: "start",
			Command:     []string{"status"},
			Description: "Show status",
		},
	}

	var stdout, stderr bytes.Buffer
	addDiscoveredCommandsToRoot(root, entries, "/city", "testcity", &stdout, &stderr, true)

	if !strings.Contains(stderr.String(), "shadows core command") {
		t.Fatalf("expected collision warning, got stderr: %q", stderr.String())
	}
	startCount := 0
	for _, c := range root.Commands() {
		if c.Name() == "start" {
			startCount++
		}
	}
	if startCount != 1 {
		t.Fatalf("got %d start commands, want 1", startCount)
	}
}

func TestTryDiscoveredCommandFallback_PrefersLongestMatch(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "pack", "commands", "repo")
	syncDir := filepath.Join(dir, "pack", "commands", "repo-sync")
	for _, p := range []string{repoDir, syncDir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "run.sh"), []byte("#!/bin/sh\necho repo:$*\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, "run.sh"), []byte("#!/bin/sh\necho sync:$*\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "testcity"},
		PackCommands: []config.DiscoveredCommand{
			{
				BindingName: "gs",
				PackName:    "mypack",
				Command:     []string{"repo"},
				RunScript:   filepath.Join(repoDir, "run.sh"),
				SourceDir:   repoDir,
			},
			{
				BindingName: "gs",
				PackName:    "mypack",
				Command:     []string{"repo", "sync"},
				RunScript:   filepath.Join(syncDir, "run.sh"),
				SourceDir:   syncDir,
			},
		},
	}

	var stdout, stderr bytes.Buffer
	ok := tryDiscoveredCommandFallback([]string{"gs", "repo", "sync", "now"}, cfg, dir, &stdout, &stderr)
	if !ok {
		t.Fatal("tryDiscoveredCommandFallback returned false, want true")
	}
	if !strings.Contains(stdout.String(), "sync:now") {
		t.Fatalf("stdout missing longest-match execution, got:\n%s", stdout.String())
	}
}

func TestTryDiscoveredCommandFallback_HelpFlagShowsHelpWithoutRunning(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "pack", "commands", "status")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho should-not-run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	helpPath := filepath.Join(sourceDir, "help.md")
	if err := os.WriteFile(helpPath, []byte("Status help from pack.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "testcity"},
		PackCommands: []config.DiscoveredCommand{{
			BindingName: "gs",
			PackName:    "mypack",
			Command:     []string{"status"},
			Description: "Show status",
			RunScript:   scriptPath,
			HelpFile:    helpPath,
			SourceDir:   sourceDir,
		}},
	}

	var stdout, stderr bytes.Buffer
	ok := tryDiscoveredCommandFallback([]string{"gs", "status", "--help"}, cfg, dir, &stdout, &stderr)
	if !ok {
		t.Fatal("tryDiscoveredCommandFallback returned false, want true")
	}
	out := stdout.String()
	if !strings.Contains(out, "Status help from pack.") {
		t.Fatalf("stdout missing discovered help, got:\n%s", out)
	}
	if strings.Contains(out, "should-not-run") {
		t.Fatalf("help should not execute the discovered command, got:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestTryDiscoveredCommandFallback_HelpAfterTerminatorPassesThrough(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "pack", "commands", "status")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf 'args=%s %s\\n' \"$1\" \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	helpPath := filepath.Join(sourceDir, "help.md")
	if err := os.WriteFile(helpPath, []byte("Status help from pack.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "testcity"},
		PackCommands: []config.DiscoveredCommand{{
			BindingName: "gs",
			PackName:    "mypack",
			Command:     []string{"status"},
			Description: "Show status",
			RunScript:   scriptPath,
			HelpFile:    helpPath,
			SourceDir:   sourceDir,
		}},
	}

	var stdout, stderr bytes.Buffer
	ok := tryDiscoveredCommandFallback([]string{"gs", "status", "--", "--help"}, cfg, dir, &stdout, &stderr)
	if !ok {
		t.Fatal("tryDiscoveredCommandFallback returned false, want true")
	}
	out := stdout.String()
	if !strings.Contains(out, "args=-- --help") {
		t.Fatalf("stdout missing script passthrough args, got:\n%s", out)
	}
	if strings.Contains(out, "Status help from pack.") {
		t.Fatalf("terminator should pass --help through to the script, got:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestTryDiscoveredCommandFallback_NamespaceHelpListsChildren(t *testing.T) {
	dir := t.TempDir()
	repoSyncDir := filepath.Join(dir, "pack", "commands", "repo", "sync")
	repoCleanDir := filepath.Join(dir, "pack", "commands", "repo", "clean")
	for _, p := range []string{repoSyncDir, repoCleanDir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "run.sh"), []byte("#!/bin/sh\necho should-not-run\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "testcity"},
		PackCommands: []config.DiscoveredCommand{
			{
				BindingName: "gs",
				PackName:    "mypack",
				Command:     []string{"repo", "sync"},
				Description: "Sync repo",
				RunScript:   filepath.Join(repoSyncDir, "run.sh"),
				SourceDir:   repoSyncDir,
			},
			{
				BindingName: "gs",
				PackName:    "mypack",
				Command:     []string{"repo", "clean"},
				Description: "Clean repo",
				RunScript:   filepath.Join(repoCleanDir, "run.sh"),
				SourceDir:   repoCleanDir,
			},
		},
	}

	var stdout, stderr bytes.Buffer
	ok := tryDiscoveredCommandFallback([]string{"gs", "repo", "--help"}, cfg, dir, &stdout, &stderr)
	if !ok {
		t.Fatal("tryDiscoveredCommandFallback returned false, want true")
	}
	out := stdout.String()
	for _, want := range []string{"Available commands for gs repo:", "clean", "Clean repo", "sync", "Sync repo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "should-not-run") {
		t.Fatalf("namespace help should not execute a discovered command, got:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestPrintDiscoveredCommandHelpFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry config.DiscoveredCommand
		want  string
	}{
		{
			name:  "description",
			entry: config.DiscoveredCommand{Command: []string{"status"}, Description: "Show status"},
			want:  "Show status\n",
		},
		{
			name:  "generic",
			entry: config.DiscoveredCommand{Command: []string{"repo", "sync"}},
			want:  "Pack command: repo sync\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			printDiscoveredCommandHelp(&stdout, tc.entry)
			if got := stdout.String(); got != tc.want {
				t.Fatalf("stdout = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrintDiscoveredCommandListFiltersPrefixAndSkipsExactNamespace(t *testing.T) {
	entries := []config.DiscoveredCommand{
		{Command: []string{"repo"}, Description: "Repo namespace"},
		{Command: []string{"repo", "sync"}, Description: "Sync repo"},
		{Command: []string{"repo", "clean"}, Description: "Clean repo"},
		{Command: []string{"status"}, Description: "Show status"},
	}

	var stdout bytes.Buffer
	printDiscoveredCommandList(&stdout, "gs", []string{"repo"}, entries)

	out := stdout.String()
	for _, want := range []string{
		"Available commands for gs repo:",
		"sync",
		"Sync repo",
		"clean",
		"Clean repo",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q, got:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"Repo namespace", "status", "Show status"} {
		if strings.Contains(out, notWant) {
			t.Fatalf("stdout unexpectedly contained %q, got:\n%s", notWant, out)
		}
	}
}

func TestDiscoveredCommandPrefixHelpers(t *testing.T) {
	entries := []config.DiscoveredCommand{{Command: []string{"repo", "sync"}}}
	if !discoveredCommandPrefixExists(entries, []string{"repo"}) {
		t.Fatal("expected repo prefix to exist")
	}
	if discoveredCommandPrefixExists(entries, []string{"missing"}) {
		t.Fatal("missing prefix unexpectedly exists")
	}
	if commandHasPrefix([]string{"repo"}, []string{"repo", "sync"}) {
		t.Fatal("short command unexpectedly matched longer prefix")
	}
}

func TestAddDiscoveredCommandsToRoot_DedupsDuplicateLeaf(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	entries := []config.DiscoveredCommand{
		{BindingName: "gs", Command: []string{"status"}, Description: "first"},
		{BindingName: "gs", Command: []string{"status"}, Description: "second"},
	}

	addDiscoveredCommandsToRoot(root, entries, "/city", "testcity", os.Stdout, os.Stderr, true)
	gs := findSubcommand(root, "gs")
	if gs == nil {
		t.Fatal("missing binding namespace")
	}
	count := 0
	for _, c := range gs.Commands() {
		if c.Name() == "status" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d status commands, want 1", count)
	}
}

func TestAddDiscoveredCommandsToRoot_CanSuppressCollisionWarnings(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "import"})

	entries := []config.DiscoveredCommand{
		{
			BindingName: "import",
			Command:     []string{"list"},
			Description: "Show imports",
		},
	}

	var stdout, stderr bytes.Buffer
	addDiscoveredCommandsToRoot(root, entries, "/city", "testcity", &stdout, &stderr, false)

	if stderr.Len() != 0 {
		t.Fatalf("expected suppressed collision warning, got stderr: %q", stderr.String())
	}
	importCount := 0
	for _, c := range root.Commands() {
		if c.Name() == "import" {
			importCount++
		}
	}
	if importCount != 1 {
		t.Fatalf("got %d import commands, want 1", importCount)
	}
}
