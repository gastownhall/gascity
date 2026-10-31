//go:build windows

package main

import (
	"bytes"
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/genshell/genshelltest"
)

// shellLifecycleHookForTest runs command, a recovery hook gc generated, as
// shellWorkQueryWithEnv runs a program gc generated, except that the bd and gc
// it starts are the test's fakes: #!/bin/sh scripts on env's PATH, run in the
// same embedded interpreter (genshelltest.ScriptFakes). A command gc did not
// generate is an error, as it would not run in the interpreter.
func shellLifecycleHookForTest(command, dir string, env []string) (string, error) {
	generated := generatedProgramWith(command, genshelltest.ScriptFakes)
	if generated == nil {
		return "", fmt.Errorf("lifecycle hook %q is not a program gc generated", command)
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookWorkQueryTimeout)
	defer cancel()
	var stderr bytes.Buffer
	out, err := generated(ctx, dir, execenv.WithUsageMetricsDisabled(workQueryEnvForDir(env, dir)), &stderr)
	return workQueryResult(ctx, command, out, &stderr, err)
}
