package main

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/spf13/cobra"
)

// bdReadMemo shares identical read-only bd subprocess results across one
// `gc doctor` run, or across one invocation of a read-only hook verb (see
// invocationBdReadMemoCommands).
//
// Doctor's store-backed checks each ask their own questions, and many of them
// ask the same ones: every per-template session lookup lists the whole
// gc:session class, v2-routed-to-namespace and hold-label-conventions list the
// same non-closed beads, and the session and migration checks re-list the same
// open beads. On a bd-backed scope each
// of those is two bd forks (the issues list plus the wisp query), and each bd
// fork costs two git forks of its own, so a healthy one-rig city paid hundreds
// of identical subprocesses per doctor run. The hook verbs repeat themselves
// on a smaller scale: resolving a session by session_name and then by alias
// lists the same open wisps twice, the routed-work lookup for each of a
// session's identities lists them again, and a resolved session's bead is read
// once to resolve it and once more for its mailbox.
//
// The memo answers a repeated read with the bytes the first one returned, so
// every caller parses exactly the output it would have parsed had it forked bd
// itself against the same state. It is deliberately narrow:
//
//   - only successful `bd list|query|ready|show` reads are kept; a failure is
//     never replayed, so retries and error reporting are unchanged;
//   - entries are per runner (that is, per opened store), because two runners
//     for the same directory may carry different environments;
//   - any other command through a memoized runner — a write, or anything this
//     file does not recognize as read-only — drops every entry, whichever
//     store it went through, as does invalidate(), which the doctor runner
//     calls before each --fix remediation and before the re-run that verifies
//     it, and which every memoized store calls as a Tx begins.
//
// Within one run the memo is a snapshot: once a question has been answered,
// every later caller asking it gets that answer, even if the controller, an
// agent or another process has written to the ledger since. Only this
// process's own writes through a memoized runner, a Tx, and invalidate()
// refresh it. A doctor report has never been an atomic snapshot, and every
// answer here is still from within this run: each run starts with an empty
// memo. That is also why a verb that waits on the ledger to change — a claim
// loop backing off and re-reading — must never run under one.
type bdReadMemo struct {
	// cityPath is the city this doctor run inspects (normalized); stores
	// opened for any other city get plain runners.
	cityPath string

	mu      sync.Mutex
	gen     uint64
	entries map[bdReadMemoKey]*bdReadMemoEntry
	nextID  uint64
}

// bdReadMemoEntry is one read, in flight until done is closed. out is set
// (and the entry kept) only when the read succeeded.
type bdReadMemoEntry struct {
	done chan struct{}
	out  []byte
	ok   bool
}

type bdReadMemoKey struct {
	runner uint64
	dir    string
	name   string
	args   string
}

// activeBdReadMemos holds the memo of each doctor run in progress, keyed by
// the city it inspects. Only doDoctor installs one.
var activeBdReadMemos sync.Map // normalized city path -> *bdReadMemo

// invocationBdReadMemo is the memo of the one-shot invocation in progress, for
// stores of any city it opens. run() installs it for the verbs in
// invocationBdReadMemoCommands; every other gc command, and every long-lived
// one, opens stores with plain runners.
var invocationBdReadMemo atomic.Pointer[bdReadMemo]

// invocationBdReadMemoCommands are the command paths run() installs the
// invocation memo for. Each is a session-side hook or read that resolves its
// session, reads its ledger state once and exits; none waits for the ledger to
// change, which a memoized read would never show it. `gc hook` is not one:
// its claim loop backs off and re-reads while racing other claimers.
var invocationBdReadMemoCommands = map[string]bool{
	"gc mail check":  true,
	"gc mail inbox":  true,
	"gc nudge drain": true,
}

// invocationBdReadMemoEligible reports whether run() installs the invocation
// memo for cmd.
func invocationBdReadMemoEligible(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	return invocationBdReadMemoCommands[cmd.CommandPath()]
}

// installInvocationBdReadMemo makes a fresh memo the one every bd store opened
// from now on uses, unless a doctor run has one for that store's city, and
// returns a function that removes it again.
func installInvocationBdReadMemo() (restore func()) {
	m := newBdReadMemo("")
	invocationBdReadMemo.Store(m)
	return func() { invocationBdReadMemo.CompareAndSwap(m, nil) }
}

// activeBdReadMemo returns the memo a bd store opened now for cityPath reads
// through — the doctor run's for that city, else the invocation's — or nil.
func activeBdReadMemo(cityPath string) *bdReadMemo {
	if m, ok := activeBdReadMemos.Load(normalizePathForCompare(cityPath)); ok {
		return m.(*bdReadMemo)
	}
	return invocationBdReadMemo.Load()
}

// newBdStoreWithReadMemo opens a BdStore over run for a scope of cityPath. While
// a memo is active its reads go through the memo, and each Tx it begins drops
// the memo first.
func newBdStoreWithReadMemo(dir, cityPath string, run beads.CommandRunner, prefix string, cfg *config.City) *beads.BdStore {
	opts := bdStoreOptionsForConfig(cfg)
	if m := activeBdReadMemo(cityPath); m != nil && run != nil {
		run = m.wrap(run)
		opts = append(opts, beads.WithBdStoreBeforeTx(m.invalidate))
	}
	return beads.NewBdStoreWithPrefix(dir, run, prefix, opts...)
}

func newBdReadMemo(cityPath string) *bdReadMemo {
	return &bdReadMemo{cityPath: normalizePathForCompare(cityPath), entries: map[bdReadMemoKey]*bdReadMemoEntry{}}
}

// installBdReadMemo makes m the memo every bd store opened for m's city from
// now on uses, and returns a function that removes it again.
func installBdReadMemo(m *bdReadMemo) (restore func()) {
	activeBdReadMemos.Store(m.cityPath, m)
	return func() { activeBdReadMemos.CompareAndDelete(m.cityPath, m) }
}

// installDoctorBdReadMemo makes the bd stores opened for cityPath during d's
// run share identical reads (see bdReadMemo), and has d drop them before each
// --fix remediation and before the re-run that verifies it. A fix may write
// the ledger without going through those stores (a pack script does), so
// without that hook the verification would be answered from reads taken
// before the fix. It returns a function that removes the memo again.
func installDoctorBdReadMemo(d *doctor.Doctor, cityPath string) (restore func()) {
	memo := newBdReadMemo(cityPath)
	d.BeforeFix = memo.invalidate
	return installBdReadMemo(memo)
}

// invalidate drops every memoized read.
func (m *bdReadMemo) invalidate() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.gen++
	clear(m.entries)
	m.mu.Unlock()
}

// withBdReadMemo wraps run, a runner for a store of cityPath, with the active
// memo (see activeBdReadMemo). It returns run unchanged when none is active.
func withBdReadMemo(cityPath string, run beads.CommandRunner) beads.CommandRunner {
	m := activeBdReadMemo(cityPath)
	if run == nil || m == nil {
		return run
	}
	return m.wrap(run)
}

func (m *bdReadMemo) wrap(run beads.CommandRunner) beads.CommandRunner {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.mu.Unlock()
	return func(dir, name string, args ...string) ([]byte, error) {
		switch bdReadMemoClassify(name, args) {
		case bdMemoCacheable:
			return m.read(bdReadMemoKey{runner: id, dir: dir, name: name, args: strings.Join(args, "\x00")}, func() ([]byte, error) {
				return run(dir, name, args...)
			})
		case bdMemoPassThrough:
			return run(dir, name, args...)
		default:
			m.invalidate()
			out, err := run(dir, name, args...)
			m.invalidate()
			return out, err
		}
	}
}

// read answers key from the memo, joining an identical read already in
// flight, or runs it. Concurrent checks (and a check's own concurrent reads)
// asking the same question therefore fork bd once. A failed read is never
// shared: each caller that waited on it runs its own, exactly as it would have
// without the memo.
func (m *bdReadMemo) read(key bdReadMemoKey, run func() ([]byte, error)) ([]byte, error) {
	m.mu.Lock()
	if entry, ok := m.entries[key]; ok {
		m.mu.Unlock()
		<-entry.done
		if entry.ok {
			return append([]byte(nil), entry.out...), nil
		}
		return run()
	}
	entry := &bdReadMemoEntry{done: make(chan struct{})}
	m.entries[key] = entry
	gen := m.gen
	m.mu.Unlock()

	out, err := run()
	m.mu.Lock()
	switch {
	case err != nil:
		if m.entries[key] == entry {
			delete(m.entries, key)
		}
	case m.gen != gen:
		// A write (or a fix) landed while this read was in flight; its
		// answer may predate the write, so it is not kept for later reads.
	default:
		entry.out = append([]byte(nil), out...)
		entry.ok = true
	}
	m.mu.Unlock()
	close(entry.done)
	return out, err
}

type bdMemoClass int

const (
	bdMemoInvalidating bdMemoClass = iota
	bdMemoCacheable
	bdMemoPassThrough
)

// bdReadMemoClassify sorts one runner call. Only the bd read verbs the store
// layer issues are cached; a few other read-only probes pass through without
// disturbing the memo; everything else is treated as a possible write.
func bdReadMemoClassify(name string, args []string) bdMemoClass {
	if name != "bd" {
		return bdMemoInvalidating
	}
	// Mode flags that only narrow what bd may do come before the verb.
	// Both are bd global flags (internal/bdflags registers the full set);
	// a list, query, ready or show led by any other flag is not cached.
	for len(args) > 0 && (args[0] == "--readonly" || args[0] == "--sandbox") {
		args = args[1:]
	}
	if len(args) == 0 {
		return bdMemoInvalidating
	}
	switch args[0] {
	case "list", "query", "ready", "show":
		return bdMemoCacheable
	case "version", "--version", "ping", "types", "where":
		return bdMemoPassThrough
	case "config":
		if len(args) > 1 && (args[1] == "get" || args[1] == "list") {
			return bdMemoPassThrough
		}
	}
	return bdMemoInvalidating
}
