//go:build !windows

package main

import "os/exec"

// runWorkQueryCommand runs a work query to completion and returns its standard
// output. Canceling it runs the Cancel cmd carries, unchanged.
func runWorkQueryCommand(cmd *exec.Cmd) ([]byte, error) {
	return cmd.Output()
}
