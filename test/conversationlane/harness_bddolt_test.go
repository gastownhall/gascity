//go:build integration

package conversationlane_test

// The bd-dolt arm: a beads workspace that the pinned bd's `bd init --server`
// provisioned against a dolt sql-server the harness runs, served natively.
// bd runs only to provision it (init and the custom types); the city binds
// its sessions class to the workspace through a beads-workspace binding,
// whose engine opens the server in-process over SQL
// (internal/storebinding/beadsworkspace: OpenNativeDoltStoreAtWithoutAmbientEnv),
// so no request of the load goes through bd. That is the store a
// direct-server city gets. It is not the default real-city topology: there
// the sessions class stays on the work binding, served by the bd provider's
// store factory, which falls back to BdStore on a proxied-local scope
// (internal/beads/factory.go; GC_BEADS_PROXIED_NATIVE is off by default),
// and the pinned bd lacks `--if-revision`, so a BdStore makes no conditional
// writes and the op journal refuses it (internal/beads/bdstore_conditional.go,
// ga-ve3tjw). Provisioning follows the repo's bd-served integration fixtures
// (test/integration/bdstore_test.go: startSharedDoltServer, git init, then
// runBDInit) with the pinned bd and dolt they run, which
// conversationlane_test's integration build stages (BUILD.bazel).

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/loopbackport"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/test/toolhome"
)

const (
	// laneBDBinEnv and laneDoltBinEnv name the pinned bd and dolt by
	// $(rootpath); the integration build stages them.
	laneBDBinEnv   = "GC_LANE_BD_BIN"
	laneDoltBinEnv = "GC_LANE_DOLT_BIN"
	// doltServerReady bounds the server's start; bdProvisionTimeout one bd
	// command against it.
	doltServerReady    = 60 * time.Second
	bdProvisionTimeout = 2 * time.Minute
)

// laneCustomTypes are the bead types a lane city's workspace registers: the
// ones gc registers with every bd store it provisions, and the coordination
// types the lane's classes write that that set does not carry.
var laneCustomTypes = append(slices.Clone(beads.RequiredCustomTypes), "wisp", "wait", "order", "nudge")

// customTypes are the bead types the city's workspace registers: all of
// laneCustomTypes, less the op type when the city is provisioned without it
// (cityOptions.opTypeMissing).
func (c *laneCity) customTypes() []string {
	if !c.opts.opTypeMissing {
		return laneCustomTypes
	}
	return slices.DeleteFunc(slices.Clone(laneCustomTypes), func(t string) bool { return t == opjournal.BeadType })
}

// doltServer is one dolt sql-server a city's workspace is served by.
type doltServer struct {
	cmd     *exec.Cmd
	done    chan struct{}
	port    int
	dataDir string
	log     string
}

// provisionServedWorkspace starts the city's dolt sql-server and makes the
// sessions binding's workspace against it with bd, as an operator does: a
// git repository for the workspace (bd init commits into the repository it
// finds, so without one it would commit into whatever repository encloses
// the city), `bd init --server` under the binding's id prefix, then the
// workspace's bead types.
func (c *laneCity) provisionServedWorkspace() {
	c.t.Helper()
	bd := bazeltest.DataPath(c.t, laneBDBinEnv)
	dolt := bazeltest.DataPath(c.t, laneDoltBinEnv)
	if bd == "" || dolt == "" {
		c.t.Fatalf("the %s arm needs %s and %s (BUILD.bazel stages them in conversationlane_test's integration build)", armBdDolt, laneBDBinEnv, laneDoltBinEnv)
	}
	home := filepath.Join(c.root, "tool-home")
	if err := testutil.SeedDoltGlobalConfig(home); err != nil {
		c.t.Fatal(err)
	}
	root := c.workspaceRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		c.t.Fatalf("creating the workspace %s: %v", root, err)
	}
	env := toolhome.Environ(bdToolBase(c.root), home)
	srv, err := startDoltServer(dolt, filepath.Join(c.root, "dolt-data"), filepath.Join(c.root, "dolt-server.log"), env)
	if err != nil {
		c.t.Fatal(err)
	}
	c.doltServer = srv
	c.runTool(env, root, "git", "init", "--quiet")
	c.runTool(env, root, bd, "init", "--server", "--server-host", "127.0.0.1", "--server-port", strconv.Itoa(srv.port), "-p", laneIDPrefix, "--skip-hooks", "--skip-agents")
	c.runTool(env, root, bd, "config", "set", "types.custom", strings.Join(c.customTypes(), ","))
}

// bdToolBase is the environment bd, git and dolt run with, before
// toolhome.Environ pins their home: PATH and TMPDIR, the git template dir
// the test environment names (if any), an author identity for bd init's
// commit, dolt's root at the tool home, and git's discovery fenced below
// the city root, so no git a provisioning runs reaches an enclosing
// repository.
func bdToolBase(cityRoot string) []string {
	home := filepath.Join(cityRoot, "tool-home")
	base := []string{
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.TempDir(),
		"DOLT_ROOT_PATH=" + home,
		"GIT_CEILING_DIRECTORIES=" + cityRoot,
		"GIT_AUTHOR_NAME=gc-test", "GIT_AUTHOR_EMAIL=gc-test@test.local",
		"GIT_COMMITTER_NAME=gc-test", "GIT_COMMITTER_EMAIL=gc-test@test.local",
	}
	if v, ok := os.LookupEnv("GIT_TEMPLATE_DIR"); ok {
		base = append(base, "GIT_TEMPLATE_DIR="+v)
	}
	return base
}

// runTool runs one provisioning command in dir and fails the test on its
// error.
func (c *laneCity) runTool(env []string, dir, bin string, args ...string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bdProvisionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("%s %v in %s: %v\n%s", filepath.Base(bin), args, dir, err, out)
	}
}

// doltServerConfig is the sql-server's configuration: loopback on port,
// over dataDir, with automatic garbage collection and the stats workers off
// as gc's managed server turns them off (cmd/gc writeManagedDoltConfigFile:
// the auto-GC behavior, its system variable and the four stats switches),
// so the data dir's size is what the city wrote. R3 measures it per round:
// a GC mid-round compacts it, so the round's growth goes negative, and a
// persisted stats write grows it with no op.
func doltServerConfig(port int, dataDir string) string {
	return fmt.Sprintf(`log_level: info
behavior:
  auto_gc_behavior:
    enable: false
system_variables:
  dolt_auto_gc_enabled: "OFF"
  dolt_stats_enabled: "OFF"
  dolt_stats_gc_enabled: "OFF"
  dolt_stats_memory_only: "ON"
  dolt_stats_paused: "ON"
listener:
  host: 127.0.0.1
  port: %d
data_dir: %q
`, port, dataDir)
}

// startDoltServer starts dolt sql-server on a leased loopback port over
// dataDir and waits until it accepts connections.
func startDoltServer(dolt, dataDir, logPath string, env []string) (*doltServer, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the dolt data dir: %w", err)
	}
	port, err := loopbackport.Reserve()
	if err != nil {
		return nil, fmt.Errorf("reserving the dolt port: %w", err)
	}
	// Beside the data dir, not in it: the data dir is what R3 measures.
	config := filepath.Join(filepath.Dir(dataDir), "dolt-server.yaml")
	if err := os.WriteFile(config, []byte(doltServerConfig(port, dataDir)), 0o644); err != nil {
		return nil, fmt.Errorf("writing the dolt server config: %w", err)
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("creating the dolt log: %w", err)
	}
	cmd := exec.Command(dolt, "sql-server", "--config", config)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dataDir, env, logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("starting dolt sql-server: %w", err)
	}
	s := &doltServer{cmd: cmd, done: make(chan struct{}), port: port, dataDir: dataDir, log: logPath}
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
		close(s.done)
	}()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(doltServerReady)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return s, nil
		}
		select {
		case <-s.done:
			return nil, fmt.Errorf("dolt sql-server on %s exited before it accepted a connection:\n%s", addr, readFile(logPath))
		default:
		}
		if time.Now().After(deadline) {
			s.stop()
			return nil, fmt.Errorf("dolt sql-server on %s accepted no connection within %s:\n%s", addr, doltServerReady, readFile(logPath))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop ends the server: SIGTERM, then SIGKILL after 30 s.
func (s *doltServer) stop() {
	select {
	case <-s.done:
		return
	default:
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
	}
}
