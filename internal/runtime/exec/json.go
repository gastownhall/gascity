// Package exec implements [runtime.Provider] by delegating each operation
// to a user-supplied script via fork/exec. This follows the Git credential
// helper pattern: a single script receives the operation name as its first
// argument and communicates via stdin/stdout.
//
// See examples/session-scripts/README.md for the protocol specification.
package exec

import (
	"encoding/json"

	"github.com/gastownhall/gascity/internal/runtime"
)

// copyEntry is the JSON wire format for [runtime.CopyEntry].
type copyEntry struct {
	Src    string `json:"src"`
	RelDst string `json:"rel_dst,omitempty"`
}

// startConfig is the JSON wire format sent to the script's stdin on Start.
// It is intentionally separate from [runtime.Config] to own the serialization
// contract — the script sees stable JSON field names regardless of Go struct
// changes.
type startConfig struct {
	WorkDir            string            `json:"work_dir,omitempty"`
	Command            string            `json:"command,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	StartupEnvelope    json.RawMessage   `json:"startup_envelope,omitempty"`
	ProcessNames       []string          `json:"process_names,omitempty"`
	Nudge              string            `json:"nudge,omitempty"`
	ReadyPromptPrefix  string            `json:"ready_prompt_prefix,omitempty"`
	ReadyDelayMs       int               `json:"ready_delay_ms,omitempty"`
	PreStart           []string          `json:"pre_start,omitempty"`
	SessionSetup       []string          `json:"session_setup,omitempty"`
	SessionSetupScript string            `json:"session_setup_script,omitempty"`
	SessionLive        []string          `json:"session_live,omitempty"`
	PackOverlayDirs    []string          `json:"pack_overlay_dirs,omitempty"`
	OverlayDir         string            `json:"overlay_dir,omitempty"`
	CopyFiles          []copyEntry       `json:"copy_files,omitempty"`
}

// marshalStartConfig converts a [runtime.Config] to JSON for the exec script.
func marshalStartConfig(cfg runtime.Config) ([]byte, error) {
	var cfs []copyEntry
	for _, ce := range cfg.CopyFiles {
		cfs = append(cfs, copyEntry{Src: ce.Src, RelDst: ce.RelDst})
	}
	sc := startConfig{
		WorkDir:            cfg.WorkDir,
		Command:            cfg.Command,
		Env:                cfg.Env,
		StartupEnvelope:    cfg.StartupEnvelope,
		ProcessNames:       cfg.ProcessNames,
		Nudge:              cfg.Nudge,
		ReadyPromptPrefix:  cfg.ReadyPromptPrefix,
		ReadyDelayMs:       cfg.ReadyDelayMs,
		PreStart:           cfg.PreStart,
		SessionSetup:       cfg.SessionSetup,
		SessionSetupScript: cfg.SessionSetupScript,
		SessionLive:        cfg.SessionLive,
		PackOverlayDirs:    cfg.PackOverlayDirs,
		OverlayDir:         cfg.OverlayDir,
		CopyFiles:          cfs,
	}
	return json.Marshal(sc)
}
