//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// statBdBinaryFile identifies the resolved bd binary at abs. It uses
// x/sys/unix, not syscall: syscall.Stat_t names the times Mtimespec and
// Ctimespec on darwin, unix.Stat_t names them Mtim and Ctim everywhere.
func statBdBinaryFile(abs string) (bdBinaryID, error) {
	var st unix.Stat_t
	if err := unix.Stat(abs, &st); err != nil {
		return bdBinaryID{}, err
	}
	return bdBinaryID{
		Path:  abs,
		Dev:   uint64(st.Dev), //nolint:unconvert // Darwin's field is signed int32.
		Ino:   st.Ino,
		Size:  st.Size,
		MTime: st.Mtim.Nano(),
		CTime: st.Ctim.Nano(),
	}, nil
}

// tryLockBdTrustCacheFile takes f's exclusive lock without blocking and
// returns the function that releases it and closes f. busy reports that
// another holder has the lock.
func tryLockBdTrustCacheFile(f *os.File) (release func(), busy bool, err error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Is(err, syscall.EWOULDBLOCK), err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, false, nil
}

// cancelBdCommandAsGroup starts cmd in its own process group and makes its
// context cancel SIGKILL that whole group.
func cancelBdCommandAsGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
