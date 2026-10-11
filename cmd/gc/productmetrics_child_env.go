package main

import (
	"os/exec"

	"github.com/gastownhall/gascity/internal/execenv"
)

// disableProductMetricsForChild applies the Gas City usage-metrics recursion
// guard without changing any other explicit or inherited child environment.
// Call it after configuring cmd.Dir and cmd.Env so nil-Env materialization uses
// exec.Cmd's final PWD semantics.
func disableProductMetricsForChild(cmd *exec.Cmd) {
	environ := cmd.Env
	if environ == nil {
		environ = cmd.Environ()
	}
	cmd.Env = execenv.WithUsageMetricsDisabled(environ)
}
