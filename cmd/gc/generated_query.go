package main

import (
	"context"
	"io"
)

// generatedProgramRunner runs one program gc generated to completion, in dir
// with env, writing its standard error to stderr and returning its standard
// output. Its error is nil for exit status 0 and otherwise carries the status,
// or why the program did not finish; once ctx is done, it is ctx's error.
type generatedProgramRunner func(ctx context.Context, dir string, env []string, stderr io.Writer) ([]byte, error)
