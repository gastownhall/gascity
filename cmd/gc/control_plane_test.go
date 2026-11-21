package main

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/connect"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// resetControlPlaneForTest drops every cached plane and clears the
// worker-site flag, now and when the test ends, so no plane leaks between
// tests. Tests that call it must stay serial: the cache and the flag are
// process state.
func resetControlPlaneForTest(t *testing.T) {
	t.Helper()
	clearControlPlaneState()
	t.Cleanup(clearControlPlaneState)
}

func clearControlPlaneState() {
	controlPlanes.mu.Lock()
	controlPlanes.byPath = nil
	controlPlanes.mu.Unlock()
	workerSiteProcess.Store(false)
}

// testDescriptor returns a valid worker-site descriptor for cityDir whose
// city ledger serves every class, plus any binding ledgers named in
// bindingClasses (binding name to the classes it serves, which the city
// ledger then gives up).
func testDescriptor(cityDir string, bindingClasses map[string][]string) connect.Descriptor {
	moved := map[string]bool{}
	for _, classes := range bindingClasses {
		for _, class := range classes {
			moved[class] = true
		}
	}
	var cityClasses []string
	for _, class := range connect.StorageClasses() {
		if !moved[class] {
			cityClasses = append(cityClasses, class)
		}
	}
	d := connect.Descriptor{
		Schema:     connect.DescriptorSchema,
		Plane:      connect.PlaneWorker,
		City:       "box-city",
		SessionID:  "box-s1",
		Generation: 1,
		Contract:   connect.Contract,
		Credential: connect.CredentialFilePrefix + "/home/agent/.config/gascity/connect/box-city/box-s1/bearer",
		Ledgers: []connect.DescriptorLedger{{
			Scope:      connect.ScopeCity,
			Classes:    cityClasses,
			URL:        "https://ledger.example.test:7491",
			ProjectID:  "proj-city",
			Workspaces: []string{filepath.Clean(cityDir)},
		}},
	}
	port := 7492
	for _, name := range slices.Sorted(maps.Keys(bindingClasses)) {
		d.Ledgers = append(d.Ledgers, connect.DescriptorLedger{
			Scope:      connect.BindingScope(name),
			Classes:    bindingClasses[name],
			URL:        "https://" + name + ".example.test:" + strconv.Itoa(port),
			ProjectID:  "proj-" + name,
			Workspaces: []string{filepath.Join(filepath.Clean(cityDir), ".gc", "storage", name)},
		})
		port++
	}
	return d
}

// writeControlPlaneFixture makes cityDir a worker site whose descriptor is d,
// as `gc connect` would leave it, and drops any plane this process cached for
// cityDir.
func writeControlPlaneFixture(t *testing.T, cityDir string, d connect.Descriptor) {
	t.Helper()
	data, err := connect.EncodeDescriptor(d)
	if err != nil {
		t.Fatalf("EncodeDescriptor: %v", err)
	}
	writeControlPlaneBytes(t, cityDir, data)
}

func writeControlPlaneBytes(t *testing.T, cityDir string, data []byte) {
	t.Helper()
	path := filepath.Join(cityDir, citylayout.ControlPlaneDescriptorPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	forgetControlPlane(cityDir)
}

// makeWorkerSite writes a descriptor whose city ledger serves every class and
// marks the invocation the way a command's own context resolution does, which
// sets the process flag.
func makeWorkerSite(t *testing.T, cityDir string) {
	t.Helper()
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	plane, err := markInvocationPlane(cityDir)
	if err != nil {
		t.Fatalf("markInvocationPlane: %v", err)
	}
	if !plane.Worker() {
		t.Fatal("fixture: the descriptor did not make a worker site")
	}
}

// countSocketDials replaces the gc socket dialer with one that counts and
// refuses every dial, and returns the count.
func countSocketDials(t *testing.T) *atomic.Int32 {
	t.Helper()
	var dials atomic.Int32
	prev := dialUnixSocket
	dialUnixSocket = func(string, time.Duration) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("counting dialer: no listener")
	}
	t.Cleanup(func() { dialUnixSocket = prev })
	return &dials
}

func TestControlPlaneForAbsentDescriptorIsLocal(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := t.TempDir()

	plane, err := controlPlaneFor(cityDir)
	if err != nil {
		t.Fatalf("controlPlaneFor: %v", err)
	}
	if plane.Worker() {
		t.Fatal("a city with no descriptor resolved Worker, want Local")
	}
	if workerSiteProcess.Load() {
		t.Fatal("a Local plane set the worker-site flag")
	}
}

// TestControlPlaneForValidDescriptorIsWorker: the lookup answers Worker and
// leaves the process flag alone (a multi-city host's lookup says nothing
// about the process); the invocation's own context resolution sets it.
func TestControlPlaneForValidDescriptorIsWorker(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := t.TempDir()
	want := testDescriptor(cityDir, map[string][]string{"infra": {"graph", "sessions"}})
	writeControlPlaneFixture(t, cityDir, want)

	plane, err := controlPlaneFor(cityDir)
	if err != nil {
		t.Fatalf("controlPlaneFor: %v", err)
	}
	if !plane.Worker() {
		t.Fatal("a valid descriptor resolved Local, want Worker")
	}
	if workerSiteProcess.Load() {
		t.Fatal("a seam lookup set the worker-site flag; only the invocation's context resolution may")
	}
	if got := plane.Descriptor(); got.SessionID != want.SessionID || len(got.Ledgers) != 2 {
		t.Fatalf("descriptor = %+v, want the one written", got)
	}

	ctx, err := withControlPlane(resolvedContext{CityPath: cityDir}, nil)
	if err != nil || !ctx.Plane.Worker() {
		t.Fatalf("withControlPlane = (%+v, %v), want a Worker plane", ctx.Plane, err)
	}
	if !workerSiteProcess.Load() {
		t.Fatal("the invocation's context resolution did not set the worker-site flag")
	}
}

func TestControlPlaneForMalformedDescriptorIsATypedError(t *testing.T) {
	for name, body := range map[string]string{
		"not toml":      "schema = [",
		"unknown key":   "schema = 1\nplane = \"worker\"\nextra = 1\n",
		"local plane":   "schema = 1\nplane = \"local\"\n",
		"empty file":    "",
		"wrong version": "schema = 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			resetControlPlaneForTest(t)
			cityDir := t.TempDir()
			writeControlPlaneBytes(t, cityDir, []byte(body))

			_, err := controlPlaneFor(cityDir)
			var descErr *errControlPlaneDescriptor
			if !errors.As(err, &descErr) {
				t.Fatalf("controlPlaneFor error = %v, want *errControlPlaneDescriptor", err)
			}
			if want := filepath.Join(cityDir, citylayout.ControlPlaneDescriptorPath); descErr.path != want {
				t.Fatalf("error path = %q, want %q", descErr.path, want)
			}
			if descErr.reason == "" {
				t.Fatal("error carries no reason")
			}
			if !descErr.present || !strings.Contains(err.Error(), "present but unusable") {
				t.Fatalf("error = %v, want the present-but-unusable wording", err)
			}
			if strings.Contains(err.Error(), "rerun gc connect") {
				t.Fatalf("error = %v names a remedy command that does not exist yet", err)
			}
			if workerSiteProcess.Load() {
				t.Fatal("a seam lookup set the worker-site flag")
			}
			// The invocation fails closed: a descriptor it cannot use marks
			// the process a worker site, even for an exempt command.
			if _, err := withControlPlane(resolvedContext{CityPath: cityDir}, nil); !errors.As(err, &descErr) {
				t.Fatalf("withControlPlane = %v, want the descriptor error", err)
			}
			if !workerSiteProcess.Load() {
				t.Fatal("an unusable descriptor did not mark the invocation a worker site")
			}
		})
	}
}

func TestControlPlaneForUnreadableDescriptorIsATypedError(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := t.TempDir()
	// A directory where the file belongs cannot be read as a descriptor.
	if err := os.MkdirAll(filepath.Join(cityDir, citylayout.ControlPlaneDescriptorPath), 0o755); err != nil {
		t.Fatal(err)
	}
	var descErr *errControlPlaneDescriptor
	if _, err := controlPlaneFor(cityDir); !errors.As(err, &descErr) || !descErr.present {
		t.Fatalf("controlPlaneFor error = %v, want a present *errControlPlaneDescriptor", err)
	}
}

// TestControlPlaneForUncheckableDescriptorSaysSo: a .gc the caller cannot
// traverse is not "present but unusable"; the message says gc could not
// check, and the plane still fails closed.
func TestControlPlaneForUncheckableDescriptorSaysSo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root traverses a mode-0 directory")
	}
	resetControlPlaneForTest(t)
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gcDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gcDir, 0o755) })

	_, err := controlPlaneFor(cityDir)
	var descErr *errControlPlaneDescriptor
	if !errors.As(err, &descErr) || descErr.present {
		t.Fatalf("controlPlaneFor error = %v, want an uncheckable *errControlPlaneDescriptor", err)
	}
	if !strings.Contains(err.Error(), "cannot check for the control-plane descriptor") {
		t.Fatalf("error = %v, want the cannot-check wording", err)
	}
	if !onWorkerSite(cityDir) {
		t.Fatal("onWorkerSite = false for a descriptor gc cannot check, want true (fail closed)")
	}
}

// TestControlPlaneForGCThatIsNotADirectoryIsLocal: a .gc file cannot hold the
// descriptor, so ENOTDIR is absence, not an error.
func TestControlPlaneForGCThatIsNotADirectoryIsLocal(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, ".gc"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	plane, err := controlPlaneFor(cityDir)
	if err != nil || plane.Worker() {
		t.Fatalf("controlPlaneFor = (%+v, %v), want Local", plane, err)
	}
}

func TestControlPlaneForCachesPerPath(t *testing.T) {
	resetControlPlaneForTest(t)
	worker, local := t.TempDir(), t.TempDir()
	writeControlPlaneFixture(t, worker, testDescriptor(worker, nil))

	if plane, err := controlPlaneFor(worker); err != nil || !plane.Worker() {
		t.Fatalf("first read: plane=%+v err=%v, want Worker", plane, err)
	}
	if plane, err := controlPlaneFor(local); err != nil || plane.Worker() {
		t.Fatalf("local city: plane=%+v err=%v, want Local", plane, err)
	}
	// Removing the file and writing one for the other city changes nothing
	// this process already resolved.
	if err := os.Remove(filepath.Join(worker, citylayout.ControlPlaneDescriptorPath)); err != nil {
		t.Fatal(err)
	}
	data, err := connect.EncodeDescriptor(testDescriptor(local, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(local, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, citylayout.ControlPlaneDescriptorPath), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if plane, err := controlPlaneFor(worker + "/"); err != nil || !plane.Worker() {
		t.Fatalf("cached read: plane=%+v err=%v, want the cached Worker", plane, err)
	}
	if plane, err := controlPlaneFor(local); err != nil || plane.Worker() {
		t.Fatalf("cached read: plane=%+v err=%v, want the cached Local", plane, err)
	}
}

// TestControlPlaneLedgerBindingsComeFromTheLedgerClasses: a class is attached
// through the ledger whose classes list it, at that ledger's binding, and the
// box's [storage] matches only when it assigns every class the same way.
func TestControlPlaneLedgerBindingsComeFromTheLedgerClasses(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := t.TempDir()
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, map[string][]string{
		"infra":   {"graph", "sessions", "messaging"},
		"archive": {"orders", "nudges"},
	}))
	plane, err := controlPlaneFor(cityDir)
	if err != nil {
		t.Fatalf("controlPlaneFor: %v", err)
	}
	want := map[string]string{
		"work": "work", "graph": "infra", "sessions": "infra", "messaging": "infra",
		"orders": "archive", "nudges": "archive",
	}
	for class, binding := range want {
		if got, ok := plane.ledgerBindingFor(class); !ok || got != binding {
			t.Errorf("ledgerBindingFor(%q) = (%q, %v), want %q", class, got, ok, binding)
		}
	}
	if got, ok := plane.ledgerBindingFor("unknown"); ok {
		t.Errorf("ledgerBindingFor(unknown) = %q, want no ledger", got)
	}
	if _, ok := (controlPlane{}).ledgerBindingFor("work"); ok {
		t.Error("a Local plane attaches a class; it has no descriptor")
	}

	if class, mismatch := plane.classBindingMismatch(func(class string) string { return want[class] }); mismatch {
		t.Errorf("a [storage] that agrees with the descriptor mismatched at %q", class)
	}
	// The binding is attached, but not for this class: the descriptor puts
	// sessions on infra and this box's [storage] routes it to archive.
	swapped := maps.Clone(want)
	swapped["sessions"] = "archive"
	if class, mismatch := plane.classBindingMismatch(func(class string) string { return swapped[class] }); !mismatch || class != "sessions" {
		t.Errorf("classBindingMismatch = (%q, %v), want sessions", class, mismatch)
	}
}

func TestDialGCSocketRefusesOnAWorkerSiteWithoutDialing(t *testing.T) {
	resetControlPlaneForTest(t)
	dials := countSocketDials(t)

	if _, err := dialGCSocket("/nonexistent/local.sock", time.Millisecond); err == nil || errors.Is(err, errWorkerSiteNoController) {
		t.Fatalf("local dial error = %v, want the dialer's own error", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("local dials = %d, want 1", got)
	}

	makeWorkerSite(t, t.TempDir())
	conn, err := dialGCSocket("/nonexistent/worker.sock", time.Millisecond)
	if conn != nil || !errors.Is(err, errWorkerSiteNoController) {
		t.Fatalf("worker-site dial = (%v, %v), want errWorkerSiteNoController", conn, err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("a worker-site dial reached the dialer: dials = %d, want 1", got)
	}
}

// TestDialCitySocketDecidesFromTheCitysOwnPlane: a city's socket is refused
// when that city is a worker site and dialed when it is Local, whichever city
// marked the invocation.
func TestDialCitySocketDecidesFromTheCitysOwnPlane(t *testing.T) {
	resetControlPlaneForTest(t)
	dials := countSocketDials(t)
	box, local := t.TempDir(), t.TempDir()
	makeWorkerSite(t, box)

	if _, err := dialCitySocket(local, "/nonexistent/local.sock", time.Millisecond); err == nil || errors.Is(err, errWorkerSiteNoController) {
		t.Fatalf("Local city dial error = %v, want the dialer's own error", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("Local city dials = %d, want 1", got)
	}
	conn, err := dialCitySocket(box, "/nonexistent/box.sock", time.Millisecond)
	if conn != nil || !errors.Is(err, errWorkerSiteNoController) {
		t.Fatalf("worker-site city dial = (%v, %v), want errWorkerSiteNoController", conn, err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("a worker-site city's dial reached the dialer: dials = %d, want 1", got)
	}
}

// TestControlPlaneGateRefusesAMalformedDescriptor resolves a city whose
// descriptor is malformed: every command refuses it except the four that read
// the file themselves.
func TestControlPlaneGateRefusesAMalformedDescriptor(t *testing.T) {
	resetControlPlaneForTest(t)
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"gate-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeControlPlaneBytes(t, cityDir, []byte("schema = ["))
	t.Setenv("GC_CITY", cityDir)

	var descErr *errControlPlaneDescriptor
	if _, err := resolveContext(); !errors.As(err, &descErr) {
		t.Fatalf("resolveContext error = %v, want *errControlPlaneDescriptor", err)
	}
	if _, err := resolveCommandContext([]string{cityDir}); !errors.As(err, &descErr) {
		t.Fatalf("resolveCommandContext(path) error = %v, want *errControlPlaneDescriptor", err)
	}

	prev := controlPlaneGateExempt
	t.Cleanup(func() { controlPlaneGateExempt = prev })
	controlPlaneGateExempt = true
	ctx, err := resolveContext()
	if err != nil {
		t.Fatalf("an exempt command's resolveContext = %v, want the city", err)
	}
	if ctx.CityPath != cityDir || ctx.Plane.Worker() {
		t.Fatalf("exempt context = %+v, want the city on the Local plane", ctx)
	}
}

func TestControlPlaneGateExemptsOnlyTheCommandsThatReadTheFile(t *testing.T) {
	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"doctor"}, true},
		{[]string{"doctor", "--verbose"}, true},
		{[]string{"--city", "/x", "doctor"}, true},
		{[]string{"version"}, true},
		{[]string{"help"}, true},
		{[]string{"help", "start"}, true},
		{[]string{"start"}, false},
		{[]string{"session", "list"}, false},
		{[]string{"events"}, false},
		{[]string{}, false},
		{[]string{"no-such-command"}, false},
	} {
		if got := controlPlaneGateExemptFor(root, tc.args); got != tc.want {
			t.Errorf("controlPlaneGateExemptFor(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
	if !controlPlaneExemptCommands["connect"] {
		t.Error("gc connect is not exempt; it writes and reads the descriptor itself")
	}
}

// TestControlPlaneGateThroughRun runs real commands through run(): a
// command that resolves the city refuses a malformed descriptor, and
// `gc version` still runs.
func TestControlPlaneGateThroughRun(t *testing.T) {
	resetControlPlaneForTest(t)
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"gate-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeControlPlaneBytes(t, cityDir, []byte("schema = ["))
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"rig", "list"}, &stdout, &stderr); code == 0 {
		t.Fatalf("gc rig list on a malformed descriptor exited 0; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), citylayout.ControlPlaneDescriptorPath) {
		t.Fatalf("stderr does not name the descriptor:\n%s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc version = %d on a malformed descriptor; stderr=%s", code, stderr.String())
	}
	if controlPlaneGateExempt {
		t.Fatal("run() left the gate exemption set")
	}
}

// newControlPlaneBoxCity returns a city directory that is a worker site whose
// descriptor this process has read but no invocation has marked.
func newControlPlaneBoxCity(t *testing.T) string {
	t.Helper()
	box := t.TempDir()
	if err := os.WriteFile(filepath.Join(box, "city.toml"), []byte("[workspace]\nname = \"box-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeControlPlaneFixture(t, box, testDescriptor(box, nil))
	return box
}

// TestControlPlaneFlagCityFromInsideABoxStopsThroughRun: eager pack-command
// discovery resolves the city environment's city before cobra parses --city.
// With GC_CITY on a worker site, `gc --city <local> stop` still reaches the
// Local city's controller, and the invocation is never marked a worker site.
func TestControlPlaneFlagCityFromInsideABoxStopsThroughRun(t *testing.T) {
	resetControlPlaneForTest(t)
	clearGCEnv(t)
	box := newControlPlaneBoxCity(t)
	local := setupCity(t, "local-city")
	commands := startAcknowledgingStandaloneController(t, local)
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	t.Setenv("GC_CITY", box)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", local, "stop"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc --city <local> stop = %d from inside a box; stderr=%s", code, stderr.String())
	}
	select {
	case cmd := <-commands:
		if cmd != "stop" {
			t.Fatalf("the Local controller received %q, want stop", cmd)
		}
	default:
		t.Fatal("the Local controller received no stop request")
	}
	if !strings.Contains(stdout.String(), "Controller stopping...") {
		t.Fatalf("stdout = %q, want the acknowledged controller stop", stdout.String())
	}
	if workerSiteProcess.Load() {
		t.Fatal("discovery of the box marked an invocation against a Local city a worker site")
	}
}

// TestControlPlaneBoxStopRefusesThroughRun: `gc stop` that resolves the box
// itself marks the invocation a worker site and refuses up front: it dials
// nothing, cleans up nothing, and leaves alone a supervisor registration the
// city held before `gc connect` attached it.
func TestControlPlaneBoxStopRefusesThroughRun(t *testing.T) {
	resetControlPlaneForTest(t)
	clearGCEnv(t)
	box := newControlPlaneBoxCity(t)
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(box, "box-city"); err != nil {
		t.Fatal(err)
	}
	shutdowns := 0
	overrideShutdownBeadsProviderForStop(t, func(string) error {
		shutdowns++
		return nil
	})
	dials := countSocketDials(t)
	t.Setenv("GC_CITY", box)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"stop"}, &stdout, &stderr); code == 0 {
		t.Fatalf("gc stop in a box exited 0; stdout=%s", stdout.String())
	}
	if want := "gc stop: " + errWorkerSiteRefusesController.Error() + "\n"; stderr.String() != want || stdout.Len() != 0 {
		t.Fatalf("gc stop in a box: stdout=%q stderr=%q, want only the refusal %q", stdout.String(), stderr.String(), want)
	}
	if entries, err := reg.List(); err != nil || len(entries) != 1 {
		t.Fatalf("registry after the refused stop = %v, %v; want the box's entry", entries, err)
	}
	if !workerSiteProcess.Load() {
		t.Fatal("gc stop in a box was not marked a worker site")
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("gc stop in a box dialed %d gc sockets, want 0", got)
	}
	if shutdowns != 0 {
		t.Fatalf("gc stop in a box shut the bead store down %d times, want 0", shutdowns)
	}
}

// TestControlPlaneGateExemptsDoctorThroughRun is D12's exemption wired through
// run(): gc doctor resolves the city, so it reaches the gate, and on a
// malformed descriptor it still runs its checks. The exempt invocation is
// still marked a worker site, so it can neither act nor dial.
func TestControlPlaneGateExemptsDoctorThroughRun(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newDoctorSelectorCity(t)
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")
	writeControlPlaneBytes(t, cityDir, []byte("schema = ["))

	code, payload, stderr := runDoctorJSON(t, cityDir, "--check", "city-structure")
	if payload.Error != nil {
		t.Fatalf("gc doctor refused on a malformed descriptor: code=%d err=%+v stderr=%s", code, payload.Error, stderr)
	}
	if len(payload.Results) != 1 || payload.Results[0].Name != "city-structure" {
		t.Fatalf("gc doctor did not run its check: code=%d payload=%+v stderr=%s", code, payload, stderr)
	}
	if !workerSiteProcess.Load() {
		t.Fatal("the exempt invocation was not marked a worker site")
	}
}

func TestNoteDeferredSessionStart(t *testing.T) {
	t.Run("poke reached a controller", func(t *testing.T) {
		var stderr bytes.Buffer
		if !noteDeferredSessionStart(&stderr, "gc session new", session.Info{ID: "gc-1", PendingCreateClaim: true}, nil) {
			t.Fatal("reached = false, want true")
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
	})
	t.Run("worker site", func(t *testing.T) {
		var stderr bytes.Buffer
		if noteDeferredSessionStart(&stderr, "gc session new", session.Info{ID: "gc-1", PendingCreateClaim: true}, errWorkerSiteNoController) {
			t.Fatal("reached = true, want false")
		}
		if got, want := stderr.String(), "gc session new: "+errWorkerSiteNoController.Error()+"\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
	})
	const anchor = "2026-10-28T09:00:00Z"
	deadline := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC).Add(pendingCreateNeverStartedTimeout).Format(time.RFC3339)
	rollbackLimit := "rolled back if none has started it by " + deadline
	for _, tc := range []struct {
		name      string
		row       session.Info
		wantLimit bool
	}{
		{name: "no pending create", row: session.Info{ID: "gc-1", PendingCreateStartedAt: anchor}},
		{name: "pending create never started", row: session.Info{ID: "gc-1", PendingCreateClaim: true, PendingCreateStartedAt: anchor}, wantLimit: true},
		{name: "pending create with a recorded start", row: session.Info{ID: "gc-1", PendingCreateClaim: true, PendingCreateStartedAt: anchor, LastWokeAt: "2026-10-28T09:01:00Z"}},
	} {
		t.Run("poke failed, "+tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if noteDeferredSessionStart(&stderr, "gc session wake", tc.row, errors.New("dial failed")) {
				t.Fatal("reached = true, want false")
			}
			got := stderr.String()
			for _, want := range []string{"gc session wake: warning: poke failed: dial failed", "gc-1", "gc supervisor start"} {
				if !strings.Contains(got, want) {
					t.Fatalf("stderr = %q, want it to contain %q", got, want)
				}
			}
			if n := strings.Count(got, "\n"); n != 1 {
				t.Fatalf("stderr lines = %d, want 1: %q", n, got)
			}
			if gotLimit := strings.Contains(got, rollbackLimit); gotLimit != tc.wantLimit {
				t.Fatalf("stderr = %q; states the rollback limit = %v, want %v", got, gotLimit, tc.wantLimit)
			}
		})
	}
}

// TestEnqueueDeferredSessionStart pins the poke result a deferred start
// proceeds under: a controller reached by either the probe poke or the enqueue
// counts, and a worker site's probe skips the enqueue.
func TestEnqueueDeferredSessionStart(t *testing.T) {
	resetControlPlaneForTest(t)
	// The supervisor fallback of every poke below is refused, so only the
	// fake controller socket can answer.
	countSocketDials(t)
	probeErr := errors.New("probe poke failed")

	t.Run("enqueue reached a controller", func(t *testing.T) {
		cityDir := shortSocketTempDir(t, "gc-enqueue-city-")
		startFakeUnixSocket(t, controllerSocketPath(cityDir), func(line string) string {
			if strings.HasPrefix(line, pokeKeyedCommandPrefix) {
				return "ok\n"
			}
			return ""
		})
		if err := enqueueDeferredSessionStart(cityDir, "gc-1", probeErr); err != nil {
			t.Fatalf("enqueueDeferredSessionStart = %v, want nil: the enqueue reached a controller", err)
		}
	})
	t.Run("probe reached a controller", func(t *testing.T) {
		if err := enqueueDeferredSessionStart(t.TempDir(), "gc-1", nil); err != nil {
			t.Fatalf("enqueueDeferredSessionStart = %v, want nil: the probe reached a controller", err)
		}
	})
	t.Run("neither reached a controller", func(t *testing.T) {
		err := enqueueDeferredSessionStart(t.TempDir(), "gc-1", probeErr)
		if err == nil || errors.Is(err, probeErr) {
			t.Fatalf("enqueueDeferredSessionStart = %v, want the enqueue's own error", err)
		}
	})
	t.Run("worker site probe", func(t *testing.T) {
		cityDir := shortSocketTempDir(t, "gc-enqueue-city-")
		accepted := startFakeUnixSocket(t, controllerSocketPath(cityDir), func(string) string { return "ok\n" })
		if err := enqueueDeferredSessionStart(cityDir, "gc-1", errWorkerSiteNoController); !errors.Is(err, errWorkerSiteNoController) {
			t.Fatalf("enqueueDeferredSessionStart = %v, want %v", err, errWorkerSiteNoController)
		}
		select {
		case <-accepted:
			t.Fatal("a worker site's deferred start enqueued to the controller socket")
		default:
		}
	})
}

// TestNeverStartedRollbackLimit pins the rollback limit the deferred-start
// notices state to the deadline the reconciler rolls a never-started pending
// create back at: its anchor (pending_create_started_at, else CreatedAt) plus
// pendingCreateNeverStartedTimeout, not the timeout counted from now.
func TestNeverStartedRollbackLimit(t *testing.T) {
	started := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	created := started.Add(-3 * time.Minute)
	for _, tc := range []struct {
		name       string
		row        session.Info
		wantAnchor time.Time
	}{
		{name: "pending_create_started_at", row: session.Info{PendingCreateClaim: true, PendingCreateStartedAt: started.Format(time.RFC3339), CreatedAt: created}, wantAnchor: started},
		{name: "CreatedAt when no start is recorded", row: session.Info{PendingCreateClaim: true, CreatedAt: created}, wantAnchor: created},
		{name: "CreatedAt when the start does not parse", row: session.Info{PendingCreateClaim: true, PendingCreateStartedAt: "not-a-time", CreatedAt: created}, wantAnchor: created},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deadline := tc.wantAnchor.Add(pendingCreateNeverStartedTimeout)
			if got, want := neverStartedRollbackLimit(tc.row), "by "+deadline.Format(time.RFC3339); got != want {
				t.Fatalf("neverStartedRollbackLimit = %q, want %q", got, want)
			}
			// The stated deadline is the reconciler's: the lease has not
			// expired just before it and has just after.
			if pendingCreateNeverStartedLeaseExpiredAt(tc.row, deadline.Add(-time.Second)) {
				t.Fatalf("lease expired a second before the stated deadline %s", deadline.Format(time.RFC3339))
			}
			if !pendingCreateNeverStartedLeaseExpiredAt(tc.row, deadline.Add(time.Second)) {
				t.Fatalf("lease still live a second after the stated deadline %s", deadline.Format(time.RFC3339))
			}
		})
	}
	t.Run("no anchor", func(t *testing.T) {
		want := "within " + pendingCreateNeverStartedTimeout.String() + " of its creation"
		if got := neverStartedRollbackLimit(session.Info{PendingCreateClaim: true}); got != want {
			t.Fatalf("neverStartedRollbackLimit = %q, want %q", got, want)
		}
	})
}

func TestNoteNotAttachingUntilSupervisor(t *testing.T) {
	const anchor = "2026-10-28T09:00:00Z"
	deadline := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC).Add(pendingCreateNeverStartedTimeout).Format(time.RFC3339)
	for _, tc := range []struct {
		name      string
		row       session.Info
		wantLimit bool
	}{
		{name: "pending create never started", row: session.Info{ID: "gc-1", PendingCreateClaim: true, PendingCreateStartedAt: anchor}, wantLimit: true},
		{name: "start request with no pending create", row: session.Info{ID: "gc-1", PendingCreateStartedAt: anchor}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			noteNotAttachingUntilSupervisor(&out, tc.row)
			got := out.String()
			for _, want := range []string{"not attaching", "gc supervisor start", "gc session attach gc-1"} {
				if !strings.Contains(got, want) {
					t.Fatalf("output = %q, want it to contain %q", got, want)
				}
			}
			limit := "by " + deadline + ", or the reconciler rolls this session back"
			if gotLimit := strings.Contains(got, limit); gotLimit != tc.wantLimit {
				t.Fatalf("output = %q; states %q = %v, want %v", got, limit, gotLimit, tc.wantLimit)
			}
			if !tc.wantLimit && strings.Contains(got, "rolls this session back") {
				t.Fatalf("output = %q, want no rollback for a row with no pending create", got)
			}
		})
	}
}

// TestPendingStartAwaitsSupervisor pins which rows a command refuses to start
// in this process: a start-pending row whose runtime is not running, in a
// city whose session starts wait for the supervisor. Any other row is decided
// without reading the city, so a registry that does not parse only fails the
// one row whose answer depends on it.
func TestPendingStartAwaitsSupervisor(t *testing.T) {
	resetControlPlaneForTest(t)
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))
	t.Setenv("XDG_RUNTIME_DIR", shortSocketTempDir(t, "gc-run-"))
	cityDir := shortSocketTempDir(t, "gc-awaits-city-")
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityDir, "awaits-city"); err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "s-running", runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	pending := session.Info{ID: "gc-1", SessionName: "s-pending", MetadataState: string(session.StateStartPending), PendingCreateClaim: true}
	for _, tc := range []struct {
		name string
		row  session.Info
		want bool
	}{
		{name: "start-pending, runtime not running", row: pending, want: true},
		{name: "start-pending, runtime running", row: session.Info{ID: "gc-2", SessionName: "s-running", MetadataState: string(session.StateStartPending), PendingCreateClaim: true}},
		{name: "asleep", row: session.Info{ID: "gc-3", SessionName: "s-asleep", MetadataState: string(session.StateAsleep)}},
		{name: "creating", row: session.Info{ID: "gc-4", SessionName: "s-creating", MetadataState: string(session.StateCreating), PendingCreateClaim: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pendingStartAwaitsSupervisor(cityDir, tc.row, sp)
			if err != nil {
				t.Fatalf("pendingStartAwaitsSupervisor: %v", err)
			}
			if got != tc.want {
				t.Fatalf("pendingStartAwaitsSupervisor = %v, want %v", got, tc.want)
			}
		})
	}

	path := supervisor.RegistryPath()
	if err := os.WriteFile(path, []byte("[[cities]\npath = \"unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := pendingStartAwaitsSupervisor(cityDir, session.Info{ID: "gc-3", SessionName: "s-asleep", MetadataState: string(session.StateAsleep)}, sp); got || err != nil {
		t.Fatalf("pendingStartAwaitsSupervisor(asleep row, corrupt registry) = (%v, %v), want (false, nil): the city is not read", got, err)
	}
	if _, err := pendingStartAwaitsSupervisor(cityDir, pending, sp); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("pendingStartAwaitsSupervisor(start-pending row, corrupt registry) error = %v, want one naming %s", err, path)
	}
}
