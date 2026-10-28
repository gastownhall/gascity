//go:build windows

package herdr

import (
	"encoding/json"
	"strings"
)

type agentInfoWire struct {
	Name        string `json:"name"`
	Agent       string `json:"agent"`
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	TerminalID  string `json:"terminal_id"`
	AgentStatus string `json:"agent_status"`
	Cwd         string `json:"cwd"`
	Revision    uint64 `json:"revision"`
}

func (a *agentInfo) UnmarshalJSON(data []byte) error {
	var w agentInfoWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*a = agentInfo{
		Name:        strings.TrimSpace(w.Name),
		PaneID:      w.PaneID,
		WorkspaceID: w.WorkspaceID,
		TabID:       w.TabID,
		TerminalID:  w.TerminalID,
		AgentStatus: w.AgentStatus,
		Cwd:         w.Cwd,
		Revision:    w.Revision,
	}
	return nil
}

func (a agentInfo) MarshalJSON() ([]byte, error) {
	return json.Marshal(agentInfoWire{
		Name:        a.Name,
		Agent:       a.Name,
		PaneID:      a.PaneID,
		WorkspaceID: a.WorkspaceID,
		TabID:       a.TabID,
		TerminalID:  a.TerminalID,
		AgentStatus: a.AgentStatus,
		Cwd:         a.Cwd,
		Revision:    a.Revision,
	})
}

func filterAgentInfos(agents []agentInfo) []agentInfo {
	out := agents[:0]
	for _, a := range agents {
		if strings.TrimSpace(a.Name) == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}
