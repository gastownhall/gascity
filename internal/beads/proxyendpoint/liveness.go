package proxyendpoint

import (
	"errors"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// Verdict is the typed classification of one endpoint inspection. It is an enum
// and never a bool: "we could not tell" is a distinct answer from both "live"
// and "gone", and the three lead to different actions — serve, wait, and
// escalate through a bd verb respectively. A caller handed a bool would have to
// guess which of those the false meant.
type Verdict int

// Verdicts, in the order a reader meets them: the healthy one, the ordinary
// stopped ones, then the ones that mean somebody else's process or an
// unanswerable question.
const (
	// VerdictUnknown is the zero value: nothing has been inspected. It is never
	// a conclusion.
	VerdictUnknown Verdict = iota
	// VerdictLive means the record validates and the PID it names is still the
	// bd proxy that published it.
	VerdictLive
	// VerdictNoRecord means the root holds no record. bd removes it on an
	// orderly exit, so this is a stopped proxy, not a fault.
	VerdictNoRecord
	// VerdictMalformed means the record could not be decoded.
	VerdictMalformed
	// VerdictLegacySchema means the record predates schema 2 and carries no
	// birth token, so no generation can be established from it.
	VerdictLegacySchema
	// VerdictNotOurs means the record describes another root's proxy, or a
	// field is out of range.
	VerdictNotOurs
	// VerdictDead means the recorded PID is not running. bd leaves the record
	// behind when its proxy is SIGKILLed and quarantines it only on the next
	// adoption in that root, so a stale record is an expected state.
	VerdictDead
	// VerdictForeignProcess means the recorded PID is alive but its argv is not
	// bd's proxy supervisor for this root — the recycled-PID case, where a
	// bare liveness probe would have said yes.
	VerdictForeignProcess
	// VerdictBirthMismatch means argv still names bd's supervisor for this root
	// but the process was born at a different time: bd restarted the proxy and
	// gc did not observe it, so the record describes a generation gc never
	// admitted.
	VerdictBirthMismatch
	// VerdictUndetermined means a read the proof depends on failed — an argv
	// gc could not obtain. It is not live and not dead, and a caller must not
	// dial on it.
	VerdictUndetermined
)

// String renders the verdict as the stable token doctor and the diagnostics
// report. Automation reads these, so they are lower-case and hyphen-free.
func (v Verdict) String() string {
	switch v {
	case VerdictLive:
		return "live"
	case VerdictNoRecord:
		return "no_record"
	case VerdictMalformed:
		return "malformed"
	case VerdictLegacySchema:
		return "legacy_schema"
	case VerdictNotOurs:
		return "not_ours"
	case VerdictDead:
		return "dead"
	case VerdictForeignProcess:
		return "foreign_process"
	case VerdictBirthMismatch:
		return "birth_mismatch"
	case VerdictUndetermined:
		return "undetermined"
	default:
		return "unknown"
	}
}

// Live reports whether the verdict permits gc to treat the endpoint as bd's
// live proxy for this root. Exactly one verdict does.
func (v Verdict) Live() bool { return v == VerdictLive }

// Evidence names the strongest liveness proof gc obtained. It is reported
// alongside the verdict because the two are different questions: a live verdict
// resting on argv alone is correct and weaker than one that also matched the
// birth token, and an operator comparing two hosts needs to see which they got.
type Evidence int

// Evidence strengths, weakest first.
const (
	// EvidenceNone means no process-table proof was obtained.
	EvidenceNone Evidence = iota
	// EvidenceArgv means the PID's argv named bd's supervisor for this root.
	EvidenceArgv
	// EvidenceArgvBirth adds a recomputed birth token equal to the record's,
	// which rules out a PID recycled into an identically-invoked process.
	EvidenceArgvBirth
)

// String renders the evidence as the token the diagnostics report.
func (e Evidence) String() string {
	switch e {
	case EvidenceArgv:
		return "argv"
	case EvidenceArgvBirth:
		return "argv+birth"
	default:
		return "none"
	}
}

// BirthOutcome is what the birth recompute concluded. `unavailable` is explicit
// rather than folded into "no match" because the two lead to opposite actions:
// an unavailable recompute keeps a live endpoint live on argv evidence, and a
// mismatch retires it.
type BirthOutcome int

// Birth recompute outcomes.
const (
	// BirthUnchecked means the recompute was not attempted — there was no
	// live process to compare against.
	BirthUnchecked BirthOutcome = iota
	// BirthMatch means the recomputed token equals the record's.
	BirthMatch
	// BirthMismatch means the PID is alive but was born at another time.
	BirthMismatch
	// BirthUnavailable means this host cannot recompute the token.
	BirthUnavailable
)

// String renders the outcome as the token the diagnostics report.
func (b BirthOutcome) String() string {
	switch b {
	case BirthMatch:
		return "match"
	case BirthMismatch:
		return "mismatch"
	case BirthUnavailable:
		return "unavailable"
	default:
		return "unchecked"
	}
}

// ProcessTable is the set of process-table reads an inspection needs. It is a
// struct of functions rather than an interface with one implementation because
// every unit test drives a different subset: a fabricated table proves the
// recycled-PID, restarted-proxy and unreadable-argv branches without a proxy,
// a root, or a privileged read anywhere in the test binary.
type ProcessTable struct {
	// Alive reports whether pid names a running, non-zombie process.
	Alive func(pid int) bool
	// Argv returns the process's command line.
	Argv func(pid int) ([]string, error)
	// Birth recomputes bd's process-birth token, or returns an error wrapping
	// ErrBirthUnavailable where the host has no recipe for it.
	Birth func(pid int) (string, error)
}

// DefaultProcessTable reads the real process table.
func DefaultProcessTable() ProcessTable {
	return ProcessTable{
		Alive: pidutil.Alive,
		Argv:  pidutil.Cmdline,
		Birth: capturePlatformBirth,
	}
}

// Liveness is the process-table half of an inspection.
type Liveness struct {
	// Evidence is the strongest proof obtained.
	Evidence Evidence
	// Birth is what the birth recompute concluded.
	Birth BirthOutcome
	// Argv is the recorded process's command line, when it could be read. It
	// is kept because it is the evidence behind the verdict, and a diagnostic
	// that names the flag it parsed is checkable by hand.
	Argv []string
	// IdlePolicy is the policy the supervisor's own argv implies, or the zero
	// IdlePolicy when argv carried no --idle-timeout.
	IdlePolicy IdlePolicy
	// Err is the read failure behind an undetermined verdict.
	Err error
}

// Endpoint is gc's complete, validated view of one proxy root.
type Endpoint struct {
	// Root is the proxy root the record was read from.
	Root string
	// RootID is the identity gc computed for Root, empty when it could not be
	// computed (a root that does not exist).
	RootID string
	// Record is the decoded record. It is the zero Record when there was none.
	Record Record
	// Liveness is the process-table half.
	Liveness Liveness
	// Verdict is the combined classification. It is the value callers branch
	// on and the value doctor reports.
	Verdict Verdict
	// Err is the failure behind any verdict other than live.
	Err error
}

// Host is the interface address bd's proxy listens on. The listener is
// loopback-only by construction (beads dbproxy/proxy/server.go binds
// 127.0.0.1), so the record publishes a port and not an address.
const Host = "127.0.0.1"

// Inspect reads, validates and liveness-checks the endpoint in root.
//
// The order is the order the evidence gets cheaper to trust: decode, then
// validate the fields including the recomputed root identity, then ask the
// process table. Nothing here dials anything — a caller that needs to know
// whether the port answers calls Probe, which costs the backend a session.
func Inspect(root string, pt ProcessTable) Endpoint {
	ep := Endpoint{Root: root}
	if id, err := RootID(root); err == nil {
		ep.RootID = id
	}

	rec, err := Read(root)
	if err != nil {
		ep.Err = err
		switch {
		case errors.Is(err, ErrNoProxy):
			ep.Verdict = VerdictNoRecord
		case errors.Is(err, ErrMalformed):
			ep.Verdict = VerdictMalformed
		default:
			ep.Verdict = VerdictUndetermined
		}
		return ep
	}
	ep.Record = rec

	if err := Validate(rec, root); err != nil {
		ep.Err = err
		switch {
		case errors.Is(err, ErrLegacyProxy):
			ep.Verdict = VerdictLegacySchema
		case errors.Is(err, ErrNotOurs):
			ep.Verdict = VerdictNotOurs
		default:
			ep.Verdict = VerdictUndetermined
		}
		return ep
	}

	ep.Liveness, ep.Verdict = checkLiveness(rec, root, pt)
	ep.Err = ep.Liveness.Err
	return ep
}

// checkLiveness asks the process table whether the recorded PID is still the bd
// proxy that published the record.
//
// Liveness alone is never the answer. bd leaves the record on disk when its
// proxy is killed, so once that PID is recycled a bare kill(pid, 0) would
// protect — or dial — an unrelated process; the argv match for THIS root is the
// proof, and the birth token closes the remaining window where the recycled
// process happens to be another bd proxy for the same root.
func checkLiveness(rec Record, root string, pt ProcessTable) (Liveness, Verdict) {
	var live Liveness
	if pt.Alive == nil || pt.Argv == nil {
		live.Err = errors.New("proxyendpoint: no process table to check liveness against")
		return live, VerdictUndetermined
	}
	if !pt.Alive(rec.PID) {
		return live, VerdictDead
	}
	argv, err := pt.Argv(rec.PID)
	if err != nil {
		live.Err = err
		return live, VerdictUndetermined
	}
	live.Argv = argv
	if !ArgvRunsChild(argv) || !ArgvNamesRoot(argv, root) {
		return live, VerdictForeignProcess
	}
	live.Evidence = EvidenceArgv
	live.IdlePolicy = ArgvIdlePolicy(argv)

	if pt.Birth == nil {
		live.Birth = BirthUnavailable
		return live, VerdictLive
	}
	birth, err := pt.Birth(rec.PID)
	switch {
	case errors.Is(err, ErrBirthUnavailable):
		live.Birth = BirthUnavailable
		return live, VerdictLive
	case err != nil:
		// The process was alive a moment ago and its argv matched, so a failed
		// stat is most likely the process exiting between the two reads. That is
		// not a mismatch and not a live endpoint either.
		live.Err = err
		live.Birth = BirthUnchecked
		return live, VerdictUndetermined
	case birth != rec.Birth:
		live.Birth = BirthMismatch
		return live, VerdictBirthMismatch
	default:
		live.Birth = BirthMatch
		live.Evidence = EvidenceArgvBirth
		return live, VerdictLive
	}
}

// ArgvRunsChild reports whether argv invokes bd's proxy-supervisor verb.
//
// It deliberately ignores argv[0]. bd execs the child as os.Executable(), so
// the filename is the operator's choice and not a contract: a versioned pin
// (BD_BIN=/opt/beads/bd-1.3.0) is a supported shape, and requiring the basename
// "bd" would unrecognise every proxy of a versioned install.
func ArgvRunsChild(argv []string) bool {
	return len(argv) >= 2 && argv[1] == ChildVerb
}

// ArgvNamesRoot reports whether argv carries `--root <root>` for this root,
// comparing resolved so a symlinked spelling of the same directory matches.
func ArgvNamesRoot(argv []string, root string) bool {
	root = pathutil.NormalizePathForCompare(root)
	if root == "" {
		return false
	}
	value, ok := argvFlagValue(argv, RootFlag)
	if !ok || value == "" {
		return false
	}
	return pathutil.SamePath(value, root)
}

// ArgvMentionsRoot reports whether ANY --root occurrence in argv resolves to
// this root, comparing symlink-resolved as ArgvNamesRoot does.
//
// It is the protection question's spelling of the same test, and it is
// deliberately wider than the admission one. ArgvNamesRoot answers "which root
// is this process serving", so it takes the value a flag parser would take — the
// last occurrence. Protection asks "could killing this process kill bd's proxy
// for this root", and for that any mention is enough: a repeated flag is a shape
// bd 1.3.0 does not emit, so a process carrying one is a process gc cannot
// explain, and an unexplained process that names this root must not be reaped.
func ArgvMentionsRoot(argv []string, root string) bool {
	root = pathutil.NormalizePathForCompare(root)
	if root == "" {
		return false
	}
	for _, value := range argvFlagValues(argv, RootFlag) {
		if value != "" && pathutil.SamePath(value, root) {
			return true
		}
	}
	return false
}

// ArgvIdlePolicy extracts the supervisor's effective idle window from its argv.
//
// This is the authoritative value for a live proxy: bd resolves the sidecar and
// its own default before the fork-exec and passes the result on the command
// line, so the flag describes the process that is running rather than what the
// next one will do. An argv with no flag yields the zero IdlePolicy, whose Kind
// is IdleUnknown, so the caller falls back to the sidecar instead of guessing.
func ArgvIdlePolicy(argv []string) IdlePolicy {
	value, ok := argvFlagValue(argv, IdleTimeoutFlag)
	if !ok {
		return IdlePolicy{}
	}
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return IdlePolicy{}
	}
	return idlePolicyFromDuration(d, IdleSourceArgv)
}

// argvFlagValue returns the value of flag in argv, accepting both `--flag
// value` and `--flag=value`. The last occurrence wins, which is what a flag
// parser does with a repeated flag.
func argvFlagValue(argv []string, flag string) (string, bool) {
	values := argvFlagValues(argv, flag)
	if len(values) == 0 {
		return "", false
	}
	return values[len(values)-1], true
}

// argvFlagValues returns every value flag carries in argv, in order, accepting
// both `--flag value` and `--flag=value`. A trailing bare flag with no value is
// not an occurrence: there is nothing to compare.
func argvFlagValues(argv []string, flag string) []string {
	var values []string
	for i, arg := range argv {
		switch {
		case arg == flag && i+1 < len(argv):
			values = append(values, argv[i+1])
		case strings.HasPrefix(arg, flag+"="):
			values = append(values, strings.TrimPrefix(arg, flag+"="))
		}
	}
	return values
}
