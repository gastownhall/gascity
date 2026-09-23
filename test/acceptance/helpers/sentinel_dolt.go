package acceptancehelpers

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// SentinelDolt is a PATH-first `dolt` that records who ran it and then execs the
// real one.
//
// It exists for one assertion that cannot be made any other way: gc must never
// spawn a Dolt server for a scope bd owns. That claim is made in gc's source by
// omission — the proxied open window sets BEADS_DOLT_SERVER_MODE with
// BEADS_DOLT_AUTO_START=0 and withholds the whole BEADS_/BD_ namespace around the
// open — and an assertion about an omission is exactly the kind that passes for
// the wrong reason. A process that is never started leaves no trace to look for,
// so "we found no trace" is equally true of a correct gc, a gc whose spawn failed
// for an unrelated reason, and a test that was looking in the wrong place.
//
// What makes the assertion falsifiable is that the same instrument records the
// dolt processes that ARE legitimately spawned: bd's proxy starts one (that is
// the whole topology), and gc itself runs `dolt version` from a doctor check. So
// the sentinel can be shown to see gc's dolt children AND to see sql-server
// spawns, and the claim becomes the intersection of the two being empty rather
// than an absence of evidence.
//
// The recording is a side effect of a real run: the shim execs the pinned dolt,
// so the city behaves exactly as it would without it.
//
// It has a second mode for the one place a pass-through instrument would be a
// hazard: an open of the linked library in the TEST's own process (see
// TrapThisProcess). There the sentinel records and REFUSES, so the control
// that proves the library's auto-start reaches it cannot start a Dolt server,
// and a regression in the fence it watches cannot either.
type SentinelDolt struct {
	// Dir holds the shim and is what a test prepends to PATH.
	Dir string
	// Path is the shim itself.
	Path string
	// Real is the dolt the shim execs.
	Real string

	t       *testing.T
	logPath string
}

// SentinelDoltTrapEnv switches the shim from recording-then-exec to
// recording-then-refusing, for any process that inherits it. TrapThisProcess
// sets it for the calling test's own process only.
const SentinelDoltTrapEnv = "GC_ACCEPTANCE_SENTINEL_DOLT_TRAP"

// sentinelDoltTrapStatus is the shim's exit status when it refuses. It is not
// one dolt uses, so a refusal is recognizable in a library error.
const sentinelDoltTrapStatus = 97

// SentinelDoltInvocation is one recorded dolt exec.
type SentinelDoltInvocation struct {
	// PPID is the process that spawned it.
	PPID string
	// Parent is that process's own command line, read from /proc at exec time.
	//
	// It is captured by the shim rather than looked up afterwards because the
	// parent is usually gone by the time a test reads the log: bd's proxy child
	// outlives its dolt, but a short-lived `dolt version` outlives nothing. This
	// is what makes ancestry answerable at all.
	Parent string
	// Argv is the command line as dolt received it, without argv[0].
	Argv []string
}

// Subcommand is the dolt verb, or "" for a bare `dolt`.
func (i SentinelDoltInvocation) Subcommand() string {
	if len(i.Argv) == 0 {
		return ""
	}
	return i.Argv[0]
}

// IsServer reports whether this exec starts a Dolt SQL server — the one shape
// the no-spawn claim is about. `dolt version`, `dolt config` and the rest are
// harmless reads and gc is free to run them.
func (i SentinelDoltInvocation) IsServer() bool {
	return i.Subcommand() == "sql-server"
}

// ParentCommand is the base name of the parent's argv[0] — "gc", "bd",
// "timeout", and so on.
//
// Deliberately NOT a substring match on the whole parent command line, and this
// is a trap worth naming: the first draft of the no-spawn row classified a parent
// as gc's with strings.Contains(parent, "/gc"), and every bd process in an
// acceptance run matched, because its argv names a city under
// /tmp/gc-acceptance-*. The row failed reporting that gc had spawned a Dolt
// server when what it had found was bd's proxy child doing its job. Only argv[0]
// says who a process IS.
func (i SentinelDoltInvocation) ParentCommand() string {
	fields := strings.Fields(i.Parent)
	if len(fields) == 0 {
		return ""
	}
	return filepath.Base(fields[0])
}

// ParentCommandIs reports whether the parent's own binary is name.
func (i SentinelDoltInvocation) ParentCommandIs(name string) bool {
	return i.ParentCommand() == name
}

// NewSentinelDolt writes a shim directory containing a `dolt` that records into
// its own log and execs realDolt.
//
// realDolt must already be resolved, and it must NOT be a symlink the shim could
// resolve back to itself: the shim's whole job is to be the `dolt` PATH finds, so
// an unresolved target is an exec loop.
func NewSentinelDolt(t *testing.T, realDolt string) *SentinelDolt {
	t.Helper()
	if strings.TrimSpace(realDolt) == "" {
		t.Fatal("NewSentinelDolt needs a resolved dolt path")
	}
	resolved, err := filepath.EvalSymlinks(realDolt)
	if err != nil {
		t.Fatalf("resolve the real dolt %s: %v", realDolt, err)
	}
	dir := filepath.Join(TempDir(t), "sentinel-dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create sentinel dolt directory: %v", err)
	}
	s := &SentinelDolt{
		Dir:     dir,
		Path:    filepath.Join(dir, "dolt"),
		Real:    resolved,
		t:       t,
		logPath: filepath.Join(dir, "invocations.log"),
	}
	if filepath.Clean(s.Path) == filepath.Clean(resolved) {
		t.Fatalf("the sentinel would exec itself: %s", s.Path)
	}

	// One write per invocation, with the same two ASCII separators RecordingBD
	// uses and for the same reason: a dolt argv carries paths and a `--config`
	// value, and a line-oriented log would split one exec into two records.
	//
	// The parent's command line is read from /proc with a redirect rather than a
	// `cat`, so the only extra process is the `tr` that turns dolt's NUL
	// separators into spaces. Failures are swallowed on purpose: an instrument
	// must not be able to fail the city it is observing, and a parent that has
	// already exited is a legitimate answer of "unknown".
	//
	// In trap mode (SentinelDoltTrapEnv set) the record is written FIRST and the
	// shim then refuses without exec'ing anything, so a trapped exec is always
	// in the log and never runs.
	script := fmt.Sprintf(`#!/bin/sh
us='%s'
rs='%s'
parent=$(tr '\0' ' ' </proc/${PPID:-0}/cmdline 2>/dev/null)
record="${PPID:-0}$us$parent$us"
for arg in "$@"; do
	record="$record$arg$us"
done
printf '%%s\n' "$record$rs" >>%s 2>/dev/null || true
if [ -n "${%s:-}" ]; then
	printf 'sentinel dolt: trapped, not running: dolt %%s\n' "$*" >&2
	exit %d
fi
exec %s "$@"
`, fieldSeparator, recordSeparator, shellQuote(s.logPath), SentinelDoltTrapEnv, sentinelDoltTrapStatus, shellQuote(resolved))
	if err := os.WriteFile(s.Path, []byte(script), 0o755); err != nil { //nolint:gosec // the shim must be executable
		t.Fatalf("write sentinel dolt shim: %v", err)
	}
	return s
}

// Invocations returns every recorded dolt exec, oldest first. An absent log means
// no dolt ran, which is a legitimate answer and not an error.
//
// A record whose first field is not a pid fails the test, exactly as
// RecordingBD's does: a census that silently dropped a record would report a
// number nobody can reproduce, and here the number is load-bearing in the
// direction that matters — a dropped gc-ancestored server spawn would read as
// proof that gc spawned nothing.
func (s *SentinelDolt) Invocations() []SentinelDoltInvocation {
	s.t.Helper()
	data, err := os.ReadFile(s.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		s.t.Fatalf("read sentinel dolt log: %v", err)
	}
	invocations, parseErr := parseSentinelDolt(data)
	if parseErr != nil {
		s.t.Fatalf("sentinel dolt log %s: %v", s.logPath, parseErr)
	}
	return invocations
}

// parseSentinelDolt decodes the shim's wire format. Separated from Invocations so
// the refusal above has a test that does not have to fail one.
func parseSentinelDolt(data []byte) ([]SentinelDoltInvocation, error) {
	var out []SentinelDoltInvocation
	for _, record := range strings.Split(string(data), recordSeparator) {
		record = strings.TrimLeft(record, "\n")
		if record == "" {
			continue
		}
		fields := strings.Split(strings.TrimSuffix(record, fieldSeparator), fieldSeparator)
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		if _, convErr := strconv.Atoi(fields[0]); convErr != nil {
			return nil, fmt.Errorf("a record's first field %q is not a pid: two dolt execs interleaved their writes, so this log's ancestry cannot be trusted", fields[0])
		}
		out = append(out, SentinelDoltInvocation{
			PPID:   fields[0],
			Parent: strings.TrimSpace(fields[1]),
			Argv:   fields[2:],
		})
	}
	return out, nil
}

// TrapThisProcess arms the sentinel for the calling test's OWN process, for the
// rest of t: the sentinel's directory goes first on this process's PATH, and
// the trap switch is set, so every `dolt` this process execs — which is every
// dolt the linked library execs, because its auto-start resolves dolt with
// exec.LookPath — is recorded and refused.
//
// Refused, because the rows that need it open the library in-process against
// a scope whose Dolt data a spawn would touch: the positive control exists to
// make the library's auto-start run, and the negative row exists to catch a
// fence that stopped holding. A pass-through instrument would start a real
// server in either case. Processes the test starts with an explicit
// environment (gc, bd, through helpers.Env) do not inherit either change.
//
// It uses t.Setenv, so it cannot be called from a parallel test.
func (s *SentinelDolt) TrapThisProcess(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", s.Dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(SentinelDoltTrapEnv, "1")
}

// FromThisProcess returns the recorded execs whose parent is the test process
// itself — the only ones the linked library, running in-process, can produce.
func (s *SentinelDolt) FromThisProcess() []SentinelDoltInvocation {
	s.t.Helper()
	self := strconv.Itoa(os.Getpid())
	var out []SentinelDoltInvocation
	for _, i := range s.Invocations() {
		if i.PPID == self {
			out = append(out, i)
		}
	}
	return out
}

// Reset discards the recorded history so a count can be attributed to one step.
func (s *SentinelDolt) Reset() {
	s.t.Helper()
	if err := os.Remove(s.logPath); err != nil && !os.IsNotExist(err) {
		s.t.Fatalf("reset sentinel dolt log: %v", err)
	}
}

// Describe renders the recorded execs for a failure message, so an assertion on
// ancestry says which processes produced it.
func (s *SentinelDolt) Describe() string {
	invocations := s.Invocations()
	if len(invocations) == 0 {
		return "(no dolt invocations recorded)"
	}
	lines := make([]string, 0, len(invocations))
	for _, i := range invocations {
		lines = append(lines, fmt.Sprintf("  ppid=%s parent=%q dolt %s", i.PPID, i.Parent, strings.Join(i.Argv, " ")))
	}
	return strings.Join(lines, "\n")
}

// CountWhere returns how many recorded execs satisfy match.
func (s *SentinelDolt) CountWhere(match func(SentinelDoltInvocation) bool) int {
	s.t.Helper()
	if match == nil {
		s.t.Fatal("CountWhere needs a predicate")
	}
	count := 0
	for _, i := range s.Invocations() {
		if match(i) {
			count++
		}
	}
	return count
}
