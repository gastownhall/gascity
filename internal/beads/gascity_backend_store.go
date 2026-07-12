package beads

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const GascityBackendProtocolV1Alpha1 = "gascity.backend.v1alpha1"

type GascityBackendStoreConfig struct {
	Command  string
	Args     []string
	Env      map[string]string
	BeadsDir string
	Database string
}

type GascityBackendStore struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	scanner   *bufio.Scanner
	sessionID string
	nextID    uint64
}

type gascityBackendRequest struct {
	ID     string `json:"id,omitempty"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type gascityBackendResponse struct {
	ID     string          `json:"id,omitempty"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func OpenGascityBackendStore(ctx context.Context, cfg GascityBackendStoreConfig) (*GascityBackendStore, error) {
	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		return nil, fmt.Errorf("gascity backend store: command is required")
	}
	args := append([]string(nil), cfg.Args...)
	cmd := exec.CommandContext(ctx, command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stderr = os.Stderr
	cmd.Env = gascityBackendProcessEnv(cfg.Env)
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	store := &GascityBackendStore{cmd: cmd, stdin: stdin, scanner: bufio.NewScanner(stdout)}
	store.scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	if err := store.readHello(); err != nil {
		_ = store.CloseStore()
		return nil, err
	}
	var opened struct {
		SessionID string `json:"session_id"`
	}
	if err := store.call("open", map[string]string{
		"beads_dir": cfg.BeadsDir,
		"database":  cfg.Database,
	}, &opened); err != nil {
		_ = store.CloseStore()
		return nil, err
	}
	store.sessionID = opened.SessionID
	return store, nil
}

func gascityBackendProcessEnv(overrides map[string]string) []string {
	out := append([]string(nil), os.Environ()...)
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}

func (s *GascityBackendStore) readHello() error {
	if !s.scanner.Scan() {
		if err := s.scanner.Err(); err != nil {
			return err
		}
		return fmt.Errorf("gascity backend store: missing hello")
	}
	var resp gascityBackendResponse
	if err := json.Unmarshal(s.scanner.Bytes(), &resp); err != nil {
		return fmt.Errorf("gascity backend hello: %w", err)
	}
	if err := gascityBackendResponseError(resp); err != nil {
		return err
	}
	var hello struct {
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		return fmt.Errorf("gascity backend hello result: %w", err)
	}
	if hello.Protocol != GascityBackendProtocolV1Alpha1 {
		return fmt.Errorf("gascity backend protocol = %q, want %q", hello.Protocol, GascityBackendProtocolV1Alpha1)
	}
	return nil
}

func (s *GascityBackendStore) call(method string, params any, result any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("gc-%d", atomic.AddUint64(&s.nextID, 1))
	req := gascityBackendRequest{ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := s.stdin.Write(append(data, '\n')); err != nil {
		return err
	}
	if !s.scanner.Scan() {
		if err := s.scanner.Err(); err != nil {
			return err
		}
		return fmt.Errorf("gascity backend %s: no response", method)
	}
	var resp gascityBackendResponse
	if err := json.Unmarshal(s.scanner.Bytes(), &resp); err != nil {
		return fmt.Errorf("gascity backend %s response: %w", method, err)
	}
	if err := gascityBackendResponseError(resp); err != nil {
		if resp.Error != nil && resp.Error.Code == "not_found" {
			return fmt.Errorf("%s: %w", resp.Error.Message, ErrNotFound)
		}
		return err
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, result)
}

func gascityBackendResponseError(resp gascityBackendResponse) error {
	if resp.OK {
		return nil
	}
	if resp.Error == nil {
		return fmt.Errorf("gascity backend error")
	}
	return fmt.Errorf("gascity backend %s: %s", resp.Error.Code, resp.Error.Message)
}

func (s *GascityBackendStore) CloseStore() error {
	if s == nil {
		return nil
	}
	if s.sessionID != "" {
		_ = s.call("close", map[string]string{"session_id": s.sessionID}, nil)
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	return nil
}

func (s *GascityBackendStore) Create(b Bead) (Bead, error) {
	if b.Type == "" {
		b.Type = "task"
	}
	var out backendIssue
	err := s.call("create_issue", map[string]any{
		"session_id": s.sessionID,
		"issue":      backendIssueFromBead(b),
		"actor":      "gc",
		"commit":     true,
		"message":    "gc create",
	}, &out)
	return out.toBead(), err
}

func (s *GascityBackendStore) Get(id string) (Bead, error) {
	var out backendIssue
	err := s.call("get_issue", map[string]string{"session_id": s.sessionID, "id": id}, &out)
	return out.toBead(), err
}

func (s *GascityBackendStore) Update(id string, opts UpdateOpts) error {
	updates := backendUpdatesFromOpts(opts)
	if len(opts.Metadata) > 0 {
		current, err := s.Get(id)
		if err != nil {
			return err
		}
		updates["metadata"] = mergeBackendMetadataPatch(current.Metadata, opts.Metadata)
	}
	return s.call("update_issue", map[string]any{
		"session_id": s.sessionID,
		"id":         id,
		"updates":    updates,
		"actor":      "gc",
		"commit":     true,
		"message":    "gc update",
	}, nil)
}

func (s *GascityBackendStore) Close(id string) error {
	return s.call("close_issue", map[string]string{"session_id": s.sessionID, "id": id, "actor": "gc"}, nil)
}

func (s *GascityBackendStore) Reopen(id string) error {
	return s.call("reopen_issue", map[string]string{"session_id": s.sessionID, "id": id, "actor": "gc"}, nil)
}

func (s *GascityBackendStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		if len(metadata) > 0 {
			if err := s.SetMetadataBatch(id, metadata); err != nil && !errors.Is(err, ErrNotFound) {
				return closed, err
			}
		}
		if err := s.Close(id); err == nil {
			closed++
		} else if !errors.Is(err, ErrNotFound) {
			return closed, err
		}
	}
	return closed, nil
}

func (s *GascityBackendStore) List(query ListQuery) ([]Bead, error) {
	if err := query.Validate(); err != nil {
		return nil, err
	}
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("gascity backend list: %w", ErrQueryRequiresScan)
	}
	var out []backendIssue
	method, params := backendListRequest(s.sessionID, query)
	if err := s.call(method, params, &out); err != nil {
		return nil, err
	}
	return ApplyListQuery(backendIssuesToBeads(out), query), nil
}

func (s *GascityBackendStore) ListOpen(status ...string) ([]Bead, error) {
	q := ListQuery{AllowScan: true}
	if len(status) > 0 {
		q.Status = status[0]
	}
	return s.List(q)
}

func (s *GascityBackendStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(query)
	var out []backendIssue
	if err := s.call("ready_work", map[string]any{"session_id": s.sessionID, "filter": backendReadyFilter(q)}, &out); err != nil {
		return nil, err
	}
	beads := backendIssuesToBeads(out)
	if q.Assignee != "" || q.Limit > 0 {
		filter := ListQuery{Assignee: q.Assignee, Limit: q.Limit, AllowScan: true}
		beads = ApplyListQuery(beads, filter)
	}
	return beads, nil
}

func (s *GascityBackendStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{ParentID: parentID, IncludeClosed: HasOpt(opts, IncludeClosed), AllowScan: true, TierMode: TierModeFromOpts(opts)})
}

func (s *GascityBackendStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{Label: label, Limit: limit, IncludeClosed: HasOpt(opts, IncludeClosed), AllowScan: true, TierMode: TierModeFromOpts(opts)})
}

func (s *GascityBackendStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{Assignee: assignee, Status: status, Limit: limit, AllowScan: true})
}

func (s *GascityBackendStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{Metadata: filters, Limit: limit, IncludeClosed: HasOpt(opts, IncludeClosed), AllowScan: true, TierMode: TierModeFromOpts(opts)})
}

func (s *GascityBackendStore) SetMetadata(id, key, value string) error {
	return s.call("slot_set", map[string]string{"session_id": s.sessionID, "issue_id": id, "key": key, "value": value, "actor": "gc"}, nil)
}

func (s *GascityBackendStore) SetMetadataBatch(id string, kvs map[string]string) error {
	for key, value := range kvs {
		if err := s.SetMetadata(id, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (s *GascityBackendStore) Tx(_ string, fn func(Tx) error) error { return runSequentialTx(s, fn) }

func (s *GascityBackendStore) Delete(id string) error {
	return s.call("delete_issue", map[string]string{"session_id": s.sessionID, "id": id}, nil)
}

func (s *GascityBackendStore) Ping() error { return nil }

func (s *GascityBackendStore) DepAdd(issueID, dependsOnID, depType string) error {
	return s.call("add_dependency", map[string]any{
		"session_id": s.sessionID,
		"actor":      "gc",
		"dependency": map[string]string{"issue_id": issueID, "depends_on_id": dependsOnID, "type": depType},
	}, nil)
}

func (s *GascityBackendStore) DepRemove(issueID, dependsOnID string) error {
	return s.call("remove_dependency", map[string]string{"session_id": s.sessionID, "issue_id": issueID, "depends_on_id": dependsOnID, "actor": "gc"}, nil)
}

func (s *GascityBackendStore) DepList(id, direction string) ([]Dep, error) {
	method := "get_dependencies"
	if direction == "up" {
		method = "get_dependents"
	}
	var out []backendIssue
	if err := s.call(method, map[string]string{"session_id": s.sessionID, "issue_id": id}, &out); err != nil {
		return nil, err
	}
	deps := make([]Dep, 0, len(out))
	for _, issue := range out {
		deps = append(deps, Dep{IssueID: id, DependsOnID: issue.ID, Type: "blocks"})
	}
	return deps, nil
}

type backendIssue struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Status      string          `json:"status,omitempty"`
	Priority    int             `json:"priority,omitempty"`
	Type        string          `json:"issue_type,omitempty"`
	Assignee    string          `json:"assignee,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Labels      []string        `json:"labels,omitempty"`
	Deps        []Dep           `json:"dependencies,omitempty"`
	ParentID    string          `json:"parent_id,omitempty"`
	Ephemeral   bool            `json:"ephemeral,omitempty"`
	NoHistory   bool            `json:"no_history,omitempty"`
	DeferUntil  *time.Time      `json:"defer_until,omitempty"`
}

func (i backendIssue) toBead() Bead {
	priority := i.Priority
	meta := StringMap{}
	if len(i.Metadata) > 0 {
		_ = json.Unmarshal(i.Metadata, &meta)
	}
	return Bead{
		ID: i.ID, Title: i.Title, Status: i.Status, Type: i.Type, Priority: &priority,
		CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt, Assignee: i.Assignee, ParentID: i.ParentID,
		Description: i.Description, Labels: i.Labels, Metadata: meta, Dependencies: i.Deps,
		Ephemeral: i.Ephemeral, NoHistory: i.NoHistory, DeferUntil: i.DeferUntil,
	}
}

func backendIssueFromBead(b Bead) backendIssue {
	priority := 0
	if b.Priority != nil {
		priority = *b.Priority
	}
	meta, _ := json.Marshal(map[string]string(b.Metadata))
	return backendIssue{
		ID: b.ID, Title: b.Title, Description: b.Description, Status: b.Status,
		Priority: priority, Type: b.Type, Assignee: b.Assignee, ParentID: b.ParentID,
		Labels: b.Labels, Metadata: meta, Deps: backendDepsFromBead(b),
		Ephemeral: b.Ephemeral, NoHistory: b.NoHistory, DeferUntil: b.DeferUntil,
	}
}

func backendDepsFromBead(b Bead) []Dep {
	deps := make([]Dep, 0, len(b.Dependencies)+len(b.Needs)+1)
	for _, dep := range b.Dependencies {
		if strings.TrimSpace(dep.DependsOnID) == "" {
			continue
		}
		deps = append(deps, normalizeBackendDep(b.ID, dep))
	}
	if strings.TrimSpace(b.ParentID) != "" {
		deps = append(deps, Dep{IssueID: b.ID, DependsOnID: b.ParentID, Type: "parent-child"})
	}
	for _, need := range b.Needs {
		depType := "blocks"
		dependsOnID := strings.TrimSpace(need)
		if before, after, ok := strings.Cut(need, ":"); ok && strings.TrimSpace(before) != "" && strings.TrimSpace(after) != "" {
			depType = strings.TrimSpace(before)
			dependsOnID = strings.TrimSpace(after)
		}
		if dependsOnID == "" {
			continue
		}
		deps = append(deps, Dep{IssueID: b.ID, DependsOnID: dependsOnID, Type: depType})
	}
	return deps
}

func normalizeBackendDep(issueID string, dep Dep) Dep {
	dep.IssueID = strings.TrimSpace(dep.IssueID)
	if dep.IssueID == "" {
		dep.IssueID = issueID
	}
	dep.DependsOnID = strings.TrimSpace(dep.DependsOnID)
	dep.Type = strings.TrimSpace(dep.Type)
	if dep.Type == "" {
		dep.Type = "blocks"
	}
	return dep
}

func backendIssuesToBeads(in []backendIssue) []Bead {
	out := make([]Bead, len(in))
	for i := range in {
		out[i] = in[i].toBead()
	}
	return out
}

func backendUpdatesFromOpts(opts UpdateOpts) map[string]any {
	out := map[string]any{}
	if opts.Title != nil {
		out["title"] = *opts.Title
	}
	if opts.Status != nil {
		out["status"] = *opts.Status
	}
	if opts.Type != nil {
		out["issue_type"] = *opts.Type
	}
	if opts.Priority != nil {
		out["priority"] = *opts.Priority
	}
	if opts.Description != nil {
		out["description"] = *opts.Description
	}
	if opts.ParentID != nil {
		out["parent_id"] = *opts.ParentID
	}
	if opts.Assignee != nil {
		out["assignee"] = *opts.Assignee
	}
	if opts.Labels != nil {
		out["labels"] = opts.Labels
	}
	return out
}

func mergeBackendMetadataPatch(current map[string]string, patch map[string]string) map[string]string {
	merged := make(map[string]string, len(current)+len(patch))
	for k, v := range current {
		merged[k] = v
	}
	for k, v := range patch {
		merged[k] = v
	}
	return merged
}

func backendListRequest(sessionID string, q ListQuery) (string, any) {
	filter := map[string]any{}
	if q.Status != "" {
		filter["status"] = q.Status
	}
	if q.Type != "" {
		filter["issue_type"] = q.Type
	}
	if q.Assignee != "" {
		filter["assignee"] = q.Assignee
	}
	if q.Label != "" {
		filter["label"] = q.Label
	}
	if q.ParentID != "" {
		filter["parent_id"] = q.ParentID
	}
	if len(q.Metadata) > 0 {
		filter["metadata"] = q.Metadata
	}
	if q.Limit > 0 {
		filter["limit"] = q.Limit
	}
	if q.TierMode == TierWisps {
		return "list_wisps", map[string]any{"session_id": sessionID, "filter": filter}
	}
	return "search_issues", map[string]any{"session_id": sessionID, "filter": filter}
}

func backendReadyFilter(q ReadyQuery) map[string]any {
	filter := map[string]any{}
	if q.Assignee != "" {
		filter["assignee"] = q.Assignee
	}
	if q.Limit > 0 {
		filter["limit"] = q.Limit
	}
	if q.TierMode == TierBoth || q.TierMode == TierWisps {
		filter["include_ephemeral"] = true
	}
	return filter
}
