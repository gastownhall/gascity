//go:build windows

package processgrouptest

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Descendant is a process a test command started and recorded, identified by
// its PID together with its creation time, so that a later process given the
// same PID is never taken for it.
type Descendant struct {
	PID     int
	Created int64
}

// DescendantCommand is a PowerShell command that starts a long-running
// descendant (ping.exe, in the same console and with its output in files
// beside recordPath), records it at recordPath for ReadDescendant, and then
// waits far longer than any test lets it.
func DescendantCommand(recordPath string) string {
	return "$psi = [System.Diagnostics.ProcessStartInfo]::new(); " +
		"$psi.FileName = (Join-Path $env:SystemRoot 'System32\\PING.EXE'); " +
		"$psi.Arguments = '-n 300 127.0.0.1'; " +
		"$psi.UseShellExecute = $false; " +
		"$psi.RedirectStandardOutput = $true; " +
		"$psi.RedirectStandardError = $true; " +
		"$p = [System.Diagnostics.Process]::new(); " +
		"$p.StartInfo = $psi; " +
		"[void]$p.Start(); " +
		"$p.BeginOutputReadLine(); " +
		"$p.BeginErrorReadLine(); " +
		"Set-Content -LiteralPath " + psQuote(recordPath) +
		" -Value ('{0} {1}' -f $p.Id, $p.StartTime.ToFileTimeUtc()); " +
		"Start-Sleep -Seconds 300"
}

// LeaveDescendantCommand starts and records a descendant as DescendantCommand
// does, then exits at once and leaves it running.
//
// The descendant inherits the command's standard handles, so a caller reading
// the command's output sees it held open until the descendant exits.
func LeaveDescendantCommand(recordPath string) string {
	return "$p = Start-Process -FilePath (Join-Path $env:SystemRoot 'System32\\PING.EXE')" +
		" -ArgumentList '-n','300','127.0.0.1' -NoNewWindow -PassThru" +
		" -RedirectStandardOutput " + psQuote(recordPath+".out") +
		" -RedirectStandardError " + psQuote(recordPath+".err") + "; " +
		"Set-Content -LiteralPath " + psQuote(recordPath) +
		" -Value ('{0} {1}' -f $p.Id, $p.StartTime.ToFileTimeUtc())"
}

// LeaveDetachedDescendantCommand starts and records a descendant as
// LeaveDescendantCommand does, but in a hidden window of its own rather than
// the command's console, so that it inherits none of the command's handles:
// the command's output closes when the command exits.
func LeaveDetachedDescendantCommand(recordPath string) string {
	return "$p = Start-Process -FilePath (Join-Path $env:SystemRoot 'System32\\PING.EXE')" +
		" -ArgumentList '-n','300','127.0.0.1' -WindowStyle Hidden -PassThru; " +
		"Set-Content -LiteralPath " + psQuote(recordPath) +
		" -Value ('{0} {1}' -f $p.Id, $p.StartTime.ToFileTimeUtc())"
}

// ReadDescendant reads the descendant DescendantCommand recorded at path, and
// registers a cleanup that terminates it should the test leave it running.
func ReadDescendant(t *testing.T, path string) Descendant {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the command never recorded its descendant (%v): it did not reach the Start-Process", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		t.Fatalf("descendant record %q, want a PID and a creation time", data)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		t.Fatalf("descendant PID %q is not usable: %v", fields[0], err)
	}
	created, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("descendant creation time %q: %v", fields[1], err)
	}
	d := Descendant{PID: pid, Created: created}
	t.Cleanup(d.Terminate)
	return d
}

// Alive reports whether the recorded process is still running.
func (d Descendant) Alive() bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(d.PID))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	if !d.is(handle) {
		return false
	}
	event, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}

// WaitGone reports whether the recorded process exits within the given time.
func (d Descendant) WaitGone(within time.Duration) bool {
	deadline := time.Now().Add(within)
	for d.Alive() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return true
}

// Terminate ends the recorded process if it is still the one recorded.
func (d Descendant) Terminate() {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(d.PID))
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	if d.is(handle) {
		_ = windows.TerminateProcess(handle, 1)
	}
}

// is reports whether handle is the recorded process rather than a later one
// holding its PID.
func (d Descendant) is(handle windows.Handle) bool {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return false
	}
	return int64(creation.HighDateTime)<<32|int64(creation.LowDateTime) == d.Created
}

// psQuote renders s as a PowerShell single-quoted string.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
