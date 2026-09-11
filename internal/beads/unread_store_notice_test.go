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

// probes counts the population probes the guard spent on this store. The guard
// no longer has one — a subprocess inside List/Ready is charged to the caller's
// read deadline — so this is the regression assertion that it stays gone, and
// every caller of it wants 0.
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

// TestUnreadStoreVerdictIsPerScopeAndCostsNoSubprocess pins both halves of the
// bound the message claims, on the shape that broke the first one.
//
// cmd/gc's scopedBdStoreForCity/scopedBdStoreForRig construct a BRAND NEW
// BdStore on every call, and internal/api's status handler reaches them through
// State.ScopedStoreLike on every request — so a verdict memoized on the store
// object degraded to once per READ, and `statusWorkCounts` fans that out over
// city plus every rig concurrently. Ten throwaway stores over one scope is that
// call pattern, and it must produce exactly one notice.
//
// The second half is the cost: a diagnostic inside List/Ready spends the
// caller's read budget, so it spends no bd subprocess at all. The ladder stops
// at one metadata read.
func TestUnreadStoreVerdictIsPerScopeAndCostsNoSubprocess(t *testing.T) {
	scope := scopeWithUnreadDatabase(t)
	runner := newRecordingRunner(map[string]string{})
	var notices bytes.Buffer

	for i := 0; i < 10; i++ {
		// Exactly what scopedBdStoreForCity does per request: a throwaway
		// store, freshly constructed, over the same scope.
		s := beads.NewBdStore(scope, runner.run, beads.WithBdStoreNoticeSink(&notices))
		if _, err := s.Ready(); err != nil {
			t.Fatalf("Ready() #%d error = %v", i, err)
		}
		if _, err := s.List(beads.ListQuery{AllowScan: true}); err != nil {
			t.Fatalf("List #%d error = %v", i, err)
		}
	}
	if n := runner.probes(); n != 0 {
		t.Fatalf("the guard spent %d probe subprocess(es); a diagnostic inside a read spends the caller's deadline", n)
	}
	if n := strings.Count(notices.String(), "does not point at"); n != 1 {
		t.Fatalf("the notice printed %d times across 10 request-scoped stores over one scope, want exactly 1:\n%s", n, notices.String())
	}
}

// TestUnreadStoreNoticeNeverBlocksAnotherRead pins the one thing a diagnostic
// on the hottest read path in the system must never do: make other readers
// wait.
//
// A sync.Once would park every concurrent empty read behind whoever is reaching
// the verdict, and that winner touches the filesystem and writes to a sink it
// does not control — on a supervisor with a goroutine per rig, one blocked
// stderr becomes a stall across all of them, which is a worse outcome than the
// silence being diagnosed. A compare-and-swap latch means the losers return
// immediately and the answer they were computing is unaffected.
func TestUnreadStoreNoticeNeverBlocksAnotherRead(t *testing.T) {
	scope := scopeWithUnreadDatabase(t)
	writing := make(chan struct{})
	release := make(chan struct{})
	sink := &blockingSink{entered: writing, release: release}
	s := beads.NewBdStore(scope, func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(noRows), nil
	}, beads.WithBdStoreNoticeSink(sink))

	go func() { _, _ = s.Ready() }()
	select {
	case <-writing:
	case <-time.After(10 * time.Second):
		t.Fatal("the notice never started writing")
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
		t.Fatal("reads queued behind the in-flight notice; the verdict must not be a barrier")
	}
	close(release)
}

// blockingSink stalls inside the notice write, standing in for a stderr nobody
// is draining.
type blockingSink struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *blockingSink) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return len(p), nil
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

// TestTheNoticeAsksBdNothingOfItsOwn is the deadline safety property stated at
// the layer it lives on, and it is the reason the population probe is gone.
//
// Store.List and Store.Ready take no context, so the store cannot see the
// budget its caller is holding. Any bd subprocess it adds is charged to that
// budget: with internal/api's per-store status deadline at 250ms and bd
// answering a probe in 400ms, a List bd had ALREADY answered successfully came
// back to the handler as "list timed out: context deadline exceeded". The only
// bd invocations a read may make are the ones the caller asked for.
func TestTheNoticeAsksBdNothingOfItsOwn(t *testing.T) {
	scope := scopeWithUnreadDatabase(t)
	runner := newRecordingRunner(map[string]string{})
	var notices bytes.Buffer
	s := beads.NewBdStore(scope, runner.run, beads.WithBdStoreNoticeSink(&notices))

	if _, err := s.Ready(); err != nil {
		t.Fatalf("Ready() error = %v, want nil", err)
	}
	runner.mu.Lock()
	afterReady := append([]string(nil), runner.commands...)
	runner.mu.Unlock()
	if len(afterReady) != 1 || afterReady[0] != readyCmd {
		t.Fatalf("Ready() ran %v; want exactly the caller's own read %q and nothing else", afterReady, readyCmd)
	}
	if _, err := s.List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("List error = %v, want nil", err)
	}
	runner.mu.Lock()
	all := append([]string(nil), runner.commands...)
	runner.mu.Unlock()
	if len(all) != 2 {
		t.Fatalf("an empty Ready plus an empty List ran %d bd command(s) (%v); want 2 — the two the caller asked for", len(all), all)
	}
	if !strings.Contains(notices.String(), "does not point at") {
		t.Fatalf("the notice did not fire without the probe: %q", notices.String())
	}
	if !strings.Contains(notices.String(), "spends no bd subprocess of its own") {
		t.Fatalf("the notice does not state the bound it now holds: %q", notices.String())
	}
}

// TestABrokenBdDoesNotSilenceOrChangeTheNotice is what the removed probe's
// failure case turns into. The old guard asked bd a second question and went
// quiet whenever that question failed, so a transient Dolt hiccup disabled the
// diagnostic. With no probe there is nothing to fail: a scope whose every bd
// invocation errors still gets its error back, unchanged, and a scope whose
// read succeeds empty still gets the notice.
func TestABrokenBdDoesNotSilenceOrChangeTheNotice(t *testing.T) {
	scope := scopeWithUnreadDatabase(t)
	wantErr := fmt.Errorf("dial tcp 127.0.0.1:3306: connection refused")
	runner := newRecordingRunner(map[string]string{}).fail(readyCmd, wantErr)
	var notices bytes.Buffer
	s := beads.NewBdStore(scope, runner.run, beads.WithBdStoreNoticeSink(&notices))

	if _, err := s.Ready(); !strings.Contains(fmt.Sprint(err), "connection refused") {
		t.Fatalf("Ready() error = %v, want the caller's own bd failure surfaced verbatim", err)
	}
	if notices.Len() != 0 {
		t.Fatalf("a read that FAILED printed the unread-store notice: %q", notices.String())
	}
	// The failed read is not a verdict either: the next read that genuinely
	// answers empty still gets the notice.
	if _, err := s.List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("List error = %v, want nil", err)
	}
	if !strings.Contains(notices.String(), "does not point at") {
		t.Fatalf("a prior bd failure suppressed the notice on a later successful empty read: %q", notices.String())
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
		"no read of this scope has returned a row in this process",
		"Nothing failed",
		"indistinguishable from a real one",
		"notice and not a refusal",
		"spends no bd subprocess of its own",
		"idle ledger",
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
	// Nor may it claim the active store is empty. Without a probe that is not
	// established, and the message that overstates here is the one that gets an
	// operator to delete the wrong directory.
	for _, forbidden := range []string{"found no row", "holds nothing", "store is empty"} {
		if strings.Contains(notice, forbidden) {
			t.Errorf("the notice claims %q about the active store, which nothing here established:\n%s", forbidden, notice)
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
// an idle scope whose one-shot verdict has already been reached. Every read
// after the first pays two atomic loads.
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
