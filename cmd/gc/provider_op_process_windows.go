//go:build windows

package main

import "os/exec"

func prepareProviderOpCommand(cmd *exec.Cmd) {
	// Windows provider cleanup remains direct-process-only until provider ops use job objects.
	cmd.Cancel = func() error {
		if cmd == nil || cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
