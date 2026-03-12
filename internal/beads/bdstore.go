package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	return func(dir, name string, args ...string) ([]byte, error) {
		start := time.Now()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if name == "bd" {
			telemetry.RecordBDCall(context.Background(),
				args, float64(time.Since(start).Milliseconds()),
				err, out, stderr.String())
		}
		if err != nil && stderr.Len() > 0 {
			return out, fmt.Errorf("%w: %s", err, stderr.String())
		}
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

// MolCook instantiates an ephemeral molecule (wisp) from a formula and returns
// the root bead ID. Uses "bd mol wisp" to create the molecule.
func (s *BdStore) MolCook(formula, _ string, vars []string) (string, error) {
	args := []string{"mol", "wisp", formula, "--json"}
	for _, v := range vars {
		args = append(args, "--var", v)
	}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return "", fmt.Errorf("bd mol wisp: %w", err)
	}
	rootID, parseErr := parseWispJSON(out)
	if parseErr != nil {
		return "", fmt.Errorf("bd mol wisp: %w", parseErr)
	}
	return rootID, nil
}

// MolCookOn instantiates an ephemeral molecule from a formula attached to an
// existing bead, and returns the wisp root bead ID. Uses "bd mol bond" to
// cook the formula and attach it to the bead in one step.
func (s *BdStore) MolCookOn(formula, beadID, _ string, vars []string) (string, error) {
	args := []string{"mol", "bond", formula, beadID, "--json"}
	for _, v := range vars {
		args = append(args, "--var", v)
	}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return "", fmt.Errorf("bd mol bond: %w", err)
	}
	rootID, parseErr := parseWispJSON(out)
	if parseErr != nil {
		return "", fmt.Errorf("bd mol bond: %w", parseErr)
	}
	return rootID, nil
}

// wispResult is the JSON structure returned by bd mol wisp and bd mol bond.
type wispResult struct {
	NewEpicID string `json:"new_epic_id"`
	RootID    string `json:"root_id"`
	ResultID  string `json:"result_id"`
}

// parseWispJSON extracts the root bead ID from bd mol wisp/bond JSON output.
func parseWispJSON(data []byte) (string, error) {
	jsonBytes := extractJSON(data)
	var result wispResult
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return "", fmt.Errorf("parsing JSON: %w (output: %s)", err, strings.TrimSpace(string(data)))
	}
	switch {
	case result.NewEpicID != "":
		return result.NewEpicID, nil
	case result.RootID != "":
		return result.RootID, nil
	case result.ResultID != "":
		return result.ResultID, nil
	default:
		return "", fmt.Errorf("no ID in output: %s", strings.TrimSpace(string(data)))
	}
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
	args := []string{"--allow-stale", "purge", "--json"}
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
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Status      string    `json:"status"`
	IssueType   string    `json:"issue_type"`
	CreatedAt   time.Time `json:"created_at"`
	Assignee    string    `json:"assignee"`
	From        string    `json:"from"`
	ParentID    string    `json:"parent_id"`
	Ref         string    `json:"ref"`
	Needs       []string  `json:"needs"`
	Description string    `json:"description"`
	Labels      []string  `json:"labels"`
	Metadata    StringMap `json:"metadata,omitempty"`
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
	return Bead{
		ID:          b.ID,
		Title:       b.Title,
		Status:      mapBdStatus(b.Status),
		Type:        b.IssueType,
		CreatedAt:   b.CreatedAt.Truncate(time.Second),
		Assignee:    b.Assignee,
		From:        b.From,
		ParentID:    b.ParentID,
		Ref:         b.Ref,
		Needs:       b.Needs,
		Description: b.Description,
		Labels:      b.Labels,
		Metadata:    b.Metadata,
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
	for _, l := range b.Labels {
		args = append(args, "--labels", l)
	}
	if b.ParentID != "" {
		args = append(args, "--parent", b.ParentID)
	}
	if len(b.Metadata) > 0 {
		metaJSON, err := json.Marshal(b.Metadata)
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
	return issue.toBead(), nil
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
	if opts.Description != nil {
		args = append(args, "--description", *opts.Description)
	}
	if opts.ParentID != nil {
		args = append(args, "--parent", *opts.ParentID)
	}
	if opts.Assignee != nil {
		args = append(args, "--assignee", *opts.Assignee)
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
	for k, v := range kvs {
		if err := s.SetMetadata(id, k, v); err != nil {
			return err
		}
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

// Close sets a bead's status to closed via bd close.
// Idempotent: closing an already-closed bead returns nil.
func (s *BdStore) Close(id string) error {
	_, err := s.runner(s.dir, "bd", "close", "--json", id)
	if err != nil {
		if isBdNotFound(err) {
			// Check if it's already closed (idempotent close).
			if b, getErr := s.Get(id); getErr == nil && b.Status == "closed" {
				return nil
			}
			return fmt.Errorf("closing bead %q: %w", id, ErrNotFound)
		}
		return fmt.Errorf("closing bead %q: %w", id, err)
	}
	return nil
}

// List returns all beads via bd list.
func (s *BdStore) List() ([]Bead, error) {
	out, err := s.runner(s.dir, "bd", "list", "--json", "--limit", "0", "--all")
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
func (s *BdStore) ListByLabel(label string, limit int) ([]Bead, error) {
	args := []string{"list", "--json", "--label=" + label, "--all", "--limit", fmt.Sprintf("%d", limit)}
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
	args := []string{"list", "--json", "--assignee=" + assignee, "--status=" + status, "--limit", fmt.Sprintf("%d", limit)}
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

// Children returns all beads whose ParentID matches the given ID. The bd CLI
// does not know about ParentID, so this filters List() results client-side.
// Returns empty for now since Tutorial 06 uses FileStore.
func (s *BdStore) Children(parentID string) ([]Bead, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var result []Bead
	for _, b := range all {
		if b.ParentID == parentID {
			result = append(result, b)
		}
	}
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
