package sqlite

// The one quiescence gate. Every class component's mutation-free inspection —
// Sessions, Messaging, Nudges, Orders — runs this before it opens anything.
//
// It exists because four hand-written copies of it did not agree. Sessions
// once opened its census read-write; Messaging once opened non-immutable and
// answered from a live WAL; Nudges statted only the WAL and returned a
// confident schema fingerprint beside a 4,616-byte mid-transaction rollback
// journal. Five defects, four files, one rule — so the rule lives in one
// function and the per-class files carry only their sentinel.
//
// Unifying the rule did not by itself make the rule right. The sixth instance
// of the same defect was in this file: the gate named its sidecars off the
// spelling it was handed rather than the file that spelling resolves to, so a
// census pointed at a symlink answered a live-WAL source with a confident
// deployed-schema verdict on all four classes at once. Everything the gate
// looks at is therefore derived from ONE canonicalization, done here, before
// any name is composed.

import (
	"errors"
	"fmt"
	"os"
)

// ErrSQLiteSourceHardLinked reports a census pointed at a database file that
// carries more than one name on disk. It is not a quiescence verdict and
// deliberately does not wrap a class's live-WAL sentinel: a source whose
// sidecars may sit beside a name we cannot enumerate is not "busy, snapshot it
// under a fence" — the fenced route derives the same sidecar names from the
// same spelling and would answer just as confidently. There is no route that
// answers a hard-linked spelling correctly, so the census says so instead of
// suggesting one. It matches Graph, which has refused hard-linked SQLite
// components since it grew its own alias handling.
var ErrSQLiteSourceHardLinked = errors.New("hard-linked SQLite source")

// statSQLiteSource is a test seam for the two stats the gate uses to prove the
// spelling it was handed and the canonical spelling it derives sidecars from
// are one file. Production leaves it as os.Stat.
var statSQLiteSource = os.Stat

// A SQLite database is accompanied on disk by at most four files, all in the
// database's own directory (https://sqlite.org/tempfiles.html). Two of them can
// make the database file alone the wrong answer, and those are the two this
// gate refuses:
//
//   - "-wal" holds committed transactions that have not been checkpointed back
//     into the database. An immutable open cannot see them, so a census of the
//     database file alone silently reports a state that no reader ever had.
//
//   - "-journal" holds the pre-transaction images of pages an in-flight writer
//     has already modified in the database file. A hot journal therefore means
//     the database file is mid-transaction and is not a consistent state at
//     all; only rollback makes it one, and rollback is a write.
//
// The other two are deliberately NOT refused, and the reasoning is recorded
// here so the next reader does not have to re-derive it:
//
//   - "-shm" is the WAL index. SQLite documents it as containing "no persistent
//     content": it is rebuilt from the WAL during recovery and is absent
//     entirely under locking_mode=EXCLUSIVE. A leftover index beside an absent
//     or empty WAL proves only that a WAL-mode connection died; the database
//     file is still the whole truth, and refusing it would fail a census that
//     is perfectly answerable.
//
//   - The super-journal (formerly master journal) coordinates a commit spanning
//     several ATTACHed databases. It holds no page data — only the names of the
//     participating files — and every database it endangers necessarily carries
//     its own non-empty "-journal" for the whole window in which it is hot
//     (the journals are written first and deleted last). So the "-journal"
//     check already covers it, which is fortunate: SQLite documents the
//     super-journal's name as the database name plus "a randomized suffix" and
//     guarantees no matchable pattern.
//
// All four names are derived from the database file SQLite RESOLVED, not from
// the spelling anyone handed it. Measured: a writer opened through a symlink
// puts its "-wal" and "-shm" beside the link's target, and the link's own
// directory stays empty. So a gate that appends "-wal" to the spelling it was
// given looks in the wrong directory the moment a caller uses an alias, sees
// nothing, and reports a quiescent source with a live WAL sitting beside the
// real file. The gate therefore canonicalizes once, up front, and every sidecar
// name below is derived from that one canonical path.
//
// Sizes, not mere presence, are the test. journal_mode=PERSIST leaves a
// zeroed-header "-journal" behind and a truncating checkpoint leaves a
// zero-length "-wal"; both are quiescent. Every non-empty case is refused
// whether or not it is genuinely hot, because deciding that would mean parsing
// the sidecar, and this gate must not be the next thing that gets it wrong.
var sqliteAuthoritativeSidecars = []sqliteAuthoritativeSidecar{
	{suffix: "-wal", holds: "committed content that is still WAL-resident"},
	{suffix: "-journal", holds: "an unrecovered rollback journal over a mid-transaction database"},
}

// sqliteAuthoritativeSidecar is one sidecar whose bytes outrank the database
// file. holds completes "<file> holds N bytes of ..." in the refusal, so an
// operator reading the log knows which recovery the fenced snapshot has to
// perform.
type sqliteAuthoritativeSidecar struct {
	suffix string
	holds  string
}

// requireQuiescentSQLiteSource is the shared quiescence gate every class
// component's mutation-free inspection runs first. A source whose sidecars
// still hold authoritative bytes cannot be read immutably without reporting
// stale or torn content, and cannot be read read-write without recovering —
// that is, rewriting — the artifact under inspection. Incompleteness must never
// read as identity equality, so the inspection fails closed with the
// component's own live-WAL sentinel and the caller routes through a fenced
// private snapshot.
//
// It gates CENSUS entry points only. ADOPTION legitimately opens the same file
// read-write one step ahead of the class store's own open, and must keep
// adopting a crash-leftover deployment rather than refusing it.
//
// The gate is not a fence against a concurrent writer. It classifies what a
// crashed or shut-down writer left on disk; a WAL can still appear between the
// stat here and the caller's open, which is what the fenced snapshot path is
// for.
//
// component names the class in the error text; liveWAL is the per-class
// sentinel callers match with errors.Is.
func requireQuiescentSQLiteSource(path, component string, liveWAL error) error {
	source, info, err := canonicalSQLiteSource(path, component)
	if err != nil {
		return err
	}
	if platformFileHasMultipleLinks(info) {
		return fmt.Errorf("%w: %s is one of several names for the same file, so a writer's sidecars can sit beside a name this census cannot enumerate; point the census at the writer's own spelling or drop the alias",
			ErrSQLiteSourceHardLinked, source)
	}
	for _, sidecar := range sqliteAuthoritativeSidecars {
		name := source + sidecar.suffix
		sidecarInfo, err := statSQLiteSource(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspecting %s source sidecar %s: %w", component, name, err)
		}
		if sidecarInfo.Size() > 0 {
			return fmt.Errorf("%w: %s holds %d bytes of %s; inspect a fenced private snapshot instead",
				liveWAL, name, sidecarInfo.Size(), sidecar.holds)
		}
	}
	return nil
}

// canonicalSQLiteSource turns the spelling a census was handed into the one
// path every sidecar name is derived from, and proves the two denote the same
// file before anything is derived from either.
//
// canonicalPath is the package's existing resolver — absolute, symlink-free,
// missing trailing segments retained — so all three awkward shapes are handled
// deliberately rather than by luck. A path that does not exist resolves against
// its nearest existing ancestor and then fails the stat below with the caller's
// own spelling in the message, so "no component" still cannot read as "a
// component with nothing in it". A relative path is made absolute first, which
// bare filepath.EvalSymlinks does not do: it answers a relative path with a
// relative path, leaving sidecar names to be resolved against a working
// directory nobody here controls. A path whose PARENT is a symlink resolves
// through the parent, which matters less than it looks — the kernel would open
// the same file either way — but it keeps the refusal message pointing at the
// file an operator has to go recover.
//
// The identity check is what makes the sidecar derivation a fact rather than an
// assumption. This gate cannot hand the canonical path to the caller's open —
// the class entry points take a path and open that same path — so it instead
// proves that the spelling the caller will open and the spelling the sidecars
// came from resolve to one file at gate time. On unix that comparison is device
// plus inode; where no inode is available it falls back to mode, size and
// modification time, which a live writer can move between the two stats. That
// costs a refusal on a source no census could have answered anyway, which is
// the direction this gate is supposed to fail in.
func canonicalSQLiteSource(path, component string) (string, os.FileInfo, error) {
	spelled, err := statSQLiteSource(path)
	if err != nil {
		return "", nil, fmt.Errorf("inspecting %s source: %w", component, err)
	}
	source, err := canonicalPath(path)
	if err != nil {
		return "", nil, fmt.Errorf("inspecting %s source: canonicalizing %s: %w", component, path, err)
	}
	info, err := statSQLiteSource(source)
	if err != nil {
		return "", nil, fmt.Errorf("inspecting %s source: canonical spelling %s: %w", component, source, err)
	}
	if platformFileIdentity(spelled) != platformFileIdentity(info) {
		return "", nil, fmt.Errorf("inspecting %s source: %s and its canonical spelling %s did not resolve to one file; the source changed under the gate",
			component, path, source)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("inspecting %s source: %s is not a regular file (mode %s)", component, source, info.Mode())
	}
	return source, info, nil
}
