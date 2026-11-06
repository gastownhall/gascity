//go:build !unix

package nudgepoller

import "os/exec"

// ScrubInheritedFDs is a no-op off unix: there is no descriptor-number
// inheritance to scrub, and os/exec passes no handle beyond stdio.
func ScrubInheritedFDs(*exec.Cmd) error { return nil }
