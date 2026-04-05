// Package agent defines agent-level types shared across Gas City subsystems.
package agent

import "github.com/gastownhall/gascity/internal/runtime"

// StartupHints carries provider startup behavior from config resolution
// through to runtime.Config. All fields are optional — zero values mean
// no special startup handling (fire-and-forget).
type StartupHints struct {
	ReadyPromptPrefix      string
	ReadyDelayMs           int
	ProcessNames           []string
	EmitsPermissionWarning bool
	// Nudge is text typed into the session after the agent is ready.
	// Used for CLI agents that don't accept command-line prompts.
	Nudge string
	// PreStart is a list of shell commands run before session creation.
	// Already template-expanded by the caller. Failures abort startup.
	PreStart []string
	// SessionSetup is a list of shell commands run after session creation.
	// Already template-expanded by the caller.
	SessionSetup []string
	// SessionSetupScript is a script path run after session_setup commands.
	SessionSetupScript string
	// SessionLive is a list of idempotent commands run after session_setup
	// and re-applied on config change without restart.
	SessionLive []string
	// PackOverlayDirs lists overlay directories from packs. Copied to
	// the session workdir before the agent's own OverlayDir.
	PackOverlayDirs []string
	// OverlayDir is the resolved overlay directory path on the host.
	// Passed through to the exec session provider for remote copy.
	OverlayDir string
	// CopyFiles lists files/directories to stage in the session's working
	// directory before the agent command starts.
	CopyFiles []runtime.CopyEntry
}
