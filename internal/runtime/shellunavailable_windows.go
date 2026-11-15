//go:build windows

package runtime

import (
	"fmt"
	"path/filepath"
	"strings"
)

var posixShells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "ash": true}

// ShellCommandUnavailable refuses a command whose first word is a POSIX shell or .sh script.
func ShellCommandUnavailable(command string) string {
	f := strings.Fields(command)
	if len(f) == 0 {
		return ""
	}
	base := strings.ToLower(filepath.Base(strings.Trim(f[0], `"'`)))
	if strings.HasSuffix(base, ".sh") {
		return fmt.Sprintf("its program %s is a POSIX shell script, and Gas City runs no POSIX shell on Windows", f[0])
	}
	if posixShells[strings.TrimSuffix(base, ".exe")] {
		return fmt.Sprintf("its program %s is a POSIX shell, and Gas City runs no POSIX shell on Windows", f[0])
	}
	return ""
}

// ShellCommandPrograms answers only for a plain command; nil keeps the process scan strict.
func ShellCommandPrograms(command string) []string {
	if f := strings.Fields(command); len(f) > 0 && !strings.ContainsAny(command, ";|&$(){}`\"'") {
		return []string{f[0]}
	}
	return nil
}
