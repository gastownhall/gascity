//go:build !windows

package runtime

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fixtureEchoArgs is a command prefix that writes each argument appended to it
// to stdout, each followed by a NUL.
const fixtureEchoArgs = `printf '%s\0'`

// decodeEchoedArgs reads fixtureEchoArgs's output back into its arguments.
func decodeEchoedArgs(out string) ([]string, error) {
	if !strings.HasSuffix(out, "\x00") {
		return nil, fmt.Errorf("output %q does not end in a NUL", out)
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

// eachShell runs fn with run executing a command in dir through
// ShellCommandContext; Unix has the one interpreter.
func eachShell(t *testing.T, fn func(t *testing.T, run func(dir, command string) (stdout, stderr string, err error))) {
	t.Helper()
	fn(t, func(dir, command string) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd, err := ShellCommandContext(ctx, command)
		if err != nil {
			return "", "", err
		}
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err = cmd.Run()
		return stdout.String(), stderr.String(), err
	})
}
