package main

// The storage migration's maintenance window (S5 §1.3, §2.3).
//
// A writing storage verb runs on a stopped city, and `gc stop` stops the
// city's bead-store provider. A managed bd/Dolt city, a proxied one or a
// bd-contract exec one then has no server to read, so its work store fell back
// to the bd CLI lane, which cannot report the payloads on dependency edges,
// and the migration refused it. The window starts the provider the way
// `gc start` does (ensureBeadsProvider), under the controller lock the verb
// already holds, and stops it again on exit the way `gc stop` does
// (shutdownBeadsProvider), but only when the window started it. The nudge
// pollers are not started: they are controller-side readers.
//
// While the window is open every work-store open of this city in this process
// takes the lanes that can answer a payload read, with the configuration the
// verb loaded (a nil one refuses the window):
//
//   - the proxied-native lane is forced on for those opens alone
//     (beads.StoreOpenOptions.ForceProxiedNative);
//   - beads.native_transport = "off" is overridden for them (owner decision
//     O9): it is a preference, not a capability, while bd hooks and schema
//     skew still keep a store on the bd CLI lane.
//
// A store that still lands on the bd CLI lane is refused by the gate that put
// it there, with that gate's remedy, where the copy first needs a payload
// read: the source refusal probes every edge the copy and the clear will read
// before anything is written (migrationWindowPayloadRefusal). A source with no
// edge needs none and migrates as it did before the window existed.
//
// Every check that reads no store runs before the window opens: the
// controller, the shared-store identity and its co-tenants' locks, the
// --fleet-stopped attestation, for a rig import the convergence and the rig
// names, and last the migration guard, which a purge or a dry-run
// recover-stranded holds without the controller lock. A run those refuse
// never touches the provider, and the window closes before the guard is
// released.
//
// A gc-managed Dolt server the window started has, a moment later, no client
// but this process: any other session is a writer nothing proved stopped, and
// the window refuses it. The sessions are listed over a direct connection to
// the published port (beads.ForeignDoltSessions), whatever lane the store
// opened on, and a list that fails refuses. A provider the window started that
// is not a gc-managed Dolt (a bd-owned proxy, whose own sessions to its Dolt
// child no list can tell from another client's; an exec provider) is named,
// and is left to the --fleet-stopped attestation. A refusal stops only a
// provider this run started; one a killed run started stays running and
// recorded.
//
// The window records that it started the provider in .gc/storage-window.json
// before the start, and removes the record once the provider is stopped. A
// stop that fails keeps the record and fails the run, whatever the run did. A
// run killed in between leaves the record, and the next window stops the
// provider it finds, whoever has started it since: the city is stopped and its
// lock is held, so nothing of the city's own is using it.
//
// The design's openMigrationSource returns the opened store. This one returns
// the window, and the verbs keep opening the work store where they did, through
// openInfraMigrationSource and openPhysicalScopeStore, which consult it: a
// store minted here would be a new storeclass row.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltpool"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	// migrationWindowRecordName is the .gc file a window writes before it
	// starts the provider and removes once it has stopped it again.
	migrationWindowRecordName = "storage-window.json"
	// migrationWindowTransport is the native transport the window's opens
	// take (O9).
	migrationWindowTransport = beads.NativeTransportAuto
	// migrationWindowSessionTimeout bounds every session list.
	migrationWindowSessionTimeout = 10 * time.Second
)

// migrationWindowSessionSettle bounds how long the window waits for the
// sessions this process closed to leave the server before it calls a session
// foreign.
var migrationWindowSessionSettle = 3 * time.Second

// The provider lifecycle the window drives, and the session list. Tests
// replace them: the real ones start, stop and dial Dolt servers.
var (
	migrationWindowProviderDown  = migrationProviderDown
	migrationWindowStartProvider = ensureBeadsProvider
	migrationWindowStopProvider  = shutdownBeadsProvider
	migrationWindowSessions      = managedDoltSessionLister
	migrationWindowStartScope    = startProviderOwnedScope
)

// startProviderOwnedScope starts one provider-owned scope, as the provider's
// start op starts the city's.
func startProviderOwnedScope(cityPath, scopeRoot string) error {
	return runProviderOwnedScopeLifecycleOpContext(context.Background(), cityPath, scopeRoot, "start")
}

// migrationSourceWindow is one open maintenance window.
type migrationSourceWindow struct {
	cityPath string
	// cfg is the configuration the verb loaded; the window's opens use it.
	cfg *config.City
	// started reports that this window, or a killed one before it, started
	// the provider, so closing the window stops it.
	started bool
	// startedHere reports that this run started it, not a killed one before.
	startedHere bool
	// lane is the city work store's lane, as its last open in the window
	// reported it (laneKnown false until one has).
	lane      beads.BeadsDiagnostic
	laneKnown bool
	closed    bool
	// deadline is when the verb's time budget runs out (zero: none). The
	// engine stops at the next batch boundary past it (infra_copy_engine.go).
	deadline time.Time
}

// setTimeBudget gives the window's run budget to spend from now; zero or less
// is none.
func (w *migrationSourceWindow) setTimeBudget(budget time.Duration) {
	if w == nil || budget <= 0 {
		return
	}
	w.deadline = time.Now().Add(budget)
}

// migrationWindows holds the process's open window. The controller lock the
// window requires admits one per city, and a verb opens one city.
var migrationWindows struct {
	sync.Mutex
	active *migrationSourceWindow
}

// migrationWindowRecord is the content of .gc/storage-window.json. It names
// no process: the next window stops the provider whoever started it since
// (see the file comment).
type migrationWindowRecord struct {
	StartedProvider bool `json:"started_provider"`
}

// openMigrationSource opens the maintenance window for a writing storage verb
// that holds lock, the city's controller lock (acquireStorageVerbExclusion),
// with cfg, the configuration it loaded. It starts the provider when it is
// down, forces the payload-reading lanes, and, on a gc-managed Dolt it
// started, refuses a foreign session. The caller closes the window before it
// releases the lock.
//
// rigRoots are the rig scopes the verb reads. ensureBeadsProvider starts the
// city's scope alone, and the stop retires every provider-owned scope, so a
// window that starts the provider also starts each of those rigs bd owns a
// proxied scope for: what the run reads, it starts, and what it starts, it
// stops.
func openMigrationSource(lock *os.File, cityPath string, cfg *config.City, logPrefix string, stderr io.Writer, rigRoots ...string) (*migrationSourceWindow, error) {
	if lock == nil {
		return nil, errors.New("the maintenance window needs the city's controller lock held")
	}
	if cfg == nil {
		return nil, errors.New("the maintenance window needs the city's loaded configuration")
	}
	migrationWindows.Lock()
	busy := migrationWindows.active != nil
	migrationWindows.Unlock()
	if busy {
		return nil, errors.New("a maintenance window is already open in this process")
	}
	w := &migrationSourceWindow{cityPath: cityPath, cfg: cfg}
	inherited, err := readMigrationWindowRecord(cityPath)
	if err != nil {
		return nil, err
	}
	down, err := migrationWindowProviderDown(cityPath)
	if err != nil {
		return nil, fmt.Errorf("checking the bead-store provider: %w", err)
	}
	if inherited {
		w.started = true
		fmt.Fprintf(stderr, "%s: an earlier run started the bead-store provider and did not stop it (%s); this run stops it when it ends\n", logPrefix, migrationWindowRecordPath(cityPath)) //nolint:errcheck // best-effort stderr
	}
	if down {
		if err := writeMigrationWindowRecord(cityPath); err != nil {
			return nil, err
		}
		w.started, w.startedHere = true, true
		if err := migrationWindowStartProvider(cityPath); err != nil {
			stopErr := w.stopProvider()
			return nil, errors.Join(fmt.Errorf("starting the bead-store provider for the maintenance window: %w", err), stopErr)
		}
		for _, root := range rigRoots {
			owned, err := scopeProviderOwned(cityPath, root)
			if err == nil && owned {
				err = migrationWindowStartScope(cityPath, root)
			}
			if err != nil {
				stopErr := w.stopProvider()
				return nil, errors.Join(fmt.Errorf("starting the proxied scope %s for the maintenance window: %w", root, err), stopErr)
			}
		}
		fmt.Fprintf(stderr, "%s: started the bead-store provider for this run; it is stopped again when the run ends\n", logPrefix) //nolint:errcheck // best-effort stderr
	}
	migrationWindows.Lock()
	migrationWindows.active = w
	migrationWindows.Unlock()
	if w.started {
		if err := w.refuseForeignSessions(logPrefix, stderr); err != nil {
			return nil, errors.Join(err, w.abandon())
		}
	}
	return w, nil
}

// abandon ends a window that refused before its verb ran. It stops the
// provider only when this run started it. A provider a killed run started is
// left running and recorded: the refusal may name a session of whoever has
// started it since, and the next window owns it again.
func (w *migrationSourceWindow) abandon() error {
	if !w.startedHere {
		w.started = false
	}
	return w.Close()
}

// refuseForeignSessions refuses any client session but this process's on the
// gc-managed Dolt the window started, and refuses when they cannot be listed.
// Another provider is named and left to the attestation.
func (w *migrationSourceWindow) refuseForeignSessions(logPrefix string, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), migrationWindowSessionSettle+migrationWindowSessionTimeout)
	defer cancel()
	who := "this run started"
	if !w.startedHere {
		who = "an earlier run started"
	}
	list, closeList, listable, err := migrationWindowSessions(ctx, w.cityPath)
	if err != nil {
		return fmt.Errorf("listing the client sessions on the Dolt server %s: %w. A server whose sessions cannot be listed is not one with none; nothing was written", who, err)
	}
	if !listable {
		fmt.Fprintf(stderr, "%s: the bead-store provider %s (%s) is not a gc-managed Dolt, so its client sessions cannot be told from the provider's own; --%s attests that nothing else reaches it\n", //nolint:errcheck // best-effort stderr
			logPrefix, who, migrationWindowProviderName(w.cityPath), storageFleetStoppedFlag)
		return nil
	}
	defer closeList()
	deadline := time.Now().Add(migrationWindowSessionSettle)
	for {
		sessions, err := list(ctx)
		if err != nil {
			return fmt.Errorf("listing the client sessions on the Dolt server %s: %w. A server whose sessions cannot be listed is not one with none; nothing was written", who, err)
		}
		// The server reaps a session this process just closed a moment later,
		// so one still there once the settle has passed is another client's.
		if len(sessions) == 0 || !time.Now().Before(deadline) {
			return migrationForeignSessionRefusal(sessions)
		}
		time.Sleep(migrationWindowSessionSettle / 10)
	}
}

// migrationWindowProviderName names the city's bead-store provider for an
// operator: the provider, and that bd owns its proxied scopes when it does.
func migrationWindowProviderName(cityPath string) string {
	name := beadsProvider(cityPath)
	if owned, err := cityScopeProviderOwned(cityPath); err == nil && owned {
		name += ", whose proxied scopes bd owns"
	}
	return name
}

// managedDoltSessionLister opens the session list of the city's gc-managed
// Dolt: a private single-connection client of its published port, which the
// caller closes. listable is false for a provider that is not a gc-managed
// Dolt. This process's own pooled sessions (internal/doltpool: the health and
// project-id reads keep them idle for up to 20s) are closed first, once; a
// one-shot verb holds none of those handles past its call.
func managedDoltSessionLister(_ context.Context, cityPath string) (func(context.Context) ([]beads.DoltConnection, error), func(), bool, error) {
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		return nil, nil, true, err
	}
	if !owned {
		return nil, nil, false, nil
	}
	port := currentManagedDoltPort(cityPath)
	if port == "" {
		return nil, nil, true, errors.New("the managed Dolt publishes no live port")
	}
	doltpool.Shutdown()
	db, err := doltpool.OpenPrivate(managedDoltConnectHost(""), port, "root", managedDoltPassword(), "")
	if err != nil {
		return nil, nil, true, err
	}
	list := func(ctx context.Context) ([]beads.DoltConnection, error) { return beads.ForeignDoltSessions(ctx, db) }
	return list, func() { _ = db.Close() }, true, nil
}

// migrationForeignSessionRefusal is the refusal for the sessions another
// client holds on a Dolt server the window started, or nil when there are none.
func migrationForeignSessionRefusal(sessions []beads.DoltConnection) error {
	if len(sessions) == 0 {
		return nil
	}
	names := make([]string, 0, len(sessions))
	for _, s := range sessions {
		db := s.DB
		if db == "" {
			db = "no database"
		}
		names = append(names, fmt.Sprintf("session %d: %s@%s on %s (%s)", s.ID, s.User, s.Host, db, s.Command))
	}
	return fmt.Errorf("the Dolt server this run just started already has %d other client session(s) (%s), and each can write to the work store. Stop whatever holds them (a surviving agent, a bd process, a shell) and run this again", len(sessions), strings.Join(names, "; "))
}

// Close ends the window: when the window started the provider, it stops it
// while the caller still holds the controller lock. A provider the stop could
// not retire keeps its record, so the next window stops it.
func (w *migrationSourceWindow) Close() error {
	if w == nil || w.closed {
		return nil
	}
	w.closed = true
	migrationWindows.Lock()
	if migrationWindows.active == w {
		migrationWindows.active = nil
	}
	migrationWindows.Unlock()
	return w.stopProvider()
}

// closeReporting is Close for a verb whose exit status is *code. A stop that
// failed leaves running a provider the verb was to leave stopped, so it is
// named on stderr and fails the run.
func (w *migrationSourceWindow) closeReporting(logPrefix string, stderr io.Writer, code *int) {
	if err := w.Close(); err != nil {
		fmt.Fprintf(stderr, "%s: closing the maintenance window: %v. Stop the city's bead store with `%s` before starting the city\n", logPrefix, err, storageStopCommand) //nolint:errcheck // best-effort stderr
		*code = 1
	}
}

// stopProvider stops a provider the window started and removes the record.
func (w *migrationSourceWindow) stopProvider() error {
	if !w.started {
		return nil
	}
	if err := migrationWindowStopProvider(w.cityPath); err != nil {
		return fmt.Errorf("stopping the bead-store provider this run started: %w", err)
	}
	if err := os.Remove(migrationWindowRecordPath(w.cityPath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", migrationWindowRecordPath(w.cityPath), err)
	}
	return nil
}

// activeMigrationWindow returns the window open on cityPath, or nil.
func activeMigrationWindow(cityPath string) *migrationSourceWindow {
	migrationWindows.Lock()
	defer migrationWindows.Unlock()
	if w := migrationWindows.active; w != nil && samePath(w.cityPath, cityPath) {
		return w
	}
	return nil
}

// migrationWindowOpen is what a store open of cityPath takes inside an open
// window: the verb's configuration and the window's native transport. Outside
// one it is (nil, nil), which resolves the city's own.
func migrationWindowOpen(cityPath string) (*config.City, *beads.NativeTransportMode) {
	w := activeMigrationWindow(cityPath)
	if w == nil {
		return nil, nil
	}
	mode := migrationWindowTransport
	return w.cfg, &mode
}

// recordMigrationLane records the lane an open of cityPath's work store took
// in the window.
func recordMigrationLane(cityPath string, diag beads.BeadsDiagnostic) {
	migrationWindows.Lock()
	defer migrationWindows.Unlock()
	if w := migrationWindows.active; w != nil && samePath(w.cityPath, cityPath) {
		w.lane, w.laneKnown = diag, true
	}
}

// migrationWindowPayloadRefusal turns err, a source's payload read refused
// because the store cannot be asked, into the refusal of the lane the window's
// open took, naming its gate and remedy. Any other err, or one outside a window,
// is returned unchanged.
func migrationWindowPayloadRefusal(cityPath string, err error) error {
	if err == nil || !errors.Is(err, beads.ErrDepMetadataUnsupported) {
		return err
	}
	migrationWindows.Lock()
	w := migrationWindows.active
	known := w != nil && samePath(w.cityPath, cityPath) && w.laneKnown
	var lane beads.BeadsDiagnostic
	if known {
		lane = w.lane
	}
	migrationWindows.Unlock()
	if !known {
		return err
	}
	if refusal := migrationSourceLaneRefusal(lane); refusal != nil {
		return fmt.Errorf("%w (%w)", refusal, err)
	}
	return err
}

// migrationSourceLaneRefusal is the refusal for a work store whose lane cannot
// answer a payload read, by the gate that put it there, or nil for a lane that
// can: native Dolt (the proxied lane included), a library backend and the file
// store.
//
// An exec store outside the bd contract opens with no gate. A generic one
// never gets here: its protocol carries no payload, it declares so
// (beads.DepMetadataAbsent), and the source probe asks it nothing. One that
// gets here is backed by bd (gc-beads-k8s), which holds payloads the exec
// protocol cannot report, and no native lane serves it.
func migrationSourceLaneRefusal(diag beads.BeadsDiagnostic) error {
	switch diag.Store {
	case beads.BeadsStoreNameNativeDoltStore, beads.BeadsStoreNameLibraryBackendStore, beads.BeadsStoreNameFileStore:
		return nil
	case beads.BeadsStoreNameExecStore:
		if diag.PreflightGate == "" {
			return fmt.Errorf("the work store opened as an exec store (%s) of a provider backed by bd, such as gc-beads-k8s, and the exec protocol cannot report the payloads bd holds on its dependency edges. No native lane serves such a provider yet. Nothing was written; keep this city unsplit until one does", diag.Store)
		}
	}
	lane := fmt.Sprintf("the work store opened on the bd CLI lane (%s, gate %s: %s), which cannot report the payloads on its dependency edges", diag.Store, diag.PreflightGate, diag.PreflightReason)
	var remedy string
	switch diag.PreflightGate {
	case beads.BeadsGateBdHooks:
		remedy = "Executable bd hooks in .beads/hooks must fire on every write, and only the bd CLI runs them. Remove or disable them for this run and put them back after, or wait for bd to report edge payloads (ga-qcpgy)"
	case string(contract.PreflightCheckVersionCompat):
		remedy = "bd's schema and this build disagree. Run `bd migrate` on the work store, then run this again"
	case beads.BeadsGateForceFallback:
		remedy = "GC_BEADS_FORCE_FALLBACK=1 pins every store to the bd CLI. Unset it for this run"
	case string(contract.PreflightCheckMetadataBackend):
		remedy = "This metadata backend is not one the native store serves, and a migration does not read its payloads through bd (hosted bd over HTTP is not yet a verified source, S5 §1.3). Nothing was written; keep this city unsplit until the backend is supported"
	case beads.BeadsGateProxiedProvider:
		verdict := "none"
		if diag.Proxied != nil && diag.Proxied.Verdict != "" {
			verdict = string(diag.Proxied.Verdict)
		}
		remedy = fmt.Sprintf("This run forces the proxied-native lane on, and it refused (verdict %s). Check the scope with `gc doctor` and run this again", verdict)
	default:
		remedy = "The provider did not serve the native lane. Check it with `gc doctor` and run this again"
	}
	return errors.New(lane + ". " + remedy)
}

// migrationProviderDown reports whether the city's bead-store provider is one
// gc starts and is down now, so the window must start (and later stop) it.
// It mirrors ensureBeadsProvider: a store gc does not start (an external or
// hosted server, a complete storage binding, doltlite, the file store) is
// never down here.
func migrationProviderDown(cityPath string) (bool, error) {
	if owned, err := cityScopeProviderOwned(cityPath); err != nil {
		return false, err
	} else if owned {
		// bd owns the proxy and its Dolt child, and restarts both on any read,
		// so whether they answer now says nothing that lasts. The city is
		// stopped: the window starts them and leaves them stopped, as gc stop
		// did.
		return true, nil
	}
	if cityUsesBdStoreContract(cityPath) {
		if _, err := os.Stat(scopeMetadataJSONPath(cityPath)); err != nil {
			return false, nil // no bd workspace: nothing to start
		}
		// A proxied scope with a persisted binding is provider-owned (above);
		// one only the fresh-scope default makes proxied has nothing gc
		// starts, as ensureBeadsProvider has not.
		if gcDoltSkip() || scopeUsesProxiedDoltMode(cityPath, cityPath) {
			return false, nil
		}
		owned, err := managedDoltLifecycleOwned(cityPath)
		if err != nil || !owned {
			return false, err
		}
		return currentManagedDoltPort(cityPath) == "", nil
	}
	provider := beadsProvider(cityPath)
	if !strings.HasPrefix(provider, "exec:") {
		return false, nil
	}
	env, err := providerLifecycleProcessEnvWithError(cityPath, provider)
	if err != nil {
		return false, err
	}
	return runProviderOpWithEnv(strings.TrimPrefix(provider, "exec:"), env, "health") != nil, nil
}

func migrationWindowRecordPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", migrationWindowRecordName)
}

// readMigrationWindowRecord reports whether a killed window left the record
// of a provider it started.
func readMigrationWindowRecord(cityPath string) (bool, error) {
	data, err := os.ReadFile(migrationWindowRecordPath(cityPath))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", migrationWindowRecordPath(cityPath), err)
	}
	var record migrationWindowRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return false, fmt.Errorf("reading %s: %w. Stop the city's bead store with `%s`, remove the file and run this again", migrationWindowRecordPath(cityPath), err, storageStopCommand)
	}
	return record.StartedProvider, nil
}

// writeMigrationWindowRecord records, durably, that this run is starting the
// provider.
func writeMigrationWindowRecord(cityPath string) error {
	data, err := json.Marshal(migrationWindowRecord{StartedProvider: true})
	if err != nil {
		return err
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, migrationWindowRecordPath(cityPath), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("recording the maintenance window: %w", err)
	}
	return nil
}
