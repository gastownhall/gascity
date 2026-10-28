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

// Command text for the portable setup-command tests, written in the
// interpreter the platform actually runs. The tests are about
// RunSetupCommand's contract — working directory, environment, captured
// streams, exit status, bounded wait — none of which is Unix-specific, so the
// only thing that needs splitting is the script.

// fixturePrintCwdToFile writes the command's working directory to out.txt,
// named relatively so it proves where the command ran.
const fixturePrintCwdToFile = "pwd > out.txt"

// fixtureBothStreamsExit3 writes to both streams and fails with a distinctive
// code.
const fixtureBothStreamsExit3 = "echo out; echo err >&2; exit 3"

// fixtureSleepPastTimeout outruns any per-command timeout a test sets.
const fixtureSleepPastTimeout = "sleep 5"

// fixtureBackgroundChildExit0 leaves a child holding the inherited stdio and
// exits successfully, which is what the WaitDelay bound exists for.
const fixtureBackgroundChildExit0 = "sleep 30 & exit 0"

// fixtureEchoEnvSecretsExit3 echoes a session credential and an inherited one
// to opposite streams before failing.
const fixtureEchoEnvSecretsExit3 = `echo "session=$GH_TOKEN" >&2; echo "inherited=$ANTHROPIC_AUTH_TOKEN"; exit 3`

// fixtureRequireEnv fails unless key holds value in the command's environment.
func fixtureRequireEnv(key, value string) string {
	return fmt.Sprintf(`[ "$%s" = %s ]`, key, value)
}

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
