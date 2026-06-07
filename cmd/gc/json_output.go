package main

import (
	"encoding/json"
	"fmt"
	"io"
)

type cliJSONErrorOutput struct {
	SchemaVersion string       `json:"schema_version"`
	OK            bool         `json:"ok"`
	Error         cliJSONError `json:"error"`
}

type cliJSONError struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	ExitCode int    `json:"exit_code"`
}

type cliJSONDiagnostic struct {
	SchemaVersion string `json:"schema_version"`
	Level         string `json:"level"`
	Code          string `json:"code,omitempty"`
	Message       string `json:"message"`
	ExitCode      int    `json:"exit_code,omitempty"`
}

func writeJSONError(stdout, stderr io.Writer, code, message string, exitCode int) int {
	if exitCode == 0 {
		exitCode = 1
	}
	payload := cliJSONErrorOutput{
		SchemaVersion: "1",
		OK:            false,
		Error: cliJSONError{
			Code:     code,
			Message:  message,
			ExitCode: exitCode,
		},
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		writeJSONDiagnostic(stderr, "error", "json_encode_failed", fmt.Sprintf("encoding JSON error: %v", err), exitCode)
		return exitCode
	}
	writeJSONDiagnostic(stderr, "error", code, message, exitCode)
	return exitCode
}

func writeJSONDiagnostic(stderr io.Writer, level, code, message string, exitCode int) {
	if stderr == nil {
		return
	}
	payload := cliJSONDiagnostic{
		SchemaVersion: "1",
		Level:         level,
		Code:          code,
		Message:       message,
		ExitCode:      exitCode,
	}
	_ = json.NewEncoder(stderr).Encode(payload)
}
