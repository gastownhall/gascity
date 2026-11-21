//go:build !linux

package proxyendpoint

import "fmt"

// capturePlatformBirth reports that this host has no recipe for bd's token.
//
// bd's token scheme is Linux-specific (boot id plus /proc start time), and bd
// captures a different shape elsewhere. Rather than invent a lookalike, gc says
// so: the caller then rests its liveness proof on argv alone and the diagnostic
// records `evidence: argv`, so an operator reading it knows which half of the
// proof this host could supply.
func capturePlatformBirth(pid int) (string, error) {
	return "", fmt.Errorf("%w: pid %d", ErrBirthUnavailable, pid)
}
