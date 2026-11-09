//go:build linux

package tmux

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

// paneSubtreeSupported reports that readPaneSubtree can read a pane's
// process tree on this platform.
const paneSubtreeSupported = true

// maxPaneSubtreeProcesses bounds the processes readPaneSubtree reads. A
// pane's tree is a launch chain and the agent's own children; one this large
// is not a pane worth walking file by file, and the full snapshot judges it.
const maxPaneSubtreeProcesses = 512

// readPaneSubtree reads the process tree rooted at pid from /proc alone
// (readProcSubtree).
func readPaneSubtree(pid string) (processSnapshot, error) {
	return readProcSubtree(os.DirFS("/proc"), pid)
}

// readProcSubtree reads the process tree rooted at pid from proc, a /proc
// file system: the root, then every process its threads'
// <pid>/task/<tid>/children files list, down to the depth
// paneRuntimeState.processAlive walks. Each process carries its comm and
// argv as the full snapshot's ps lists them, so the result judges the pane
// exactly as the full snapshot would, at the cost of a few small reads
// instead of the whole host's table.
//
// It fails, so the caller falls back to the full snapshot, when the root
// cannot be read, when the kernel offers no children files, when a read is
// refused, or when the tree outgrows maxPaneSubtreeProcesses. A descendant
// that exits mid-walk is left out (exitedMidWalk), as a later snapshot would
// leave it out.
func readProcSubtree(proc fs.FS, pid string) (processSnapshot, error) {
	if !decimalRe.MatchString(pid) {
		return processSnapshot{}, fmt.Errorf("reading the process subtree of %q: not a pid", pid)
	}
	root, err := readProcProcess(proc, pid, "")
	if err != nil {
		return processSnapshot{}, fmt.Errorf("reading the process subtree of %s: %w", pid, err)
	}
	processes := []processRuntimeState{root}
	level := []string{pid}
	for depth := 0; depth <= maxProcessDescendantDepth+1 && len(level) > 0; depth++ {
		var next []string
		for _, parent := range level {
			children, err := readProcChildren(proc, parent)
			if err != nil {
				if parent != pid && exitedMidWalk(err) {
					continue
				}
				return processSnapshot{}, fmt.Errorf("reading the process subtree of %s: %w", pid, err)
			}
			for _, child := range children {
				process, err := readProcProcess(proc, child, parent)
				if exitedMidWalk(err) {
					continue
				}
				if err != nil {
					return processSnapshot{}, fmt.Errorf("reading the process subtree of %s: %w", pid, err)
				}
				processes = append(processes, process)
				if len(processes) > maxPaneSubtreeProcesses {
					return processSnapshot{}, fmt.Errorf("reading the process subtree of %s: more than %d processes", pid, maxPaneSubtreeProcesses)
				}
				next = append(next, child)
			}
		}
		level = next
	}
	return newProcessSnapshot(processes), nil
}

// exitedMidWalk reports whether err is a read of a process that exited
// during the walk: its /proc entry is gone (ENOENT), or it was reaped between
// a file's open and its read, which fails a comm or cmdline read with ESRCH.
func exitedMidWalk(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// readProcChildren lists pid's children: the union of its threads'
// <pid>/task/<tid>/children files, each a space-separated pid list.
func readProcChildren(proc fs.FS, pid string) ([]string, error) {
	taskDir := path.Join(pid, "task")
	tasks, err := fs.ReadDir(proc, taskDir)
	if err != nil {
		return nil, err
	}
	var children []string
	for _, task := range tasks {
		data, err := fs.ReadFile(proc, path.Join(taskDir, task.Name(), "children"))
		if err != nil {
			if _, statErr := fs.Stat(proc, path.Join(taskDir, task.Name())); errors.Is(statErr, fs.ErrNotExist) {
				continue // the thread exited between the listing and the read
			}
			return nil, err
		}
		children = append(children, strings.Fields(string(data))...)
	}
	return children, nil
}

// readProcProcess reads pid's comm and argv. ppid is the parent the walk
// found it under ("" for the root, whose parent the probes never consult).
// argv is cmdline's NUL-separated words joined by spaces, or "[comm]" for a
// process with no argv, as ps prints args.
func readProcProcess(proc fs.FS, pid, ppid string) (processRuntimeState, error) {
	comm, err := fs.ReadFile(proc, path.Join(pid, "comm"))
	if err != nil {
		return processRuntimeState{}, err
	}
	cmdline, err := fs.ReadFile(proc, path.Join(pid, "cmdline"))
	if err != nil {
		return processRuntimeState{}, err
	}
	command := strings.TrimSuffix(string(comm), "\n")
	args := strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
	if args == "" {
		args = "[" + command + "]"
	}
	return processRuntimeState{PID: pid, PPID: ppid, Command: command, Args: args}, nil
}
