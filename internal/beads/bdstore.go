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

// CommandRunner executes a command in the given directory and returns stdout bytes.
// The dir argument sets the working directory; name and args specify the command.
type CommandRunner func(dir, name string, args ...string) ([]byte, error)

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
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.WaitDelay = 2 * time.Second
		cmd.Dir = dir
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
			timeoutErr := fmt.Errorf("timed out after 120s")
			trace("timeout", timeoutErr)
			if stderr.Len() > 0 {
				return out, fmt.Errorf("%w: %s", timeoutErr, stderr.String())
			}
			return out, timeoutErr
		}
		if err != nil && stderr.Len() > 0 {
			trace("error", err)
			return out, fmt.Errorf("%w: %s", err, stderr.String())
		}
		trace("done", err)
		return out, err
	}
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
}

// NewBdStore creates a BdStore rooted at dir using the given runner.
func NewBdStore(dir string, runner CommandRunner) *BdStore {
	return &BdStore{dir: dir, runner: runner}
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
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	IssueType    string    `json:"issue_type"`
	Priority     *int      `json:"priority,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Assignee     string    `json:"assignee"`
	From         string    `json:"from"`
	ParentID     string    `json:"parent"`
	Ref          string    `json:"ref"`
	Needs        []string  `json:"needs"`
	Description  string    `json:"description"`
	Labels       []string  `json:"labels"`
	Metadata     StringMap `json:"metadata,omitempty"`
	Dependencies []Dep     `json:"dependencies,omitempty"`
}

// parseIssuesTolerant unmarshals a JSON array of bdIssue objects, skipping
// any entries that fail to parse (e.g. corrupt metadata with non-string values).
// This prevents a single bad bead from breaking all list operations.
func parseIssuesTolerant(data []byte) []bdIssue {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	result := make([]bdIssue, 0, len(raw))
	for _, r := range raw {
		var issue bdIssue
		if err := json.Unmarshal(r, &issue); err != nil {
			// Skip corrupt entry — log the ID if we can extract it.
			var peek struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(r, &peek)
			fmt.Fprintf(os.Stderr, "beads: skipping corrupt bead %q: %v\n", peek.ID, err)
			continue
		}
		result = append(result, issue)
	}
	return result
}

// toBead converts a bdIssue to a Gas City Bead. CreatedAt is truncated to
// second precision because dolt stores timestamps at second granularity —
// bd create may return sub-second precision that bd show then truncates.
func (b *bdIssue) toBead() Bead {
	from := b.From
	if from == "" && b.Metadata != nil {
		from = b.Metadata["from"]
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
		ParentID:     b.ParentID,
		Ref:          b.Ref,
		Needs:        b.Needs,
		Description:  b.Description,
		Labels:       b.Labels,
		Metadata:     b.Metadata,
		Dependencies: append([]Dep(nil), b.Dependencies...),
	}
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
	for _, l := range b.Labels {
		args = append(args, "--labels", l)
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
	if created.Metadata == nil && len(metadata) > 0 {
		created.Metadata = metadata
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

// SetMetadata sets a key-value metadata pair on a bead via bd update.
func (s *BdStore) SetMetadata(id, key, value string) error {
	_, err := s.runner(s.dir, "bd", "update", "--json", id,
		"--set-metadata", key+"="+value)
	if err != nil {
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
	_, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return fmt.Errorf("setting metadata on %q: %w", id, err)
	}
	return nil
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
func (s *BdStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	// Set metadata on all beads first (before closing, since some stores
	// prevent metadata writes on closed beads).
	for _, id := range ids {
		if len(metadata) > 0 {
			_ = s.SetMetadataBatch(id, metadata)
		}
	}

	// Batch close: bd close id1 id2 id3 ...
	args := append([]string{"close", "--json"}, ids...)
	_, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		// Fall back to individual closes on batch failure.
		closed := 0
		for _, id := range ids {
			if closeErr := s.Close(id); closeErr == nil {
				closed++
			}
		}
		return closed, nil
	}
	return len(ids), nil
}

// Close sets a bead's status to closed via bd close.
// Idempotent: closing an already-closed bead returns nil.
func (s *BdStore) Close(id string) error {
	_, err := s.runner(s.dir, "bd", "close", "--json", id)
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

// ListOpen returns non-closed beads via bd list. Pass a status to filter further.
func (s *BdStore) ListOpen(status ...string) ([]Bead, error) {
	args := []string{"list", "--json", "--limit", "0", "--include-infra"}
	if len(status) > 0 && status[0] != "" {
		args = append(args, "--status="+status[0])
	}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return nil, fmt.Errorf("bd list: %w", err)
	}
	issues := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, len(issues))
	for i := range issues {
		result[i] = issues[i].toBead()
	}
	return result, nil
}

// ListByLabel returns beads matching an exact label via bd list --label.
// Limit controls max results (0 = unlimited). Results are ordered by bd's
// default sort (newest first).
func (s *BdStore) ListByLabel(label string, limit int, _ ...QueryOpt) ([]Bead, error) {
	args := []string{"list", "--json", "--label=" + label, "--all", "--include-infra", "--limit", fmt.Sprintf("%d", limit)}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return nil, fmt.Errorf("bd list: %w", err)
	}
	issues := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, len(issues))
	for i := range issues {
		result[i] = issues[i].toBead()
	}
	return result, nil
}

// ListByAssignee returns beads assigned to the given agent with the specified
// status via bd list --assignee --status. Limit controls max results (0 = unlimited).
func (s *BdStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	args := []string{"list", "--json", "--assignee=" + assignee, "--status=" + status, "--include-infra", "--limit", fmt.Sprintf("%d", limit)}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return nil, fmt.Errorf("bd list: %w", err)
	}
	issues := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, len(issues))
	for i := range issues {
		result[i] = issues[i].toBead()
	}
	return result, nil
}

// ListByMetadata returns beads matching all given metadata key=value filters.
// Limit controls max results (0 = unlimited). Results use bd's default order.
func (s *BdStore) ListByMetadata(filters map[string]string, limit int) ([]Bead, error) {
	args := []string{"list", "--json", "--all", "--include-infra", "--limit", fmt.Sprintf("%d", limit)}
	if len(filters) > 0 {
		keys := make([]string, 0, len(filters))
		for k := range filters {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--metadata-field", k+"="+filters[k])
		}
	}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return nil, fmt.Errorf("bd list: %w", err)
	}
	issues := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, len(issues))
	for i := range issues {
		result[i] = issues[i].toBead()
	}
	return result, nil
}

// Children returns all beads whose ParentID matches the given ID, including
// closed beads. Uses bd list --all --parent for a targeted server-side query.
func (s *BdStore) Children(parentID string, _ ...QueryOpt) ([]Bead, error) {
	args := []string{"list", "--json", "--all", "--include-infra", "--limit", "0", "--parent", parentID}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return nil, fmt.Errorf("bd list children: %w", err)
	}
	issues := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, len(issues))
	for i := range issues {
		result[i] = issues[i].toBead()
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

// Ready returns all open beads via bd ready.
func (s *BdStore) Ready() ([]Bead, error) {
	out, err := s.runner(s.dir, "bd", "ready", "--json", "--limit", "0")
	if err != nil {
		return nil, fmt.Errorf("bd ready: %w", err)
	}
	issues := parseIssuesTolerant(extractJSON(out))
	result := make([]Bead, len(issues))
	for i := range issues {
		result[i] = issues[i].toBead()
	}
	return result, nil
}

// DepAdd records a dependency via bd dep add.
func (s *BdStore) DepAdd(issueID, dependsOnID, depType string) error {
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
