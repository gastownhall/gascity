package main

import (
	"io"
	"syscall"
)

// leakedSupervisorProc is one `supervisor run` candidate read from /proc.
type leakedSupervisorProc struct {
	PID            int
	Argv           []string
	CWDState       string
	GCHomeState    string
	StartTimeTicks uint64
}

// leakedSupervisorOps is the process-table surface the sweep uses.
type leakedSupervisorOps struct {
	Discover func() ([]leakedSupervisorProc, error)
	Reread   func(pid int) (leakedSupervisorProc, bool)
	Signal   func(pid int, sig syscall.Signal) error
}

// sweepLeakedTestSupervisorsFn runs the leaked-process sweep on Linux.
var sweepLeakedTestSupervisorsFn = func(io.Writer) {}

func classifyLeakedTestSupervisor(leakedSupervisorProc, string) (bool, string) {
	return false, "not implemented"
}

func sweepLeakedTestSupervisors(io.Writer, leakedSupervisorOps, string) []int {
	return nil
}
