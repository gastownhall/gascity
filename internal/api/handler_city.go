package api

type cityGetResponse struct {
	Name            string `json:"name"`
	Path            string `json:"path"`
	Version         string `json:"version,omitempty"`
	Suspended       bool   `json:"suspended"`
	Provider        string `json:"provider,omitempty"`
	SessionTemplate string `json:"session_template,omitempty"`
	UptimeSec       int    `json:"uptime_sec"`
	AgentCount      int    `json:"agent_count"`
	RigCount        int    `json:"rig_count"`
}
