package sessionlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrAgentNotFound is returned when a subagent file does not exist.
var ErrAgentNotFound = errors.New("agent not found")

// AgentMapping links a subagent to the parent Task tool_use that spawned it.
type AgentMapping struct {
	AgentID         string `json:"agent_id"`
	ParentToolUseID string `json:"parent_tool_use_id"`
}

// AgentStatus describes the lifecycle state of a subagent.
type AgentStatus string

// Agent lifecycle states.
const (
	AgentStatusPending   AgentStatus = "pending"
	AgentStatusRunning   AgentStatus = "running"
	AgentStatusCompleted AgentStatus = "completed"
	AgentStatusFailed    AgentStatus = "failed"
)

// AgentSession is a subagent's transcript and inferred status.
type AgentSession struct {
	Messages []*Entry    `json:"messages"`
	Status   AgentStatus `json:"status"`
}

// RawPayloads decodes each non-empty Entry.Raw into a generic JSON value
// and returns the slice. Same semantics as Session.RawPayloads.
func (s *AgentSession) RawPayloads() []any {
	out := make([]any, 0, len(s.Messages))
	for _, entry := range s.Messages {
		if entry == nil || len(entry.Raw) == 0 {
			continue
		}
		var v any
		if err := json.Unmarshal(entry.Raw, &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// agentDir returns the subagents directory for a session log path.
// Claude Code stores subagent files in {slug}/{session-uuid}/subagents/.
func agentDir(parentLogPath string) string {
	base := filepath.Base(parentLogPath)
	sessionID := strings.TrimSuffix(base, filepath.Ext(base))
	return filepath.Join(filepath.Dir(parentLogPath), sessionID, "subagents")
}

// FindAgentFiles returns all agent-*.jsonl files in the subagents
// directory for the given parent session. Returns an empty slice if the
// subagents directory does not exist or contains no agent files.
func FindAgentFiles(parentLogPath string) ([]string, error) {
	dir := agentDir(parentLogPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no subagents directory — not an error
		}
		return nil, fmt.Errorf("reading subagents directory: %w", err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "agent-") && strings.HasSuffix(name, ".jsonl") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	return paths, nil
}

// FindAgentMappings scans agent-*.jsonl files alongside the parent session
// and extracts the parent_tool_use_id from each agent's first entry.
func FindAgentMappings(parentLogPath string) ([]AgentMapping, error) {
	agentPaths, err := FindAgentFiles(parentLogPath)
	if err != nil {
		return nil, err
	}
	if len(agentPaths) == 0 {
		return nil, nil
	}

	var mappings []AgentMapping
	for _, path := range agentPaths {
		agentID := agentIDFromPath(path)
		toolUseID := extractParentToolUseID(path)
		if agentID == "" {
			continue
		}
		mappings = append(mappings, AgentMapping{
			AgentID:         agentID,
			ParentToolUseID: toolUseID,
		})
	}
	return mappings, nil
}

// ValidateAgentID checks that an agent ID is safe for use in filesystem
// paths. It rejects IDs containing path separators or dot-dot sequences.
func ValidateAgentID(agentID string) error {
	if agentID == "" {
		return fmt.Errorf("empty agent ID")
	}
	if strings.ContainsAny(agentID, "/\\") || strings.Contains(agentID, "..") {
		return fmt.Errorf("invalid agent ID %q: must not contain path separators or '..'", agentID)
	}
	return nil
}

// ReadAgentSession reads a subagent JSONL file and returns its transcript
// and inferred status. Uses the same DAG resolution as parent sessions.
// Returns ErrAgentNotFound if the agent file does not exist.
func ReadAgentSession(parentLogPath, agentID string) (*AgentSession, error) {
	if err := ValidateAgentID(agentID); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAgentNotFound, err)
	}

	dir := agentDir(parentLogPath)
	agentPath := filepath.Join(dir, "agent-"+agentID+".jsonl")

	if _, err := os.Stat(agentPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", ErrAgentNotFound, err)
		}
		return nil, fmt.Errorf("checking agent file: %w", err)
	}

	entries, err := parseFile(agentPath)
	if err != nil {
		return nil, fmt.Errorf("reading agent transcript: %w", err)
	}

	dag := BuildDag(entries)
	status := inferAgentStatus(dag.ActiveBranch)

	return &AgentSession{
		Messages: dag.ActiveBranch,
		Status:   status,
	}, nil
}

// agentIDFromPath extracts the agent ID from a path like
// "/path/to/agent-{id}.jsonl".
func agentIDFromPath(path string) string {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "agent-") || !strings.HasSuffix(base, ".jsonl") {
		return ""
	}
	name := strings.TrimPrefix(base, "agent-")
	name = strings.TrimSuffix(name, ".jsonl")
	if name == "" {
		return ""
	}
	return name
}

// extractParentToolUseID reads the first few lines of an agent JSONL file
// and looks for the parentToolUseId field. Claude Code writes this on
// the first entry of every subagent session.
func extractParentToolUseID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// Check first 5 lines — the field is usually on line 1.
	for i := 0; i < 5 && scanner.Scan(); i++ {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry struct {
			ParentToolUseID string `json:"parentToolUseId"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.ParentToolUseID != "" {
			return entry.ParentToolUseID
		}
	}
	return ""
}

// inferAgentStatus determines the agent's status from its message history.
func inferAgentStatus(messages []*Entry) AgentStatus {
	if len(messages) == 0 {
		return AgentStatusPending
	}

	// Scan from the end for a result entry.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Type == "result" {
			if len(messages[i].Message) > 0 {
				var msg struct {
					IsError bool `json:"is_error"`
				}
				if err := json.Unmarshal(messages[i].Message, &msg); err == nil && msg.IsError {
					return AgentStatusFailed
				}
			}
			return AgentStatusCompleted
		}
	}
	return AgentStatusRunning
}
