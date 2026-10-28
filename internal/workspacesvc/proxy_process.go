package workspacesvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/processgroup"
)

const (
	proxyProcessReadyTimeout   = 5 * time.Second
	proxyProcessRestartBackoff = 1 * time.Second
	proxyProcessShutdownWait   = 2 * time.Second
)

var errProxyProcessExitedEarly = errors.New("process exited before listener became ready")

// extraHelperEnv is an env-list builder that proxy_process tests can augment
// to inject additional KEY=VALUE entries into the helper subprocess
// environment (e.g. GC_TESTENV_PASSTHROUGH for internal/testenv). Production
// leaves it nil. Tests are serial so no locking is needed.
var extraHelperEnv []string

// releaseProxyProcessTree ends the launcher's ownership of a generation's
// process tree after its leader has exited, reaping whatever the leader left
// running; see processgroup.ReleaseCommand. Tests substitute a release that is
// delayed or fails.
var releaseProxyProcessTree = processgroup.ReleaseCommand

// stopProxyProcessTree stops a generation's process tree when its instance is
// closed. Tests substitute a stop that fails.
var stopProxyProcessTree = stopProcessGroup

// proxyProcessReapWait bounds how long Close waits for the generation it
// stopped to finish its release. Termination is confirmed by then, so this
// only has to outlast the release's own confirmation.
const proxyProcessReapWait = 10 * time.Second

// serviceReaps tracks, per service state root, the generations whose leader
// has exited but whose tree is not confirmed reaped: those a release is still
// working on, and those a release could not confirm. An unconfirmed tree stays
// owned by processgroup, so a later release can still finish the reap. No
// generation starts under that state root while either kind remains: starting
// beside a tree that may still be running would duplicate the service. The set
// is keyed by state root rather than held by an instance because a replacement
// instance must wait for it too, and by serviceRootKey because two spellings of
// one directory are one service.
var serviceReaps = struct {
	sync.Mutex
	releasing   map[string]int
	unconfirmed map[string][]*exec.Cmd
}{releasing: map[string]int{}, unconfirmed: map[string][]*exec.Cmd{}}

type proxyProcessInstance struct {
	rt           RuntimeContext
	svc          config.Service
	publication  Status
	absStateRoot string
	reapKey      string
	socketPath   string
	healthPath   string
	transport    *http.Transport

	mu          sync.Mutex
	cmd         *exec.Cmd
	doneCh      chan struct{}
	nextRestart time.Time
	closed      bool
	status      Status
}

// proxyProcessStateRoot is the absolute state root of a proxy_process service.
func proxyProcessStateRoot(rt RuntimeContext, svc config.Service) string {
	absRoot := svc.StateRootOrDefault()
	if !filepath.IsAbs(absRoot) {
		absRoot = filepath.Join(rt.CityPath(), absRoot)
	}
	return absRoot
}

func newProxyProcessInstance(rt RuntimeContext, svc config.Service, publication Status) (Instance, error) {
	absRoot := proxyProcessStateRoot(rt, svc)
	socketPath, err := allocateProxyProcessSocketPath(rt.CityPath(), svc.Name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		if cleanupErr := cleanupProxyProcessSocketPath(socketPath); cleanupErr != nil {
			return nil, fmt.Errorf("create socket dir: %w; %w", err, cleanupErr)
		}
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	inst := &proxyProcessInstance{
		rt:           rt,
		svc:          svc,
		publication:  publication,
		absStateRoot: absRoot,
		reapKey:      serviceRootKey(absRoot),
		socketPath:   socketPath,
		healthPath:   svc.Process.HealthPath,
		transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		status: Status{
			ServiceName: svc.Name,
			Kind:        svc.KindOrDefault(),
			State:       "starting",
			LocalState:  "starting",
			UpdatedAt:   time.Now().UTC(),
		},
	}
	if err := inst.start(time.Now().UTC()); err != nil {
		if cleanupErr := cleanupProxyProcessSocketPath(socketPath); cleanupErr != nil {
			return nil, fmt.Errorf("%w; %w", err, cleanupErr)
		}
		return nil, err
	}
	return inst, nil
}

func (p *proxyProcessInstance) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.status
	out.UpdatedAt = time.Now().UTC()
	return out
}

func (p *proxyProcessInstance) HandleHTTP(w http.ResponseWriter, r *http.Request, subpath string) bool {
	p.mu.Lock()
	ready := !p.closed && p.cmd != nil && p.status.LocalState == "ready"
	reason := p.status.Reason
	transport := p.transport
	p.mu.Unlock()

	if !ready {
		http.Error(w, serviceUnavailableMessage(reason), http.StatusServiceUnavailable)
		return true
	}

	target := &url.URL{Scheme: "http", Host: "gc-service"}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, fmt.Sprintf("service unavailable: %v", err), http.StatusBadGateway)
		},
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(target)
			req.Out.URL.Path = subpath
			req.Out.URL.RawPath = subpath
			req.Out.Host = ""
		},
	}
	proxy.ServeHTTP(w, r)
	return true
}

func (p *proxyProcessInstance) Tick(_ context.Context, now time.Time) {
	p.mu.Lock()
	closed := p.closed
	shouldStart := p.cmd == nil && (p.nextRestart.IsZero() || !now.Before(p.nextRestart))
	p.mu.Unlock()
	if closed {
		return
	}
	if shouldStart {
		if err := p.start(now); err != nil {
			p.mu.Lock()
			p.status.State = "degraded"
			p.status.LocalState = "degraded"
			p.status.Reason = err.Error()
			p.status.UpdatedAt = now
			p.nextRestart = now.Add(proxyProcessRestartBackoff)
			p.mu.Unlock()
		}
		return
	}
	if err := p.checkHealth(now); err != nil {
		p.mu.Lock()
		if !p.closed && p.cmd != nil {
			p.status.State = "degraded"
			p.status.LocalState = "degraded"
			p.status.Reason = err.Error()
			p.status.UpdatedAt = now
		}
		p.mu.Unlock()
	}
}

func (p *proxyProcessInstance) Close() error {
	p.mu.Lock()
	p.closed = true
	cmd := p.cmd
	doneCh := p.doneCh
	p.cmd = nil
	p.status.State = "stopped"
	p.status.LocalState = "stopped"
	p.status.Reason = "service_closed"
	p.status.UpdatedAt = time.Now().UTC()
	p.mu.Unlock()

	removeErr := cleanupProxyProcessSocketPath(p.socketPath)
	var stopErr error
	if cmd != nil {
		// An owned tree is released by the generation's reaper once its
		// leader has exited, and that release decides whether the tree was
		// reaped, so the service is not stopped until it has finished.
		owned := processgroup.HoldsTree(cmd)
		stopErr = stopProxyProcessTree(cmd)
		switch {
		case stopErr == nil && owned:
			stopErr = awaitGenerationReaper(cmd, doneCh, proxyProcessReapWait)
		case stopErr != nil && owned:
			// A tree the stop could not confirm empty is still owned, and its
			// leader may still be running, in which case its reaper has not
			// registered it. It is held pending here instead, so nothing
			// starts under this state root until a release confirms it; the
			// settle below makes the first attempt, and its result stands.
			finishServiceRelease(p.reapKey, cmd, false, stopErr)
			stopErr = nil
		}
	}
	// A generation whose reap was never confirmed may still be running, so
	// the service is not stopped until a release confirms it.
	if reapErr := settleServiceReaps(p.reapKey); reapErr != nil {
		if stopErr == nil {
			stopErr = reapErr
		} else {
			stopErr = fmt.Errorf("%w; %w", stopErr, reapErr)
		}
	}
	if stopErr != nil {
		if removeErr != nil {
			return fmt.Errorf("%w; %w", stopErr, removeErr)
		}
		return stopErr
	}
	return removeErr
}

func (p *proxyProcessInstance) start(now time.Time) error {
	if err := settleServiceReaps(p.reapKey); err != nil {
		return err
	}
	// Survivors of a previous supervisor hard exit keep running re-parented
	// to init; spawning next to them accumulates duplicates (ga-mukg0s).
	// Best-effort sweep before every spawn, scoped to this instance's state
	// root so sibling cities' orphans are untouched; see orphan_reap.go.
	reapOrphanedServiceProcesses(newOrphanIdentity(p.svc.Name, p.absStateRoot, p.svc.Process.Command))

	logFile, err := os.OpenFile(filepath.Join(p.absStateRoot, "logs", "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("open service log: %w", err)
	}
	_ = os.Remove(p.socketPath)

	cmd := exec.Command(p.svc.Process.Command[0], p.svc.Process.Command[1:]...)
	cmd.Dir = p.commandDir()
	cmd.Env = append(os.Environ(), citylayout.CityRuntimeEnvForRuntimeDir(p.rt.CityPath(), citylayout.TrustedAmbientCityRuntimeDir(p.rt.CityPath()))...)
	cmd.Env = append(cmd.Env,
		"GC_SERVICE_NAME="+p.svc.Name,
		"GC_SERVICE_STATE_ROOT="+p.absStateRoot,
		// Scaffolded at 0700 by ensureStateRoot; the sanctioned home for
		// pack-managed credentials (bot tokens etc.).
		"GC_SERVICE_SECRETS_DIR="+filepath.Join(p.absStateRoot, "secrets"),
		"GC_SERVICE_RUN_ROOT="+filepath.Join(p.absStateRoot, "run"),
		"GC_SERVICE_SOCKET="+p.socketPath,
		"GC_SERVICE_URL_PREFIX="+citylayout.PublicServiceMountPath(p.rt.CityName(), p.svc.Name),
		"GC_SERVICE_PUBLIC_URL="+p.publication.URL,
		"GC_SERVICE_VISIBILITY="+p.publication.Visibility,
		"GC_PUBLISHED_SERVICES_DIR="+citylayout.PublishedServicesDir(p.rt.CityPath()),
	)
	cmd.Env = append(cmd.Env, extraHelperEnv...)
	cmd.Env = execenv.WithUsageMetricsDisabled(cmd.Env)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = proxyProcessSysProcAttr()
	// Own the child's whole tree and reap it when ownership is released. This
	// is the portable half of the guarantee Pdeathsig gives on Linux: a
	// supervisor that dies without running cleanup must not leave the service
	// process — or anything it spawned — behind to be duplicated on the next
	// start.
	if _, err := processgroup.StartCommand(cmd, processgroup.KillOnRelease); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start process: %w", err)
	}

	p.mu.Lock()
	p.cmd = cmd
	doneCh := make(chan struct{})
	p.doneCh = doneCh
	p.nextRestart = time.Time{}
	p.status.State = "starting"
	p.status.LocalState = "starting"
	p.status.Reason = ""
	p.status.UpdatedAt = now
	p.mu.Unlock()

	// reapErr is the generation's release result, final once doneCh closes.
	var reapErr error
	go func(cmd *exec.Cmd, logFile *os.File, doneCh chan struct{}) {
		defer close(doneCh)
		err := cmd.Wait()
		_ = logFile.Close()
		// The service process is gone. Releasing ownership reaps whatever it
		// spawned — the guarantee KillOnRelease was chosen for at the start
		// site, and what keeps a restart from duplicating a helper the old
		// instance left running. The release finishes before the generation
		// is retired or doneCh closes, so no restart can overtake it and no
		// waiter sees the generation finished while its tree may still be
		// running. While it runs it is registered under the state root, so no
		// other instance starts there either. A reap it could not confirm
		// leaves the tree owned and the command registered as unconfirmed,
		// where the next start or Close for this state root retries it; until
		// one confirms, no replacement starts. The job's kill-on-close is the
		// backstop if none ever does, and it fires only when this process
		// exits. Only a release over a tree processgroup still holds does any
		// work, so only such a release is registered.
		counted := processgroup.HoldsTree(cmd)
		if counted {
			beginServiceRelease(p.reapKey)
		}
		reapErr = releaseProxyProcessTree(cmd)
		finishServiceRelease(p.reapKey, cmd, counted, reapErr)

		p.mu.Lock()
		defer p.mu.Unlock()
		if p.cmd == cmd {
			p.cmd = nil
			if !p.closed {
				p.status.State = "degraded"
				p.status.LocalState = "degraded"
				p.status.Reason = processExitReason(err)
				p.status.UpdatedAt = time.Now().UTC()
				p.nextRestart = time.Now().UTC().Add(proxyProcessRestartBackoff)
			}
		}
		if reapErr != nil {
			p.status.Reason = fmt.Sprintf("%s; %v", p.status.Reason, reapErr)
			p.status.UpdatedAt = time.Now().UTC()
		}
	}(cmd, logFile, doneCh)

	if err := p.waitReady(now.Add(proxyProcessReadyTimeout)); err != nil {
		if !errors.Is(err, errProxyProcessExitedEarly) {
			_ = stopProcessGroup(cmd)
		} else if reapErr != nil {
			return fmt.Errorf("%w; %w", err, reapErr)
		}
		return err
	}

	p.mu.Lock()
	if p.cmd == cmd && !p.closed {
		p.status.State = "ready"
		p.status.LocalState = "ready"
		p.status.Reason = ""
		p.status.UpdatedAt = time.Now().UTC()
	}
	p.mu.Unlock()
	return nil
}

func (p *proxyProcessInstance) waitReady(deadline time.Time) error {
	for time.Now().Before(deadline) {
		p.mu.Lock()
		closed := p.closed
		doneCh := p.doneCh
		p.mu.Unlock()
		if closed {
			return errors.New("service closed")
		}
		select {
		case <-doneCh:
			return errProxyProcessExitedEarly
		default:
		}
		if conn, err := net.DialTimeout("unix", p.socketPath, 100*time.Millisecond); err == nil {
			_ = conn.Close()
			if p.healthPath == "" {
				return nil
			}
			if err := p.checkHealth(time.Now().UTC()); err == nil {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("service %q did not become ready before timeout", p.svc.Name)
}

func (p *proxyProcessInstance) checkHealth(now time.Time) error {
	if p.healthPath == "" {
		p.mu.Lock()
		if p.cmd != nil && !p.closed {
			p.status.State = "ready"
			p.status.LocalState = "ready"
			p.status.Reason = ""
			p.status.UpdatedAt = now
		}
		p.mu.Unlock()
		return nil
	}

	client := &http.Client{
		Timeout:   500 * time.Millisecond,
		Transport: p.transport,
	}
	req, err := http.NewRequest(http.MethodGet, "http://gc-service"+p.healthPath, nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}

	p.mu.Lock()
	if p.cmd != nil && !p.closed {
		p.status.State = "ready"
		p.status.LocalState = "ready"
		p.status.Reason = ""
		p.status.UpdatedAt = now
	}
	p.mu.Unlock()
	return nil
}

func (p *proxyProcessInstance) commandDir() string {
	if p.svc.SourceDir != "" {
		return p.svc.SourceDir
	}
	return p.rt.CityPath()
}

func allocateProxyProcessSocketPath(cityPath, serviceName string) (string, error) {
	sum := sha256.Sum256([]byte(cityPath))
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("gcsvc-%d", os.Getuid()), hex.EncodeToString(sum[:4]))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("chmod socket dir: %w", err)
	}
	prefix := config.NormalizePublicationLabel(serviceName, "svc")
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	tmp, err := os.CreateTemp(dir, prefix+"-*.sock")
	if err != nil {
		return "", fmt.Errorf("allocate socket path: %w", err)
	}
	path := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close socket path placeholder: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove socket path placeholder: %w", err)
	}
	return path, nil
}

func cleanupProxyProcessSocketPath(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove socket path: %w", err)
	}
	dir := filepath.Dir(path)
	root := filepath.Join(os.TempDir(), fmt.Sprintf("gcsvc-%d", os.Getuid()))
	if filepath.Dir(dir) != root {
		return nil
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) && !fsys.IsDirNotEmpty(err) {
		return fmt.Errorf("remove socket dir: %w", err)
	}
	if err := os.Remove(root); err != nil && !os.IsNotExist(err) && !fsys.IsDirNotEmpty(err) {
		return fmt.Errorf("remove socket root dir: %w", err)
	}
	return nil
}

func processExitReason(err error) string {
	if err == nil {
		return "process_exited"
	}
	return fmt.Sprintf("process exited: %v", err)
}

func serviceUnavailableMessage(reason string) string {
	if reason == "" {
		return "service unavailable"
	}
	return "service unavailable: " + reason
}

func stopProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return processgroup.TerminateCommand(cmd, 0, proxyProcessShutdownWait, processgroup.Options{})
}

// serviceRootKey is the form of a service state root the reap registry is
// keyed by: absolute, cleaned, with links resolved, and folded where the
// platform's paths are case-insensitive.
func serviceRootKey(root string) string {
	return foldServiceRootKey(pathutil.NormalizePathForCompare(root))
}

// beginServiceRelease registers a release starting under key.
func beginServiceRelease(key string) {
	serviceReaps.Lock()
	defer serviceReaps.Unlock()
	serviceReaps.releasing[key]++
}

// finishServiceRelease records the end of a release under key, counted if
// beginServiceRelease registered it. A release that failed leaves cmd pending
// until a later release confirms its tree reaped.
func finishServiceRelease(key string, cmd *exec.Cmd, counted bool, releaseErr error) {
	serviceReaps.Lock()
	defer serviceReaps.Unlock()
	if counted {
		if serviceReaps.releasing[key] <= 1 {
			delete(serviceReaps.releasing, key)
		} else {
			serviceReaps.releasing[key]--
		}
	}
	if releaseErr != nil && !slices.Contains(serviceReaps.unconfirmed[key], cmd) {
		serviceReaps.unconfirmed[key] = append(serviceReaps.unconfirmed[key], cmd)
	}
}

// settleServiceReaps retries the release of every generation pending under
// key. Each one a release confirms is dropped; the rest stay pending, and
// their failures are returned, as is any release still running there.
func settleServiceReaps(key string) error {
	serviceReaps.Lock()
	inFlight := serviceReaps.releasing[key]
	pending := slices.Clone(serviceReaps.unconfirmed[key])
	serviceReaps.Unlock()

	var errs []error
	if inFlight > 0 {
		errs = append(errs, fmt.Errorf("%d release(s) still running", inFlight))
	}
	for _, cmd := range pending {
		if err := releaseProxyProcessTree(cmd); err != nil {
			errs = append(errs, err)
			continue
		}
		serviceReaps.Lock()
		remaining := slices.DeleteFunc(serviceReaps.unconfirmed[key], func(held *exec.Cmd) bool { return held == cmd })
		if len(remaining) == 0 {
			delete(serviceReaps.unconfirmed, key)
		} else {
			serviceReaps.unconfirmed[key] = remaining
		}
		serviceReaps.Unlock()
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("previous service process tree not confirmed reaped: %w", errors.Join(errs...))
}

// serviceRootPending reports whether a generation under key is not confirmed
// reaped: a release is still running there, or one could not confirm its tree.
func serviceRootPending(key string) bool {
	serviceReaps.Lock()
	defer serviceReaps.Unlock()
	return serviceReaps.releasing[key] > 0 || len(serviceReaps.unconfirmed[key]) > 0
}

// awaitGenerationReaper waits, bounded by timeout, for the reaper of the
// generation cmd to finish its release.
func awaitGenerationReaper(cmd *exec.Cmd, doneCh <-chan struct{}, timeout time.Duration) error {
	if doneCh == nil {
		return nil
	}
	select {
	case <-doneCh:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("service process %d: its tree's release had not finished after %s, so its reap is not confirmed", cmd.Process.Pid, timeout)
	}
}
