package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Regression tests from the second review of #8332.

// "Transient" means store I/O that got no answer, nothing else: the real
// vocabulary of hosted bd serve, Dolt/MySQL, SQLite and bd itself, shared with
// gc hook's store-unavailable classification (storeGotNoAnswer).
func TestIsTransientStoreFaultIsStoreIOOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		// transient
		{"hosted dial timeout", errors.New(`Get "https://hosted-bd/v1/issues": dial tcp: i/o timeout`), true},
		{"hosted 503 busy", errors.New("bd list: exit status 1: Error: list: bd serve at https://bd.example answered 503 busy: transaction retry budget exhausted"), true},
		{"hosted 503 db_unavailable", errors.New("bd list: exit status 1: Error: list: bd serve at https://bd.example answered 503 db_unavailable: bd serve cannot reach its database"), true},
		{"hosted connect EOF", errors.New(`bd list: exit status 1: Error: cannot reach bd serve at https://bd.example: Post "https://bd.example/v1/issues:list": EOF`), true},
		{"hosted 502", errors.New("bd list: exit status 1: Error: list: bd serve at https://bd.example answered 502"), true},
		{"hosted 504", errors.New("bd list: exit status 1: Error: list: bd serve at https://bd.example answered 504"), true},
		{"dolt lost connection", errors.New("bd list: exit status 1: Error: failed to list issues: Error 2013: lost connection to MySQL server during query"), true},
		{"mysql unexpected EOF text", errors.New("bd list: exit status 1: Error: failed to list issues: [mysql] packets.go:58: unexpected EOF"), true},
		{"typed unexpected EOF", fmt.Errorf("reading: %w", io.ErrUnexpectedEOF), true},
		{"closed network connection", errors.New("bd list: exit status 1: read tcp 127.0.0.1:1->127.0.0.1:3307: use of closed network connection"), true},
		{"bd silent fallback", errors.New("bd list: exit status 1: Importing 12 issues from JSONL into empty database"), true},
		{"sqlite table locked", errors.New("listing work store: database table is locked (6) (SQLITE_LOCKED)"), true},
		{"database is locked", errors.New("database is locked (SQLITE_BUSY)"), true},
		{"bd killed", errors.New("bd list: signal: killed"), true},
		{"EMFILE", fmt.Errorf("open: %w", syscall.EMFILE), true},
		{"EAGAIN", fmt.Errorf("read: %w", syscall.EAGAIN), true},
		{"context deadline", fmt.Errorf("listing: %w", context.DeadlineExceeded), true},
		{"store unavailable", fmt.Errorf("x: %w", beads.ErrStoreUnavailable), true},
		{"ECONNREFUSED", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"EHOSTUNREACH", fmt.Errorf("dial: %w", syscall.EHOSTUNREACH), true},
		{"ENETUNREACH", fmt.Errorf("dial: %w", syscall.ENETUNREACH), true},
		{"net timeout", &net.OpError{Op: "read", Err: timeoutNetError{}}, true},
		// standing
		{"permission denied", fmt.Errorf("stat: %w", os.ErrPermission), false},
		{"EACCES", &os.PathError{Op: "stat", Path: "/x", Err: syscall.EACCES}, false},
		{"permission on a store that also timed out", fmt.Errorf("%w; %w", os.ErrPermission, context.DeadlineExceeded), false},
		{"conditional write unsupported", fmt.Errorf("x: %w", beads.ErrConditionalWriteUnsupported), false},
		{"EBUSY mount", fmt.Errorf("rename: %w", syscall.EBUSY), false},
		{"ENOSPC", fmt.Errorf("write: %w", syscall.ENOSPC), false},
		{"sqlite disk I/O error", errors.New("disk I/O error (10) (SQLITE_IOERR)"), false},
		{"sqlite malformed", errors.New("database disk image is malformed (11)"), false},
		{"hosted 401", errors.New("bd list: exit status 1: Error: list: bd serve at https://bd.example answered 401 unauthenticated"), false},
		{"hosted no such host", errors.New(`cannot reach bd serve at https://bd.exmaple: Get "x": dial tcp: lookup bd.exmaple: no such host`), false},
		{"malformed annotation", errors.New(`reading the infra copy manifest: malformed "#gc:written x" line`), false},
		{"misconfigured store", errors.New("unknown beads provider \"nope\""), false},
		// Each marker alone (mutants C4, C7, C9, C19).
		{"db_unavailable alone", errors.New("bd serve cannot reach its database: db_unavailable"), true},
		{"too many open files text alone", errors.New("open /x: too many open files"), true},
		{"EMFILE typed alone", opaqueErr{"accept failed", syscall.EMFILE}, true},
		// A syscall.Errno is a net.Error, and EAGAIN (EWOULDBLOCK) is a timeout.
		{"EAGAIN typed alone", opaqueErr{"read failed", syscall.EAGAIN}, true},
		{"ENOSPC typed alone", opaqueErr{"write failed", syscall.ENOSPC}, false},
		{"ENOSPC typed behind a no-answer marker", opaqueErr{"dolt server unreachable while writing", syscall.ENOSPC}, false},
		// A standing marker wins over a no-answer marker in the same text
		// (precedence pinned; mutants C23-C25).
		{"x509 behind cannot reach", errors.New(`cannot reach bd serve at https://bd.example: Get "x": tls: failed to verify certificate: x509: certificate has expired or is not yet valid`), false},
		{"untrusted CA behind cannot reach", errors.New(`cannot reach bd serve at https://bd.example: Get "x": x509: certificate signed by unknown authority`), false},
		{"tls alert behind cannot reach", errors.New(`cannot reach bd serve at https://bd.example: Get "x": remote error: tls: handshake failure`), false},
		{"bad scheme behind cannot reach", errors.New(`cannot reach bd serve at htps://bd.example: Get "htps://bd.example/v1": unsupported protocol scheme "htps"`), false},
		{"disk i/o error with a lock wait", errors.New("database is locked; disk I/O error (10) (SQLITE_IOERR)"), false},
		{"401 behind cannot reach", errors.New("cannot reach bd serve at https://bd.example: answered 401"), false},
		{"unauthenticated behind 503", errors.New("bd serve at https://bd.example answered 503: unauthenticated"), false},
		{"nil", nil, false},
	} {
		if got := isTransientStoreFault(tc.err); got != tc.want {
			t.Errorf("%s: isTransientStoreFault = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The hook reads the same no-answer vocabulary: a hosted bd serve answering
// 503 is an unknown answer (exit 2), not an empty queue.
func TestHookAndGateShareTheNoAnswerVocabulary(t *testing.T) {
	for _, msg := range []string{
		"bd serve at https://bd.example answered 503 busy",
		"cannot reach bd serve at https://bd.example: EOF",
		"Error 2013: lost connection to MySQL server during query",
		"bd list: signal: killed",
	} {
		err := errors.New(msg)
		if !workQueryStoreUnavailable(err) {
			t.Errorf("gc hook does not read %q as store-unavailable", msg)
		}
		if !isTransientStoreFault(err) {
			t.Errorf("the storage gate does not read %q as transient", msg)
		}
	}
	// gc hook --help promises exit 2 with the token for EMFILE and EAGAIN.
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.EAGAIN} {
		if err := (opaqueErr{"work query failed", errno}); !workQueryStoreUnavailable(err) {
			t.Errorf("gc hook does not read a typed %v as store-unavailable", errno)
		}
	}
}

// opaqueErr carries a typed cause behind a message that names none of the
// text markers, so a row tests the type alone.
type opaqueErr struct {
	msg   string
	cause error
}

func (e opaqueErr) Error() string { return e.msg }
func (e opaqueErr) Unwrap() error { return e.cause }

// gc hook reads the shared no-answer vocabulary but NOT the gate's standing
// exclusions (mutant C21): a work query that fails on an unresolvable host or
// a certificate still reports the store-unavailable token, as it did before
// the shared classifier existed.
func TestHookIgnoresTheGatesStandingExclusions(t *testing.T) {
	for _, msg := range []string{
		`cannot reach bd serve at https://bd.exmaple: Get "x": dial tcp: lookup bd.exmaple: no such host`,
		`cannot reach bd serve at https://bd.example: Get "x": x509: certificate signed by unknown authority`,
	} {
		err := errors.New(msg)
		if !workQueryStoreUnavailable(err) {
			t.Errorf("gc hook lost the token for %q", msg)
		}
		if isTransientStoreFault(err) {
			t.Errorf("the gate reads %q as transient", msg)
		}
	}
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o deadline" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

// Probe P1: a malformed manifest annotation holds until edited, so it is a
// standing refusal and the drain fence fails CLOSED on it.
func TestMalformedManifestAnnotationFailsTheFenceClosed(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	f, err := os.OpenFile(target.ManifestPath(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("#gc:written not-a-time\n")
	_ = f.Close()
	_, err = hookSessionDrainPending(drainPendingTestSessionID)
	if err == nil || isTransientStorageGateFault(err) || !hookDrainProbeFailsClosed(err) {
		t.Errorf("malformed manifest: err=%v transient=%v; want a standing, fail-closed refusal", err, isTransientStorageGateFault(err))
	}
}

// A misconfigured work store (the census cannot open it, and not for want of
// an answer) is standing too.
func TestMisconfiguredWorkStoreIsStanding(t *testing.T) {
	migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return nil, errors.New(`unknown beads provider "nope"`)
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
	_, err := hookSessionDrainPending(drainPendingTestSessionID)
	if err == nil || isTransientStorageGateFault(err) {
		t.Errorf("misconfigured work store: err=%v transient=%v; want standing", err, isTransientStorageGateFault(err))
	}
}

// unmarkSplitCity leaves a split city with no marker, manifest or database:
// the no-marker path (a census of the work store, then genesis or refusal).
func unmarkSplitCity(t *testing.T) string {
	t.Helper()
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	for _, p := range []string{target.MarkerPath(), target.ManifestPath()} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Dir(target.Database)); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

type listTimeoutStore struct{ beads.Store }

func (listTimeoutStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("listing: %w", context.DeadlineExceeded)
}

// Probe P2 and mutant N11: on the no-marker path a census that times out
// (open or list) is transient, and its message does not claim the city "has
// not migrated onto it" — a census that could not run says nothing either way.
func TestNoMarkerCensusTimeoutsAreTransient(t *testing.T) {
	for name, tc := range map[string]struct {
		store   beads.Store
		openErr error
	}{
		"open": {openErr: errors.New("dial tcp: i/o timeout")},
		"list": {store: listTimeoutStore{beads.NewMemStore()}},
	} {
		t.Run(name, func(t *testing.T) {
			expireUnsettledCLIRoutesImmediately(t)
			unmarkSplitCity(t)
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) { return tc.store, tc.openErr }
			t.Cleanup(func() { openInfraMigrationSource = prev })
			_, err := hookSessionDrainPending(drainPendingTestSessionID)
			if err == nil || !isTransientStorageGateFault(err) || hookDrainProbeFailsClosed(err) {
				t.Errorf("err=%v transient=%v; want a transient, fail-open fault", err, isTransientStorageGateFault(err))
			}
			if err != nil && strings.Contains(err.Error(), "has not migrated onto it") {
				t.Errorf("a census that could not run claims the city has not migrated: %v", err)
			}
		})
	}
}

// The genesis race (another process created the binding at the same moment)
// clears on the next attempt: it is marked as the race.
func TestGenesisRaceIsRecognized(t *testing.T) {
	err := infraGenesisOpenFailure(infraBindingTarget{Binding: "infra", Database: "/x"}, errors.New("table issues already exists"))
	if !errors.Is(err, errInfraGenesisRace) {
		t.Errorf("genesis race not marked: %v", err)
	}
	if !strings.Contains(err.Error(), "lost the race") || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("genesis race message lost its explanation: %v", err)
	}
	if errors.Is(infraGenesisOpenFailure(infraBindingTarget{}, errors.New("disk full")), errInfraGenesisRace) {
		t.Error("an ordinary open failure was marked as the race")
	}
}

// The genesis race, reached through the gate on a split city with nothing to
// migrate, is a transient fault: the next attempt serves from the winner's
// binding.
func TestGenesisRaceThroughTheGateIsTransient(t *testing.T) {
	expireUnsettledCLIRoutesImmediately(t)
	unmarkSplitCity(t)
	prev := inspectInfraGenesis
	inspectInfraGenesis = func(target infraBindingTarget) error {
		return infraGenesisOpenFailure(target, errors.New("applying sqlite schema: table kv already exists (1)"))
	}
	t.Cleanup(func() { inspectInfraGenesis = prev })
	_, err := hookSessionDrainPending(drainPendingTestSessionID)
	if err == nil || !isTransientStorageGateFault(err) || hookDrainProbeFailsClosed(err) {
		t.Errorf("genesis race: err=%v transient=%v; want a transient, fail-open fault", err, isTransientStorageGateFault(err))
	}
}

// Mutant N14: a marker-state read that times out is transient; one refused
// for permission is standing.
func TestMarkerStateReadTransience(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		transient bool
	}{
		"timeout":    {fmt.Errorf("reading the convergence marker: %w", syscall.ETIMEDOUT), true},
		"permission": {fmt.Errorf("reading the convergence marker: %w", &os.PathError{Op: "stat", Path: "/x", Err: syscall.EACCES}), false},
	} {
		t.Run(name, func(t *testing.T) {
			expireUnsettledCLIRoutesImmediately(t)
			migratedOneShotCLICity(t)
			captureCLIStorageStderr(t)
			if err := closeCLIStorageRoutes(); err != nil {
				t.Fatal(err)
			}
			prev := inspectInfraConvergenceState
			inspectInfraConvergenceState = func(infraBindingTarget) (infraConvergenceState, error) { return infraConvergenceAbsent, tc.err }
			t.Cleanup(func() { inspectInfraConvergenceState = prev })
			_, err := hookSessionDrainPending(drainPendingTestSessionID)
			if err == nil || isTransientStorageGateFault(err) != tc.transient {
				t.Errorf("err=%v transient=%v, want transient=%v", err, isTransientStorageGateFault(err), tc.transient)
			}
		})
	}
}

// Mutants N13 and N13b: the born-split census's open and list timeouts are
// transient; a misconfiguration is not.
func TestBornSplitCensusTransience(t *testing.T) {
	for name, tc := range map[string]struct {
		open      func(string) (beads.Store, error)
		transient bool
	}{
		"open timeout": {func(string) (beads.Store, error) { return nil, errors.New("dial tcp: i/o timeout") }, true},
		"list timeout": {func(string) (beads.Store, error) { return listTimeoutStore{beads.NewMemStore()}, nil }, true},
		"misconfig":    {func(string) (beads.Store, error) { return nil, errors.New(`unknown beads provider "nope"`) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			prev := openInfraMigrationSource
			openInfraMigrationSource = tc.open
			t.Cleanup(func() { openInfraMigrationSource = prev })
			var stderr bytes.Buffer
			report := checkBornSplitDiscipline(t.TempDir(), "gc", &stderr)
			if report.Outcome != infraMigrationUncheckable || report.Transient != tc.transient {
				t.Errorf("report = %+v, want uncheckable with transient=%v", report, tc.transient)
			}
		})
	}
}

// Probe P3: a city migrated by `gc storage migrate` but not yet booted, whose
// city.toml is then truncated, must not route its classes to the work store:
// the migrate command now writes the served-binding note.
func TestMigratedUnbootedCityWithATruncatedConfigRefuses(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	stubStrandedRecoverySource(t)
	stubInfraControllerPing(t, 0)
	var stdout, stderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), request, &stdout, &stderr); code != 0 {
		t.Fatalf("gc storage migrate = %d: %s %s", code, stdout.String(), stderr.String())
	}
	if _, present, err := readBornSplitServedNote(request.CityPath); err != nil || !present {
		t.Fatalf("gc storage migrate left no served-binding note (present=%v err=%v)", present, err)
	}
	if err := os.WriteFile(filepath.Join(request.CityPath, "city.toml"), []byte("[workspace]\nname = \"trunc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !cityMayRelocateClasses(request.CityPath) {
		t.Error("a migrated, unbooted city with a truncated city.toml is judged single-store")
	}
}

// The fence event names a transient gate fault as what it is: not a refusal.
func TestDrainFenceReasonForATransientGateFault(t *testing.T) {
	err := standingStorageRefusal{err: storageGateTransientFault{err: errors.New("gc: binding could NOT be verified: dial tcp: i/o timeout")}}
	reason := drainFenceUnavailableReason(err)
	if strings.Contains(reason, "refused") || !strings.Contains(reason, "failed open") || !strings.Contains(reason, "transient") {
		t.Errorf("transient gate fault reason = %q", reason)
	}
}

// TestRevertOfACityThisBuildMigrated is the other half of
// TestRevertingToTheWorkBindingAfterAClearRefusesBoot: a city migrated by THIS
// build's `gc storage migrate` carries the served-binding note too, so its
// revert is held until the operator removes both notes, and the hold names the
// served note.
func TestRevertOfACityThisBuildMigrated(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	source := stubStrandedRecoverySource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	stubInfraControllerPing(t, 0)
	var stdout, stderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), request, &stdout, &stderr); code != 0 {
		t.Fatalf("migrate = %d: %s %s", code, stdout.String(), stderr.String())
	}
	cityPath := request.CityPath
	allWork := &config.City{Storage: &config.StorageConfig{Classes: config.StorageClasses{
		Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding, Sessions: config.StorageWorkBinding,
		Messaging: config.StorageWorkBinding, Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
	}}}
	_ = os.Remove(infraClearedNotePath(cityPath))
	_, err := storageBootGate(cityPath, allWork, "gc start", nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), bornSplitServedNotePath(cityPath)) {
		t.Errorf("revert with only the cleared note removed = %v, want the served-note hold naming %s", err, bornSplitServedNotePath(cityPath))
	}
	if err := os.Remove(bornSplitServedNotePath(cityPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := storageBootGate(cityPath, allWork, "gc start", nil, &bytes.Buffer{}); err != nil {
		t.Errorf("revert after removing both notes still refuses: %v", err)
	}
}

const splitSourceStorage = `
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
`

// Mutant A17: init --from takes the SOURCE's explicit prefix into account, in
// both directions: an explicit non-reserved prefix makes a reserved-looking
// name fine, and an explicit reserved prefix makes a plain name refused.
func TestInitFromASplitSourceHonorsItsExplicitPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, cityName string
		refused                bool
	}{
		{"explicit gw on gas-city-gateway", "gw", "gas-city-gateway", false},
		{"explicit gcg on plain-city", "gcg", "plain-city", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := t.TempDir()
			body := "[workspace]\nname = \"declared\"\nprefix = \"" + tc.prefix + "\"\n" + splitSourceStorage
			if err := os.WriteFile(filepath.Join(src, "city.toml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			err := initFromReservedPrefixRefusal(fsysOSFSForTest(), filepath.Join(src, "city.toml"), filepath.Join(t.TempDir(), tc.cityName), tc.cityName)
			if (err != nil) != tc.refused {
				t.Errorf("refusal = %v, want refused=%v", err, tc.refused)
			}
		})
	}
}

// Plain `gc init --file` of a split template into a reserved-deriving name is
// refused before anything is created (the check split-by-default needs).
func TestInitFromASplitTemplateFileRefusesAReservedDerivedPrefix(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	tmpl := filepath.Join(t.TempDir(), "template.toml")
	if err := os.WriteFile(tmpl, []byte("[workspace]\nname = \"declared\"\n"+splitSourceStorage), 0o644); err != nil {
		t.Fatal(err)
	}
	cityPath := filepath.Join(t.TempDir(), "gas-city-gateway")
	var stdout, stderr bytes.Buffer
	if code := cmdInitFromTOMLFileWithOptions(fsysOSFSForTest(), tmpl, cityPath, "", &stdout, &stderr, true, false); code == 0 {
		t.Fatalf("gc init --file of a split template into gas-city-gateway succeeded: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gcg") || strings.Contains(stderr.String(), "site.toml") {
		t.Errorf("stderr = %q, want the new-city refusal naming gcg", stderr.String())
	}
	if _, err := os.Stat(cityPath); err == nil {
		t.Error("the refused init created the city directory")
	}
}

func fsysOSFSForTest() fsys.FS { return fsys.OSFS{} }

type censusListFailStore struct {
	beads.Store
	err error
}

func (s censusListFailStore) List(beads.ListQuery) ([]beads.Bead, error) { return nil, s.err }

// Mutants A7/A8: on the no-marker path a census that fails for a STANDING
// reason (a misconfigured work store, a list the store refused) is not
// transient.
func TestNoMarkerCensusMisconfigurationIsStanding(t *testing.T) {
	for name, tc := range map[string]struct {
		store   beads.Store
		openErr error
	}{
		"open": {openErr: errors.New(`unknown beads provider "nope"`)},
		"list": {store: censusListFailStore{beads.NewMemStore(), errors.New("no such table: issues")}},
	} {
		t.Run(name, func(t *testing.T) {
			expireUnsettledCLIRoutesImmediately(t)
			unmarkSplitCity(t)
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) { return tc.store, tc.openErr }
			t.Cleanup(func() { openInfraMigrationSource = prev })
			_, err := hookSessionDrainPending(drainPendingTestSessionID)
			if err == nil || isTransientStorageGateFault(err) || !hookDrainProbeFailsClosed(err) {
				t.Errorf("err=%v transient=%v; want a standing, fail-closed refusal", err, isTransientStorageGateFault(err))
			}
		})
	}
}

// Mutant A11: a genesis failure that is NOT the race (and not store I/O) is
// standing.
func TestGenesisFailureOtherThanTheRaceIsStanding(t *testing.T) {
	expireUnsettledCLIRoutesImmediately(t)
	unmarkSplitCity(t)
	prev := inspectInfraGenesis
	inspectInfraGenesis = func(target infraBindingTarget) error {
		return infraGenesisOpenFailure(target, errors.New("applying sqlite schema: near \"TABEL\": syntax error"))
	}
	t.Cleanup(func() { inspectInfraGenesis = prev })
	_, err := hookSessionDrainPending(drainPendingTestSessionID)
	if err == nil || isTransientStorageGateFault(err) {
		t.Errorf("non-race genesis failure: err=%v transient=%v; want standing", err, isTransientStorageGateFault(err))
	}
}

// Mutant A15: the born-split census's list failing for a standing reason is
// not transient.
func TestBornSplitCensusListMisconfigurationIsStanding(t *testing.T) {
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return censusListFailStore{beads.NewMemStore(), errors.New("no such table: issues")}, nil
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
	var stderr bytes.Buffer
	if report := checkBornSplitDiscipline(t.TempDir(), "gc", &stderr); report.Outcome != infraMigrationUncheckable || report.Transient {
		t.Errorf("report = %+v, want uncheckable and not transient", report)
	}
}

// Mutant A24: a .gc/store that cannot even be stat'ed (here a symlink loop) is
// not proof that the city has no binding root.
func TestUnstatableBindingRootIsEvidence(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("store", filepath.Join(dir, ".gc", "store")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"c\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc", "store")); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture: stat of the looping .gc/store = %v, want a non-absence error", err)
	}
	if !cityMayRelocateClasses(dir) {
		t.Error("an unstatable .gc/store was read as no binding root")
	}
}

// Mutant A29 (and review probe Q4): `gc storage migrate` whose served-binding
// note write fails fails loudly, and a re-run completes and writes the note.
func TestStorageMigrateFailsLoudlyWhenTheServedNoteCannotBeWritten(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	stubStrandedRecoverySource(t)
	stubInfraControllerPing(t, 0)
	prev := writeServedBindingNote
	writeServedBindingNote = func(string, bornSplitServedNote) error { return errors.New("injected ENOSPC") }
	var stdout, stderr bytes.Buffer
	code := doStorageMigrate(context.Background(), request, &stdout, &stderr)
	writeServedBindingNote = prev
	if code == 0 || !strings.Contains(stderr.String(), "injected ENOSPC") {
		t.Fatalf("migrate with a failing note write = %d, stderr %q; want a loud failure", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = doStorageMigrate(context.Background(), request, &stdout, &stderr)
	if _, present, _ := readBornSplitServedNote(request.CityPath); code != 0 || !present {
		t.Errorf("the re-run = %d (note present=%v): %s %s", code, present, stdout.String(), stderr.String())
	}
}

// Mutant I1: plain `gc init` runs the reserved-prefix check before the
// scaffold is created, so a refusal leaves nothing behind. No template
// declares [storage] today, so the test shapes one through the hook.
func TestPlainInitRefusesAReservedPrefixBeforeTheScaffold(t *testing.T) {
	prev := initCityConfigHook
	initCityConfigHook = func(cfg *config.City) {
		split := infraSplitConfig(".gc/store")
		cfg.Storage = split.Storage
	}
	t.Cleanup(func() { initCityConfigHook = prev })
	cityPath := filepath.Join(t.TempDir(), "gas-city-gateway")
	var stdout, stderr bytes.Buffer
	if code := doInit(fsysOSFSForTest(), cityPath, wizardConfig{}, "", &stdout, &stderr, false); code == 0 {
		t.Fatalf("plain gc init of a split city named gas-city-gateway succeeded: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gcg") || !strings.Contains(stderr.String(), "name the city differently") {
		t.Errorf("stderr = %q, want the new-city refusal", stderr.String())
	}
	if entries, err := os.ReadDir(cityPath); err == nil && len(entries) > 0 {
		t.Errorf("the refused init left %d entr(ies) behind", len(entries))
	}
}

// Mutants I3/I4: the prefix init gives the new city decides, not whatever the
// template's [workspace] says.
func TestInitReservedPrefixRefusalUsesThePrefixInitAssigns(t *testing.T) {
	cfg := infraSplitConfig(".gc/store")
	cfg.Workspace.Prefix = "gw"
	if err := initReservedPrefixRefusal(fsysOSFSForTest(), "", cfg, "plain-city", "gcg"); err == nil {
		t.Error("the assigned prefix gcg was not refused")
	}
	cfg.Workspace.Prefix = "gcg"
	if err := initReservedPrefixRefusal(fsysOSFSForTest(), "", cfg, "plain-city", "gw"); err != nil {
		t.Errorf("the assigned prefix gw was refused because of the template's: %v", err)
	}
}
