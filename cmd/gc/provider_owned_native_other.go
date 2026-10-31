//go:build !windows

package main

import (
	"context"
	"io"
)

// builtinProviderOwnedOp is always nil here: the bundled gc-beads-bd script
// runs as itself, and stays the contract gc's native form is held to.
func builtinProviderOwnedOp(string, []string) func(context.Context, []string, io.Writer, []string) error {
	return nil
}
