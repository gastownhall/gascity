//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beads"
)

// managedDoltWindowCity is a stopped managed bd/Dolt city (S5 §10.1, S21):
// the default shape, initialized by the real gc-beads-bd provider script, its
// Dolt pinned to a free loopback port the test chose. It holds two session
// beads joined by an edge whose payload is a waits_for gate, and a work bead.
type managedDoltWindowCity struct {
	cityPath string
	port     int
	from, to string
	payload  string
}

// reservedDoltPorts are the ports a test must never start or reach a Dolt on.
var reservedDoltPorts = map[int]bool{3307: true, 45865: true, 8123: true}

// freeDoltTestPort picks a free loopback port, choosing the candidate outside
// the reserved ones before anything binds it.
func freeDoltTestPort(t *testing.T) int {
	t.Helper()
	for range 50 {
		candidate := 20000 + rand.IntN(40000)
		if reservedDoltPorts[candidate] {
			continue
		}
		lis, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(candidate))
		if err != nil {
			continue
		}
		_ = lis.Close()
		return candidate
	}
	t.Fatal("no free port outside the reserved ones")
	return 0
}

// managedDoltRuntimePID is the PID of the city's managed Dolt, or 0.
func managedDoltRuntimePID(t *testing.T, cityPath string, port int) int {
	t.Helper()
	state, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil || !state.Running {
		return 0
	}
	if state.Port != port || reservedDoltPorts[state.Port] {
		t.Fatalf("the managed Dolt runs on port %d, want the test's %d", state.Port, port)
	}
	return state.PID
}

func newManagedDoltWindowCity(t *testing.T) managedDoltWindowCity {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	bdPath := waitTestRealBDPath(t)
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	homeDir := filepath.Join(shortSocketTempDir(t, "gc-s5b-home-"), "home")
	if err := writeTestDoltIdentity(homeDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("DOLT_ROOT_PATH", homeDir)
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)))
	port := freeDoltTestPort(t)
	t.Setenv("GC_DOLT_PORT", strconv.Itoa(port))
	gcBin := currentGCBinaryForTests(t)
	prevResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() (string, error) { return gcBin, nil }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = prevResolve })
	prevCity, prevRig := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() { cityFlag, rigFlag = prevCity, prevRig })

	cityPath := shortSocketTempDir(t, "gc-s5b-city-")
	rigPath, err := writeManagedBdWaitTestCityScaffold(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{cityPath, rigPath} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("dolt.mode: server\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	materializeBuiltinPacksForTest(t, cityPath)
	script := gcBeadsBdScriptPath(cityPath)
	scriptEnv := waitTestEnv(map[string]string{
		"GC_BEADS": "bd", "GC_DOLT": "", "GC_BIN": gcBin,
		"GC_CITY": cityPath, "GC_CITY_PATH": cityPath,
		"GC_BEADS_TRANSPORT": "direct", "GC_BEADS_TARGET": "local",
		"GC_DOLT_PORT": strconv.Itoa(port),
		"HOME":         homeDir, "DOLT_ROOT_PATH": homeDir, "PATH": os.Getenv("PATH"),
	})
	runScript := func(args ...string) {
		t.Helper()
		if err := runProviderOpWithEnv(script, scriptEnv, args...); err != nil {
			t.Fatalf("%s: %v", strings.Join(args, " "), err)
		}
	}
	// The cleanup of last resort: the script's stop, then every Dolt the
	// test saw started, by the PID it recorded, never by name.
	var started []int
	t.Cleanup(func() {
		_ = runProviderOpWithEnv(script, scriptEnv, "stop")
		for _, pid := range started {
			cleanupManagedDoltTestPID(t, pid)
		}
	})
	runScript("start")
	runScript("init", cityPath, "gc", "hq")
	runScript("init", rigPath, "fe", "fe")
	runScript("stop")
	if err := clearManagedDoltRuntimeState(cityPath); err != nil {
		t.Fatal(err)
	}

	// Seed through the city's own provider, started and stopped as gc start
	// and gc stop do.
	if err := ensureBeadsProvider(cityPath); err != nil {
		t.Fatalf("starting the provider to seed: %v", err)
	}
	pid := managedDoltRuntimePID(t, cityPath, port)
	if pid == 0 {
		t.Fatal("the provider started no managed Dolt")
	}
	started = append(started, pid)
	city := managedDoltWindowCity{cityPath: cityPath, port: port, payload: `{"gate":"waits_for","threshold":2}`}
	result, err := openStoreResultAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostic.Store != beads.BeadsStoreNameNativeDoltStore {
		t.Fatalf("the seeding open took %+v, want the native lane", result.Diagnostic)
	}
	store := result.Store
	from := mustCreateInfraBead(t, store, beads.Bead{Title: "gated step", Type: "session"})
	to := mustCreateInfraBead(t, store, beads.Bead{Title: "the step it waits on", Type: "session"})
	mustCreateInfraBead(t, store, beads.Bead{Title: "an ordinary work bead", Type: "task"})
	if err := store.DepAdd(from.ID, to.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(store)
	city.from, city.to = from.ID, to.ID
	city.setEdgePayload(t)
	if err := shutdownBeadsProvider(cityPath); err != nil {
		t.Fatalf("stopping the provider after seeding: %v", err)
	}
	if pidAlive(pid) || doltPortReachable(strconv.Itoa(port)) {
		t.Fatal("precondition: the city's Dolt is still up after gc stop's provider shutdown")
	}
	return city
}

// setEdgePayload writes the gate onto the edge's metadata column, as a graph
// plan would, straight into the managed database.
func (c managedDoltWindowCity) setEdgePayload(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil || meta.DoltDatabase == "" {
		t.Fatalf("reading the city's database name from metadata.json: %v (%s)", err, data)
	}
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s", c.port, meta.DoltDatabase))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // test cleanup
	// The edge lives in dependencies or, between wisps, wisp_dependencies;
	// its target in the column of the target's own table.
	var touched int64
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		res, err := db.Exec("UPDATE "+table+" SET metadata = ? WHERE issue_id = ? AND (depends_on_issue_id = ? OR depends_on_wisp_id = ?)", c.payload, c.from, c.to, c.to)
		if err != nil {
			t.Fatalf("writing the edge payload into %s: %v", table, err)
		}
		n, _ := res.RowsAffected()
		touched += n
	}
	if touched != 1 {
		t.Fatalf("the payload write touched %d rows, want 1", touched)
	}
	// A wisp table is not versioned, so there may be nothing to commit.
	if _, err := db.Exec("CALL DOLT_COMMIT('-Am', 'test: edge payload')"); err != nil && !strings.Contains(err.Error(), "nothing to commit") {
		t.Fatalf("committing the edge payload: %v", err)
	}
}

// migrateManagedCity runs `gc storage migrate --from-work --fleet-stopped` on
// the stopped city and returns its exit, its stderr and the lanes the work
// store opened on inside the window.
func (c managedDoltWindowCity) migrateManagedCity(t *testing.T) (int, string, []string, infraBindingTarget) {
	t.Helper()
	cfg, err := loadCityConfig(c.cityPath, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage = infraSplitConfig(filepath.Join(c.cityPath, ".gc", "store")).Storage
	stubInfraControllerPing(t, 0)
	stubSessionInventory(t, nil, true, nil)
	stubOtherCities(t)
	var lanes []string
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(cityPath string) (beads.Store, error) {
		store, err := prev(cityPath)
		if w := activeMigrationWindow(cityPath); w != nil && w.laneKnown {
			lanes = append(lanes, w.lane.Store)
		}
		return store, err
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
	request := storageOperatorRequest{CityPath: c.cityPath, Cfg: cfg, FleetStopped: true}
	var stdout, stderr bytes.Buffer
	code := doStorageMigrate(context.Background(), request, &stdout, &stderr)
	return code, stdout.String() + stderr.String(), lanes, mustResolveInfraTarget(t, c.cityPath, cfg)
}

// TestStorageMigrateMovesAStoppedManagedDoltCityInsideTheMaintenanceWindow is
// S21: a stopped managed bd/Dolt city, which used to fall to the bd CLI lane
// and be refused on its edge payload, migrates. The window starts its
// provider, every open of the work store takes the native lane, the gate on
// the edge reaches the binding, and the provider is stopped again.
func TestStorageMigrateMovesAStoppedManagedDoltCityInsideTheMaintenanceWindow(t *testing.T) {
	city := newManagedDoltWindowCity(t)
	code, said, lanes, target := city.migrateManagedCity(t)
	if code != 0 {
		t.Fatalf("migrate = %d: %s", code, said)
	}
	if !strings.Contains(said, "started the bead-store provider") {
		t.Errorf("the run did not say it started the provider: %s", said)
	}
	if len(lanes) == 0 {
		t.Fatal("no work-store open went through the window")
	}
	for _, lane := range lanes {
		if lane != beads.BeadsStoreNameNativeDoltStore {
			t.Fatalf("a work-store open in the window took %s, want the native lane: %v", lane, lanes)
		}
	}
	if pid := managedDoltRuntimePID(t, city.cityPath, city.port); pid != 0 || doltPortReachable(strconv.Itoa(city.port)) {
		t.Fatalf("the provider the window started is still up (pid %d)", pid)
	}
	if fileExists(migrationWindowRecordPath(city.cityPath)) {
		t.Error("the window's record outlived the run")
	}
	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
	reader, ok := binding.(beads.DepMetadataReader)
	if !ok {
		t.Fatalf("binding %T cannot be asked about payloads", binding)
	}
	got, carried, err := reader.DepMetadata(city.from, city.to)
	if err != nil || !carried || got != city.payload {
		t.Fatalf("the binding's edge carries (%q, %v, %v), want the gate %q", got, carried, err, city.payload)
	}
}

// TestStorageMigrateRefusesAForeignSessionOnTheManagedDoltItStarted is S76: a
// client that reaches the Dolt the window just started is a writer nothing
// proved stopped. The run refuses, naming it, and stops the server.
func TestStorageMigrateRefusesAForeignSessionOnTheManagedDoltItStarted(t *testing.T) {
	city := newManagedDoltWindowCity(t)
	var foreign *sql.DB
	prevStart := migrationWindowStartProvider
	migrationWindowStartProvider = func(cityPath string) error {
		if err := prevStart(cityPath); err != nil {
			return err
		}
		db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/", city.port))
		if err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		if err := db.Ping(); err != nil {
			return err
		}
		foreign = db
		return nil
	}
	t.Cleanup(func() {
		migrationWindowStartProvider = prevStart
		if foreign != nil {
			_ = foreign.Close()
		}
	})
	code, said, _, target := city.migrateManagedCity(t)
	if code == 0 {
		t.Fatalf("the migration ran with a foreign session on the server: %s", said)
	}
	if !strings.Contains(said, "other client session") || !strings.Contains(said, "root@") {
		t.Errorf("the refusal does not name the session: %s", said)
	}
	if pid := managedDoltRuntimePID(t, city.cityPath, city.port); pid != 0 {
		t.Fatalf("the refused run left the provider it started up (pid %d)", pid)
	}
	assertNoConvergenceMarker(t, target, "a refused migration records nothing")
}

// TestStorageMigrateDoesNotCountItsOwnPooledSessions: this process's own idle
// pooled sessions on the managed Dolt (internal/doltpool keeps them for up to
// 20s, past the settle) are not another client's. The window closes them
// before it lists the server's sessions.
func TestStorageMigrateDoesNotCountItsOwnPooledSessions(t *testing.T) {
	city := newManagedDoltWindowCity(t)
	prevStart := migrationWindowStartProvider
	migrationWindowStartProvider = func(cityPath string) error {
		if err := prevStart(cityPath); err != nil {
			return err
		}
		db, err := managedDoltOpenDB("", strconv.Itoa(city.port), "")
		if err != nil {
			return err
		}
		// Two sessions held at once, both returned to the pool idle.
		first, err := db.Conn(context.Background())
		if err != nil {
			return err
		}
		second, err := db.Conn(context.Background())
		if err != nil {
			return err
		}
		_ = first.Close()
		_ = second.Close()
		return nil
	}
	t.Cleanup(func() { migrationWindowStartProvider = prevStart })
	if code, said, _, _ := city.migrateManagedCity(t); code != 0 {
		t.Fatalf("migrate = %d: this process's own pooled sessions were counted as another client's: %s", code, said)
	}
	if pid := managedDoltRuntimePID(t, city.cityPath, city.port); pid != 0 {
		t.Fatalf("the provider the window started is still up (pid %d)", pid)
	}
}
