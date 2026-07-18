//go:build windows

package credentialprovider

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func newCommandControl(cmd *exec.Cmd) (*commandControl, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create credential provider job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure credential provider job: %w", err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED

	var jobMu sync.Mutex
	closed := false
	canceled := false
	var terminateOnce sync.Once
	var terminateErr error
	terminate := func() error {
		terminateOnce.Do(func() {
			jobMu.Lock()
			defer jobMu.Unlock()
			canceled = true
			var jobErr error
			if !closed {
				jobErr = windows.TerminateJobObject(job, 1)
			}
			var processErr error
			if cmd != nil && cmd.Process != nil {
				if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					processErr = err
				}
			}
			terminateErr = errors.Join(jobErr, processErr)
		})
		return terminateErr
	}
	closeJob := func() error {
		jobMu.Lock()
		defer jobMu.Unlock()
		if closed {
			return nil
		}
		closed = true
		return windows.CloseHandle(job)
	}
	afterStart := func() error {
		jobMu.Lock()
		defer jobMu.Unlock()
		if canceled || closed {
			return errors.New("credential provider process was canceled before job assignment")
		}
		var assignErr error
		if err := cmd.Process.WithHandle(func(handle uintptr) {
			assignErr = windows.AssignProcessToJobObject(job, windows.Handle(handle))
		}); err != nil {
			return fmt.Errorf("access credential provider process handle: %w", err)
		}
		if assignErr != nil {
			return fmt.Errorf("assign credential provider job: %w", assignErr)
		}
		if err := resumeProcessThreads(uint32(cmd.Process.Pid)); err != nil {
			return fmt.Errorf("resume credential provider process: %w", err)
		}
		return nil
	}
	return &commandControl{afterStart: afterStart, cancel: terminate, close: closeJob}, nil
}

func resumeProcessThreads(processID uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	resumed := 0
	for {
		if entry.OwnerProcessID == processID {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return err
			}
			_, resumeErr := windows.ResumeThread(thread)
			closeErr := windows.CloseHandle(thread)
			if resumeErr != nil {
				return resumeErr
			}
			if closeErr != nil {
				return closeErr
			}
			resumed++
		}
		entry.Size = uint32(unsafe.Sizeof(windows.ThreadEntry32{}))
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return err
		}
	}
	if resumed == 0 {
		return errors.New("credential provider process has no resumable thread")
	}
	return nil
}
