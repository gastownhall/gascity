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
)

// migrationWindowProvider stubs the provider lifecycle the window drives and
// counts what it asked for.
type migrationWindowProvider struct {
	down          bool
	startErr      error
	starts, stops int
	// lockHeldAtStart records whether the controller lock was held when the
	// window started the provider.
	lockHeldAtStart bool
}

func stubMigrationWindowProvider(t *testing.T, p *migrationWindowProvider) {
	t.Helper()
	stubMigrationWindowStartStop(t, p)
	prevDown := migrationWindowProviderDown
	t.Cleanup(func() { migrationWindowProviderDown = prevDown })
	migrationWindowProviderDown = func(string) (bool, error) { return p.down, nil }
}

// stubMigrationWindowStartStop stubs the start and the stop only: whether the
// provider is down is the real probe's answer.
func stubMigrationWindowStartStop(t *testing.T, p *migrationWindowProvider) {
	t.Helper()
	prevStart, prevStop := migrationWindowStartProvider, migrationWindowStopProvider
	t.Cleanup(func() { migrationWindowStartProvider, migrationWindowStopProvider = prevStart, prevStop })
	migrationWindowStartProvider = func(cityPath string) error {
		p.starts++
		probe, err := acquireControllerLock(cityPath)
		if err == nil {
			probe.Close() //nolint:errcheck // probe only
		}
		p.lockHeldAtStart = errors.Is(err, errControllerAlreadyRunning)
		return p.startErr
	}
	migrationWindowStopProvider = func(string) error { p.stops++; return nil }
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
		{name: "generic exec", diag: beads.BeadsDiagnostic{Store: beads.BeadsStoreNameExecStore}},
		{name: "bd hooks", diag: bd(beads.BeadsGateBdHooks, "bd hooks are installed"), want: []string{"gate bd_hooks", ".beads/hooks", "ga-qcpgy"}},
		{name: "schema skew", diag: bd(string(contract.PreflightCheckVersionCompat), "schema behind"), want: []string{"gate version_compat", "`bd migrate`"}},
		{name: "forced fallback", diag: bd(beads.BeadsGateForceFallback, "GC_BEADS_FORCE_FALLBACK=1"), want: []string{"Unset it"}},
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

// TestMigrationWindowStartsADownProviderAndStopsItOnClose: the window starts
// the provider under the held lock, records that it did, forces the
// payload-reading lanes for its lifetime, and on close stops the provider and
// restores the process.
func TestMigrationWindowStartsADownProviderAndStopsItOnClose(t *testing.T) {
	stubInfraMigrationSource(t)
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	t.Setenv(migrationWindowProxiedEnv, "0")

	var stderr bytes.Buffer
	window, err := openMigrationSource(lock, cityPath, "gc storage migrate", &stderr)
	if err != nil {
		t.Fatalf("openMigrationSource: %v", err)
	}
	if provider.starts != 1 || !provider.lockHeldAtStart {
		t.Fatalf("starts = %d, lock held at start = %v; want one start under the controller lock", provider.starts, provider.lockHeldAtStart)
	}
	if !fileExists(migrationWindowRecordPath(cityPath)) {
		t.Fatal("the window started the provider without recording that it did: a killed run would leave it running")
	}
	if got := os.Getenv(migrationWindowProxiedEnv); got != "1" {
		t.Errorf("%s = %q inside the window, want 1", migrationWindowProxiedEnv, got)
	}
	if mode := migrationWindowNativeTransport(cityPath); mode == nil || *mode != beads.NativeTransportAuto {
		t.Errorf("native transport inside the window = %v, want auto (O9)", mode)
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
	if got := os.Getenv(migrationWindowProxiedEnv); got != "0" {
		t.Errorf("%s = %q after the window, want the 0 it had", migrationWindowProxiedEnv, got)
	}
	if mode := migrationWindowNativeTransport(cityPath); mode != nil {
		t.Errorf("native transport after the window = %v, want the city's own", *mode)
	}
	if err := window.Close(); err != nil || provider.stops != 1 {
		t.Errorf("a second Close = %v with %d stops, want a no-op", err, provider.stops)
	}
}

func TestMigrationWindowLeavesARunningProviderRunning(t *testing.T) {
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{}
	stubMigrationWindowProvider(t, provider)
	window, err := openMigrationSource(lock, cityPath, "gc storage migrate", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := window.Close(); err != nil {
		t.Fatal(err)
	}
	if provider.starts != 0 || provider.stops != 0 {
		t.Fatalf("starts = %d, stops = %d on a provider that was up; want neither", provider.starts, provider.stops)
	}
}

// TestMigrationWindowStopsAProviderAKilledRunStarted: a run killed after it
// started the provider left the record; the next window owns that provider.
func TestMigrationWindowStopsAProviderAKilledRunStarted(t *testing.T) {
	stubInfraMigrationSource(t)
	cityPath, lock := windowCity(t)
	if err := writeMigrationWindowRecord(cityPath); err != nil {
		t.Fatal(err)
	}
	provider := &migrationWindowProvider{}
	stubMigrationWindowProvider(t, provider)
	var stderr bytes.Buffer
	window, err := openMigrationSource(lock, cityPath, "gc storage migrate", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if err := window.Close(); err != nil {
		t.Fatal(err)
	}
	if provider.starts != 0 || provider.stops != 1 {
		t.Fatalf("starts = %d, stops = %d; want the inherited provider stopped, not restarted", provider.starts, provider.stops)
	}
	if fileExists(migrationWindowRecordPath(cityPath)) {
		t.Error("the inherited record outlived the stop")
	}
	if !strings.Contains(stderr.String(), "did not stop it") {
		t.Errorf("the window did not name the provider it inherited: %s", stderr.String())
	}
}

func TestMigrationWindowNeedsTheControllerLock(t *testing.T) {
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	if _, err := openMigrationSource(nil, recoveryCityPath(t), "gc storage migrate", &bytes.Buffer{}); err == nil {
		t.Fatal("a window opened without the controller lock")
	}
	if provider.starts != 0 {
		t.Fatal("the provider was started without the controller lock")
	}
}

// TestMigrationWindowFailedStartStopsWhatItStarted: a start that failed
// half-way is stopped, so the run leaves nothing it began running.
func TestMigrationWindowFailedStartStopsWhatItStarted(t *testing.T) {
	stubInfraMigrationSource(t)
	cityPath, lock := windowCity(t)
	provider := &migrationWindowProvider{down: true, startErr: errors.New("dolt did not come up")}
	stubMigrationWindowProvider(t, provider)
	_, err := openMigrationSource(lock, cityPath, "gc storage migrate", &bytes.Buffer{})
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

// sessionListingSource is a work store on a Dolt server that reports the
// server's other sessions.
type sessionListingSource struct {
	beads.Store
	sessions []beads.DoltConnection
	listErr  error
}

func (s sessionListingSource) ForeignConnections(context.Context) ([]beads.DoltConnection, error) {
	return s.sessions, s.listErr
}

// TestMigrationWindowRefusesAForeignSessionOnAServerItStarted (S5 §2.3): a
// Dolt server the window just started has no client but this process; any
// other session is a writer, and the window refuses and stops the server.
func TestMigrationWindowRefusesAForeignSessionOnAServerItStarted(t *testing.T) {
	foreign := sessionListingSource{Store: beads.NewMemStore(), sessions: []beads.DoltConnection{{ID: 7, User: "root", Host: "localhost", DB: "hq", Command: "Sleep"}}}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return foreign, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	prevSettle := migrationWindowSessionSettle
	migrationWindowSessionSettle = 0
	t.Cleanup(func() { migrationWindowSessionSettle = prevSettle })

	t.Run("started", func(t *testing.T) {
		cityPath, lock := windowCity(t)
		provider := &migrationWindowProvider{down: true}
		stubMigrationWindowProvider(t, provider)
		_, err := openMigrationSource(lock, cityPath, "gc storage migrate", &bytes.Buffer{})
		if err == nil {
			t.Fatal("the window opened over a foreign session on the server it started")
		}
		if !strings.Contains(err.Error(), "session 7: root@localhost on hq (Sleep)") {
			t.Errorf("refusal %q does not name the session", err)
		}
		if provider.stops != 1 || activeMigrationWindow(cityPath) != nil {
			t.Fatalf("stops = %d, active = %v: the refused window left the server it started running", provider.stops, activeMigrationWindow(cityPath) != nil)
		}
	})
	t.Run("unlistable", func(t *testing.T) {
		unlistable := sessionListingSource{Store: beads.NewMemStore(), listErr: errors.New("processlist denied")}
		openInfraMigrationSource = func(string) (beads.Store, error) { return unlistable, nil }
		defer func() { openInfraMigrationSource = func(string) (beads.Store, error) { return foreign, nil } }()
		cityPath, lock := windowCity(t)
		provider := &migrationWindowProvider{down: true}
		stubMigrationWindowProvider(t, provider)
		if _, err := openMigrationSource(lock, cityPath, "gc storage migrate", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "processlist denied") {
			t.Fatalf("openMigrationSource = %v, want the listing's failure: a server whose sessions cannot be listed is not one with none", err)
		}
		if provider.stops != 1 {
			t.Fatalf("stops = %d: the refused window left the server it started running", provider.stops)
		}
	})
	t.Run("already up", func(t *testing.T) {
		cityPath, lock := windowCity(t)
		stubMigrationWindowProvider(t, &migrationWindowProvider{})
		window, err := openMigrationSource(lock, cityPath, "gc storage migrate", &bytes.Buffer{})
		if err != nil {
			t.Fatalf("a server the window did not start is not judged by its sessions: %v", err)
		}
		_ = window.Close()
	})
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

// migrateInWindow runs the migration of source inside a maintenance window
// whose city work store opened on lane.
func migrateInWindow(t *testing.T, source beads.Store, lane beads.BeadsDiagnostic) (infraMigrationReport, string) {
	t.Helper()
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	cityPath, lock := windowCity(t)
	stubMigrationWindowProvider(t, &migrationWindowProvider{})
	window, err := openMigrationSource(lock, cityPath, "gc storage migrate", &bytes.Buffer{})
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
// not move off the bd CLI lane is refused with that lane's gate and remedy
// where the copy needs a payload read, and only there: a source with no
// within-infra edge needs none, and migrates as it did before the window.
func TestMigrationWindowRefusesTheBdLaneByItsGate(t *testing.T) {
	hooks := beads.BeadsDiagnostic{Store: beads.BeadsStoreNameBdStore, PreflightGate: beads.BeadsGateBdHooks, PreflightReason: "bd hooks are installed"}

	t.Run("an edge to read", func(t *testing.T) {
		backing, _, _, _ := seedInfraEdgeSource(t)
		report, said := migrateInWindow(t, wrapStoreWithBeadPolicies(mutePayloadSource{Store: backing}, &config.City{}), hooks)
		if report.Outcome == infraMigrationConverged {
			t.Fatalf("a bd-lane source with a within-infra edge converged: %s", said)
		}
		for _, want := range []string{"gate bd_hooks", ".beads/hooks"} {
			if !strings.Contains(said, want) {
				t.Errorf("refusal does not name %q: %s", want, said)
			}
		}
		assertRefusedBeforeTheDestinationWasTouched(t, report, said)
	})
	t.Run("no edge to read", func(t *testing.T) {
		backing := beads.NewMemStore()
		mustCreateInfraBead(t, backing, beads.Bead{Title: "carried", Type: "session"})
		report, said := migrateInWindow(t, wrapStoreWithBeadPolicies(mutePayloadSource{Store: backing}, &config.City{}), hooks)
		if report.Outcome != infraMigrationConverged {
			t.Fatalf("Outcome = %v: a bd-lane source with no edge to read, which migrates today, was refused: %s", report.Outcome, said)
		}
	})
}

// TestOpenInfraMigrationSourceTakesTheWindowsLanes: inside the window the
// production opener forces the proxied-native lane and overrides
// beads.native_transport = "off" (O9), and records the lane; outside it the
// city's own setting stands.
func TestOpenInfraMigrationSourceTakesTheWindowsLanes(t *testing.T) {
	cityPath, lock := windowCity(t)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"t\"\n\n[beads]\nnative_transport = \"off\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(migrationWindowProxiedEnv, "")
	var captured beads.StoreOpenOptions
	var proxiedAtOpen string
	lane := beads.BeadsDiagnostic{Store: beads.BeadsStoreNameNativeDoltStore, NativeStoreEligible: true}
	restore := openStoreFactoryForCity
	openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		captured, proxiedAtOpen = opts, os.Getenv(migrationWindowProxiedEnv)
		return beads.StoreOpenResult{Store: beads.NewMemStore(), Diagnostic: lane}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = restore })

	open := func() {
		t.Helper()
		store, err := openInfraMigrationSource(cityPath)
		if err != nil {
			t.Fatal(err)
		}
		_ = closeBeadStoreHandle(store)
	}
	open()
	if captured.NativeTransport != beads.NativeTransportOff {
		t.Fatalf("outside a window the open took native transport %q, want the city's off", captured.NativeTransport)
	}

	stubMigrationWindowProvider(t, &migrationWindowProvider{})
	window, err := openMigrationSource(lock, cityPath, "gc storage migrate", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer window.Close() //nolint:errcheck // test cleanup
	open()
	if captured.NativeTransport != beads.NativeTransportAuto {
		t.Errorf("inside the window the open took native transport %q, want auto (O9)", captured.NativeTransport)
	}
	if proxiedAtOpen != "1" {
		t.Errorf("inside the window the open saw %s=%q, want 1", migrationWindowProxiedEnv, proxiedAtOpen)
	}
	if !window.laneKnown || window.lane.Store != lane.Store {
		t.Errorf("the window recorded lane %+v (known %v), want %+v", window.lane, window.laneKnown, lane)
	}

	// The physical scope open the rig importer uses takes the same transport.
	if _, err := openPhysicalScopeStore(cityPath, cityPath); err != nil {
		t.Fatal(err)
	}
	if captured.NativeTransport != beads.NativeTransportAuto {
		t.Errorf("a physical scope open inside the window took native transport %q, want auto", captured.NativeTransport)
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
	if provider.starts != 1 || !provider.lockHeldAtStart || provider.stops != 1 {
		t.Fatalf("starts = %d (lock held %v), stops = %d; want one start under the lock and one stop", provider.starts, provider.lockHeldAtStart, provider.stops)
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
		if provider.starts != want || provider.stops != want {
			t.Errorf("dry run %v: starts = %d, stops = %d, want %d each: %s", dryRun, provider.starts, provider.stops, want, stderr.String())
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
	if provider.starts != 1 || !provider.lockHeldAtStart || provider.stops != 1 {
		t.Fatalf("starts = %d (lock held %v), stops = %d; want one start under the lock and one stop", provider.starts, provider.lockHeldAtStart, provider.stops)
	}
}
