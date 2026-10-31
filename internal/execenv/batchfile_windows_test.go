//go:build windows

package execenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckBatchFileArgsRefusesWhatCmdExeWouldRead pins that a batch-file
// program never receives an argument cmd.exe would act on. Windows starts a
// batch file through cmd.exe, which reads the command line again after Go has
// quoted it, so an & in an argument runs a command and a %NAME% expands.
func TestCheckBatchFileArgsRefusesWhatCmdExeWouldRead(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "tool.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{
		"a&echo>pwned", "a|b", "a<b", "a>b", "a^b", "(a)", "100%", "%PATH%", "!x!",
		`say "hi"`, "line1\nline2", "line1\rline2",
	} {
		cmd := unstartedCmd(shim, "--title", arg)
		if err := CheckBatchFileArgs(cmd); err == nil || !strings.Contains(err.Error(), "batch file") {
			t.Errorf("CheckBatchFileArgs(%q) = %v, want a refusal", arg, err)
		}
	}
	for _, arg := range []string{"plain", "a title with spaces", "it's fine: a/b.c, d=e; #1 @x +y ~z [k] {v} `t` $HOME"} {
		if err := CheckBatchFileArgs(unstartedCmd(shim, "--title", arg)); err != nil {
			t.Errorf("CheckBatchFileArgs(%q) = %v, want it allowed", arg, err)
		}
	}
	native, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckBatchFileArgs(unstartedCmd(native, "a&b", "%PATH%", `"q"`)); err != nil {
		t.Errorf("CheckBatchFileArgs(native) = %v, want nil: a native program receives its argv as given", err)
	}
}

// TestCheckBatchFileArgsJudgesTheFileWindowsRuns pins that the guard judges
// the file Windows starts, not the spelling of the path. Windows normalizes a
// path before running it: . and .. components fold away lexically, even after
// a file name, and trailing dots and spaces leave the last component, so
// tool.cmd., tool.cmd\. and tool.cmd\x\.. all run tool.cmd. And os/exec adds a
// PATHEXT extension at Start to a path that has none, relative to cmd.Dir when
// the path is relative, so tool runs tool.cmd.
func TestCheckBatchFileArgsJudgesTheFileWindowsRuns(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "tool.cmd")
	for _, name := range []string{"tool.cmd", "tool.bat"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("@echo off\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "native.exe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unc := `\\localhost\` + dir[:1] + `$` + dir[2:]
	for _, tc := range []struct {
		name, path, dir string
	}{
		{"trailing dot", shim + ".", ""},
		{"trailing dots", shim + "..", ""},
		{"trailing dot space dot", shim + ". .", ""},
		{"trailing space", shim + " ", ""},
		{"upper-case extension", filepath.Join(dir, "TOOL.CMD"), ""},
		{"absolute path without extension", filepath.Join(dir, "tool"), ""},
		{"bare name resolved in cmd.Dir", "tool", dir},
		{"relative path resolved in cmd.Dir", filepath.Join(".", "tool"), dir},
		{"dot component after the file", shim + `\.`, ""},
		{"dot component after a forward slash", shim + `/.`, ""},
		{"dot components after the file", shim + `\.\.`, ""},
		{"doubled separator before a dot component", shim + `\\.`, ""},
		{"space before a dot component", shim + ` \.`, ""},
		{"trailing dot before a dot component", shim + `.\.`, ""},
		{"dot-dot component after the file", shim + `\x\..`, ""},
		{"dot-dot then dot after the file", shim + `\x\..\.`, ""},
		{"dot-dot over two components", shim + `\x\y\..\..`, ""},
		{"dot-dot over a dotted component", shim + `\x.\..`, ""},
		{"dot-dot over a space component", shim + `\ \..`, ""},
		{"dot-dot over a three-dot component", shim + `\...\..`, ""},
		{"dot-dot with forward slashes", shim + `/x/..`, ""},
		{"trailing dot space before dot-dot", shim + `. \x\..`, ""},
		{"dot-dot before the file and a dot after", filepath.Join(dir, "x") + `\..\tool.cmd\.`, ""},
		{"bat dot component after the file", filepath.Join(dir, "tool.bat") + `\.`, ""},
		{"bat dot-dot component after the file", filepath.Join(dir, "tool.bat") + `\x\..`, ""},
		{`\\.\ device path with a dot component`, `\\.\` + shim + `\.`, ""},
		{`\\.\ device path with a dot-dot component`, `\\.\` + shim + `\x\..`, ""},
		{"UNC path with a dot component", unc + `\tool.cmd\.`, ""},
		{"UNC path with a space before a dot component", unc + `\tool.cmd \.`, ""},
		{"relative dot component resolved in cmd.Dir", `tool.cmd\.`, dir},
		{"relative dot-dot component resolved in cmd.Dir", `x\..\tool.cmd\x\..`, dir},
	} {
		cmd := unstartedCmd(tc.path, "a&echo>pwned")
		cmd.Dir = tc.dir
		if err := CheckBatchFileArgs(cmd); err == nil || !strings.Contains(err.Error(), "batch file") {
			t.Errorf("%s: CheckBatchFileArgs(%q) = %v, want a refusal", tc.name, tc.path, err)
		}
		if !IsBatchFile(tc.path) && tc.dir == "" {
			t.Errorf("%s: IsBatchFile(%q) = false, want true", tc.name, tc.path)
		}
	}
	for _, path := range []string{
		filepath.Join(dir, "native"), filepath.Join(dir, "native.exe"), filepath.Join(dir, "missing"),
		filepath.Join(dir, "native.exe") + `\.`, filepath.Join(dir, "native.exe") + `\x\..`,
	} {
		if err := CheckBatchFileArgs(unstartedCmd(path, "a&b", "%PATH%")); err != nil {
			t.Errorf("CheckBatchFileArgs(%q) = %v, want nil: it does not run a batch file", path, err)
		}
		if IsBatchFile(path) {
			t.Errorf("IsBatchFile(%q) = true, want false", path)
		}
	}
}

// unstartedCmd builds the command exec.Command would for an absolute path.
// Nothing here starts a process.
func unstartedCmd(path string, args ...string) *exec.Cmd {
	return &exec.Cmd{Path: path, Args: append([]string{path}, args...)}
}
