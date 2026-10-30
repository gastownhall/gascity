//go:build integration

package conversationlane_test

// The harness the conversation-lane evidence runs on: a city with one named
// session on a file-backed fake exec runtime, its sessions class bound to
// SQLite, to an embedded-Dolt beads workspace or to the bd-dolt arm's (a
// workspace bd provisioned on a dolt sql-server, served natively; X12's
// load, harness_bddolt_test.go), the controller run as
// `gc start --foreground` by the test-hooks gc, and the evidence read back
// from the city's own store and the fake's delivery log.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/pathutil"
	sqlitebinding "github.com/gastownhall/gascity/internal/storebinding/sqlite"
)

// storeArm is what serves the city's sessions class.
type storeArm string

const (
	armSQLite storeArm = "sqlite"
	armDolt   storeArm = "dolt"
	// armBdDolt is a beads workspace bd provisioned on a dolt sql-server the
	// city does not own, served natively over SQL: a direct-server city's
	// store, not the default topology's BdStore (harness_bddolt_test.go).
	armBdDolt storeArm = "bd-dolt"
)

// Activity modes of the fake runtime (fakeruntime's activity-mode file).
const (
	activityNone      = "none"
	activityLastNudge = "last-nudge"
)

const (
	// laneIDPrefix is the id prefix the city's infrastructure binding mints
	// under.
	laneIDPrefix = "gcg"
	// sessionName is the city's one named session.
	sessionName = "worker"
	// backstopSpeedup shortens the lane host's anti-entropy scan from 60 s
	// to 5 s; every lease keeps its production length.
	backstopSpeedup = "12"
	// failpointEnv is gc's failpoint variable.
	failpointEnv = "GC_LANE_FAILPOINT"
	// testHookActive is the line an armed failpoint prints.
	testHookActive = "TEST HOOK ACTIVE"
	// ctlLeaseRecovery and cliLeaseRecovery are how long after a Take a
	// lease of the controller's (2 min) or the CLI's (30 s) lapses, skew
	// included.
	ctlLeaseRecovery = opjournal.DefaultLeaseTTL + opjournal.LeaseSkew
	cliLeaseRecovery = 30*time.Second + opjournal.LeaseSkew
	// settleWindow is how long a case waits to see that nothing more
	// happens.
	settleWindow = 5 * time.Second
)

// cityParallel bounds how many cities one test binary runs at once.
var cityParallel = make(chan struct{}, 8)

// parallelCity marks t parallel and holds one of the city slots until it
// ends.
func parallelCity(t *testing.T) {
	t.Helper()
	t.Parallel()
	cityParallel <- struct{}{}
	t.Cleanup(func() { <-cityParallel })
}

// cityOptions shapes a city beyond the lane evidence's default. The zero
// value is the X2, X6 and X8 city.
type cityOptions struct {
	// legacyOps omits [ops] authoritative, so message requests take their
	// O0 (legacy) paths.
	legacyOps bool
	// opsMode, when set, is written as [ops] content, control and
	// lifecycle ("off", "auto" or "require").
	opsMode string
	// apiPort, when > 0, serves the controller's API on 127.0.0.1:<port>.
	apiPort int
	// onDemand names extra on_demand named sessions of the worker template.
	onDemand []string
	// opTypeMissing provisions a Dolt arm's workspace without the op bead
	// type, so the op journal cannot propose.
	opTypeMissing bool
	// realCadences runs controllers without GC_TEST_BACKSTOP_SPEEDUP.
	realCadences bool
	// pool runs the city's sessions on real tmux, under a socket root of
	// the city's own, so the v2 reconciler and its work ladder run (v2
	// refuses the exec runtime), and adds a pool of one seat that writes
	// what is typed into it to a file (ladder_lane_test.go).
	pool bool
}

// laneCity is one isolated city.
type laneCity struct {
	t     *testing.T
	arm   storeArm
	opts  cityOptions
	root  string
	dir   string
	state string
	gc    string
	// env is the environment every gc of the city runs with.
	env  []string
	ctl  *ctlProc
	logs int
	// procs are every controller the city started, for the cleanup.
	procs []*ctlProc
	// conv and token are the named session's bead id and instance token,
	// read once the controller started it.
	conv  string
	token string
	// doltServer is the bd-dolt arm's dolt sql-server, stopped after the
	// city's controllers.
	doltServer *doltServer
	// tmuxRoot is a pool city's TMUX_TMPDIR: its tmux servers' sockets
	// live there and nowhere else, so the cleanup kills only those.
	tmuxRoot string
	// mu guards logs, procs, conv and token.
	mu sync.Mutex
	// doltMu serializes the test's own store handles on the Dolt arm:
	// embedded Dolt serves one open handle per process at a time. SQLite
	// handles run concurrently, so a stream may hold one while a restart
	// reads the store.
	doltMu sync.Mutex
}

// ctlProc is one controller process the test started.
type ctlProc struct {
	cmd       *exec.Cmd
	done      chan struct{}
	log       string
	failpoint string
	// fpEnv is the variable failpoint was set as.
	fpEnv string
}

func newLaneCity(t *testing.T, arm storeArm, activity string) *laneCity {
	t.Helper()
	return newLaneCityWith(t, arm, activity, cityOptions{})
}

func newLaneCityWith(t *testing.T, arm storeArm, activity string, opts cityOptions) *laneCity {
	t.Helper()
	gcBin := bazeltest.DataPath(t, gcBinEnv)
	fake := bazeltest.DataPath(t, fakeRuntimeEnv)
	root, err := os.MkdirTemp("", "gcl")
	if err != nil {
		t.Fatalf("creating the city root: %v", err)
	}
	c := &laneCity{t: t, arm: arm, opts: opts, root: root, dir: filepath.Join(root, "c"), state: filepath.Join(root, "rt"), gc: gcBin}
	for _, d := range []string{filepath.Join(c.dir, ".gc"), c.state, filepath.Join(root, "home"), filepath.Join(root, "run")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}
	t.Cleanup(c.cleanup)
	if opts.pool {
		c.preparePool()
	}
	wrapper := filepath.Join(c.dir, ".gc", "fake-runtime.sh")
	c.writeFile(wrapper, fmt.Sprintf("#!/bin/sh\nexec %q --state %q \"$@\"\n", fake, c.state), 0o755)
	c.setActivityMode(activity)
	c.writeFile(filepath.Join(c.dir, "city.toml"), c.cityTOML(wrapper), 0o644)
	switch arm {
	case armDolt:
		c.provisionWorkspace()
	case armBdDolt:
		c.provisionServedWorkspace()
	}
	c.env = []string{
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.TempDir(),
		"HOME=" + filepath.Join(root, "home"),
		"GC_HOME=" + filepath.Join(root, "home"),
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"),
		"GC_BEADS=file",
		"GC_DOLT=skip",
		"DO_NOT_TRACK=1",
	}
	if c.tmuxRoot != "" {
		c.env = append(c.env, "TMUX_TMPDIR="+c.tmuxRoot)
	}
	return c
}

// cityTOML is the city's whole configuration: no supervisor, the file
// work store, the fake runtime, message on the lane, the five
// infrastructure classes on one binding, and one always-on named session
// with no prompt and no work. The city's options change the [ops] table,
// add an [api] table and add on_demand named sessions.
func (c *laneCity) cityTOML(wrapper string) string {
	binding := "provider = \"sqlite-beads\"\npath = \".gc/lanes\"\n"
	if c.arm == armDolt || c.arm == armBdDolt {
		binding = "provider = \"beads-workspace\"\nconfig_ref = \"lanes\"\n"
	}
	ops := "[ops]\nauthoritative = [\"message\"]\n"
	if c.opts.legacyOps {
		ops = ""
		if m := c.opts.opsMode; m != "" {
			ops = fmt.Sprintf("[ops]\ncontent = %q\ncontrol = %q\nlifecycle = %q\n", m, m, m)
		}
	}
	if c.opts.apiPort > 0 {
		ops += fmt.Sprintf("\n[api]\nport = %d\nbind = \"127.0.0.1\"\n", c.opts.apiPort)
	}
	provider := "exec:" + wrapper
	var extra strings.Builder
	if c.opts.pool {
		provider = "tmux"
		extra.WriteString(c.poolTOML())
	}
	for _, name := range c.opts.onDemand {
		fmt.Fprintf(&extra, "\n[[named_session]]\nname = %q\ntemplate = %q\nmode = \"on_demand\"\n", name, sessionName)
	}
	return fmt.Sprintf(`[workspace]
name = %q

[beads]
provider = "file"

[session]
provider = %q

%s
[storage.classes]
work = "work"
graph = "lanes"
sessions = "lanes"
messaging = "lanes"
orders = "lanes"
nudges = "lanes"

[storage.bindings.lanes]
%s
[[agent]]
name = %q
start_command = "sleep 100000"

[[named_session]]
template = %q
mode = "always"
%s`, filepath.Base(c.root), provider, ops, binding, sessionName, sessionName, extra.String())
}

// workspaceRoot is the Dolt arms' beads workspace (beadsworkspace's
// <city>/.gc/storage/<config_ref>).
func (c *laneCity) workspaceRoot() string { return filepath.Join(c.dir, ".gc", "storage", "lanes") }

// bindingDir is where the sessions binding's data sits on disk: the SQLite
// binding, the embedded workspace's .beads, or the bd-dolt arm's server
// data dir (that arm's workspace holds only bd's configuration).
func (c *laneCity) bindingDir() string {
	switch c.arm {
	case armDolt:
		return filepath.Join(c.workspaceRoot(), ".beads")
	case armBdDolt:
		return c.doltServer.dataDir
	default:
		return filepath.Join(c.dir, ".gc", "lanes")
	}
}

// provisionWorkspace gives the Dolt arm's workspace the configuration a
// serving workspace needs: the embedded backend, the binding's id prefix and
// its bead types (customTypes).
func (c *laneCity) provisionWorkspace() {
	c.t.Helper()
	root := c.workspaceRoot()
	c.writeFile(filepath.Join(root, ".beads", "metadata.json"), "{}\n", 0o644)
	ctx := context.Background()
	storage, err := beads.OpenNativeStorage(ctx, root, nil)
	if err != nil {
		c.t.Fatalf("opening the beads workspace at %s: %v", root, err)
	}
	defer storage.Close() //nolint:errcheck // fixture
	if err := storage.SetConfig(ctx, "issue_prefix", laneIDPrefix); err != nil {
		c.t.Fatalf("configuring the workspace id prefix: %v", err)
	}
	encoded, err := json.Marshal(c.customTypes())
	if err != nil {
		c.t.Fatal(err)
	}
	if err := storage.SetConfig(ctx, "types.custom", string(encoded)); err != nil {
		c.t.Fatalf("configuring the workspace bead types: %v", err)
	}
}

func (c *laneCity) writeFile(path, body string, mode os.FileMode) {
	c.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		c.t.Fatalf("writing %s: %v", path, err)
	}
}

func (c *laneCity) setActivityMode(mode string) {
	c.writeFile(filepath.Join(c.state, "activity-mode"), mode+"\n", 0o644)
}

// setActivity sets the fake runtime's last activity, which makes it busy
// for gc's quiescence window after at.
func (c *laneCity) setActivity(at time.Time) {
	c.writeFile(filepath.Join(c.state, "activity."+sessionName), at.UTC().Format(time.RFC3339Nano), 0o644)
}

// cleanup stops the controller, kills anything the test started that is
// still alive, and removes the city.
func (c *laneCity) cleanup() {
	c.mu.Lock()
	procs := append([]*ctlProc(nil), c.procs...)
	c.mu.Unlock()
	for _, p := range procs {
		if p.exited() {
			continue
		}
		_, _, _ = c.run(nil, "stop", c.dir)
		select {
		case <-p.done:
		case <-time.After(30 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	}
	if c.doltServer != nil {
		c.doltServer.stop()
	}
	c.killTmuxServers()
	if c.t.Failed() {
		c.dumpLogs()
	}
	_ = os.RemoveAll(c.root)
}

// dumpLogs logs every controller log and the fake's records of a failed
// case.
func (c *laneCity) dumpLogs() {
	logs, _ := filepath.Glob(filepath.Join(c.root, "ctl-*.log"))
	logs = append(logs, filepath.Join(c.state, "deliveries.jsonl"))
	if c.doltServer != nil {
		logs = append(logs, c.doltServer.log)
	}
	for _, l := range logs {
		data, _ := os.ReadFile(l)
		if len(data) > 64<<10 {
			data = data[len(data)-64<<10:]
		}
		c.t.Logf("--- %s:\n%s", l, data)
	}
}

func (p *ctlProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// startController runs `gc start --foreground` with the backstop speedup
// and, when set, failpoint, and waits until the controller answers and the
// named session is up.
func (c *laneCity) startController(failpoint string) *ctlProc {
	c.t.Helper()
	if c.ctl != nil && !c.ctl.exited() {
		c.t.Fatal("startController: a controller is still running")
	}
	p, err := c.launchController(failpoint)
	if err != nil {
		c.t.Fatal(err)
	}
	c.ctl = p
	return p
}

// launchController starts a controller and waits for it to come up, without
// failing the test, so a goroutine may restart one.
func (c *laneCity) launchController(failpoint string) (*ctlProc, error) {
	return c.launchControllerWith(failpointEnv, failpoint)
}

// launchControllerWith is launchController with the failpoint set as the
// variable fpEnv. A city with an API port is up once its API answers too.
func (c *laneCity) launchControllerWith(fpEnv, failpoint string) (*ctlProc, error) {
	c.mu.Lock()
	c.logs++
	log := filepath.Join(c.root, fmt.Sprintf("ctl-%d.log", c.logs))
	c.mu.Unlock()
	f, err := os.Create(log)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", log, err)
	}
	cmd := exec.Command(c.gc, "start", "--foreground", c.dir)
	cmd.Dir = c.dir
	cmd.Env = append([]string(nil), c.env...)
	if !c.opts.realCadences {
		cmd.Env = append(cmd.Env, "GC_TEST_BACKSTOP_SPEEDUP="+backstopSpeedup)
	}
	if failpoint != "" {
		cmd.Env = append(cmd.Env, fpEnv+"="+failpoint)
	}
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("gc start --foreground: %w", err)
	}
	p := &ctlProc{cmd: cmd, done: make(chan struct{}), log: log, failpoint: failpoint, fpEnv: fpEnv}
	go func() {
		_ = cmd.Wait()
		_ = f.Close()
		close(p.done)
	}()
	c.mu.Lock()
	c.procs = append(c.procs, p)
	c.mu.Unlock()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if p.exited() {
			return p, fmt.Errorf("the controller exited during startup:\n%s", readFile(log))
		}
		reply, err := c.socket("ping")
		pinged := err == nil && reply == fmt.Sprint(cmd.Process.Pid)
		box := c.boxUp()
		// The store is read only once the controller answers: by then its
		// boot opened (and on a fresh city created) the sessions binding,
		// which a read racing that boot must never do itself.
		var active bool
		var readErr error
		if pinged && box {
			active, readErr = c.sessionActive()
		}
		if active && c.apiUp() {
			return p, nil
		}
		if time.Now().After(deadline) {
			return p, fmt.Errorf("the controller did not come up within 2m (ping %q %v, box %v, session active %v %v):\n%s", reply, err, box, active, readErr, readFile(log))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// killController sends the running controller SIGKILL and waits for it.
func (c *laneCity) killController() {
	c.t.Helper()
	if c.ctl == nil || c.ctl.exited() {
		c.t.Fatal("killController: no controller is running")
	}
	if err := c.ctl.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		c.t.Fatalf("killing the controller: %v", err)
	}
	<-c.ctl.done
}

// waitKilledBySignal waits for p to die by its own failpoint: SIGKILL, with
// the armed line in its log.
func (c *laneCity) waitKilledBySignal(p *ctlProc, timeout time.Duration) {
	c.t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		c.t.Fatalf("the controller did not die at %s within %s:\n%s", p.failpoint, timeout, readFile(p.log))
	}
	assertKilledByFailpointOf(c.t, p.fpEnv, p.cmd.ProcessState, readFile(p.log), p.failpoint)
}

// assertKilledByFailpoint fails unless state ended by SIGKILL and output
// carries the armed line for the lane failpoint.
func assertKilledByFailpoint(t *testing.T, state *os.ProcessState, output, failpoint string) {
	t.Helper()
	assertKilledByFailpointOf(t, failpointEnv, state, output, failpoint)
}

// assertKilledByFailpointOf fails unless state ended by SIGKILL and output
// carries the armed line for failpoint, set as the variable env.
func assertKilledByFailpointOf(t *testing.T, env string, state *os.ProcessState, output, failpoint string) {
	t.Helper()
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("the process ended %v, want SIGKILL at %s:\n%s", state, failpoint, output)
	}
	want := fmt.Sprintf("%s=%q: %s", env, failpoint, testHookActive)
	if !strings.Contains(output, want) {
		t.Fatalf("output lacks %q: the failpoint was not armed:\n%s", want, output)
	}
}

// cliResult is one gc CLI run.
type cliResult struct {
	stdout, stderr string
	state          *os.ProcessState
	err            error
}

func (r cliResult) code() int {
	if r.state == nil {
		return -1
	}
	return r.state.ExitCode()
}

// run runs gc in the city with extra env.
func (c *laneCity) run(extra []string, args ...string) (string, string, *os.ProcessState) {
	r := c.cli(extra, args...)
	return r.stdout, r.stderr, r.state
}

func (c *laneCity) cli(extra []string, args ...string) cliResult {
	cmd := exec.Command(c.gc, args...)
	cmd.Dir = c.dir
	cmd.Env = append(append([]string(nil), c.env...), extra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return cliResult{stdout: stdout.String(), stderr: stderr.String(), state: cmd.ProcessState, err: err}
}

// nudge runs gc session nudge to the named session.
func (c *laneCity) nudge(mode, key, payload string, extra ...string) cliResult {
	args := []string{"session", "nudge", "--delivery", mode}
	if key != "" {
		args = append(args, "--key", key)
	}
	return c.cli(extra, append(args, sessionName, payload)...)
}

// socketPath is the controller's socket, as gc places it.
func (c *laneCity) socketPath() string {
	canonical := pathutil.NormalizePathForCompare(c.dir)
	legacy := filepath.Join(canonical, ".gc", "controller.sock")
	if len(legacy) <= 100 {
		return legacy
	}
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join("/tmp", "gascity-controller", fmt.Sprintf("%x.sock", sum[:16]))
}

// socket sends one controller-socket command and returns its reply.
func (c *laneCity) socket(command string) (string, error) {
	conn, err := net.DialTimeout("unix", c.socketPath(), time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close() //nolint:errcheck // best effort
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(command + "\n")); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line), err
}

// ring rings the lane host for op id, as a producer does after its
// Propose; with no controller nothing answers.
func (c *laneCity) ring(id string) {
	_, _ = c.socket("lane:" + c.conv + ":" + id)
}

// apiUp reports whether the city's API answers its health check; a city
// without an API port has none to wait for.
func (c *laneCity) apiUp() bool {
	if c.opts.apiPort <= 0 {
		return true
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", c.opts.apiPort))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (c *laneCity) boxUp() bool {
	if c.opts.pool {
		return true // no fake runtime: the session's row says it is up
	}
	_, err := os.Stat(filepath.Join(c.state, "box."+sessionName))
	return err == nil
}

// withStore runs fn over a fresh handle on the sessions store and the op
// store over it, closed after.
func (c *laneCity) withStore(fn func(raw beads.Store, st *opjournal.Store) error) error {
	if c.arm == armDolt {
		c.doltMu.Lock()
		defer c.doltMu.Unlock()
	}
	var raw beads.Store
	var closeFn func() error
	switch c.arm {
	case armDolt, armBdDolt:
		s, err := beads.OpenNativeDoltStoreAt(context.Background(), c.workspaceRoot(), nil)
		if err != nil {
			return fmt.Errorf("opening the workspace: %w", err)
		}
		raw, closeFn = s, s.CloseStore
	default:
		graph, err := sqlitebinding.GraphPath(filepath.Join(c.dir, ".gc", "lanes"))
		if err != nil {
			return err
		}
		// The controller creates the binding; a handle here must never.
		if _, err := os.Stat(graph); err != nil {
			return fmt.Errorf("the SQLite binding %s: %w", graph, err)
		}
		s, err := beads.OpenSQLiteStore(filepath.Dir(graph), beads.WithSQLiteStoreIDPrefix(laneIDPrefix))
		if err != nil {
			return fmt.Errorf("opening the SQLite store: %w", err)
		}
		raw = s
		closeFn = func() error {
			if cl, ok := s.(interface{ CloseStore() error }); ok {
				return cl.CloseStore()
			}
			return nil
		}
	}
	defer closeFn() //nolint:errcheck // read handle
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: raw}, clock.Real{}, opjournal.WithIDPrefix(laneIDPrefix))
	if err != nil {
		return err
	}
	return fn(raw, st)
}

func (c *laneCity) mustStore(fn func(raw beads.Store, st *opjournal.Store) error) {
	c.t.Helper()
	if err := c.withStore(fn); err != nil {
		c.t.Fatalf("store: %v", err)
	}
}

// sessionActive reads the named session's row and records its id and token
// once it is active.
func (c *laneCity) sessionActive() (bool, error) {
	active := false
	err := c.withStore(func(raw beads.Store, _ *opjournal.Store) error {
		rows, err := raw.List(beads.ListQuery{Label: "gc:session"})
		if err != nil {
			return err
		}
		for _, b := range rows {
			state := b.Metadata["state"]
			if b.Metadata["session_name"] == sessionName && (state == "active" || state == "awake") && b.Status != "closed" {
				c.setSession(b.ID, b.Metadata["instance_token"])
				active = true
			}
		}
		return nil
	})
	return active, err
}

// setSession records the named session's id and token. A restart that
// finds them unchanged writes nothing, so a stream that reads them while a
// goroutine restarts the controller does not race it.
func (c *laneCity) setSession(conv, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conv != conv {
		c.conv = conv
	}
	if c.token != token {
		c.token = token
	}
}

// get reads op id.
func (c *laneCity) get(id string) opjournal.Op {
	c.t.Helper()
	var op opjournal.Op
	c.mustStore(func(_ beads.Store, st *opjournal.Store) error {
		var err error
		op, err = st.Get(id)
		return err
	})
	return op
}

// ops reads every op of the conversation, closed ones included.
func (c *laneCity) ops() []opjournal.Op {
	c.t.Helper()
	var ops []opjournal.Op
	c.mustStore(func(raw beads.Store, st *opjournal.Store) error {
		rows, err := raw.List(beads.ListQuery{Label: opjournal.ConversationLabel(c.conv), IncludeClosed: true})
		if err != nil {
			return err
		}
		for _, b := range rows {
			if b.Type != opjournal.BeadType || !slices.Contains(b.Labels, opjournal.LabelOp) {
				continue
			}
			op, err := st.Get(b.ID)
			if errors.Is(err, opjournal.ErrUnsupportedSchema) {
				continue
			}
			if err != nil {
				return err
			}
			if op.Kind == opjournal.KindMessage {
				ops = append(ops, op)
			}
		}
		return nil
	})
	return ops
}

// cliOpID is the id of the op the CLI proposes under key.
func (c *laneCity) cliOpID(key string) string {
	c.t.Helper()
	id, err := opjournal.DeriveID(laneIDPrefix, c.conv, "operator", key)
	if err != nil {
		c.t.Fatalf("deriving the op id: %v", err)
	}
	return id
}

// propose proposes a lane message directly, as an in-process producer
// would, and rings when ring is set.
func (c *laneCity) propose(req opjournal.Request, ring bool) opjournal.Op {
	c.t.Helper()
	req.Conversation, req.Kind, req.Executor = c.conv, opjournal.KindMessage, opjournal.ExecutorLane
	var op opjournal.Op
	c.mustStore(func(_ beads.Store, st *opjournal.Store) error {
		var err error
		op, _, err = st.Propose(req)
		return err
	})
	if ring {
		c.ring(op.ID)
	}
	return op
}

// noticeRequest is a background producer's lane notice: boundary, never
// waking the session.
func noticeRequest(principal, key, payload string, guarantee opjournal.Guarantee) opjournal.Request {
	return opjournal.Request{
		ClientKey: key,
		Producer:  opjournal.Producer{Principal: principal, Actor: opjournal.ActorBackground},
		Payload:   payload,
		Policy: opjournal.Policy{Guarantee: guarantee, Message: opjournal.MessagePolicy{
			Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeLiveOnly,
		}},
	}
}

// waitClosed waits until every op in ids is closed.
func (c *laneCity) waitClosed(timeout time.Duration, ids ...string) []opjournal.Op {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var ops []opjournal.Op
		open := 0
		c.mustStore(func(_ beads.Store, st *opjournal.Store) error {
			for _, id := range ids {
				op, err := st.Get(id)
				if err != nil {
					return err
				}
				if op.Status != opjournal.StatusClosed {
					open++
				}
				ops = append(ops, op)
			}
			return nil
		})
		if open == 0 {
			return ops
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%d of %d ops still open after %s: %+v", open, len(ids), timeout, ops)
		}
		time.Sleep(time.Second)
	}
}

// delivery is one line of the fake runtime's delivery log.
type delivery struct {
	Seq     int    `json:"seq"`
	At      string `json:"at"`
	Session string `json:"session"`
	Text    string `json:"text"`
}

func (d delivery) at(t *testing.T) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, d.At)
	if err != nil {
		t.Fatalf("delivery time %q: %v", d.At, err)
	}
	return at
}

// deliveries reads the fake's delivery log.
func (c *laneCity) deliveries() []delivery {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.state, "deliveries.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		c.t.Fatalf("reading the delivery log: %v", err)
	}
	var out []delivery
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var d delivery
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			c.t.Fatalf("delivery line %q: %v", line, err)
		}
		out = append(out, d)
	}
	return out
}

// deliveriesOf are the deliveries whose text carries payload.
func deliveriesOf(all []delivery, payload string) []delivery {
	var out []delivery
	for _, d := range all {
		if strings.Contains(d.Text, payload) {
			out = append(out, d)
		}
	}
	return out
}

// render is the text a delivery of ops (batch order) carries.
func render(t *testing.T, note bool, ops ...opjournal.Op) string {
	t.Helper()
	text, err := opjournal.RenderDelivery(ops, opjournal.TextReminder, note)
	if err != nil {
		t.Fatalf("rendering %d ops: %v", len(ops), err)
	}
	return text
}

// assertNoIdentifier fails when any delivered text carries an identifier
// of the conversation (I14): an op id, client key or delivery key, a lease
// or its nonce, a principal with a scheme, the conversation id or instance
// token, or the lane's counters.
func (c *laneCity) assertNoIdentifier(ops []opjournal.Op) {
	c.t.Helper()
	ids := []string{c.conv, "lane_seq", "attempt"}
	if c.token != "" {
		ids = append(ids, c.token)
	}
	for _, op := range ops {
		ids = append(ids, op.ID, op.ClientKey)
		if op.DeliveryKey != "" {
			ids = append(ids, op.DeliveryKey, "gc:"+op.DeliveryKey)
		}
		if !op.Lease.IsZero() {
			ids = append(ids, op.Lease.String(), op.Lease.Nonce)
		}
		if strings.Contains(op.Producer.Principal, ":") {
			ids = append(ids, op.Producer.Principal)
		}
		for _, tr := range op.Transitions {
			if _, nonce, ok := strings.Cut(tr.By, "."); ok && strings.HasPrefix(tr.By, "lane:") {
				host := strings.TrimPrefix(strings.SplitN(tr.By, ".", 2)[0], "lane:")
				ids = append(ids, nonce, "#"+host+".")
			}
		}
	}
	for _, d := range c.deliveries() {
		for _, id := range ids {
			if id != "" && strings.Contains(d.Text, id) {
				c.t.Fatalf("delivery %d carries identifier %q:\n%s", d.Seq, id, d.Text)
			}
		}
	}
}

func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}
