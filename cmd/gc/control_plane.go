package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/connect"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// This file is worker-site mode (DES-CN D12, §4.4). A city directory holding
// `.gc/control-plane.toml` is a worker site: a box `gc connect` attached to a
// remote controller's ledgers. Every gc process there resolves Plane=Worker,
// builds no runtime provider that can act, dials no controller, supervisor
// or wake socket through the seams below (internal/worker/AGENTS.md lists the
// direct dials CN-1b2 still converts), and refuses to read controller-local
// state as a fallback. A city with no descriptor is Plane=Local and behaves
// exactly as before, apart from one stat per city path per process.

// planeKind says which plane a city directory is on.
type planeKind int

const (
	// planeLocal is a city with no descriptor: the controller host's view.
	planeLocal planeKind = iota
	// planeWorker is a box `gc connect` attached to a remote controller.
	planeWorker
)

// controlPlane is a city directory's resolved plane. The zero value is Local.
type controlPlane struct {
	kind       planeKind
	descriptor connect.Descriptor
}

// Worker reports whether the plane is a worker site.
func (p controlPlane) Worker() bool { return p.kind == planeWorker }

// Descriptor returns the decoded descriptor of a worker site, or the zero
// Descriptor on the Local plane.
func (p controlPlane) Descriptor() connect.Descriptor { return p.descriptor }

// ledgerBindingFor returns the storage binding of the descriptor ledger that
// lists class (the reserved work binding for the city ledger), and false when
// no ledger lists it or the plane is Local. CN-1c's ledger classes are the
// authority: a binding is attached for a class only through the ledger the
// bundle put that class on.
func (p controlPlane) ledgerBindingFor(class string) (string, bool) {
	if !p.Worker() {
		return "", false
	}
	for _, ledger := range p.descriptor.Ledgers {
		if !slices.Contains(ledger.Classes, class) {
			continue
		}
		return connect.ScopeBinding(ledger.Scope)
	}
	return "", false
}

// classBindingMismatch returns the first class whose configured binding is
// not the binding of the descriptor ledger that lists it, and false when the
// box's [storage] assigns every class as the descriptor does.
func (p controlPlane) classBindingMismatch(bindingFor func(class string) string) (string, bool) {
	for _, class := range connect.StorageClasses() {
		if attached, ok := p.ledgerBindingFor(class); !ok || attached != bindingFor(class) {
			return class, true
		}
	}
	return "", false
}

// workerSiteProcess is set once this invocation's own context resolution
// finds a worker site, or a descriptor it cannot use. Only senders that know
// no city path read it (pokeSupervisor through dialGCSocket, and a seam handed
// an empty city path): a multi-city host such as the supervisor never resolves
// its managed cities that way, so one box-like city among them cannot turn the
// others into worker sites. An advisory resolution never sets it either (see
// withAdvisoryControlPlane).
var workerSiteProcess atomic.Bool

// errWorkerSiteNoController is returned instead of dialing a controller,
// supervisor or wake socket on a worker site. The box has none; the remote
// controller acts on durable ledger state at its next reconcile.
var errWorkerSiteNoController = errors.New("worker site: no controller or supervisor runs in this box; the remote controller acts on the recorded change at its next reconcile")

// errWorkerSiteRefusesController refuses a command that would run a city
// controller or the supervisor inside a box.
var errWorkerSiteRefusesController = errors.New("worker site: this city directory is attached to a remote controller (.gc/control-plane.toml), so it cannot run a controller or supervisor here")

// errWorkerSiteResetNeedsController refuses gc session reset on a worker site
// before it writes anything. Clearing a named session's respawn breaker goes
// over the controller socket, and rolling back a stuck create needs a runtime
// observation that the guard cannot make.
var errWorkerSiteResetNeedsController = errors.New("worker site: reset needs the controller, which does not run in this box; nothing was recorded, so run it where the controller runs")

// errWorkerSiteDeliveryNeedsController refuses gc session nudge and gc session
// submit on a worker site before they resolve the target or write anything.
// The guard types into no runtime, and the box's nudge queue is a local file
// the remote controller never reads, so no delivery mode reaches the session.
var errWorkerSiteDeliveryNeedsController = errors.New("worker site: delivering text to a session needs the controller's runtime or nudge queue, and this box holds neither; nothing was recorded, so send it as mail (gc mail send) or run it where the controller runs")

// errWorkerSiteDrainNeedsController refuses gc runtime drain and gc runtime
// undrain on a worker site before they resolve the target or write anything.
// The agent learns of a drain from the runtime drain flag, which only the
// controller's runtime holds. The row's operator mark only explains an ack
// (A11 reads it, and nothing else does), so a mark written here alone would
// drain nothing.
var errWorkerSiteDrainNeedsController = errors.New("worker site: drain and undrain set and clear the runtime drain flag the agent reads, and only the controller's runtime holds it; nothing was recorded, so run it where the controller runs")

// errWorkerSiteHandoffTargetNeedsController refuses gc handoff --target on a
// worker site before it resolves the target or sends its mail. A handoff to
// another session stops that session's runtime, which only the controller can
// do, and the guard cannot tell whether that runtime is live.
var errWorkerSiteHandoffTargetNeedsController = errors.New("worker site: a handoff to another session stops its runtime, which only the controller can do; nothing was recorded, so send the handoff as mail (gc mail send) or run it where the controller runs")

// workerSiteNudgeNotQueuedNotice opens what a courtesy nudge (gc sling's "Work
// slung", gc mail's new-mail notice) says on a worker site in place of
// queuing its text (workerSiteNudgeNotice). The routed bead or the mail is on
// the ledger either way, and the session's wake is the one signal the box
// records for it.
const workerSiteNudgeNotQueuedNotice = "worker site: the nudge text was not queued, because this box's nudge queue is a local file the remote controller does not read"

// workerSiteNudgeNotice is a courtesy nudge's worker-site notice once
// requestWorkerSiteNudgeWake reported outcome. Only a recorded wake reaches
// the remote controller, so only then does it promise that controller's
// reconcile.
func workerSiteNudgeNotice(outcome session.WakeRequestOutcome) string {
	if outcome == session.WakeRecorded {
		return workerSiteNudgeNotQueuedNotice + "; the remote controller acts on the recorded wake at its next reconcile"
	}
	return workerSiteNudgeNotQueuedNotice + "; no wake was recorded"
}

// errWorkerSiteNotifyNotDelivered is gc mail's new-mail notice on a worker
// site when it recorded no wake: the recipient is held, needs no wake, or has
// no session bead. The recipient's wake is the only notice a box leaves, so
// the recipient was not notified, and gc mail reports notified false. The
// mail is on the ledger either way.
var errWorkerSiteNotifyNotDelivered = errors.New(workerSiteNudgeNotQueuedNotice + "; no wake was recorded, so the recipient was not notified, and the mail waits on the ledger")

// errWorkerSiteLocalState refuses a read of controller-local state on a
// worker site. what names the state, such as "events" or "sessions store".
type errWorkerSiteLocalState struct{ what string }

func (e errWorkerSiteLocalState) Error() string {
	return fmt.Sprintf("worker site: %s is controller-local state, which this box does not hold; read it through the controller", e.what)
}

// errControlPlaneDescriptor reports a `.gc/control-plane.toml` that exists but
// cannot be read or decoded, or one gc cannot tell is absent. Every command
// except the exempt ones refuses it, and every worker-site seam treats it as a
// worker site: presence, not a decoded field, is what makes a box.
type errControlPlaneDescriptor struct {
	path   string
	reason string
	// present is false when gc could not stat the path, so it cannot tell
	// whether the file is there.
	present bool
}

func (e *errControlPlaneDescriptor) Error() string {
	if !e.present {
		return fmt.Sprintf("cannot check for the control-plane descriptor %s: %s; make the city's .gc directory readable so gc can tell whether this city directory is a worker site", e.path, e.reason)
	}
	return fmt.Sprintf("control-plane descriptor %s is present but unusable: %s; its presence marks this city directory a worker site attached to a remote controller, and only gc connect writes it (remove it only if this directory is not a worker site)", e.path, e.reason)
}

// isControlPlaneDescriptorError reports whether err carries an
// *errControlPlaneDescriptor.
func isControlPlaneDescriptorError(err error) bool {
	var descErr *errControlPlaneDescriptor
	return errors.As(err, &descErr)
}

// controlPlanes caches each city path's resolved plane, or the error
// resolving it, for the life of the process.
var controlPlanes struct {
	mu     sync.Mutex
	byPath map[string]controlPlaneResult
}

type controlPlaneResult struct {
	plane controlPlane
	err   error
}

// controlPlaneFor returns cityPath's plane: Local when it holds no
// `.gc/control-plane.toml`, Worker with the strictly decoded descriptor when
// it does, and an *errControlPlaneDescriptor when the file cannot be read or
// decoded. The answer is cached per process per path. It never sets
// workerSiteProcess: a lookup made for one city by a multi-city host says
// nothing about the process (see markInvocationPlane).
func controlPlaneFor(cityPath string) (controlPlane, error) {
	key := filepath.Clean(cityPath)
	controlPlanes.mu.Lock()
	defer controlPlanes.mu.Unlock()
	if cached, ok := controlPlanes.byPath[key]; ok {
		return cached.plane, cached.err
	}
	plane, err := readControlPlane(key)
	if controlPlanes.byPath == nil {
		controlPlanes.byPath = make(map[string]controlPlaneResult)
	}
	controlPlanes.byPath[key] = controlPlaneResult{plane: plane, err: err}
	return plane, err
}

// forgetControlPlane drops cityPath's cached plane, so the next
// controlPlaneFor reads the file again. It never clears workerSiteProcess.
func forgetControlPlane(cityPath string) {
	controlPlanes.mu.Lock()
	defer controlPlanes.mu.Unlock()
	delete(controlPlanes.byPath, filepath.Clean(cityPath))
}

// readControlPlane stats the descriptor first, so a path gc cannot check is
// told apart from a file that is there but unusable. A `.gc` that is not a
// directory (ENOTDIR) cannot hold the file, so it is absent.
func readControlPlane(cityPath string) (controlPlane, error) {
	path := filepath.Join(cityPath, citylayout.ControlPlaneDescriptorPath)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return controlPlane{kind: planeLocal}, nil
		}
		return controlPlane{}, &errControlPlaneDescriptor{path: path, reason: err.Error()}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return controlPlane{}, &errControlPlaneDescriptor{path: path, reason: err.Error(), present: true}
	}
	descriptor, err := connect.DecodeDescriptor(data)
	if err != nil {
		return controlPlane{}, &errControlPlaneDescriptor{path: path, reason: err.Error(), present: true}
	}
	return controlPlane{kind: planeWorker, descriptor: descriptor}, nil
}

// onWorkerSite reports whether cityPath is a worker site, deciding from that
// city's own descriptor; with no city path it reports whether this
// invocation resolved one. It fails closed: a descriptor that cannot be used
// answers true, so a command that never reached the gate (or an exempt one)
// still cannot act on a runtime or dial a socket.
func onWorkerSite(cityPath string) bool {
	if cityPath == "" {
		return workerSiteProcess.Load()
	}
	plane, err := controlPlaneFor(cityPath)
	return err != nil || plane.Worker()
}

// workerSiteClassRefusal returns the refusal for class on a worker site unless
// the binding it resolves to has the name of the descriptor ledger binding
// that lists the class. Only the name is compared, not the provider or the
// endpoint: a box binding of that name opens whatever its own [storage]
// configures. A descriptor that cannot be used is itself the refusal. It
// returns nil on the Local plane.
func workerSiteClassRefusal(cityPath, class, binding string) error {
	if cityPath == "" {
		return nil
	}
	plane, err := controlPlaneFor(cityPath)
	switch {
	case err != nil:
		return err
	case !plane.Worker():
		return nil
	}
	if attached, ok := plane.ledgerBindingFor(class); ok && attached == binding {
		return nil
	}
	return errWorkerSiteLocalState{what: class + " store"}
}

// workerSiteStorageRefusal is the one-shot storage funnel's worker-site
// verdict, taken before any binding is opened or created: on a worker site
// whose [storage] assigns some class to another binding than the descriptor
// ledger listing it, every infrastructure class refuses (a standing refusal,
// so work ids still resolve to work). A descriptor that cannot be used refuses
// the same way. It returns nil on the Local plane and when the box's
// [storage] agrees with the descriptor.
func workerSiteStorageRefusal(cityPath string, storage config.StorageConfig) error {
	plane, err := controlPlaneFor(cityPath)
	switch {
	case err != nil:
		return err
	case !plane.Worker():
		return nil
	}
	class, mismatch := plane.classBindingMismatch(func(class string) string {
		return storage.Classes.BindingFor(config.StorageClass(class))
	})
	if !mismatch {
		return nil
	}
	return errWorkerSiteLocalState{what: class + " store"}
}

// dialUnixSocket dials a gc unix socket. Tests replace it with a counting
// dialer and must stay serial.
var dialUnixSocket = func(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}

// dialGCSocket dials a gc socket that belongs to no one city (the
// supervisor's). When this invocation resolved a worker site it returns
// errWorkerSiteNoController without dialing.
func dialGCSocket(path string, timeout time.Duration) (net.Conn, error) {
	if workerSiteProcess.Load() {
		return nil, errWorkerSiteNoController
	}
	return dialUnixSocket(path, timeout)
}

// dialCitySocket dials one city's gc socket (its nudge wake socket, or its
// controller socket for gc stop), deciding from that city's own plane: when
// cityPath is a worker site it returns errWorkerSiteNoController without
// dialing, and a Local city's socket is dialed whichever city the invocation
// resolved. With no city path it decides from the invocation (onWorkerSite).
func dialCitySocket(cityPath, path string, timeout time.Duration) (net.Conn, error) {
	if onWorkerSite(cityPath) {
		return nil, errWorkerSiteNoController
	}
	return dialUnixSocket(path, timeout)
}

// controlPlaneExemptCommands are the top-level commands that run with a
// malformed descriptor: the exemption spares them the descriptor error, not
// the fail-closed worker-site marking (fillControlPlane). doctor runs
// regardless of the descriptor so that a box can be diagnosed; it does not
// read the file itself. version and help need no city. connect, the
// descriptor's writer, arrives with CN-2; when it does, it gets a run-level
// case in TestControlPlaneGateExemptsOnlyTheCommandsThatReadTheFile.
var controlPlaneExemptCommands = map[string]bool{
	"connect": true,
	"doctor":  true,
	"version": true,
	"help":    true,
}

// controlPlaneGateExempt is set for the duration of one invocation of an
// exempt command; context resolution then resolves a city with a malformed
// descriptor on the Local plane instead of refusing it.
var controlPlaneGateExempt bool

// controlPlaneGateExemptFor reports whether args invoke an exempt command.
func controlPlaneGateExemptFor(root *cobra.Command, args []string) bool {
	target, _, err := root.Find(args)
	if err != nil || target == nil || target == root {
		return false
	}
	for target.Parent() != nil && target.Parent() != root {
		target = target.Parent()
	}
	return controlPlaneExemptCommands[target.Name()]
}

// withControlPlane fills a local context's Plane and marks the invocation
// (markInvocationPlane). A descriptor that cannot be read or decoded fails
// the resolution unless the command is exempt.
func withControlPlane(ctx resolvedContext, err error) (resolvedContext, error) {
	return fillControlPlane(ctx, err, markInvocationPlane)
}

// withAdvisoryControlPlane is withControlPlane for an advisory resolution
// (eager pack-command discovery): it reads the city's plane without marking
// the invocation. Discovery runs before cobra parses the command line, so the
// city it finds from the city environment, GC_DIR or the working directory
// need not be the city a --city flag targets, and marking it would make an
// invocation against a Local city refuse as a worker site. A descriptor it
// cannot use still fails the resolution, so discovery degrades to no city.
func withAdvisoryControlPlane(ctx resolvedContext, err error) (resolvedContext, error) {
	return fillControlPlane(ctx, err, controlPlaneFor)
}

// fillControlPlane fills a local context's Plane from planeOf. A descriptor
// that cannot be read or decoded fails the resolution unless the command is
// exempt. planeOf runs before the exemption is consulted, so withControlPlane
// marks an exempt invocation too: the exemption spares it only the refusal.
func fillControlPlane(ctx resolvedContext, err error, planeOf func(cityPath string) (controlPlane, error)) (resolvedContext, error) {
	if err != nil || ctx.Remote != nil || ctx.CityPath == "" {
		return ctx, err
	}
	plane, planeErr := planeOf(ctx.CityPath)
	if planeErr != nil {
		if controlPlaneGateExempt {
			return ctx, nil
		}
		return resolvedContext{}, planeErr
	}
	ctx.Plane = plane
	return ctx, nil
}

// gateControlPlaneAt is the command gate for a city a command found without
// the context resolver (its session environment, GC_DIR or the working
// directory): it marks the invocation and returns the descriptor error unless
// the command is exempt.
func gateControlPlaneAt(cityPath string) error {
	_, err := withControlPlane(resolvedContext{CityPath: cityPath}, nil)
	return err
}

// markInvocationPlane resolves the plane of the city this invocation runs
// against, and sets workerSiteProcess when it is a worker site or holds a
// descriptor that cannot be used (fail closed). Only the invocation's own
// context resolution calls it.
func markInvocationPlane(cityPath string) (controlPlane, error) {
	plane, err := controlPlaneFor(cityPath)
	if err != nil || plane.Worker() {
		workerSiteProcess.Store(true)
	}
	return plane, err
}

// refuseControllerOnWorkerSite returns errWorkerSiteRefusesController when
// cityPath is a worker site, and the descriptor error when its descriptor
// cannot be used.
func refuseControllerOnWorkerSite(cityPath string) error {
	if cityPath == "" {
		if workerSiteProcess.Load() {
			return errWorkerSiteRefusesController
		}
		return nil
	}
	plane, err := controlPlaneFor(cityPath)
	switch {
	case err != nil:
		return err
	case plane.Worker():
		return errWorkerSiteRefusesController
	}
	return nil
}

// refuseCityStartOnWorkerSite is refuseControllerOnWorkerSite for one start
// attempt of a long-lived host (the supervisor's per-city start). It drops
// cityPath's cached plane first, so a descriptor an operator has since added
// or removed decides this attempt instead of the plane cached at the first.
func refuseCityStartOnWorkerSite(cityPath string) error {
	forgetControlPlane(cityPath)
	return refuseControllerOnWorkerSite(cityPath)
}

// refuseSupervisorOnWorkerSite refuses `gc supervisor run`, `start` and
// `install` in a box. The supervisor is machine-wide, so the city it is
// judged by is the one this invocation's --city flag, city environment,
// GC_DIR or working directory finds; finding no city is not a refusal. A
// supervisor started with no city context (systemd delegation, a service
// unit) is judged per city instead: startOneCity refuses each worker-site
// city it is asked to run.
func refuseSupervisorOnWorkerSite() error {
	if workerSiteProcess.Load() {
		return errWorkerSiteRefusesController
	}
	// The local tiers only (flag, env, GC_DIR, cwd): a rig binding does not
	// change the plane, and resolving one reads the supervisor registry.
	cityPath, ok := probeLocalCity()
	if !ok {
		return nil
	}
	if _, err := markInvocationPlane(cityPath); err != nil {
		return err
	}
	return refuseControllerOnWorkerSite(cityPath)
}

// noteWorkerSitePoke reports a poke that found no controller on a worker
// site, after the caller's durable write, as one notice line on stderr. It
// returns false for any other error, which the caller still treats as a
// failure.
func noteWorkerSitePoke(stderr io.Writer, command string, err error) bool {
	if !errors.Is(err, errWorkerSiteNoController) {
		return false
	}
	fmt.Fprintf(stderr, "%s: %v\n", command, err) //nolint:errcheck // best-effort stderr
	return true
}

// noteDeferredSessionStart reports on stderr how the start of session row,
// deferred to the reconciler, will proceed after a poke that returned
// pokeErr, and returns whether a controller answered the poke. A worker site
// gets its notice; any other failure gets a warning naming the session and
// how to bring the supervisor up and, for a row the reconciler rolls back
// unless a controller starts it in time (rolledBackUnlessStarted), that limit.
func noteDeferredSessionStart(stderr io.Writer, command string, row session.Info, pokeErr error) bool {
	if pokeErr == nil {
		return true
	}
	if noteWorkerSitePoke(stderr, command, pokeErr) {
		return false
	}
	limit := ""
	if rolledBackUnlessStarted(row) {
		limit = ", and is rolled back if none has started it " + neverStartedRollbackLimit(row)
	}
	fmt.Fprintf(stderr, "%s: warning: poke failed: %v; session %s will start when the city's controller next reconciles%s (if the supervisor is stopped, run \"gc supervisor start\")\n", command, pokeErr, row.ID, limit) //nolint:errcheck // best-effort stderr
	return false
}

// rolledBackUnlessStarted reports whether the reconciler rolls row back when
// no controller starts it within pendingCreateNeverStartedTimeout: a pending
// create with no recorded start, which pendingCreateNeverStartedLeaseExpiredAt
// ages out from its create.
func rolledBackUnlessStarted(row session.Info) bool {
	return row.PendingCreateClaim && strings.TrimSpace(row.LastWokeAt) == ""
}

// neverStartedRollbackLimit names when the reconciler rolls back row, a
// pending create no controller has started (rolledBackUnlessStarted): "by"
// the deadline pendingCreateNeverStartedLeaseExpiredAt reads, its anchor plus
// pendingCreateNeverStartedTimeout, in RFC 3339. An Info that records no
// anchor is one this process built for a row it just created, whose stored
// CreatedAt is the anchor, so the timeout is counted from its creation.
func neverStartedRollbackLimit(row session.Info) string {
	anchor := pendingCreateNeverStartedAnchor(row)
	if anchor.IsZero() {
		return fmt.Sprintf("within %s of its creation", pendingCreateNeverStartedTimeout)
	}
	return "by " + anchor.Add(pendingCreateNeverStartedTimeout).UTC().Format(time.RFC3339)
}

// noteNotAttachingUntilSupervisor tells the operator, on w, that the session
// of row, whose start only a controller makes while none runs, is not
// attached, and how to start and attach it.
func noteNotAttachingUntilSupervisor(w io.Writer, row session.Info) {
	limit := ""
	if rolledBackUnlessStarted(row) {
		limit = " " + neverStartedRollbackLimit(row) + ", or the reconciler rolls this session back"
	}
	fmt.Fprintf(w, "No controller answered; not attaching. Start the supervisor (\"gc supervisor start\")%s; then attach with \"gc session attach %s\" once \"gc session list\" shows the session running.\n", limit, row.ID) //nolint:errcheck // best-effort output
}

// pendingStartAwaitsSupervisor reports whether row is a start only a
// controller makes, which none can make now: a start-pending row whose
// runtime sp does not run, in a city whose session starts wait for the
// supervisor (sessionStartAwaitsSupervisor). A command that would start that
// runtime in this process refuses instead, as gc session new does: the
// reconciler starts the session once the supervisor runs, from the provider
// environment managed sessions get rather than this process's. Only a
// start-pending row is read further, so only one costs a runtime probe and a
// controller ping.
func pendingStartAwaitsSupervisor(cityPath string, row session.Info, sp runtime.Provider) (bool, error) {
	if session.State(strings.TrimSpace(row.MetadataState)) != session.StateStartPending || sp.IsRunning(row.SessionName) {
		return false, nil
	}
	return sessionStartAwaitsSupervisor(cityPath)
}

// enqueueDeferredSessionStart asks the controller to reconcile session id
// after its deferred create, and returns the poke result its start proceeds
// under: nil when the probe poke taken before the create (probeErr) or this
// enqueue reached a controller. A failed probe still enqueues, since a
// controller may have come up since; a worker site has no controller to
// enqueue to.
func enqueueDeferredSessionStart(cityPath, id string, probeErr error) error {
	if errors.Is(probeErr, errWorkerSiteNoController) {
		return probeErr
	}
	if err := enqueueController(cityPath, reconcilekey.Session(id)); err != nil && probeErr != nil {
		return err
	}
	return nil
}

// controllerWillReconcile reports whether a controller will act on a durable
// session write after a poke that returned pokeErr: the poke reached one, or
// cityPath is a worker site, whose remote controller acts at its next
// reconcile and which no poke ever reaches.
func controllerWillReconcile(cityPath string, pokeErr error) bool {
	return pokeErr == nil || onWorkerSite(cityPath)
}
