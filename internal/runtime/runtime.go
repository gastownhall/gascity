// Package runtime defines the interface for agent runtime management.
//
// Callers depend on [Provider] for lifecycle and attach operations.
// The tmux subpackage provides the production implementation;
// [Fake] provides a test double with spy capabilities.
package runtime //nolint:revive // shadows stdlib runtime; isolated to internal

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrSessionExists reports that the runtime already has a live session with the
// requested name.
var ErrSessionExists = errors.New("session already exists")

// ErrSessionInitializing reports that a session's infrastructure exists but is
// still starting up (e.g., K8s pod is running but tmux hasn't started yet).
// Callers should back off and retry rather than treating it as a failure.
var ErrSessionInitializing = errors.New("session is initializing")

// ErrInteractionUnsupported reports that a provider does not implement the
// structured pending/respond interaction capability for the requested session.
var ErrInteractionUnsupported = errors.New("session interaction is unsupported")

// ErrSessionDiedDuringStartup reports that a provider created a session
// process, but it exited before startup completed successfully.
var ErrSessionDiedDuringStartup = errors.New("session died during startup")

// ErrSessionNotFound reports that an operation targeted a session the
// runtime does not know about. Benign for Stop() — the session was
// already gone — but fatal for Attach/Send. Providers wrap their own
// internal "not found" conditions with this sentinel so callers can
// dispatch with errors.Is.
var ErrSessionNotFound = errors.New("session not found")

// IsSessionGone reports whether err represents a "the session is not
// there" condition — either ErrSessionNotFound or the legacy provider
// phrasings that predate the sentinel (tmux/subprocess providers may
// still return raw strings). Callers that treat a missing session as
// benign (e.g. bulk Stop) use this helper so the semantics live in one
// place.
func IsSessionGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrSessionNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "session not found") ||
		strings.Contains(msg, "not running") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no tmux server running")
}

// ContentBlock represents a content element in a message.
// Type is "text" or "file_path".
type ContentBlock struct {
	Type string `json:"type"`           // "text" or "file_path"
	Text string `json:"text,omitempty"` // for type=text
	Path string `json:"path,omitempty"` // for type=file_path (server-side path)
}

// TextContent is a convenience constructor for a single text block.
func TextContent(text string) []ContentBlock {
	return []ContentBlock{{Type: "text", Text: text}}
}

// FlattenText concatenates the Text fields of all content blocks,
// separated by newlines. For file_path blocks, a placeholder reference
// is included so downstream consumers know a file was referenced.
func FlattenText(content []ContentBlock) string {
	var parts []string
	for _, b := range content {
		switch b.Type {
		case "file_path":
			if b.Path != "" {
				parts = append(parts, "[File: "+filepath.Base(b.Path)+"]")
			}
		default: // "text"
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// Provider manages agent sessions. Implementations handle the details
// of creating, destroying, and connecting to running agent processes.
//
// Implementations must be safe for concurrent use. Callers may invoke
// Start, Stop, Interrupt, IsRunning, ProcessAlive, and ListRunning from
// multiple goroutines at once for distinct session names. Same-name
// races must still preserve the documented semantics (for example,
// duplicate Start calls must reject consistently).
type Provider interface {
	// Start creates a new session with the given name and configuration.
	// The context controls the overall startup deadline — providers should
	// check ctx.Err() between steps and abort early on cancellation.
	// Returns an error if a session with that name already exists.
	Start(ctx context.Context, name string, cfg Config) error

	// Stop destroys the named session and cleans up its resources.
	// Returns nil if the session does not exist (idempotent).
	Stop(name string) error

	// Interrupt sends a soft interrupt signal (e.g., Ctrl-C / SIGINT) to
	// the named session. Best-effort: returns nil if the session doesn't
	// exist. Used for graceful shutdown before Stop.
	Interrupt(name string) error

	// IsRunning reports whether the named session exists and has a
	// live process.
	IsRunning(name string) bool

	// IsAttached reports whether a user terminal is currently connected
	// to the named session. Returns false if the session doesn't exist
	// or the provider doesn't support attach detection.
	IsAttached(name string) bool

	// Attach connects the user's terminal to the named session for
	// interactive use. Blocks until the user detaches.
	Attach(name string) error

	// ProcessAlive reports whether the named session has a live agent
	// process matching one of the given names in its process tree.
	// Returns true if processNames is empty (no check possible).
	ProcessAlive(name string, processNames []string) bool

	// Nudge sends structured content to the named session to wake or
	// redirect the agent. Returns nil if the session does not exist
	// and the provider can safely treat that as a best-effort no-op.
	// Providers that can observe a live session without owning the
	// delivery channel return [ErrSessionNotFound] so callers do not
	// mistake a no-op for delivery. Use [TextContent] to wrap a plain
	// string.
	Nudge(name string, content []ContentBlock) error

	// SetMeta stores a key-value pair associated with the named session.
	// Used for drain signaling and config fingerprint storage.
	SetMeta(name, key, value string) error

	// GetMeta retrieves a previously stored metadata value.
	// Returns ("", nil) if the key is not set.
	GetMeta(name, key string) (string, error)

	// RemoveMeta removes a metadata key from the named session.
	RemoveMeta(name, key string) error

	// Peek captures the last N lines of output from the named session.
	// If lines <= 0, captures all available scrollback.
	Peek(name string, lines int) (string, error)

	// ListRunning returns the names of all running sessions whose names
	// have the given prefix. Used for orphan detection.
	ListRunning(prefix string) ([]string, error)

	// GetLastActivity returns the time of the last I/O activity in the
	// named session. Returns zero time if unknown or unsupported.
	GetLastActivity(name string) (time.Time, error)

	// ClearScrollback clears the scrollback history of the named session.
	// Used after agent restart to give a clean slate. Best-effort.
	ClearScrollback(name string) error

	// CopyTo copies src (local file/directory) into the named session's
	// filesystem at relDst (relative to session workDir). Used for ad-hoc
	// post-Start copies (e.g., controller city-dir deployment).
	// Best-effort: returns nil if session unknown or src missing.
	CopyTo(name, src, relDst string) error

	// SendKeys sends bare keystrokes (e.g., "Enter", "Down", "C-c") to
	// the named session. Unlike Nudge (which sends text + Enter), SendKeys
	// sends raw key events without appending Enter. Used for dialog
	// dismissal and other non-text input.
	// Best-effort: returns nil if the session doesn't exist or the
	// provider doesn't support interactive input.
	SendKeys(name string, keys ...string) error

	// RunLive re-applies session_live commands to a running session.
	// Called by the reconciler when only session_live config has changed
	// (no restart needed). Best-effort: warnings on failure.
	RunLive(name string, cfg Config) error

	// Capabilities reports what this provider can reliably detect.
	// Used by the reconciler to skip inapplicable wake reasons.
	Capabilities() ProviderCapabilities
}

// PendingInteraction describes a blocking interaction raised by a session.
// This is an optional capability exposed by providers that support
// structured approvals, questions, or other turn-blocking prompts.
type PendingInteraction struct {
	RequestID string            `json:"request_id"`
	Kind      string            `json:"kind"`
	Prompt    string            `json:"prompt,omitempty"`
	Options   []string          `json:"options,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// InteractionResponse is the client's answer to a pending interaction.
type InteractionResponse struct {
	RequestID string            `json:"request_id,omitempty"`
	Action    string            `json:"action"`
	Text      string            `json:"text,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// InteractionProvider is an optional extension for providers that can
// surface and resolve pending session interactions.
type InteractionProvider interface {
	Pending(name string) (*PendingInteraction, error)
	Respond(name string, response InteractionResponse) error
}

// IdleWaitProvider is an optional extension for runtimes that can wait for a
// safe interactive boundary before input is injected.
//
// Implementations must treat timeout as a hard upper bound and return
// promptly once it expires. Callers may launch WaitForIdle asynchronously and
// rely on timeout-bounded completion for cleanup.
type IdleWaitProvider interface {
	WaitForIdle(ctx context.Context, name string, timeout time.Duration) error
}

// DialogProvider is an optional extension for runtimes that can detect and
// dismiss known startup-style dialogs (workspace trust, bypass permissions,
// rate-limit prompts) on an already-running session.
type DialogProvider interface {
	DismissKnownDialogs(ctx context.Context, name string, timeout time.Duration) error
}

// TransportCapabilityProvider is an optional extension for providers that can
// report whether they support starting sessions with a specific transport.
//
// Callers use this to fail fast when a requested transport cannot be routed by
// the active session provider before session creation starts mutating state.
type TransportCapabilityProvider interface {
	SupportsTransport(transport string) bool
}

// ImmediateNudgeProvider is an optional extension for runtimes that can inject
// input immediately without performing their own wait-idle heuristic first.
type ImmediateNudgeProvider interface {
	NudgeNow(name string, content []ContentBlock) error
}

// InterruptedTurnResetProvider is an optional extension for runtimes that can
// discard the just-interrupted user turn from the provider's active
// conversation state without restarting the session.
//
// Gemini CLI needs this after Ctrl-C: canceling generation alone does not
// remove the interrupted user turn, so the next reply can otherwise answer both
// the canceled request and the replacement request in one combined turn.
type InterruptedTurnResetProvider interface {
	ResetInterruptedTurn(ctx context.Context, name string) error
}

// InterruptBoundaryWaitProvider is an optional extension for runtimes that can
// confirm a provider-native interrupt boundary before the next user turn is
// injected.
//
// Codex CLI emits a durable "<turn_aborted>" marker when an in-flight turn has
// actually been canceled. Waiting for that marker avoids racing a replacement
// prompt into a session that still intends to finish the interrupted turn.
type InterruptBoundaryWaitProvider interface {
	WaitForInterruptBoundary(ctx context.Context, name string, since time.Time, timeout time.Duration) error
}

// CopyEntry describes a file or directory to stage in the session's
// working directory before the agent command starts.
type CopyEntry struct {
	// Src is the host-side source path (file or directory).
	Src string
	// RelDst is the destination relative to session workDir.
	// Empty means the workDir root.
	RelDst string
	// Probed indicates this entry was discovered via filesystem probing
	// (os.Stat) rather than derived from config. Probed entries use
	// content-based fingerprinting to avoid spurious config-drift when
	// files are recreated with identical content.
	Probed bool
	// ContentHash is a hex-encoded hash of the entry's content at discovery
	// time. Set for filesystem-probed entries (hook files, skills dirs) so
	// the config fingerprint is stable when content hasn't changed, even if
	// the file is recreated on every tick.
	// Empty for config-derived entries — those use Src/RelDst paths in the
	// fingerprint instead. When Probed is true but ContentHash is empty
	// (transient I/O error), the fingerprint uses a stable sentinel rather
	// than falling back to path-based hashing.
	ContentHash string
}

// HashPathContent returns a hex-encoded SHA-256 of the content at path.
// For a regular file, hashes the file content. For a directory, hashes
// a sorted manifest of relative paths and their contents while ignoring
// runtime-generated Python cache and editor backup artifacts. Returns empty
// string on any error (caller should treat as "unknown").
func HashPathContent(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	h := sha256.New()
	if !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		h.Write(data) //nolint:errcheck // hash.Write never errors
		return fmt.Sprintf("%x", h.Sum(nil))
	}
	// Directory: hash sorted manifest of relative paths + contents.
	// Fail closed: any walk or read error returns "" so the caller
	// gets the stable HASH_UNAVAILABLE sentinel instead of a partial hash.
	var entries []string
	var walkErr bool
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			walkErr = true
			return nil
		}
		rel, _ := filepath.Rel(path, p)
		if rel == "." {
			return nil
		}
		if hashPathContentSkipEntry(d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		entries = append(entries, rel)
		return nil
	})
	if walkErr {
		return ""
	}
	sort.Strings(entries)
	for _, rel := range entries {
		h.Write([]byte(rel)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})   //nolint:errcheck // hash.Write never errors
		data, err := os.ReadFile(filepath.Join(path, rel))
		if err != nil {
			return ""
		}
		h.Write(data)      //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0}) //nolint:errcheck // hash.Write never errors
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func hashPathContentSkipEntry(d fs.DirEntry) bool {
	base := d.Name()
	if d.IsDir() {
		switch base {
		case "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache":
			return true
		default:
			return false
		}
	}
	switch filepath.Ext(base) {
	case ".pyc", ".pyo":
		return true
	case ".swp", ".swx":
		return strings.HasPrefix(base, ".")
	}
	return strings.HasSuffix(base, "~")
}

// Config holds the parameters for starting a new session.
type Config struct {
	// WorkDir is the working directory for the session process.
	WorkDir string

	// Command is the shell command to run in the session.
	// If empty, a default shell is started.
	Command string

	// Env is additional environment variables set in the session.
	Env map[string]string

	// MCPServers is the effective ACP session/new MCP server list for this
	// session. Non-ACP providers ignore it.
	MCPServers []MCPServerConfig

	// Startup reliability hints (all optional — zero values skip).

	// ReadyPromptPrefix is the prompt prefix for readiness detection (e.g. "> ").
	ReadyPromptPrefix string

	// ReadyDelayMs is a fallback fixed delay when no prompt prefix is available.
	ReadyDelayMs int

	// ProcessNames lists expected process names for liveness checks.
	ProcessNames []string

	// EmitsPermissionWarning is true if the agent shows a bypass-permissions dialog.
	EmitsPermissionWarning bool

	// Nudge is text typed into the session after the agent is ready.
	// Used for CLI agents that don't accept command-line prompts.
	Nudge string

	// PreStart is a list of shell commands run before session creation,
	// on the target filesystem. Used for directory/worktree preparation.
	// Failures abort startup so agents never launch into an unprepared workDir.
	PreStart []string

	// SessionSetup is a list of shell commands run after session creation,
	// between verify-alive and nudge. Commands run in gc's process via sh -c.
	SessionSetup []string

	// SessionSetupScript is a script path run after session_setup commands.
	// Receives context via env vars (GC_SESSION plus existing GC_* vars).
	SessionSetupScript string

	// SessionLive is a list of idempotent shell commands run at startup
	// (after session_setup) and re-applied on config change without restart.
	// Typical use: tmux theming, keybindings, status bars.
	SessionLive []string

	// ProviderName is the resolved provider name (e.g., "claude", "codex").
	// Used for launch/runtime behavior that follows a built-in family.
	ProviderName string

	// ProviderOverlayName is the concrete provider name used for per-provider
	// overlay filtering. When empty, ProviderName is used for compatibility.
	ProviderOverlayName string

	// InstallAgentHooks lists additional provider hook slots whose
	// overlay/per-provider/<name>/ content should be staged alongside
	// ProviderOverlayName's. Populated from the agent's install_agent_hooks
	// config, so an agent running Claude can still get a materialized
	// .gemini/settings.json for parallel tooling.
	InstallAgentHooks []string

	// PackOverlayDirs lists overlay directories from packs. Contents are
	// copied to the session workdir before the agent's own OverlayDir,
	// providing additive pack-level file staging with lower priority.
	PackOverlayDirs []string

	// OverlayDir is the host-side overlay directory whose contents should
	// be copied into the session's working directory. Used by the exec
	// provider (e.g., K8s) to kubectl cp overlay files into the pod.
	// Empty means no overlay. Highest priority — overwrites pack overlays.
	OverlayDir string

	// CopyFiles lists files/directories to stage before the command runs.
	// Provider.Start handles the copy atomically: for local providers,
	// files are copied to workDir; for remote providers, files are
	// transported into the session environment.
	CopyFiles []CopyEntry

	// FingerprintExtra carries additional config data that should
	// participate in fingerprint comparison but isn't part of the session
	// startup command (e.g. pool config). Nil means no
	// extra data — the fingerprint covers only Command + Env.
	FingerprintExtra map[string]string

	// PromptSuffix is the shell-quoted prompt text appended to Command
	// when starting the session. Excluded from CoreFingerprint because
	// it contains beacon text with timestamps or other volatile data
	// that should not trigger restarts.
	PromptSuffix string

	// PromptFlag is the CLI flag (e.g., "--prompt") prepended to
	// PromptSuffix when constructing the startup command. When empty,
	// PromptSuffix is appended as a bare positional argument. Stored
	// separately so the tmux adapter's file-expansion path can
	// reconstruct the command correctly for long prompts.
	PromptFlag string
}

// OverlayProviderNames returns the effective provider overlay slots to stage for
// cfg, preserving first-use order while skipping empty and duplicate names.
func OverlayProviderNames(cfg Config) []string {
	return OverlayProviderNamesFromParts(cfg.ProviderName, cfg.ProviderOverlayName, cfg.InstallAgentHooks)
}

// OverlayProviderNamesFromParts returns the effective provider overlay slots
// for a launch provider, concrete overlay provider, and installed hooks.
func OverlayProviderNamesFromParts(providerName, providerOverlayName string, installAgentHooks []string) []string {
	primary := strings.TrimSpace(providerOverlayName)
	if primary == "" {
		primary = strings.TrimSpace(providerName)
	}
	providers := make([]string, 0, 1+len(installAgentHooks))
	providers = appendOverlayProviderName(providers, primary)
	for _, hook := range installAgentHooks {
		providers = appendOverlayProviderName(providers, hook)
	}
	return providers
}

func appendOverlayProviderName(providers []string, name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return providers
	}
	for _, existing := range providers {
		if existing == name {
			return providers
		}
	}
	return append(providers, name)
}

// SyncWorkDirEnv returns cfg with GC_DIR synchronized to WorkDir.
// It copies the Env map before mutation so callers can safely derive
// per-session configs from shared template state.
func SyncWorkDirEnv(cfg Config) Config {
	if cfg.WorkDir == "" {
		return cfg
	}
	if cfg.Env != nil && cfg.Env["GC_DIR"] == cfg.WorkDir {
		return cfg
	}
	env := make(map[string]string)
	for k, v := range cfg.Env {
		env[k] = v
	}
	env["GC_DIR"] = cfg.WorkDir
	cfg.Env = env
	return cfg
}
