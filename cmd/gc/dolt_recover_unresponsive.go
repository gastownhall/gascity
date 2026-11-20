package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// resolveManagedDoltUnresponsiveRestartAfter returns the city's
// [daemon].dolt_unresponsive_restart_after window. A city without city.toml
// uses the default; an existing config that cannot be loaded, or a malformed
// or negative setting, is an error so recovery never picks a stop policy the
// operator did not configure.
func resolveManagedDoltUnresponsiveRestartAfter(cityPath string) (time.Duration, error) {
	if strings.TrimSpace(cityPath) == "" {
		return config.DefaultDoltUnresponsiveRestartAfter, nil
	}
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config.DefaultDoltUnresponsiveRestartAfter, nil
		}
		return 0, fmt.Errorf("resolve dolt_unresponsive_restart_after: %w", err)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return 0, fmt.Errorf("resolve dolt_unresponsive_restart_after: load city config: %w", err)
	}
	window, err := cfg.Daemon.DoltUnresponsiveRestartAfterDuration()
	if err != nil {
		return 0, fmt.Errorf("resolve dolt_unresponsive_restart_after: %w", err)
	}
	return window, nil
}

// managedDoltEndpointState is what one bounded TCP dial learned about the
// managed endpoint.
type managedDoltEndpointState int

const (
	// managedDoltEndpointInconclusive means the dial neither connected nor
	// was refused (timeout, local resource failure): it says nothing reliable
	// about the server and never authorizes a stop.
	managedDoltEndpointInconclusive managedDoltEndpointState = iota
	// managedDoltEndpointListening means a TCP connection succeeded: a server
	// is alive on the endpoint even if its SQL layer is stalled.
	managedDoltEndpointListening
	// managedDoltEndpointRefused means the endpoint explicitly refused the
	// connection: nothing is listening.
	managedDoltEndpointRefused
)

func (s managedDoltEndpointState) String() string {
	switch s {
	case managedDoltEndpointListening:
		return "listening"
	case managedDoltEndpointRefused:
		return "refused"
	default:
		return "inconclusive"
	}
}

// managedDoltEndpointObservation is one live endpoint observation.
type managedDoltEndpointObservation struct {
	State managedDoltEndpointState
	Err   error
}

// managedDoltServerIncarnation identifies one running server process: its PID
// and kernel start time, so a recycled PID is a different incarnation.
type managedDoltServerIncarnation struct {
	PID       int
	StartTime string
}

func (i managedDoltServerIncarnation) known() bool {
	return i.PID > 0 && strings.TrimSpace(i.StartTime) != ""
}

const managedDoltRecoveryObserveTimeout = time.Second

// observeManagedDoltEndpoint makes one bounded TCP dial to the endpoint.
func observeManagedDoltEndpoint(host, port string) managedDoltEndpointObservation {
	conn, err := managedDoltDialFn("tcp", net.JoinHostPort(managedDoltConnectHost(host), strings.TrimSpace(port)), managedDoltRecoveryObserveTimeout)
	if err == nil {
		_ = conn.Close()
		return managedDoltEndpointObservation{State: managedDoltEndpointListening}
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return managedDoltEndpointObservation{State: managedDoltEndpointRefused, Err: err}
	}
	return managedDoltEndpointObservation{State: managedDoltEndpointInconclusive, Err: err}
}

// identifyManagedDoltListener returns the incarnation of the process that
// holds the endpoint's listening socket, verified from the live process
// table. A cached runtime-state PID is never taken as the listener.
func identifyManagedDoltListener(cityPath, port string) (managedDoltServerIncarnation, error) {
	info, err := inspectManagedDoltProcess(cityPath, port)
	if err != nil {
		return managedDoltServerIncarnation{}, fmt.Errorf("inspect managed dolt listener on port %s: %w", port, err)
	}
	if info.PortHolderPID <= 0 {
		return managedDoltServerIncarnation{}, fmt.Errorf("no live process identified as the listener on port %s", port)
	}
	startTime, err := pidutil.StartTime(info.PortHolderPID)
	if err != nil {
		return managedDoltServerIncarnation{}, fmt.Errorf("read start time of listener pid %d: %w", info.PortHolderPID, err)
	}
	return managedDoltServerIncarnation{PID: info.PortHolderPID, StartTime: startTime}, nil
}

// withGateDefaults fills the unresponsiveness-gate hooks a caller left unset
// with their live implementations.
func (ops managedDoltRecoveryOps) withGateDefaults() managedDoltRecoveryOps {
	if ops.observeEndpoint == nil {
		ops.observeEndpoint = observeManagedDoltEndpoint
	}
	if ops.identify == nil {
		ops.identify = identifyManagedDoltListener
	}
	if ops.restartAfter == nil {
		ops.restartAfter = resolveManagedDoltUnresponsiveRestartAfter
	}
	if ops.now == nil {
		ops.now = time.Now
	}
	return ops
}

// managedDoltRecoveryDecision is the kind of a recorded recovery decision.
type managedDoltRecoveryDecision string

const (
	managedDoltRecoveryDecisionDefer   managedDoltRecoveryDecision = "defer"
	managedDoltRecoveryDecisionStop    managedDoltRecoveryDecision = "stop"
	managedDoltRecoveryDecisionHealthy managedDoltRecoveryDecision = "healthy"
)

// Recovery decision reasons recorded in dolt.log.
const (
	managedDoltRecoveryReasonFirstObservation = "first_unresponsive_observation"
	managedDoltRecoveryReasonWithinWindow     = "within_unresponsive_window"
	managedDoltRecoveryReasonInconclusive     = "inconclusive_endpoint_observation"
	managedDoltRecoveryReasonUnknownIdentity  = "unknown_server_identity"
)

// managedDoltRecoveryRecordTag starts every recovery decision line appended to
// the managed dolt log, so readers can tell them apart from Dolt's own output.
const managedDoltRecoveryRecordTag = "gc-dolt-recovery/v1"

// managedDoltRecoveryRecord is one recovery decision appended to dolt.log. It
// records a past decision, never current liveness: live endpoint and process
// observations stay authoritative about what is running.
type managedDoltRecoveryRecord struct {
	At                time.Time
	Decision          managedDoltRecoveryDecision
	Reason            string
	Scope             string
	Endpoint          string
	PID               int
	StartTime         string
	UnresponsiveSince time.Time
	Window            time.Duration
	Inconclusive      bool
	ProbeError        string
	Caller            string
}

func (r managedDoltRecoveryRecord) elapsed() time.Duration {
	if r.UnresponsiveSince.IsZero() {
		return 0
	}
	return r.At.Sub(r.UnresponsiveSince)
}

func formatManagedDoltRecoveryTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// format renders the record as one complete log line.
func (r managedDoltRecoveryRecord) format() string {
	var b strings.Builder
	b.WriteString(managedDoltRecoveryRecordTag)
	fmt.Fprintf(&b, " at=%s", formatManagedDoltRecoveryTime(r.At))
	fmt.Fprintf(&b, " decision=%s", r.Decision)
	fmt.Fprintf(&b, " reason=%s", r.Reason)
	fmt.Fprintf(&b, " scope=%s", strconv.Quote(r.Scope))
	fmt.Fprintf(&b, " endpoint=%s", strconv.Quote(r.Endpoint))
	fmt.Fprintf(&b, " pid=%d", r.PID)
	fmt.Fprintf(&b, " start=%s", strconv.Quote(r.StartTime))
	fmt.Fprintf(&b, " since=%s", formatManagedDoltRecoveryTime(r.UnresponsiveSince))
	fmt.Fprintf(&b, " elapsed=%s", r.elapsed())
	fmt.Fprintf(&b, " window=%s", r.Window)
	fmt.Fprintf(&b, " inconclusive=%t", r.Inconclusive)
	fmt.Fprintf(&b, " probe_error=%s", strconv.Quote(r.ProbeError))
	fmt.Fprintf(&b, " caller=%s", strconv.Quote(r.Caller))
	b.WriteByte('\n')
	return b.String()
}

// managedDoltRecoveryCaller attributes a decision to the process making it.
func managedDoltRecoveryCaller() string {
	name := "gc"
	if len(os.Args) > 0 {
		name = filepath.Base(os.Args[0])
	}
	return fmt.Sprintf("%s pid=%d", name, os.Getpid())
}

// appendManagedDoltRecoveryRecord appends one complete record to the managed
// dolt log with a single O_APPEND write. Callers hold the lifecycle lock.
func appendManagedDoltRecoveryRecord(logFile string, rec managedDoltRecoveryRecord) error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return fmt.Errorf("record dolt recovery decision: create log dir: %w", err)
	}
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("record dolt recovery decision: open %s: %w", logFile, err)
	}
	line := rec.format()
	n, writeErr := f.Write([]byte(line))
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("record dolt recovery decision: write %s: %w", logFile, writeErr)
	}
	if n != len(line) {
		return fmt.Errorf("record dolt recovery decision: short write to %s (%d of %d bytes)", logFile, n, len(line))
	}
	if closeErr != nil {
		return fmt.Errorf("record dolt recovery decision: close %s: %w", logFile, closeErr)
	}
	return nil
}

// errManagedDoltRecoveryDeferred marks a recovery that left a live or
// uncertain server running instead of restarting it.
var errManagedDoltRecoveryDeferred = errors.New("managed dolt recovery deferred")

// managedDoltRecoveryGate decides whether one failed SQL observation may take
// the managed stop/start path. It runs under the lifecycle lock.
type managedDoltRecoveryGate struct {
	cityPath string
	host     string
	port     string
	logFile  string
	ops      managedDoltRecoveryOps
}

func (g managedDoltRecoveryGate) endpoint() string {
	return net.JoinHostPort(managedDoltConnectHost(g.host), strings.TrimSpace(g.port))
}

func (g managedDoltRecoveryGate) record(rec managedDoltRecoveryRecord) error {
	rec.Scope = normalizePathForCompare(g.cityPath)
	rec.Endpoint = g.endpoint()
	rec.Caller = managedDoltRecoveryCaller()
	return appendManagedDoltRecoveryRecord(g.logFile, rec)
}

// deferRecovery records a defer decision and returns the deferral error.
func (g managedDoltRecoveryGate) deferRecovery(rec managedDoltRecoveryRecord, detail string) error {
	rec.Decision = managedDoltRecoveryDecisionDefer
	if err := g.record(rec); err != nil {
		return fmt.Errorf("%w: %s; %w", errManagedDoltRecoveryDeferred, detail, err)
	}
	return fmt.Errorf("%w: %s (decision recorded in %s)", errManagedDoltRecoveryDeferred, detail, g.logFile)
}

// evaluate returns nil when the failure may enter the existing stop/start
// path, or a deferral error that leaves the server running.
func (g managedDoltRecoveryGate) evaluate(failure error) error {
	now := g.ops.now().UTC()
	probeError := ""
	if failure != nil {
		probeError = failure.Error()
	}
	base := managedDoltRecoveryRecord{At: now, ProbeError: probeError}

	observation := g.ops.observeEndpoint(g.host, g.port)
	switch observation.State {
	case managedDoltEndpointRefused:
		return nil
	case managedDoltEndpointListening:
	default:
		base.Reason = managedDoltRecoveryReasonInconclusive
		base.Inconclusive = true
		return g.deferRecovery(base, fmt.Sprintf("endpoint %s observation was inconclusive (%v) after %v; leaving the server running", g.endpoint(), observation.Err, failure))
	}

	window, err := g.ops.restartAfter(g.cityPath)
	if err != nil {
		return fmt.Errorf("%w: endpoint %s is listening after %s and the restart window is unavailable: %w", errManagedDoltRecoveryDeferred, g.endpoint(), failure.Error(), err)
	}
	base.Window = window

	ident, identErr := g.ops.identify(g.cityPath, g.port)
	if identErr != nil || !ident.known() {
		base.Reason = managedDoltRecoveryReasonUnknownIdentity
		base.Inconclusive = true
		base.PID = ident.PID
		base.StartTime = ident.StartTime
		return g.deferRecovery(base, fmt.Sprintf("endpoint %s is listening after %v but its server identity is unknown (%v); leaving it running", g.endpoint(), failure, identErr))
	}
	base.PID = ident.PID
	base.StartTime = ident.StartTime
	base.Reason = managedDoltRecoveryReasonFirstObservation
	base.UnresponsiveSince = now
	return g.deferRecovery(base, fmt.Sprintf("endpoint %s (pid %d) is listening after %v; first unresponsive observation, restart allowed after %s", g.endpoint(), ident.PID, failure, window))
}
