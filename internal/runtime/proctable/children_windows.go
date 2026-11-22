//go:build windows

package proctable

import "time"

// ChildProcess is one child of a running process.
type ChildProcess struct {
	PID int
	// Name is the image file name, extension included, as Windows reports it.
	// Compare it through ProcessNameKey.
	Name string
	// Created is when the child started.
	Created time.Time
}

// ChildProcesses returns the running children of the process pid, read from
// one snapshot of the process table. The snapshot opens no process, so a child
// this user could not inspect is still reported.
//
// A row counts as a child only when the table can vouch for the link, which is
// the test processTable.parent applies: Windows never clears a parent PID, so a
// row naming pid is a child only if it was created no earlier than pid's own
// process. Without that, a process whose real parent exited long ago would be
// reported as a child of whichever process was later given the same PID.
//
// A pid that is not running has no children, and that is not an error.
func ChildProcesses(pid int) ([]ChildProcess, error) {
	rows, err := systemProcesses()
	if err != nil {
		return nil, err
	}
	return childrenInTable(indexProcessTable(rows), pid), nil
}

func childrenInTable(table processTable, pid int) []ChildProcess {
	var children []ChildProcess
	for _, row := range table.rows {
		if row.ppid != pid {
			continue
		}
		if parent, ok := table.parent(row); !ok || parent.pid != pid {
			continue
		}
		children = append(children, ChildProcess{PID: row.pid, Name: row.image, Created: time.Unix(0, row.creation)})
	}
	return children
}
