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

// IsScanRoot reports whether pid should be treated as an agent root. A root
// carries a GC_SESSION_ID, is not itself infrastructure — a tmux server or
// client is never a root, whoever its parent is — and sits outside its
// parent's envelope: the parent is gone, carries a different GC_SESSION_ID,
// or is infrastructure.
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
	if isInfrastructureProcess(scanRoot, pid) {
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
		live, reported, err := scanProcEntry(root, entry.Name(), id, incarnationStartedAt)
		if err != nil {
			scanErr = errors.Join(scanErr, err)
			continue
		}
		if !reported {
			continue
		}
		out = append(out, live)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PID < out[j].PID
	})
	if out == nil {
		out = []runtime.LiveRuntime{}
	}
	return out, scanErr
}

// scanProcEntry decides whether one /proc entry is an agent root for id and,
// when it is, builds the record to report. reported is false for every entry
// the scan declines to surface; err is returned only for a read that failed in
// a way the caller must accumulate, never for an ordinary decline.
//
// It is a separate function from the enumeration loop because the decision is a
// sequence of independent refusals; keeping them inside the loop nests every
// one of them, which is how this became the most complex function in the
// package.
func scanProcEntry(root, entryName, id string, incarnationStartedAt time.Time) (runtime.LiveRuntime, bool, error) {
	pid, err := strconv.Atoi(entryName)
	if err != nil || pid <= 1 {
		return runtime.LiveRuntime{}, false, nil
	}
	owned, err := processOwnedByUID(root, pid, os.Geteuid())
	if err != nil {
		return runtime.LiveRuntime{}, false, unreadableOwnerScanError(root, pid, incarnationStartedAt, err)
	}
	if !owned {
		return runtime.LiveRuntime{}, false, nil
	}
	env, err := parseEnvironFile(filepath.Join(root, entryName, "environ"))
	if err != nil {
		return runtime.LiveRuntime{}, false, unreadableEnvironScanError(root, pid, id, incarnationStartedAt, err)
	}
	if root == "/proc" && pid == os.Getpid() {
		env = mergeCurrentEnv(env)
	}
	if len(env) == 0 {
		return runtime.LiveRuntime{}, false, nil
	}
	sessionID := env["GC_SESSION_ID"]
	if sessionID == "" {
		return runtime.LiveRuntime{}, false, nil
	}
	if id != "" && sessionID != id {
		return runtime.LiveRuntime{}, false, nil
	}
	// Infrastructure is never an agent root, whoever its parent is: the
	// tmux server a session founded inherits its GC_SESSION_ID and
	// reparents to init, and the parent test below would report it — and
	// the orphan sweep would kill the server every agent in the city
	// shares (gastownhall/gascity#5392).
	if isInfrastructureProcess(root, pid) {
		return runtime.LiveRuntime{}, false, nil
	}
	rootProcess, err := isRootWithSessionID(root, pid, sessionID)
	if err != nil {
		return runtime.LiveRuntime{}, false, fmt.Errorf("checking root for pid %d: %w", pid, err)
	}
	if !rootProcess {
		return runtime.LiveRuntime{}, false, nil
	}
	epoch, _ := strconv.Atoi(env["GC_RUNTIME_EPOCH"])
	city := env["GC_CITY_PATH"]
	if city == "" {
		city = env["GC_CITY"]
	}
	ppid, _, _ := readParentPID(filepath.Join(root, entryName, "stat"))
	comm, _ := os.ReadFile(filepath.Join(root, entryName, "comm"))
	return runtime.LiveRuntime{
		SessionID: sessionID,
		City:      city,
		Epoch:     epoch,
		PID:       pid,
		PPID:      ppid,
		// Read from the SAME ppid that is reported above, so a caller fencing
		// on this field is fencing on the parent it was told about. A pid <= 1
		// parent is init or an unreadable stat, never the provider's server, so
		// it is refused without a comm read.
		ParentIsProviderInfrastructure: ppid > 1 && isInfrastructureProcess(root, ppid),
		Name:                           strings.TrimSpace(string(comm)),
	}, true, nil
}

// unreadableOwnerScanError returns the error a process whose owner could not
// be read adds to the scan, or nil when the process is proven to predate
// incarnationStartedAt and so cannot make absence incomplete.
func unreadableOwnerScanError(root string, pid int, incarnationStartedAt time.Time, readErr error) error {
	irrelevant, proofErr := processPredatesIncarnation(root, pid, incarnationStartedAt)
	if irrelevant {
		return nil
	}
	var errs []error
	if proofErr != nil {
		errs = append(errs, fmt.Errorf("proving age for pid %d: %w", pid, proofErr))
	}
	return errors.Join(append(errs, fmt.Errorf("reading owner for pid %d: %w", pid, readErr))...)
}

// unreadableEnvironScanError returns the error a process whose environment
// could not be read adds to the scan, or nil when the process is proven to
// predate incarnationStartedAt. A process proven outside the incarnation by its
// tmux-spawn parent is skipped too, but a failed age proof still leaves the
// scan incomplete.
func unreadableEnvironScanError(root string, pid int, id string, incarnationStartedAt time.Time, readErr error) error {
	irrelevant, proofErr := processPredatesIncarnation(root, pid, incarnationStartedAt)
	if irrelevant {
		return nil
	}
	var errs []error
	if proofErr != nil {
		errs = append(errs, fmt.Errorf("proving age for pid %d: %w", pid, proofErr))
	}
	irrelevant, proofErr = unreadableProcessProvenOutsideIncarnation(root, pid, id, incarnationStartedAt)
	if irrelevant {
		return errors.Join(errs...)
	}
	if proofErr != nil {
		errs = append(errs, fmt.Errorf("proving tmux parent for pid %d: %w", pid, proofErr))
	}
	return errors.Join(append(errs, fmt.Errorf("reading environ for pid %d: %w", pid, readErr))...)
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
	lineage, ok, err := readTmuxSpawnLineage(root, pid, bootedAt, incarnationStartedAt)
	if err != nil || !ok {
		return false, err
	}

	parentEnv, err := parseEnvironFile(
		filepath.Join(root, strconv.Itoa(lineage.parent.PID), "environ"),
	)
	if err != nil || parentEnv == nil {
		return false, err
	}
	if parentEnv["GC_SESSION_ID"] == targetSessionID {
		return false, nil
	}

	return lineage.stillCurrent(root)
}

// tmuxSpawnLineage is the candidate/parent identity pair captured by the first
// census of the PID-reuse fence.
type tmuxSpawnLineage struct {
	candidate processIdentity
	parent    processIdentity
}

// readTmuxSpawnLineage takes the first census of the unreadable process at pid
// and its parent. ok is false unless the candidate sits alone in a
// tmux-spawn-*.scope leaf whose parent provably predates
// incarnationStartedAt — the only shape whose parent environment can stand in
// for the candidate's own. Any other shape leaves the process unproven rather
// than excluded.
func readTmuxSpawnLineage(root string, pid int, bootedAt, incarnationStartedAt time.Time) (tmuxSpawnLineage, bool, error) {
	candidate, exists, err := readProcessIdentity(root, pid)
	if err != nil || !exists {
		return tmuxSpawnLineage{}, false, err
	}
	if processDefinitelyPredatesIncarnation(
		processStartedAt(bootedAt, candidate.StartTicks),
		incarnationStartedAt,
	) ||
		candidate.PPID <= 1 ||
		!isUniqueTmuxSpawnScope(candidate.Cgroup) {
		return tmuxSpawnLineage{}, false, nil
	}

	parent, exists, err := readProcessIdentity(root, candidate.PPID)
	if err != nil || !exists {
		return tmuxSpawnLineage{}, false, err
	}
	if parent.Cgroup != candidate.Cgroup ||
		!processDefinitelyPredatesIncarnation(
			processStartedAt(bootedAt, parent.StartTicks),
			incarnationStartedAt,
		) {
		return tmuxSpawnLineage{}, false, nil
	}
	return tmuxSpawnLineage{candidate: candidate, parent: parent}, true, nil
}

// stillCurrent takes the second census and reports whether both identities are
// unchanged. A PID reused between the two censuses changes at least its start
// ticks or its cgroup, so a mismatch abandons the proof rather than resolving
// it either way.
func (lineage tmuxSpawnLineage) stillCurrent(root string) (bool, error) {
	parentAfter, exists, err := readProcessIdentity(root, lineage.parent.PID)
	if err != nil || !exists {
		return false, err
	}
	candidateAfter, exists, err := readProcessIdentity(root, lineage.candidate.PID)
	if err != nil || !exists {
		return false, err
	}
	return parentAfter.PID == lineage.parent.PID &&
		parentAfter.StartTicks == lineage.parent.StartTicks &&
		parentAfter.Cgroup == lineage.parent.Cgroup &&
		candidateAfter == lineage.candidate, nil
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
	if parentEnv["GC_SESSION_ID"] == sessionID && isInfrastructureProcess(root, ppid) {
		return true, nil
	}
	return parentEnv["GC_SESSION_ID"] != sessionID, nil
}

// isInfrastructureProcess reports whether pid's comm names infrastructure (a
// tmux server or client) rather than an agent; see isInfrastructureCommand.
func isInfrastructureProcess(root string, pid int) bool {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "comm"))
	if err != nil {
		return false
	}
	return isInfrastructureCommand(string(data))
}

func readParentPID(path string) (int, bool, error) {
	ppid, _, _, ok, err := readProcStatIdentity(path)
	return ppid, ok, err
}

func readProcStatIdentity(path string) (ppid, pgid int, startTime string, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, "", false, nil
		}
		return 0, 0, "", false, err
	}
	ppid, pgid, startTime, ok, err = parseProcStatIdentity(string(data))
	if err != nil {
		return 0, 0, "", false, fmt.Errorf("parsing %s: %w", path, err)
	}
	return ppid, pgid, startTime, ok, nil
}

func parseProcStatIdentity(text string) (ppid, pgid int, startTime string, ok bool, err error) {
	closeParen := strings.LastIndexByte(text, ')')
	if closeParen < 0 || closeParen+1 >= len(text) {
		return 0, 0, "", false, fmt.Errorf("malformed proc stat record")
	}
	fields := strings.Fields(text[closeParen+1:])
	const startTimeIndexAfterComm = 19
	if len(fields) <= startTimeIndexAfterComm {
		return 0, 0, "", false, fmt.Errorf("malformed proc stat record: got %d post-comm fields", len(fields))
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, "", false, fmt.Errorf("parsing proc stat ppid: %w", err)
	}
	pgid, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, "", false, fmt.Errorf("parsing proc stat pgid: %w", err)
	}
	startTime = fields[startTimeIndexAfterComm]
	if startTime == "" {
		return 0, 0, "", false, fmt.Errorf("proc stat start time is empty")
	}
	return ppid, pgid, startTime, true, nil
}
