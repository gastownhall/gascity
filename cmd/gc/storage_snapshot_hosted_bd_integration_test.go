//go:build integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// hostedSnapshotForbiddenPorts are the ports a test must never bind or reach.
var hostedSnapshotForbiddenPorts = map[int]bool{3307: true, 45865: true, 8123: true}

func hostedSnapshotFreePort(t *testing.T) int {
	t.Helper()
	for range 100 {
		port := 20000 + rand.IntN(40000)
		if hostedSnapshotForbiddenPorts[port] {
			continue
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port
	}
	t.Fatal("no free port outside the reserved ones")
	return 0
}

// startHostedSnapshotProcess starts a long-running helper and stops it, by its
// own handle, when the test ends.
func startHostedSnapshotProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", cmd.Path, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
}

// TestStorageSnapshotHostedBdWorkStore is the hosted-bd row of the design's
// table, against a real `bd serve`: a Dolt sql-server on a free port, a bd
// workspace on it in server mode, `bd serve` over that workspace, and the city
// connected to it over HTTP (`bd connect`), as MC's work store is. `gc storage
// snapshot --work --fleet-stopped` runs the maintenance window over the
// http-backed city and takes the exact copy through `bd export` over HTTP;
// the export holds every row bd serves, and the snapshot verifies.
func TestStorageSnapshotHostedBdWorkStore(t *testing.T) {
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	bd, err := findPreferredBinary("bd", strings.TrimSpace(os.Getenv("GC_TEST_BD_BIN")))
	if err != nil {
		t.Skip("bd not found (set GC_TEST_BD_BIN or install bd)")
	}
	for _, probe := range [][]string{{"serve", "--help"}, {"connect", "--help"}} {
		if out, err := exec.Command(bd, probe...).CombinedOutput(); err != nil || strings.Contains(string(out), "unknown command") {
			t.Skipf("this bd has no `bd %s`, which a hosted store needs (%v)", probe[0], err)
		}
	}
	configureTestDoltIdentityEnv(t)
	bdEnv := func(ws string) []string {
		return append(os.Environ(), "BEADS_DIR="+filepath.Join(ws, ".beads"), "BD_NON_INTERACTIVE=1", "BD_BACKUP_ENABLED=false", "BD_EXPORT_AUTO=false", "BEADS_ACTOR=gc-test")
	}
	runBd := func(ws string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bd, args...)
		cmd.Dir, cmd.Env = ws, bdEnv(ws)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	// The hosted side: Dolt, a bd workspace on it, and bd serve.
	dataDir := t.TempDir()
	port := hostedSnapshotFreePort(t)
	dolt := exec.Command(doltBin, "sql-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--data-dir", dataDir, "--loglevel=warning")
	dolt.Env = append(os.Environ(), "DOLT_ROOT_PATH="+dataDir)
	startHostedSnapshotProcess(t, dolt)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server on %d never listened", port)
		}
		time.Sleep(100 * time.Millisecond)
	}
	server := t.TempDir()
	if out, err := exec.Command("git", "-C", server, "init", "-q", ".").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	runBd(server, "init", "--prefix", "hb", "--quiet", "--skip-agents", "--skip-hooks", "--server", "--server-host", "127.0.0.1", "--server-port", strconv.Itoa(port))
	runBd(server, "create", "hosted one", "-t", "task", "--quiet")
	runBd(server, "create", "hosted two", "-t", "task", "--quiet")
	// An explicit port, picked outside the reserved ones: bd's own ephemeral
	// pick could land on one.
	servePort := hostedSnapshotFreePort(t)
	serve := exec.Command(bd, "serve", "--addr", "127.0.0.1:"+strconv.Itoa(servePort))
	serve.Dir, serve.Env = server, bdEnv(server)
	stdout, err := serve.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	startHostedSnapshotProcess(t, serve)
	endpoint := ""
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if _, url, ok := strings.Cut(scanner.Text(), "listening on "); ok {
			endpoint = strings.TrimSpace(url)
			break
		}
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	if endpoint != "http://127.0.0.1:"+strconv.Itoa(servePort) {
		t.Fatalf("bd serve listens at %q, want the picked port %d", endpoint, servePort)
	}

	// The city, connected to the hosted store over HTTP.
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	runBd(cityPath, "connect", endpoint)
	if out := runBd(cityPath, "list", "--json"); !strings.Contains(out, "hosted two") {
		t.Fatalf("the city's bd does not read the hosted store:\n%s", out)
	}
	t.Setenv("BEADS_DIR", filepath.Join(cityPath, ".beads"))
	t.Setenv("GC_TEST_BD_BIN", bd)
	// The test binary puts its own bd shim first on PATH (TestMain); the
	// city's bd runner must reach the real bd that `bd connect` configured.
	t.Setenv("PATH", filepath.Dir(bd)+string(os.PathListSeparator)+os.Getenv("PATH"))
	// This (OSS) build links no HTTP client for the beads library: the
	// enterprise build's bdhttp.Register wires one. So gc itself cannot open
	// the hosted store here, and the JSONL's read is the one stubbed seam: the
	// http name is registered as the distribution would (with an opener that
	// refuses), so the window and the lane checks see the city as it ships,
	// and the JSONL reads a store holding the rows bd serves. The exact copy,
	// `bd export` over HTTP through the city's own bd runner, is real.
	registerLibraryBackendForTest(t, "http")
	served := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, served, beads.Bead{Title: "hosted one", Type: "task"})
	mustCreateInfraBead(t, served, beads.Bead{Title: "hosted two", Type: "task"})
	stubInfraControllerPing(t, 0)
	stubSessionInventory(t, nil, true, nil)
	stubOtherCities(t)

	to := filepath.Join(t.TempDir(), "snapshots")
	request := storageOperatorRequest{CityPath: cityPath, Cfg: &config.City{}, FleetStopped: true}
	var out, errOut bytes.Buffer
	if code := doStorageSnapshot(context.Background(), request, snapshotOptions{Work: true, To: to, Keep: 2}, "gc storage snapshot", &out, &errOut); code != 0 {
		t.Fatalf("snapshot of the hosted work store: exit %d\n%s\n%s", code, out.String(), errOut.String())
	}
	id, m := onlySnapshot(t, to)
	rec := m.Stores[0]
	if rec.Name != "work" || rec.Exact.Method != snapshotMethodBdExport || rec.Exact.File == nil {
		t.Fatalf("the hosted work store's exact copy = %+v, want bd export", rec.Exact)
	}
	export := string(mustReadFile(t, filepath.Join(to, id, rec.Exact.File.Path)))
	for _, title := range []string{"hosted one", "hosted two"} {
		if !strings.Contains(export, title) {
			t.Errorf("the bd export over HTTP does not hold %q:\n%s", title, export)
		}
	}
	if rec.Rows < 2 {
		t.Errorf("the JSONL holds %d row(s), want the 2 bd serves", rec.Rows)
	}
	if err := verifySnapshotFiles(context.Background(), filepath.Join(to, id), m); err != nil {
		t.Fatalf("the hosted snapshot does not verify: %v", err)
	}
	fmt.Fprintf(io.Discard, "%s", out.String())
}
