package beads_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The read-time half of the metadata-rewrite fail-open (ga-clsfl), and the
// three experiments that killed its first version (ga-qi9km, removed in
// 63f65d583c). Each experiment below is named EXP1/EXP2/EXP3 after the council
// finding it reproduces, and each one FAILS if the per-store shape regresses to
// the per-call one.

const (
	rowJSON  = `[{"id":"jc-aaa","title":"a real bead","status":"open","issue_type":"task","created_at":"2026-01-15T10:30:00Z"}]`
	noRows   = `[]`
	probeCmd = `bd list --json --all --include-infra --include-gates --limit 1`
	readyCmd = `bd ready --json --limit 0`
)

// recordingRunner answers canned bd output and records every argv, so a test
// can assert both what a read returned and how many subprocesses the guard
// spent getting there.
type recordingRunner struct {
	mu        sync.Mutex
	commands  []string
	responses map[string]string
	errs      map[string]error
}

func newRecordingRunner(responses map[string]string) *recordingRunner {
	return &recordingRunner{responses: responses, errs: map[string]error{}}
}

func (r *recordingRunner) fail(cmd string, err error) *recordingRunner {
	r.errs[cmd] = err
	return r
}

func (r *recordingRunner) run(_, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	r.mu.Lock()
	r.commands = append(r.commands, key)
	r.mu.Unlock()
	if err, ok := r.errs[key]; ok {
		return nil, err
	}
	if out, ok := r.responses[key]; ok {
		return []byte(out), nil
	}
	return []byte(noRows), nil
}

// probes counts the unfiltered population probes the guard spent on this store.
// It is the cost assertion: a store the guard never had to ask about answers 0.
func (r *recordingRunner) probes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.commands {
		if c == probeCmd {
			n++
		}
	}
	return n
}

// scopeWithUnreadDatabase writes the on-disk shape gc's canonicalization
// leaves behind: metadata.json naming the server store, and the embedded
// database it stopped reading still on disk.
func scopeWithUnreadDatabase(t *testing.T) string {
	t.Helper()
	scope := t.TempDir()
	writeScopeMetadata(t, scope, map[string]string{
		"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
	})
	makeDoltRepoDir(t, scope, "embeddeddolt", "jc")
	return scope
}

func writeScopeMetadata(t *testing.T, scope string, fields map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeDoltRepoDir(t *testing.T, scope, sub, database string) string {
	t.Helper()
	path := filepath.Join(scope, ".beads", sub, database)
	if err := os.MkdirAll(filepath.Join(path, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEmptyReadyOnAPopulatedStoreSucceeds is EXP1, and the case the whole suite
// missed the first time.
//
// A scope with dolt_mode=server and a retained .beads/embeddeddolt/jc/.dolt is
// the shape `gc doctor` tells operators to hold ("keep both directories until
// reconciled"). If the active store answers List with a row, that store is
// DEMONSTRABLY the populated one — and an empty ready frontier on it is the
// steady state of an idle city, not evidence of anything. The first version
// judged per CALL and returned ErrOrphanedBeadStore here, which would have
// aborted federateBeadLegs, failed `gc ready`, and hard-failed every agent's
// `gc hook` on a city that was working.
func TestEmptyReadyOnAPopulatedStoreSucceeds(t *testing.T) {
	scope := scopeWithUnreadDatabase(t)
	runner := newRecordingRunner(map[string]string{
		`bd list --json --include-infra --include-gates --limit 0`: rowJSON,
		// A populated store answers the probe with a row too — the point is
		// that the guard must never have to ask.
		probeCmd: rowJSON,
		readyCmd: noRows,
	})
	var notices bytes.Buffer
	s := beads.NewBdStore(scope, runner.run, beads.WithBdStoreNoticeSink(&notices))

	got, err := s.List(beads.ListQuery{AllowScan: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("List = (%d beads, %v), want (1, nil): the active store is populated", len(got), err)
	}
	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready() error = %v, want nil: an empty frontier on a populated store is an ordinary answer", err)
	}
	if len(ready) != 0 {
		t.Fatalf("Ready() = %d beads, want 0", len(ready))
	}
	if notices.Len() != 0 {
		t.Fatalf("a populated store printed the unread-store notice: %q", notices.String())
	}
	if n := runner.probes(); n != 0 {
		t.Fatalf("the guard probed a store that had already answered with rows %d time(s); the disproof must be free", n)
	}
}

// TestFilteredEmptyResultsAreEvidenceOfNothing is EXP2.
//
// The first version sat AFTER client-side filtering, so bd answered with a row
// and the guard refused anyway: Ready{Assignee}, Ready{TierWisps},
// Children(leaf) and an empty mail poll all returned ErrOrphanedBeadStore from
// a store that had just handed back data. Its own justification — "a scope that
// answers with rows pays nothing" — was a claim about the STORE, and the check
// was a claim about one call's result.
//
// Two independent mechanisms have to hold for this to stay fixed, and both are
// asserted here: the store latches rows from what bd RETURNED (before
// applyListQuery and before Ready's tier/assignee loop), and a selector-bearing
// read is not eligible to notice at all.
func TestFilteredEmptyResultsAreEvidenceOfNothing(t *testing.T) {
	for name, read := range map[string]func(s *beads.BdStore) ([]beads.Bead, error){
		"assignee-scoped ready": func(s *beads.BdStore) ([]beads.Bead, error) {
			return s.Ready(beads.ReadyQuery{Assignee: "nobody"})
		},
		"wisp-tier ready": func(s *beads.BdStore) ([]beads.Bead, error) {
			return s.Ready(beads.ReadyQuery{TierMode: beads.TierWisps})
		},
		"children of a leaf": func(s *beads.BdStore) ([]beads.Bead, error) {
			return s.Children("jc-leaf")
		},
		"empty mail poll": func(s *beads.BdStore) ([]beads.Bead, error) {
			return s.List(beads.ListQuery{Type: "message", Assignee: "nobody", AllowScan: true})
		},
		"label lookup that matches nothing": func(s *beads.BdStore) ([]beads.Bead, error) {
			return s.ListByLabel("no-such-label", 0)
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Every bd invocation answers with a row, so anything that comes
			// back empty here was emptied by a CLIENT-side filter, not by the
			// store — while the scope carries the on-disk shape that would
			// otherwise trigger the guard.
			always := &alwaysRowRunner{}
			var notices bytes.Buffer
			s := beads.NewBdStore(scopeWithUnreadDatabase(t), always.run, beads.WithBdStoreNoticeSink(&notices))

			got, err := read(s)
			if err != nil {
				t.Fatalf("%s error = %v, want nil", name, err)
			}
			if len(got) != 0 {
				t.Fatalf("%s returned %d beads; the fixture must produce a client-filtered EMPTY result to prove anything", name, len(got))
			}
			if notices.Len() != 0 {
				t.Fatalf("a client-side-filtered empty result printed the unread-store notice: %q", notices.String())
			}
			if always.probes != 0 {
				t.Fatalf("the guard ran %d probe(s) after bd answered with rows", always.probes)
			}
		})
	}
}

// TestFilteredReadsNeverNoticeEvenOnAnEmptyStore is the other half of EXP2, and
// the one that pins the placement rather than the latch.
//
// Here the store really is empty and the unread database really is on disk —
// every precondition the notice has — and the reads are still not eligible,
// because each carries a selector. "Nothing matched this predicate" is the
// answer a filtered read is FOR. Only a read that asked the whole ledger, and
// got nothing, is making a statement about which database answered it.
func TestFilteredReadsNeverNoticeEvenOnAnEmptyStore(t *testing.T) {
	for name, read := range map[string]func(s *beads.BdStore) ([]beads.Bead, error){
		"assignee-scoped ready": func(s *beads.BdStore) ([]beads.Bead, error) {
			return s.Ready(beads.ReadyQuery{Assignee: "demo/worker"})
		},
		"children of a leaf": func(s *beads.BdStore) ([]beads.Bead, error) { return s.Children("jc-leaf") },
		"mail poll": func(s *beads.BdStore) ([]beads.Bead, error) {
			return s.List(beads.ListQuery{Type: "message", Status: "open", Assignee: "demo/worker"})
		},
		"label lookup": func(s *beads.BdStore) ([]beads.Bead, error) { return s.ListByLabel("some-label", 0) },
		"metadata lookup": func(s *beads.BdStore) ([]beads.Bead, error) {
			return s.ListByMetadata(map[string]string{"gc.root_bead_id": "jc-root"}, 0)
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := newRecordingRunner(map[string]string{})
			var notices bytes.Buffer
			s := beads.NewBdStore(scopeWithUnreadDatabase(t), runner.run, beads.WithBdStoreNoticeSink(&notices))

			got, err := read(s)
			if err != nil || len(got) != 0 {
				t.Fatalf("%s = (%d beads, %v), want (0, nil)", name, len(got), err)
			}
			if notices.Len() != 0 {
				t.Fatalf("a filtered empty result printed the unread-store notice: %q", notices.String())
			}
			if n := runner.probes(); n != 0 {
				t.Fatalf("a filtered empty result spent %d probe subprocess(es); a predicate that matched nothing is evidence of nothing", n)
			}
		})
	}
}

// alwaysRowRunner answers every bd read with one row, so the only way a call
// can come back empty is client-side filtering.
type alwaysRowRunner struct {
	mu     sync.Mutex
	probes int
}

func (a *alwaysRowRunner) run(_, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	if key == probeCmd {
		a.mu.Lock()
		a.probes++
		a.mu.Unlock()
	}
	if strings.HasPrefix(key, "bd query ") {
		return []byte(`[{"id":"jc-wisp","title":"w","status":"open","issue_type":"task","created_at":"2026-01-15T10:30:00Z","ephemeral":true}]`), nil
	}
	return []byte(rowJSON), nil
}

// TestFreshBdInitAdoptionIsNeverRefused is EXP3, and the reason this ships as a
// notice rather than a refusal.
//
// `bd init -p X` uses bd's DEFAULT (embedded) mode and creates an EMPTY
// .beads/embeddeddolt/X/.dolt. `gc rig add` canonicalizes that to server mode,
// which leaves "empty active store beside a second Dolt directory" true forever
// on a workspace that was never broken — the same on-disk shape as the bug.
// Telling them apart means OPENING the unread database, taking a Dolt file lock
// and possibly migrating the schema of the directory the operator was told to
// preserve, which a read path may not do.
//
// So the read succeeds. In particular verifyCanonicalBdScopeStoreReady gates
// `gc rig add` on exactly this List returning a nil error, twenty times with a
// 500ms sleep between, and a guard that failed here would break adoption after
// a ten-second stall.
func TestFreshBdInitAdoptionIsNeverRefused(t *testing.T) {
	scope := scopeWithUnreadDatabase(t)
	runner := newRecordingRunner(map[string]string{})
	var notices bytes.Buffer
	s := beads.NewBdStore(scope, runner.run, beads.WithBdStoreNoticeSink(&notices))

	// The adoption gate, verbatim from verifyCanonicalBdScopeStoreReady.
	got, err := s.List(beads.ListQuery{AllowScan: true, Limit: 1})
	if err != nil {
		t.Fatalf("List(AllowScan, Limit 1) error = %v, want nil: this call gates `gc rig add`", err)
	}
	if len(got) != 0 {
		t.Fatalf("List returned %d beads from an empty store", len(got))
	}
	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready() error = %v, want nil", err)
	}
	if len(ready) != 0 {
		t.Fatalf("Ready() = %d beads, want 0", len(ready))
	}
	// It is still SAID, once — the fresh adoption and the re-pointed workspace
	// are indistinguishable, so the honest answer is to describe the shape and
	// let the read through.
	if !strings.Contains(notices.String(), "does not point at") {
		t.Fatalf("nothing was said about the unread database: %q", notices.String())
	}
}

// TestUnreadStoreVerdictIsPerStoreAndPaidOnce pins the memoization, which is
// what makes the probe affordable on the two most-called reads in the system.
//
// The ladder is: one atomic load for a store that has answered with rows, one
// metadata read for a store that has not, and at most ONE `bd list --limit 1`
// subprocess per store — reached only when a second bead database is actually
// on disk.
func TestUnreadStoreVerdictIsPerStoreAndPaidOnce(t *testing.T) {
	scope := scopeWithUnreadDatabase(t)
	runner := newRecordingRunner(map[string]string{})
	var notices bytes.Buffer
	s := beads.NewBdStore(scope, runner.run, beads.WithBdStoreNoticeSink(&notices))

	for i := 0; i < 25; i++ {
		if _, err := s.Ready(); err != nil {
			t.Fatalf("Ready() #%d error = %v", i, err)
		}
		if _, err := s.List(beads.ListQuery{AllowScan: true}); err != nil {
			t.Fatalf("List #%d error = %v", i, err)
		}
	}
	if n := runner.probes(); n != 1 {
		t.Fatalf("the guard probed the active store %d times across 50 empty reads, want exactly 1", n)
	}
	if n := strings.Count(notices.String(), "does not point at"); n != 1 {
		t.Fatalf("the notice printed %d times across 50 empty reads, want exactly 1:\n%s", n, notices.String())
	}
}

// TestUnreadStoreProbeNeverBlocksAnotherRead pins the one thing a diagnostic on
// the hottest read path in the system must never do: make other readers wait.
//
// The verdict costs a bd subprocess, and a sync.Once would park every
// concurrent empty read behind it — on a supervisor with a goroutine per rig,
// one slow probe becomes a stall across all of them, which is a worse outcome
// than the silence being diagnosed. A compare-and-swap latch means the losers
// return immediately and the answer they were computing is unaffected.
func TestUnreadStoreProbeNeverBlocksAnotherRead(t *testing.T) {
	scope := scopeWithUnreadDatabase(t)
	probeStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name+" "+strings.Join(args, " ") == probeCmd {
			once.Do(func() { close(probeStarted) })
			<-release
		}
		return []byte(noRows), nil
	}
	var notices bytes.Buffer
	s := beads.NewBdStore(scope, runner, beads.WithBdStoreNoticeSink(&notices))

	go func() { _, _ = s.Ready() }()
	select {
	case <-probeStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the probe never started")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 7; i++ {
			if _, err := s.Ready(); err != nil {
				t.Errorf("Ready() #%d error = %v", i, err)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("reads queued behind the in-flight probe; the verdict must not be a barrier")
	}
	close(release)
}

// TestUnreadStoreNoticeStaysQuietOnAScopeWithOneLedger is the false-positive
// budget at the store layer: a city with a single bead database must be
// byte-identical to one that never had this guard, and must never spend a
// subprocess on it.
func TestUnreadStoreNoticeStaysQuietOnAScopeWithOneLedger(t *testing.T) {
	for name, build := range map[string]func(t *testing.T) string{
		"server metadata, server database only": func(t *testing.T) string {
			scope := t.TempDir()
			writeScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
			})
			makeDoltRepoDir(t, scope, "dolt", "jc")
			return scope
		},
		"the unread directory holds no repository": func(t *testing.T) string {
			scope := t.TempDir()
			writeScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
			})
			if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", "jc"), 0o755); err != nil {
				t.Fatal(err)
			}
			return scope
		},
		"a scope with no .beads at all": func(t *testing.T) string { return t.TempDir() },
		"a store with no directory":     func(_ *testing.T) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			runner := newRecordingRunner(map[string]string{})
			var notices bytes.Buffer
			s := beads.NewBdStore(build(t), runner.run, beads.WithBdStoreNoticeSink(&notices))

			if _, err := s.Ready(); err != nil {
				t.Fatalf("Ready() error = %v", err)
			}
			if _, err := s.List(beads.ListQuery{AllowScan: true}); err != nil {
				t.Fatalf("List error = %v", err)
			}
			if notices.Len() != 0 {
				t.Fatalf("a scope with one ledger printed a notice: %q", notices.String())
			}
			if n := runner.probes(); n != 0 {
				t.Fatalf("a scope with one ledger cost %d probe subprocess(es); the shape check must stop first", n)
			}
		})
	}
}

// TestUnreadStoreGuardIsNotSplitStoreSpecific states the effect on a
// SINGLE-STORE city explicitly, because the defect is not split-specific
// either: no coordination class is involved in a metadata rewrite, only a
// workspace pointed at the wrong database.
//
// A store that relocates nothing — the [storage.classes]-free city — must get
// the identical notice as one that relocates a class. If the guard consulted
// the class topology, the city with the simplest configuration would be the one
// left silent.
func TestUnreadStoreGuardIsNotSplitStoreSpecific(t *testing.T) {
	relocated := beads.RelocatedClass{Class: "graph", IDPrefix: "gcg", Location: `the "infra" storage binding`}
	for name, opts := range map[string][]beads.BdStoreOption{
		"single-store city (no relocated classes)": nil,
		"split city (graph relocated)":             {beads.WithBdStoreRelocatedClasses(relocated)},
	} {
		t.Run(name, func(t *testing.T) {
			scope := scopeWithUnreadDatabase(t)
			runner := newRecordingRunner(map[string]string{})
			var notices bytes.Buffer
			s := beads.NewBdStore(scope, runner.run, append(opts, beads.WithBdStoreNoticeSink(&notices))...)

			if _, err := s.Ready(); err != nil {
				t.Fatalf("Ready() error = %v", err)
			}
			if !strings.Contains(notices.String(), "does not point at") {
				t.Fatalf("no notice on a %s: %q", name, notices.String())
			}
		})
	}
}

// TestUnreadStoreNoticeHonorsTheOverride pins the escape hatch. A store-layer
// guard with no way out is the difference between a warning and an outage, and
// `gc doctor` parks operators in this exact shape for as long as
// reconciliation takes.
func TestUnreadStoreNoticeHonorsTheOverride(t *testing.T) {
	t.Setenv(beads.AllowUnreadStoreReadEnvVar, "1")
	scope := scopeWithUnreadDatabase(t)
	runner := newRecordingRunner(map[string]string{})
	var notices bytes.Buffer
	s := beads.NewBdStore(scope, runner.run, beads.WithBdStoreNoticeSink(&notices))

	if _, err := s.Ready(); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
	if notices.Len() != 0 {
		t.Fatalf("%s=1 did not silence the notice: %q", beads.AllowUnreadStoreReadEnvVar, notices.String())
	}
	if n := runner.probes(); n != 0 {
		t.Fatalf("%s=1 still spent %d probe subprocess(es)", beads.AllowUnreadStoreReadEnvVar, n)
	}
}

// TestUnreadStoreProbeFailureSaysNothing keeps the guard inside what it can
// establish. A probe that errors has not shown the active store is empty, and a
// notice on that evidence would fire on any transient bd failure — the same
// overreach, one layer down.
func TestUnreadStoreProbeFailureSaysNothing(t *testing.T) {
	scope := scopeWithUnreadDatabase(t)
	runner := newRecordingRunner(map[string]string{}).fail(probeCmd, fmt.Errorf("dial tcp 127.0.0.1:3306: connection refused"))
	var notices bytes.Buffer
	s := beads.NewBdStore(scope, runner.run, beads.WithBdStoreNoticeSink(&notices))

	if _, err := s.Ready(); err != nil {
		t.Fatalf("Ready() error = %v, want nil: a failed probe must not change the read's answer", err)
	}
	if notices.Len() != 0 {
		t.Fatalf("a failed probe printed a notice anyway: %q", notices.String())
	}
}

// TestUnreadBeadDatabaseReportsOnlyADecidableDisagreement is the false-positive
// budget for the on-disk fact, written as the list of shapes it must stay quiet
// about. Every negative here is a scope some real deployment has.
func TestUnreadBeadDatabaseReportsOnlyADecidableDisagreement(t *testing.T) {
	t.Run("server metadata with an embedded database left behind", func(t *testing.T) {
		scope := t.TempDir()
		want := makeDoltRepoDir(t, scope, "embeddeddolt", "jc")
		writeScopeMetadata(t, scope, map[string]string{
			"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
		})
		unread, active, ok := beads.UnreadBeadDatabase(scope)
		if !ok || unread != want || active != "dolt" {
			t.Fatalf("UnreadBeadDatabase = (%q, %q, %v), want (%q, %q, true)", unread, active, ok, want, "dolt")
		}
	})

	t.Run("embedded metadata with a server database left behind", func(t *testing.T) {
		scope := t.TempDir()
		want := makeDoltRepoDir(t, scope, "dolt", "jc")
		writeScopeMetadata(t, scope, map[string]string{
			"database": "dolt", "backend": "dolt", "dolt_mode": "embedded", "dolt_database": "jc",
		})
		unread, active, ok := beads.UnreadBeadDatabase(scope)
		if !ok || unread != want || active != "embeddeddolt" {
			t.Fatalf("UnreadBeadDatabase = (%q, %q, %v), want (%q, %q, true)", unread, active, ok, want, "embeddeddolt")
		}
	})

	for name, build := range map[string]func(t *testing.T) string{
		"no .beads at all":     func(t *testing.T) string { return t.TempDir() },
		"no scope root at all": func(_ *testing.T) string { return "" },
		"metadata pointing at the database it has": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepoDir(t, scope, "embeddeddolt", "jc")
			writeScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "embedded", "dolt_database": "jc",
			})
			return scope
		},
		"a different database name": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepoDir(t, scope, "embeddeddolt", "other")
			writeScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
			})
			return scope
		},
		"a directory with no dolt repository": func(t *testing.T) string {
			scope := t.TempDir()
			if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", "jc"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
			})
			return scope
		},
		"a non-dolt backend": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepoDir(t, scope, "embeddeddolt", "jc")
			writeScopeMetadata(t, scope, map[string]string{
				"database": "sqlite", "backend": "sqlite", "dolt_mode": "server", "dolt_database": "jc",
			})
			return scope
		},
		"no recorded mode": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepoDir(t, scope, "embeddeddolt", "jc")
			writeScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_database": "jc",
			})
			return scope
		},
		"no recorded database": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepoDir(t, scope, "embeddeddolt", "jc")
			writeScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "server",
			})
			return scope
		},
		"a database name that is a path": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepoDir(t, scope, "embeddeddolt", "jc")
			writeScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "../jc",
			})
			return scope
		},
		"malformed metadata": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepoDir(t, scope, "embeddeddolt", "jc")
			if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"), []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
			return scope
		},
	} {
		t.Run(name, func(t *testing.T) {
			if unread, active, ok := beads.UnreadBeadDatabase(build(t)); ok {
				t.Fatalf("UnreadBeadDatabase reported %q unread (active %q); this shape is not a decidable disagreement", unread, active)
			}
		})
	}
}

// TestUnreadStoreNoticeNamesWhatAnOperatorNeeds pins the message, because a
// notice that only says "something is odd" moves the dead end instead of
// removing it. The reader has to learn which store answered, which one did not,
// why nothing errored, why this is not being called a fault, what to run, and
// how to make it stop — without the source in front of them.
func TestUnreadStoreNoticeNamesWhatAnOperatorNeeds(t *testing.T) {
	notice := beads.UnreadStoreNotice("bd ready", "/cities/demo", "/cities/demo/.beads/embeddeddolt/jc", "dolt")
	for _, want := range []string{
		"bd ready",
		"/cities/demo",
		"/cities/demo/.beads/embeddeddolt/jc",
		`"dolt"`,
		"unfiltered probe",
		"Nothing failed",
		"indistinguishable from a real one",
		"notice and not a refusal",
		"gc doctor",
		"bd import --dry-run",
		"keep both directories until reconciled",
		beads.AllowUnreadStoreReadEnvVar + "=1",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice does not name %q:\n%s", want, notice)
		}
	}
	// It must not claim rows were lost — that is not knowable from disk, and a
	// message that overstates gets ignored the next time it is right.
	for _, forbidden := range []string{"lost", "deleted", "corrupt"} {
		if strings.Contains(strings.ToLower(notice), forbidden) {
			t.Errorf("the notice claims %q, which the on-disk evidence does not support:\n%s", forbidden, notice)
		}
	}
}

// BenchmarkReadyOnAPopulatedStore measures the hot path this guard sits on.
// A store that has answered with rows pays one atomic load per read and never
// reaches the metadata, the disk or a subprocess.
func BenchmarkReadyOnAPopulatedStore(b *testing.B) {
	scope := b.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", "jc", ".dolt"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"jc"}`), 0o644); err != nil {
		b.Fatal(err)
	}
	runner := func(_, _ string, _ ...string) ([]byte, error) { return []byte(rowJSON), nil }
	s := beads.NewBdStore(scope, runner, beads.WithBdStoreNoticeSink(&bytes.Buffer{}))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Ready(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReadyOnAnEmptyStoreAfterTheVerdict measures the OTHER steady state:
// an idle store whose one-shot verdict has already been reached. Every read
// after the first pays an atomic load and a sync.Once fast path.
func BenchmarkReadyOnAnEmptyStoreAfterTheVerdict(b *testing.B) {
	scope := b.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", "jc", ".dolt"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"jc"}`), 0o644); err != nil {
		b.Fatal(err)
	}
	runner := func(_, _ string, _ ...string) ([]byte, error) { return []byte(noRows), nil }
	s := beads.NewBdStore(scope, runner, beads.WithBdStoreNoticeSink(&bytes.Buffer{}))
	if _, err := s.Ready(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Ready(); err != nil {
			b.Fatal(err)
		}
	}
}
