package acceptancehelpers

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The init topology matrix.
//
// Gas City supports more than one way to bring a beads scope up, and the
// proxied-local default is only the newest of them. This file describes each
// supported shape once — how to initialize it, what must end up on disk, which
// processes must exist and who owns them — so a test can walk all of them
// instead of proving the default and hoping the rest still work.
//
// The shapes are AC-M of the beads-proxied-local-default design:
//
//	M1 proxied-local      the default: bd's proxy plus its own Dolt child
//	M2 direct-local       bd's own server-mode Dolt, no proxy
//	M3 direct-external    a Dolt server somebody else runs, reached directly.
//	                      Two front doors: the legacy --dolt-host alias (no
//	                      journal, canonical city endpoint) and the
//	                      transport/target selector (provider-owned).
//	M4 proxied-external   a local bd proxy fronting that external server
//	M5 legacy GC-managed  the pre-journal shape: gc runs the sql-server itself
//	M6 doltlite           the embedded engine; no Dolt process at all
//	M7 deferred           GC_DOLT=skip at init; gc start finishes the store
//
// M5 cannot be created by the gc under test — every fresh `gc init` journals
// its scope as provider-owned — so it is initialized with the gc binary named
// by GC_ACCEPTANCE_LEGACY_GC_BIN and driven with the new one from there on.

// ProcessOwner names who is expected to own a Dolt-family process.
type ProcessOwner string

const (
	// OwnerProvider is bd: a db-proxy-child, or a server-mode sql-server bd
	// started and records in .beads/dolt-server.pid.
	OwnerProvider ProcessOwner = "bd"
	// OwnerCity is gc's own managed sql-server under .gc/runtime/packs/dolt.
	OwnerCity ProcessOwner = "gc"
	// OwnerUpstream is a Dolt server the fixture itself started. gc must never
	// stop it, and the fixture is responsible for retiring it.
	OwnerUpstream ProcessOwner = "fixture"
	// OwnerNobody is a scope with no Dolt process of its own.
	OwnerNobody ProcessOwner = "none"
)

// ScopeShape is what a single scope — a city or one of its rigs — must look
// like on disk and in the process table once it is up.
type ScopeShape struct {
	// DoltMode is the metadata.json dolt_mode this scope must carry. Empty
	// means the scope has no Dolt binding at all (doltlite).
	DoltMode string
	// ForbiddenDoltMode is a mode this scope must never acquire. It exists for
	// doltlite, whose only observable failure is being quietly re-stamped with
	// the proxied default.
	ForbiddenDoltMode string
	// Sidecar requires .beads/proxied_server_client_info.json, and IdleTimeout
	// the value in it. GC-owned proxies are pinned resident (-1).
	Sidecar     bool
	IdleTimeout int
	// ExternalUpstreamSidecar requires the sidecar to name the external
	// upstream the fixture started.
	ExternalUpstreamSidecar bool
	// Journaled requires a ready .gc/scope-ownership.json record.
	Journaled bool
	// Proxies and Servers are the bd proxy children and sql-servers that must
	// be running under this scope's own root.
	Proxies int
	Servers int
	// ManagedDoltState requires gc's own .gc/runtime/packs/dolt/dolt-state.json.
	// Two owners for one process is the failure it catches.
	ManagedDoltState bool
	// Owner is who holds the scope's Dolt process, for the failure message.
	Owner ProcessOwner
	// EndpointOrigin, when set, is the gc.endpoint_origin the scope's
	// .beads/config.yaml must carry.
	EndpointOrigin string
}

// BeadsTopology is one supported way to initialize a Gas City beads scope.
type BeadsTopology struct {
	Name string
	Doc  string

	// Env is layered onto the shared topology environment for every command
	// this shape runs, init included.
	Env map[string]string
	// InitArgs are the `gc init` flags that select this shape. The upstream is
	// nil unless Upstream is set.
	InitArgs func(up *ExternalDolt) []string
	// LegacyInit initializes through GC_ACCEPTANCE_LEGACY_GC_BIN instead of
	// the gc under test.
	LegacyInit bool
	// Upstream asks the fixture to start a real external `dolt sql-server`
	// before init and to retire it after the scope is gone.
	Upstream bool
	// ProvisionUpstreamDatabase asks the fixture to create the hosted beads
	// database on that server before init, for the front doors that adopt a
	// database rather than create one.
	ProvisionUpstreamDatabase bool

	City ScopeShape
	Rig  ScopeShape

	// Deferred means `gc init` leaves no store behind and `gc start` creates
	// it. The shape assertions are made after start rather than after init.
	Deferred bool
	// PreStartDoctorGaps names the doctor checks this shape may legitimately
	// fail before `gc start` has run once. A store gc did not create carries
	// whoever's bead vocabulary made it until gc's lifecycle runs over it.
	// After start there are no allowances.
	PreStartDoctorGaps []string
	// DoctorGaps names checks this shape fails for a reason that predates this
	// work and is not this feature's to fix. Every entry needs a comment saying
	// what the limitation is; an unexplained entry is a suppressed failure.
	DoctorGaps []string
	// ExpectedTopologyWarnings maps a bead-topology check to the substring its
	// warning must contain for this shape. It is deliberately message-scoped: a
	// shape that is allowed to warn about one thing must still fail on any
	// other warning from the same check, or the allowance becomes a blindfold.
	ExpectedTopologyWarnings map[string]string
	// BeadFrontDoorRefusal pins a shape that cannot serve beads at all: `gc bd
	// create` must fail, and its message must contain this text. AC-M asks for
	// the typed outcome rather than a skip, so the limitation is recorded here
	// instead of being stepped around.
	BeadFrontDoorRefusal string
	// KnownStopLeak records a shape whose `gc init` starts a Dolt process that
	// `gc stop` never retires, with the reason. The fixture still kills it, so
	// the shape cannot poison the next one, but it does not fail the run for a
	// defect that predates this work — measured against a pre-journal gc, which
	// leaks the same process in the same place.
	KnownStopLeak string
	// NoStore marks a front door that creates no bead store at all. Such a
	// shape runs init, the typed front-door refusal, and the stop
	// postcondition, and stops there — `gc start` on a storeless city does not
	// produce a topology to measure, it produces a Dolt process started for a
	// city that has no owner for it, which is a finding in its own right and
	// not something to bake into a matrix expectation.
	NoStore bool
}

// ExternalDolt is a real `dolt sql-server` the fixture owns. gc must reach it
// and must never stop it.
type ExternalDolt struct {
	Host     string
	Port     string
	DataDir  string
	Database string
	// ProjectID is the beads project identity of the hosted database, set when
	// the fixture provisioned it. The legacy --dolt-host alias needs it: that
	// front door writes the identity handshake itself rather than letting bd
	// resolve one.
	ProjectID string

	cmd     *exec.Cmd
	stopped bool
}

// Addr is the host:port a client dials.
func (e *ExternalDolt) Addr() string { return net.JoinHostPort(e.Host, e.Port) }

// Stop retires the upstream. It is idempotent: the fixture calls it during
// cleanup and a test may call it earlier to prove an outage.
func (e *ExternalDolt) Stop() {
	if e == nil || e.stopped {
		return
	}
	e.stopped = true
	if e.cmd != nil && e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
		_ = e.cmd.Wait()
	}
}

const externalDoltStartupLimit = 60 * time.Second

// StartExternalDolt runs a `dolt sql-server` on a free loopback port under
// dataDir and waits for it to accept connections.
//
// The port is reserved with the usual listen-then-close probe. That race is
// accepted here as it is everywhere else in the suite: the readiness dial that
// follows is what actually decides the server came up.
func StartExternalDolt(t *testing.T, env *Env, dataDir, database string) *ExternalDolt {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("create external dolt data dir: %v", err)
	}
	port, err := reservePort()
	if err != nil {
		t.Fatalf("reserve external dolt port: %v", err)
	}
	logPath := filepath.Join(dataDir, "sql-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create external dolt log: %v", err)
	}

	cmd := exec.Command("dolt", "sql-server", "-H", "127.0.0.1", "-P", strconv.Itoa(port), "--data-dir", dataDir) //nolint:gosec // fixed argv, resolved through the test PATH
	cmd.Dir = dataDir
	cmd.Env = env.List()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start external dolt: %v", err)
	}
	up := &ExternalDolt{Host: "127.0.0.1", Port: strconv.Itoa(port), DataDir: dataDir, Database: database, cmd: cmd}
	t.Cleanup(func() {
		up.Stop()
		_ = logFile.Close()
	})

	deadline := time.Now().Add(externalDoltStartupLimit)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", up.Addr(), 250*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return up
		}
		time.Sleep(100 * time.Millisecond)
	}
	log, _ := os.ReadFile(logPath)
	t.Fatalf("external dolt did not accept connections on %s within %s:\n%s", up.Addr(), externalDoltStartupLimit, log)
	return nil
}

// ProvisionBeadsDatabase creates the hosted beads database on the upstream the
// way an operator does — `bd init --server` from a throwaway workspace — and
// records its project identity.
//
// It exists because the legacy --dolt-host front door binds a city to a
// database somebody else operates; gc does not create one, and pointing that
// flag at a server with no beads schema is a different test.
func (e *ExternalDolt) ProvisionBeadsDatabase(t *testing.T, env *Env, bdPath, workspace, prefix string) {
	t.Helper()
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("create provisioning workspace: %v", err)
	}
	cmd := exec.Command(bdPath, "init", "--server", //nolint:gosec // resolved test binary
		"--server-host", e.Host, "--server-port", e.Port,
		"--database", e.Database, "-p", prefix,
		"--skip-hooks", "--skip-agents", "--quiet", "--non-interactive", workspace)
	cmd.Dir = workspace
	cmd.Env = append(env.List(), "BEADS_DIR="+filepath.Join(workspace, ".beads"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provision beads database %q on %s: %v\n%s", e.Database, e.Addr(), err, out)
	}
	data, err := os.ReadFile(filepath.Join(workspace, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read provisioned metadata: %v", err)
	}
	var identity struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		t.Fatalf("parse provisioned project identity: %v\n%s", err, data)
	}
	if strings.TrimSpace(identity.ProjectID) == "" {
		t.Fatalf("provisioned database has no project_id:\n%s", data)
	}
	e.ProjectID = identity.ProjectID
}

// BeadsTopologies returns the matrix in the order AC-M lists it.
func BeadsTopologies() []BeadsTopology {
	proxiedLocalScope := ScopeShape{
		DoltMode: "proxied-server", Sidecar: true, IdleTimeout: -1,
		Journaled: true, Proxies: 1, Servers: 1, Owner: OwnerProvider,
	}
	directLocalScope := ScopeShape{
		DoltMode: "server", Journaled: true, Proxies: 0, Servers: 1, Owner: OwnerProvider,
	}
	directExternalScope := ScopeShape{
		DoltMode: "server", Journaled: true, Proxies: 0, Servers: 0, Owner: OwnerUpstream,
	}
	// A scope bd initialized carries the project identity bd minted, in
	// metadata.json. gc's preflight confirms identity with a direct SQL probe
	// for `metadata._project_id`, a row rc.2's `bd init --server` does not
	// write, so the check cannot confirm what it is asked to confirm and the
	// scope stays on the bd CLI front door.
	//
	// That is the same place every proxied scope lives by design (decision D2),
	// where the proxied_provider gate reports it as expected rather than
	// degraded. The direct transports have no equivalent yet, which is a
	// reporting gap worth closing separately — not a store that does not work.
	bdOwnedDirectStoreWarning := map[string]string{"beads-store": "BdStore fallback"}
	return []BeadsTopology{
		{
			Name:     "M1-proxied-local",
			Doc:      "the default: no selector at all, bd owns a proxy and its Dolt child",
			City:     proxiedLocalScope,
			Rig:      proxiedLocalScope,
			InitArgs: func(*ExternalDolt) []string { return nil },
		},
		{
			Name:                     "M2-direct-local",
			Doc:                      "the documented escape hatch: bd owns a server-mode Dolt, no proxy",
			City:                     directLocalScope,
			Rig:                      directLocalScope,
			ExpectedTopologyWarnings: bdOwnedDirectStoreWarning,
			InitArgs: func(*ExternalDolt) []string {
				return []string{"--beads-transport", "direct", "--beads-target", "local"}
			},
		},
		{
			Name:                      "M3a-direct-external-alias",
			Doc:                       "the legacy --dolt-host alias: a canonical city endpoint over a database somebody else provisioned",
			Upstream:                  true,
			ProvisionUpstreamDatabase: true,
			// The adopted store carries the bead vocabulary of the bd that
			// created it, not gc's, until gc's lifecycle has run over it once.
			PreStartDoctorGaps: []string{"custom-types:city"},
			City: ScopeShape{
				DoltMode: "server", Journaled: false, Proxies: 0, Servers: 0,
				Owner: OwnerUpstream, EndpointOrigin: "city_canonical",
			},
			Rig: ScopeShape{
				DoltMode: "server", Journaled: false, Proxies: 0, Servers: 0,
				Owner: OwnerUpstream, EndpointOrigin: "inherited_city",
			},
			InitArgs: func(up *ExternalDolt) []string {
				return []string{
					"--dolt-host", up.Host, "--dolt-port", up.Port,
					"--dolt-database", up.Database, "--dolt-project-id", up.ProjectID,
				}
			},
		},
		{
			Name:                     "M3b-direct-external-selector",
			Doc:                      "the transport/target selector against the same upstream: provider-owned, journaled",
			Upstream:                 true,
			City:                     directExternalScope,
			Rig:                      directExternalScope,
			ExpectedTopologyWarnings: bdOwnedDirectStoreWarning,
			InitArgs: func(up *ExternalDolt) []string {
				return []string{
					"--beads-transport", "direct", "--beads-target", "external",
					"--dolt-host", up.Host, "--dolt-port", up.Port, "--dolt-database", up.Database,
				}
			},
		},
		{
			Name:     "M4-proxied-external",
			Doc:      "a local bd proxy fronting the external server: the proxy is ours, the data is not",
			Upstream: true,
			City: ScopeShape{
				DoltMode: "proxied-server", Sidecar: true, IdleTimeout: -1,
				ExternalUpstreamSidecar: true,
				Journaled:               true, Proxies: 1, Servers: 0, Owner: OwnerProvider,
			},
			Rig: ScopeShape{
				DoltMode: "proxied-server", Sidecar: true, IdleTimeout: -1,
				ExternalUpstreamSidecar: true,
				Journaled:               true, Proxies: 1, Servers: 0, Owner: OwnerProvider,
			},
			InitArgs: func(up *ExternalDolt) []string {
				return []string{
					"--beads-transport", "proxied", "--beads-target", "external",
					"--dolt-host", up.Host, "--dolt-port", up.Port, "--dolt-database", up.Database,
				}
			},
		},
		{
			Name:       "M5-legacy-gc-managed",
			Doc:        "the shape that exists in the field: gc runs the sql-server, no ownership journal",
			LegacyInit: true,
			// The store was created by an older gc and carries that gc's bead
			// vocabulary. `gc start` re-canonicalizes the scope's config.yaml
			// but never writes the store's own custom_types table, so an
			// upgraded city keeps reporting the type the newer gc added until
			// an operator runs `gc doctor --fix`, which registers it. That is
			// the upgrade path as it stands rather than something this feature
			// broke — a gap worth closing, recorded here until it is.
			DoctorGaps: []string{"custom-types:city"},
			City: ScopeShape{
				DoltMode: "server", Journaled: false, Proxies: 0, Servers: 1,
				ManagedDoltState: true, Owner: OwnerCity, EndpointOrigin: "managed_city",
			},
			Rig: ScopeShape{
				DoltMode: "server", Journaled: false, Proxies: 0, Servers: 0,
				Owner: OwnerCity, EndpointOrigin: "inherited_city",
			},
			InitArgs: func(*ExternalDolt) []string { return nil },
		},
		{
			Name: "M6-doltlite",
			Doc:  "the embedded engine: no server, no proxy, and no proxied binding stamped over it",
			Env:  map[string]string{"GC_BEADS_BACKEND": "doltlite"},
			// What this shape holds is an init-time property: the proxied-local
			// default must never reach a doltlite city. It did, and the ownership
			// classifier then read the city as a bd-owned proxied scope and let bd
			// raise a proxy and a Dolt child over a workspace that is supposed to
			// have neither.
			//
			// It stops there because the rest of the list has nothing to act on.
			// GC_BEADS_BACKEND=doltlite selects the backend for gc's own
			// classification, but `gc init` never runs the adapter's doltlite init
			// op, so no embedded store is created and every bead read reaches for a
			// Dolt server that is not there — equally true on main, measured
			// against a pre-journal gc. The front-door refusal below is asserted
			// rather than skipped; `gc start` is not, because starting a storeless
			// city does not produce a topology to measure, it produces a Dolt
			// process nothing owns and `gc stop` does not retire.
			NoStore:              true,
			BeadFrontDoorRefusal: "failed to open database",
			KnownStopLeak: "`gc init` on a doltlite city starts a gc-managed sql-server under " +
				".gc/runtime/packs/dolt, and `gc stop` does not retire it: the city is not " +
				"classified as owning a managed-Dolt lifecycle, so the stop path never reaches " +
				"the process the init path started. A pre-journal gc leaks the same process in " +
				"the same place, so this is recorded rather than asserted",
			City: ScopeShape{
				ForbiddenDoltMode: "proxied-server", Journaled: false,
				Proxies: 0, Servers: 0, Owner: OwnerNobody,
			},
			InitArgs: func(*ExternalDolt) []string { return nil },
		},
		{
			Name:     "M7-deferred-init",
			Doc:      "GC_DOLT=skip: init records the intent and creates nothing; start finishes the store",
			Env:      map[string]string{"GC_DOLT": "skip"},
			Deferred: true,
			City:     proxiedLocalScope,
			Rig:      proxiedLocalScope,
			InitArgs: func(*ExternalDolt) []string { return nil },
		},
	}
}

// TopologyRun is one shape, initialized and ready to drive.
type TopologyRun struct {
	Topology BeadsTopology
	Env      *Env
	City     *City
	// Root is the directory every scope and every upstream of this shape lives
	// under. Cleanup sweeps the process table for it, so nothing this shape
	// starts may live outside it.
	Root string
	// Upstream is the external Dolt server the fixture started, or nil.
	Upstream *ExternalDolt
	// LegacyGCPath is the old gc binary, empty unless the shape needs one.
	LegacyGCPath string
}

// ForEachTopology runs fn as a subtest for every shape in the matrix, each with
// its own city root, its own upstream where the shape needs one, and a cleanup
// that refuses to let a Dolt process outlive the subtest.
//
// The whole matrix needs a real bd with proxied-server support and a real dolt;
// without either it skips with a message naming the variable to set. Only the
// legacy shape needs GC_ACCEPTANCE_LEGACY_GC_BIN, so only that shape skips when
// it is unset.
func ForEachTopology(t *testing.T, base *Env, fn func(t *testing.T, run *TopologyRun)) {
	t.Helper()
	bdPath, doltPath := RequireTopologyTooling(t)
	for _, topo := range BeadsTopologies() {
		t.Run(topo.Name, func(t *testing.T) {
			run := StartTopology(t, base, topo, bdPath, doltPath)
			fn(t, run)
		})
	}
}

// RequireTopologyTooling resolves the bd and dolt the matrix needs, or skips.
func RequireTopologyTooling(t *testing.T) (bdPath, doltPath string) {
	t.Helper()
	bdPath = FindBD()
	if bdPath == "" {
		t.Skip("bd not available; set GC_ACCEPTANCE_BD_BIN to a bd >= 1.3.0")
	}
	out, err := exec.Command(bdPath, "init", "--help").CombinedOutput() //nolint:gosec // resolved test binary
	if err != nil || !strings.Contains(string(out), "--proxied-server") {
		t.Skipf("bd at %s has no proxied-server support; set GC_ACCEPTANCE_BD_BIN to a bd >= 1.3.0", bdPath)
	}
	doltPath, err = exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	return bdPath, doltPath
}

// LegacyGCBinary returns the pre-journal gc binary, or "" when unset.
func LegacyGCBinary() string {
	override := strings.TrimSpace(os.Getenv("GC_ACCEPTANCE_LEGACY_GC_BIN"))
	if override == "" {
		return ""
	}
	bin, err := filepath.Abs(override)
	if err != nil {
		return ""
	}
	if info, statErr := os.Stat(bin); statErr != nil || info.IsDir() {
		return ""
	}
	return bin
}

// TopologyEnv is the environment every shape shares: the Tier A harness with a
// real Dolt-backed bd store instead of the file store, and this run's bd and
// dolt ahead of any host copies but behind the hermetic provider doubles, which
// must stay first.
func TopologyEnv(t *testing.T, base *Env, root, bdPath, doltPath string) *Env {
	t.Helper()
	linkDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"bd": bdPath, "dolt": doltPath} {
		if err := os.Symlink(target, filepath.Join(linkDir, name)); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
	}
	env := base.Clone()
	entries := filepath.SplitList(env.Get("PATH"))
	path := append([]string{entries[0], linkDir}, entries[1:]...)
	return env.With("PATH", strings.Join(path, string(os.PathListSeparator))).
		With("GC_BEADS", "bd").
		Without("GC_DOLT").
		Without("GC_BEADS_BACKEND")
}

// StartTopology builds one shape: its root, its environment, its upstream if it
// has one, and the `gc init` that brings the city up. It registers the cleanup
// that proves nothing survived.
func StartTopology(t *testing.T, base *Env, topo BeadsTopology, bdPath, doltPath string) *TopologyRun {
	t.Helper()
	root := TempDir(t)
	env := TopologyEnv(t, base, root, bdPath, doltPath)
	for k, v := range topo.Env {
		env.With(k, v)
	}

	run := &TopologyRun{Topology: topo, Env: env, Root: root}
	if topo.LegacyInit {
		run.LegacyGCPath = LegacyGCBinary()
		if run.LegacyGCPath == "" {
			t.Skip("no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal")
		}
	}
	if topo.Upstream {
		run.Upstream = StartExternalDolt(t, env, filepath.Join(root, "upstream"), topologyDatabaseName(topo))
		if topo.ProvisionUpstreamDatabase {
			run.Upstream.ProvisionBeadsDatabase(t, env, bdPath, filepath.Join(root, "provision"), "hosted")
		}
	}

	run.City = NewCityAt(t, env, filepath.Join(root, "city"))
	// Registered before init: a half-built scope leaks the same way a complete
	// one does, and init is exactly where a shape is most likely to die.
	t.Cleanup(func() { run.assertNothingSurvived(t) })
	run.initCity(t)
	run.widenStartReadyTimeout(t)
	return run
}

// widenStartReadyTimeout raises the city's startup budget.
//
// The matrix runs eight cities' worth of real Dolt lifecycle back to back on
// one box, and every bead read on a bd-owned store costs a fork. The default
// five minutes is a sensible product default and a bad test assumption: a start
// that misses it here is a statement about the machine, not about the topology
// under test. The composition is untouched — this widens a timeout, it does not
// remove anything the shape has to start.
func (r *TopologyRun) widenStartReadyTimeout(t *testing.T) {
	t.Helper()
	path := filepath.Join(r.City.Dir, "city.toml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	widened := string(existing) + "\n[daemon]\nstart_ready_timeout = \"15m\"\n"
	if err := os.WriteFile(path, []byte(widened), 0o644); err != nil {
		t.Fatalf("widen start-ready timeout: %v", err)
	}
}

// Start brings the city up under the isolated supervisor, reporting against the
// subtest that asked for it.
//
// City.StartWithSupervisor reports against the testing.T the city was built
// with, which for this fixture is the parent — a failure inside a subtest then
// calls FailNow on the wrong goroutine and go test complains about a Goexit
// rather than showing the failure.
func (r *TopologyRun) Start(t *testing.T) {
	t.Helper()
	RunGC(r.Env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best effort: a stale supervisor must not outlive the previous step
	RunGC(r.Env, r.City.Dir, "stop", r.City.Dir)     //nolint:errcheck // best effort
	if out, err := RunGC(r.Env, r.City.Dir, "start", r.City.Dir); err != nil {
		t.Fatalf("gc start on a %s city: %v\n%s", r.Topology.Name, err, out)
	}
}

func topologyDatabaseName(topo BeadsTopology) string {
	name := strings.ToLower(topo.Name)
	name = strings.NewReplacer("-", "_", ".", "_").Replace(name)
	return name + "_db"
}

func (r *TopologyRun) initCity(t *testing.T) {
	t.Helper()
	args := []string{"init", "--skip-provider-readiness", "--no-start", "--provider", "claude"}
	if r.Topology.InitArgs != nil {
		args = append(args, r.Topology.InitArgs(r.Upstream)...)
	}
	args = append(args, r.City.Dir)

	var out string
	var err error
	// The legacy shape is the one scope the gc under test cannot create.
	if legacy := r.LegacyGCPath; legacy != "" {
		out, err = r.runBinary(legacy, "", args...)
	} else {
		out, err = RunGC(r.Env, "", args...)
	}
	if err != nil {
		t.Fatalf("gc init for %s: %v\n%s", r.Topology.Name, err, out)
	}
}

// RigWorkspace creates a git repo under this run's root, ready for `gc rig add`.
// It lives under Root so the cleanup sweep sees anything started inside it.
func (r *TopologyRun) RigWorkspace(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(r.Root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create rig workspace: %v", err)
	}
	for _, args := range [][]string{
		{"init", dir},
		{"-C", dir, "config", "user.email", "test@test.com"},
		{"-C", dir, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec // fixed argv
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", dir, "add", "."}, {"-C", dir, "commit", "-m", "init"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec // fixed argv
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := EnsureClaudeProjectState(r.Env, dir); err != nil {
		t.Fatalf("seed Claude state for %s: %v", dir, err)
	}
	return dir
}

func (r *TopologyRun) runBinary(bin, dir string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...) //nolint:gosec // resolved test binary
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = r.Env.List()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// GC runs the gc under test in the city directory.
func (r *TopologyRun) GC(args ...string) (string, error) {
	return RunGC(r.Env, r.City.Dir, args...)
}

// Stop retires this shape the way an operator does, in the order the design
// requires.
//
// `gc stop` runs first, while the supervisor is still up: it needs a live
// supervisor to drop the city's registration, and stopping the supervisor
// first makes it restore the registration and exit non-zero. The supervisor
// goes next, because on rc.2's proxied path any bd read restarts the proxy and
// its Dolt child, so a sampler outliving the store undoes the stop. Then `gc
// stop` again, to retire whatever that last sample revived — which is what
// "stop is re-runnable" is for.
func (r *TopologyRun) Stop() (string, error) {
	first, err := RunGC(r.Env, r.City.Dir, "stop", r.City.Dir)
	if err != nil {
		return first, err
	}
	// "supervisor is not running" is the answer a second stop gets, and it is
	// success: retiring what is already retired is the whole point of a
	// re-runnable stop.
	if out, supervisorErr := RunGC(r.Env, "", "supervisor", "stop", "--wait"); supervisorErr != nil && !strings.Contains(out, "supervisor is not running") {
		return first + "\n" + out, supervisorErr
	}
	second, err := RunGC(r.Env, r.City.Dir, "stop", r.City.Dir)
	return first + "\n" + second, err
}

// assertNothingSurvived is the cleanup contract: stop the city, retire the
// fixture's own upstream, then kill anything left whose argv names this run's
// root and fail if there was anything to kill.
func (r *TopologyRun) assertNothingSurvived(t *testing.T) {
	RunGC(r.Env, r.City.Dir, "stop", r.City.Dir)       //nolint:errcheck // best effort
	RunGC(r.Env, "", "supervisor", "stop", "--wait")   //nolint:errcheck // best effort
	RunGC(r.Env, r.City.Dir, "stop", r.City.Dir)       //nolint:errcheck // best effort
	RunGC(r.Env, r.City.Dir, "unregister", r.City.Dir) //nolint:errcheck // best effort
	r.Upstream.Stop()

	leaked := WaitForNoDoltProcesses(t, r.Root, 20*time.Second)
	if len(leaked) == 0 {
		return
	}
	for _, pid := range doltProcessPIDs(t, r.Root) {
		_ = syscallKill(pid)
	}
	if reason := r.Topology.KnownStopLeak; reason != "" {
		t.Logf("%s left Dolt processes behind under %s, which is the recorded defect — %s:\n%s",
			r.Topology.Name, r.Root, reason, strings.Join(leaked, "\n"))
		return
	}
	t.Errorf("%s left Dolt processes behind under %s:\n%s", r.Topology.Name, r.Root, strings.Join(leaked, "\n"))
}

// DoltProcessesUnder returns the command lines of every live bd proxy or dolt
// sql-server whose argv names root. It reads the process table rather than a
// pid file because the leak worth catching is a process whose record is gone.
func DoltProcessesUnder(t *testing.T, root string) []string {
	t.Helper()
	lines, _ := doltProcessTable(t, root)
	return lines
}

func doltProcessPIDs(t *testing.T, root string) []int {
	t.Helper()
	_, pids := doltProcessTable(t, root)
	return pids
}

func doltProcessTable(t *testing.T, root string) ([]string, []int) {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("read process table: %v", err)
	}
	var (
		found []string
		pids  []int
	)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, root) {
			continue
		}
		if !strings.Contains(line, "db-proxy-child") && !strings.Contains(line, "sql-server") {
			continue
		}
		pidText, args, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, convErr := strconv.Atoi(pidText)
		if convErr != nil {
			continue
		}
		found = append(found, strings.TrimSpace(args))
		pids = append(pids, pid)
	}
	return found, pids
}

// WaitForNoDoltProcesses polls until nothing under root remains, or timeout.
func WaitForNoDoltProcesses(t *testing.T, root string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		last := DoltProcessesUnder(t, root)
		if len(last) == 0 || time.Now().After(deadline) {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ScopeArtifacts is the on-disk binding of one scope, read through the files bd
// and gc actually persist.
type ScopeArtifacts struct {
	Metadata BeadsMetadata
	// HasMetadata is false for a scope with no .beads/metadata.json: doltlite
	// cities and a deferred scope before `gc start`.
	HasMetadata bool
	Sidecar     ProxiedSidecar
	HasSidecar  bool
	// ConfigYAML is the raw .beads/config.yaml, or "" when absent.
	ConfigYAML string
	// ManagedDoltState is true when gc published its own runtime state for the
	// city this scope belongs to.
	ManagedDoltState bool
	Proxies          int
	Servers          int
	Processes        []string
}

// BeadsMetadata is the part of bd's .beads/metadata.json the matrix reads.
type BeadsMetadata struct {
	Backend          string `json:"backend"`
	Database         string `json:"database"`
	DoltMode         string `json:"dolt_mode"`
	DoltDatabase     string `json:"dolt_database"`
	DoltServerHost   string `json:"dolt_server_host"`
	DoltServerPort   int    `json:"dolt_server_port"`
	DoltServerSocket string `json:"dolt_server_socket"`
}

// ProxiedSidecar is bd's proxied_server_client_info.json.
type ProxiedSidecar struct {
	RootPath    string `json:"root_path"`
	IdleTimeout int    `json:"idle_timeout"`
	External    *struct {
		Host   string `json:"host"`
		Port   int    `json:"port"`
		Socket string `json:"socket"`
	} `json:"external"`
}

// ScopeOwnershipJournal is gc's .gc/scope-ownership.json.
type ScopeOwnershipJournal struct {
	Version int                          `json:"version"`
	Scopes  map[string]ScopeOwnershipRow `json:"scopes"`
}

// ScopeOwnershipRow is one journal entry.
type ScopeOwnershipRow struct {
	ScopePath      string `json:"scope_path"`
	LifecycleOwner string `json:"lifecycle_owner"`
	State          string `json:"state"`
	Intent         struct {
		Transport string `json:"transport"`
		Target    string `json:"target"`
	} `json:"intent"`
}

// ReadScopeArtifacts reads everything the matrix asserts about one scope.
func ReadScopeArtifacts(t *testing.T, cityRoot, scopeRoot string) ScopeArtifacts {
	t.Helper()
	var a ScopeArtifacts
	if data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "metadata.json")); err == nil {
		if err := json.Unmarshal(data, &a.Metadata); err != nil {
			t.Fatalf("parse %s metadata.json: %v\n%s", scopeRoot, err, data)
		}
		a.HasMetadata = true
	} else if !os.IsNotExist(err) {
		t.Fatalf("read %s metadata.json: %v", scopeRoot, err)
	}
	if data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "proxied_server_client_info.json")); err == nil {
		if err := json.Unmarshal(data, &a.Sidecar); err != nil {
			t.Fatalf("parse %s sidecar: %v\n%s", scopeRoot, err, data)
		}
		a.HasSidecar = true
	} else if !os.IsNotExist(err) {
		t.Fatalf("read %s sidecar: %v", scopeRoot, err)
	}
	if data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "config.yaml")); err == nil {
		a.ConfigYAML = string(data)
	}
	if _, err := os.Stat(filepath.Join(cityRoot, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); err == nil {
		a.ManagedDoltState = true
	}
	a.Processes = DoltProcessesUnder(t, scopeRoot)
	for _, p := range a.Processes {
		if strings.Contains(p, "db-proxy-child") {
			a.Proxies++
			continue
		}
		if strings.Contains(p, "sql-server") {
			a.Servers++
		}
	}
	return a
}

// ReadOwnershipJournal reads gc's ownership journal, reporting absence rather
// than failing: a shape that must not journal anything is a real expectation.
func ReadOwnershipJournal(t *testing.T, cityRoot string) (ScopeOwnershipJournal, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cityRoot, ".gc", "scope-ownership.json"))
	if os.IsNotExist(err) {
		return ScopeOwnershipJournal{}, false
	}
	if err != nil {
		t.Fatalf("read ownership journal: %v", err)
	}
	var journal ScopeOwnershipJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatalf("parse ownership journal: %v\n%s", err, data)
	}
	return journal, true
}

// sameTopologyScope reports whether two scope roots name the same directory.
func sameTopologyScope(a, b string) bool {
	resolve := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}

// AssertScopeShape checks one scope against the shape its topology promises.
func AssertScopeShape(t *testing.T, cityRoot, scopeRoot string, want ScopeShape, label string) {
	t.Helper()
	got := ReadScopeArtifacts(t, cityRoot, scopeRoot)

	switch {
	case want.DoltMode != "":
		if !got.HasMetadata {
			t.Errorf("%s has no .beads/metadata.json; want dolt_mode %q", label, want.DoltMode)
			break
		}
		if !strings.EqualFold(got.Metadata.DoltMode, want.DoltMode) {
			t.Errorf("%s dolt_mode = %q, want %q", label, got.Metadata.DoltMode, want.DoltMode)
		}
	case want.ForbiddenDoltMode != "":
		if got.HasMetadata && strings.EqualFold(got.Metadata.DoltMode, want.ForbiddenDoltMode) {
			t.Errorf("%s was stamped dolt_mode %q, which this shape must never acquire", label, got.Metadata.DoltMode)
		}
	}

	if want.Sidecar {
		if !got.HasSidecar {
			t.Errorf("%s has no proxied_server_client_info.json", label)
		} else {
			if got.Sidecar.IdleTimeout != want.IdleTimeout {
				t.Errorf("%s idle_timeout = %d, want %d", label, got.Sidecar.IdleTimeout, want.IdleTimeout)
			}
			if want.ExternalUpstreamSidecar && got.Sidecar.External == nil {
				t.Errorf("%s sidecar names no external upstream: %+v", label, got.Sidecar)
			}
			if !want.ExternalUpstreamSidecar && got.Sidecar.External != nil {
				t.Errorf("%s sidecar names an external upstream it should not have: %+v", label, got.Sidecar.External)
			}
		}
	} else if got.HasSidecar {
		t.Errorf("%s has a proxied sidecar it should not have: %+v", label, got.Sidecar)
	}

	if got.Proxies != want.Proxies || got.Servers != want.Servers {
		t.Errorf("%s topology = %d proxy / %d sql-server, want %d / %d (owner %s):\n%s",
			label, got.Proxies, got.Servers, want.Proxies, want.Servers, want.Owner,
			strings.Join(got.Processes, "\n"))
	}

	// gc's managed-Dolt runtime state is a city-level artifact — one file for
	// the one server a managed city runs, which its rigs share — so it is a
	// claim about the city, asserted once.
	if sameTopologyScope(cityRoot, scopeRoot) && got.ManagedDoltState != want.ManagedDoltState {
		if want.ManagedDoltState {
			t.Errorf("%s: gc published no managed-Dolt runtime state, but gc owns this shape's server", label)
		} else {
			t.Errorf("%s: gc published managed-Dolt runtime state for a scope it does not own — two owners for one process", label)
		}
	}

	if want.EndpointOrigin != "" && !strings.Contains(got.ConfigYAML, "gc.endpoint_origin: "+want.EndpointOrigin) {
		t.Errorf("%s canonical config does not carry gc.endpoint_origin %s:\n%s", label, want.EndpointOrigin, got.ConfigYAML)
	}
}

// AssertJournalState checks the ownership journal against a shape: a ready
// provider record for the scopes gc initialized, or no record at all.
func AssertJournalState(t *testing.T, cityRoot, key string, want ScopeShape, label string) {
	t.Helper()
	journal, present := ReadOwnershipJournal(t, cityRoot)
	entry, ok := journal.Scopes[key]
	if !want.Journaled {
		if present && ok {
			t.Errorf("%s was journaled %+v, but this shape has no provider-owned scopes", label, entry)
		}
		return
	}
	if !present {
		t.Errorf("%s has no ownership journal at all", label)
		return
	}
	if !ok {
		t.Errorf("%s missing from the ownership journal: %+v", label, journal.Scopes)
		return
	}
	if entry.LifecycleOwner != "provider" || entry.State != "ready" {
		t.Errorf("%s ownership = %+v, want provider/ready", label, entry)
	}
	if entry.Intent.Transport != "" || entry.Intent.Target != "" {
		t.Errorf("%s is ready but still carries intent %+v", label, entry.Intent)
	}
}

func syscallKill(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}
