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
)

// CommandRunner executes a command in the given directory and returns stdout bytes.
// The dir argument sets the working directory; name and args specify the command.
type CommandRunner func(dir, name string, args ...string) ([]byte, error)

var bdCommandTimeout = 120 * time.Second

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
		ctx, cancel := context.WithTimeout(context.Background(), bdCommandTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.WaitDelay = 2 * time.Second
		prepareCommandForTimeout(cmd)
		cmd.Dir = dir
		cmd.Cancel = func() error {
			return killCommandTree(cmd)
		}
		if len(env) > 0 {
			cmd.Env = mergeEnv(os.Environ(), env)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if name == "bd" {
			telemetry.RecordBDCall(context.Background(),
				args, float64(time.Since(start).Milliseconds()),
				err, out, stderr.String())
		}
		if ctx.Err() == context.DeadlineExceeded {
			timeoutErr := fmt.Errorf("timed out after %s", bdCommandTimeout)
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
	env := envWithout(os.Environ(), "BEADS_DIR")
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bd", args...)
	cmd.Dir = dir
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
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
	Assignee     string       `json:"assignee"`
	From         string       `json:"from"`
	ParentID     string       `json:"parent"`
	Ref          string       `json:"ref"`
	Needs        []string     `json:"needs"`
	Description  string       `json:"description"`
	Labels       []string     `json:"labels"`
	Metadata     StringMap    `json:"metadata,omitempty"`
	Dependencies []bdIssueDep `json:"dependencies,omitempty"`
}

type bdIssueDep struct {
	IssueID        string `json:"issue_id"`
	DependsOnID    string `json:"depends_on_id"`
	Type           string `json:"type"`
	ID             string `json:"id"`
	DependencyType string `json:"dependency_type"`
}

// PartialResultError indicates that a list-style bd command returned at least
// one usable entry but also included entries that failed to parse. The
// successful entries are still returned alongside this error; callers that can
// surface partial data may proceed with those rows, while callers that require
// a complete picture should treat this as a hard failure.
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
		Assignee:     b.Assignee,
		From:         from,
		ParentID:     parentID,
		Ref:          b.Ref,
		Needs:        b.Needs,
		Description:  b.Description,
		Labels:       b.Labels,
		Metadata:     b.Metadata,
		Dependencies: deps,
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
	out, err := s.runner(s.dir, "bd", args...)
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
	_, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		if isBdNotFound(err) {
			return fmt.Errorf("updating bead %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("updating bead %q: %w", id, err)
	}
	return nil
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

func (s *BdStore) runBDTransientWrite(args ...string) error {
	var err error
	for attempt := 1; attempt <= bdTransientWriteAttempts; attempt++ {
		_, err = s.runner(s.dir, "bd", args...)
		if err == nil || !isBdTransientWriteConflict(err) || attempt == bdTransientWriteAttempts {
			return err
		}
		time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
	}
	return err
}

func isBdTransientWriteConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1213 (40001): serialization failure") ||
		strings.Contains(msg, "this transaction conflicts with a committed transaction")
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
	_, err := s.runner(s.dir, "bd", args...)
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
	_, err := s.runner(s.dir, "bd", bdCloseArgs(reason, id)...)
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
	return nil
}

// Reopen sets a closed bead's status to open via bd reopen.
func (s *BdStore) Reopen(id string) error {
	_, err := s.runner(s.dir, "bd", "reopen", "--json", id)
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
	_, err := s.runner(s.dir, "bd", "delete", "--force", "--json", id)
	if err != nil {
		if isBdNotFound(err) {
			return fmt.Errorf("deleting bead %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("deleting bead %q: %w", id, err)
	}
	return nil
}

// List returns beads matching the query via bd list.
func (s *BdStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("bd list: %w", ErrQueryRequiresScan)
	}

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

// Ready returns open ready beads via bd ready.
func (s *BdStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(query)
	args := []string{"ready", "--json"}
	if q.Assignee != "" {
		args = append(args, "--assignee", q.Assignee)
	}
	if q.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(q.Limit))
	} else {
		args = append(args, "--limit", "0")
	}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return nil, fmt.Errorf("bd ready: %w", err)
	}
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, 0, len(issues))
	for i := range issues {
		bead := issues[i].toBead()
		if IsReadyExcludedType(bead.Type) {
			continue
		}
		if q.Assignee != "" && bead.Assignee != q.Assignee {
			continue
		}
		result = append(result, bead)
	}
	if parseErr != nil {
		if len(result) == 0 {
			return nil, fmt.Errorf("bd ready: %w", parseErr)
		}
		return result, &PartialResultError{Op: "bd ready", Err: parseErr}
	}
	return result, nil
}

// DepAdd records a dependency via bd dep add.
func (s *BdStore) DepAdd(issueID, dependsOnID, depType string) error {
	if depType == "parent-child" {
		bead, err := s.Get(issueID)
		if err == nil && bead.ParentID == dependsOnID {
			return nil
		}
	}
	_, err := s.runner(s.dir, "bd", "dep", "add", issueID, dependsOnID, "--type", depType)
	if err != nil {
		return fmt.Errorf("adding dep %s→%s: %w", issueID, dependsOnID, err)
	}
	return nil
}

// DepRemove removes a dependency via bd dep remove.
func (s *BdStore) DepRemove(issueID, dependsOnID string) error {
	_, err := s.runner(s.dir, "bd", "dep", "remove", issueID, dependsOnID)
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
