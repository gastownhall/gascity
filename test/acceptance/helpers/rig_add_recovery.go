package acceptancehelpers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bd picks its Dolt backend port by binding a socket and closing it, so a
// foreign listener can take the port before the backend binds it. Dolt then
// logs "Port N already in use." and the proxy bd fronts it with tears down. On
// a fresh rig in the default managed proxied shape that failure is not about
// the code under test, and only that failure is retried here: once, with the
// proof logged, and never when the evidence is anything less than an exact
// conflict line in this rig's own server.log.

var (
	portConflictPattern = regexp.MustCompile(`^Port [0-9]+ already in use\.$`)
	providerInitPrefix  = "provider-owned beads init:"
	// The two provider-owned init failures a lost backend port produces: the
	// uow provider's ping, and the wait for the proxy's OS-assigned port.
	portCollisionSignatures = []string{
		"uow: ping db:",
		"timeout waiting for proxy to become ready on its OS-assigned port",
		"waiting for proxy on its OS-assigned port",
	}
	// Names that pin the beads endpoint. An operator who set one chose the
	// backend, so a port lost to a foreign listener is theirs to fix.
	endpointPinVars = []string{
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_HOST", "BEADS_DOLT_PORT",
		"BD_DOLT_HOST", "BD_DOLT_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT",
	}
)

// portCollisionProof returns the exact `Port N already in use.` line from this
// rig's own server.log section in out, when err and out together show a
// provider-owned init failure of the kind a lost backend port causes. roots are
// the spellings of the rig directory. Anything less is an error that says what
// was missing.
func portCollisionProof(out string, err error, roots []string) (string, error) {
	if err == nil {
		return "", errors.New("gc rig add reported no failure")
	}
	// The signature is read outside the log sections: a log can say anything.
	surrounding := out
	if i := strings.Index(out, bdInitDiagnosticsOpen); i >= 0 {
		surrounding = out[:i]
	}
	if !strings.Contains(surrounding, providerInitPrefix) || !containsAny(surrounding, portCollisionSignatures) {
		return "", errors.New("not a provider-owned init failure of the port-collision kind")
	}

	var own, foreign []BDInitDiagnostic
	for _, s := range ParseBDInitDiagnostics(out) {
		if filepath.Base(s.Path) != "server.log" {
			continue
		}
		if sectionInRoots(s.Path, roots) {
			own = append(own, s)
		} else {
			foreign = append(foreign, s)
		}
	}
	if len(own) == 0 {
		if len(foreign) > 0 {
			return "", fmt.Errorf("server.log section is not this rig's (%s)", foreign[0].Path)
		}
		return "", errors.New("no server.log diagnostic section in the output")
	}
	for _, s := range own {
		for _, line := range strings.Split(s.Body, "\n") {
			if portConflictPattern.MatchString(line) {
				return line, nil
			}
		}
	}
	return "", errors.New("server.log has no exact `Port N already in use.` line")
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// sectionInRoots reports whether path is the server.log under one of the rig
// directory spellings in roots.
func sectionInRoots(path string, roots []string) bool {
	for _, root := range roots {
		if path == filepath.Join(root, ".beads", "dolt", "server.log") {
			return true
		}
	}
	return false
}

// recoverableShape returns nil for the one shape a lost backend port may be
// retried on: a fresh rig in the default local managed proxied city, with no
// pre-created store, no other initializer, and no endpoint pinned in the
// topology's or the run's environment.
func recoverableShape(topo BeadsTopology, env []string) error {
	switch {
	case topo.LegacyInit:
		return errors.New("shape is initialized by a legacy gc")
	case topo.Upstream:
		return errors.New("shape uses an external upstream")
	case topo.Deferred:
		return errors.New("shape defers its store")
	case topo.Rig.DoltMode != "proxied-server" || topo.Rig.Owner != OwnerProvider || topo.Rig.Proxies != 1 || topo.Rig.ExternalUpstreamSidecar:
		return errors.New("shape is not the managed proxied local rig")
	case topo.InitArgs != nil && len(topo.InitArgs(nil)) > 0:
		return errors.New("shape selects its transport or target explicitly")
	}
	for name := range topo.Env {
		if isEndpointPin(name) {
			return fmt.Errorf("shape pins the endpoint through %s", name)
		}
	}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if isEndpointPin(name) {
			return fmt.Errorf("environment pins the endpoint through %s", name)
		}
	}
	return nil
}

func isEndpointPin(name string) bool {
	for _, pin := range endpointPinVars {
		if name == pin {
			return true
		}
	}
	return false
}

// RigAddRecovery runs one gc rig add and, on a proved lost backend port, runs
// it once more.
type RigAddRecovery struct {
	Topology BeadsTopology
	// Env is the run's environment (KEY=VALUE), read for endpoint pins.
	Env []string
	// RigDir is the rig directory being added.
	RigDir string
	// Add runs gc rig add and returns its combined output.
	Add func() (string, error)
	// StoreExists reports whether path exists.
	StoreExists func(path string) bool
	// Logf records the original failure and the proof.
	Logf func(format string, args ...any)
}

// Run performs the add. It retries exactly once, and only when the shape is
// recoverable, the rig had no .beads before the first attempt, the first
// attempt failed with a port-collision proof, and its rollback left no .beads
// behind. In every other case it returns the first attempt's output and error,
// and a failed retry returns the retry's.
func (r RigAddRecovery) Run() (string, error) {
	store := filepath.Join(r.RigDir, ".beads")
	existedBefore := r.StoreExists(store)

	out, err := r.Add()
	if err == nil || existedBefore {
		return out, err
	}
	if shapeErr := recoverableShape(r.Topology, r.Env); shapeErr != nil {
		return out, err
	}
	proof, proofErr := portCollisionProof(out, err, rigRoots(r.RigDir))
	if proofErr != nil || r.StoreExists(store) {
		return out, err
	}

	r.Logf("gc rig add failed on a lost backend port (%v); proof %q; recovering once by running it again\n%s", err, proof, out)
	return r.Add()
}

// rigRoots is the rig directory as given and, when different, as the
// filesystem resolves it, because bd reports whichever spelling it saw.
func rigRoots(dir string) []string {
	roots := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		roots = append(roots, resolved)
	}
	return roots
}

// RigAdd runs `gc rig add rigDir` in the city, recovering once from a proved
// lost backend port (see RigAddRecovery).
func (r *TopologyRun) RigAdd(t *testing.T, rigDir string) (string, error) {
	t.Helper()
	return RigAddRecovery{
		Topology:    r.Topology,
		Env:         r.Env.List(),
		RigDir:      rigDir,
		Add:         func() (string, error) { return r.GC("rig", "add", rigDir) },
		StoreExists: pathExists,
		Logf:        t.Logf,
	}.Run()
}
