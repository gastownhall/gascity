package main

import (
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

// bdReadMemo shares identical read-only bd subprocess results across one
// `gc doctor` run.
//
// Doctor's store-backed checks each ask their own questions, and many of them
// ask the same ones: every per-template session lookup lists the whole
// gc:session class, v2-routed-to-namespace and hold-label-conventions list the
// same non-closed beads, and the session and migration checks re-list the same
// open beads. On a bd-backed scope each
// of those is two bd forks (the issues list plus the wisp query), and each bd
// fork costs two git forks of its own, so a healthy one-rig city paid hundreds
// of identical subprocesses per doctor run.
//
// The memo answers a repeated read with the bytes the first one returned, so
// every check parses exactly the output it would have parsed had it forked bd
// itself against the same state. It is deliberately narrow:
//
//   - only successful `bd list|query|ready|show` reads are kept; a failure is
//     never replayed, so retries and error reporting are unchanged;
//   - entries are per runner (that is, per opened store), because two runners
//     for the same directory may carry different environments;
//   - any other command through a memoized runner — a write, or anything this
//     file does not recognize as read-only — drops every entry, as does
//     invalidate(), which the doctor runner calls before each --fix
//     remediation and before the re-run that verifies it.
//
// Within one run the memo is a snapshot: once a question has been answered,
// every later check asking it gets that answer, even if the controller, an
// agent or another process has written to the ledger since. Only this
// process's own writes through a memoized runner, and invalidate(), refresh
// it. A doctor report has never been an atomic snapshot, and every answer
// here is still from within this run: each run starts with an empty memo.
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
// the city it inspects. Only doDoctor installs one; every other gc command
// opens stores with plain runners.
var activeBdReadMemos sync.Map // normalized city path -> *bdReadMemo

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
// doctor memo. It returns run unchanged when no doctor run is in progress or
// the run in progress inspects a different city.
func withBdReadMemo(cityPath string, run beads.CommandRunner) beads.CommandRunner {
	if run == nil {
		return run
	}
	m, ok := activeBdReadMemos.Load(normalizePathForCompare(cityPath))
	if !ok {
		return run
	}
	return m.(*bdReadMemo).wrap(run)
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
