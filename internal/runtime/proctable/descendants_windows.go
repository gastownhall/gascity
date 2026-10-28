//go:build windows

package proctable

// snapshotProcesses reads the host process table once. There is no
// liveScanGuard (that guard protects the orphan sweep in ScanBySessionID, not
// this read-only snapshot) and no GC_SESSION_ID filtering.
//
// PGID is always 0, and that is a decision rather than a gap. ProcessRecord's
// PGID is a Unix process group: a kernel-maintained set a signal can be
// addressed to as a unit. Windows has no such thing. Its console process groups
// carry no descendants and cannot be terminated as a group, and the real
// ownership boundary — a Job Object, as internal/processgroup builds — belongs
// to whoever started the tree and is invisible to a host-wide snapshot of
// processes this package did not launch. Reporting PID-as-PGID would invent a
// group leader out of every row and invite exactly the bare-PID group kill
// processgroup.Terminate refuses to perform. Zero says "this platform reports
// no process group", so teardown selection falls back to the PPID chain fenced
// by StartTime, which is the only ownership evidence Windows actually offers.
func snapshotProcesses() ([]ProcessRecord, error) {
	table, err := readProcessTable()
	if err != nil {
		return nil, err
	}
	return snapshotFromTable(table)
}

// snapshotFromTable converts one table read into records. A row that was opened
// but could not be read is still returned — its PID, PPID, creation time and
// image come from the snapshot, not from inspection — but its session ID is
// unknown, and the table's incompleteness is returned alongside so no caller
// mistakes that for a process carrying none.
func snapshotFromTable(table processTable) ([]ProcessRecord, error) {
	records := make([]ProcessRecord, 0, len(table.rows))
	for _, row := range table.rows {
		records = append(records, ProcessRecord{
			PID:  row.pid,
			PPID: reportedParentPID(table, row),
			PGID: 0,
			// Empty only when the snapshot carried no creation time, which is
			// the idle and system processes. Every other row has one, whether or
			// not this user could open the process. Callers already treat an
			// empty start identity as "cannot fence this PID".
			StartTime: row.startIdentity(),
			// The image file name, extension included, because that is what
			// Windows calls the process. DescendantAlive compares names through
			// processNameKey, so configured bare names still match it.
			Name:             row.image,
			SessionID:        row.sessionID(),
			SessionIDUnknown: row.uninspected(),
		})
	}
	return records, table.incomplete()
}
