package main

import (
	"io"
)

type lifecycleActionJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Action        string `json:"action"`
	Message       string `json:"message,omitempty"`
	CityName      string `json:"city_name,omitempty"`
	CityPath      string `json:"city_path,omitempty"`
	SupervisorPID int    `json:"supervisor_pid,omitempty"`
	Wait          *bool  `json:"wait,omitempty"`
	Force         *bool  `json:"force,omitempty"`
	Async         *bool  `json:"async,omitempty"`
	Soft          *bool  `json:"soft,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	Revision      string `json:"revision,omitempty"`
}

func writeLifecycleActionJSON(stdout io.Writer, payload lifecycleActionJSON) error {
	payload.SchemaVersion = "1"
	payload.OK = true
	return writeCLIJSONLine(stdout, payload)
}

func writeLifecycleActionJSONOrExit(stdout, stderr io.Writer, context string, payload lifecycleActionJSON) int {
	payload.SchemaVersion = "1"
	payload.OK = true
	return writeCLIJSONLineOrExit(stdout, stderr, context, payload)
}

func lifecycleBoolPtr(v bool) *bool {
	return &v
}
