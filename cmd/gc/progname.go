package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// binaryName is the build-time default, overridable via ldflags:
//
//	-X main.binaryName=city
var binaryName = "gc"

var (
	progOnce   sync.Once
	progCached string
)

// prog returns the binary name for use in error messages, help text, and
// examples. It prefers os.Args[0] at runtime, falling back to the
// build-time binaryName.
func prog() string {
	progOnce.Do(func() {
		if len(os.Args) > 0 && os.Args[0] != "" {
			name := filepath.Base(os.Args[0])
			name = strings.TrimSuffix(name, ".exe")
			if strings.HasSuffix(name, ".test") {
				progCached = binaryName
			} else {
				progCached = name
			}
		} else {
			progCached = binaryName
		}
	})
	return progCached
}

// cmdName returns "prog subcmd" for error message prefixes.
func cmdName(subcmd string) string {
	if subcmd == "" {
		return prog()
	}
	return prog() + " " + subcmd
}

// cmdErr writes a formatted error message prefixed with the binary and
// subcommand name to w: "<prog> <subcmd>: <msg>\n".
func cmdErr(w io.Writer, subcmd string, err error) {
	fmt.Fprintf(w, "%s: %v\n", cmdName(subcmd), err) //nolint:errcheck // best-effort stderr
}
