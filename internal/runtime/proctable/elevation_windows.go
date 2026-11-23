//go:build windows

package proctable

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// CurrentIntegrity returns this process's mandatory integrity level, as the RID
// of its token's label: 0x2000 for a standard user, 0x3000 elevated.
func CurrentIntegrity() (uint32, error) {
	self, err := currentProcessOwner()
	if err != nil {
		return 0, err
	}
	return self.integrity, nil
}

// ElevatedProcessesSince returns this user's processes, created at or after
// since, that run at a higher integrity level than this process. Windows keeps
// a process from reading the memory of one above its own integrity, so these
// are the processes of this user that no scan from here can inspect.
func ElevatedProcessesSince(since time.Time) ([]ProcessRecord, error) {
	table, err := readProcessTable()
	if err != nil {
		return nil, err
	}
	return elevatedSince(table.rows, since), nil
}

// elevatedSince keeps the elevated rows created at or after since. A row the
// snapshot carried no creation time for is kept too: nothing shows it predates
// since.
func elevatedSince(rows []windowsProcess, since time.Time) []ProcessRecord {
	var found []ProcessRecord
	for _, row := range rows {
		if row.inspection != inspectionElevated {
			continue
		}
		if row.creationOK && row.creation < since.UnixNano() {
			continue
		}
		found = append(found, ProcessRecord{
			PID:              row.pid,
			PPID:             row.ppid,
			StartTime:        row.startIdentity(),
			Name:             row.image,
			SessionIDUnknown: true,
		})
	}
	return found
}

// systemProcessID is the PID Windows gives the System process.
const systemProcessID = 4

// CurrentBoot identifies the running boot by the creation time, in Unix
// nanoseconds, of the System process. Windows creates that process once per
// boot and stamps its creation time once, so every reader in a boot gets the
// same value however the clock is changed afterwards, and a later boot gets
// another. A boot time worked out from the clock, such as now minus the tick
// count, moves with every clock step instead.
func CurrentBoot() (int64, error) {
	rows, err := systemProcesses()
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if row.pid != systemProcessID {
			continue
		}
		if row.image != "System" || !row.creationOK {
			return 0, fmt.Errorf("proctable: PID %d is %q (creation time known: %t), not the System process a boot is identified by", row.pid, row.image, row.creationOK)
		}
		return row.creation, nil
	}
	return 0, fmt.Errorf("proctable: the process table has no System process (PID %d) to identify the boot by", systemProcessID)
}

// CurrentProcessStart returns this process's PID and creation time in Unix
// nanoseconds, the identity ProcessRunning checks a process against.
func CurrentProcessStart() (int, int64, error) {
	creation, err := handleCreationTime(windows.CurrentProcess())
	if err != nil {
		return 0, 0, err
	}
	return os.Getpid(), creation, nil
}

// ProcessRunning reports whether pid still names the process created at
// creation (Unix nanoseconds), and that process has not exited. A PID no
// longer in use, or now naming a process created at another time, is not
// running. Anything that keeps the question from being answered is an error,
// never "not running".
func ProcessRunning(pid int, creation int64) (bool, error) {
	if pid <= 0 || creation <= 0 {
		return false, fmt.Errorf("proctable: PID %d created at %d identifies no process", pid, creation)
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if openErrorMeansProcessGone(err) {
			return false, nil
		}
		return false, fmt.Errorf("proctable: opening PID %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	got, err := handleCreationTime(handle)
	if err != nil {
		return false, fmt.Errorf("proctable: PID %d: %w", pid, err)
	}
	if got != creation {
		return false, nil
	}
	return !processHandleExited(handle), nil
}
