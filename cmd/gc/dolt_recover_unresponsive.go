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
	managedDoltRecoveryReasonWindowElapsed    = "unresponsive_window_elapsed"
	managedDoltRecoveryReasonWindowDisabled   = "unresponsive_window_disabled"
	managedDoltRecoveryReasonRecheckFailed    = "elapsed_window_recheck_failed"
	managedDoltRecoveryReasonHealthy          = "healthy_observation"
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

	history, err := readManagedDoltRecoveryHistory(g.logFile, now)
	if err != nil {
		return fmt.Errorf("%w: endpoint %s (pid %d) is listening after %s and recovery history is unreadable: %w", errManagedDoltRecoveryDeferred, g.endpoint(), ident.PID, failure.Error(), err)
	}
	historyNote := history.note()

	if window == 0 {
		return g.authorizeStop(base, ident, managedDoltRecoveryReasonWindowDisabled, failure)
	}
	since := history.openStreak(normalizePathForCompare(g.cityPath), g.endpoint(), ident)
	if since.IsZero() {
		base.Reason = managedDoltRecoveryReasonFirstObservation
		base.UnresponsiveSince = now
		return g.deferRecovery(base, fmt.Sprintf("endpoint %s (pid %d) is listening after %v; first unresponsive observation, restart allowed after %s%s", g.endpoint(), ident.PID, failure, window, historyNote))
	}
	base.UnresponsiveSince = since
	if elapsed := now.Sub(since); elapsed < window {
		base.Reason = managedDoltRecoveryReasonWithinWindow
		return g.deferRecovery(base, fmt.Sprintf("endpoint %s (pid %d) is listening after %v; unresponsive for %s of the %s window%s", g.endpoint(), ident.PID, failure, elapsed.Round(time.Second), window, historyNote))
	}
	return g.authorizeStop(base, ident, managedDoltRecoveryReasonWindowElapsed, failure)
}

// authorizeStop re-verifies that the same incarnation still holds the
// endpoint, then records the stop decision. Recording must succeed before the
// caller may stop anything.
func (g managedDoltRecoveryGate) authorizeStop(base managedDoltRecoveryRecord, ident managedDoltServerIncarnation, reason string, failure error) error {
	recheck := g.ops.observeEndpoint(g.host, g.port)
	current, identErr := g.ops.identify(g.cityPath, g.port)
	if recheck.State != managedDoltEndpointListening || identErr != nil || current != ident {
		base.Reason = managedDoltRecoveryReasonRecheckFailed
		base.Inconclusive = true
		return g.deferRecovery(base, fmt.Sprintf("endpoint %s changed before restart (observation %s, pid %d -> %d, identity error %v); leaving it running", g.endpoint(), recheck.State, ident.PID, current.PID, identErr))
	}
	base.Decision = managedDoltRecoveryDecisionStop
	base.Reason = reason
	if err := g.record(base); err != nil {
		return fmt.Errorf("%w: refusing an unrecorded restart of endpoint %s (pid %d) after %s: %w", errManagedDoltRecoveryDeferred, g.endpoint(), ident.PID, failure.Error(), err)
	}
	return nil
}

// closeStreakAfterHealthy records a healthy decision when the gate's endpoint
// has an open unresponsiveness streak, so a later failure starts a fresh
// window. The caller holds the lifecycle lock and has just measured the
// endpoint healthy; without an open streak nothing is written.
func (g managedDoltRecoveryGate) closeStreakAfterHealthy() error {
	now := g.ops.now().UTC()
	history, err := readManagedDoltRecoveryHistory(g.logFile, now)
	if err != nil {
		return fmt.Errorf("close dolt unresponsiveness streak for %s: %w", g.endpoint(), err)
	}
	if !history.streakOpen(normalizePathForCompare(g.cityPath), g.endpoint()) {
		return nil
	}
	rec := managedDoltRecoveryRecord{At: now, Decision: managedDoltRecoveryDecisionHealthy, Reason: managedDoltRecoveryReasonHealthy}
	if ident, identErr := g.ops.identify(g.cityPath, g.port); identErr == nil && ident.known() {
		rec.PID = ident.PID
		rec.StartTime = ident.StartTime
	}
	if err := g.record(rec); err != nil {
		return fmt.Errorf("close dolt unresponsiveness streak for %s: %w", g.endpoint(), err)
	}
	return nil
}

// closeManagedDoltStreakAfterProviderHealth closes an open unresponsiveness
// streak after a provider health check passed outside the lifecycle lock. It
// takes the lock without waiting (a lifecycle operation already holding it
// owns the decision), re-measures each open-streak endpoint on port as user
// under the lock, and records healthy only for an endpoint that is still healthy, so a
// delayed healthy result never clears a newer server's streak.
func closeManagedDoltStreakAfterProviderHealth(cityPath, port, user string, ops managedDoltRecoveryOps) error {
	port = strings.TrimSpace(port)
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	if strings.TrimSpace(cityPath) == "" || port == "" {
		return nil
	}
	ops = ops.withGateDefaults()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return fmt.Errorf("close dolt unresponsiveness streak: %w", err)
	}
	scope := normalizePathForCompare(cityPath)
	history, err := readManagedDoltRecoveryHistory(layout.LogFile, ops.now().UTC())
	if err != nil {
		return fmt.Errorf("close dolt unresponsiveness streak: %w", err)
	}
	endpoints := history.openStreakEndpoints(scope, port)
	if len(endpoints) == 0 {
		return nil
	}
	lockFile, _, err := openManagedDoltLifecycleLock(cityPath)
	if err != nil {
		return fmt.Errorf("close dolt unresponsiveness streak: %w", err)
	}
	locked, err := tryManagedDoltLifecycleLock(lockFile)
	if err != nil {
		_ = lockFile.Close()
		return fmt.Errorf("close dolt unresponsiveness streak: %w", err)
	}
	if !locked {
		_ = lockFile.Close()
		return nil
	}
	defer releaseManagedDoltLifecycleLock(lockFile)
	for _, endpoint := range endpoints {
		host, endpointPort, err := net.SplitHostPort(endpoint)
		if err != nil {
			continue
		}
		if ops.queryProbe(host, endpointPort, user) != nil {
			continue
		}
		health, healthErr := ops.healthCheck(host, endpointPort, user)
		if healthErr != nil || !health.QueryReady || health.ReadOnly != "false" {
			continue
		}
		gate := managedDoltRecoveryGate{cityPath: cityPath, host: host, port: endpointPort, logFile: layout.LogFile, ops: ops}
		if err := gate.closeStreakAfterHealthy(); err != nil {
			return err
		}
	}
	return nil
}

// managedDoltUserFromEnv returns the GC_DOLT_USER a provider process runs
// with, or empty when the environment does not set one.
func managedDoltUserFromEnv(environ []string) string {
	user := ""
	for _, kv := range environ {
		if value, ok := strings.CutPrefix(kv, "GC_DOLT_USER="); ok {
			user = strings.TrimSpace(value)
		}
	}
	return user
}

// managedDoltRecoveryHistoryTailBytes bounds how much of dolt.log recovery
// reads back. History older than the tail cannot justify a restart.
const managedDoltRecoveryHistoryTailBytes = 1 << 20

// managedDoltRecoveryHistory is the valid recovery records in the readable
// tail of dolt.log, oldest first.
type managedDoltRecoveryHistory struct {
	Records   []managedDoltRecoveryRecord
	Malformed int
}

func (h managedDoltRecoveryHistory) note() string {
	if h.Malformed == 0 {
		return ""
	}
	return fmt.Sprintf("; ignored %d malformed recovery record(s) in dolt.log", h.Malformed)
}

// openStreak returns the start of the unresponsiveness streak for ident on
// endpoint, or zero when none is open. Healthy and stop decisions close a
// streak, a positive observation of another incarnation replaces it, and
// inconclusive deferrals neither open nor advance it.
func (h managedDoltRecoveryHistory) openStreak(scope, endpoint string, ident managedDoltServerIncarnation) time.Time {
	var since time.Time
	for _, rec := range h.Records {
		if rec.Scope != scope || rec.Endpoint != endpoint {
			continue
		}
		switch rec.Decision {
		case managedDoltRecoveryDecisionHealthy, managedDoltRecoveryDecisionStop:
			since = time.Time{}
		case managedDoltRecoveryDecisionDefer:
			if rec.Inconclusive || rec.UnresponsiveSince.IsZero() {
				continue
			}
			if rec.PID != ident.PID || rec.StartTime != ident.StartTime {
				since = time.Time{}
				continue
			}
			if since.IsZero() {
				since = rec.UnresponsiveSince
			}
		}
	}
	return since
}

// streakOpen reports whether endpoint has an unresponsiveness streak for any
// incarnation that no healthy or stop decision has closed.
func (h managedDoltRecoveryHistory) streakOpen(scope, endpoint string) bool {
	open := false
	for _, rec := range h.Records {
		if rec.Scope != scope || rec.Endpoint != endpoint {
			continue
		}
		switch rec.Decision {
		case managedDoltRecoveryDecisionHealthy, managedDoltRecoveryDecisionStop:
			open = false
		case managedDoltRecoveryDecisionDefer:
			if !rec.Inconclusive && !rec.UnresponsiveSince.IsZero() {
				open = true
			}
		}
	}
	return open
}

// openStreakEndpoints returns the endpoints on port with an open streak.
func (h managedDoltRecoveryHistory) openStreakEndpoints(scope, port string) []string {
	var endpoints []string
	seen := map[string]bool{}
	for _, rec := range h.Records {
		if rec.Scope != scope || seen[rec.Endpoint] {
			continue
		}
		if _, recPort, err := net.SplitHostPort(rec.Endpoint); err != nil || recPort != port {
			continue
		}
		seen[rec.Endpoint] = true
		if h.streakOpen(scope, rec.Endpoint) {
			endpoints = append(endpoints, rec.Endpoint)
		}
	}
	return endpoints
}

// readManagedDoltRecoveryHistory reads the recovery records in the bounded
// tail of logFile. A missing log is an empty history. Records that do not
// parse, lie in the future, or claim an impossible streak start are counted
// as malformed and never used.
func readManagedDoltRecoveryHistory(logFile string, now time.Time) (managedDoltRecoveryHistory, error) {
	f, err := os.Open(logFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return managedDoltRecoveryHistory{}, nil
		}
		return managedDoltRecoveryHistory{}, fmt.Errorf("read dolt recovery history: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil {
		return managedDoltRecoveryHistory{}, fmt.Errorf("read dolt recovery history: stat %s: %w", logFile, err)
	}
	offset := info.Size() - managedDoltRecoveryHistoryTailBytes
	if offset < 0 {
		offset = 0
	}
	data := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		return managedDoltRecoveryHistory{}, fmt.Errorf("read dolt recovery history: %s: %w", logFile, err)
	}
	lines := strings.Split(string(data), "\n")
	if offset > 0 && len(lines) > 0 {
		// The first line of a tail may be cut mid-record.
		lines = lines[1:]
	}
	if len(lines) > 0 {
		// Only newline-terminated lines are complete records.
		lines = lines[:len(lines)-1]
	}
	history := managedDoltRecoveryHistory{}
	for _, line := range lines {
		if !strings.HasPrefix(line, managedDoltRecoveryRecordTag+" ") {
			continue
		}
		rec, err := parseManagedDoltRecoveryRecord(line)
		if err != nil || rec.At.After(now) || (!rec.UnresponsiveSince.IsZero() && rec.UnresponsiveSince.After(rec.At)) {
			history.Malformed++
			continue
		}
		history.Records = append(history.Records, rec)
	}
	return history, nil
}

// parseManagedDoltRecoveryRecord parses one line written by
// managedDoltRecoveryRecord.format. Every field must be present.
func parseManagedDoltRecoveryRecord(line string) (managedDoltRecoveryRecord, error) {
	rest, ok := strings.CutPrefix(line, managedDoltRecoveryRecordTag+" ")
	if !ok {
		return managedDoltRecoveryRecord{}, fmt.Errorf("not a recovery record")
	}
	fields := map[string]string{}
	for rest != "" {
		key, value, found := strings.Cut(rest, "=")
		if !found || key == "" || strings.Contains(key, " ") {
			return managedDoltRecoveryRecord{}, fmt.Errorf("malformed field near %q", rest)
		}
		if strings.HasPrefix(value, "\"") {
			quoted, err := strconv.QuotedPrefix(value)
			if err != nil {
				return managedDoltRecoveryRecord{}, fmt.Errorf("field %s: %w", key, err)
			}
			unquoted, err := strconv.Unquote(quoted)
			if err != nil {
				return managedDoltRecoveryRecord{}, fmt.Errorf("field %s: %w", key, err)
			}
			fields[key] = unquoted
			value = value[len(quoted):]
		} else {
			end := strings.IndexByte(value, ' ')
			if end < 0 {
				end = len(value)
			}
			fields[key] = value[:end]
			value = value[end:]
		}
		if value != "" && !strings.HasPrefix(value, " ") {
			return managedDoltRecoveryRecord{}, fmt.Errorf("field %s: trailing data", key)
		}
		rest = strings.TrimPrefix(value, " ")
	}
	for _, key := range []string{"at", "decision", "reason", "scope", "endpoint", "pid", "start", "since", "elapsed", "window", "inconclusive", "probe_error", "caller"} {
		if _, ok := fields[key]; !ok {
			return managedDoltRecoveryRecord{}, fmt.Errorf("missing field %s", key)
		}
	}
	rec := managedDoltRecoveryRecord{
		Decision:   managedDoltRecoveryDecision(fields["decision"]),
		Reason:     fields["reason"],
		Scope:      fields["scope"],
		Endpoint:   fields["endpoint"],
		StartTime:  fields["start"],
		ProbeError: fields["probe_error"],
		Caller:     fields["caller"],
	}
	switch rec.Decision {
	case managedDoltRecoveryDecisionDefer, managedDoltRecoveryDecisionStop, managedDoltRecoveryDecisionHealthy:
	default:
		return managedDoltRecoveryRecord{}, fmt.Errorf("unknown decision %q", rec.Decision)
	}
	var err error
	if rec.At, err = time.Parse(time.RFC3339Nano, fields["at"]); err != nil {
		return managedDoltRecoveryRecord{}, fmt.Errorf("field at: %w", err)
	}
	if fields["since"] != "-" {
		if rec.UnresponsiveSince, err = time.Parse(time.RFC3339Nano, fields["since"]); err != nil {
			return managedDoltRecoveryRecord{}, fmt.Errorf("field since: %w", err)
		}
	}
	if rec.PID, err = strconv.Atoi(fields["pid"]); err != nil || rec.PID < 0 {
		return managedDoltRecoveryRecord{}, fmt.Errorf("field pid: %q", fields["pid"])
	}
	if rec.Window, err = time.ParseDuration(fields["window"]); err != nil {
		return managedDoltRecoveryRecord{}, fmt.Errorf("field window: %w", err)
	}
	if _, err = time.ParseDuration(fields["elapsed"]); err != nil {
		return managedDoltRecoveryRecord{}, fmt.Errorf("field elapsed: %w", err)
	}
	if rec.Inconclusive, err = strconv.ParseBool(fields["inconclusive"]); err != nil {
		return managedDoltRecoveryRecord{}, fmt.Errorf("field inconclusive: %w", err)
	}
	return rec, nil
}
