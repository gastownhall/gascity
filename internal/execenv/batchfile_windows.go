//go:build windows

package execenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// batchFileMetacharacters are the characters cmd.exe acts on in a batch
// file's command line however Go quoted the argument holding them.
const batchFileMetacharacters = "\"%!^&|<>()\r\n\x00"

// CheckBatchFileArgs refuses cmd when its program is a batch file (.cmd or
// .bat) and an argument holds a character cmd.exe acts on. Windows starts a
// batch file through cmd.exe, which parses the command line again after Go has
// quoted each argument for a native program: & | < > run commands, ( ) group
// them, ^ escapes, %NAME% and !NAME! expand, " ends the quoting Go added, and a
// line break ends the command. No quoting prevents that, so a caller that
// passes data as arguments checks the command before starting it. Any other
// program receives its arguments as given and is never refused.
//
// The program is the file Windows will run, not the spelling of cmd.Path: Start
// adds a PATHEXT extension to a path that has none (in cmd.Dir when the path is
// relative), and Windows normalizes the path before running it, folding . and
// .. components away even after a file name and dropping trailing dots and
// spaces from the last component, so "tool", "tool.cmd.", "tool.cmd\." and
// "tool.cmd\x\.." all run tool.cmd. Set cmd.Dir before checking.
func CheckBatchFileArgs(cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	program := startedProgram(cmd.Path, cmd.Dir)
	if !isBatchFile(program) {
		return nil
	}
	for i, arg := range cmd.Args[1:] {
		if j := strings.IndexAny(arg, batchFileMetacharacters); j >= 0 {
			return fmt.Errorf("refusing to run the batch file %s: argument %d holds %q, which cmd.exe acts on whatever the quoting; use the program's native executable rather than a .cmd or .bat shim", program, i+1, arg[j:j+1])
		}
	}
	return nil
}

// IsBatchFile reports whether Windows runs the program at path, an absolute
// path or one relative to the working directory, as a batch file.
func IsBatchFile(path string) bool {
	return isBatchFile(startedProgram(path, ""))
}

func isBatchFile(program string) bool {
	ext := strings.ToLower(filepath.Ext(program))
	return ext == ".cmd" || ext == ".bat"
}

// startedProgram names the file Windows runs for a command whose Path is path
// and whose Dir is dir: the extension os/exec's Start resolves, then the path
// as Win32 normalizes it before CreateProcess opens it (GetFullPathNameW, which
// syscall.StartProcess also applies to a path relative to Dir). That folds .
// and .. components lexically, even after a file name, and drops trailing dots
// and spaces from the last component. A path whose extension does not resolve
// gets none added; Start fails to run it.
func startedProgram(path, dir string) string {
	if resolved, err := resolveProgramExtension(path, dir); err == nil {
		path = resolved
	}
	if dir != "" && filepath.VolumeName(path) == "" && (path == "" || !os.IsPathSeparator(path[0])) {
		path = dir + string(filepath.Separator) + path
	}
	if full, err := syscall.FullPath(path); err == nil {
		path = full
	} else {
		path = filepath.Clean(path)
	}
	return strings.TrimRight(path, ". ")
}

// resolveProgramExtension mirrors os/exec's lookExtensions, which Cmd.Start
// applies to Cmd.Path on Windows: a path whose extension is in PATHEXT is
// taken as is; otherwise LookPath adds the first PATHEXT extension that names
// a file, looking in dir when the path is relative.
func resolveProgramExtension(path, dir string) (string, error) {
	if filepath.Base(path) == path {
		path = "." + string(filepath.Separator) + path
	}
	if ext := filepath.Ext(path); ext != "" {
		for _, e := range pathExtensions() {
			if strings.EqualFold(ext, e) {
				return path, nil
			}
		}
	}
	if dir == "" || filepath.VolumeName(path) != "" || (len(path) > 1 && os.IsPathSeparator(path[0])) {
		return exec.LookPath(path)
	}
	joined := filepath.Join(dir, path)
	resolved, err := exec.LookPath(joined)
	if err != nil {
		return "", err
	}
	return path + strings.TrimPrefix(resolved, joined), nil
}

// pathExtensions is os/exec's PATHEXT list.
func pathExtensions() []string {
	value := os.Getenv("PATHEXT")
	if value == "" {
		return []string{".com", ".exe", ".bat", ".cmd"}
	}
	var exts []string
	for _, e := range strings.Split(strings.ToLower(value), ";") {
		if e == "" {
			continue
		}
		if e[0] != '.' {
			e = "." + e
		}
		exts = append(exts, e)
	}
	return exts
}
