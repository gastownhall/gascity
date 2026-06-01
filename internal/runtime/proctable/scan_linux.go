//go:build linux

package proctable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionID returns live agent root processes whose environment carries
// GC_SESSION_ID equal to id. Empty id returns all roots with any GC_SESSION_ID.
func ScanBySessionID(id string) ([]runtime.LiveRuntime, error) {
	root, err := liveScanRoot()
	if err != nil {
		return []runtime.LiveRuntime{}, err
	}
	return scanWithRoot(root, id)
}

// IsScanRoot reports whether pid is outside its GC_SESSION_ID parent's
// envelope and should be treated as an agent root.
func IsScanRoot(pid int) bool {
	root, err := liveScanRoot()
	if err != nil {
		return false
	}
	if pid == 1 {
		return true
	}
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return false
	}
	env, err := parseEnvironFile(filepath.Join(root, strconv.Itoa(pid), "environ"))
	if err != nil || len(env) == 0 {
		return false
	}
	sessionID := env["GC_SESSION_ID"]
	if sessionID == "" {
		return false
	}
	isRoot, err := isRootWithSessionID(root, pid, sessionID)
	return err == nil && isRoot
}

func scanWithRoot(root, id string) ([]runtime.LiveRuntime, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return []runtime.LiveRuntime{}, fmt.Errorf("enumerating %s: %w", root, err)
	}

	var (
		out     []runtime.LiveRuntime
		scanErr error
	)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		env, err := parseEnvironFile(filepath.Join(root, entry.Name(), "environ"))
		if err != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("reading environ for pid %d: %w", pid, err))
			continue
		}
		if root == "/proc" && pid == os.Getpid() {
			env = mergeCurrentEnv(env)
		}
		if len(env) == 0 {
			continue
		}
		sessionID := env["GC_SESSION_ID"]
		if sessionID == "" {
			continue
		}
		if id != "" && sessionID != id {
			continue
		}
		rootProcess, err := isRootWithSessionID(root, pid, sessionID)
		if err != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("checking root for pid %d: %w", pid, err))
			continue
		}
		if !rootProcess {
			continue
		}
		epoch, _ := strconv.Atoi(env["GC_RUNTIME_EPOCH"])
		out = append(out, runtime.LiveRuntime{
			SessionID: sessionID,
			Epoch:     epoch,
			PID:       pid,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PID < out[j].PID
	})
	if out == nil {
		out = []runtime.LiveRuntime{}
	}
	return out, scanErr
}

func mergeCurrentEnv(env map[string]string) map[string]string {
	if env == nil {
		env = make(map[string]string)
	}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		env[key] = value
	}
	return env
}

func parseEnvironFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || os.IsPermission(err) {
			return nil, nil
		}
		return nil, err
	}
	env := make(map[string]string)
	for _, entry := range strings.Split(string(data), "\x00") {
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		env[key] = value
	}
	return env, nil
}

func isRootWithSessionID(root string, pid int, sessionID string) (bool, error) {
	ppid, ok, err := readParentPID(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return false, err
	}
	if !ok {
		// stat vanished between environ read and here; process died in the race
		// window — skip rather than misreport it as a root.
		return false, nil
	}
	if ppid <= 1 {
		return true, nil
	}
	parentEnv, err := parseEnvironFile(filepath.Join(root, strconv.Itoa(ppid), "environ"))
	if err != nil {
		return false, err
	}
	if parentEnv["GC_SESSION_ID"] == sessionID && isInfrastructureParent(root, ppid) {
		return true, nil
	}
	return parentEnv["GC_SESSION_ID"] != sessionID, nil
}

func isInfrastructureParent(root string, pid int) bool {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "comm"))
	if err != nil {
		return false
	}
	command := strings.ToLower(strings.TrimSpace(string(data)))
	return strings.Contains(command, "tmux")
}

func readParentPID(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || os.IsPermission(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	text := string(data)
	closeParen := strings.LastIndex(text, ")")
	if closeParen < 0 || closeParen+1 >= len(text) {
		return 0, false, fmt.Errorf("malformed stat file %s", path)
	}
	fields := strings.Fields(text[closeParen+1:])
	if len(fields) < 2 {
		return 0, false, fmt.Errorf("malformed stat file %s", path)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false, fmt.Errorf("parsing ppid from %s: %w", path, err)
	}
	return ppid, true, nil
}
