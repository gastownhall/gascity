//go:build windows

package proctable

import (
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"golang.org/x/sys/windows"
)

// rootArgv reads pid's command line out of its PEB and splits it the way
// Windows itself does.
//
// A Windows process stores one command-line STRING, not an argv vector, so the
// split has to be performed rather than read. CommandLineToArgvW is the
// system's own parser, which keeps the quoting and backslash rules identical to
// the ones the target's own runtime applied.
func rootArgv(pid int) ([]string, error) {
	handle, err := openProcessForInspection(pid)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	argv, err := handleArgv(handle)
	if err != nil {
		return nil, fmt.Errorf("proctable: argv for PID %d: %w", pid, err)
	}
	return argv, nil
}

// handleArgv reads the command line of the process behind handle and splits
// it as rootArgv does. The handle must carry PROCESS_VM_READ and a query right.
func handleArgv(handle windows.Handle) ([]string, error) {
	commandLine, err := readProcessCommandLine(handle)
	if err != nil {
		return nil, err
	}
	argv, err := splitWindowsCommandLine(commandLine)
	if err != nil {
		return nil, err
	}
	return pidutil.NormalizeArgv(argv), nil
}

// rootClassifyAccess is what reading a runtime root's command line takes, with
// what confirming its identity and its exit take.
const rootClassifyAccess = windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_VM_READ | windows.SYNCHRONIZE

func rootIsCityInfrastructure(live runtime.LiveRuntime) (bool, error) {
	creation, err := strconv.ParseInt(live.StartIdentity, 10, 64)
	if err != nil || creation <= 0 {
		return false, fmt.Errorf("proctable: cannot classify the runtime root at PID %d without the start identity its scan observed", live.PID)
	}
	handle, held, err := holdTreeMember(treeMember{pid: live.PID, creation: creation}, rootClassifyAccess)
	if err != nil {
		return false, fmt.Errorf("proctable: opening the runtime root at PID %d to read its command line: %w", live.PID, err)
	}
	if !held {
		return false, nil
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	argv, err := handleArgv(handle)
	if err != nil {
		if processHandleExited(handle) {
			return false, nil
		}
		return false, fmt.Errorf("proctable: reading the command line of the runtime root at PID %d: %w", live.PID, err)
	}
	return IsCityInfrastructureArgv(argv), nil
}

func rootStartIdentity(pid int) string {
	identity, err := ProcessIdentity(pid)
	if err != nil {
		return ""
	}
	return identity
}

// maxCommandLineArguments is the most arguments splitWindowsCommandLine can
// return. CommandLineToArgvW's result is typed as a fixed-size array, so
// indexing past this would read past the array. A command line may be up to
// 32767 characters, enough for about twice this many one-character arguments,
// so exceeding it is reported rather than truncated: a truncated argv is a
// wrong one.
const maxCommandLineArguments = 8192

func splitWindowsCommandLine(commandLine string) ([]string, error) {
	trimmed := strings.TrimSpace(commandLine)
	if trimmed == "" {
		// CommandLineToArgvW answers an empty command line with this process's
		// own executable path, which would be a fabricated argv for the target.
		return nil, nil
	}
	encoded, err := windows.UTF16PtrFromString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("encoding command line: %w", err)
	}
	var count int32
	argv, err := windows.CommandLineToArgv(encoded, &count)
	if err != nil {
		return nil, fmt.Errorf("splitting command line: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(argv))) //nolint:errcheck

	if count < 0 {
		return nil, fmt.Errorf("command line split reported %d arguments", count)
	}
	if count > maxCommandLineArguments {
		return nil, fmt.Errorf("command line has %d arguments, more than the %d that can be read", count, maxCommandLineArguments)
	}
	out := make([]string, 0, count)
	for i := range int(count) {
		out = append(out, windows.UTF16PtrToString(&argv[i][0]))
	}
	return out, nil
}
