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

// memberStateName names a p_stat from <sys/proc.h>, or its number if unknown.
func memberStateName(stat int8) string {
	switch stat {
	case 1:
		return "SIDL"
	case 2:
		return "SRUN"
	case 3:
		return "SSLEEP"
	case 4:
		return "SSTOP"
	case szomb:
		return "SZOMB"
	}
	return fmt.Sprintf("stat=%d", stat)
}

// defaultMemberStates lists the members of process group pgid with their
// kernel states, from the same per-group process list defaultLiveMembers reads.
func defaultMemberStates(pgid int) ([]MemberState, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, fmt.Errorf("list members of process group %d: %w", pgid, err)
	}
	members := make([]MemberState, 0, len(procs))
	for i := range procs {
		members = append(members, MemberState{
			Pid:   int(procs[i].Proc.P_pid),
			State: memberStateName(procs[i].Proc.P_stat),
		})
	}
	return members, nil
}
