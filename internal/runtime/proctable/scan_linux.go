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

	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionID returns live agent root processes whose environment carries
// GC_SESSION_ID equal to id. Empty id is the orphan sweep's scan: the roots
// with any GC_SESSION_ID that were reparented away from their spawner, to
// init or the user subreaper ([OrphanCandidate]), the only roots the sweep
// may reap. It reads a process's environment only when its parent makes it
// a candidate, so a host's other processes cost one stat read each, not an
// environ read (mc-ygved).
func ScanBySessionID(id string) ([]runtime.LiveRuntime, error) {
	if err := liveScanGuard(); err != nil {
		return []runtime.LiveRuntime{}, err
	}
	if id == "" {
		return scanOrphanCandidatesWithRoot(scanRoot, detectSubreaper(os.Getpid()))
	}
	return scanWithRoot(scanRoot, id)
}

// detectSubreaper is the user subreaper the empty-id scan counts as an
// orphan parent (a systemd --user supervisor); a test sets its fixture's.
var detectSubreaper = pidutil.DetectUserSubreaperPID

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
// carries a GC_SESSION_ID, is neither infrastructure nor a program-status
// companion — a tmux server or client is never a root, whoever its parent
// is, the relay is looked through, and a tap belongs to its tmux server — and
// sits outside its parent's envelope: the parent is gone, carries a different
// GC_SESSION_ID, is infrastructure or the relay, or cannot be read
// (permission denied).
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
	if isInfrastructureProcess(scanRoot, pid) || isProgramStatusCompanion(scanRoot, pid) {
		return false
	}
	isRoot, err := isRootWithSessionID(scanRoot, pid, sessionID)
	return err == nil && isRoot
}

func scanWithRoot(root, id string) ([]runtime.LiveRuntime, error) {
	return scanWithRootSince(root, id, time.Time{})
}

func scanWithRootSince(root, id string, incarnationStartedAt time.Time) ([]runtime.LiveRuntime, error) {
	return scanEntries(root, id, incarnationStartedAt, nil)
}

// scanOrphanCandidatesWithRoot is the all-roots scan restricted to processes
// reparented to init or subreaperPID (OrphanCandidate), each decided on its
// stat before anything else is read. A stat that cannot be read is the
// entry's error, as an unreadable owner is: the scan stays fail-closed.
//
// A process whose parent is the program-status relay is decided on the
// relay's parent, as the scan reports it (see effectiveParent): an orphaned
// relay's program is the orphan. Each parent's relay check is read once per
// scan.
func scanOrphanCandidatesWithRoot(root string, subreaperPID int) ([]runtime.LiveRuntime, error) {
	relays := make(map[int]bool)
	return scanEntries(root, "", time.Time{}, func(pid int) (bool, error) {
		ppid, ok, err := readParentPID(filepath.Join(root, strconv.Itoa(pid), "stat"))
		if err != nil || !ok {
			return false, err // a vanished process is no candidate; a failed read is the entry's error
		}
		if OrphanCandidate(ppid, subreaperPID) {
			return true, nil
		}
		relay, seen := relays[ppid]
		if !seen {
			relay = ppid > 1 && isProgramStatusRelay(root, ppid)
			relays[ppid] = relay
		}
		if !relay {
			return false, nil
		}
		return OrphanCandidate(effectiveParent(root, ppid), subreaperPID), nil
	})
}

// scanEntries scans root's entries for id's roots; candidate, when set,
// decides first whether an entry is read at all.
func scanEntries(root, id string, incarnationStartedAt time.Time, candidate func(pid int) (bool, error)) ([]runtime.LiveRuntime, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return []runtime.LiveRuntime{}, fmt.Errorf("enumerating %s: %w", root, err)
	}

	var (
		out     []runtime.LiveRuntime
		scanErr error
	)
	// Boot time turns each root's start ticks into a wall-clock StartedAt;
	// without it StartedAt stays zero and callers that bound on it refuse.
	bootedAt, bootErr := readBootTime(root)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if pid, perr := strconv.Atoi(entry.Name()); candidate != nil && perr == nil && pid > 1 {
			keep, err := candidate(pid)
			if err != nil {
				scanErr = errors.Join(scanErr, &EntryError{PID: pid, Err: fmt.Errorf("reading parent for pid %d: %w", pid, err)})
				continue
			}
			if !keep {
				continue
			}
		}
		live, reported, err := scanProcEntry(root, entry.Name(), id, incarnationStartedAt)
		if err != nil {
			pid, _ := strconv.Atoi(entry.Name())
			scanErr = errors.Join(scanErr, &EntryError{PID: pid, Err: err})
			continue
		}
		if !reported {
			continue
		}
		if ticks, err := strconv.ParseUint(live.StartIdentity, 10, 64); err == nil && bootErr == nil {
			live.StartedAt = processStartedAt(bootedAt, ticks)
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
	environReadAt := time.Now()
	env, err := parseEnvironFile(filepath.Join(root, entryName, "environ"))
	if err != nil {
		return runtime.LiveRuntime{}, false, unreadableEnvironScanError(root, pid, id, incarnationStartedAt, environReadAt, err)
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
	// Neither program-status companion is a root: the relay is looked
	// through (IsProgramStatusRelayArgv), and the program it runs is reported
	// in its place; a tap belongs to its tmux server (IsProgramStatusTapArgv).
	if isProgramStatusCompanion(root, pid) {
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
	stat, _, _ := readProcStat(filepath.Join(root, entryName, "stat"))
	ppid := effectiveParent(root, stat.ppid)
	comm, _ := os.ReadFile(filepath.Join(root, entryName, "comm"))
	return runtime.LiveRuntime{
		SessionID: sessionID,
		City:      city,
		Epoch:     epoch,
		PID:       pid,
		// A relay's program reports the relay's parent (effectiveParent), so
		// it reads as the relay itself would have: owned while the relay's
		// tmux server is its parent, an orphan once the relay is adopted.
		PPID: ppid,
		// Read from the SAME ppid that is reported above, so a caller fencing
		// on this field is fencing on the parent it was told about. A pid <= 1
		// parent is init or an unreadable stat, never the provider's server, so
		// it is refused without a comm read.
		ParentIsProviderInfrastructure: ppid > 1 && isInfrastructureProcess(root, ppid),
		// The same token ProcessIdentity and RootStartIdentity read, so a
		// killer can re-check it against the live process.
		StartIdentity: stat.startTime,
		Name:          strings.TrimSpace(string(comm)),
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
// could not be read adds to the scan, or nil when the process is proven not
// to matter. Three proofs excuse it: a permission-denied read of a process
// proven no runtime at all (unreadableProcessProvenNonRuntime, which runs
// first and needs no incarnation bound), a process proven to predate
// incarnationStartedAt, and a process proven outside the incarnation by its
// tmux-spawn parent. A failed age proof still leaves the scan incomplete.
// environReadAt is when the failed environ read began; the non-runtime proof
// only accepts a process that already existed then.
func unreadableEnvironScanError(root string, pid int, id string, incarnationStartedAt, environReadAt time.Time, readErr error) error {
	if errors.Is(readErr, fs.ErrPermission) && unreadableProcessProvenNonRuntime(root, pid, environReadAt) {
		return nil
	}
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

// unreadableProcessProvenNonRuntime reports whether a same-uid process whose
// environment could not be read (EACCES) is positively proven, from the
// signals /proc still exposes for it, to be something no gc session runtime
// can be. An environ read is a ptrace-mode access, so a same-uid process
// refuses it when it is non-dumpable: a privilege-transitioned exec (sudo), a
// helper that sets PR_SET_DUMPABLE=0 on itself, or an agent CLI that hardens
// itself the same way. Non-dumpability alone therefore proves nothing, and an
// unproven process stays an entry error: the scan keeps failing closed for
// anything that could be ours. Two shapes are proven:
//
//   - The systemd user manager's own cgroup, user@<uid>.service/init.scope,
//     which holds only `systemd --user` and its non-dumpable `(sd-pam)`
//     helper. systemd starts every unit, scope and session elsewhere, so no
//     process gc or tmux launches can live there.
//   - A credential agent (ssh-agent, gpg-agent and its gpg helpers), named
//     alike by its comm and by its argv[0]. These set PR_SET_DUMPABLE=0 by
//     design and never run an agent provider, so even one started from a
//     session's shell holds no session work for a replacement to race.
//
// The proof is bound to the process the environ read failed on: the process
// it inspects must have started before environReadAt (beyond /proc's clock
// quantization), and its start identity is read again after the proof, so a
// pid recycled before or during the proof abandons it rather than resolving
// it either way. Any read that fails leaves the process unproven.
func unreadableProcessProvenNonRuntime(root string, pid int, environReadAt time.Time) bool {
	bootedAt, err := readBootTime(root)
	if err != nil {
		return false
	}
	before, exists, err := readProcessStat(root, pid)
	if err != nil || !exists ||
		!processDefinitelyPredatesIncarnation(processStartedAt(bootedAt, before.StartTicks), environReadAt) {
		return false
	}
	if !userManagerInitScopeProcess(root, pid, os.Geteuid()) && !credentialAgentProcess(root, pid) {
		return false
	}
	after, exists, err := readProcessStat(root, pid)
	return err == nil && exists && after == before
}

// userManagerInitScopeProcess reports whether pid sits in uid's systemd user
// manager's own init.scope (…/user@<uid>.service/init.scope). The scan has
// already proven pid is uid's, so another user's manager is no proof.
func userManagerInitScopeProcess(root string, pid, uid int) bool {
	cgroup, ok := readUnifiedCgroup(root, pid)
	return ok && isUserManagerInitScope(cgroup, uid)
}

// readUnifiedCgroup returns pid's cgroup v2 path: the "0::" line, which a
// unified host's cgroup file holds alone and a hybrid host's beside its v1
// controller lines. ok is false when there is none or the file is unreadable.
func readUnifiedCgroup(root string, pid int) (string, bool) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", false
	}
	var unified string
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		path, ok := strings.CutPrefix(strings.TrimSpace(line), "0::")
		if !ok {
			continue
		}
		if found || path == "" {
			return "", false
		}
		unified, found = filepath.Clean(path), true
	}
	return unified, found
}

func isUserManagerInitScope(cgroup string, uid int) bool {
	if filepath.Base(cgroup) != "init.scope" {
		return false
	}
	return filepath.Base(filepath.Dir(cgroup)) == "user@"+strconv.Itoa(uid)+".service"
}

// credentialAgents are the non-dumpable credential helpers a host or a
// session's shell starts that never run an agent provider.
var credentialAgents = map[string]struct{}{
	"ssh-agent": {}, "gpg-agent": {}, "scdaemon": {}, "dirmngr": {}, "keyboxd": {},
}

// credentialAgentProcess reports whether pid's comm and argv[0] basename both
// name the same credential agent. Requiring both keeps a renamed thread or an
// unrelated binary that merely shares one of them unproven.
func credentialAgentProcess(root string, pid int) bool {
	dir := filepath.Join(root, strconv.Itoa(pid))
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return false
	}
	name := strings.TrimSpace(string(comm))
	if _, ok := credentialAgents[name]; !ok {
		return false
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return false
	}
	argv0, _, _ := strings.Cut(string(cmdline), "\x00")
	return filepath.Base(strings.TrimSpace(argv0)) == name
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
	switch {
	case errors.Is(err, fs.ErrPermission):
		// An unreadable parent names no session we can see: outside the envelope.
		parentEnv = nil
	case err != nil:
		return false, err
	}
	if parentEnv["GC_SESSION_ID"] == sessionID && (isInfrastructureProcess(root, ppid) || isProgramStatusRelay(root, ppid)) {
		return true, nil
	}
	return parentEnv["GC_SESSION_ID"] != sessionID, nil
}

// isProgramStatusRelay reports whether pid is gc's program-status relay
// (IsProgramStatusRelayArgv). An unreadable argv reports false.
func isProgramStatusRelay(root string, pid int) bool {
	argv, err := readArgv(root, pid)
	return err == nil && IsProgramStatusRelayArgv(argv)
}

// isProgramStatusCompanion reports whether pid is gc's program-status relay
// or tap (IsProgramStatusRelayArgv, IsProgramStatusTapArgv), reading its argv
// once. An unreadable argv reports false.
func isProgramStatusCompanion(root string, pid int) bool {
	argv, err := readArgv(root, pid)
	return err == nil && (IsProgramStatusRelayArgv(argv) || IsProgramStatusTapArgv(argv))
}

// effectiveParent is the parent the scan reports for a process whose parent
// is ppid: ppid itself, or, when ppid is the program-status relay, the
// relay's own parent (0 when it cannot be read). The relay is looked
// through, so its program stands where the relay stood.
func effectiveParent(root string, ppid int) int {
	if ppid <= 1 || !isProgramStatusRelay(root, ppid) {
		return ppid
	}
	grand, ok, err := readParentPID(filepath.Join(root, strconv.Itoa(ppid), "stat"))
	if err != nil || !ok {
		return 0
	}
	return grand
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
	stat, ok, err := readProcStat(path)
	return stat.ppid, ok, err
}

// procStat is what the scanner and the snapshots read from one
// /proc/<pid>/stat record.
type procStat struct {
	ppid      int
	pgid      int
	sid       int // session ID: the PID of the session's leader
	startTime string
}

func readProcStat(path string) (procStat, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return procStat{}, false, nil
		}
		return procStat{}, false, err
	}
	stat, err := parseProcStat(string(data))
	if err != nil {
		return procStat{}, false, fmt.Errorf("parsing %s: %w", path, err)
	}
	return stat, true, nil
}

func parseProcStat(text string) (procStat, error) {
	closeParen := strings.LastIndexByte(text, ')')
	if closeParen < 0 || closeParen+1 >= len(text) {
		return procStat{}, fmt.Errorf("malformed proc stat record")
	}
	fields := strings.Fields(text[closeParen+1:])
	const startTimeIndexAfterComm = 19
	if len(fields) <= startTimeIndexAfterComm {
		return procStat{}, fmt.Errorf("malformed proc stat record: got %d post-comm fields", len(fields))
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return procStat{}, fmt.Errorf("parsing proc stat ppid: %w", err)
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return procStat{}, fmt.Errorf("parsing proc stat pgid: %w", err)
	}
	sid, err := strconv.Atoi(fields[3])
	if err != nil {
		return procStat{}, fmt.Errorf("parsing proc stat session: %w", err)
	}
	startTime := fields[startTimeIndexAfterComm]
	if startTime == "" {
		return procStat{}, fmt.Errorf("proc stat start time is empty")
	}
	return procStat{ppid: ppid, pgid: pgid, sid: sid, startTime: startTime}, nil
}
