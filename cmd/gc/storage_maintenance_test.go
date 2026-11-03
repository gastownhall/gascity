package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// migrationWindowProvider stubs the provider lifecycle and the session list
// the window drives, and records what it asked for.
type migrationWindowProvider struct {
	down          bool
	startErr      error
	stopErr       error
	starts, stops int
	// lockHeldAtStart and lockHeldAtStop record whether the controller lock
	// was held when the window started and stopped the provider.
	lockHeldAtStart, lockHeldAtStop bool
	// recordAtStart records whether .gc/storage-window.json existed when the
	// provider was started.
	recordAtStart bool
	// sessions, unlistable and listErr are the session list's answer.
	sessions   []beads.DoltConnection
	unlistable bool
	listErr    error
	lists      int
	// scopeStarts are the rig scopes the window started.
	scopeStarts []string
}

func stubMigrationWindowProvider(t *testing.T, p *migrationWindowProvider) {
	t.Helper()
	stubMigrationWindowStartStop(t, p)
	prevDown := migrationWindowProviderDown
	t.Cleanup(func() { migrationWindowProviderDown = prevDown })
	migrationWindowProviderDown = func(string) (bool, error) { return p.down, nil }
}

// stubMigrationWindowStartStop stubs the start, the stop and the session list:
// whether the provider is down is the real probe's answer.
func stubMigrationWindowStartStop(t *testing.T, p *migrationWindowProvider) {
	t.Helper()
	prevStart, prevStop, prevSessions, prevSettle, prevScope := migrationWindowStartProvider, migrationWindowStopProvider, migrationWindowSessions, migrationWindowSessionSettle, migrationWindowStartScope
	t.Cleanup(func() {
		migrationWindowStartProvider, migrationWindowStopProvider, migrationWindowSessions, migrationWindowSessionSettle, migrationWindowStartScope = prevStart, prevStop, prevSessions, prevSettle, prevScope
	})
	migrationWindowStartScope = func(_, scopeRoot string) error {
		p.scopeStarts = append(p.scopeStarts, scopeRoot)
		return nil
	}
	migrationWindowSessionSettle = 0
	migrationWindowStartProvider = func(cityPath string) error {
		p.starts++
		p.lockHeldAtStart = controllerLockHeld(cityPath)
		p.recordAtStart = fileExists(migrationWindowRecordPath(cityPath))
		return p.startErr
	}
	migrationWindowStopProvider = func(cityPath string) error {
		p.stops++
		p.lockHeldAtStop = controllerLockHeld(cityPath)
		return p.stopErr
	}
	migrationWindowSessions = func(context.Context, string) (func(context.Context) ([]beads.DoltConnection, error), func(), bool, error) {
		p.lists++
		if p.unlistable {
			return nil, nil, false, nil
		}
		list := func(context.Context) ([]beads.DoltConnection, error) { return p.sessions, p.listErr }
		return list, func() {}, true, nil
	}
}

// controllerLockHeld probes whether another descriptor holds the city's
// controller lock.
func controllerLockHeld(cityPath string) bool {
	probe, err := acquireControllerLock(cityPath)
	if err == nil {
		probe.Close() //nolint:errcheck // probe only
	}
	return errors.Is(err, errControllerAlreadyRunning)
}

// windowCity is a city directory with its controller lock held, as a writing
// storage verb holds it.
func windowCity(t *testing.T) (string, *os.File) {
	t.Helper()
	cityPath := recoveryCityPath(t)
	lock, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() }) //nolint:errcheck // test cleanup
	return cityPath, lock
}

// openTestWindow opens a window on cityPath with an empty configuration.
func openTestWindow(t *testing.T, lock *os.File, cityPath string, stderr *bytes.Buffer) (*migrationSourceWindow, error) {
	t.Helper()
	if stderr == nil {
		stderr = &bytes.Buffer{}
	}
	return openMigrationSource(lock, cityPath, &config.City{}, "gc storage migrate", stderr)
}

func TestMigrationSourceLaneRefusalNamesTheGate(t *testing.T) {
	bd := func(gate, reason string) beads.BeadsDiagnostic {
		return beads.BeadsDiagnostic{Store: beads.BeadsStoreNameBdStore, PreflightGate: gate, PreflightReason: reason}
	}
	for _, tc := range []struct {
		name string
		diag beads.BeadsDiagnostic
		want []string // empty: the lane answers payload reads
	}{
		{name: "native", diag: beads.BeadsDiagnostic{Store: beads.BeadsStoreNameNativeDoltStore, NativeStoreEligible: true}},
		{name: "proxied", diag: beads.BeadsDiagnostic{Store: beads.BeadsStoreNameNativeDoltStore, NativeStoreEligible: true, Proxied: &beads.ProxiedDiagnostic{}}},
		{name: "library backend", diag: beads.BeadsDiagnostic{Store: beads.BeadsStoreNameLibraryBackendStore, NativeStoreEligible: true}},
		{name: "file", diag: beads.BeadsDiagnostic{Store: beads.BeadsStoreNameFileStore}},
		// A generic exec store never reaches the lane refusal: it is
		// payload-blind, and the source probe asks it nothing. An exec store
		// that does is backed by bd, and no native lane serves it.
		{name: "bd-backed exec", diag: beads.BeadsDiagnostic{Store: beads.BeadsStoreNameExecStore}, want: []string{"ExecStore", "gc-beads-k8s", "No native lane serves", "Nothing was written"}},
		{name: "bd hooks", diag: bd(beads.BeadsGateBdHooks, "bd hooks are installed"), want: []string{"gate bd_hooks", ".beads/hooks", "ga-qcpgy"}},
		{name: "schema skew", diag: bd(string(contract.PreflightCheckVersionCompat), "schema behind"), want: []string{"gate version_compat", "`bd migrate`"}},
		{name: "forced fallback", diag: bd(beads.BeadsGateForceFallback, "GC_BEADS_FORCE_FALLBACK=1"), want: []string{"Unset it"}},
		{name: "metadata backend", diag: bd(string(contract.PreflightCheckMetadataBackend), "backend http"), want: []string{"gate metadata_backend", "not one the native store serves", "Nothing was written"}},
		{name: "proxied lane refused", diag: beads.BeadsDiagnostic{Store: beads.BeadsStoreNameBdStore, PreflightGate: beads.BeadsGateProxiedProvider, Proxied: &beads.ProxiedDiagnostic{Verdict: beads.ProxiedVerdict("cursor_skew")}}, want: []string{"verdict cursor_skew", "`gc doctor`"}},
		{name: "native open failed", diag: bd("native_open", "dial tcp: connection refused"), want: []string{"gate native_open", "connection refused", "`gc doctor`"}},
		{name: "bd-contract exec fallback", diag: beads.BeadsDiagnostic{Store: beads.BeadsStoreNameExecStore, PreflightGate: "native_open", PreflightReason: "down"}, want: []string{"ExecStore", "gate native_open"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := migrationSourceLaneRefusal(tc.diag)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("a lane that answers payload reads was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a lane that cannot answer a payload read was not refused")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
		})
	}
}

// TestMigrationWindowStartsADownProviderAndStopsItOnClose: the window records
// that it is starting the provider before the start, starts it under the held
// lock, overrides the native transport for its lifetime, and on close stops
// the provider and removes the record.
func TestMigrationWindowStartsADownProviderAndStopsItOnClose(t *testing.T) {
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)

	var stderr bytes.Buffer
	window, err := openTestWindow(t, lock, cityPath, &stderr)
	if err != nil {
		t.Fatalf("openMigrationSource: %v", err)
	}
	if provider.starts != 1 || !provider.lockHeldAtStart {
		t.Fatalf("starts = %d, lock held at start = %v; want one start under the controller lock", provider.starts, provider.lockHeldAtStart)
	}
	if !provider.recordAtStart {
		t.Fatal("the provider was started before the record was written: a run killed in between leaves it running and unrecorded")
	}
	if !fileExists(migrationWindowRecordPath(cityPath)) {
		t.Fatal("the window started the provider without recording that it did")
	}
	if cfg, mode := migrationWindowOpen(cityPath); cfg == nil || mode == nil || *mode != beads.NativeTransportAuto {
		t.Errorf("inside the window the opens take (%v, %v), want the verb's config and auto (O9)", cfg, mode)
	}
	if provider.lists != 1 {
		t.Errorf("the window listed sessions %d time(s) on the server it started, want 1", provider.lists)
	}
	if !strings.Contains(stderr.String(), "started the bead-store provider") {
		t.Errorf("the window did not say it started the provider: %s", stderr.String())
	}

	if err := window.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if provider.stops != 1 {
		t.Fatalf("stops = %d, want 1: the provider the window started was left running", provider.stops)
	}
	if fileExists(migrationWindowRecordPath(cityPath)) {
		t.Error("the record outlived the stop")
	}
	if cfg, mode := migrationWindowOpen(cityPath); cfg != nil || mode != nil {
		t.Errorf("after the window the opens take (%v, %v), want the city's own", cfg, mode)
	}
	if err := window.Close(); err != nil || provider.stops != 1 {
		t.Errorf("a second Close = %v with %d stops, want a no-op", err, provider.stops)
	}
}

func TestMigrationWindowLeavesARunningProviderRunning(t *testing.T) {
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{}
	stubMigrationWindowProvider(t, provider)
	window, err := openTestWindow(t, lock, cityPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := window.Close(); err != nil {
		t.Fatal(err)
	}
	if provider.starts != 0 || provider.stops != 0 || provider.lists != 0 {
		t.Fatalf("starts = %d, stops = %d, lists = %d on a provider that was up; want none", provider.starts, provider.stops, provider.lists)
	}
}

// TestMigrationWindowStopsAProviderAKilledRunStarted: a run killed after it
// started the provider left the record; the next window owns that provider,
// and starts it as well when it is down again.
func TestMigrationWindowStopsAProviderAKilledRunStarted(t *testing.T) {
	for _, down := range []bool{false, true} {
		cityPath, lock := windowCity(t)
		if err := writeMigrationWindowRecord(cityPath); err != nil {
			t.Fatal(err)
		}
		provider := &migrationWindowProvider{down: down}
		stubMigrationWindowProvider(t, provider)
		var stderr bytes.Buffer
		window, err := openTestWindow(t, lock, cityPath, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		if err := window.Close(); err != nil {
			t.Fatal(err)
		}
		wantStarts := 0
		if down {
			wantStarts = 1
		}
		if provider.starts != wantStarts || provider.stops != 1 {
			t.Fatalf("down=%v: starts = %d, stops = %d; want %d start(s) and the inherited provider stopped", down, provider.starts, provider.stops, wantStarts)
		}
		if fileExists(migrationWindowRecordPath(cityPath)) {
			t.Errorf("down=%v: the inherited record outlived the stop", down)
		}
		if !strings.Contains(stderr.String(), "did not stop it") {
			t.Errorf("down=%v: the window did not name the provider it inherited: %s", down, stderr.String())
		}
	}
}

// TestMigrationWindowKeepsTheRecordWhenTheStopFails: a provider the stop could
// not retire stays recorded, so the next window stops it.
func TestMigrationWindowKeepsTheRecordWhenTheStopFails(t *testing.T) {
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{down: true, stopErr: errors.New("dolt would not stop")}
	stubMigrationWindowProvider(t, provider)
	window, err := openTestWindow(t, lock, cityPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := window.Close(); err == nil || !strings.Contains(err.Error(), "dolt would not stop") {
		t.Fatalf("Close = %v, want the stop's failure", err)
	}
	if !fileExists(migrationWindowRecordPath(cityPath)) {
		t.Fatal("the record was removed although the provider it names is still running")
	}
}

func TestMigrationWindowNeedsTheControllerLockAndTheConfig(t *testing.T) {
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	if _, err := openMigrationSource(nil, recoveryCityPath(t), &config.City{}, "gc storage migrate", &bytes.Buffer{}); err == nil {
		t.Fatal("a window opened without the controller lock")
	}
	cityPath, lock := windowCity(t)
	if _, err := openMigrationSource(lock, cityPath, nil, "gc storage migrate", &bytes.Buffer{}); err == nil {
		t.Fatal("a window opened without the verb's configuration: its opens would proceed on none")
	}
	if provider.starts != 0 {
		t.Fatal("the provider was started by a window that refused")
	}
}

// TestMigrationWindowFailedStartStopsWhatItStarted: a start that failed
// half-way is stopped, so the run leaves nothing it began running.
func TestMigrationWindowFailedStartStopsWhatItStarted(t *testing.T) {
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{down: true, startErr: errors.New("dolt did not come up")}
	stubMigrationWindowProvider(t, provider)
	_, err := openTestWindow(t, lock, cityPath, nil)
	if err == nil || !strings.Contains(err.Error(), "dolt did not come up") {
		t.Fatalf("openMigrationSource = %v, want the start's failure", err)
	}
	if provider.stops != 1 || fileExists(migrationWindowRecordPath(cityPath)) {
		t.Fatalf("stops = %d, record kept = %v; want the half-started provider stopped", provider.stops, fileExists(migrationWindowRecordPath(cityPath)))
	}
	if activeMigrationWindow(cityPath) != nil {
		t.Fatal("a window that failed to open is still active")
	}
}

// TestMigrationWindowJudgesTheSessionsOnAServerItStarted (S5 §2.3): a
// gc-managed Dolt the window just started has no client but this process.
// Another session refuses, a list that fails refuses, and a provider that is
// not a gc-managed Dolt is named and left to the attestation.
func TestMigrationWindowJudgesTheSessionsOnAServerItStarted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider migrationWindowProvider
		refusal  string
		note     string
	}{
		{name: "foreign session", provider: migrationWindowProvider{sessions: []beads.DoltConnection{{ID: 7, User: "root", Host: "localhost", DB: "hq", Command: "Sleep"}}}, refusal: "session 7: root@localhost on hq (Sleep)"},
		{name: "unlistable", provider: migrationWindowProvider{listErr: errors.New("processlist denied")}, refusal: "processlist denied"},
		{name: "not a managed Dolt", provider: migrationWindowProvider{unlistable: true}, note: "not a gc-managed Dolt"},
		{name: "alone", provider: migrationWindowProvider{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, lock := windowCity(t)
			provider := tc.provider
			provider.down = true
			stubMigrationWindowProvider(t, &provider)
			var stderr bytes.Buffer
			window, err := openTestWindow(t, lock, cityPath, &stderr)
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("openMigrationSource = %v, want a refusal naming %q", err, tc.refusal)
				}
				if provider.stops != 1 || activeMigrationWindow(cityPath) != nil {
					t.Fatalf("stops = %d, active = %v: the refused window left the server it started running", provider.stops, activeMigrationWindow(cityPath) != nil)
				}
				return
			}
			if err != nil {
				t.Fatalf("openMigrationSource: %v", err)
			}
			defer window.Close() //nolint:errcheck // test cleanup
			if tc.note != "" && !strings.Contains(stderr.String(), tc.note) {
				t.Errorf("the window did not name the provider it cannot judge: %s", stderr.String())
			}
		})
	}
}

// TestMigrationProviderDownReadsTheCitysProvider runs the real probe: a
// journal-less bd-owned proxied city is always started (bd restarts it on any
// read), a city with no bd workspace never is, and a generic exec provider is
// down exactly when its health op fails.
func TestMigrationProviderDownReadsTheCitysProvider(t *testing.T) {
	t.Run("journal-less proxied", func(t *testing.T) {
		city := t.TempDir()
		if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"migrated\"\n\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(city, ".beads", "metadata.json"), contract.MetadataState{
			Database: "dolt", Backend: "dolt", DoltMode: "proxied-server", DoltDatabase: "hq",
		}); err != nil {
			t.Fatal(err)
		}
		if down, err := migrationProviderDown(city); err != nil || !down {
			t.Fatalf("migrationProviderDown = (%v, %v), want a bd-owned proxy always started", down, err)
		}
	})
	t.Run("no bd workspace", func(t *testing.T) {
		t.Setenv("GC_BEADS", "bd")
		if down, err := migrationProviderDown(t.TempDir()); err != nil || down {
			t.Fatalf("migrationProviderDown = (%v, %v), want a city with no workspace never started", down, err)
		}
	})
	for _, tc := range []struct {
		health   string
		wantDown bool
	}{{health: "exit 0", wantDown: false}, {health: "exit 1", wantDown: true}} {
		t.Run("exec health "+tc.health, func(t *testing.T) {
			city := t.TempDir()
			writeMinimalCityToml(t, city)
			script := filepath.Join(t.TempDir(), "beads-provider")
			body := "#!/bin/sh\ncase \"$1\" in\n  health) " + tc.health + " ;;\n  *) exit 2 ;;\nesac\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_BEADS", "exec:"+script)
			if down, err := migrationProviderDown(city); err != nil || down != tc.wantDown {
				t.Fatalf("migrationProviderDown = (%v, %v), want %v", down, err, tc.wantDown)
			}
		})
	}
}

// payloadBlindSource is a work store whose protocol carries no payload, as a
// generic exec store's does: it cannot be asked, and declares there is
// nothing to ask about.
type payloadBlindSource struct{ beads.Store }

func (payloadBlindSource) DepMetadataAbsentByProtocol() (string, bool) { return "exec", true }

// TestInfraMigrationCarriesAPayloadBlindSourceAndSaysSo (S5 §1.4): a generic
// exec source is copied, its edges carrying none, and the manifest and the
// operator are told why. It reaches the migration under the bead-policy
// wrapper, as every production open hands it out.
func TestInfraMigrationCarriesAPayloadBlindSourceAndSaysSo(t *testing.T) {
	backing, from, to, _ := seedInfraEdgeSource(t)
	source := wrapStoreWithBeadPolicies(payloadBlindSource{Store: backing}, &config.City{})

	report, said := migrateFromSource(t, source)
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("Outcome = %v, want converged: a payload-blind source is still refused: %s", report.Outcome, said)
	}
	if !strings.Contains(said, "carries no dependency payload") {
		t.Errorf("the migration did not say the source is payload-blind: %s", said)
	}
	manifest, recorded, err := readInfraCopyManifestRecord(report.Target)
	if err != nil || !recorded {
		t.Fatalf("reading the manifest: recorded=%v err=%v", recorded, err)
	}
	if manifest.SourcePayloadBlind != "exec" {
		t.Errorf("manifest SourcePayloadBlind = %q, want exec", manifest.SourcePayloadBlind)
	}
	binding := openMigratedDestination(t, report.Target)
	deps, err := binding.DepList(from.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range deps {
		found = found || d.DependsOnID == to.ID
	}
	if !found {
		t.Fatalf("the within-infra edge %s -> %s was not copied: %+v", from.ID, to.ID, deps)
	}
	if _, carried, err := openMigratedBinding(t, report).DepMetadata(from.ID, to.ID); err != nil || carried {
		t.Fatalf("the copied edge carries a payload (%v, %v), want none", carried, err)
	}

	// A rewrite (recover-stranded records rows it proved) keeps the record.
	if err := rewriteInfraCopyManifest(report.Target, manifest, setKeys(manifest.IDs), manifest.Created); err != nil {
		t.Fatal(err)
	}
	if again, _, err := readInfraCopyManifestRecord(report.Target); err != nil || again.SourcePayloadBlind != "exec" {
		t.Fatalf("after a rewrite SourcePayloadBlind = %q (%v), want exec", again.SourcePayloadBlind, err)
	}
}

// TestExecStoreOpenerGrantsPayloadBlindOnlyToStoresNotBackedByBd (M1): the
// opener grants the declaration to a generic exec provider, and never to one
// whose rows live in bd: gc-beads-bd, or gc-beads-k8s, whose pods write the
// cluster's Dolt through bd.
func TestExecStoreOpenerGrantsPayloadBlindOnlyToStoresNotBackedByBd(t *testing.T) {
	city := t.TempDir()
	writeMinimalCityToml(t, city)
	for _, tc := range []struct {
		provider  string
		wantBlind bool
	}{
		{provider: "exec:/opt/providers/gc-beads-file", wantBlind: true},
		{provider: "exec:/opt/contrib/gc-beads-k8s", wantBlind: false},
		{provider: "exec:/opt/contrib/gc-beads-k8s.sh", wantBlind: false},
	} {
		store, err := openExecStoreAtForCityWithConfig(tc.provider, city, city, &config.City{})
		if err != nil {
			t.Fatalf("%s: %v", tc.provider, err)
		}
		if _, blind := beads.DepMetadataAbsent(store); blind != tc.wantBlind {
			t.Errorf("%s: payload-blind = %v, want %v", tc.provider, blind, tc.wantBlind)
		}
	}
	for _, provider := range []string{"exec:/city/.gc/scripts/gc-beads-bd.sh", "exec:gc-beads-bd", "exec:/opt/gc-beads-k8s", "bd"} {
		if !execProviderIsBdBacked(provider) {
			t.Errorf("%s is not classified bd-backed", provider)
		}
	}
}

// migrateInWindow runs the migration of source inside a maintenance window
// whose city work store opened on lane.
func migrateInWindow(t *testing.T, source beads.Store, lane beads.BeadsDiagnostic) (infraMigrationReport, string) {
	t.Helper()
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	cityPath, lock := windowCity(t)
	stubMigrationWindowProvider(t, &migrationWindowProvider{})
	window, err := openTestWindow(t, lock, cityPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Close() //nolint:errcheck // test cleanup
	recordMigrationLane(cityPath, lane)
	var stderr bytes.Buffer
	report := migrateInfraClasses(t, cityPath, infraSplitConfig(".gc/store"), &stderr)
	return report, stderr.String()
}

// TestMigrationWindowRefusesTheBdLaneByItsGate: a work store the window could
// not move off the bd CLI lane is refused with that lane's gate and remedy,
// before anything is written, wherever the copy or the clear after it would
// read a payload: a within-infra edge, an infra row's edge into work, or a
// work row's edge onto an infra row. A source with no edge needs no read, and
// migrates as it did before the window.
func TestMigrationWindowRefusesTheBdLaneByItsGate(t *testing.T) {
	hooks := beads.BeadsDiagnostic{Store: beads.BeadsStoreNameBdStore, PreflightGate: beads.BeadsGateBdHooks, PreflightReason: "bd hooks are installed"}
	seed := func(t *testing.T, edge func(store beads.Store, infra, work beads.Bead)) beads.Store {
		t.Helper()
		backing := beads.NewMemStore()
		infra := mustCreateInfraBead(t, backing, beads.Bead{Title: "carried", Type: "session"})
		other := mustCreateInfraBead(t, backing, beads.Bead{Title: "carried too", Type: "session"})
		work := mustCreateInfraBead(t, backing, beads.Bead{Title: "work", Type: "task"})
		if edge != nil {
			edge(backing, infra, work)
		}
		_ = other
		return wrapStoreWithBeadPolicies(mutePayloadSource{Store: backing}, &config.City{})
	}
	for _, tc := range []struct {
		name string
		edge func(store beads.Store, infra, work beads.Bead)
	}{
		{name: "within infra", edge: func(store beads.Store, infra, _ beads.Bead) {
			rows, _ := store.List(beads.ListQuery{Type: "session", AllowScan: true})
			for _, r := range rows {
				if r.ID != infra.ID {
					_ = store.DepAdd(infra.ID, r.ID, "blocks")
				}
			}
		}},
		{name: "infra onto work", edge: func(store beads.Store, infra, work beads.Bead) { _ = store.DepAdd(infra.ID, work.ID, "blocks") }},
		{name: "work onto infra", edge: func(store beads.Store, infra, work beads.Bead) { _ = store.DepAdd(work.ID, infra.ID, "tracks") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, said := migrateInWindow(t, seed(t, tc.edge), hooks)
			if report.Outcome == infraMigrationConverged {
				t.Fatalf("a bd-lane source with an edge to read converged: %s", said)
			}
			for _, want := range []string{"gate bd_hooks", ".beads/hooks"} {
				if !strings.Contains(said, want) {
					t.Errorf("refusal does not name %q: %s", want, said)
				}
			}
			assertRefusedBeforeTheDestinationWasTouched(t, report, said)
		})
	}
	t.Run("no edge to read", func(t *testing.T) {
		report, said := migrateInWindow(t, seed(t, nil), hooks)
		if report.Outcome != infraMigrationConverged {
			t.Fatalf("Outcome = %v: a bd-lane source with no edge to read, which migrates today, was refused: %s", report.Outcome, said)
		}
	})
}

// TestOpenersTakeTheWindowsLanesAndConfig: inside the window both openers
// force the proxied-native lane for their own opens (not the process env),
// override beads.native_transport = "off" (O9), and open with the verb's
// configuration, so an unparseable city.toml is never opened on none. Outside
// it the city's own setting stands, and the unparseable file refuses.
func TestOpenersTakeTheWindowsLanesAndConfig(t *testing.T) {
	cityPath, lock := windowCity(t)
	toml := filepath.Join(cityPath, "city.toml")
	if err := os.WriteFile(toml, []byte("[workspace]\nname = \"t\"\n\n[beads]\nnative_transport = \"off\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS_PROXIED_NATIVE", "")
	var captured beads.StoreOpenOptions
	var proxiedEnv string
	lane := beads.BeadsDiagnostic{Store: beads.BeadsStoreNameNativeDoltStore, NativeStoreEligible: true}
	restore := openStoreFactoryForCity
	openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		captured, proxiedEnv = opts, os.Getenv("GC_BEADS_PROXIED_NATIVE")
		return beads.StoreOpenResult{Store: beads.NewMemStore(), Diagnostic: lane}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = restore })

	open := func() error {
		t.Helper()
		store, err := openInfraMigrationSource(cityPath)
		if err == nil {
			_ = closeBeadStoreHandle(store)
		}
		return err
	}
	if err := open(); err != nil {
		t.Fatal(err)
	}
	if captured.NativeTransport != beads.NativeTransportOff || captured.ForceProxiedNative {
		t.Fatalf("outside a window the open took (%q, forced %v), want the city's off and no forced lane", captured.NativeTransport, captured.ForceProxiedNative)
	}

	stubMigrationWindowProvider(t, &migrationWindowProvider{})
	// The verb's config is told from none by what it resolves: an explicit
	// conditional_writes, where no config resolves the mode as unset. It also
	// says native_transport = "off", so only the window's override opens auto.
	verbCfg := &config.City{Workspace: config.Workspace{Name: "from-the-verb"}, Beads: config.BeadsConfig{ConditionalWrites: "require", NativeTransport: "off"}}
	wantMode := resolvedConditionalWritesMode(verbCfg)
	if wantMode == gate.ModeUnset {
		t.Fatal("the verb's config resolves no conditional-writes mode, so nothing tells it from none")
	}
	window, err := openMigrationSource(lock, cityPath, verbCfg, "gc storage migrate", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer window.Close() //nolint:errcheck // test cleanup
	if err := os.WriteFile(toml, []byte("[workspace\nname = "), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := open(); err != nil {
		t.Fatalf("inside the window the open re-read the unparseable city.toml instead of the verb's config: %v", err)
	}
	if captured.NativeTransport != beads.NativeTransportAuto || !captured.ForceProxiedNative {
		t.Errorf("inside the window the open took (%q, forced %v), want auto (O9) and the forced proxied lane", captured.NativeTransport, captured.ForceProxiedNative)
	}
	if captured.ConditionalWrites != wantMode {
		t.Errorf("inside the window the open resolved conditional writes %q, want the verb's config's %q: it opened on no config", captured.ConditionalWrites, wantMode)
	}
	if proxiedEnv != "" {
		t.Errorf("the window set GC_BEADS_PROXIED_NATIVE=%q for the whole process", proxiedEnv)
	}
	if !window.laneKnown || window.lane.Store != lane.Store {
		t.Errorf("the window recorded lane %+v (known %v), want %+v", window.lane, window.laneKnown, lane)
	}
	if _, err := openPhysicalScopeStore(cityPath, cityPath); err != nil {
		t.Fatalf("a physical scope open inside the window: %v", err)
	}
	if captured.NativeTransport != beads.NativeTransportAuto || !captured.ForceProxiedNative || captured.ConditionalWrites != wantMode {
		t.Errorf("a physical scope open inside the window took (%q, forced %v, mode %q), want auto, the forced lane and the verb's config", captured.NativeTransport, captured.ForceProxiedNative, captured.ConditionalWrites)
	}

	if err := window.Close(); err != nil {
		t.Fatal(err)
	}
	if err := open(); err == nil {
		t.Fatal("outside the window an unparseable city.toml opened on no config")
	}
}

// TestStorageMigrateRunsInsideTheMaintenanceWindow: `gc storage migrate`
// starts a down provider under its controller lock and stops it before the
// lock is released.
func TestStorageMigrateRunsInsideTheMaintenanceWindow(t *testing.T) {
	request, _ := exclusionCity(t)
	request.FleetStopped = true
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("migration = %d: %s", code, stderr)
	}
	if provider.starts != 1 || !provider.lockHeldAtStart || provider.stops != 1 || !provider.lockHeldAtStop {
		t.Fatalf("starts = %d (lock held %v), stops = %d (lock held %v); want one start and one stop, both under the lock", provider.starts, provider.lockHeldAtStart, provider.stops, provider.lockHeldAtStop)
	}
}

// TestStorageMigrateOpensTheWindowBeforeTheSharedStoreRowCheck (S2): under
// --force=shared-store the row check opens the work store, so on a stopped
// managed city the window must already be open, or the bd CLI's recovery
// starts Dolt with no record.
func TestStorageMigrateOpensTheWindowBeforeTheSharedStoreRowCheck(t *testing.T) {
	request, source := exclusionCity(t)
	request.FleetStopped = true
	sharedStoreCities(t, request)
	request.Force = storageForce{SharedStores: []string{sharedToken}}
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	windowOpenAtFirstOpen := false
	opened := false
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(cityPath string) (beads.Store, error) {
		if !opened {
			opened = true
			windowOpenAtFirstOpen = activeMigrationWindow(cityPath) != nil
		}
		return source, nil
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
	_, _ = migrateFor(t, request)
	if !opened || !windowOpenAtFirstOpen {
		t.Fatalf("opened = %v, window open at the first open = %v: the work store was opened before the window", opened, windowOpenAtFirstOpen)
	}
}

// TestStorageRecoverStrandedRunsInsideTheMaintenanceWindow: a writing
// recover-stranded opens the window; a dry run does not.
func TestStorageRecoverStrandedRunsInsideTheMaintenanceWindow(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		cityPath := recoveryCityPath(t)
		cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
		source := stubInfraMigrationSource(t)
		mustCreateInfraBead(t, source, beads.Bead{Title: "carried", Type: "session"})
		stubInfraControllerPing(t, 0)
		stubSessionInventory(t, nil, true, nil)
		stubOtherCities(t)
		if report := migrateInfraClasses(t, cityPath, cfg, &bytes.Buffer{}); report.Outcome != infraMigrationConverged {
			t.Fatalf("cutover = %v", report.Outcome)
		}
		provider := &migrationWindowProvider{down: true}
		stubMigrationWindowProvider(t, provider)
		request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
		var stdout, stderr bytes.Buffer
		_ = doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{DryRun: dryRun}, &stdout, &stderr)
		want := 1
		if dryRun {
			want = 0
		}
		if provider.starts != want || provider.stops != want || (want == 1 && !provider.lockHeldAtStop) {
			t.Errorf("dry run %v: starts = %d, stops = %d (lock held at stop %v), want %d each under the lock: %s", dryRun, provider.starts, provider.stops, provider.lockHeldAtStop, want, stderr.String())
		}
	}
}

// TestStorageMigrateFromRigsRunsInsideTheMaintenanceWindow: the rig import
// opens the window too; the city's provider serves its rigs' stores.
func TestStorageMigrateFromRigsRunsInsideTheMaintenanceWindow(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-root1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs", "gc.kind": "workflow"})}, nil)
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	if code, stdout, stderr := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s %s", code, stdout, stderr)
	}
	if provider.starts != 1 || !provider.lockHeldAtStart || provider.stops != 1 || !provider.lockHeldAtStop {
		t.Fatalf("starts = %d (lock held %v), stops = %d (lock held %v); want one start and one stop under the lock", provider.starts, provider.lockHeldAtStart, provider.stops, provider.lockHeldAtStop)
	}
}

// TestStorageVerbsFailWhenTheProviderTheyStartedWillNotStop: in each of the
// three verbs, a run that did its work but whose window could not stop the
// provider it started reports the work, exits non-zero, keeps the record, and
// says to stop the bead store before starting the city; the migration then
// tells no one to start the city. The same run with a stop that succeeds exits
// zero.
func TestStorageVerbsFailWhenTheProviderTheyStartedWillNotStop(t *testing.T) {
	verbs := []struct {
		name string
		// reports is what the run's stdout says once its work is done.
		reports string
		// run runs the verb on a city of its own.
		run func(t *testing.T) (cityPath string, code int, stdout, stderr string)
	}{
		{name: "migrate", reports: "Migrated.", run: func(t *testing.T) (string, int, string, string) {
			request, _ := exclusionCity(t)
			request.FleetStopped = true
			var stdout, stderr bytes.Buffer
			code := doStorageMigrate(context.Background(), request, &stdout, &stderr)
			return request.CityPath, code, stdout.String(), stderr.String()
		}},
		{name: "recover-stranded", reports: "nothing to recover", run: func(t *testing.T) (string, int, string, string) {
			cityPath := recoveryCityPath(t)
			cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
			source := stubInfraMigrationSource(t)
			mustCreateInfraBead(t, source, beads.Bead{Title: "carried", Type: "session"})
			stubInfraControllerPing(t, 0)
			stubSessionInventory(t, nil, true, nil)
			stubOtherCities(t)
			if report := migrateInfraClasses(t, cityPath, cfg, &bytes.Buffer{}); report.Outcome != infraMigrationConverged {
				t.Fatalf("cutover = %v", report.Outcome)
			}
			var stdout, stderr bytes.Buffer
			code := doStorageRecoverStranded(context.Background(), storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}, strandedRecoveryOptions{}, &stdout, &stderr)
			return cityPath, code, stdout.String(), stderr.String()
		}},
		{name: "from-rigs", reports: "packs", run: func(t *testing.T) (string, int, string, string) {
			f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
			f.seed("packs", []beads.Bead{graphRow("gp-root1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs", "gc.kind": "workflow"})}, nil)
			code, stdout, stderr := f.importRigs()
			return f.request.CityPath, code, stdout, stderr
		}},
	}
	for _, v := range verbs {
		for _, stopFails := range []bool{false, true} {
			name := v.name + "/stop succeeds"
			if stopFails {
				name = v.name + "/stop fails"
			}
			t.Run(name, func(t *testing.T) {
				provider := &migrationWindowProvider{down: true}
				if stopFails {
					provider.stopErr = errors.New("dolt would not stop")
				}
				stubMigrationWindowProvider(t, provider)
				cityPath, code, stdout, stderr := v.run(t)
				if provider.starts != 1 || provider.stops != 1 || !strings.Contains(stdout, v.reports) {
					t.Fatalf("starts = %d, stops = %d; want one start, one stop, and the work reported (%q): %s %s", provider.starts, provider.stops, v.reports, stdout, stderr)
				}
				kept := fileExists(migrationWindowRecordPath(cityPath))
				if !stopFails {
					if code != 0 || kept {
						t.Fatalf("exit %d, record kept %v; want a clean run: %s %s", code, kept, stdout, stderr)
					}
					return
				}
				if code == 0 {
					t.Fatalf("exit 0 although the provider this run started is still running: %s %s", stdout, stderr)
				}
				if !kept {
					t.Fatal("the record was removed although the provider it names is still running")
				}
				for _, want := range []string{"dolt would not stop", "Stop the city's bead store with `" + storageStopCommand + "` before starting the city"} {
					if !strings.Contains(stderr, want) {
						t.Errorf("stderr does not name %q: %s", want, stderr)
					}
				}
				if strings.Contains(stdout, "Start the city") {
					t.Errorf("the run told the operator to start the city over the provider still running: %s", stdout)
				}
			})
		}
	}
}

// TestRigImportRefusesAPayloadBlindRigWithAnEdgeToRead (S4): the rig import
// does not take the payload-blind declaration, so a generic exec rig with an
// edge on a row it moves is refused before anything is written, and one with
// no edge imports.
func TestRigImportRefusesAPayloadBlindRigWithAnEdgeToRead(t *testing.T) {
	for _, withEdge := range []bool{true, false} {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
		root := graphRow("gp-root1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs", "gc.kind": "workflow"})
		step := graphRow("gp-step1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"})
		var deps []beads.Dep
		if withEdge {
			deps = []beads.Dep{{IssueID: "gp-step1", DependsOnID: "gp-root1", Type: "blocks"}}
		}
		f.seed("packs", []beads.Bead{root, step}, deps)
		f.open["packs"] = func() (beads.Store, error) {
			store, err := f.openFileRig("packs")
			if err != nil {
				return nil, err
			}
			return wrapStoreWithBeadPolicies(payloadBlindSource{Store: store}, f.cfg), nil
		}
		code, stdout, stderr := f.importRigs()
		if withEdge {
			if code == 0 || !strings.Contains(stdout+stderr, "protocol carries no dependency payload") {
				t.Fatalf("import = %d, want the payload-blind rig refused: %s %s", code, stdout, stderr)
			}
			if fileExists(f.loc("packs").InFlightPath()) {
				t.Fatal("the refused import left an in-flight record")
			}
			continue
		}
		if code != 0 {
			t.Fatalf("import = %d: a payload-blind rig with no edge to read was refused: %s %s", code, stdout, stderr)
		}
	}
}

// TestEmittingClassStoreForwardsThePayloadBlindDeclaration: a payload-blind
// store under the class emitter still declares it, as under the bead-policy
// wrapper; a store that declares nothing still does not.
func TestEmittingClassStoreForwardsThePayloadBlindDeclaration(t *testing.T) {
	cityPath := t.TempDir()
	blind := splitClassRoutes(payloadBlindSource{Store: beads.NewMemStore()}).withCLIEmission(cityPath).stores[coordclass.ClassGraph]
	if protocol, absent := beads.DepMetadataAbsent(blind); !absent || protocol != "exec" {
		t.Fatalf("the emitter over a payload-blind store answers (%q, %v), want (exec, true)", protocol, absent)
	}
	plain := splitClassRoutes(beads.NewMemStore()).withCLIEmission(cityPath).stores[coordclass.ClassGraph]
	if _, absent := beads.DepMetadataAbsent(plain); absent {
		t.Fatal("the emitter over a store that declares nothing reads as payload-blind")
	}
}

// TestStorageVerbsTouchNoProviderBeforeTheirChecksPass (R2-M1): a run refused
// by a check that reads no store (here the missing --fleet-stopped
// attestation, a controller that still answers, and the migration guard
// another storage verb holds, as a purge holds it without the controller lock)
// never starts or stops the provider, in each of the three verbs.
func TestStorageVerbsTouchNoProviderBeforeTheirChecksPass(t *testing.T) {
	type verb struct {
		name string
		// run runs the verb on a city of its own, after hold has run on it.
		run func(t *testing.T, fleetStopped bool, livePID int, hold func(t *testing.T, cityPath string)) (int, string)
	}
	verbs := []verb{
		{name: "migrate", run: func(t *testing.T, fleetStopped bool, livePID int, hold func(*testing.T, string)) (int, string) {
			request, _ := exclusionCity(t)
			request.FleetStopped = fleetStopped
			stubInfraControllerPing(t, livePID)
			hold(t, request.CityPath)
			return migrateFor(t, request)
		}},
		{name: "recover-stranded", run: func(t *testing.T, fleetStopped bool, livePID int, hold func(*testing.T, string)) (int, string) {
			cityPath := recoveryCityPath(t)
			cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
			source := stubInfraMigrationSource(t)
			mustCreateInfraBead(t, source, beads.Bead{Title: "carried", Type: "session"})
			stubInfraControllerPing(t, 0)
			stubSessionInventory(t, nil, true, nil)
			stubOtherCities(t)
			if report := migrateInfraClasses(t, cityPath, cfg, &bytes.Buffer{}); report.Outcome != infraMigrationConverged {
				t.Fatalf("cutover = %v", report.Outcome)
			}
			stubInfraControllerPing(t, livePID)
			hold(t, cityPath)
			var stdout, stderr bytes.Buffer
			code := doStorageRecoverStranded(context.Background(), storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: fleetStopped}, strandedRecoveryOptions{}, &stdout, &stderr)
			return code, stderr.String()
		}},
		{name: "from-rigs", run: func(t *testing.T, fleetStopped bool, livePID int, hold func(*testing.T, string)) (int, string) {
			f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
			f.request.FleetStopped = fleetStopped
			stubInfraControllerPing(t, livePID)
			hold(t, f.request.CityPath)
			code, stdout, stderr := f.importRigs()
			return code, stdout + stderr
		}},
	}
	holdNothing := func(*testing.T, string) {}
	holdGuard := func(t *testing.T, cityPath string) {
		t.Helper()
		held, err := storebinding.AcquireMigrationGuard(context.Background(), cityMigrationGuardDirectory(cityPath), storageMigrationGeneration)
		if err != nil {
			t.Fatalf("taking the migration guard: %v", err)
		}
		t.Cleanup(func() { _ = held.Release() })
	}
	for _, v := range verbs {
		for _, tc := range []struct {
			name         string
			fleetStopped bool
			livePID      int
			hold         func(*testing.T, string)
			want         string
		}{
			{name: "no attestation", hold: holdNothing, want: "--" + storageFleetStoppedFlag},
			{name: "live controller", fleetStopped: true, livePID: 4242, hold: holdNothing, want: "controller PID 4242"},
			{name: "guard held", fleetStopped: true, hold: holdGuard, want: "holds this city"},
		} {
			t.Run(v.name+"/"+tc.name, func(t *testing.T) {
				provider := &migrationWindowProvider{down: true}
				stubMigrationWindowProvider(t, provider)
				code, said := v.run(t, tc.fleetStopped, tc.livePID, tc.hold)
				if code == 0 || !strings.Contains(said, tc.want) {
					t.Fatalf("exit %d, output %q; want a refusal naming %q", code, said, tc.want)
				}
				if provider.starts != 0 || provider.stops != 0 {
					t.Fatalf("starts = %d, stops = %d on a refused run; want the provider untouched", provider.starts, provider.stops)
				}
			})
		}
	}

	// The checks particular to a verb: a shared work store with no
	// attestation (it would take the co-tenants' locks), and for a rig
	// import an unconverged city and an unknown rig name.
	for _, tc := range []struct {
		name string
		want string
		run  func(t *testing.T) (int, string)
	}{
		{name: "migrate/shared store unattested", want: "--force=shared-store:" + sharedToken, run: func(t *testing.T) (int, string) {
			request, _ := exclusionCity(t)
			request.FleetStopped = true
			sharedStoreCities(t, request)
			return migrateFor(t, request)
		}},
		{name: "recover-stranded/shared store unattested", want: "--force=shared-store:" + sharedToken, run: func(t *testing.T) (int, string) {
			request, _ := exclusionCity(t)
			if report := migrateInfraClasses(t, request.CityPath, request.Cfg, &bytes.Buffer{}); report.Outcome != infraMigrationConverged {
				t.Fatalf("cutover = %v", report.Outcome)
			}
			sharedStoreCities(t, request)
			var stdout, stderr bytes.Buffer
			code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{}, &stdout, &stderr)
			return code, stderr.String()
		}},
		{name: "from-rigs/unconverged city", want: "has not converged", run: func(t *testing.T) (int, string) {
			request, _ := exclusionCity(t)
			request.Cfg.Rigs = []config.Rig{{Name: "packs", Prefix: "gp", Path: t.TempDir()}}
			var stdout, stderr bytes.Buffer
			code := doStorageMigrateFromRigs(context.Background(), request, nil, &stdout, &stderr)
			return code, stdout.String() + stderr.String()
		}},
		{name: "from-rigs/unknown rig", want: "nope", run: func(t *testing.T) (int, string) {
			f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
			code, stdout, stderr := f.importRigs("nope")
			return code, stdout + stderr
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &migrationWindowProvider{down: true}
			stubMigrationWindowProvider(t, provider)
			code, said := tc.run(t)
			if code == 0 || !strings.Contains(said, tc.want) {
				t.Fatalf("exit %d, output %q; want a refusal naming %q", code, said, tc.want)
			}
			if provider.starts != 0 || provider.stops != 0 {
				t.Fatalf("starts = %d, stops = %d on a refused run; want the provider untouched", provider.starts, provider.stops)
			}
		})
	}
}

// TestStorageMigrateFromRigsStartsTheProxiedRigsItReads: the window starts
// the city's scope through ensureBeadsProvider and stops every provider-owned
// scope, so it also starts each rig scope bd owns that the import reads, and
// only those, and only when it started the provider.
func TestStorageMigrateFromRigsStartsTheProxiedRigsItReads(t *testing.T) {
	for _, down := range []bool{true, false} {
		f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "plain", Prefix: "pl"})
		writeScopeBeadsMetadata(t, f.rigPaths["packs"], `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"gp"}`)
		provider := &migrationWindowProvider{down: down}
		stubMigrationWindowProvider(t, provider)
		if code, stdout, stderr := f.importRigs(); code != 0 {
			t.Fatalf("down=%v: import = %d: %s %s", down, code, stdout, stderr)
		}
		want := []string{}
		if down {
			want = []string{normalizePathForCompare(f.rigPaths["packs"])}
		}
		got := make([]string, 0, len(provider.scopeStarts))
		for _, root := range provider.scopeStarts {
			got = append(got, normalizePathForCompare(root))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("down=%v: the window started rig scopes %v, want %v", down, got, want)
		}
	}
}

// TestMigrationWindowRefusalStopsOnlyWhatThisRunStarted: a refused session
// check stops a provider this run started, and leaves one a killed run
// started running and recorded. The note names the provider and who started
// it.
func TestMigrationWindowRefusalStopsOnlyWhatThisRunStarted(t *testing.T) {
	foreign := []beads.DoltConnection{{ID: 9, User: "root", Host: "localhost", DB: "hq", Command: "Sleep"}}
	t.Run("inherited", func(t *testing.T) {
		cityPath, lock := windowCity(t)
		if err := writeMigrationWindowRecord(cityPath); err != nil {
			t.Fatal(err)
		}
		provider := &migrationWindowProvider{sessions: foreign}
		stubMigrationWindowProvider(t, provider)
		if _, err := openTestWindow(t, lock, cityPath, nil); err == nil || !strings.Contains(err.Error(), "session 9") {
			t.Fatalf("openMigrationSource = %v, want the foreign session refused", err)
		}
		if provider.stops != 0 || !fileExists(migrationWindowRecordPath(cityPath)) {
			t.Fatalf("stops = %d, record kept = %v: a refusal stopped a provider this run did not start", provider.stops, fileExists(migrationWindowRecordPath(cityPath)))
		}
	})
	t.Run("started here", func(t *testing.T) {
		cityPath, lock := windowCity(t)
		provider := &migrationWindowProvider{down: true, sessions: foreign}
		stubMigrationWindowProvider(t, provider)
		if _, err := openTestWindow(t, lock, cityPath, nil); err == nil {
			t.Fatal("the foreign session was not refused")
		}
		if provider.stops != 1 || fileExists(migrationWindowRecordPath(cityPath)) {
			t.Fatalf("stops = %d, record kept = %v: the provider this run started was left running", provider.stops, fileExists(migrationWindowRecordPath(cityPath)))
		}
	})
	t.Run("note", func(t *testing.T) {
		cityPath, lock := windowCity(t)
		if err := writeMigrationWindowRecord(cityPath); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GC_BEADS", "exec:/opt/providers/gc-beads-file")
		stubMigrationWindowProvider(t, &migrationWindowProvider{unlistable: true})
		var stderr bytes.Buffer
		window, err := openTestWindow(t, lock, cityPath, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		defer window.Close() //nolint:errcheck // test cleanup
		said := stderr.String()
		if !strings.Contains(said, "an earlier run started (exec:/opt/providers/gc-beads-file)") || strings.Contains(said, "this run started (") {
			t.Fatalf("the note does not name the provider and who started it: %s", said)
		}
	})
}

// TestMigrationWindowFailedRigScopeStartStopsWhatItStarted: a proxied rig
// scope that will not start refuses the window, and the provider it started
// is stopped.
func TestMigrationWindowFailedRigScopeStartStopsWhatItStarted(t *testing.T) {
	cityPath, lock := windowCity(t)
	rig := t.TempDir()
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"gp"}`)
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	migrationWindowStartScope = func(string, string) error { return errors.New("proxy would not start") }
	_, err := openMigrationSource(lock, cityPath, &config.City{}, "gc storage migrate --from-rigs", &bytes.Buffer{}, rig)
	if err == nil || !strings.Contains(err.Error(), "proxy would not start") {
		t.Fatalf("openMigrationSource = %v, want the rig scope's start failure", err)
	}
	if provider.stops != 1 || activeMigrationWindow(cityPath) != nil {
		t.Fatalf("stops = %d, active = %v: the refused window left what it started running", provider.stops, activeMigrationWindow(cityPath) != nil)
	}
}
