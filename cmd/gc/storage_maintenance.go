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
// takes the lanes that can answer a payload read:
//
//   - the proxied-native lane is forced on (GC_BEADS_PROXIED_NATIVE=1, in this
//     process only, restored on exit);
//   - beads.native_transport = "off" is overridden for the migration's own
//     opens (owner decision O9): it is a preference, not a capability, while
//     bd hooks and schema skew still keep a store on the bd CLI lane.
//
// A store that still lands on the bd CLI lane is refused by the gate that put
// it there, with that gate's remedy, at the point the copy needs a payload
// read (migrationWindowPayloadRefusal). A source with no within-infra edge
// needs none and migrates as it did before the window existed.
//
// A Dolt server the window started has, a moment later, no client of its own
// but this process: any other session is a writer nothing proved stopped
// (beads.ForeignDoltConnections), and the window refuses it.
//
// The window records that it started the provider in .gc/storage-window.json
// before the start, and removes the record once the provider is stopped. A run
// killed in between leaves the record, and the next window stops the provider
// it inherits.
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
	"github.com/gastownhall/gascity/internal/doltpool"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	// migrationWindowRecordName is the .gc file a window writes before it
	// starts the provider and removes once it has stopped it again.
	migrationWindowRecordName = "storage-window.json"
	// migrationWindowProxiedEnv forces the proxied-native lane on.
	migrationWindowProxiedEnv = "GC_BEADS_PROXIED_NATIVE"
)

// migrationWindowTransport is the native transport the window's opens take.
var migrationWindowTransport = beads.NativeTransportAuto

// migrationWindowSessionSettle bounds how long the window waits for the
// sessions its own open closed to leave the server before it calls a session
// foreign.
var migrationWindowSessionSettle = 3 * time.Second

// The provider lifecycle the window drives. Tests replace them: the real ones
// start and stop Dolt servers.
var (
	migrationWindowProviderDown  = migrationProviderDown
	migrationWindowStartProvider = ensureBeadsProvider
	migrationWindowStopProvider  = shutdownBeadsProvider
)

// migrationSourceWindow is one open maintenance window.
type migrationSourceWindow struct {
	cityPath string
	// started reports that this window, or a killed one before it, started
	// the provider, so closing the window stops it.
	started bool
	// prevProxied and hadProxied restore GC_BEADS_PROXIED_NATIVE on close.
	prevProxied string
	hadProxied  bool
	// lane is the city work store's lane, as its last open in the window
	// reported it (laneKnown false until one has).
	lane      beads.BeadsDiagnostic
	laneKnown bool
	closed    bool
}

// migrationWindows holds the process's open window. The controller lock the
// window requires admits one per city, and a verb opens one city.
var migrationWindows struct {
	sync.Mutex
	active *migrationSourceWindow
}

// migrationWindowRecord is the content of .gc/storage-window.json.
type migrationWindowRecord struct {
	StartedProvider bool      `json:"started_provider"`
	PID             int       `json:"pid"`
	At              time.Time `json:"at"`
}

// openMigrationSource opens the maintenance window for a writing storage verb
// that holds lock, the city's controller lock (acquireStorageVerbExclusion).
// It starts the provider when it is down, forces the payload-reading lanes,
// and, on a Dolt server it started, refuses a foreign session. The caller
// closes the window before it releases the lock.
func openMigrationSource(lock *os.File, cityPath, logPrefix string, stderr io.Writer) (*migrationSourceWindow, error) {
	if lock == nil {
		return nil, errors.New("the maintenance window needs the city's controller lock held")
	}
	migrationWindows.Lock()
	busy := migrationWindows.active != nil
	migrationWindows.Unlock()
	if busy {
		return nil, errors.New("a maintenance window is already open in this process")
	}
	w := &migrationSourceWindow{cityPath: cityPath}
	inherited, err := readMigrationWindowRecord(cityPath)
	if err != nil {
		return nil, err
	}
	down, err := migrationWindowProviderDown(cityPath)
	if err != nil {
		return nil, fmt.Errorf("checking the bead-store provider: %w", err)
	}
	switch {
	case down:
		if err := writeMigrationWindowRecord(cityPath); err != nil {
			return nil, err
		}
		w.started = true
		if err := migrationWindowStartProvider(cityPath); err != nil {
			stopErr := w.stopProvider()
			return nil, errors.Join(fmt.Errorf("starting the bead-store provider for the maintenance window: %w", err), stopErr)
		}
		fmt.Fprintf(stderr, "%s: started the bead-store provider for this run; it is stopped again when the run ends\n", logPrefix) //nolint:errcheck // best-effort stderr
	case inherited:
		w.started = true
		fmt.Fprintf(stderr, "%s: an earlier run started the bead-store provider and did not stop it (%s); this run stops it when it ends\n", logPrefix, migrationWindowRecordPath(cityPath)) //nolint:errcheck // best-effort stderr
	}
	w.prevProxied, w.hadProxied = os.LookupEnv(migrationWindowProxiedEnv)
	if err := os.Setenv(migrationWindowProxiedEnv, "1"); err != nil {
		return nil, errors.Join(fmt.Errorf("forcing the proxied-native lane: %w", err), w.stopProvider())
	}
	migrationWindows.Lock()
	migrationWindows.active = w
	migrationWindows.Unlock()
	if w.started {
		if err := w.refuseForeignSessions(); err != nil {
			return nil, errors.Join(err, w.Close())
		}
	}
	return w, nil
}

// refuseForeignSessions opens the work store once, through the window, and
// refuses any session on its Dolt server but this process's own. A store not
// served by a Dolt server it can ask (a proxied, file or exec store) has
// nothing to list.
func (w *migrationSourceWindow) refuseForeignSessions() error {
	store, err := openInfraMigrationSource(w.cityPath)
	if err != nil {
		return fmt.Errorf("opening the work store in the maintenance window: %w", err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
	under, _, _ := unwrapBeadPolicyStore(store)
	ctx := context.Background()
	if _, served, _ := beads.ForeignDoltConnections(ctx, under); !served {
		return nil
	}
	// This process's own sessions are closed first. The open dialed the
	// server more than once (the library's fail-fast probe and no-database
	// check, bd's preflight in a subprocess), and the project-id read keeps
	// pooled sessions in internal/doltpool's process-lifetime registry, idle
	// for up to 20s; a one-shot verb holds none of those handles past its
	// call, and the next Open dials afresh. The server reaps a closed session
	// a moment later, so one still there once the settle has passed is
	// another client's.
	doltpool.Shutdown()
	deadline := time.Now().Add(migrationWindowSessionSettle)
	for {
		sessions, _, err := beads.ForeignDoltConnections(ctx, under)
		if err != nil {
			return fmt.Errorf("listing the sessions on the Dolt server this run started: %w", err)
		}
		if len(sessions) == 0 || !time.Now().Before(deadline) {
			return migrationForeignSessionRefusal(sessions)
		}
		time.Sleep(migrationWindowSessionSettle / 10)
	}
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

// Close ends the window: it restores GC_BEADS_PROXIED_NATIVE and, when the
// window started the provider, stops it while the caller still holds the
// controller lock. A provider the stop could not retire keeps its record, so
// the next window stops it.
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
	var envErr error
	if w.hadProxied {
		envErr = os.Setenv(migrationWindowProxiedEnv, w.prevProxied)
	} else {
		envErr = os.Unsetenv(migrationWindowProxiedEnv)
	}
	return errors.Join(envErr, w.stopProvider())
}

// closeReporting is Close for a defer, naming a failure on stderr.
func (w *migrationSourceWindow) closeReporting(logPrefix string, stderr io.Writer) {
	if err := w.Close(); err != nil {
		fmt.Fprintf(stderr, "%s: closing the maintenance window: %v. Stop the city's bead store with `%s` before starting the city\n", logPrefix, err, storageStopCommand) //nolint:errcheck // best-effort stderr
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

// migrationWindowNativeTransport is the native transport a store open of
// cityPath takes: the window's override while one is open, otherwise nil,
// which resolves the city's own setting.
func migrationWindowNativeTransport(cityPath string) *beads.NativeTransportMode {
	if activeMigrationWindow(cityPath) == nil {
		return nil
	}
	mode := migrationWindowTransport
	return &mode
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
// can: native Dolt (the proxied lane included), a library backend, the file
// store, and a generic exec store, whose protocol carries no payload.
func migrationSourceLaneRefusal(diag beads.BeadsDiagnostic) error {
	switch diag.Store {
	case beads.BeadsStoreNameNativeDoltStore, beads.BeadsStoreNameLibraryBackendStore, beads.BeadsStoreNameFileStore:
		return nil
	case beads.BeadsStoreNameExecStore:
		if diag.PreflightGate == "" {
			return nil
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
	data, err := json.Marshal(migrationWindowRecord{StartedProvider: true, PID: os.Getpid(), At: time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, migrationWindowRecordPath(cityPath), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("recording the maintenance window: %w", err)
	}
	return nil
}
