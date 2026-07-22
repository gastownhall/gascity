package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Store implements [beads.Store] by delegating each operation to a
// user-supplied script via fork/exec. The script receives the operation
// name as its first argument and communicates via stdin/stdout JSON.
//
// Exit codes: 0 = success, 1 = error (stderr has message), 2 = unknown
// operation (treated as success for forward compatibility).
type Store struct {
	script  string
	timeout time.Duration
	env     map[string]string
}

// SetEnv sets environment variables passed to the script process.
func (s *Store) SetEnv(env map[string]string) {
	s.env = env
}

// CacheMutationScope returns the configured durable store scope so decorating
// caches opened during a controller reload share mutation/event ordering.
func (s *Store) CacheMutationScope() string {
	if s == nil {
		return ""
	}
	return s.env["GC_STORE_ROOT"]
}

// NewStore returns a Store that delegates to the given script.
// The script path may be absolute, relative, or a bare name resolved via
// exec.LookPath.
func NewStore(script string) *Store {
	return &Store{
		script:  script,
		timeout: 30 * time.Second,
	}
}

func execProcessEnv(base, explicit map[string]string) []string {
	out := make([]string, 0, len(os.Environ())+len(base)+len(explicit))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || stripExecEnvKey(key) {
			continue
		}
		out = append(out, entry)
	}
	overrides := make(map[string]string, len(base)+len(explicit))
	for key, value := range base {
		if !isLifecycleMutationEnvKey(key) {
			overrides[key] = value
		}
	}
	for key, value := range explicit {
		overrides[key] = value
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+overrides[key])
	}
	return out
}

func stripExecEnvKey(key string) bool {
	switch key {
	case "GC_BEADS_PREFIX", "GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT", "GC_CITY_RUNTIME_DIR", "GC_LIFECYCLE_MUTATION_SCOPE", "GC_LIFECYCLE_MUTATION_TOKEN", "GC_PROVIDER", "GC_RIG", "GC_RIG_ROOT", "GC_STORE_ROOT", "GC_STORE_SCOPE":
		return true
	}
	return strings.HasPrefix(key, "BEADS_") || strings.HasPrefix(key, "GC_DOLT_")
}

func isLifecycleMutationEnvKey(key string) bool {
	return key == "GC_LIFECYCLE_MUTATION_SCOPE" || key == "GC_LIFECYCLE_MUTATION_TOKEN"
}

// run executes the script with the given args, optionally piping stdinData
// to its stdin. Returns the trimmed stdout on success.
//
// Exit code 2 is treated as success for unknown operation names, matching the
// documented exec-provider forward-compatibility contract. The one exception is
// a `ready` call carrying contract flags (such as --include-ephemeral): there,
// exit 2 fails closed so runnable ephemeral beads are never quietly hidden from
// the orchestrator.
func (s *Store) run(stdinData []byte, args ...string) (string, error) {
	return s.runWithCommandEnv(stdinData, nil, args...)
}

func (s *Store) runWithCommandEnv(stdinData []byte, commandEnv map[string]string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.script, args...)
	// WaitDelay ensures Go forcibly closes I/O pipes after the context
	// expires, even if grandchild processes still hold them open.
	cmd.WaitDelay = 2 * time.Second

	cmd.Env = execProcessEnv(s.env, commandEnv)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if stdinData != nil {
		cmd.Stdin = bytes.NewReader(stdinData)
	}

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 2 {
				if exit2IsRejectedInvocation(args) {
					errMsg := strings.TrimSpace(stderr.String())
					if errMsg == "" {
						errMsg = err.Error()
					}
					return "", fmt.Errorf("exec beads %s %s: %s", s.script, strings.Join(args, " "), errMsg)
				}
				return "", nil
			}
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("exec beads %s %s: %s", s.script, strings.Join(args, " "), errMsg)
	}

	return strings.TrimRight(stdout.String(), "\n"), nil
}

// exit2IsRejectedInvocation reports whether an exit-2 response must fail closed
// rather than degrade to the documented "unknown operation → no-op success".
// Only `ready` invoked with contract flags qualifies: silently hiding runnable
// ephemeral beads from the orchestrator is worse than a hard error. `dep-list`
// is deliberately excluded — the exec-provider contract lets a script omit it
// and expects an empty dependency list, which the composed callers (convoy
// membership, drain, event hydration, and the lifecycle close path) treat as
// the complete answer.
func exit2IsRejectedInvocation(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return args[0] == "ready" && len(args) > 1
}

// isNotFoundError reports whether an error from the script indicates a
// bead was not found. Scripts signal this by exiting with code 1 and
// including "not found" or "no issue found" in stderr.
func isNotFoundError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no issue found")
}

// parseBead parses a single bead from JSON output.
func parseBead(data string) (beads.Bead, error) {
	var w beadWire
	if err := json.Unmarshal([]byte(data), &w); err != nil {
		return beads.Bead{}, fmt.Errorf("parsing JSON: %w", err)
	}
	return w.toBead(), nil
}

// parseBeadList parses a JSON array of beads. Returns empty slice for
// empty input (not nil).
func parseBeadList(data string) ([]beads.Bead, error) {
	if data == "" {
		return []beads.Bead{}, nil
	}
	var ws []beadWire
	if err := json.Unmarshal([]byte(data), &ws); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}
	result := make([]beads.Bead, len(ws))
	for i := range ws {
		result[i] = ws[i].toBead()
	}
	return result, nil
}

// toBead converts the wire format to a Gas City Bead.
func (w *beadWire) toBead() beads.Bead {
	var priority *int
	if w.Priority != nil {
		cloned := *w.Priority
		priority = &cloned
	}
	status := w.Status
	if strings.TrimSpace(status) == "" {
		status = "open"
	}
	return beads.Bead{
		ID:           w.ID,
		Title:        w.Title,
		Status:       status,
		Type:         w.Type,
		Priority:     priority,
		CreatedAt:    w.CreatedAt,
		UpdatedAt:    w.UpdatedAt,
		Revision:     w.Revision,
		Assignee:     w.Assignee,
		From:         w.From,
		ParentID:     w.ParentID,
		Ref:          w.Ref,
		Needs:        w.Needs,
		Description:  w.Description,
		Labels:       w.Labels,
		Metadata:     coerceMetadata(w.Metadata),
		Dependencies: append([]beads.Dep(nil), w.Dependencies...),
		Ephemeral:    w.Ephemeral,
		NoHistory:    w.NoHistory,
		DeferUntil:   cloneTimePtr(w.DeferUntil),
		IsBlocked:    cloneBoolPtr(w.IsBlocked),
	}
}

func cloneTimePtr(v *time.Time) *time.Time {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

func cloneBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

// coerceMetadata converts raw JSON metadata values to strings. Backing stores
// may return numbers or booleans; the domain model is map[string]string.
func coerceMetadata(raw map[string]json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	m := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) == nil {
			m[k] = s
		} else {
			// Number, boolean, or other non-string — use the raw JSON text.
			m[k] = strings.TrimSpace(string(v))
		}
	}
	return m
}

// Create persists a new bead: script create (stdin: JSON)
func (s *Store) Create(b beads.Bead) (beads.Bead, error) {
	if b.Type == "" {
		b.Type = "task"
	}
	data, err := marshalCreate(b)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("exec beads create: marshaling: %w", err)
	}
	out, err := s.run(data, "create")
	if err != nil {
		return beads.Bead{}, fmt.Errorf("exec beads create: %w", err)
	}
	result, err := parseBead(out)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("exec beads create: %w", err)
	}
	return result, nil
}

// Get retrieves a bead by ID: script get <id>
func (s *Store) Get(id string) (beads.Bead, error) {
	out, err := s.run(nil, "get", id)
	if err != nil {
		if isNotFoundError(err) {
			return beads.Bead{}, fmt.Errorf("getting bead %q: %w", id, beads.ErrNotFound)
		}
		return beads.Bead{}, fmt.Errorf("getting bead %q: %w", id, err)
	}
	result, err := parseBead(out)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("exec beads get: %w", err)
	}
	return result, nil
}

// Update modifies fields of an existing bead: script update <id> (stdin: JSON)
func (s *Store) Update(id string, opts beads.UpdateOpts) error {
	data, err := marshalUpdate(opts)
	if err != nil {
		return fmt.Errorf("exec beads update: marshaling: %w", err)
	}
	_, err = s.run(data, "update", id)
	if err != nil {
		if isNotFoundError(err) {
			return fmt.Errorf("updating bead %q: %w", id, beads.ErrNotFound)
		}
		return fmt.Errorf("updating bead %q: %w", id, err)
	}
	return nil
}

// Close sets a bead's status to "closed": script close <id>.
//
// This delegate stays bare (no --force). The exec: wrapper scripts
// (contrib/beads-scripts/gc-beads-k8s, gc-beads-br) are the canonical place
// that injects --force for bd's cross-actor close guard
// (gastownhall/beads#3734): they run under the pod/controller actor with
// BEADS_ACTOR stripped while closing agent-owned beads, mirroring the SDK
// BdStore, which always force-closes. Do not add --force here — the wrappers
// read the bead id as their first positional argument, so a leading --force
// would break them. A new exec: wrapper that closes agent-owned beads must
// inject --force itself.
func (s *Store) Close(id string) error {
	return s.closeWithCommandEnv(id, nil)
}

func (s *Store) closeWithCommandEnv(id string, commandEnv map[string]string) error {
	_, err := s.runWithCommandEnv(nil, commandEnv, "close", id)
	if err != nil {
		if isNotFoundError(err) {
			return fmt.Errorf("closing bead %q: %w", id, beads.ErrNotFound)
		}
		return fmt.Errorf("closing bead %q: %w", id, err)
	}
	return nil
}

// Reopen sets a bead's status to "open": script reopen <id>
func (s *Store) Reopen(id string) error {
	_, err := s.run(nil, "reopen", id)
	if err != nil {
		if isNotFoundError(err) {
			return fmt.Errorf("reopening bead %q: %w", id, beads.ErrNotFound)
		}
		return fmt.Errorf("reopening bead %q: %w", id, err)
	}
	return nil
}

// CloseAll closes multiple beads and sets metadata on each.
func (s *Store) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		for k, v := range metadata {
			_ = s.SetMetadata(id, k, v)
		}
		if err := s.Close(id); err == nil {
			closed++
		}
	}
	return closed, nil
}

// List returns beads matching the query.
func (s *Store) List(query beads.ListQuery) ([]beads.Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("exec beads list: %w", beads.ErrQueryRequiresScan)
	}

	var (
		out string
		err error
	)
	switch {
	case query.ParentID != "":
		out, err = s.run(nil, "children", query.ParentID)
	case query.Label != "":
		out, err = s.run(nil, "list-by-label", query.Label, "0")
	default:
		args := []string{"list"}
		if query.Status != "" {
			args = append(args, "--status="+query.Status)
		}
		if query.Assignee != "" {
			args = append(args, "--assignee="+query.Assignee)
		}
		if query.Type != "" {
			args = append(args, "--type="+query.Type)
		}
		// SeekAfter (like CreatedBefore) is applied Go-side after the script
		// returns, so a script-side limit would cut rows before the boundary
		// filter runs and silently skip page rows.
		if query.Limit > 0 && query.CreatedBefore.IsZero() && query.SeekAfter == nil {
			args = append(args, "--limit="+strconv.Itoa(query.Limit))
		}
		out, err = s.run(nil, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("exec beads list: %w", err)
	}
	list, err := parseBeadList(out)
	if err != nil {
		return nil, err
	}
	return beads.ApplyListQuery(list, query), nil
}

// ListOpen returns non-closed beads by default. The exec protocol's `list`
// command may return all beads, so the store enforces the status filter
// client-side.
func (s *Store) ListOpen(status ...string) ([]beads.Bead, error) {
	query := beads.ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
	}
	return s.List(query)
}

// Ready returns actionable open beads (excluding infrastructure types):
// script ready [--include-ephemeral]
func (s *Store) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	q := beads.ReadyQuery{}
	if len(query) > 0 {
		q = query[0]
	}
	args := []string{"ready"}
	if q.TierMode == beads.TierBoth || q.TierMode == beads.TierWisps {
		args = append(args, "--include-ephemeral")
	}
	out, err := s.run(nil, args...)
	if err != nil {
		return nil, fmt.Errorf("exec beads ready: %w", err)
	}
	all, err := parseBeadList(out)
	if err != nil {
		return nil, err
	}
	result := all[:0]
	now := time.Now().UTC()
	for _, b := range all {
		if beads.IsReadyCandidateForTier(b, now, q.TierMode) {
			result = append(result, b)
		}
	}
	return beads.ApplyListQuery(result, beads.ListQuery{Assignee: q.Assignee, Limit: q.Limit, TierMode: q.TierMode}), nil
}

// Children returns non-closed beads whose ParentID matches by default:
// script children <parent-id>
func (s *Store) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.List(beads.ListQuery{
		ParentID:      parentID,
		IncludeClosed: beads.HasOpt(opts, beads.IncludeClosed),
		Sort:          beads.SortCreatedAsc,
	})
}

// ListByLabel returns non-closed beads matching a label by default:
// script list-by-label <label> <limit>
func (s *Store) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.List(beads.ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: beads.HasOpt(opts, beads.IncludeClosed),
		Sort:          beads.SortCreatedDesc,
		TierMode:      beads.TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to the given agent with the specified
// status.
func (s *Store) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	return s.List(beads.ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
		Sort:     beads.SortCreatedDesc,
	})
}

// ListByMetadata returns beads whose metadata contains all key-value pairs in
// filters.
func (s *Store) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.List(beads.ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: beads.HasOpt(opts, beads.IncludeClosed),
		Sort:          beads.SortCreatedDesc,
		TierMode:      beads.TierModeFromOpts(opts),
	})
}

// SetMetadata sets a key-value metadata pair: script set-metadata <id> <key> (stdin: value)
func (s *Store) SetMetadata(id, key, value string) error {
	return s.setMetadataWithCommandEnv(id, key, value, nil)
}

func (s *Store) setMetadataWithCommandEnv(id, key, value string, commandEnv map[string]string) error {
	_, err := s.runWithCommandEnv([]byte(value), commandEnv, "set-metadata", id, key)
	if err != nil {
		return fmt.Errorf("setting metadata on %q: %w", id, err)
	}
	return nil
}

// SetMetadataBatch sets multiple key-value metadata pairs on a bead.
// Delegates to sequential SetMetadata calls.
func (s *Store) SetMetadataBatch(id string, kvs map[string]string) error {
	return s.setMetadataBatchWithCommandEnv(id, kvs, nil)
}

func (s *Store) setMetadataBatchWithCommandEnv(id string, kvs map[string]string, commandEnv map[string]string) error {
	for k, v := range kvs {
		if err := s.setMetadataWithCommandEnv(id, k, v, commandEnv); err != nil {
			return err
		}
	}
	return nil
}

// WithLifecycleMetadataTransaction holds the durable GC_STORE_ROOT lifecycle
// lease while the callback performs live reads and bridge mutations. Mutation
// mutation children receive a transaction-local lease delegation; reads and
// ordinary exec children never inherit lifecycle ownership from the process.
func (s *Store) WithLifecycleMetadataTransaction(id string, fn func(beads.LifecycleMetadataTransaction) error) error {
	if s == nil {
		return errors.New("lifecycle metadata transaction: nil exec Store")
	}
	if fn == nil {
		return errors.New("lifecycle metadata transaction: nil callback")
	}
	scope := strings.TrimSpace(s.CacheMutationScope())
	if scope == "" {
		return errors.New("lifecycle metadata transaction: exec Store has no GC_STORE_ROOT")
	}
	return beads.WithLifecycleMutationLease(scope, func(commandEnv map[string]string) error {
		return fn(execLifecycleMetadataTransaction{
			id:         id,
			store:      s,
			commandEnv: commandEnv,
		})
	})
}

type execLifecycleMetadataTransaction struct {
	id         string
	store      *Store
	commandEnv map[string]string
}

func (tx execLifecycleMetadataTransaction) Get() (beads.Bead, error) {
	return tx.GetByID(tx.id)
}

func (tx execLifecycleMetadataTransaction) GetByID(id string) (beads.Bead, error) {
	bead, err := tx.store.Get(id)
	if err != nil {
		return beads.Bead{}, err
	}
	// A script that does not implement dep-list exits 2, which DepList degrades
	// to an empty dependency list per the exec-provider contract. That empty set
	// is the complete answer for a backend with no dependency concept, so the
	// lifecycle payload stays complete rather than failing the close.
	dependencies, err := tx.store.DepList(id, "down")
	if err != nil {
		return beads.Bead{}, fmt.Errorf("loading lifecycle dependencies for %q: %w", id, err)
	}
	bead.Dependencies = dependencies
	return bead, nil
}

func (tx execLifecycleMetadataTransaction) List(_ beads.ListQuery) ([]beads.Bead, error) {
	// The exec `list` wire forwards only --status/--assignee/--type/--limit to the
	// script; Live, TierMode, IncludeClosed, and Metadata are applied client-side
	// by ApplyListQuery, which can filter a returned superset but cannot recover
	// rows the script never emitted. Scripts (including the bundled bd bridge)
	// default `list` to open, issues-tier beads, so a lifecycle read carrying
	// IncludeClosed/TierBoth/Metadata would silently omit closed beads and
	// ephemeral wisps. Fail loud so lifecycle-read consumers take their
	// conservative fallback instead of proving from an incomplete subtree.
	return nil, fmt.Errorf("exec lifecycle read: %w", beads.ErrLifecycleReadUnsupported)
}

func (tx execLifecycleMetadataTransaction) SetMetadata(key, value string) error {
	return tx.store.setMetadataWithCommandEnv(tx.id, key, value, tx.commandEnv)
}

func (tx execLifecycleMetadataTransaction) SetMetadataBatch(values map[string]string) error {
	return tx.store.setMetadataBatchWithCommandEnv(tx.id, values, tx.commandEnv)
}

func (tx execLifecycleMetadataTransaction) CloseWithReasonWithoutObserver(reason string) (beads.LifecycleCloseResult, error) {
	before, err := tx.Get()
	if err != nil {
		return beads.LifecycleCloseResult{}, err
	}
	result := beads.LifecycleCloseResult{Before: before}
	if before.Status == "closed" {
		result.After = before
		return result, nil
	}

	trimmedReason := strings.TrimSpace(reason)
	if trimmedReason != "" && strings.TrimSpace(before.Metadata["close_reason"]) != trimmedReason {
		if err := tx.SetMetadata("close_reason", trimmedReason); err != nil {
			return result, err
		}
	}
	closeErr := tx.store.closeWithCommandEnv(tx.id, tx.commandEnv)
	result.CloseSucceeded = closeErr == nil
	after, readErr := tx.Get()
	if readErr == nil {
		result.After = after
		result.Transitioned = before.Status != "closed" && after.Status == "closed"
	}
	return result, closeErr
}

// Tx executes fn sequentially against the exec store.
func (s *Store) Tx(_ string, fn func(beads.Tx) error) error {
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	return fn(s)
}

// Delete permanently removes a bead by calling the "delete" subcommand.
func (s *Store) Delete(id string) error {
	_, err := s.run(nil, "delete", "--force", id)
	return err
}

// Ping verifies the store script is accessible by running a list operation.
func (s *Store) Ping() error {
	_, err := s.run(nil, "list")
	if err != nil {
		return fmt.Errorf("exec store ping: %w", err)
	}
	return nil
}

// DepAdd delegates dependency creation to the script's dep-add operation.
func (s *Store) DepAdd(issueID, dependsOnID, depType string) error {
	_, err := s.run(nil, "dep-add", issueID, dependsOnID, depType)
	if err != nil {
		return fmt.Errorf("adding dep %s→%s: %w", issueID, dependsOnID, err)
	}
	return nil
}

// DepRemove delegates dependency removal to the script's dep-remove operation.
func (s *Store) DepRemove(issueID, dependsOnID string) error {
	_, err := s.run(nil, "dep-remove", issueID, dependsOnID)
	if err != nil {
		return fmt.Errorf("removing dep %s→%s: %w", issueID, dependsOnID, err)
	}
	return nil
}

// DepList delegates dependency listing to the script's dep-list operation.
func (s *Store) DepList(id, direction string) ([]beads.Dep, error) {
	out, err := s.run(nil, "dep-list", id, direction)
	if err != nil {
		return nil, fmt.Errorf("listing deps for %q: %w", id, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var deps []beads.Dep
	if err := json.Unmarshal([]byte(out), &deps); err != nil {
		return nil, fmt.Errorf("exec beads dep-list: parsing JSON: %w", err)
	}
	return deps, nil
}

// Compile-time interface check.
var (
	_ beads.Store                             = (*Store)(nil)
	_ beads.LifecycleMetadataTransactionStore = (*Store)(nil)
	_ beads.LifecycleCloseTransaction         = execLifecycleMetadataTransaction{}
	_ beads.LifecycleReadTransaction          = execLifecycleMetadataTransaction{}
)
