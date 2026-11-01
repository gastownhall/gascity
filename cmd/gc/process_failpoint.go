package main

import (
	"fmt"
	"io"
	"os"
)

// killSelfAtFailpoint ends the process at a test-hooks failpoint
// (GC_LANE_FAILPOINT, GC_OPS_FAILPOINT): SIGKILL to itself, then it blocks so
// nothing more runs if the signal is late.
func killSelfAtFailpoint() {
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Kill()
	}
	select {}
}

// failpointNotice prints a failpoint's one-line notice on w:
// gc: WARNING: <env>="<raw>": <msg>. A test reads it to tell an armed gc
// from one that ignored the variable.
func failpointNotice(w io.Writer, env, raw, msg string) {
	fmt.Fprintf(w, "gc: WARNING: %s=%q: %s\n", env, raw, msg) //nolint:errcheck // best-effort notice
}
