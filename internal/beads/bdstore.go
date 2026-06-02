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
	// ready, show, stats). Default matches bdCommandTimeout to preserve
	// pre-bounded behavior; lowered in follow-up work after slow read
	// paths are identified.
	bdReadCommandTimeout = 120 * time.Second
	// bdGraphApplyCommandTimeout bounds atomic graph creation below callers'
	// outer command budgets so transient Dolt stalls can retry or fall back.
	bdGraphApplyCommandTimeout = 45 * time.Second
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
	return func(dir, name string, args ...string) ([]byte, error) {
		start := time.Now()
		trace := func(status string, err error) {
			// GC_BD_TRACE_JSON wins: when the structured JSONL trace
			// (via TraceBDCall in bdtrace.go) is enabled, suppress the
			// legacy line-format trace so the two don't interleave
			// incompatible records in the same file when an operator
			// points both env vars at the same path.
			if strings.TrimSpace(os.Getenv("GC_BD_TRACE_JSON")) != "" {
				return
			}
			path := strings.TrimSpace(os.Getenv("GC_BD_TRACE"))
			if path == "" {
				return
			}
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
		trace("start", nil)
		timeout := bdCommandTimeoutFor(name, args)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var slowTimer *time.Timer
		if name == "bd" {
			bdArgs := append([]string(nil), args...)
			agentID := bdTelemetryAgentID(env)
			slowTimer = time.AfterFunc(bdSlowTelemetryThreshold, func() {
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
		baseEnv := processEnvSnapshotExcludingNativeDoltOpen()
		if len(env) > 0 {
			cmd.Env = mergeEnv(baseEnv, env)
		} else {
			cmd.Env = baseEnv
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if name == "bd" {
			// Structured JSONL trace — independent of the legacy line-format
			// trace above (gated by GC_BD_TRACE_JSON, not GC_BD_TRACE).
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
				err, out, stderr.String())
		}
		if ctx.Err() == context.DeadlineExceeded {
			timeoutErr := fmt.Errorf("timed out after %s", timeout)
			trace("timeout", timeoutErr)
			if stderr.Len() > 0 {
				return out, fmt.Errorf("%w: %s", timeoutErr, stderr.String())
			}
			return out, timeoutErr
		}
		if err != nil {
			// bd writes structured errors to stdout (JSON envelope) when
			// invoked with --json, while stderr is often empty. Surface
			// whichever stream has content so supervisor logs become
			// actionable instead of bare "exit status 1".
			detail := strings.TrimSpace(stderr.String())
			if detail == "" && name == "bd" {
				detail = bdStdoutErrorDetail(out)
			}
			if detail != "" {
				trace("error", err)
				return out, fmt.Errorf("%w: %s", err, detail)
			}
		}
		trace("done", err)
		return out, err
	}
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
	case "count", "list", "ready", "show", "stats":
		return bdReadCommandTimeout
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
}

const bdTransientWriteAttempts = 3

// NewBdStore creates a BdStore rooted at dir using the given runner.
func NewBdStore(dir string, runner CommandRunner) *BdStore {
	return NewBdStoreWithPrefix(dir, runner, "")
}

// NewBdStoreWithPrefix creates a BdStore with an explicit owned bead ID prefix.
func NewBdStoreWithPrefix(dir string, runner CommandRunner, idPrefix string) *BdStore {
	return &BdStore{dir: dir, runner: runner, idPrefix: normalizeIDPrefix(idPrefix)}
}

// IDPrefix returns the bead ID prefix owned by this store, without trailing "-".
func (s *BdStore) IDPrefix() string {
	if s == nil {
		return ""
	}
	return s.idPrefix
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
	DeferUntil   *time.Time   `json:"defer_until,omitempty"`
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

// parseIssuesTolerant unmarshals a JSON array of bdIssue objects, skipping
// any entries that fail to parse (e.g. corrupt metadata with non-string values).
// This prevents a single bad bead from breaking all list operations.
func parseIssuesTolerant(data []byte) ([]bdIssue, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
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
		DeferUntil:   cloneTimePtr(b.DeferUntil),
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
// bd uses several phrasings: "no issue found", "issue not found", "not found".
func isBdNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no issue found")
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

// Create persists a new bead via bd create.
func (s *BdStore) Create(b Bead) (Bead, error) {
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
	if b.Ephemeral {
		args = append(args, "--ephemeral")
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
	return created, nil
}

// Get retrieves a bead by ID via bd show.
func (s *BdStore) Get(id string) (Bead, error) {
	out, err := s.runner(s.dir, "bd", "show", "--json", id)
	if err != nil {
		if isBdNotFound(err) {
			return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
		}
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, err)
	}
	var issues []bdIssue
	if err := json.Unmarshal(extractJSON(out), &issues); err != nil {
		return Bead{}, fmt.Errorf("bd show: parsing JSON: %w", err)
	}
	if len(issues) == 0 {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	return issues[0].toBead(), nil
}

// Update modifies fields of an existing bead via bd update.
func (s *BdStore) Update(id string, opts UpdateOpts) error {
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
	// No fields to update — no-op (bd errors on empty update).
	if len(args) == 3 {
		return nil
	}
	err := s.runBDTransientWrite(args...)
	if err != nil {
		if isBdNotFound(err) {
			return fmt.Errorf("updating bead %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("updating bead %q: %w", id, err)
	}
	return nil
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
		isBdAmbiguousWriteError(err)
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
	for _, id := range ids {
		if len(metadata) > 0 {
			if err := s.SetMetadataBatch(id, metadata); err != nil {
				return 0, err
			}
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
	err := s.runBDTransientWrite("delete", "--force", "--json", id)
	if err != nil {
		if isBdNotFound(err) {
			return fmt.Errorf("deleting bead %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("deleting bead %q: %w", id, err)
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
		return s.listEphemeral(query)
	case TierBoth:
		if bdListCoversBothTiers(query) {
			return s.listViaBDList(query)
		}
		return s.listBothTiers(query)
	}

	return s.listViaBDList(query)
}

func bdListCoversBothTiers(query ListQuery) bool {
	return query.Type == "message"
}

func (s *BdStore) listViaBDList(query ListQuery) ([]Bead, error) {
	limit := query.Limit
	if query.Sort == SortCreatedAsc {
		limit = 0
	}
	args := []string{"list", "--json"}
	if query.Label != "" {
		args = append(args, "--label="+query.Label)
	}
	if query.Assignee != "" {
		args = append(args, "--assignee="+query.Assignee)
	}
	if query.Status != "" {
		args = append(args, "--status="+query.Status)
	}
	if query.Type != "" {
		args = append(args, "--type="+query.Type)
	}
	if query.IncludeClosed || query.Status == "closed" {
		args = append(args, "--all")
	}
	if !query.CreatedBefore.IsZero() {
		args = append(args, "--created-before", query.CreatedBefore.Format(time.RFC3339Nano))
	}
	args = append(args, "--include-infra", "--include-gates", "--limit", fmt.Sprintf("%d", limit))
	if query.ParentID != "" {
		args = append(args, "--parent", query.ParentID)
	}
	if len(query.Metadata) > 0 {
		keys := make([]string, 0, len(query.Metadata))
		for k := range query.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--metadata-field", k+"="+query.Metadata[k])
		}
	}

	out, err := s.runner(s.dir, "bd", args...)
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

// listEphemeral reads only the wisps tier using `bd query "ephemeral=true AND
// <filters>"`. For most bead types, bd query is the canonical way to reach the
// wisps table (mirrors gastown's internal/beads/beads.go listEphemeral path).
func (s *BdStore) listEphemeral(query ListQuery) ([]Bead, error) {
	clauses := []string{"ephemeral=true"}
	serverFilteredOnly := true
	clauses, serverFilteredOnly = appendBdQueryClause(clauses, serverFilteredOnly, "label", query.Label)
	clauses, serverFilteredOnly = appendBdQueryClause(clauses, serverFilteredOnly, "status", query.Status)
	clauses, serverFilteredOnly = appendBdQueryClause(clauses, serverFilteredOnly, "type", query.Type)
	clauses, serverFilteredOnly = appendBdQueryClause(clauses, serverFilteredOnly, "assignee", query.Assignee)
	clauses, serverFilteredOnly = appendBdQueryClause(clauses, serverFilteredOnly, "parent", query.ParentID)

	args := []string{"query", "--json", strings.Join(clauses, " AND ")}
	if query.IncludeClosed || query.Status == "closed" {
		args = append(args, "--all")
	}
	wispsLimit := 0
	if query.Limit > 0 && serverFilteredOnly && canApplyWispsServerLimit(query) {
		wispsLimit = query.Limit
	}
	args = append(args, "--limit", strconv.Itoa(wispsLimit))

	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return nil, fmt.Errorf("bd query (wisps): %w", err)
	}
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, len(issues))
	for i := range issues {
		result[i] = issues[i].toBead()
		// bd query against wisps returns ephemeral beads; tolerate older bd
		// versions that omit the ephemeral field in JSON.
		result[i].Ephemeral = true
	}
	// Re-apply filters client-side (defense in depth against bd-query DSL
	// drift) and re-cap Limit after client-only filters/sorts.
	filtered := applyListQuery(result, query)
	if parseErr != nil {
		if len(filtered) > 0 {
			return filtered, &PartialResultError{Op: "bd query", Err: parseErr}
		}
		return filtered, fmt.Errorf("bd query: %w", parseErr)
	}
	return filtered, nil
}

func canApplyWispsServerLimit(query ListQuery) bool {
	return (query.Sort == SortDefault || query.Sort == SortCreatedDesc) &&
		query.CreatedBefore.IsZero() &&
		len(query.Metadata) == 0
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

// isBareBdQueryValue reports whether value can be emitted unquoted into the bd
// query DSL. Values outside this narrow token set are filtered client-side.
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

// listBothTiers unions the issues and wisps tiers in a single logical query.
// Each tier is queried with its own TierMode; results are deduped by ID and
// re-sorted under the caller-supplied Sort.
//
// Partial failure: if exactly one tier errors, the other tier's rows are
// returned along with a non-nil error so callers can decide whether to
// degrade or fail. When survivor rows exist, the error is a PartialResultError
// so controller demand paths can retain those rows while still reporting the
// incomplete read. Silently swallowing the failure would let dispatch paths see
// "no in-flight work" and double-fire.
func (s *BdStore) listBothTiers(query ListQuery) ([]Bead, error) {
	issuesQ := query
	issuesQ.TierMode = TierIssues
	issuesResult, issuesErr := s.List(issuesQ)

	wispsQ := query
	wispsQ.TierMode = TierWisps
	wispsResult, wispsErr := s.List(wispsQ)

	if issuesErr != nil && wispsErr != nil {
		return nil, errors.Join(issuesErr, wispsErr)
	}

	merged := make([]Bead, 0, len(issuesResult)+len(wispsResult))
	seen := make(map[string]struct{}, len(issuesResult)+len(wispsResult))
	for _, b := range issuesResult {
		if _, ok := seen[b.ID]; ok {
			continue
		}
		seen[b.ID] = struct{}{}
		merged = append(merged, b)
	}
	for _, b := range wispsResult {
		if _, ok := seen[b.ID]; ok {
			continue
		}
		seen[b.ID] = struct{}{}
		merged = append(merged, b)
	}
	sortBeadsForQuery(merged, query.Sort)
	if query.Limit > 0 && len(merged) > query.Limit {
		merged = merged[:query.Limit]
	}

	// Surface single-tier failure so callers don't mistake a partial
	// result for a complete one.
	switch {
	case issuesErr != nil:
		if len(merged) > 0 {
			return merged, &PartialResultError{Op: "bd list both tiers", Err: fmt.Errorf("issues tier: %w", issuesErr)}
		}
		return merged, fmt.Errorf("bd list both tiers: issues tier: %w", issuesErr)
	case wispsErr != nil:
		if len(merged) > 0 {
			return merged, &PartialResultError{Op: "bd list both tiers", Err: fmt.Errorf("wisps tier: %w", wispsErr)}
		}
		return merged, fmt.Errorf("bd list both tiers: wisps tier: %w", wispsErr)
	}
	return merged, nil
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
	out, err := s.runner(s.dir, "bd", args...)
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
	out, err := s.runner(s.dir, "bd", args...)
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
	out, err := s.runner(s.dir, "bd", args...)
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
