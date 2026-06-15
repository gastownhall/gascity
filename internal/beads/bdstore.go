package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/telemetry"
)

const (
	bdParentProjectionPollInterval = 50 * time.Millisecond
	bdTxProjectionTimeout          = 5 * time.Second
)

// CommandRunner executes a command in the given directory and returns stdout bytes.
// The dir argument sets the working directory; name and args specify the command.
type CommandRunner func(dir, name string, args ...string) ([]byte, error)

var (
	bdCommandTimeout = 120 * time.Second
	// bdReadCommandTimeout bounds bd read-only subcommands (count, list,
	// ready, show, sql, stats, version). Default matches bdCommandTimeout to preserve
	// pre-bounded behavior; lowered in follow-up work after slow read
	// paths are identified.
	bdReadCommandTimeout = 120 * time.Second
	// bdGraphApplyCommandTimeout bounds atomic graph creation below callers'
	// outer command budgets so transient Dolt stalls can retry or fall back.
	bdGraphApplyCommandTimeout = 45 * time.Second
	// bdQueryCommandTimeout bounds the `bd query` subcommand, which reads the
	// ephemeral (wisp) tier. gc reload and gc doctor run a sequence of these
	// ephemeral/order-run reads; under the 120s general timeout an
	// intermittently slow child blocked those commands for minutes (#3191).
	// A bound well below that lets the runner kill the slow child quickly so
	// the tier-merge degrades to the durable tier and the lookups continue
	// instead of blocking. Normal ephemeral reads return in ~2s.
	bdQueryCommandTimeout = 30 * time.Second
	// bdSlowTelemetryThreshold is fixed in production via telemetry.BDSlowThreshold:
	// high enough to avoid normal bd list calls, but below the wrapper timeout.
	bdSlowTelemetryThreshold = telemetry.BDSlowThreshold
)

// ExecCommandRunner returns a CommandRunner that uses os/exec to run commands.
// Captures stdout for parsing and stderr for error diagnostics.
// When the command is "bd", records telemetry (duration, status, output).
func ExecCommandRunner() CommandRunner {
	return ExecCommandRunnerWithEnv(nil)
}

// ExecCommandRunnerWithEnv returns a CommandRunner that uses os/exec and
// applies the provided environment overrides. Explicit keys replace any
// inherited values from the parent process.
func ExecCommandRunnerWithEnv(env map[string]string) CommandRunner {
	return execCommandRunnerWithEnv(context.Background(), env)
}

// ExecCommandRunnerWithEnvContext is like ExecCommandRunnerWithEnv but binds
// every command it runs to ctx, so each command exits at the sooner of ctx's
// deadline and the per-command bd timeout. Callers with a short best-effort
// budget (for example the claim-time gc.current_run_id decoration) use this so a
// slow or stuck bd child cannot outlast that budget.
func ExecCommandRunnerWithEnvContext(ctx context.Context, env map[string]string) CommandRunner {
	return execCommandRunnerWithEnv(ctx, env)
}

func execCommandRunnerWithEnv(parent context.Context, env map[string]string) CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		start := time.Now()
		trace := newBDExecTrace(start, dir, name, args)
		trace("start", nil)

		timeout := bdCommandTimeoutFor(name, args)
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		if name == "bd" {
			bdArgs := append([]string(nil), args...)
			agentID := bdTelemetryAgentID(env)
			slowTimer := time.AfterFunc(bdSlowTelemetryThreshold, func() {
				telemetry.RecordBDSlow(ctx, bdArgs, dir, agentID)
			})
			defer slowTimer.Stop()
		}

		cmd := exec.CommandContext(ctx, name, args...)
		cmd.WaitDelay = 2 * time.Second
		prepareCommandForTimeout(cmd)
		cmd.Dir = dir
		cmd.Cancel = func() error {
			return killCommandTree(cmd)
		}
		cmd.Env = execEnvFor(name, processEnvSnapshotExcludingNativeDoltOpen(), env)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()

		recordBDExecTelemetry(name, dir, args, start, out, stderr.String(), err)

		status, traceErr, resultErr := classifyBDExecResult(
			parent, ctx, name, timeout, start, out, stderr.String(), err)
		trace(status, traceErr)
		return out, resultErr
	}
}

// newBDExecTrace returns the legacy line-format trace callback for one command
// invocation. It is a no-op when GC_BD_TRACE is unset, or when GC_BD_TRACE_JSON
// has claimed tracing (the structured JSONL trace in bdtrace.go) so the two
// formats never interleave incompatible records when an operator points both
// env vars at the same file. The returned callback appends one
// "status=… dur=… cmd=… err=…" line per call.
func newBDExecTrace(start time.Time, dir, name string, args []string) func(status string, err error) {
	if strings.TrimSpace(os.Getenv("GC_BD_TRACE_JSON")) != "" {
		return func(string, error) {}
	}
	path := strings.TrimSpace(os.Getenv("GC_BD_TRACE"))
	if path == "" {
		return func(string, error) {}
	}
	return func(status string, err error) {
		f, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if openErr != nil {
			return
		}
		defer f.Close() //nolint:errcheck // best-effort trace log
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		fmt.Fprintf(f, "%s status=%s dur=%s dir=%s cmd=%s args=%q err=%q\n", //nolint:errcheck // best-effort trace log
			time.Now().UTC().Format(time.RFC3339Nano), status, time.Since(start), dir, name, args, msg)
	}
}

// recordBDExecTelemetry emits the structured JSONL trace (bdtrace.go) and the
// telemetry RecordBDCall for a completed "bd" invocation; it is a no-op for any
// other command. The trace exit code is the child's exit status, or -1 when the
// failure was not an *exec.ExitError (for example a context kill or a spawn
// failure). It is independent of the legacy line-format trace (gated by
// GC_BD_TRACE_JSON, not GC_BD_TRACE).
func recordBDExecTelemetry(name, dir string, args []string, start time.Time, out []byte, stderr string, err error) {
	if name != "bd" {
		return
	}
	traceExit := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			traceExit = exitErr.ExitCode()
		} else {
			traceExit = -1
		}
	}
	TraceBDCall("go:bdstore.runner", dir, args, start, traceExit, err)
	telemetry.RecordBDCall(context.Background(),
		args, float64(time.Since(start).Milliseconds()),
		err, out, stderr)
}

// classifyBDExecResult maps a finished invocation to its legacy trace status,
// the error to trace, and the error to return to the caller. The trace error
// and the returned error differ only where the returned error is enriched with
// stderr/stdout detail that the historical status-level trace line omits, so
// the line-format trace stays compatible with prior releases. bd writes
// structured errors to stdout (JSON envelope) under --json while stderr is
// often empty, so the generic-error path surfaces whichever stream has content
// rather than a bare "exit status 1".
func classifyBDExecResult(parent, ctx context.Context, name string, timeout time.Duration, start time.Time, out []byte, stderr string, runErr error) (status string, traceErr, resultErr error) {
	if runErr == nil && name == "bd" && bdOutputIndicatesSilentFallback(stderr) {
		fallbackErr := fmt.Errorf("%w: %s", ErrBDSilentFallback, strings.TrimSpace(stderr))
		return "error", fallbackErr, fallbackErr
	}
	if ctx.Err() == context.DeadlineExceeded {
		timeoutErr := bdExecTimeoutError(parent, timeout, start)
		if stderr != "" {
			return "timeout", timeoutErr, fmt.Errorf("%w: %s", timeoutErr, stderr)
		}
		return "timeout", timeoutErr, timeoutErr
	}
	if runErr != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" && name == "bd" {
			detail = bdStdoutErrorDetail(out)
		}
		if detail != "" {
			return "error", runErr, fmt.Errorf("%w: %s", runErr, detail)
		}
	}
	return "done", runErr, runErr
}

// bdExecTimeoutError formats the deadline error after the per-command context
// expired, attributed to whichever budget actually won the race. ctx's
// effective deadline is min(parent deadline, start+timeout): when the caller's
// parent deadline is the binding one (for example the short claim-time
// gc.current_run_id write budget), it reports that effective budget so the
// failure is not misreported as the much larger per-command bd timeout; when
// the per-command timer wins, it reports that timeout unchanged.
func bdExecTimeoutError(parent context.Context, timeout time.Duration, start time.Time) error {
	if deadline, ok := parent.Deadline(); ok && deadline.Before(start.Add(timeout)) {
		budget := deadline.Sub(start)
		if budget < 0 {
			budget = 0
		}
		return fmt.Errorf("timed out after %s (caller deadline)", budget.Round(time.Millisecond))
	}
	return fmt.Errorf("timed out after %s", timeout)
}

func bdTelemetryAgentID(env map[string]string) string {
	for _, key := range []string{"GC_ALIAS", "GC_AGENT"} {
		if env != nil {
			if value := strings.TrimSpace(env[key]); value != "" {
				return value
			}
		}
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func bdCommandTimeoutFor(name string, args []string) time.Duration {
	if name != "bd" || len(args) == 0 {
		return bdCommandTimeout
	}
	if len(args) >= 2 && args[0] == "create" && args[1] == "--graph" {
		return bdGraphApplyCommandTimeout
	}
	switch args[0] {
	case "count", "list", "ready", "show", "sql", "stats", "version":
		return bdReadCommandTimeout
	case "query":
		return bdQueryCommandTimeout
	default:
		return bdCommandTimeout
	}
}

// bdStdoutErrorDetail extracts a human-readable error description from
// bd's JSON error envelope on stdout. bd writes structured errors as
// {"error": "...", "schema_version": N} on stdout when invoked with
// --json, while stderr is often empty. Returns "" when the output does
// not look like a bd error envelope so callers can fall through.
func bdStdoutErrorDetail(out []byte) string {
	trimmed := bytes.TrimSpace(extractJSON(out))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ""
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return ""
	}
	return strings.TrimSpace(env.Error)
}

const (
	bdSilentFallbackMarkerImport  = "auto-importing"
	bdSilentFallbackMarkerEmptyDB = "into empty database"
)

func bdOutputIndicatesSilentFallback(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, bdSilentFallbackMarkerImport) &&
		strings.Contains(lower, bdSilentFallbackMarkerEmptyDB)
}

// PurgeRunnerFunc executes a bd purge command with custom dir and env.
// Unlike CommandRunner, this supports environment variable manipulation
// needed by bd purge (BEADS_DIR override).
type PurgeRunnerFunc func(dir string, env []string, args ...string) ([]byte, error)

// PurgeResult holds the outcome of a bd purge operation.
type PurgeResult struct {
	Purged int
}

// BdStore implements Store by shelling out to the bd CLI (beads v0.55.1+).
// It delegates all persistence to bd's embedded Dolt database.
type BdStore struct {
	dir         string          // city root directory (where .beads/ lives)
	runner      CommandRunner   // injectable for testing
	purgeRunner PurgeRunnerFunc // injectable for testing; nil uses exec default
	idPrefix    string          // bead ID prefix owned by this store, without trailing "-"

	listSkipLabelsEnabled bool // whether bd list may receive --skip-labels

	readyProjectionMu      sync.Mutex
	readyProjectionChecked bool
	readyProjectionEnabled bool

	// Conditional-write (ConditionalWriter) capability state, populated lazily on
	// the first conditional write (bdstore_conditional.go). condWriteProbed/
	// condWriteCapable memoize the four-verb --if-revision probe; condWriteLatched
	// records a runtime unsupported response and is authoritative over the probe.
	condWriteMu      sync.Mutex
	condWriteProbed  bool
	condWriteCapable bool
	condWriteLatched bool
	// condWriteProbeErr memoizes a probe SUBPROCESS failure (bd missing or
	// broken) so incapable-because-broken stays distinguishable from
	// incapable-because-old on every later capability answer.
	condWriteProbeErr error

	// condWritesStamp carries the factory-stamped beads.conditional_writes
	// mode plus the once-per-store degrade latch, under its own mutex
	// (disjoint from condWriteMu's capability state; no nesting).
	condWritesStamp
}

const (
	bdTransientWriteAttempts = 3
	bdTransientReadAttempts  = 3
)

var _ ConditionalAssignmentReleaser = (*BdStore)(nil)

// BdStoreOption configures optional bd CLI behavior for a BdStore.
type BdStoreOption func(*BdStore)

// WithBdStoreListSkipLabels controls whether List may pass --skip-labels to bd.
// Keep disabled unless the caller has opted into bd 1.0.5-compatible CLI
// semantics; bd 1.0.4 rejects the flag.
func WithBdStoreListSkipLabels(enabled bool) BdStoreOption {
	return func(s *BdStore) {
		s.listSkipLabelsEnabled = enabled
	}
}

// NewBdStore creates a BdStore rooted at dir using the given runner.
func NewBdStore(dir string, runner CommandRunner, opts ...BdStoreOption) *BdStore {
	return NewBdStoreWithPrefix(dir, runner, "", opts...)
}

// NewBdStoreWithPrefix creates a BdStore with an explicit owned bead ID prefix.
func NewBdStoreWithPrefix(dir string, runner CommandRunner, idPrefix string, opts ...BdStoreOption) *BdStore {
	s := &BdStore{dir: dir, runner: runner, idPrefix: normalizeIDPrefix(idPrefix)}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// IDPrefix returns the bead ID prefix owned by this store, without trailing "-".
func (s *BdStore) IDPrefix() string {
	if s == nil {
		return ""
	}
	return s.idPrefix
}

// Dir returns the root directory this store was constructed with (where
// .beads/ lives). Callers that need to build an equivalent throwaway store
// bound to a different context (e.g. a per-request scoped clone) use this
// to target the same backend without a second, parallel way to track it.
func (s *BdStore) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// ListSkipLabelsEnabled reports whether this store may ask bd list to skip
// label hydration.
func (s *BdStore) ListSkipLabelsEnabled() bool {
	return s != nil && s.listSkipLabelsEnabled
}

func (s *BdStore) listIncludesCompleteDependencies() bool {
	return false
}

// Init initializes a beads database via bd init --server. This is an admin
// operation on BdStore directly, not part of the Store interface (MemStore/
// FileStore don't need it). If host is non-empty, --server-host (and
// optionally --server-port) are added to connect to a remote dolt server.
func (s *BdStore) Init(prefix, host, port string) error {
	args := []string{"init", "--server", "-p", prefix, "--skip-hooks"}
	if host != "" {
		args = append(args, "--server-host", host)
	}
	if port != "" {
		args = append(args, "--server-port", port)
	}
	_, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return fmt.Errorf("bd init: %w", err)
	}
	return nil
}

// ConfigSet sets a bd config key/value pair via bd config set.
func (s *BdStore) ConfigSet(key, value string) error {
	_, err := s.runner(s.dir, "bd", "config", "set", key, value)
	if err != nil {
		return fmt.Errorf("bd config set: %w", err)
	}
	return nil
}

// SetPurgeRunner overrides the default exec-based purge implementation.
// Used in tests to inject a fake runner.
func (s *BdStore) SetPurgeRunner(fn PurgeRunnerFunc) {
	s.purgeRunner = fn
}

// Purge runs "bd purge" to remove closed ephemeral beads from the given
// beads directory. Uses a 60-second timeout as a safety circuit breaker.
// The beadsDir is the .beads/ directory path; bd runs from its parent.
func (s *BdStore) Purge(beadsDir string, dryRun bool) (PurgeResult, error) {
	args := []string{"purge", "--json"}
	if dryRun {
		args = append(args, "--dry-run")
	}

	dir := filepath.Dir(beadsDir)
	env := envWithout(processEnvSnapshotExcludingNativeDoltOpen(), "BEADS_DIR")
	env = append(env, "BEADS_DIR="+beadsDir)

	var out []byte
	var err error
	if s.purgeRunner != nil {
		out, err = s.purgeRunner(dir, env, args...)
	} else {
		out, err = execPurge(dir, env, args)
	}
	if err != nil {
		return PurgeResult{}, fmt.Errorf("bd purge: %w", err)
	}

	// Parse JSON output to get purged count.
	jsonBytes := extractJSON(out)
	var result struct {
		PurgedCount *int `json:"purged_count"`
	}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return PurgeResult{}, fmt.Errorf("bd purge: unexpected output format: %s", strings.TrimSpace(string(out)))
	}

	purged := 0
	if result.PurgedCount != nil {
		purged = *result.PurgedCount
	}
	return PurgeResult{Purged: purged}, nil
}

// execPurge runs bd purge via exec.CommandContext with a 60-second timeout.
func execPurge(dir string, env, args []string) ([]byte, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bd", args...)
	cmd.Dir = dir
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	traceExit := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			traceExit = exitErr.ExitCode()
		} else {
			traceExit = -1
		}
	}
	TraceBDCall("go:bdstore.execPurge", dir, args, start, traceExit, err)
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("timed out after 60s")
	}
	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("%w (%s)", err, errMsg)
	}
	return stdout.Bytes(), nil
}

// extractJSON finds the first JSON value (object or array) in raw output
// that may contain non-JSON preamble (warnings, debug lines).
func extractJSON(data []byte) []byte {
	objStart := bytes.IndexByte(data, '{')
	arrStart := bytes.IndexByte(data, '[')

	switch {
	case objStart >= 0 && arrStart >= 0:
		if arrStart < objStart {
			return data[arrStart:]
		}
		return data[objStart:]
	case objStart >= 0:
		return data[objStart:]
	case arrStart >= 0:
		return data[arrStart:]
	default:
		return data
	}
}

// truncateRawOutput returns a trimmed slice of bd CLI output suitable for
// embedding in error messages. Limits to maxBytes to keep error strings
// bounded, marking truncation explicitly so the reader knows there's more.
func truncateRawOutput(data []byte, maxBytes int) string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) <= maxBytes {
		return string(trimmed)
	}
	return string(trimmed[:maxBytes]) + "...(truncated)"
}

// bdAutoBackupOptOutEnvKey disables bd's PersistentPostRun auto-backup (the
// hardcoded "backup_export" Dolt remote synced into <root>/.beads/backup on
// nearly every bd invocation, with no retention). A stuck-looping
// backup_export sync was the root cause of the 2026-06-08 town-wide wedge
// (ga-0eq), and the unrotated archives reached 210GB on one dev store
// (ga-yfbs28). gc's projected envs already opt out (cmd/gc applyBdAutoBackupOptOut);
// injecting it here covers every other runner-spawned bd call — hook claim,
// store bridge, t3bridge, libstore, provider lifecycle — current and future.
const bdAutoBackupOptOutEnvKey = "BD_BACKUP_ENABLED"

// execEnvFor assembles the child environment for a runner exec. For bd
// commands the auto-backup opt-out is injected as a baseline, replacing any
// value inherited from the parent process (matching the unconditional
// projected-env opt-out policy); an explicit per-call override still wins
// because mergeEnv applies overrides last. Non-bd commands (e.g. direct dolt
// queries) pass through untouched.
func execEnvFor(name string, baseEnv []string, overrides map[string]string) []string {
	if name == "bd" {
		baseEnv = append(envWithout(baseEnv, bdAutoBackupOptOutEnvKey), bdAutoBackupOptOutEnvKey+"=false")
	}
	return mergeEnv(baseEnv, overrides)
}

// envWithout returns a copy of environ with all entries for the given key removed.
func envWithout(environ []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func mergeEnv(environ []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return append([]string(nil), environ...)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := append([]string(nil), environ...)
	for _, key := range keys {
		out = envWithout(out, key)
		out = append(out, key+"="+overrides[key])
	}
	return out
}

// StringMap is a map[string]string that tolerates non-string JSON values
// (booleans, numbers) by coercing them to their string representation.
// This prevents bd CLI's type-inference from breaking metadata deserialization
// (e.g., bd stores "true" as JSON boolean true, "42" as JSON number 42).
type StringMap map[string]string

// UnmarshalJSON implements json.Unmarshaler for StringMap.
func (m *StringMap) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	result := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			result[k] = s
			continue
		}
		// Coerce non-string values to their JSON text representation
		// (e.g., true → "true", 42 → "42").
		result[k] = strings.TrimSpace(string(v))
	}
	*m = result
	return nil
}

// bdIssue is the JSON shape returned by bd CLI commands. We decode only the
// fields Gas City cares about; all others are silently ignored.
type bdIssue struct {
	ID           string       `json:"id"`
	Title        string       `json:"title"`
	Status       string       `json:"status"`
	IssueType    string       `json:"issue_type"`
	Priority     *int         `json:"priority,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
	Assignee     string       `json:"assignee"`
	From         string       `json:"from"`
	ParentID     string       `json:"parent"`
	Ref          string       `json:"ref"`
	Needs        []string     `json:"needs"`
	Description  string       `json:"description"`
	Labels       []string     `json:"labels"`
	Metadata     StringMap    `json:"metadata,omitempty"`
	Dependencies []bdIssueDep `json:"dependencies,omitempty"`
	Ephemeral    bool         `json:"ephemeral,omitempty"`
	NoHistory    bool         `json:"no_history,omitempty"`
	DeferUntil   *time.Time   `json:"defer_until,omitempty"`
	IsBlocked    optionalBool `json:"is_blocked,omitempty"`
	// Revision carries bd's optimistic-concurrency token for ConditionalWriter.
	// Pre-#4682 bd omits it, so it decodes to 0; toBead stamps it onto the
	// otherwise json:"-" Bead.Revision field. The "revision" key is provisional:
	// bd #4682 (which adds the column and --if-revision) is unlanded, so the
	// exact wire key is unconfirmed. The integration conformance row against a
	// #4682-capable bd is the guard — an absent key is indistinguishable from
	// legacy bd here (both decode to 0), so a key-name mismatch would fail there,
	// not silently.
	Revision int64 `json:"revision,omitempty"`
}

type bdIssueDep struct {
	IssueID        string `json:"issue_id"`
	DependsOnID    string `json:"depends_on_id"`
	Type           string `json:"type"`
	ID             string `json:"id"`
	DependencyType string `json:"dependency_type"`
}

// PartialResultError indicates that a list-style bd command returned at least
// one usable entry but could not produce a complete result because some entries
// failed to parse or one underlying tier failed. The successful entries are
// still returned alongside this error; callers that can surface partial data
// may proceed with those rows, while callers that require a complete picture
// should treat this as a hard failure.
type PartialResultError struct {
	// Op identifies the bd subcommand that produced the partial result
	// (e.g. "bd list", "bd ready").
	Op string
	// Err wraps the joined per-entry parse errors from parseIssuesTolerant.
	Err error
}

// Error reports the operation and underlying parse failures.
func (e *PartialResultError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

// Unwrap returns the joined parse error so errors.Is / errors.As traversal
// continues into the underlying causes.
func (e *PartialResultError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsPartialResult reports whether err wraps a PartialResultError.
func IsPartialResult(err error) bool {
	var partial *PartialResultError
	return errors.As(err, &partial)
}

// parseIssuesTolerant unmarshals bd list output, skipping any entries that
// fail to parse (e.g. corrupt metadata with non-string values). bd 1.0.4 emits
// a top-level array; bd 1.0.5 may emit an object envelope with an issues array.
// This prevents a single bad bead from breaking all list operations.
func parseIssuesTolerant(data []byte) ([]bdIssue, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	raw, err := bdListIssueRows(data)
	if err != nil {
		// Include a snippet of the raw bd output so the failure surface is
		// diagnosable. Historical case (gascity #1726): bd returned the
		// literal string "None" and the unwrapped error was the opaque
		// "invalid character 'N' looking for beginning of value" with no
		// hint that the offending byte was a Python None text.
		return nil, fmt.Errorf("parsing JSON: raw=%q: %w", truncateRawOutput(data, 200), err)
	}
	result := make([]bdIssue, 0, len(raw))
	var parseErr error
	for _, r := range raw {
		var issue bdIssue
		if err := json.Unmarshal(r, &issue); err != nil {
			var peek struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(r, &peek)
			if peek.ID == "" {
				peek.ID = "<unknown>"
			}
			parseErr = errors.Join(parseErr, fmt.Errorf("%s: %w", peek.ID, err))
			continue
		}
		result = append(result, issue)
	}
	if parseErr != nil {
		skipped := len(raw) - len(result)
		beadNoun := "beads"
		if skipped == 1 {
			beadNoun = "bead"
		}
		return result, fmt.Errorf("skipped %d corrupt %s: %w", skipped, beadNoun, parseErr)
	}
	return result, nil
}

func bdListIssueRows(data []byte) ([]json.RawMessage, error) {
	var raw []json.RawMessage
	arrayErr := json.Unmarshal(data, &raw)
	if arrayErr == nil {
		return raw, nil
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, arrayErr
	}
	rawIssues, ok := envelope["issues"]
	if !ok {
		return nil, fmt.Errorf("JSON object missing issues field")
	}
	if err := json.Unmarshal(rawIssues, &raw); err != nil {
		return nil, fmt.Errorf("issues field: %w", err)
	}
	return raw, nil
}

// toBead converts a bdIssue to a Gas City Bead. CreatedAt is truncated to
// second precision because dolt stores timestamps at second granularity —
// bd create may return sub-second precision that bd show then truncates.
func (b *bdIssue) toBead() Bead {
	from := b.From
	if from == "" && b.Metadata != nil {
		from = b.Metadata["from"]
	}
	deps := b.normalizedDependencies()
	parentID := b.ParentID
	if parentID == "" {
		for _, dep := range deps {
			if dep.IssueID == b.ID && dep.Type == "parent-child" {
				parentID = dep.DependsOnID
				break
			}
		}
	}
	return Bead{
		ID:           b.ID,
		Title:        b.Title,
		Status:       mapBdStatus(b.Status),
		Type:         b.IssueType,
		Priority:     cloneIntPtr(b.Priority),
		CreatedAt:    b.CreatedAt.Truncate(time.Second),
		UpdatedAt:    b.UpdatedAt.Truncate(time.Second),
		Assignee:     b.Assignee,
		From:         from,
		ParentID:     parentID,
		Ref:          b.Ref,
		Needs:        b.Needs,
		Description:  b.Description,
		Labels:       b.Labels,
		Metadata:     b.Metadata,
		Dependencies: deps,
		Ephemeral:    b.Ephemeral,
		NoHistory:    b.NoHistory,
		DeferUntil:   cloneTimePtr(b.DeferUntil),
		IsBlocked:    b.IsBlocked.ptr(),
		Revision:     b.Revision,
	}
}

func (b *bdIssue) normalizedDependencies() []Dep {
	if len(b.Dependencies) == 0 {
		return nil
	}
	deps := make([]Dep, 0, len(b.Dependencies))
	for _, raw := range b.Dependencies {
		issueID := strings.TrimSpace(raw.IssueID)
		if issueID == "" && raw.ID != "" {
			issueID = b.ID
		}
		dependsOnID := strings.TrimSpace(raw.DependsOnID)
		if dependsOnID == "" {
			dependsOnID = strings.TrimSpace(raw.ID)
		}
		depType := strings.TrimSpace(raw.Type)
		if depType == "" {
			depType = strings.TrimSpace(raw.DependencyType)
		}
		if issueID == "" || dependsOnID == "" {
			continue
		}
		if depType == "" {
			depType = "blocks"
		}
		deps = append(deps, Dep{
			IssueID:     issueID,
			DependsOnID: dependsOnID,
			Type:        depType,
		})
	}
	return deps
}

// isBdNotFound returns true if the error from bd CLI indicates a "not found" condition.
// bd uses several phrasings: "no issue found", "issue not found", "not found" on
// stderr, and the plural "no issues found matching the provided IDs" in its
// stdout --json error envelope. The plural form matters because
// classifyBDExecResult falls back to the stdout envelope when stderr is empty,
// which bd's --json error path increasingly is (see classifyBDExecResult); the
// contract corpus pins this exact phrasing.
func isBdNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no issue found") ||
		strings.Contains(msg, "no issues found")
}

func isBdClaimConflictMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "already assigned") ||
		strings.Contains(msg, "already claimed") ||
		strings.Contains(msg, "claimed by") ||
		strings.Contains(msg, "claim conflict")
}

// mapBdStatus maps bd's statuses to Gas City's 3. bd uses: open,
// in_progress, blocked, review, testing, closed. Gas City uses:
// open, in_progress, closed.
func mapBdStatus(s string) string {
	switch s {
	case "closed":
		return "closed"
	case "in_progress":
		return "in_progress"
	default:
		return "open"
	}
}

type optionalBool struct {
	set   bool
	value bool
}

func (b *optionalBool) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*b = optionalBool{}
		return nil
	}
	var boolValue bool
	if err := json.Unmarshal(data, &boolValue); err == nil {
		b.set = true
		b.value = boolValue
		return nil
	}
	var intValue int
	if err := json.Unmarshal(data, &intValue); err == nil {
		b.set = true
		b.value = intValue != 0
		return nil
	}
	var stringValue string
	if err := json.Unmarshal(data, &stringValue); err == nil {
		switch strings.ToLower(strings.TrimSpace(stringValue)) {
		case "1", "t", "true", "y", "yes":
			b.set = true
			b.value = true
			return nil
		case "0", "f", "false", "n", "no":
			b.set = true
			b.value = false
			return nil
		}
	}
	return fmt.Errorf("invalid bool value %q", string(data))
}

func (b optionalBool) ptr() *bool {
	if !b.set {
		return nil
	}
	return cloneBoolPtr(&b.value)
}

// Create persists a new bead via bd create.
func (s *BdStore) Create(b Bead) (Bead, error) {
	return s.CreateWithStorage(b, StorageDefault)
}

// CreateWithStorage persists a new bead via bd create using a storage tier
// selected by policy middleware.
func (s *BdStore) CreateWithStorage(b Bead, storage StorageClass) (Bead, error) {
	effectiveEphemeral, effectiveNoHistory, err := effectiveStorageFlags(b, storage)
	if err != nil {
		return Bead{}, fmt.Errorf("bd create: %w", err)
	}
	if effectiveEphemeral && effectiveNoHistory {
		return Bead{}, fmt.Errorf("bd create: ephemeral and no-history storage are mutually exclusive")
	}
	typ := b.Type
	if typ == "" {
		typ = "task"
	}
	args := []string{"create", "--json", b.Title, "-t", typ}
	hasStableID := false
	if id := strings.TrimSpace(b.ID); id != "" {
		args = append(args, "--id", id)
		hasStableID = true
	}
	if b.Priority != nil {
		args = append(args, "--priority", strconv.Itoa(*b.Priority))
	}
	if b.Description != "" {
		args = append(args, "--description", b.Description)
	}
	if b.Assignee != "" {
		args = append(args, "--assignee", b.Assignee)
	}
	if len(b.Needs) > 0 {
		args = append(args, "--deps", strings.Join(b.Needs, ","))
	}
	if len(b.Labels) > 0 {
		args = append(args, "--labels", strings.Join(b.Labels, ","))
	}
	if b.ParentID != "" {
		args = append(args, "--parent", b.ParentID)
	}
	if effectiveEphemeral {
		args = append(args, "--ephemeral")
	}
	if effectiveNoHistory {
		args = append(args, "--no-history")
	}
	if b.DeferUntil != nil {
		args = append(args, "--defer", b.DeferUntil.Format(time.RFC3339))
	}
	metadata := maps.Clone(b.Metadata)
	if b.From != "" {
		if metadata == nil {
			metadata = make(map[string]string, 1)
		}
		if metadata["from"] == "" {
			metadata["from"] = b.From
		}
	}
	if len(metadata) > 0 {
		metaJSON, err := json.Marshal(metadata)
		if err != nil {
			return Bead{}, fmt.Errorf("bd create: marshaling metadata: %w", err)
		}
		args = append(args, "--metadata", string(metaJSON))
	}
	out, err := s.runBDTransientCreateOutput(hasStableID, args...)
	if err != nil {
		return Bead{}, fmt.Errorf("bd create: %w", err)
	}
	var issue bdIssue
	if err := json.Unmarshal(extractJSON(out), &issue); err != nil {
		return Bead{}, fmt.Errorf("bd create: parsing JSON: %w", err)
	}
	created := issue.toBead()
	if created.Assignee == "" {
		created.Assignee = b.Assignee
	}
	if created.From == "" {
		created.From = b.From
	}
	if created.Priority == nil && b.Priority != nil {
		created.Priority = cloneIntPtr(b.Priority)
	}
	if len(metadata) > 0 {
		if created.Metadata == nil {
			created.Metadata = maps.Clone(metadata)
		}
	}
	if effectiveEphemeral {
		created.Ephemeral = true
	}
	if effectiveNoHistory {
		created.NoHistory = true
	}
	return created, nil
}

func effectiveStorageFlags(b Bead, storage StorageClass) (ephemeral bool, noHistory bool, err error) {
	switch storage {
	case StorageDefault:
		return b.Ephemeral, b.NoHistory, nil
	case StorageHistory:
		return false, false, nil
	case StorageNoHistory:
		return false, true, nil
	case StorageEphemeral:
		return true, false, nil
	default:
		return false, false, fmt.Errorf("unknown storage class %q", storage)
	}
}

// Get retrieves a bead by ID via bd show.
func (s *BdStore) Get(id string) (Bead, error) {
	// Read via the transient-retry wrapper so a Get that races a managed-Dolt
	// restart (SIGKILL + port rebind) recovers instead of surfacing a one-shot
	// "invalid connection"/"i/o timeout" transport error. The runner performs a
	// single recover-and-retry per call; the wrapper's outer attempts give the
	// rebind enough total time to complete under CI load, matching every other
	// BdStore read/write path (ga-gellq1).
	out, err := s.runBDTransientRead("show", "--json", id)
	if err != nil {
		if !isBdNotFound(err) {
			return Bead{}, fmt.Errorf("getting bead %q: %w", id, err)
		}
		// bd show only queries the issues table; ephemeral beads live in the
		// wisps table and are invisible to it. Fall back to bd query with
		// ephemeral=true and id=<id> so Get succeeds for wisp-tier beads
		// (e.g. auto-handoff mail created by gc handoff --auto). Only IDs
		// that look like bead IDs are eligible: callers also pass through
		// non-bead names (e.g. slash-qualified session recipients), which
		// must not leak into a supplemental wisp query.
		if isWispQueryableID(id) {
			wisps, queryErr := s.getEphemeralByID(id)
			if queryErr == nil {
				for _, b := range wisps {
					if b.ID == id {
						return b, nil
					}
				}
			}
		}
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	var issues []bdIssue
	if err := json.Unmarshal(extractJSON(out), &issues); err != nil {
		return Bead{}, fmt.Errorf("bd show: parsing JSON: %w", err)
	}
	if len(issues) == 0 {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	bead := issues[0].toBead()
	// Guard against bd's fuzzy/substring ID resolution silently returning a
	// different bead than requested (gascity#gcy-g4o). bd may resolve a short
	// ID like "gcy-dv7" to an unrelated bead whose ID contains "dv7" as a
	// substring (e.g. "gcy-wisp-dv78"). Since gc always passes full bead IDs,
	// a mismatch means the requested bead does not exist in this store.
	if bead.ID != id {
		// bd resolved a DIFFERENT bead (substring collision): the requested ID
		// does not exist in this store but bd silently matched another one.
		// Return ErrIDCollision so mutation guards can distinguish this from a
		// plain absent bead. ErrIDCollision wraps ErrNotFound so existing
		// errors.Is(err, ErrNotFound) callers remain unaffected.
		return Bead{}, fmt.Errorf("getting bead %q (resolved to %q): %w", id, bead.ID, ErrIDCollision)
	}
	return bead, nil
}

// bdUpdateArgs builds the `bd update` argv for opts, fanning each set field to
// its flag. The result always begins with the three-element prefix
// {"update","--json",id}; a return of exactly that prefix means no fields were
// set (the empty-update no-op that bd itself rejects). It is shared by the
// unconditional Update and the fenced UpdateIfMatch so a new UpdateOpts field is
// wired into both paths from one place.
func bdUpdateArgs(id string, opts UpdateOpts) []string {
	args := []string{"update", "--json", id}
	if opts.Title != nil {
		args = append(args, "--title", *opts.Title)
	}
	if opts.Status != nil {
		args = append(args, "--status", *opts.Status)
	}
	if opts.Type != nil {
		args = append(args, "--type", *opts.Type)
	}
	if opts.Priority != nil {
		args = append(args, "--priority", strconv.Itoa(*opts.Priority))
	}
	if opts.Description != nil {
		args = append(args, "--description", *opts.Description)
	}
	if opts.ParentID != nil {
		args = append(args, "--parent", *opts.ParentID)
	}
	if opts.Assignee != nil {
		args = append(args, "--assignee", *opts.Assignee)
	}
	if len(opts.Metadata) > 0 {
		keys := make([]string, 0, len(opts.Metadata))
		for k := range opts.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--set-metadata", k+"="+opts.Metadata[k])
		}
	}
	for _, l := range opts.Labels {
		args = append(args, "--add-label", l)
	}
	for _, l := range opts.RemoveLabels {
		args = append(args, "--remove-label", l)
	}
	return args
}

// Update modifies fields of an existing bead via bd update.
func (s *BdStore) Update(id string, opts UpdateOpts) error {
	args := bdUpdateArgs(id, opts)
	// No fields to update — no-op (bd errors on empty update).
	if len(args) == 3 {
		return nil
	}
	// Internal store callers supply canonical full IDs; the exact-ID collision
	// guard lives at the CLI/API entry points (cmd_bd.go, huma_handlers_beads.go)
	// where user-typed short IDs originate (gcy-g4o).
	err := s.runBDTransientWrite(args...)
	if err != nil {
		if isBdNotFound(err) {
			return fmt.Errorf("updating bead %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("updating bead %q: %w", id, err)
	}
	return nil
}

// ReleaseIfCurrent clears an in-progress assignment only when the bead still
// has the expected assignee.
//
// SEAM (bd conditional-release verb): today this rides raw `bd sql`. The
// sqlite backend refuses raw DB access, so that rejection — and embedded dolt
// WITHOUT a configured dolt directory — surface ErrConditionalReleaseUnsupported
// (the latter via the releaseIfCurrentViaEmbeddedDoltSQL fallback). Embedded
// dolt WITH a configured directory instead services the CAS directly through
// that fallback, returning real rows-affected rather than reporting
// unsupported. When bd ships its native issueops CAS release verb, consume it
// HERE as the first attempt:
// probe by invoking the verb and fall back to this `bd sql` path when bd
// reports the command unknown (older pinned bd). Callers already treat
// ErrConditionalReleaseUnsupported as "take a conditional recheck fallback"
// (see cmd/gc releasePoolAssignmentIfCurrent), so no caller changes are needed.
func (s *BdStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	query := "UPDATE issues SET status = 'open', assignee = '', updated_at = CURRENT_TIMESTAMP" +
		" WHERE id = " + bdSQLStringLiteral(id) +
		" AND status = 'in_progress'" +
		" AND assignee = " + bdSQLStringLiteral(expectedAssignee)
	out, err := s.runBDTransientWriteOutput("sql", "--json", query)
	if err != nil {
		if isBdSQLUnsupportedInEmbeddedMode(err) {
			return s.releaseIfCurrentViaEmbeddedDoltSQL(id, expectedAssignee)
		}
		return false, fmt.Errorf("bd release-if-current: %w", err)
	}
	var result struct {
		RowsAffected int `json:"rows_affected"`
	}
	if err := json.Unmarshal(extractJSON(out), &result); err != nil {
		return false, fmt.Errorf("bd release-if-current: parsing SQL result: %w", err)
	}
	return result.RowsAffected > 0, nil
}

func (s *BdStore) releaseIfCurrentViaEmbeddedDoltSQL(id, expectedAssignee string) (bool, error) {
	doltDir, ok, err := s.embeddedDoltDir()
	if err != nil {
		return false, fmt.Errorf("bd release-if-current embedded fallback: %w", err)
	}
	if !ok {
		return false, fmt.Errorf("bd release-if-current embedded fallback: %w", ErrConditionalReleaseUnsupported)
	}
	query := "UPDATE issues SET status = 'open', assignee = '', updated_at = CURRENT_TIMESTAMP" +
		" WHERE id = " + bdSQLStringLiteral(id) +
		" AND status = 'in_progress'" +
		" AND assignee = " + bdSQLStringLiteral(expectedAssignee) +
		"; SELECT ROW_COUNT() AS rows_affected"
	out, err := s.runner(doltDir, "dolt", "sql", "-r", "json", "-q", query)
	if err != nil {
		return false, fmt.Errorf("bd release-if-current embedded fallback: dolt sql: %w", err)
	}
	rowsAffected, err := parseDoltRowsAffected(out)
	if err != nil {
		return false, fmt.Errorf("bd release-if-current embedded fallback: parsing SQL result: %w", err)
	}
	return rowsAffected > 0, nil
}

func (s *BdStore) embeddedDoltDir() (string, bool, error) {
	metaPath := filepath.Join(s.dir, ".beads", "metadata.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	var meta struct {
		Backend      string `json:"backend"`
		Database     string `json:"database"`
		DoltMode     string `json:"dolt_mode"`
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", false, fmt.Errorf("parsing metadata.json: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(meta.Backend), "dolt") &&
		!strings.EqualFold(strings.TrimSpace(meta.Database), "dolt") {
		return "", false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(meta.DoltMode), "embedded") {
		return "", false, nil
	}
	database := strings.TrimSpace(meta.DoltDatabase)
	if database == "" {
		return "", false, errors.New("metadata.json missing dolt_database")
	}
	if database != filepath.Base(database) {
		return "", false, fmt.Errorf("metadata.json dolt_database %q must be a database name, not a path", database)
	}
	return filepath.Join(s.dir, ".beads", "embeddeddolt", database), true, nil
}

func parseDoltRowsAffected(out []byte) (int, error) {
	data := bytes.TrimSpace(extractJSON(out))
	found := false
	rowsAffected := 0
	for len(data) > 0 {
		start := bytes.IndexAny(data, "{[")
		if start < 0 {
			break
		}
		data = data[start:]
		decoder := json.NewDecoder(bytes.NewReader(data))
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return 0, err
		}
		if got, ok, err := doltRowsAffectedFromJSON(raw); err != nil {
			return 0, err
		} else if ok {
			rowsAffected = got
			found = true
		}
		consumed := decoder.InputOffset()
		if consumed <= 0 || int(consumed) > len(data) {
			return 0, errors.New("could not advance through dolt JSON output")
		}
		data = data[consumed:]
	}
	if !found {
		return 0, errors.New("missing rows_affected row")
	}
	return rowsAffected, nil
}

func doltRowsAffectedFromJSON(raw json.RawMessage) (int, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0, false, nil
	}
	if trimmed[0] == '[' {
		var results []doltRowsAffectedResult
		if err := json.Unmarshal(trimmed, &results); err != nil {
			return 0, false, err
		}
		found := false
		rowsAffected := 0
		for _, result := range results {
			if got, ok := result.rowsAffected(); ok {
				rowsAffected = got
				found = true
			}
		}
		return rowsAffected, found, nil
	}
	var result doltRowsAffectedResult
	if err := json.Unmarshal(trimmed, &result); err != nil {
		return 0, false, err
	}
	rowsAffected, ok := result.rowsAffected()
	return rowsAffected, ok, nil
}

type doltRowsAffectedResult struct {
	Rows []struct {
		RowsAffected *int `json:"rows_affected"`
	} `json:"rows"`
}

func (r doltRowsAffectedResult) rowsAffected() (int, bool) {
	for _, row := range r.Rows {
		if row.RowsAffected != nil {
			return *row.RowsAffected, true
		}
	}
	return 0, false
}

func isBdSQLUnsupportedInEmbeddedMode(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "bd sql") &&
		strings.Contains(msg, "not yet supported") &&
		strings.Contains(msg, "embedded mode")
}

func bdSQLStringLiteral(value string) string {
	// bd sql runs against Dolt/MySQL string-literal semantics.
	value = strings.ReplaceAll(value, "\\", "\\\\")
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// Claim atomically claims an open bead through bd update --claim.
//
// It returns ok=false when bd reports that another actor won the claim race.
// The caller controls the claim actor through the store's CommandRunner
// environment, typically BEADS_ACTOR.
func (s *BdStore) Claim(id string) (Bead, bool, error) {
	out, err := s.runBDTransientWriteOutput("update", id, "--claim", "--json")
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if isBdClaimConflictMessage(msg) || isBdClaimConflictMessage(err.Error()) {
			return Bead{}, false, nil
		}
		if isBdNotFound(err) {
			return Bead{}, false, fmt.Errorf("claiming bead %q: %w", id, ErrNotFound)
		}
		if msg != "" {
			return Bead{}, false, fmt.Errorf("claiming bead %q: %w: %s", id, err, msg)
		}
		return Bead{}, false, fmt.Errorf("claiming bead %q: %w", id, err)
	}
	claimed, err := parseBDMutationBead("bd claim", out)
	if err != nil {
		return Bead{}, false, fmt.Errorf("claiming bead %q: %w", id, err)
	}
	return claimed, true, nil
}

func parseBDMutationBead(op string, out []byte) (Bead, error) {
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	if parseErr == nil && len(issues) > 0 {
		return issues[0].toBead(), nil
	}
	var issue bdIssue
	if err := json.Unmarshal(extractJSON(out), &issue); err == nil && strings.TrimSpace(issue.ID) != "" {
		return issue.toBead(), nil
	}
	if parseErr != nil {
		return Bead{}, fmt.Errorf("%s: parsing JSON: %w", op, parseErr)
	}
	return Bead{}, fmt.Errorf("%s returned no bead", op)
}

// UpdateAll modifies the same fields on multiple beads via one bd update
// invocation. It is intended for controller hot paths that need the semantics
// of bd update, not bd close, across a batch of known bead IDs.
func (s *BdStore) UpdateAll(ids []string, opts UpdateOpts) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := append([]string{"update", "--json"}, ids...)
	baseLen := len(args)
	if opts.Title != nil {
		args = append(args, "--title", *opts.Title)
	}
	if opts.Status != nil {
		args = append(args, "--status", *opts.Status)
	}
	if opts.Type != nil {
		args = append(args, "--type", *opts.Type)
	}
	if opts.Priority != nil {
		args = append(args, "--priority", strconv.Itoa(*opts.Priority))
	}
	if opts.Description != nil {
		args = append(args, "--description", *opts.Description)
	}
	if opts.ParentID != nil {
		args = append(args, "--parent", *opts.ParentID)
	}
	if opts.Assignee != nil {
		args = append(args, "--assignee", *opts.Assignee)
	}
	if len(opts.Metadata) > 0 {
		keys := make([]string, 0, len(opts.Metadata))
		for k := range opts.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--set-metadata", k+"="+opts.Metadata[k])
		}
	}
	for _, l := range opts.Labels {
		args = append(args, "--add-label", l)
	}
	for _, l := range opts.RemoveLabels {
		args = append(args, "--remove-label", l)
	}
	if len(args) == baseLen {
		return 0, nil
	}
	if err := s.runBDTransientWrite(args...); err != nil {
		if isBdNotFound(err) {
			return 0, fmt.Errorf("batch updating beads %v: %w", ids, ErrNotFound)
		}
		return 0, fmt.Errorf("batch updating beads %v: %w", ids, err)
	}
	return len(ids), nil
}

// WaitForParentProjection blocks until bd's parent-child listing projection
// reflects a successful reparent from oldParentID to newParentID for id.
func (s *BdStore) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	return s.waitForParentProjection(ctx, id, oldParentID, newParentID)
}

func (s *BdStore) waitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	ticker := time.NewTicker(bdParentProjectionPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		current, err := s.Get(id)
		if err == nil {
			switch current.ParentID {
			case newParentID:
				matches, matchErr := s.parentProjectionMatches(id, oldParentID, newParentID)
				if matchErr == nil && matches {
					return nil
				}
				lastErr = matchErr
			case oldParentID:
				lastErr = nil
			default:
				return fmt.Errorf("updating bead %q: %w", id, ErrParentProjectionSuperseded)
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("updating bead %q: waiting for parent projection from %q to %q: %w (last check error: %w)", id, oldParentID, newParentID, ctx.Err(), lastErr)
			}
			return fmt.Errorf("updating bead %q: waiting for parent projection from %q to %q: %w", id, oldParentID, newParentID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *BdStore) parentProjectionMatches(id, oldParentID, newParentID string) (bool, error) {
	if oldParentID != "" {
		oldChildren, err := s.List(ListQuery{ParentID: oldParentID})
		if err != nil {
			return false, fmt.Errorf("listing old parent %q children: %w", oldParentID, err)
		}
		if beadSliceContains(oldChildren, id) {
			return false, nil
		}
	}
	if newParentID != "" {
		newChildren, err := s.List(ListQuery{ParentID: newParentID})
		if err != nil {
			return false, fmt.Errorf("listing new parent %q children: %w", newParentID, err)
		}
		if !beadSliceContains(newChildren, id) {
			return false, nil
		}
	}
	return true, nil
}

func beadSliceContains(items []Bead, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

// SetMetadata sets a key-value metadata pair on a bead via bd update.
func (s *BdStore) SetMetadata(id, key, value string) error {
	err := s.runBDTransientWrite("update", "--json", id,
		"--set-metadata", key+"="+value)
	if err != nil {
		if isBdNotFound(err) {
			return fmt.Errorf("setting metadata on %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("setting metadata on %q: %w", id, err)
	}
	return nil
}

// SetMetadataBatch sets multiple key-value metadata pairs on a bead via
// sequential bd update calls. Note: not truly atomic for external stores,
// but each individual call is idempotent.
func (s *BdStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	args := []string{"update", "--json", id}
	keys := make([]string, 0, len(kvs))
	for k := range kvs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--set-metadata", k+"="+kvs[k])
	}
	err := s.runBDTransientWrite(args...)
	if err != nil {
		if isBdNotFound(err) {
			return fmt.Errorf("setting metadata on %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("setting metadata on %q: %w", id, err)
	}
	return nil
}

// Tx executes fn against a staged BdStore transaction. BdStore reads each bead
// on first touch, applies callback writes to that snapshot, and reasserts the
// staged fields when fn returns; concurrent edits to the same bead fields made
// during the callback may be overwritten.
func (s *BdStore) Tx(_ string, fn func(Tx) error) error {
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	tx := newBdStoreTx(s)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.apply()
}

type bdStoreTx struct {
	store *BdStore
	items map[string]*bdStoreTxItem
	order []string
}

type bdStoreTxItem struct {
	original Bead
	current  Bead
	touched  bdStoreTxTouched
	updated  bool
	closed   bool
}

type bdStoreTxTouched struct {
	title       bool
	status      bool
	beadType    bool
	priority    bool
	description bool
	parentID    bool
	assignee    bool
}

func newBdStoreTx(store *BdStore) *bdStoreTx {
	return &bdStoreTx{
		store: store,
		items: make(map[string]*bdStoreTxItem),
	}
}

func (tx *bdStoreTx) item(id string) (*bdStoreTxItem, error) {
	if item, ok := tx.items[id]; ok {
		return item, nil
	}
	bead, err := tx.store.Get(id)
	if err != nil {
		return nil, err
	}
	item := &bdStoreTxItem{
		original: snapshotBdStoreTxBead(bead),
		current:  bead,
	}
	tx.items[id] = item
	tx.order = append(tx.order, id)
	return item, nil
}

// Create persists a bead immediately. The bd CLI has no multi-statement
// transaction, so a create cannot be staged: subsequent staged writes in the
// same Tx may reference the new bead's ID, which only exists after creation.
// This matches the Store.Tx contract for stores without native transactions.
func (tx *bdStoreTx) Create(b Bead) (Bead, error) {
	return tx.store.Create(b)
}

func (tx *bdStoreTx) Update(id string, opts UpdateOpts) error {
	if !hasUpdateOpts(opts) {
		return nil
	}
	item, err := tx.item(id)
	if err != nil {
		return err
	}
	item.current = applyUpdateOptsToBead(item.current, opts)
	item.touched.note(opts)
	item.updated = true
	return nil
}

func (tx *bdStoreTx) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	return tx.Update(id, UpdateOpts{Metadata: kvs})
}

func (tx *bdStoreTx) Close(id string) error {
	item, err := tx.item(id)
	if err != nil {
		return err
	}
	item.current.Status = "closed"
	item.closed = true
	return nil
}

func (tx *bdStoreTx) apply() error {
	for _, id := range tx.order {
		item := tx.items[id]
		if item.closed {
			if item.updated {
				opts := item.preservedUpdateOpts(false)
				if hasUpdateOpts(opts) {
					if err := tx.store.Update(id, opts); err != nil {
						return err
					}
					if err := tx.store.waitForUpdateProjection(id, opts); err != nil {
						return err
					}
				}
			}
			if err := tx.store.close(id, strings.TrimSpace(item.current.Metadata["close_reason"])); err != nil {
				return err
			}
			if !item.updated {
				continue
			}
			opts := item.preservedUpdateOpts(true)
			if hasUpdateOpts(opts) {
				if err := tx.store.Update(id, opts); err != nil {
					return err
				}
				if err := tx.store.waitForUpdateProjection(id, opts); err != nil {
					return err
				}
			}
			continue
		}
		opts := item.preservedUpdateOpts(true)
		if !hasUpdateOpts(opts) {
			continue
		}
		if err := tx.store.Update(id, opts); err != nil {
			return err
		}
	}
	return nil
}

func (s *BdStore) waitForUpdateProjection(id string, opts UpdateOpts) error {
	ctx, cancel := context.WithTimeout(context.Background(), bdTxProjectionTimeout)
	defer cancel()

	ticker := time.NewTicker(bdParentProjectionPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		current, err := s.Get(id)
		if err == nil {
			if updateProjectionMatches(current, opts) {
				return nil
			}
			lastErr = nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("updating bead %q: waiting for tx update projection: %w (last check error: %w)", id, ctx.Err(), lastErr)
			}
			return fmt.Errorf("updating bead %q: waiting for tx update projection: %w", id, ctx.Err())
		case <-ticker.C:
		}
	}
}

func updateProjectionMatches(current Bead, opts UpdateOpts) bool {
	if opts.Title != nil && current.Title != *opts.Title {
		return false
	}
	if opts.Status != nil && current.Status != *opts.Status {
		return false
	}
	if opts.Type != nil && current.Type != *opts.Type {
		return false
	}
	if opts.Priority != nil {
		if current.Priority == nil || *current.Priority != *opts.Priority {
			return false
		}
	}
	if opts.Description != nil && current.Description != *opts.Description {
		return false
	}
	if opts.ParentID != nil && current.ParentID != *opts.ParentID {
		return false
	}
	if opts.Assignee != nil && current.Assignee != *opts.Assignee {
		return false
	}
	for key, value := range opts.Metadata {
		if current.Metadata[key] != value {
			return false
		}
	}
	for _, label := range opts.Labels {
		if !bdStoreStringSliceContains(current.Labels, label) {
			return false
		}
	}
	for _, label := range opts.RemoveLabels {
		if bdStoreStringSliceContains(current.Labels, label) {
			return false
		}
	}
	return true
}

func bdStoreStringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (item *bdStoreTxItem) preservedUpdateOpts(includeStatus bool) UpdateOpts {
	current := item.current
	opts := UpdateOpts{}
	if current.Title != "" || item.touched.title {
		opts.Title = &current.Title
	}
	if includeStatus && (current.Status != "" || item.touched.status) {
		opts.Status = &current.Status
	}
	if current.Type != "" || item.touched.beadType {
		opts.Type = &current.Type
	}
	if current.Priority != nil || item.touched.priority {
		opts.Priority = cloneIntPtr(current.Priority)
	}
	if current.Description != "" || item.touched.description {
		opts.Description = &current.Description
	}
	if current.ParentID != "" || item.touched.parentID {
		opts.ParentID = &current.ParentID
	}
	if current.Assignee != "" || item.touched.assignee {
		opts.Assignee = &current.Assignee
	}
	if len(current.Metadata) > 0 {
		opts.Metadata = maps.Clone(current.Metadata)
	}
	// bd update can clobber unspecified fields in dolt-server mode, so labels
	// are re-emitted as a full post-mutation set for staged Tx applies.
	opts.Labels = append([]string(nil), current.Labels...)
	opts.RemoveLabels = removedLabels(item.original.Labels, current.Labels)
	return opts
}

func snapshotBdStoreTxBead(bead Bead) Bead {
	bead.Metadata = maps.Clone(bead.Metadata)
	bead.Labels = append([]string(nil), bead.Labels...)
	return bead
}

func (t *bdStoreTxTouched) note(opts UpdateOpts) {
	t.title = t.title || opts.Title != nil
	t.status = t.status || opts.Status != nil
	t.beadType = t.beadType || opts.Type != nil
	t.priority = t.priority || opts.Priority != nil
	t.description = t.description || opts.Description != nil
	t.parentID = t.parentID || opts.ParentID != nil
	t.assignee = t.assignee || opts.Assignee != nil
}

func hasUpdateOpts(opts UpdateOpts) bool {
	return opts.Title != nil ||
		opts.Status != nil ||
		opts.Type != nil ||
		opts.Priority != nil ||
		opts.Description != nil ||
		opts.ParentID != nil ||
		opts.Assignee != nil ||
		len(opts.Metadata) > 0 ||
		len(opts.Labels) > 0 ||
		len(opts.RemoveLabels) > 0
}

func removedLabels(original, current []string) []string {
	if len(original) == 0 {
		return nil
	}
	kept := make(map[string]struct{}, len(current))
	for _, label := range current {
		kept[label] = struct{}{}
	}
	var removed []string
	for _, label := range original {
		if _, ok := kept[label]; !ok {
			removed = append(removed, label)
		}
	}
	return removed
}

func (s *BdStore) runBDTransientWrite(args ...string) error {
	_, err := s.runBDTransientWriteOutput(args...)
	return err
}

func (s *BdStore) runBDTransientWriteOutput(args ...string) ([]byte, error) {
	return s.runBDTransientWriteOutputWhen(isBdTransientWriteError, args...)
}

func (s *BdStore) runBDTransientCreateOutput(hasStableID bool, args ...string) ([]byte, error) {
	return s.runBDTransientWriteOutputWhen(func(err error) bool {
		if !isBdTransientWriteError(err) {
			return false
		}
		return hasStableID || !isBdAmbiguousWriteError(err)
	}, args...)
}

func (s *BdStore) runBDTransientWriteOutputWhen(shouldRetry func(error) bool, args ...string) ([]byte, error) {
	var err error
	var out []byte
	args = s.bdTransientWriteArgs(args)
	for attempt := 1; attempt <= bdTransientWriteAttempts; attempt++ {
		out, err = s.runner(s.dir, "bd", args...)
		if err == nil || !shouldRetry(err) || attempt == bdTransientWriteAttempts {
			return out, err
		}
		time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
	}
	return out, err
}

// runBDTransientRead runs a read-only bd command, retrying on transient Dolt
// connection errors (invalid connection, broken pipe, etc.). Reads are
// idempotent so retry is unconditional on isBdAmbiguousWriteError, with no
// stable-ID guard needed.
func (s *BdStore) runBDTransientRead(args ...string) ([]byte, error) {
	var (
		out []byte
		err error
	)
	for attempt := 1; attempt <= bdTransientReadAttempts; attempt++ {
		out, err = s.runner(s.dir, "bd", args...)
		if err == nil || !isBdAmbiguousWriteError(err) || attempt == bdTransientReadAttempts {
			return out, err
		}
		time.Sleep(time.Duration(attempt) * 50 * time.Millisecond)
	}
	return out, err
}

func (s *BdStore) bdTransientWriteArgs(args []string) []string {
	if !s.isDoltliteBackend() {
		return args
	}
	out := []string{"--dolt-auto-commit", "off"}
	out = append(out, args...)
	return out
}

func (s *BdStore) isDoltliteBackend() bool {
	metaPath := filepath.Join(s.dir, ".beads", "metadata.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return false
	}
	ok, err := metadataDeclaresDoltlite(data)
	if err != nil {
		return false
	}
	return ok
}

func metadataDeclaresDoltlite(data []byte) (bool, error) {
	var meta struct {
		Backend  string `json:"backend"`
		Database string `json:"database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return false, err
	}
	return isDoltliteMetadata(meta.Backend, meta.Database), nil
}

func isDoltliteMetadata(backend, database string) bool {
	return strings.EqualFold(strings.TrimSpace(backend), "doltlite") ||
		strings.EqualFold(strings.TrimSpace(database), "doltlite")
}

func isBdTransientWriteError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1213 (40001): serialization failure") ||
		strings.Contains(msg, "this transaction conflicts with a committed transaction") ||
		strings.Contains(msg, "failed to prepare catalog") ||
		isBdSqliteBusyError(msg) ||
		isBdAmbiguousWriteError(err)
}

// isBdSqliteBusyError reports whether msg carries an explicit sqlite
// busy/locked result-code marker ("database is locked (5) (SQLITE_BUSY)"
// and friends) — the sqlite analog of a Dolt serialization failure: the
// write lost a lock race without applying, so it is safe to retry. Only
// the unambiguous SQLITE_BUSY / SQLITE_LOCKED code markers match. bd's
// sqlite driver (modernc.org/sqlite) always appends the code marker, so
// this loses no real coverage, while bare "database is locked" phrasings
// stay excluded on purpose: Dolt's embedded mode emits "database is
// locked by another dolt process" for a persistent lock-file condition
// that a bounded retry cannot clear and must keep failing fast.
func isBdSqliteBusyError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "sqlite_busy") ||
		strings.Contains(lower, "sqlite_locked")
}

func isBdAmbiguousWriteError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "invalid connection") ||
		strings.Contains(msg, "bad connection") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "timed out after") ||
		strings.Contains(msg, "deadline exceeded")
}

// Ping verifies the bd binary is accessible by running a no-op command.
func (s *BdStore) Ping() error {
	_, err := s.runner(s.dir, "bd", "list", "--json", "--limit", "0")
	if err != nil {
		return fmt.Errorf("bd store ping: %w", err)
	}
	return nil
}

// CloseAll closes multiple beads in batch and sets metadata on each.
// Idempotent: closing an already-closed bead returns nil.
//
// Forwards metadata["close_reason"] as the --reason argument to bd close,
// so callers can satisfy validators like validation.on-close=error (which
// rejects close calls without an explicit --reason of >=20 characters).
// Whitespace is trimmed; an empty or whitespace-only value is treated as
// absent and no --reason flag is added, preserving backward compatibility
// for callers that don't pre-stamp a reason. The same map is also written
// via SetMetadataBatch on each bead before close, so the reason is persisted
// in the bead's metadata as well as forwarded to bd. If batch close falls
// back to per-id closes, the same shared reason is forwarded to every
// fallback close.
func (s *BdStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	// Set metadata on all beads first (before closing, since some stores
	// prevent metadata writes on closed beads).
	if len(metadata) > 0 {
		if err := s.setMetadataBatchAll(ids, metadata); err != nil {
			return 0, err
		}
	}

	// Batch close: bd close [--reason "..."] id1 id2 id3 ...
	reason := strings.TrimSpace(metadata["close_reason"])
	args := bdCloseArgs(reason, ids...)
	err := s.runBDTransientWrite(args...)
	if err != nil {
		// Fall back to individual closes on batch failure.
		closed := 0
		var fallbackErr error
		for _, id := range ids {
			if closeErr := s.close(id, reason); closeErr == nil {
				closed++
			} else {
				fallbackErr = errors.Join(fallbackErr, closeErr)
			}
		}
		if fallbackErr != nil {
			return closed, errors.Join(fmt.Errorf("bd close batch: %w", err), fallbackErr)
		}
		return closed, nil
	}
	return len(ids), nil
}

func (s *BdStore) setMetadataBatchAll(ids []string, kvs map[string]string) error {
	if len(ids) == 0 || len(kvs) == 0 {
		return nil
	}
	args := []string{"update", "--json"}
	args = append(args, ids...)
	keys := make([]string, 0, len(kvs))
	for k := range kvs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--set-metadata", k+"="+kvs[k])
	}
	err := s.runBDTransientWrite(args...)
	if err == nil {
		return nil
	}
	if isBdNotFound(err) {
		if len(ids) == 1 {
			return fmt.Errorf("setting metadata on %q: %w", ids[0], ErrNotFound)
		}
		return fmt.Errorf("setting metadata on %d beads: %w", len(ids), ErrNotFound)
	}
	if len(ids) == 1 {
		return fmt.Errorf("setting metadata on %q: %w", ids[0], err)
	}
	return fmt.Errorf("setting metadata on %d beads: %w", len(ids), err)
}

// CloseAllWithReason closes multiple beads with one reasoned bd close command
// without pre-writing metadata on each bead.
func (s *BdStore) CloseAllWithReason(ids []string, reason string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	reason = strings.TrimSpace(reason)
	err := s.runBDTransientWrite(bdCloseArgs(reason, ids...)...)
	if err != nil {
		closed := 0
		var fallbackErr error
		for _, id := range ids {
			if closeErr := s.close(id, reason); closeErr == nil {
				closed++
			} else {
				fallbackErr = errors.Join(fallbackErr, closeErr)
			}
		}
		if fallbackErr != nil {
			return closed, errors.Join(fmt.Errorf("bd close batch: %w", err), fallbackErr)
		}
		return closed, nil
	}
	return len(ids), nil
}

// Close sets a bead's status to closed via bd close. If the bead already has
// metadata.close_reason, the trimmed value is forwarded as bd close --reason.
// Idempotent: closing an already-closed bead returns nil.
//
// Reads metadata.close_reason from the bead (set by callers like the
// session reconciler or convoy autoclose via SetMetadata or
// SetMetadataBatch before invoking Close) and forwards it as the
// --reason argument to bd close. Without this, bd assigns its default
// reason "Closed", silently discarding caller intent and (when the city
// runs with validation.on-close=error) failing the close outright.
//
// Callers are responsible for providing a reason that satisfies any
// configured validator — e.g. bd's validation.on-close=error rejects
// reasons under 20 characters. This function does not pad or rewrite
// the supplied reason; it forwards what the caller set, or omits
// --reason entirely when no metadata is set.
func (s *BdStore) Close(id string) error {
	reason := ""
	if b, err := s.Get(id); err == nil {
		reason = strings.TrimSpace(b.Metadata["close_reason"])
	}
	return s.close(id, reason)
}

// CloseWithReason closes a bead with an explicit reason without first reading
// the bead metadata. Callers that need close_reason persisted for audit trails
// should write metadata before calling this method.
func (s *BdStore) CloseWithReason(id, reason string) error {
	return s.close(id, strings.TrimSpace(reason))
}

func bdCloseArgs(reason string, ids ...string) []string {
	args := []string{"close", "--force", "--json"}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	return append(args, ids...)
}

func (s *BdStore) close(id, reason string) error {
	// Internal callers supply canonical full IDs; exact-ID guard lives at the
	// CLI/API entry points (gcy-g4o).
	err := s.runBDTransientWrite(bdCloseArgs(reason, id)...)
	if err != nil {
		// Some bd error paths collapse to a bare exit status without a helpful
		// not-found string. Re-read the bead to distinguish "already closed" from
		// true not-found and map both cases deterministically.
		if b, getErr := s.Get(id); getErr == nil && b.Status == "closed" {
			return nil
		} else if getErr != nil && (isBdNotFound(err) || errors.Is(getErr, ErrNotFound)) {
			return fmt.Errorf("closing bead %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("closing bead %q: %w", id, err)
	}
	// Honesty guard: bd close can exit 0 yet leave the bead un-closed when an
	// import-revert race (gastownhall/beads#3948) rolls the committed close
	// back to open after the CLI has already returned. Trust the store, not the
	// exit code — re-read and confirm the status landed. A failed re-read is
	// not positive evidence of a revert, so we keep trusting the reported
	// success in that case rather than masking it with a synthetic failure.
	if b, getErr := s.Get(id); getErr == nil && b.Status != "closed" {
		return fmt.Errorf("closing bead %q: bd close exited 0 but status is %q, not closed; suspected gastownhall/beads#3948 import-revert race", id, b.Status)
	}
	return nil
}

// Reopen sets a closed bead's status to open via bd reopen.
func (s *BdStore) Reopen(id string) error {
	err := s.runBDTransientWrite("reopen", "--json", id)
	if err != nil {
		if isBdNotFound(err) {
			return fmt.Errorf("reopening bead %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("reopening bead %q: %w", id, err)
	}
	return nil
}

// Delete permanently removes a bead from the store via bd delete.
func (s *BdStore) Delete(id string) error {
	// Internal callers supply canonical full IDs; exact-ID guard lives at the
	// CLI/API entry points (gcy-g4o).
	err := s.runBDTransientWrite("delete", "--force", "--json", id)
	if err != nil {
		if isBdNotFound(err) {
			return fmt.Errorf("deleting bead %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("deleting bead %q: %w", id, err)
	}
	return nil
}

// bdDeleteBatchChunk bounds how many ids ride on a single `bd delete`
// invocation so a large closure stays within command-line argument limits.
const bdDeleteBatchChunk = 256

// DeleteBatch removes exactly the given beads with batched `bd delete … --force`
// calls. `--force` deletes the listed ids and orphans external dependents — it
// removes every dependency link touching each deleted bead (any type, both
// directions) and leaves beads that merely depend on them alive, matching the
// per-bead Delete path (BdStore.Delete also uses `--force`). It is
// deliberately NOT `--cascade`, which would recursively delete dependent issues
// outside the collected closure. --force tolerates ids that are already gone,
// and ids are chunked to respect command-line limits. DeleteBatch is the
// batched counterpart to Delete that lets the wisp GC tear down a molecule
// closure with a handful of subprocesses instead of one per bead and edge. It
// satisfies BatchDeleter.
//
// Each chunk is a separate committed `bd delete` subprocess, so a later chunk
// can fail after earlier chunks are durably gone. On such a partial failure it
// returns a *BatchDeleteError carrying the ids from the fully-committed earlier
// chunks, letting a caching layer reconcile exactly those instead of treating
// the whole batch as untouched.
func (s *BdStore) DeleteBatch(ids []string) error {
	for start := 0; start < len(ids); start += bdDeleteBatchChunk {
		end := start + bdDeleteBatchChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		args := make([]string, 0, len(chunk)+2)
		args = append(args, "delete")
		args = append(args, chunk...)
		args = append(args, "--force")
		if err := s.runBDTransientWrite(args...); err != nil {
			return &BatchDeleteError{
				Committed: append([]string(nil), ids[:start]...),
				Err:       fmt.Errorf("batch delete of %d bead(s): %w", len(chunk), err),
			}
		}
	}
	return nil
}

// List returns beads matching the query via bd list and bd query.
func (s *BdStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("bd list: %w", ErrQueryRequiresScan)
	}

	switch query.TierMode {
	case TierWisps:
		return s.listWispsTier(query)
	case TierBoth:
		return s.listBothTiers(query)
	}
	return s.listViaBDList(query)
}

func (s *BdStore) listViaBDList(query ListQuery) ([]Bead, error) {
	serverQuery, clientFilteredAssignees := bdServerQueryForAssignees(query)
	limit := serverQuery.Limit
	if bdListRequiresClientLimit(query, serverQuery, clientFilteredAssignees) {
		limit = 0
	}
	args := []string{"list", "--json"}
	if serverQuery.Label != "" {
		args = append(args, "--label="+serverQuery.Label)
	}
	if serverQuery.Assignee != "" {
		args = append(args, "--assignee="+serverQuery.Assignee)
	}
	if serverQuery.Status != "" {
		args = append(args, "--status="+serverQuery.Status)
	}
	if serverQuery.Type != "" {
		args = append(args, "--type="+serverQuery.Type)
	}
	if serverQuery.IncludeClosed || serverQuery.Status == "closed" {
		args = append(args, "--all")
	}
	if !serverQuery.CreatedBefore.IsZero() {
		args = append(args, "--created-before", serverQuery.CreatedBefore.Format(time.RFC3339Nano))
	}
	args = append(args, "--include-infra", "--include-gates")
	if bdListShouldIncludeTemplates(query) {
		args = append(args, "--include-templates")
	}
	args = append(args, "--limit", fmt.Sprintf("%d", limit))
	if serverQuery.ParentID != "" {
		args = append(args, "--parent", serverQuery.ParentID)
	}
	if len(serverQuery.Metadata) > 0 {
		keys := make([]string, 0, len(serverQuery.Metadata))
		for k := range serverQuery.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--metadata-field", k+"="+serverQuery.Metadata[k])
		}
	}
	if query.SkipLabels && serverQuery.Label == "" && s.listSkipLabelsEnabled {
		args = append(args, "--skip-labels")
	}

	out, err := s.runBDTransientRead(args...)
	if err != nil {
		return nil, fmt.Errorf("bd list: %w", err)
	}
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, len(issues))
	for i := range issues {
		result[i] = issues[i].toBead()
	}
	filtered := applyListQuery(result, query)
	if parseErr != nil {
		if len(filtered) == 0 {
			return nil, fmt.Errorf("bd list: %w", parseErr)
		}
		// Surface partial-parse outcomes so callers can distinguish a complete
		// list from one that silently dropped entries. Treating a partial list
		// as authoritative has driven a runaway cache-reconcile loop in the
		// past (synthesizing bead.closed for beads that were merely dropped
		// by parseIssuesTolerant).
		return filtered, &PartialResultError{Op: "bd list", Err: parseErr}
	}
	return filtered, nil
}

func bdListRequiresClientLimit(query, serverQuery ListQuery, clientFilteredAssignees bool) bool {
	if query.TierMode == TierIssues || query.TierMode == TierWisps {
		return true
	}
	if serverQuery.Sort == SortCreatedAsc || clientFilteredAssignees {
		return true
	}
	if len(serverQuery.Metadata) > 0 || !serverQuery.CreatedBefore.IsZero() || !serverQuery.UpdatedBefore.IsZero() {
		return true
	}
	// bd list exposes no compound (created_at, id) seek flag; the boundary is
	// resolved Go-side (identical tie-break to the in-memory sort), so a
	// bd-side limit would cut rows before that filter runs — fetch unbounded
	// and let applyListQuery filter then limit.
	if serverQuery.SeekAfter != nil {
		return true
	}
	return false
}

func bdListShouldIncludeTemplates(query ListQuery) bool {
	return query.TierMode == TierWisps || (query.TierMode == TierBoth && query.Type != "message")
}

func bdServerQueryForAssignees(query ListQuery) (ListQuery, bool) {
	serverQuery := query
	if query.Assignee != "" {
		return serverQuery, false
	}
	switch len(query.Assignees) {
	case 0:
		return serverQuery, false
	case 1:
		serverQuery.Assignee = query.Assignees[0]
		serverQuery.Assignees = nil
		return serverQuery, false
	default:
		serverQuery.Assignees = nil
		serverQuery.AllowScan = true
		return serverQuery, true
	}
}

func (s *BdStore) listWispsTier(query ListQuery) ([]Bead, error) {
	listQ := query
	listQ.TierMode = TierWisps
	listResult, listErr := s.listViaBDList(listQ)

	ephemeralQ := query
	ephemeralQ.TierMode = TierWisps
	ephemeralResult, ephemeralErr := s.listEphemeral(ephemeralQ)

	return mergeListTierResults(query, "bd list wisps tier", listResult, listErr, ephemeralResult, ephemeralErr)
}

// listEphemeral reads only ephemeral rows using `bd query "ephemeral=true AND
// <filters>"`. The installed bd list surface does not expose ephemeral rows, so
// TierWisps and TierBoth must union this path with bd list results.
func (s *BdStore) listEphemeral(query ListQuery) ([]Bead, error) {
	serverQuery, clientFilteredAssignees := bdServerQueryForAssignees(query)
	clauses := []string{"ephemeral=true"}
	serverFilteredOnly := !clientFilteredAssignees
	clauses, serverFilteredOnly = appendBdQueryClause(clauses, serverFilteredOnly, "label", serverQuery.Label)
	clauses, serverFilteredOnly = appendBdQueryClause(clauses, serverFilteredOnly, "status", serverQuery.Status)
	clauses, serverFilteredOnly = appendBdQueryClause(clauses, serverFilteredOnly, "type", serverQuery.Type)
	clauses, serverFilteredOnly = appendBdQueryClause(clauses, serverFilteredOnly, "assignee", serverQuery.Assignee)
	clauses, serverFilteredOnly = appendBdQueryClause(clauses, serverFilteredOnly, "parent", serverQuery.ParentID)

	args := []string{"query", "--json", strings.Join(clauses, " AND ")}
	if serverQuery.IncludeClosed || serverQuery.Status == "closed" {
		args = append(args, "--all")
	}
	wispsLimit := 0
	if query.Limit > 0 && serverFilteredOnly && canApplyWispsServerLimit(query) {
		wispsLimit = query.Limit
	}
	args = append(args, "--limit", strconv.Itoa(wispsLimit))

	// #3288: route the wisp `bd query` through the retried/deadline-bounded
	// transient-read path (as listViaBDList already does) so BOTH subprocesses
	// inside listWispsTier are bounded — a bare s.runner call here would be the
	// one unretried read on the hot tier-merge path.
	out, err := s.runBDTransientRead(args...)
	if err != nil {
		if isBdQueryUnsupported(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("bd query (wisps): %w", err)
	}
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, len(issues))
	for i := range issues {
		result[i] = issues[i].toBead()
		result[i].Ephemeral = true
		result[i].NoHistory = false
	}
	filtered := applyListQuery(result, query)
	if parseErr != nil {
		if len(filtered) > 0 {
			return filtered, &PartialResultError{Op: "bd query", Err: parseErr}
		}
		return filtered, fmt.Errorf("bd query: %w", parseErr)
	}
	return filtered, nil
}

// getEphemeralByID looks up a single wisp-tier bead by exact ID using bd query.
// bd show does not expose the wisps table, so this is the fallback for Get.
// isWispQueryableID reports whether id is safe to interpolate into a bd query
// clause as a bead ID: non-empty, ASCII letters/digits/hyphens only. Session
// names ("rig/agent.name") and other non-bead identifiers are excluded so the
// wisp-tier Get fallback never issues supplemental queries for them.
func isWispQueryableID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

func (s *BdStore) getEphemeralByID(id string) ([]Bead, error) {
	clause := "ephemeral=true AND id=" + id
	args := []string{"query", "--json", clause, "--all", "--limit", "1"}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		if isBdQueryUnsupported(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("bd query (wisp by id): %w", err)
	}
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, len(issues))
	for i := range issues {
		result[i] = issues[i].toBead()
		result[i].Ephemeral = true
	}
	if parseErr != nil {
		return result, fmt.Errorf("bd query (wisp by id): %w", parseErr)
	}
	return result, nil
}

func isBdQueryUnsupported(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unknown subcommand \"query\"") ||
		strings.Contains(text, "unknown command \"query\"") ||
		strings.Contains(text, "unknown subcommand query") ||
		strings.Contains(text, "unknown command query")
}

func canApplyWispsServerLimit(query ListQuery) bool {
	// SeekAfter: the compound (created_at, id) boundary is resolved Go-side
	// (identical tie-break to the in-memory sort), not via a bd query flag, so
	// a bd-side limit would cut rows before that filter runs — same class as
	// CreatedBefore.
	return (query.Sort == SortDefault || query.Sort == SortCreatedDesc) &&
		query.CreatedBefore.IsZero() &&
		query.UpdatedBefore.IsZero() &&
		len(query.Metadata) == 0 &&
		query.SeekAfter == nil
}

func appendBdQueryClause(clauses []string, serverFilteredOnly bool, field, value string) ([]string, bool) {
	if value == "" {
		return clauses, serverFilteredOnly
	}
	if !isBareBdQueryValue(value) {
		return clauses, false
	}
	return append(clauses, field+"="+value), serverFilteredOnly
}

func isBareBdQueryValue(value string) bool {
	upper := strings.ToUpper(value)
	if upper == "AND" || upper == "OR" || upper == "NOT" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == ':' || r == '.':
		default:
			return false
		}
	}
	return true
}

func (s *BdStore) listBothTiers(query ListQuery) ([]Bead, error) {
	listQ := query
	listQ.TierMode = TierBoth
	listResult, listErr := s.listViaBDList(listQ)

	ephemeralQ := query
	ephemeralQ.TierMode = TierWisps
	ephemeralResult, ephemeralErr := s.listEphemeral(ephemeralQ)

	return mergeListTierResults(query, "bd list both tiers", listResult, listErr, ephemeralResult, ephemeralErr)
}

func mergeListTierResults(query ListQuery, op string, primary []Bead, primaryErr error, ephemeral []Bead, ephemeralErr error) ([]Bead, error) {
	if primaryErr != nil && ephemeralErr != nil {
		return nil, errors.Join(primaryErr, ephemeralErr)
	}

	merged := make([]Bead, 0, len(primary)+len(ephemeral))
	seen := make(map[string]int, len(primary)+len(ephemeral))
	add := func(b Bead) {
		if idx, ok := seen[b.ID]; ok {
			if b.Ephemeral && !merged[idx].Ephemeral {
				merged[idx] = b
			}
			return
		}
		seen[b.ID] = len(merged)
		merged = append(merged, b)
	}
	for _, b := range primary {
		add(b)
	}
	for _, b := range ephemeral {
		add(b)
	}
	sortBeadsForQuery(merged, query.Sort)
	if query.Limit > 0 && len(merged) > query.Limit {
		merged = merged[:query.Limit]
	}

	switch {
	case primaryErr != nil:
		if len(merged) > 0 {
			return merged, &PartialResultError{Op: op, Err: fmt.Errorf("bd list: %w", primaryErr)}
		}
		return merged, fmt.Errorf("%s: bd list: %w", op, primaryErr)
	case ephemeralErr != nil:
		if len(merged) > 0 {
			return merged, &PartialResultError{Op: op, Err: fmt.Errorf("bd query: %w", ephemeralErr)}
		}
		return merged, fmt.Errorf("%s: bd query: %w", op, ephemeralErr)
	default:
		return merged, nil
	}
}

// ListOpen returns non-closed beads via bd list. Pass a status to filter further.
func (s *BdStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
	}
	return s.List(query)
}

// ListByLabel returns beads matching an exact label via bd list --label.
// Limit controls max results (0 = unlimited). Results are ordered by bd's
// default sort (newest first). Pass IncludeClosed to include closed beads.
func (s *BdStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to the given agent with the specified
// status via bd list --assignee --status. Limit controls max results (0 = unlimited).
func (s *BdStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
		Sort:     SortCreatedDesc,
	})
}

// ListByMetadata returns beads matching all given metadata key=value filters.
// Limit controls max results (0 = unlimited). Results use bd's default order.
// Pass IncludeClosed to include closed beads.
func (s *BdStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// Children returns beads whose ParentID matches the given ID. Pass
// IncludeClosed to include closed children.
func (s *BdStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedAsc,
	})
}

// Ready returns open ready beads via bd ready, including ephemeral rows for
// wisp-aware tier modes.
func (s *BdStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(query)
	includeEphemeral := q.TierMode == TierBoth || q.TierMode == TierWisps
	args := bdReadyArgs(q, includeEphemeral)
	out, err := s.runBDTransientRead(args...)
	if err != nil {
		return nil, fmt.Errorf("bd ready: %w", err)
	}
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, 0, len(issues))
	now := time.Now().UTC()
	for i := range issues {
		bead := issues[i].toBead()
		if !IsReadyCandidateForTier(bead, now, q.TierMode) {
			continue
		}
		if q.Assignee != "" && bead.Assignee != q.Assignee {
			continue
		}
		result = append(result, bead)
		if q.Limit > 0 && len(result) >= q.Limit {
			break
		}
	}
	if parseErr != nil {
		if len(result) == 0 {
			return nil, fmt.Errorf("bd ready: %w", parseErr)
		}
		return result, &PartialResultError{Op: "bd ready", Err: parseErr}
	}
	return result, nil
}

func bdReadyArgs(q ReadyQuery, includeEphemeral bool) []string {
	args := []string{"ready", "--json"}
	if includeEphemeral {
		args = append(args, "--include-ephemeral")
	}
	if q.Assignee != "" {
		args = append(args, "--assignee", q.Assignee)
	}
	args = append(args, "--limit", "0")
	return args
}

// DepAdd records a dependency via bd dep add.
func (s *BdStore) DepAdd(issueID, dependsOnID, depType string) error {
	if depType == "parent-child" {
		bead, err := s.Get(issueID)
		if err == nil && bead.ParentID == dependsOnID {
			return nil
		}
	}
	err := s.runBDTransientWrite("dep", "add", issueID, dependsOnID, "--type", depType)
	if err != nil {
		return fmt.Errorf("adding dep %s→%s: %w", issueID, dependsOnID, err)
	}
	return nil
}

// DepRemove removes a dependency via bd dep remove.
func (s *BdStore) DepRemove(issueID, dependsOnID string) error {
	err := s.runBDTransientWrite("dep", "remove", issueID, dependsOnID)
	if err != nil {
		return fmt.Errorf("removing dep %s→%s: %w", issueID, dependsOnID, err)
	}
	return nil
}

// bdDepIssue is the JSON shape returned by bd dep list --json.
// It's a bdIssue with an added dependency_type field.
type bdDepIssue struct {
	bdIssue
	DepType string `json:"dependency_type"`
}

// DepList returns dependencies via bd dep list --json.
func (s *BdStore) DepList(id, direction string) ([]Dep, error) {
	args := []string{"dep", "list", id, "--json"}
	if direction == "up" {
		args = append(args, "--direction=up")
	}
	out, err := s.runBDTransientRead(args...)
	if err != nil {
		// Empty dep list may return error on some bd versions.
		if isBdNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing deps for %q: %w", id, err)
	}
	extracted := extractJSON(out)
	if len(extracted) == 0 || string(extracted) == "[]" {
		return nil, nil
	}
	var depIssues []bdDepIssue
	if err := json.Unmarshal(extracted, &depIssues); err != nil {
		return nil, fmt.Errorf("bd dep list: parsing JSON: %w", err)
	}
	result := make([]Dep, len(depIssues))
	for i, di := range depIssues {
		depType := di.DepType
		if depType == "" {
			depType = "blocks"
		}
		switch direction {
		case "up":
			// "up" query on id: returned issues depend on id.
			result[i] = Dep{IssueID: di.ID, DependsOnID: id, Type: depType}
		default:
			// "down" query on id: id depends on returned issues.
			result[i] = Dep{IssueID: id, DependsOnID: di.ID, Type: depType}
		}
	}
	return result, nil
}

// DepListBatch fetches "down" deps for multiple issue IDs in a single bd
// subprocess call. Returns a map from issue ID to its deps.
func (s *BdStore) DepListBatch(ids []string) (map[string][]Dep, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"dep", "list"}, ids...)
	args = append(args, "--json")
	out, err := s.runBDTransientRead(args...)
	if err != nil {
		if isBdNotFound(err) {
			return make(map[string][]Dep), nil
		}
		return nil, fmt.Errorf("batch dep list: %w", err)
	}
	extracted := extractJSON(out)
	if len(extracted) == 0 || string(extracted) == "[]" {
		return make(map[string][]Dep), nil
	}
	// Batch bd dep list returns raw dependency records:
	// [{"issue_id":"ga-1","depends_on_id":"ga-2","type":"blocks"}, ...]
	var records []struct {
		IssueID     string `json:"issue_id"`
		DependsOnID string `json:"depends_on_id"`
		Type        string `json:"type"`
	}
	if err := json.Unmarshal(extracted, &records); err != nil {
		return nil, fmt.Errorf("batch dep list: parsing JSON: %w", err)
	}
	result := make(map[string][]Dep, len(ids))
	for _, r := range records {
		depType := r.Type
		if depType == "" {
			depType = "blocks"
		}
		result[r.IssueID] = append(result[r.IssueID], Dep{
			IssueID:     r.IssueID,
			DependsOnID: r.DependsOnID,
			Type:        depType,
		})
	}
	return result, nil
}
