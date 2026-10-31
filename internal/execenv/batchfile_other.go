//go:build !windows

package execenv

import "os/exec"

// CheckBatchFileArgs reports whether cmd may carry its arguments to its
// program. Outside Windows it always may: a program receives its argv as given,
// with no command interpreter between.
func CheckBatchFileArgs(*exec.Cmd) error {
	return nil
}

// IsBatchFile reports whether the program at path runs through a command
// interpreter that re-reads its command line. Outside Windows none does.
func IsBatchFile(string) bool {
	return false
}
