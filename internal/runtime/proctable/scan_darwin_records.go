package proctable

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The pure half of the Darwin scan (scan_darwin.go): one ps snapshot's
// records and the roots they hold. It carries no build tag, so Linux CI runs
// its tests too.

// psRecord is one line of "ps eww -ax -o pid=,ppid=,command=", whose command
// column holds argv and then the environment, split on whitespace.
type psRecord struct {
	pid  int
	ppid int
	// command and arg1 are the column's first two words: argv[0] and argv[1]
	// (an environment entry when argv has one element). ps joins arguments
	// with spaces, so an argument that holds one is split.
	command string
	arg1    string
	env     map[string]string
}

// parsePSRecord parses one ps line; ok is false for a line that is not
// "<pid> <ppid> <command>".
func parsePSRecord(line string) (psRecord, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return psRecord{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return psRecord{}, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return psRecord{}, false
	}
	record := psRecord{pid: pid, ppid: ppid, command: darwinPSCommand(fields), env: parseInlineEnv(fields[2:])}
	if len(fields) > 3 {
		record.arg1 = fields[3]
	}
	return record, true
}

func parseInlineEnv(fields []string) map[string]string {
	env := make(map[string]string)
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key == "" {
			continue
		}
		env[key] = value
	}
	return env
}

// scanRecordsBySessionID is the pure half of ScanBySessionID, over an
// already-read process table.
func scanRecordsBySessionID(records map[int]psRecord, id string) []runtime.LiveRuntime {
	var out []runtime.LiveRuntime
	for _, record := range records {
		if record.pid <= 1 || !isRecordScanRoot(records, record) {
			continue
		}
		sessionID := record.env["GC_SESSION_ID"]
		if id != "" && sessionID != id {
			continue
		}
		epoch, _ := strconv.Atoi(record.env["GC_RUNTIME_EPOCH"])
		city := record.env["GC_CITY_PATH"]
		if city == "" {
			city = record.env["GC_CITY"]
		}
		ppid, parentIsInfrastructure := reportedParent(records, record.ppid)
		out = append(out, runtime.LiveRuntime{
			SessionID:                      sessionID,
			City:                           city,
			Epoch:                          epoch,
			PID:                            record.pid,
			PPID:                           ppid,
			ParentIsProviderInfrastructure: parentIsInfrastructure,
			Name:                           filepath.Base(strings.TrimSpace(record.command)),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PID < out[j].PID
	})
	if out == nil {
		out = []runtime.LiveRuntime{}
	}
	return out
}

// orphanCandidates keeps the roots reparented to init or subreaperPID.
func orphanCandidates(roots []runtime.LiveRuntime, subreaperPID int) []runtime.LiveRuntime {
	out := roots[:0]
	for _, r := range roots {
		if OrphanCandidate(r.PPID, subreaperPID) {
			out = append(out, r)
		}
	}
	return out
}

// isRecordScanRoot is the pure half of IsScanRoot, and the root test of
// scanRecordsBySessionID.
func isRecordScanRoot(records map[int]psRecord, record psRecord) bool {
	// A process that is itself infrastructure is never an agent root,
	// whoever its parent is. The tmux server a session's first new-session
	// call founded inherits that session's GC_SESSION_ID and reparents to
	// launchd, so the parent-envelope test below cannot exclude it; reported
	// as a root, it is handed to the orphan sweep, which kills the one server
	// every agent in the city shares — one socket per city is the default
	// topology, so that is the whole city (gastownhall/gascity#5392).
	if isInfrastructureCommand(record.command) {
		return false
	}
	// Neither program-status companion is a root: the relay is looked
	// through (IsProgramStatusRelayArgv), and the program it runs is the root
	// in its place; a tap belongs to its tmux server (IsProgramStatusTapArgv).
	if isRelayRecord(record) || isTapRecord(record) {
		return false
	}
	sessionID := record.env["GC_SESSION_ID"]
	if sessionID == "" {
		return false
	}
	parent, ok := records[record.ppid]
	if !ok || parent.env["GC_SESSION_ID"] != sessionID {
		return true
	}
	// The tmux server and the relay carry the session's GC_SESSION_ID, but
	// neither is the session's runtime: the process under either one is.
	return isInfrastructureCommand(parent.command) || isRelayRecord(parent)
}

// isRelayRecord reports whether record is gc's program-status relay
// (IsProgramStatusRelayArgv).
func isRelayRecord(record psRecord) bool {
	return IsProgramStatusRelayArgv([]string{record.command, record.arg1})
}

// isTapRecord reports whether record is gc's program-status tap
// (IsProgramStatusTapArgv).
func isTapRecord(record psRecord) bool {
	return IsProgramStatusTapArgv([]string{record.command, record.arg1})
}

// reportedParent is the parent the scan reports for a root whose parent is
// ppid, and whether the snapshot shows that parent to be provider
// infrastructure. It is ppid itself or, when ppid is the program-status
// relay, the relay's own parent, as on Linux (scan_linux.go's
// effectiveParent): the relay's program reads as the relay itself would have,
// owned while the relay's tmux server is its parent and an orphan once
// launchd adopts the relay. A parent the snapshot does not hold (it exited,
// or ps could not report it) is not provider infrastructure: the scan only
// ever reports what it positively saw.
func reportedParent(records map[int]psRecord, ppid int) (int, bool) {
	if relay, ok := records[ppid]; ok && isRelayRecord(relay) {
		ppid = relay.ppid
	}
	parent, ok := records[ppid]
	return ppid, ok && isInfrastructureCommand(parent.command)
}
