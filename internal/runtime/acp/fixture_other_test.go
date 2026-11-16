//go:build !windows

package acp

import (
	"fmt"
	"strings"
)

// fixtureCaptureArgsCommand writes each argument appended to it to the file
// $GC_ARGS_FILE names, each followed by a NUL.
const fixtureCaptureArgsCommand = `sh -c 'printf "%s\0" "$@" > "$GC_ARGS_FILE"' capture`

// decodeCapturedArgs reads fixtureCaptureArgsCommand's file back into its
// arguments.
func decodeCapturedArgs(out string) ([]string, error) {
	if !strings.HasSuffix(out, "\x00") {
		return nil, fmt.Errorf("captured %q does not end in a NUL", out)
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

// fixtureIdleAgentCommand runs until its stdin closes and answers nothing.
const fixtureIdleAgentCommand = `cat >/dev/null`
