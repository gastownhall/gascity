// Package censusgrowthcli implements the census growth report command.
package censusgrowthcli

import (
	"fmt"
	"io"
)

// Run reads the four census files named by args, writes the markdown report to
// stdout and returns the process exit code.
func Run(_ []string, _, stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, "census-growth: not implemented")
	return 1
}
