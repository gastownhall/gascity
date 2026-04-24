package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectShell(t *testing.T) {
	tests := []struct {
		env  string
		want string
		ok   bool
	}{
		{"/bin/bash", "bash", true},
		{"/bin/zsh", "zsh", true},
		{"/usr/local/bin/fish", "fish", true},
		{"bash", "bash", true},
		{"/bin/ksh", "", false},
	}
	for _, tt := range tests {
		got, err := detectShell(tt.env)
		if tt.ok && err != nil {
			t.Errorf("detectShell(%q): unexpected error: %v", tt.env, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("detectShell(%q): expected error, got %q", tt.env, got)
		}
		if got != tt.want {
			t.Errorf("detectShell(%q) = %q, want %q", tt.env, got, tt.want)
		}
	}
}

func TestReplaceHookBlock(t *testing.T) {
	content := "before\n" + shellHookMarkerBegin + "\nold stuff\n" + shellHookMarkerEnd + "\nafter\n"

	// Replace.
	got := replaceHookBlock(content, shellHookMarkerBegin+"\nnew stuff\n"+shellHookMarkerEnd+"\n")
	if !strings.Contains(got, "new stuff") {
		t.Errorf("replace: expected 'new stuff', got:\n%s", got)
	}
	if strings.Contains(got, "old stuff") {
		t.Errorf("replace: should not contain 'old stuff', got:\n%s", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("replace: should preserve surrounding content, got:\n%s", got)
	}

	// Remove.
	got = replaceHookBlock(content, "")
	if strings.Contains(got, shellHookMarkerBegin) {
		t.Errorf("remove: should not contain marker, got:\n%s", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("remove: should preserve surrounding content, got:\n%s", got)
	}
}

func TestReplaceHookBlock_NoMarker(t *testing.T) {
	content := "line1\nline2\n"
	got := replaceHookBlock(content, "replacement\n")
	if got != content {
		t.Errorf("no marker: content should be unchanged, got:\n%s", got)
	}
}

func TestRCFileHasHook(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, ".zshrc")

	// File doesn't exist.
	has, err := rcFileHasHook(rc)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if has {
		t.Fatal("expected false for missing file")
	}

	// File without marker.
	shellTestWriteFile(t, rc, "export PATH=/usr/bin\n")
	has, err = rcFileHasHook(rc)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("expected false when no marker present")
	}

	// File with marker.
	shellTestWriteFile(t, rc, "stuff\n"+shellHookMarkerBegin+"\nsource foo\n"+shellHookMarkerEnd+"\n")
	has, err = rcFileHasHook(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("expected true when marker present")
	}
}

func TestRCFileAppendAndRemove(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, ".bashrc")
	shellTestWriteFile(t, rc, "# my bashrc\n")

	block := hookBlock("bash", "/home/user/.gc/completions/gc.bash")

	// Append.
	if err := rcFileAppendHook(rc, block); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(rc)
	if !strings.Contains(string(data), shellHookMarkerBegin) {
		t.Fatal("append: marker not found")
	}
	if !strings.Contains(string(data), "]] && source") {
		t.Fatal("append: guarded source line not found")
	}

	// Remove.
	if err := rcFileRemoveHook(rc); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(rc)
	if strings.Contains(string(data), shellHookMarkerBegin) {
		t.Fatal("remove: marker still present")
	}
	if !strings.Contains(string(data), "# my bashrc") {
		t.Fatal("remove: original content lost")
	}
}

func TestRCFileReplaceHook(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, ".zshrc")
	shellTestWriteFile(t, rc, "before\n"+shellHookMarkerBegin+"\nsource old\n"+shellHookMarkerEnd+"\nafter\n")

	newBlock := hookBlock("zsh", "/new/path/_gc")
	if err := rcFileReplaceHook(rc, newBlock); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(rc)
	content := string(data)
	if strings.Contains(content, "source old") {
		t.Error("old source line still present")
	}
	if !strings.Contains(content, "/new/path/_gc") {
		t.Errorf("new source line not found, got:\n%s", content)
	}
	if !strings.Contains(content, "before") || !strings.Contains(content, "after") {
		t.Errorf("surrounding content lost, got:\n%s", content)
	}
}

func TestGenerateCompletion(t *testing.T) {
	root := newRootCmd(os.Stdout, os.Stderr)
	for _, sh := range []string{"bash", "zsh", "fish"} {
		data, err := generateCompletion(root, sh)
		if err != nil {
			t.Errorf("generateCompletion(%s): %v", sh, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("generateCompletion(%s): empty output", sh)
		}
		// All completion scripts should mention "gc" somewhere.
		if !strings.Contains(string(data), "gc") {
			t.Errorf("generateCompletion(%s): output doesn't reference gc", sh)
		}
	}
}

func TestGenerateCompletion_Unsupported(t *testing.T) {
	root := newRootCmd(os.Stdout, os.Stderr)
	_, err := generateCompletion(root, "ksh")
	if err == nil {
		t.Fatal("expected error for unsupported shell")
	}
}

func TestShellInstall(t *testing.T) {
	// Override HOME so we don't touch real RC files.
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create a .zshrc to install into.
	rc := filepath.Join(home, ".zshrc")
	shellTestWriteFile(t, rc, "# zshrc\n")

	root := newRootCmd(os.Stdout, os.Stderr)
	var stdout, stderr bytes.Buffer
	code := cmdShellInstall(root, []string{"zsh"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}

	// Completion script should exist.
	compFile := filepath.Join(home, ".gc", "completions", "_gc")
	if _, err := os.Stat(compFile); err != nil {
		t.Fatalf("completion script not created: %v", err)
	}

	// RC file should have the hook.
	data, _ := os.ReadFile(rc)
	if !strings.Contains(string(data), shellHookMarkerBegin) {
		t.Error("RC file missing hook marker")
	}

	// Installing again should update (not duplicate).
	stdout.Reset()
	stderr.Reset()
	code = cmdShellInstall(root, []string{"zsh"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("re-install exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Updated") {
		t.Error("expected 'Updated' message on re-install")
	}
	data, _ = os.ReadFile(rc)
	if strings.Count(string(data), shellHookMarkerBegin) != 1 {
		t.Error("duplicate hook markers after re-install")
	}
}

func TestShellRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Set up installed state.
	compDir := filepath.Join(home, ".gc", "completions")
	shellTestMkdirAll(t, compDir)
	compFile := filepath.Join(compDir, "gc.bash")
	shellTestWriteFile(t, compFile, "# completion")

	rc := filepath.Join(home, ".bashrc")
	shellTestWriteFile(t, rc, "before\n"+hookBlock("bash", compFile)+"after\n")

	var stdout, stderr bytes.Buffer
	code := cmdShellRemove(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Removed") {
		t.Error("expected 'Removed' in output")
	}

	// Completion file should be gone.
	if _, err := os.Stat(compFile); !os.IsNotExist(err) {
		t.Error("completion file not removed")
	}

	// RC hook should be gone.
	data, _ := os.ReadFile(rc)
	if strings.Contains(string(data), shellHookMarkerBegin) {
		t.Error("hook marker still in RC file")
	}
}

func TestShellInstallFishCreatesConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := newRootCmd(os.Stdout, os.Stderr)
	var stdout, stderr bytes.Buffer
	code := cmdShellInstall(root, []string{"fish"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}

	rc := filepath.Join(home, ".config", "fish", "config.fish")
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("expected fish rc file to be created: %v", err)
	}
	if !strings.Contains(string(data), shellHookMarkerBegin) {
		t.Fatalf("fish rc file missing hook marker:\n%s", string(data))
	}
}

func TestShellStatusTracksBashProfileInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := newRootCmd(os.Stdout, os.Stderr)
	var installOut, installErr bytes.Buffer
	code := cmdShellInstall(root, []string{"bash"}, &installOut, &installErr)
	if code != 0 {
		t.Fatalf("install exit %d: %s", code, installErr.String())
	}

	bashProfile := filepath.Join(home, ".bash_profile")
	data, err := os.ReadFile(bashProfile)
	if err != nil {
		t.Fatalf("expected bash profile to be created: %v", err)
	}
	if !strings.Contains(string(data), shellHookMarkerBegin) {
		t.Fatalf("bash profile missing hook marker:\n%s", string(data))
	}

	// Simulate a later shell session creating .bashrc after install.
	shellTestWriteFile(t, filepath.Join(home, ".bashrc"), "# created later\n")

	var stdout, stderr bytes.Buffer
	code = cmdShellStatus(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("status exit %d: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "bash: installed") {
		t.Fatalf("expected bash to still be reported installed, got:\n%s", out)
	}
	if !strings.Contains(out, bashProfile) {
		t.Fatalf("expected status to point at .bash_profile, got:\n%s", out)
	}
}

func TestShellRemoveRemovesBashProfileHookAfterBashrcAppears(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := newRootCmd(os.Stdout, os.Stderr)
	var installOut, installErr bytes.Buffer
	code := cmdShellInstall(root, []string{"bash"}, &installOut, &installErr)
	if code != 0 {
		t.Fatalf("install exit %d: %s", code, installErr.String())
	}

	bashProfile := filepath.Join(home, ".bash_profile")
	shellTestWriteFile(t, filepath.Join(home, ".bashrc"), "# created later\n")

	var stdout, stderr bytes.Buffer
	code = cmdShellRemove(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("remove exit %d: %s", code, stderr.String())
	}

	data, err := os.ReadFile(bashProfile)
	if err != nil {
		t.Fatalf("reading bash profile: %v", err)
	}
	if strings.Contains(string(data), shellHookMarkerBegin) {
		t.Fatalf("expected hook to be removed from bash profile, got:\n%s", string(data))
	}
	if !strings.Contains(stdout.String(), "Removed hook from "+bashProfile) {
		t.Fatalf("expected remove output to mention bash profile, got:\n%s", stdout.String())
	}
}

func TestShellReinstallUpdatesExistingBashProfileHookAfterBashrcAppears(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := newRootCmd(os.Stdout, os.Stderr)
	var stdout, stderr bytes.Buffer
	code := cmdShellInstall(root, []string{"bash"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("first install exit %d: %s", code, stderr.String())
	}

	bashProfile := filepath.Join(home, ".bash_profile")
	bashRC := filepath.Join(home, ".bashrc")
	shellTestWriteFile(t, bashRC, "# created later\n")

	stdout.Reset()
	stderr.Reset()
	code = cmdShellInstall(root, []string{"bash"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("reinstall exit %d: %s", code, stderr.String())
	}

	profileData, err := os.ReadFile(bashProfile)
	if err != nil {
		t.Fatalf("reading bash profile: %v", err)
	}
	if strings.Count(string(profileData), shellHookMarkerBegin) != 1 {
		t.Fatalf("expected exactly one hook block in bash profile, got:\n%s", string(profileData))
	}

	rcData, err := os.ReadFile(bashRC)
	if err != nil {
		t.Fatalf("reading bashrc: %v", err)
	}
	if strings.Contains(string(rcData), shellHookMarkerBegin) {
		t.Fatalf("expected reinstall to keep hook in original bash profile, got bashrc:\n%s", string(rcData))
	}
}

func TestShellReinstallPreservesRCFileMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rc := filepath.Join(home, ".zshrc")
	shellTestWriteFile(t, rc, "# zshrc\n")
	if err := os.Chmod(rc, 0o600); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd(os.Stdout, os.Stderr)
	var stdout, stderr bytes.Buffer
	code := cmdShellInstall(root, []string{"zsh"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("first install exit %d: %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = cmdShellInstall(root, []string{"zsh"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("reinstall exit %d: %s", code, stderr.String())
	}

	info, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected rc mode 0600 after reinstall, got %03o", info.Mode().Perm())
	}
}

func TestShellRemovePreservesRCFileMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rc := filepath.Join(home, ".zshrc")
	shellTestWriteFile(t, rc, "# zshrc\n")
	if err := os.Chmod(rc, 0o600); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd(os.Stdout, os.Stderr)
	var stdout, stderr bytes.Buffer
	code := cmdShellInstall(root, []string{"zsh"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("install exit %d: %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = cmdShellRemove(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("remove exit %d: %s", code, stderr.String())
	}

	info, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected rc mode 0600 after remove, got %03o", info.Mode().Perm())
	}
}

func TestShellStatus_NotInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Create RC files so shellRCFile doesn't fall through.
	shellTestWriteFile(t, filepath.Join(home, ".bashrc"), "")
	shellTestWriteFile(t, filepath.Join(home, ".zshrc"), "")
	shellTestMkdirAll(t, filepath.Join(home, ".config", "fish"))
	shellTestWriteFile(t, filepath.Join(home, ".config", "fish", "config.fish"), "")

	var stdout, stderr bytes.Buffer
	code := cmdShellStatus(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not installed") {
		t.Errorf("expected 'not installed', got: %s", stdout.String())
	}
}

func TestShellStatus_Installed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create installed state for zsh.
	compDir := filepath.Join(home, ".gc", "completions")
	shellTestMkdirAll(t, compDir)
	shellTestWriteFile(t, filepath.Join(compDir, "_gc"), "# zsh completion")
	rc := filepath.Join(home, ".zshrc")
	shellTestWriteFile(t, rc, shellHookMarkerBegin+"\nsource foo\n"+shellHookMarkerEnd+"\n")

	var stdout, stderr bytes.Buffer
	code := cmdShellStatus(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "zsh: installed") {
		t.Errorf("expected 'zsh: installed', got: %s", out)
	}
}

func TestShellCmd_ViaCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shellTestWriteFile(t, filepath.Join(home, ".zshrc"), "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"shell", "install", "zsh"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Wrote completion script") {
		t.Errorf("expected success message, got:\n%s", stdout.String())
	}
}

func TestResolveShellArg(t *testing.T) {
	tests := []struct {
		args []string
		env  string
		want string
		ok   bool
	}{
		{[]string{"bash"}, "", "bash", true},
		{[]string{"ZSH"}, "", "zsh", true},
		{[]string{"fish"}, "", "fish", true},
		{[]string{"ksh"}, "", "", false},
		{nil, "/bin/zsh", "zsh", true},
		{nil, "/bin/ksh", "", false},
	}
	for _, tt := range tests {
		if tt.env != "" {
			t.Setenv("SHELL", tt.env)
		}
		got, err := resolveShellArg(tt.args)
		if tt.ok && err != nil {
			t.Errorf("resolveShellArg(%v): %v", tt.args, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("resolveShellArg(%v): expected error", tt.args)
		}
		if got != tt.want {
			t.Errorf("resolveShellArg(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := atomicWriteFile(path, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("got %q, want %q", data, "hello")
	}
	// Temp file should not exist.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file still exists")
	}
}

// shellTestWriteFile is a test helper that writes content to path, failing the test on error.
func shellTestWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// shellTestMkdirAll is a test helper that creates a directory tree, failing the test on error.
func shellTestMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
