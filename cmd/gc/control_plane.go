package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/connect"
	"github.com/spf13/cobra"
)

// This file is worker-site mode (DES-CN D12, §4.4). A city directory holding
// `.gc/control-plane.toml` is a worker site: a box `gc connect` attached to a
// remote controller's ledgers. Every gc process there resolves Plane=Worker,
// builds no runtime provider that can act, dials no controller, supervisor
// or wake socket, and refuses to read controller-local state as a fallback.
// A city with no descriptor is Plane=Local and behaves exactly as before,
// apart from one stat per city path per process.

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

// attachedBindings maps each storage binding a descriptor ledger serves to
// the classes the ledger lists for it. The Local plane attaches nothing: it
// has no descriptor, and its bindings are opened as configured.
func (p controlPlane) attachedBindings() map[string][]string {
	if !p.Worker() {
		return nil
	}
	bindings := make(map[string][]string, len(p.descriptor.Ledgers))
	for _, ledger := range p.descriptor.Ledgers {
		binding, ok := connect.ScopeBinding(ledger.Scope)
		if !ok {
			continue
		}
		bindings[binding] = append(bindings[binding], ledger.Classes...)
	}
	return bindings
}

// attachesBinding reports whether a descriptor ledger serves binding.
func (p controlPlane) attachesBinding(binding string) bool {
	_, ok := p.attachedBindings()[binding]
	return ok
}

// workerSiteProcess is set once any city this process resolved is a worker
// site. The socket dial seam reads it, because a socket path does not say
// which city it belongs to.
var workerSiteProcess atomic.Bool

// errWorkerSiteNoController is returned instead of dialing a controller,
// supervisor or wake socket on a worker site. The box has none; the remote
// controller acts on durable ledger state at its next reconcile.
var errWorkerSiteNoController = errors.New("worker site: no controller or supervisor runs in this box; the remote controller acts on the recorded change at its next reconcile")

// errWorkerSiteRefusesController refuses a command that would run a city
// controller or the supervisor inside a box.
var errWorkerSiteRefusesController = errors.New("worker site: this city directory is attached to a remote controller (.gc/control-plane.toml), so it cannot run a controller or supervisor here")

// errWorkerSiteLocalState refuses a read of controller-local state on a
// worker site. what names the state, such as "events" or "sessions store".
type errWorkerSiteLocalState struct{ what string }

func (e errWorkerSiteLocalState) Error() string {
	return fmt.Sprintf("worker site: %s is controller-local state, which this box does not hold; read it through the controller", e.what)
}

// errControlPlaneDescriptor reports a `.gc/control-plane.toml` that exists but
// cannot be read or decoded. Every command except the exempt ones refuses it.
type errControlPlaneDescriptor struct {
	path   string
	reason string
}

func (e *errControlPlaneDescriptor) Error() string {
	return fmt.Sprintf("control-plane descriptor %s: %s (rerun gc connect to rewrite it)", e.path, e.reason)
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
// decoded. The answer is cached per process per path, and a Worker answer sets
// workerSiteProcess.
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
	if err == nil && plane.Worker() {
		workerSiteProcess.Store(true)
	}
	return plane, err
}

// forgetControlPlane drops cityPath's cached plane, so the next
// controlPlaneFor reads the file again. It never clears workerSiteProcess.
func forgetControlPlane(cityPath string) {
	controlPlanes.mu.Lock()
	defer controlPlanes.mu.Unlock()
	delete(controlPlanes.byPath, filepath.Clean(cityPath))
}

func readControlPlane(cityPath string) (controlPlane, error) {
	path := filepath.Join(cityPath, citylayout.ControlPlaneDescriptorPath)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return controlPlane{kind: planeLocal}, nil
	case err != nil:
		return controlPlane{}, &errControlPlaneDescriptor{path: path, reason: err.Error()}
	}
	descriptor, err := connect.DecodeDescriptor(data)
	if err != nil {
		return controlPlane{}, &errControlPlaneDescriptor{path: path, reason: err.Error()}
	}
	return controlPlane{kind: planeWorker, descriptor: descriptor}, nil
}

// onWorkerSite reports whether this process is a worker site or cityPath is
// one. A descriptor that does not decode is not a worker site here: the
// command gate has already refused it for every command but the exempt ones,
// which read the file themselves.
func onWorkerSite(cityPath string) bool {
	if workerSiteProcess.Load() {
		return true
	}
	if cityPath == "" {
		return false
	}
	plane, err := controlPlaneFor(cityPath)
	return err == nil && plane.Worker()
}

// workerSiteClassRefusal returns the refusal for class on a worker site when
// the binding it resolves to is one no descriptor ledger serves: a SQLite or
// file binding, or any binding the bundle did not carry. It returns nil on
// the Local plane and for an attached binding.
func workerSiteClassRefusal(cityPath, class, binding string) error {
	if cityPath == "" {
		return nil
	}
	plane, err := controlPlaneFor(cityPath)
	if err != nil || !plane.Worker() || plane.attachesBinding(binding) {
		return nil
	}
	return errWorkerSiteLocalState{what: class + " store"}
}

// dialUnixSocket dials a gc unix socket. Tests replace it with a counting
// dialer and must stay serial.
var dialUnixSocket = func(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}

// dialGCSocket dials a gc control socket (the supervisor's or the nudge wake
// socket). On a worker site it returns errWorkerSiteNoController without
// dialing.
func dialGCSocket(path string, timeout time.Duration) (net.Conn, error) {
	if workerSiteProcess.Load() {
		return nil, errWorkerSiteNoController
	}
	return dialUnixSocket(path, timeout)
}

// controlPlaneExemptCommands are the top-level commands that run with a
// malformed descriptor, because they read the file themselves.
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

// withControlPlane fills a local context's Plane. A descriptor that cannot
// be read or decoded fails the resolution unless the command is exempt.
func withControlPlane(ctx resolvedContext, err error) (resolvedContext, error) {
	if err != nil || ctx.Remote != nil || ctx.CityPath == "" {
		return ctx, err
	}
	plane, planeErr := controlPlaneFor(ctx.CityPath)
	if planeErr != nil {
		if controlPlaneGateExempt {
			return ctx, nil
		}
		return resolvedContext{}, planeErr
	}
	ctx.Plane = plane
	return ctx, nil
}

// refuseControllerOnWorkerSite returns errWorkerSiteRefusesController when
// cityPath, or this process, is a worker site.
func refuseControllerOnWorkerSite(cityPath string) error {
	if onWorkerSite(cityPath) {
		return errWorkerSiteRefusesController
	}
	return nil
}

// refuseSupervisorOnWorkerSite refuses `gc supervisor run` in a box. The
// supervisor is machine-wide, so the city it is judged by is the one this
// invocation's flags, environment or working directory resolve; finding no
// city is not a refusal.
func refuseSupervisorOnWorkerSite() error {
	if workerSiteProcess.Load() {
		return errWorkerSiteRefusesController
	}
	ctx, err := resolveContextMode(contextResolutionMode{advisory: true})
	var descErr *errControlPlaneDescriptor
	switch {
	case errors.As(err, &descErr):
		return err
	case err != nil:
		return nil
	case ctx.Plane.Worker():
		return errWorkerSiteRefusesController
	}
	return nil
}

// noteWorkerSitePoke reports a poke that found no controller on a worker
// site, after the caller's durable write, as one notice line on stderr. It
// returns false for any other error, which the caller still treats as a
// failure.
func noteWorkerSitePoke(stderr io.Writer, command string, err error) bool {
	if !errors.Is(err, errWorkerSiteNoController) {
		return false
	}
	fmt.Fprintf(stderr, "%s: worker site: no controller runs in this box; the remote controller acts on the recorded change at its next reconcile\n", command) //nolint:errcheck // best-effort stderr
	return true
}
