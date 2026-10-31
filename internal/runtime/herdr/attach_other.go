//go:build !windows

package herdr

import (
	"os"
	"os/exec"
)

// Attach runs `herdr agent attach`, blocking until the user detaches.
func (p *Provider) Attach(name string) error {
	cmd := exec.Command(p.c.bin, "--session", p.c.session, "agent", "attach", herdrAgentName(name))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run() // blocks until the user detaches
}
