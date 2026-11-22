//go:build !windows

package main

import "github.com/gastownhall/gascity/internal/beads/contract"

// providerOwnedDirectProbeTarget supplies no endpoint beyond the canonical
// target.
func providerOwnedDirectProbeTarget(string, string) (contract.DoltConnectionTarget, bool) {
	return contract.DoltConnectionTarget{}, false
}
