package main

import (
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/convergence"
)

// controllerTokenChildSpyPath names the file a re-executed test binary writes
// its view of the controller token into. It is set only on the child.
const controllerTokenChildSpyPath = "GC_TEST_CONTROLLER_TOKEN_CHILD_SPY"

// maybeRunControllerTokenChildSpy turns this process into the child half of
// TestPassthroughEnvWithholdsControllerTokenFromChildProcess when the spy path
// is set, recording whether the controller token reached it and with what
// value. It must run before the test framework starts, so TestMain calls it
// first.
//
// The distinction being recorded is present-but-empty versus absent. Deleting
// the key satisfies every assertion made against the overlay map and still
// lets a child inherit the controller's value, which is how the leak shipped.
func maybeRunControllerTokenChildSpy() {
	path := os.Getenv(controllerTokenChildSpyPath)
	if path == "" {
		return
	}
	record := "ABSENT"
	if value, present := os.LookupEnv(convergence.TokenEnvVar); present {
		record = "[" + value + "]"
	}
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "writing controller token child spy: %v\n", err) //nolint:errcheck
		os.Exit(97)
	}
	os.Exit(0)
}
