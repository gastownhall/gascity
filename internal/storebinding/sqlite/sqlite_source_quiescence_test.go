package sqlite

// One quiescence contract, five source states, three spellings, one table.
//
// The same defect has been found six times. Four of them were in the
// hand-written per-class gates this package used to carry: Sessions opened its
// census read-write, Messaging opened non-immutable and answered from a live
// WAL, Nudges statted only the WAL and returned a confident schema fingerprint
// beside a hot rollback journal. Those four classes and their gates are retired
// removed, and the sixth occurrence was
// in the unified gate that replaced them: it derived its sidecar names from the
// spelling it was handed instead of the file SQLite resolves.
//
// That unified gate survives, because the Beads provider's read-only source
// open runs it — one scope now, but the same rule and the same failure modes.
// Each occurrence above was originally caught by a test that existed for
// exactly one class, or for exactly one spelling, so this table still runs the
// gate against the SAME fixtures under EVERY spelling of the same file.
//
// Adoption is deliberately absent from the refusal table: an active open lands
// one step ahead of a writer and must keep adopting a crash-leftover
// deployment rather than refusing it. That is pinned by
// TestActiveBeadsOpenStillAcceptsANonQuiescentSource below.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// sourceCensus is a mutation-free inspection entry point together with the two
// things a shared fixture needs: how to produce a quiescent deployed component
// it can read, and which sentinel its refusal must carry.
type sourceCensus struct {
	scope    string
	sentinel error
	deployed func(t *testing.T, dir string) string
	inspect  func(t *testing.T, path string) error
	// ungated is the same inspection with the quiescence gate skipped: the
	// shape every census entry point in this package had before the gate, and
	// the shape four of them regressed to at least once. It is here so the
	// table can prove the gate is what stands between the census and a
	// confident wrong answer, rather than asserting a refusal that some other
	// check produced.
	ungated func(t *testing.T, path string) error
}

// sourceCensuses lists every gated mutation-free inspection entry point in this
// package. Adding a component whose census reads a SQLite file without adding
// it here leaves its gate untested; adding a second entry point to an existing
// scope means adding a row, not a copy of these fixtures.
func sourceCensuses() []sourceCensus {
	return []sourceCensus{
		{
			// The gate exactly as the Beads provider's read-only source open
			// calls it (beads_provider.go). That the provider reaches it at all
			// is pinned end to end by
			// TestBeadsProviderCompletesInspectionOnlyUnderAFence; what this
			// table pins is the verdict it reaches on every source shape.
			scope:    "Beads",
			sentinel: ErrBeadsLiveWAL,
			deployed: deployedBeadsComponent,
			inspect: func(t *testing.T, path string) error {
				t.Helper()
				return requireQuiescentSQLiteSource(path, "Beads", ErrBeadsLiveWAL)
			},
			ungated: func(t *testing.T, path string) error {
				t.Helper()
				return readSQLiteSchemaImmutably(t, path)
			},
		},
	}
}

// deployedBeadsComponent creates and cleanly closes the deployed Beads database
// below dir, leaving a quiescent source a mutation-free census can complete on.
func deployedBeadsComponent(t *testing.T, dir string) string {
	t.Helper()
	store, err := beads.OpenSQLiteStore(dir, beads.WithSQLiteStoreIDPrefix(graphIDPrefix))
	if err != nil {
		t.Fatalf("opening the deployed Beads component: %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "deployed row", Type: "task"}); err != nil {
		t.Fatalf("seeding the deployed Beads component: %v", err)
	}
	closer, ok := store.(interface{ CloseStore() error })
	if !ok {
		t.Fatalf("seeded store %T cannot release its physical handle", store)
	}
	if err := closer.CloseStore(); err != nil {
		t.Fatalf("closing the deployed Beads component: %v", err)
	}
	return filepath.Join(dir, graphFilename)
}

// readSQLiteSchemaImmutably is the ungated shape: a plain read-only immutable
// open that reports the schema it finds. It is what every census entry point in
// this package looked like before the gate, and it answers a torn or stale
// source just as confidently as a quiescent one — which is the whole point of
// running it beside the gated call.
func readSQLiteSchemaImmutably(t *testing.T, path string) error {
	t.Helper()
	db, err := sql.Open("sqlite", graphReadOnlyDSN(path, true))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	rows, queryErr := db.QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if queryErr == nil {
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				queryErr = err
				break
			}
		}
		queryErr = errors.Join(queryErr, rows.Err(), rows.Close())
	}
	return errors.Join(queryErr, db.Close())
}

// sourceState is one shape a deployed component can be found in on disk.
type sourceState struct {
	state string
	// derive turns a quiescent deployed component into this state and returns
	// the path the census is pointed at.
	derive func(t *testing.T, deployed string) string
	// refused is what every census must do with it. Nothing here is
	// scope-specific: a rule that holds for one census entry point and not for
	// the next is the bug this table exists to catch.
	refused bool
}

func sourceStates() []sourceState {
	return []sourceState{
		{
			state:   "quiescent",
			derive:  func(_ *testing.T, deployed string) string { return deployed },
			refused: false,
		},
		{
			// A truncating checkpoint leaves the WAL in place at zero length.
			// Nothing is unmerged, so refusing it would fail a census that is
			// perfectly answerable.
			state: "checkpointed empty WAL",
			derive: func(t *testing.T, deployed string) string {
				t.Helper()
				path := copySQLiteComponent(t, deployed)
				writeSidecar(t, path+"-wal", nil)
				return path
			},
			refused: false,
		},
		{
			// The WAL index carries no persistent content and is rebuilt from
			// the WAL during recovery, so a leftover index beside no WAL proves
			// only that a WAL-mode connection died. The database file is still
			// the whole truth.
			state: "leftover WAL index",
			derive: func(t *testing.T, deployed string) string {
				t.Helper()
				path := copySQLiteComponent(t, deployed)
				writeSidecar(t, path+"-shm", make([]byte, 32*1024))
				return path
			},
			refused: false,
		},
		{
			// A writer committed and died before any checkpoint. An immutable
			// open cannot see the committed rows.
			state:   "live WAL",
			derive:  liveWALSource,
			refused: true,
		},
		{
			// A writer died mid-transaction. The database file holds pages the
			// journal has to undo, so it is not a consistent state at all — and
			// an immutable open reads it anyway and reports a confident
			// fingerprint. This is the shape that made it past the Nudges gate.
			state:   "hot rollback journal",
			derive:  hotJournalSource,
			refused: true,
		},
	}
}

// sourceSpelling is one name a census can be handed for the SAME file on disk.
// SQLite names its sidecars after the database file it RESOLVED, so a gate that
// appends "-wal" to whatever spelling it was given looks in the wrong directory
// the moment a caller uses an alias. Every state below is therefore run under
// every spelling.
type sourceSpelling struct {
	name string
	// unavailable returns why this platform cannot exercise the spelling, or
	// "" when it can. A platform that cannot is a stated hole rather than a
	// silently passing row.
	unavailable func(t *testing.T) string
	// alias returns a second name for source. It must never copy: both names
	// have to reach the same inode or the fixture proves nothing.
	alias func(t *testing.T, source string) string
	// want is the error the census must fail with for this spelling over this
	// state, or nil when it must answer.
	want func(census sourceCensus, source sourceState) error
}

// quiescenceVerdict is the verdict for a spelling that names the file the way
// the writer did: exactly the state's own contract.
func quiescenceVerdict(census sourceCensus, source sourceState) error {
	if source.refused {
		return census.sentinel
	}
	return nil
}

func sourceSpellings() []sourceSpelling {
	return []sourceSpelling{
		{
			name:  "the writer's own spelling",
			alias: func(_ *testing.T, source string) string { return source },
			want:  quiescenceVerdict,
		},
		{
			// The occurrence-six shape, measured: a census pointed at a symlink
			// to a live-WAL source answered with a confident deployed-schema
			// verdict, because the gate saw no sidecar beside the link while the
			// real non-empty -wal sat beside the target. Resolving the spelling
			// is the whole fix, so an alias must not change any verdict here:
			// the refusals stay refusals and the answerable states stay
			// answerable.
			name:        "a symlink in another directory",
			unavailable: symlinksUnavailable,
			alias: func(t *testing.T, source string) string {
				t.Helper()
				alias := filepath.Join(t.TempDir(), "alias.db")
				if err := os.Symlink(source, alias); err != nil {
					t.Fatalf("creating a symlinked spelling: %v", err)
				}
				return alias
			},
			want: quiescenceVerdict,
		},
		{
			// A hard link is the spelling canonicalization cannot fix:
			// EvalSymlinks does not resolve one, and the alias sits in its own
			// directory while the writer's sidecars sit beside the writer's
			// name. Through this spelling all five states look identical — and
			// two of them are wrong answers — so the gate refuses the spelling
			// wholesale rather than answering the three it would happen to get
			// right. That refusal is deliberately NOT the live-WAL sentinel: the
			// fenced snapshot route derives the same sidecar names from the same
			// spelling and would be just as blind, so there is no route to
			// suggest.
			name:        "a hard link in another directory",
			unavailable: hardLinkAliasesUnavailable,
			alias: func(t *testing.T, source string) string {
				t.Helper()
				alias := filepath.Join(t.TempDir(), "alias.db")
				if err := os.Link(source, alias); err != nil {
					t.Fatalf("creating a hard-linked spelling: %v", err)
				}
				return alias
			},
			want: func(sourceCensus, sourceState) error { return ErrSQLiteSourceHardLinked },
		},
	}
}

// TestSourceCensusesShareOneQuiescenceContract is the cross-class rail: every
// class's census entry point must reach the same verdict on the same source
// state under every spelling of it, and must leave the source byte-identical
// whichever verdict it reaches.
func TestSourceCensusesShareOneQuiescenceContract(t *testing.T) {
	for _, census := range sourceCensuses() {
		t.Run(census.scope, func(t *testing.T) {
			for _, source := range sourceStates() {
				t.Run(source.state, func(t *testing.T) {
					for _, spelling := range sourceSpellings() {
						t.Run(spelling.name, func(t *testing.T) {
							skipUnavailableSpelling(t, spelling)
							deployed := source.derive(t, census.deployed(t, t.TempDir()))
							path := spelling.alias(t, deployed)
							requireOneFile(t, path, deployed)
							before := sqliteSourceImage(t, deployed)
							beside := directoryListing(t, filepath.Dir(path))

							err := census.inspect(t, path)
							want := spelling.want(census, source)
							switch {
							case want != nil && !errors.Is(err, want):
								t.Fatalf("census of a %s source = %v, want %v", source.state, err, want)
							case want == nil && err != nil:
								t.Fatalf("census of a %s source = %v, want it answered", source.state, err)
							}

							if diffs := sqliteSourceImageDiff(before, sqliteSourceImage(t, deployed)); len(diffs) != 0 {
								t.Fatalf("the census rewrote the source it was reporting on:\n%s", strings.Join(diffs, "\n"))
							}
							// A non-immutable open through an alias would drop
							// fresh sidecars somewhere; this catches them
							// wherever the spelling put them.
							if after := directoryListing(t, filepath.Dir(path)); after != beside {
								t.Fatalf("the census left files beside the spelling it was handed: before [%s], after [%s]", beside, after)
							}
						})
					}
				})
			}
		})
	}
}

// TestNonQuiescentSourcesWouldFoolAnUngatedCensus is what keeps the table
// above from passing vacuously. For every cell the table expects a refusal in,
// it proves the inspection WITHOUT the gate answers happily — so the gate
// really is the only thing between the census and a confident report about
// stale or torn bytes, rather than a refusal some other check produced.
//
// Two of those cells carry extra weight. Through an alias, the ungated
// inspection answers a live-WAL source exactly as it answers a quiescent one:
// that is occurrence six, measured. And a gate statting only the WAL sees
// nothing at all in the hot-journal case, which is the shape that got past
// Nudges — pinned separately by TestHotJournalSourceCarriesNoWAL.
//
// The quiescent hard-linked cells are the honest exception: there the ungated
// answer is right, and the gate refuses anyway because through that spelling it
// cannot tell those cells from the two it would get wrong.
func TestNonQuiescentSourcesWouldFoolAnUngatedCensus(t *testing.T) {
	for _, census := range sourceCensuses() {
		t.Run(census.scope, func(t *testing.T) {
			for _, source := range sourceStates() {
				t.Run(source.state, func(t *testing.T) {
					for _, spelling := range sourceSpellings() {
						want := spelling.want(census, source)
						if want == nil {
							continue
						}
						t.Run(spelling.name, func(t *testing.T) {
							skipUnavailableSpelling(t, spelling)
							path := spelling.alias(t, source.derive(t, census.deployed(t, t.TempDir())))
							if err := census.ungated(t, path); err != nil {
								t.Fatalf("the ungated inspection refused a %s source on its own (%v); this fixture proves nothing about the gate", source.state, err)
							}
							if err := census.inspect(t, path); !errors.Is(err, want) {
								t.Fatalf("the gated census of a %s source = %v, want %v", source.state, err, want)
							}
						})
					}
				})
			}
		})
	}
}

// TestQuiescenceGateResolvesAwkwardPathShapes pins the three path shapes the
// canonicalization has to handle deliberately rather than by luck, because
// filepath.EvalSymlinks answers each of them differently: it fails outright on
// a path that does not exist, it answers a relative path with a relative path,
// and it resolves a symlinked parent silently.
//
// One class is enough here, and only here: this is the shared gate's own
// resolution, reached before any class code runs. What has to be proven per
// class is that every class still routes through the gate at all, and the
// spelling rows of the cross-class table above are what prove that.
func TestQuiescenceGateResolvesAwkwardPathShapes(t *testing.T) {
	census := censusFor(t, "Beads")

	t.Run("a leaf that does not exist stays a missing source", func(t *testing.T) {
		err := census.inspect(t, filepath.Join(t.TempDir(), "absent.db"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("census of an absent source = %v, want it to report the missing file", err)
		}
	})

	t.Run("a missing leaf under a symlinked parent stays a missing source", func(t *testing.T) {
		skipWithoutSymlinks(t)
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("creating a symlinked parent: %v", err)
		}
		err := census.inspect(t, filepath.Join(link, "absent.db"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("census of an absent source under a symlinked parent = %v, want it to report the missing file", err)
		}
	})

	t.Run("a symlinked parent resolves to the writer's sidecars", func(t *testing.T) {
		skipWithoutSymlinks(t)
		deployed := liveWALSource(t, census.deployed(t, t.TempDir()))
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(filepath.Dir(deployed), link); err != nil {
			t.Fatalf("creating a symlinked parent: %v", err)
		}
		if err := census.inspect(t, filepath.Join(link, filepath.Base(deployed))); !errors.Is(err, census.sentinel) {
			t.Fatalf("census through a symlinked parent = %v, want %v", err, census.sentinel)
		}
	})

	t.Run("a relative spelling is refused with an absolute path to recover", func(t *testing.T) {
		deployed := liveWALSource(t, census.deployed(t, t.TempDir()))
		// The temp root itself can be a symlink (/var on macOS), so the name the
		// refusal must carry is the resolved one, not the one the fixture built.
		resolved, err := canonicalPath(deployed)
		if err != nil {
			t.Fatalf("resolving the fixture path: %v", err)
		}
		t.Chdir(filepath.Dir(filepath.Dir(deployed)))
		relative := filepath.Join(filepath.Base(filepath.Dir(deployed)), filepath.Base(deployed))

		err = census.inspect(t, relative)
		if !errors.Is(err, census.sentinel) {
			t.Fatalf("census of a relative spelling = %v, want %v", err, census.sentinel)
		}
		// EvalSymlinks on its own answers a relative path with a relative path,
		// which would leave the operator a name that only means anything from
		// the working directory the census happened to run in.
		if !strings.Contains(err.Error(), resolved+"-wal") {
			t.Fatalf("refusal of a relative spelling = %q, want it to name %q", err, resolved+"-wal")
		}
	})
}

// TestQuiescenceGateRefusesASourceThatMovedUnderIt covers the branch no fixture
// can reach by racing: the gate derives its sidecar names from the canonical
// path, but the caller opens the spelling it was handed, so the two have to be
// proven to be one file. The seam stands in for a spelling repointed between
// the two resolutions.
func TestQuiescenceGateRefusesASourceThatMovedUnderIt(t *testing.T) {
	skipWithoutSymlinks(t)
	census := censusFor(t, "Beads")
	deployed := census.deployed(t, t.TempDir())
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(deployed, alias); err != nil {
		t.Fatalf("creating a symlinked spelling: %v", err)
	}
	decoy := filepath.Join(t.TempDir(), "decoy.db")
	if err := os.WriteFile(decoy, []byte("not the source"), 0o600); err != nil {
		t.Fatalf("writing the decoy: %v", err)
	}
	// The seam has to fire on the name the gate derives, which is the resolved
	// one — the temp root itself can be a symlink.
	resolved, err := canonicalPath(deployed)
	if err != nil {
		t.Fatalf("resolving the fixture path: %v", err)
	}

	original := statSQLiteSource
	t.Cleanup(func() { statSQLiteSource = original })
	statSQLiteSource = func(name string) (os.FileInfo, error) {
		if name == resolved {
			return original(decoy)
		}
		return original(name)
	}

	err = census.inspect(t, alias)
	if err == nil {
		t.Fatal("the census answered for a spelling that no longer named the file its sidecars came from")
	}
	if errors.Is(err, census.sentinel) {
		t.Fatalf("a source that moved under the gate was reported as a quiescence failure: %v", err)
	}
	if !strings.Contains(err.Error(), "one file") {
		t.Fatalf("refusal = %q, want it to say the two spellings are not one file", err)
	}
}

// TestQuiescenceGateRefusesANonDatabaseSource keeps the sidecar reasoning
// honest: "at most four files in the database's own directory" is a statement
// about a regular file, and a directory carries a link count of its own that
// would otherwise read as a hard-linked alias.
func TestQuiescenceGateRefusesANonDatabaseSource(t *testing.T) {
	for _, census := range sourceCensuses() {
		t.Run(census.scope, func(t *testing.T) {
			err := census.inspect(t, t.TempDir())
			if err == nil {
				t.Fatal("the census answered for a directory")
			}
			if errors.Is(err, ErrSQLiteSourceHardLinked) {
				t.Fatalf("a directory was refused as a hard-linked source: %v", err)
			}
			if !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("refusal = %q, want it to name the source as not a regular file", err)
			}
		})
	}
}

// requireOneFile keeps an alias fixture from quietly degrading into a copy. A
// spelling that named a second file would test nothing: the whole question is
// what a gate does when two names reach one set of bytes and only one of them
// has the sidecars beside it.
func requireOneFile(t *testing.T, spelling, source string) {
	t.Helper()
	spelled, err := os.Stat(spelling)
	if err != nil {
		t.Fatalf("stating the aliased spelling: %v", err)
	}
	deployed, err := os.Stat(source)
	if err != nil {
		t.Fatalf("stating the deployed source: %v", err)
	}
	if platformFileIdentity(spelled) != platformFileIdentity(deployed) {
		t.Fatalf("the fixture spelling %s is a different file from %s; it proves nothing about aliases", spelling, source)
	}
}

// skipUnavailableSpelling states the hole rather than passing the row.
func skipUnavailableSpelling(t *testing.T, spelling sourceSpelling) {
	t.Helper()
	if spelling.unavailable == nil {
		return
	}
	if reason := spelling.unavailable(t); reason != "" {
		t.Skipf("%s is unexercised here: %s", spelling.name, reason)
	}
}

func skipWithoutSymlinks(t *testing.T) {
	t.Helper()
	if reason := symlinksUnavailable(t); reason != "" {
		t.Skip(reason)
	}
}

// symlinksUnavailable reports whether this platform lets an unprivileged
// process create a symlink at all (Windows before developer mode does not).
func symlinksUnavailable(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Symlink(filepath.Join(directory, "target"), filepath.Join(directory, "link")); err != nil {
		return fmt.Sprintf("this platform cannot create symlinks: %v", err)
	}
	return ""
}

// hardLinkAliasesUnavailable reports whether this platform can both create a
// hard link and let the gate SEE the resulting link count. Both halves matter:
// a platform that creates hard links without reporting a link count cannot be
// defended by this gate at all, which is a hole worth naming out loud rather
// than skipping quietly.
func hardLinkAliasesUnavailable(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatalf("writing the hard-link probe source: %v", err)
	}
	if err := os.Link(source, filepath.Join(directory, "alias")); err != nil {
		return fmt.Sprintf("this platform cannot create hard links: %v", err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatalf("stating the hard-link probe source: %v", err)
	}
	if !platformFileHasMultipleLinks(info) {
		return "this platform creates hard links but reports no link count, so an aliased spelling is undetectable here"
	}
	return ""
}

// directoryListing names everything beside a spelling, so a census that created
// a sidecar under ANY name — not just the four this package knows — is caught.
func directoryListing(t *testing.T, directory string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("listing %s: %v", directory, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// TestHotJournalSourceCarriesNoWAL pins why a WAL-only gate fails open on it:
// there is no WAL to stat. The refusal has to come from the journal.
func TestHotJournalSourceCarriesNoWAL(t *testing.T) {
	for _, census := range sourceCensuses() {
		t.Run(census.scope, func(t *testing.T) {
			path := hotJournalSource(t, census.deployed(t, t.TempDir()))
			info, err := os.Stat(path + "-wal")
			if err == nil && info.Size() > 0 {
				t.Fatalf("the hot-journal fixture also carries a %d-byte WAL; it no longer isolates the journal case", info.Size())
			}
		})
	}
}

// TestSourceCensusesRejectAMissingSource keeps "no component" from reading as
// "a component with nothing in it" on every class at once.
func TestSourceCensusesRejectAMissingSource(t *testing.T) {
	for _, census := range sourceCensuses() {
		t.Run(census.scope, func(t *testing.T) {
			if err := census.inspect(t, filepath.Join(t.TempDir(), "absent.db")); err == nil {
				t.Fatal("the census answered for a file that does not exist")
			}
		})
	}
}

// censusFor returns the named class's census row, so the adoption table below
// reuses the same deployed-fixture builders without indexing into a slice.
func censusFor(t *testing.T, scope string) sourceCensus {
	t.Helper()
	for _, census := range sourceCensuses() {
		if census.scope == scope {
			return census
		}
	}
	t.Fatalf("no census entry point registered for scope %q", scope)
	return sourceCensus{}
}

// TestActiveBeadsOpenStillAcceptsANonQuiescentSource is the other half of the
// contract, and the reason the gate is not simply applied everywhere: an active
// open lands one step ahead of a writer and recovers the file it opens, so it
// must keep opening a city whose writer died mid-flight. A refusal here would
// leave an operator unable to start a crashed city at all.
//
// The read-only source open is the asymmetric one: it can neither recover a
// hot journal nor checkpoint a WAL, so it is gated and the caller is routed to
// a fenced private snapshot instead. Both halves therefore run against the same
// crash-leftover fixture, one line apart, so neither can be changed on the
// assumption that the other agrees with it.
func TestActiveBeadsOpenStillAcceptsANonQuiescentSource(t *testing.T) {
	census := censusFor(t, "Beads")
	path := liveWALSource(t, census.deployed(t, t.TempDir()))

	store, err := beads.OpenSQLiteStore(filepath.Dir(path), beads.WithSQLiteStoreIDPrefix(graphIDPrefix))
	if err != nil {
		t.Fatalf("adopting a crash-leftover Beads component: %v", err)
	}
	if err := store.(interface{ CloseStore() error }).CloseStore(); err != nil {
		t.Fatalf("closing the adopted component: %v", err)
	}

	// The same source, through the census the read-only open runs first. The
	// active open above recovered the WAL, so the census is re-pointed at a
	// fresh copy of the crash leftover rather than at the recovered file.
	if err := census.inspect(t, liveWALSource(t, census.deployed(t, t.TempDir()))); !errors.Is(err, census.sentinel) {
		t.Fatalf("the census of a crash-leftover source = %v, want %v", err, census.sentinel)
	}
}

// liveWALSource copies a component out from under a writer that has committed
// into the WAL without checkpointing — byte-for-byte what a crashed city leaves
// behind. The write is a no-op user_version stamp, which dirties page 1 and
// nothing else, so the schema an ungated census would report is still exactly
// the deployed one: a refusal can only have come from the quiescence gate.
func liveWALSource(t *testing.T, deployed string) string {
	t.Helper()
	db := openRawSQLite(t, deployed)
	defer closeRawSQLite(t, db)

	if mode := sqlitePragmaMode(t, db, "journal_mode = WAL"); mode != "wal" {
		t.Fatalf("fixture journal_mode = %q, want wal", mode)
	}
	stampSQLitePageOne(t, db)

	path := copySQLiteComponent(t, deployed, "-wal")
	requireSidecarContent(t, path+"-wal")
	return path
}

// hotJournalSource copies a component out from under a writer killed
// mid-transaction: the rollback journal beside it holds the pre-transaction
// image of a page the database file may already carry the new version of. Only
// rollback resolves that, and rollback is a write — so the census must refuse
// rather than read the database file as if it were consistent.
func hotJournalSource(t *testing.T, deployed string) string {
	t.Helper()
	db := openRawSQLite(t, deployed)
	defer closeRawSQLite(t, db)

	if mode := sqlitePragmaMode(t, db, "journal_mode = DELETE"); mode != "delete" {
		t.Fatalf("fixture journal_mode = %q, want delete", mode)
	}
	transaction, err := db.Begin()
	if err != nil {
		t.Fatalf("opening the mid-transaction fixture: %v", err)
	}
	var version int
	if err := transaction.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("reading user_version inside the transaction: %v", err)
	}
	if _, err := transaction.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		t.Fatalf("dirtying page 1 inside the transaction: %v", err)
	}

	path := copySQLiteComponent(t, deployed, "-journal")
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("rolling back the fixture transaction: %v", err)
	}
	requireSidecarContent(t, path+"-journal")
	return path
}

// openRawSQLite opens a component through the bare driver, bypassing every
// class store, so a fixture can put a real writer's sidecars on disk without a
// class-specific write path.
func openRawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	// The journal-mode change and the transaction that follows it must land on
	// one connection.
	db.SetMaxOpenConns(1)
	return db
}

func closeRawSQLite(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Errorf("closing the fixture database: %v", err)
	}
}

// sqlitePragmaMode runs a journal-mode pragma, which answers with the mode it
// settled on rather than with nothing.
func sqlitePragmaMode(t *testing.T, db *sql.DB, pragma string) string {
	t.Helper()
	var mode string
	if err := db.QueryRow(`PRAGMA ` + pragma).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA %s: %v", pragma, err)
	}
	return strings.ToLower(mode)
}

// stampSQLitePageOne rewrites user_version with the value it already has: a
// real page-1 write that changes no schema and no content.
func stampSQLitePageOne(t *testing.T, db *sql.DB) {
	t.Helper()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		t.Fatalf("stamping user_version: %v", err)
	}
}

// copySQLiteComponent copies a component and the named sidecars into a fresh
// directory. Copying a file set out from under a live writer is exactly what a
// crash leaves on disk, and it gives the fixture a source no live handle is
// still attached to.
func copySQLiteComponent(t *testing.T, source string, sidecars ...string) string {
	t.Helper()
	destination := filepath.Join(t.TempDir(), filepath.Base(source))
	copyFile(t, source, destination)
	for _, suffix := range sidecars {
		copyFile(t, source+suffix, destination+suffix)
	}
	return destination
}

func writeSidecar(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func requireSidecarContent(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the fixture produced no %s: %v", filepath.Base(path), err)
	}
	if info.Size() == 0 {
		t.Fatalf("the fixture's %s is empty; it proves nothing about a non-quiescent source", filepath.Base(path))
	}
}

// sqliteSourceImage captures exactly what a mutation-free inspection must leave
// alone: the database bytes plus the presence and content of every sidecar
// SQLite co-locates with a database file.
func sqliteSourceImage(t *testing.T, path string) map[string]string {
	t.Helper()
	image := make(map[string]string, 4)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		name := filepath.Base(path) + suffix
		data, err := os.ReadFile(path + suffix) //nolint:gosec // test-controlled path
		if errors.Is(err, os.ErrNotExist) {
			image[name] = "absent"
			continue
		}
		if err != nil {
			t.Fatalf("reading %s: %v", path+suffix, err)
		}
		sum := sha256.Sum256(data)
		image[name] = fmt.Sprintf("%d bytes sha=%s", len(data), hex.EncodeToString(sum[:]))
	}
	return image
}

func sqliteSourceImageDiff(before, after map[string]string) []string {
	var diffs []string
	for name, state := range after {
		if prior := before[name]; prior != state {
			diffs = append(diffs, fmt.Sprintf("%s: before %s, after %s", name, prior, state))
		}
	}
	sort.Strings(diffs)
	return diffs
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("reading %s: %v", source, err)
	}
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", destination, err)
	}
}
