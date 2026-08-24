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
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionID returns live agent root processes whose environment carries
// GC_SESSION_ID equal to id. Empty id returns all roots with any GC_SESSION_ID.
func ScanBySessionID(id string) ([]runtime.LiveRuntime, error) {
	if err := liveScanGuard(); err != nil {
		return []runtime.LiveRuntime{}, err
	}
	return scanWithRoot(scanRoot, id)
}

// ScanBySessionIDSince scans for an exact session incarnation. Inspection
// failures from processes proven to predate incarnationStartedAt do not make
// absence incomplete; those processes cannot belong to that incarnation.
func ScanBySessionIDSince(id string, incarnationStartedAt time.Time) ([]runtime.LiveRuntime, error) {
	if err := liveScanGuard(); err != nil {
		return []runtime.LiveRuntime{}, err
	}
	return scanWithRootSince(scanRoot, id, incarnationStartedAt)
}

// IsScanRoot reports whether pid is outside its GC_SESSION_ID parent's
// envelope and should be treated as an agent root.
func IsScanRoot(pid int) bool {
	if err := liveScanGuard(); err != nil {
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
	env, err := parseEnvironFile(filepath.Join(scanRoot, strconv.Itoa(pid), "environ"))
	if err != nil || len(env) == 0 {
		return false
	}
	sessionID := env["GC_SESSION_ID"]
	if sessionID == "" {
		return false
	}
	isRoot, err := isRootWithSessionID(scanRoot, pid, sessionID)
	return err == nil && isRoot
}

func scanWithRoot(root, id string) ([]runtime.LiveRuntime, error) {
	return scanWithRootSince(root, id, time.Time{})
}

func scanWithRootSince(root, id string, incarnationStartedAt time.Time) ([]runtime.LiveRuntime, error) {
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
		owned, err := processOwnedByUID(root, pid, os.Geteuid())
		if err != nil {
			if irrelevant, proofErr := processPredatesIncarnation(root, pid, incarnationStartedAt); irrelevant {
				continue
			} else if proofErr != nil {
				scanErr = errors.Join(scanErr, fmt.Errorf("proving age for pid %d: %w", pid, proofErr))
			}
			scanErr = errors.Join(scanErr, fmt.Errorf("reading owner for pid %d: %w", pid, err))
			continue
		}
		if !owned {
			continue
		}
		env, err := parseEnvironFile(filepath.Join(root, entry.Name(), "environ"))
		if err != nil {
			if irrelevant, proofErr := processPredatesIncarnation(root, pid, incarnationStartedAt); irrelevant {
				continue
			} else if proofErr != nil {
				scanErr = errors.Join(scanErr, fmt.Errorf("proving age for pid %d: %w", pid, proofErr))
			}
			if irrelevant, proofErr := unreadableProcessProvenOutsideIncarnation(
				root,
				pid,
				id,
				incarnationStartedAt,
			); irrelevant {
				continue
			} else if proofErr != nil {
				scanErr = errors.Join(scanErr, fmt.Errorf("proving tmux parent for pid %d: %w", pid, proofErr))
			}
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
		city := env["GC_CITY_PATH"]
		if city == "" {
			city = env["GC_CITY"]
		}
		out = append(out, runtime.LiveRuntime{
			SessionID: sessionID,
			City:      city,
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

const (
	linuxUserHZ                  = 100
	linuxProcessStartUncertainty = time.Second + time.Second/linuxUserHZ
)

func processPredatesIncarnation(root string, pid int, incarnationStartedAt time.Time) (bool, error) {
	if incarnationStartedAt.IsZero() || incarnationStartedAt.After(time.Now()) {
		return false, nil
	}
	bootedAt, err := readBootTime(root)
	if err != nil {
		return false, err
	}
	startedAt, exists, err := readProcessStartTime(root, pid, bootedAt)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	return processDefinitelyPredatesIncarnation(startedAt, incarnationStartedAt), nil
}

func processDefinitelyPredatesIncarnation(startedAt, incarnationStartedAt time.Time) bool {
	// /proc/stat exposes btime only to whole seconds and /proc/<pid>/stat
	// exposes start time in USER_HZ ticks. Require the process to precede the
	// boundary by more than both quantization errors before excluding it.
	return startedAt.Add(linuxProcessStartUncertainty).Before(incarnationStartedAt)
}

type processIdentity struct {
	PID        int
	PPID       int
	StartTicks uint64
	Cgroup     string
}

func unreadableProcessProvenOutsideIncarnation(
	root string,
	pid int,
	targetSessionID string,
	incarnationStartedAt time.Time,
) (bool, error) {
	if targetSessionID == "" ||
		incarnationStartedAt.IsZero() ||
		incarnationStartedAt.After(time.Now()) {
		return false, nil
	}

	bootedAt, err := readBootTime(root)
	if err != nil {
		return false, err
	}
	candidateBefore, exists, err := readProcessIdentity(root, pid)
	if err != nil || !exists {
		return false, err
	}
	if processDefinitelyPredatesIncarnation(
		processStartedAt(bootedAt, candidateBefore.StartTicks),
		incarnationStartedAt,
	) ||
		candidateBefore.PPID <= 1 ||
		!isUniqueTmuxSpawnScope(candidateBefore.Cgroup) {
		return false, nil
	}

	parentBefore, exists, err := readProcessIdentity(root, candidateBefore.PPID)
	if err != nil || !exists {
		return false, err
	}
	if parentBefore.Cgroup != candidateBefore.Cgroup ||
		!processDefinitelyPredatesIncarnation(
			processStartedAt(bootedAt, parentBefore.StartTicks),
			incarnationStartedAt,
		) {
		return false, nil
	}

	parentEnv, err := parseEnvironFile(
		filepath.Join(root, strconv.Itoa(parentBefore.PID), "environ"),
	)
	if err != nil || parentEnv == nil {
		return false, err
	}
	if parentEnv["GC_SESSION_ID"] == targetSessionID {
		return false, nil
	}

	parentAfter, exists, err := readProcessIdentity(root, parentBefore.PID)
	if err != nil || !exists {
		return false, err
	}
	candidateAfter, exists, err := readProcessIdentity(root, candidateBefore.PID)
	if err != nil || !exists {
		return false, err
	}
	if parentAfter.PID != parentBefore.PID ||
		parentAfter.StartTicks != parentBefore.StartTicks ||
		parentAfter.Cgroup != parentBefore.Cgroup ||
		candidateAfter != candidateBefore {
		return false, nil
	}
	return true, nil
}

func readProcessIdentity(root string, pid int) (processIdentity, bool, error) {
	stat, exists, err := readProcessStat(root, pid)
	if err != nil || !exists {
		return processIdentity{}, exists, err
	}
	cgroup, exists, err := readProcessCgroup(root, pid)
	if err != nil || !exists {
		return processIdentity{}, exists, err
	}
	return processIdentity{
		PID:        stat.PID,
		PPID:       stat.PPID,
		StartTicks: stat.StartTicks,
		Cgroup:     cgroup,
	}, true, nil
}

func readProcessCgroup(root string, pid int) (string, bool, error) {
	path := filepath.Join(root, strconv.Itoa(pid), "cgroup")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	var cgroup string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 || fields[2] == "" || cgroup != "" {
			return "", false, fmt.Errorf("malformed cgroup file %s", path)
		}
		cgroup = filepath.Clean(fields[2])
	}
	if cgroup == "" {
		return "", false, fmt.Errorf("missing cgroup path in %s", path)
	}
	return cgroup, true, nil
}

func isUniqueTmuxSpawnScope(cgroup string) bool {
	if cgroup == "" || cgroup == "." || cgroup == "/" {
		return false
	}
	leaf := filepath.Base(cgroup)
	const (
		prefix = "tmux-spawn-"
		suffix = ".scope"
	)
	return strings.HasPrefix(leaf, prefix) &&
		strings.HasSuffix(leaf, suffix) &&
		len(leaf) > len(prefix)+len(suffix)
}

func readBootTime(root string) (time.Time, error) {
	data, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "btime" {
			continue
		}
		seconds, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parsing btime %q: %w", fields[1], err)
		}
		return time.Unix(seconds, 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("missing btime field")
}

type processStat struct {
	PID        int
	PPID       int
	StartTicks uint64
}

func readProcessStat(root string, pid int) (processStat, bool, error) {
	path := filepath.Join(root, strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return processStat{}, false, nil
		}
		return processStat{}, false, err
	}
	text := string(data)
	openParen := strings.Index(text, "(")
	closeParen := strings.LastIndex(text, ")")
	if openParen <= 0 || closeParen < openParen || closeParen+1 >= len(text) {
		return processStat{}, false, fmt.Errorf("malformed stat file %s", path)
	}
	observedPID, err := strconv.Atoi(strings.TrimSpace(text[:openParen]))
	if err != nil || observedPID != pid {
		return processStat{}, false, fmt.Errorf("invalid pid in stat file %s", path)
	}
	fields := strings.Fields(text[closeParen+1:])
	const starttimeIndexAfterComm = 19
	if len(fields) <= starttimeIndexAfterComm {
		return processStat{}, false, fmt.Errorf("malformed stat file %s", path)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return processStat{}, false, fmt.Errorf("parsing ppid from %s: %w", path, err)
	}
	startTicks, err := strconv.ParseUint(fields[starttimeIndexAfterComm], 10, 64)
	if err != nil {
		return processStat{}, false, fmt.Errorf("parsing start time from %s: %w", path, err)
	}
	return processStat{
		PID:        observedPID,
		PPID:       ppid,
		StartTicks: startTicks,
	}, true, nil
}

func readProcessStartTime(root string, pid int, bootedAt time.Time) (time.Time, bool, error) {
	stat, exists, err := readProcessStat(root, pid)
	if err != nil || !exists {
		return time.Time{}, exists, err
	}
	return processStartedAt(bootedAt, stat.StartTicks), true, nil
}

func processStartedAt(bootedAt time.Time, startTicks uint64) time.Time {
	wholeSeconds := startTicks / linuxUserHZ
	remainderTicks := startTicks % linuxUserHZ
	return bootedAt.Add(
		time.Duration(wholeSeconds)*time.Second +
			time.Duration(remainderTicks)*(time.Second/linuxUserHZ),
	)
}

func processOwnedByUID(root string, pid, uid int) (bool, error) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "status"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		if len(fields) == 0 {
			break
		}
		observed, err := strconv.Atoi(fields[0])
		if err != nil {
			break
		}
		return observed == uid, nil
	}
	return false, fmt.Errorf("missing valid Uid field")
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
		if errors.Is(err, fs.ErrNotExist) {
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
		if errors.Is(err, fs.ErrNotExist) {
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
