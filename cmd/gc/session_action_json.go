package main

import "io"

type sessionActionResult struct {
	SchemaVersion       string `json:"schema_version"`
	OK                  bool   `json:"ok"`
	Command             string `json:"command"`
	Action              string `json:"action"`
	SessionID           string `json:"session_id,omitempty"`
	State               string `json:"state,omitempty"`
	Mode                string `json:"mode,omitempty"`
	Title               string `json:"title,omitempty"`
	Identity            string `json:"identity,omitempty"`
	Before              string `json:"before,omitempty"`
	Cutoff              string `json:"cutoff,omitempty"`
	Count               *int   `json:"count,omitempty"`
	WaitNudgesWithdrawn int    `json:"wait_nudges_withdrawn,omitempty"`
	Pinned              *bool  `json:"pinned,omitempty"`
	MaterializedNamed   bool   `json:"materialized_named,omitempty"`
}

func sessionJSONRequested(values []bool) bool {
	return len(values) > 0 && values[0]
}

func writeSessionActionJSON(stdout io.Writer, result sessionActionResult) error {
	result.SchemaVersion = "1"
	result.OK = true
	if result.Command == "" && result.Action != "" {
		result.Command = commandName("session", result.Action)
	}
	return writeCLIJSONLine(stdout, result)
}
