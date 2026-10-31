//go:build windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/gastownhall/gascity/internal/platform/localsocket"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"golang.org/x/sys/windows"
)

const supervisorMaxCommandLineArguments = 8192

// stopSupervisorConnJSON drives the stop protocol over an already dialed
// supervisor connection without identifying the supervisor process, so --wait
// ends once the control socket is gone.
func stopSupervisorConnJSON(conn net.Conn, sockPath string, stdout, stderr io.Writer, wait bool, waitTimeout time.Duration, jsonOut bool) int {
	return stopSupervisorConnForProcessJSON(conn, sockPath, 0, nil, stdout, stderr, wait, waitTimeout, jsonOut)
}

// driftRestartNoticeWriter receives the notice that a pre-session-preserving
// supervisor's sessions will restart. Overridable for tests.
var driftRestartNoticeWriter io.Writer = os.Stdout

var readSupervisorCommandLineHook = func(pid int) (string, error) {
	return proctable.ProcessCommandLine(pid, time.Time{})
}

func printUnreadableSupervisorRestartError(stderr io.Writer, pid int, exeErr error) {
	fmt.Fprintf(stderr, "error: cannot auto-restart supervisor: Windows cannot query the executable path for PID %d (%v). Stop the supervisor manually and rerun 'gc start', rerun gc start from an account allowed to inspect that process, or pass --no-auto-restart to skip the restart and surface the drift as an error.\n", pid, exeErr) //nolint:errcheck // best-effort stderr
}

// readSupervisorExePath returns the path gc should spawn when restarting a
// direct supervisor. QueryFullProcessImageName reports the running image, while
// the command line preserves the path the supervisor was launched from. When a
// Windows rebuild renames the old image aside and writes a new gc.exe at the
// original path, the running image follows the aside name; prefer the launch
// path when it now names different on-disk bytes.
func readSupervisorExePath(pid int) (string, error) {
	imagePath, err := querySupervisorProcessImagePath(pid)
	if err != nil {
		return "", err
	}
	launchPath, err := readSupervisorLaunchPath(pid, imagePath)
	if err != nil {
		return "", err
	}
	return supervisorRestartExePath(imagePath, launchPath)
}

func querySupervisorProcessImagePath(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid supervisor PID %d", pid)
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", fmt.Errorf("opening supervisor PID %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	for size := uint32(windows.MAX_PATH); ; size *= 2 {
		buffer := make([]uint16, size)
		length := size
		err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &length)
		if err == nil {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || size >= 32768 {
			return "", fmt.Errorf("querying image path for supervisor PID %d: %w", pid, err)
		}
	}
}

func readSupervisorLaunchPath(pid int, imagePath string) (string, error) {
	commandLine, err := readSupervisorCommandLineHook(pid)
	if err != nil {
		return "", fmt.Errorf("reading command line for supervisor PID %d: %w", pid, err)
	}
	argv0, err := windowsCommandLineArgv0(commandLine)
	if err != nil {
		return "", fmt.Errorf("reading command line for supervisor PID %d: %w", pid, err)
	}
	if argv0 == "" {
		return "", fmt.Errorf("supervisor PID %d command line does not name an executable", pid)
	}
	if !filepath.IsAbs(argv0) {
		if supervisorRelativeArgv0MatchesImage(imagePath, argv0) {
			return filepath.Clean(imagePath), nil
		}
		return "", fmt.Errorf("supervisor PID %d command line starts with %q, which does not match image %q", pid, argv0, imagePath)
	}
	return filepath.Clean(argv0), nil
}

func supervisorRelativeArgv0MatchesImage(imagePath, argv0 string) bool {
	argvName := filepath.Base(argv0)
	if filepath.Ext(argvName) == "" {
		argvName += ".exe"
	}
	return strings.EqualFold(filepath.Base(imagePath), argvName)
}

func windowsCommandLineArgv0(commandLine string) (string, error) {
	trimmed := strings.TrimSpace(commandLine)
	if trimmed == "" {
		return "", nil
	}
	encoded, err := windows.UTF16PtrFromString(trimmed)
	if err != nil {
		return "", fmt.Errorf("encoding command line: %w", err)
	}
	var count int32
	argv, err := windows.CommandLineToArgv(encoded, &count)
	if err != nil {
		return "", fmt.Errorf("splitting command line: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(argv))) //nolint:errcheck

	if count < 0 {
		return "", fmt.Errorf("command line split reported %d arguments", count)
	}
	if count == 0 {
		return "", nil
	}
	if count > supervisorMaxCommandLineArguments {
		return "", fmt.Errorf("command line has %d arguments, more than the %d that can be read", count, supervisorMaxCommandLineArguments)
	}
	return windows.UTF16PtrToString(&argv[0][0]), nil
}

func supervisorRestartExePath(imagePath, launchPath string) (string, error) {
	if launchPath == "" || !filepath.IsAbs(launchPath) {
		return imagePath, nil
	}
	cleanImage := filepath.Clean(imagePath)
	cleanLaunch := filepath.Clean(launchPath)
	if strings.EqualFold(cleanImage, cleanLaunch) {
		return cleanImage, nil
	}
	launchInfo, launchErr := os.Stat(cleanLaunch)
	if launchErr != nil {
		return "", fmt.Errorf("launch path %q differs from running image %q but cannot be inspected: %w", cleanLaunch, cleanImage, launchErr)
	}
	imageInfo, imageErr := os.Stat(cleanImage)
	if imageErr == nil && os.SameFile(imageInfo, launchInfo) {
		return cleanImage, nil
	}
	return cleanLaunch, nil
}

func platformDriftRestartKill(pid int) error {
	return requestSupervisorStopForPID(pid)
}

func requestSupervisorStopForPID(pid int) error {
	unsupported, err := requestSupervisorSocketCommandForPID(pid, supervisorSignalTermSocketCommand)
	if err != nil {
		return err
	}
	if !unsupported {
		return nil
	}
	if driftRestartNoticeWriter != nil {
		fmt.Fprintln(driftRestartNoticeWriter, "\nRunning supervisor predates session-preserving restart; its sessions will restart.") //nolint:errcheck
	}
	_, err = requestSupervisorSocketCommandForPID(pid, "stop")
	return err
}

func requestSupervisorSocketCommandForPID(pid int, command string) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("refusing to stop unsafe Windows supervisor PID %d", pid)
	}
	var mismatched []string
	deadline := time.Now().Add(3 * time.Second)
	for _, sockPath := range supervisorSocketPathCandidates() {
		probedPID := supervisorAliveAtPathUntil(sockPath, deadline)
		switch {
		case probedPID == pid:
			return requestSupervisorSocketCommandAtPathForPID(sockPath, pid, command)
		case probedPID != 0:
			mismatched = append(mismatched, fmt.Sprintf("%s reports PID %d", sockPath, probedPID))
		}
	}
	if len(mismatched) == 0 {
		return false, fmt.Errorf("cannot request graceful stop for supervisor PID %d: no supervisor control socket in this GC_HOME reported that PID", pid)
	}
	return false, fmt.Errorf("cannot request graceful stop for supervisor PID %d: %s", pid, strings.Join(mismatched, "; "))
}

func requestSupervisorSocketCommandAtPathForPID(sockPath string, pid int, command string) (bool, error) {
	conn, err := localsocket.Dial(sockPath, 2*time.Second)
	if err != nil {
		return false, fmt.Errorf("dialing supervisor socket %s for PID %d: %w", sockPath, pid, err)
	}
	defer conn.Close() //nolint:errcheck
	peerPID, err := localsocket.PeerPID(conn)
	if err != nil {
		return false, fmt.Errorf("checking supervisor socket %s peer PID for requested PID %d: %w", sockPath, pid, err)
	}
	if peerPID != pid {
		return false, fmt.Errorf("refusing supervisor shutdown at %s: requested PID %d but connected peer is PID %d", sockPath, pid, peerPID)
	}
	if command == "stop" {
		var stdout, stderr bytes.Buffer
		if code := stopSupervisorConnJSON(conn, sockPath, &stdout, &stderr, false, 0, false); code != 0 {
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = strings.TrimSpace(stdout.String())
			}
			if detail == "" {
				detail = "stop request failed"
			}
			return false, fmt.Errorf("requesting graceful stop for supervisor PID %d at %s: %s", pid, sockPath, detail)
		}
		return false, nil
	}
	if _, err := requestSupervisorSocketCommand(conn, command); err != nil {
		if errors.Is(err, errSupervisorSocketNoAckBeforeEOF) {
			return true, nil
		}
		return false, fmt.Errorf("requesting signal-equivalent shutdown for supervisor PID %d at %s: %w", pid, sockPath, err)
	}
	return false, nil
}
