//go:build windows

package main

import "os/exec"

func prepareProviderOpCommand(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd == nil || cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
