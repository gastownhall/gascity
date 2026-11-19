//go:build darwin

package processgroup

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// szomb is the p_stat of an exited process its parent has not yet reaped
// (SZOMB in <sys/proc.h>).
const szomb = 5

// defaultLiveMembers counts the members of process group pgid that are not
// zombies, from the kernel's per-group process list.
func defaultLiveMembers(pgid int) (int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return 0, fmt.Errorf("list members of process group %d: %w", pgid, err)
	}
	live := 0
	for i := range procs {
		if procs[i].Proc.P_stat != szomb {
			live++
		}
	}
	return live, nil
}
