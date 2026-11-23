//go:build !windows

package acp

import "syscall"

// captureNoFollow makes the transcript open refuse a symbolic link at its path.
const captureNoFollow = syscall.O_NOFOLLOW
