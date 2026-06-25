// Package runtime defines the interface for agent runtime management.
//
// Callers depend on [Provider] for lifecycle and attach operations.
// The tmux subpackage provides the production implementation;
// [Fake] provides a test double with spy capabilities.
package runtime //nolint:revive // shadows stdlib runtime; isolated to internal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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

// ErrExecUnsupported reports that a provider implements [ExecProvider] but the
// underlying runtime does not implement the RPP `exec` wire op (it answered
// exit 2). Carriers treat this as "fall back to the legacy driving op".
var ErrExecUnsupported = errors.New("runtime does not implement the exec op")

// ErrRelaunchUnsupported reports that the underlying runtime cannot relaunch the
// agent in a warm box (it is not a [RelaunchProvider], or is conjoined like
// subprocess/acp/t3bridge). Composite/wrapping providers return it from their
// own [RelaunchProvider.Relaunch] when the routed/wrapped backend does not
// support relaunch; the reconciler treats it as "fall back to full Stop+Start".
var ErrRelaunchUnsupported = errors.New("runtime does not support warm-box relaunch")

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

	// IsRunning reports whether the named provider runtime exists. It does not
	// prove that the configured agent process is alive; callers that need that
	// distinction should use ObserveLiveness or ProcessAlive.
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

// ExecProvider is an optional extension for runtimes that expose the RPP
// connection primitive: run a command inside the box and return its standard
// output and exit code. It is the op a [Carrier] drives the session-interaction
// verbs (Nudge / Peek / SendKeys / Interrupt / ClearScrollback) through. The
// exec Provider drives over it via the tmux carrier and falls back to the
// dedicated wire ops when Exec returns [ErrExecUnsupported]. A caller using an
// ExecProvider directly must likewise handle a provider that does not implement
// ExecProvider, or an Exec that returns [ErrExecUnsupported] (the provider type
// supports Exec but the underlying runtime does not implement the wire op).
//
// argv is the command and its arguments (no shell interpretation by the
// caller). output is the command's standard output, verbatim. code is the
// command's exit code: 0 on success, non-zero is the command's own result and
// is NOT an error; providers whose transport cannot observe the numeric code
// report 1 for any non-zero exit. A non-nil err signals the op could not run
// at all (transport/spawn failure, including context cancellation/timeout) or
// [ErrExecUnsupported] — distinct from a non-zero exit code.
type ExecProvider interface {
	Exec(ctx context.Context, name string, argv []string) (output []byte, code int, err error)
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

// RelaunchProvider is an optional extension for runtimes that can re-launch the
// agent inside an already-provisioned (warm) box WITHOUT re-provisioning it — the
// runtime/transport un-weld payoff. The reconciler calls Relaunch on a launch-only
// config change (LaunchFingerprint moved, ProvisionFingerprint unchanged) instead
// of a full Stop+Start. A missing box yields ErrSessionNotFound; the box, its env,
// and any staged files are reused. Runtimes whose agent IS the box (subprocess /
// acp / t3bridge) do NOT implement this — the reconciler falls back to Stop+Start
// for them.
//
// tmux / ssh / k8s implement it directly (respawn-pane in the warm box); the exec
// provider relaunches the agent over the exec op for a separable pack and falls
// back to Stop+Start for a welded pack. See worker-runtime-transport-unweld-v0.md.
type RelaunchProvider interface {
	Relaunch(ctx context.Context, name string, cfg Config) error
}

// LiveRuntime identifies a single agent runtime process discovered via
// process-table scan, independent of provider-visible artifacts.
type LiveRuntime struct {
	// SessionID is the GC_SESSION_ID value from the process environment.
	SessionID string
	// City is the GC_CITY_PATH value from the process environment (falling
	// back to GC_CITY). Empty when neither is readable. The process-table scan
	// is supervisor-wide (it walks all of /proc), but session beads and tmux
	// runtime tracking are per-city. A consumer that owns only one city's
	// store MUST filter scan results to City == its own city before reaping,
	// or it will mistake another city's live session for an orphan and kill
	// it.
	City string
	// Epoch is the GC_RUNTIME_EPOCH from the process environment, if readable.
	// Zero if the variable is absent or unparseable.
	Epoch int
	// PID is the OS process ID for local providers, or a provider-specific
	// process identifier for remote infrastructure.
	PID int
	// ProviderName is the session name as known to the provider. Empty means
	// the runtime is not visible in the provider's artifact registry.
	ProviderName string
	// IsTracked is true when this runtime also appears in the provider's
	// registry. False marks a live process that is invisible to the provider.
	IsTracked bool
}

// ProcessTableScanner is an optional extension for runtimes that can discover
// live agent root processes by GC_SESSION_ID independently of provider-visible
// artifacts.
//
// Providers that cannot inspect process tables do not implement this
// interface. Callers must use a type assertion and continue safely when the
// provider lacks the capability.
type ProcessTableScanner interface {
	// FindRuntimesBySessionID returns live agent root processes carrying
	// GC_SESSION_ID equal to id. If id is empty, it returns all live agent root
	// processes with any GC_SESSION_ID set.
	//
	// Best-effort implementations may return both partial results and a non-nil
	// error; callers should proceed with any returned results.
	FindRuntimesBySessionID(id string) ([]LiveRuntime, error)

	// TerminateRuntime stops the process or infrastructure unit identified by r.
	// It returns nil when the runtime is already gone.
	TerminateRuntime(r LiveRuntime) error
}

// ServerLifecycleProvider is an optional extension for providers that own
// server-level lifecycle alongside individual session management.
//
// This interface must not be added to [Provider]: providers backed by
// subprocesses, Kubernetes, fakes, or other non-server runtimes do not have a
// shared server to configure or tear down.
type ServerLifecycleProvider interface {
	// ConfigureServer applies server-level configuration. Implementations must
	// be idempotent, and callers should treat errors as best-effort warnings.
	ConfigureServer() error

	// TeardownServer terminates the shared server after all sessions have been
	// drained. Implementations should return nil when the server is already gone.
	TeardownServer() error
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

// Lifecycle describes the expected lifetime of a runtime command.
type Lifecycle string

const (
	// LifecycleOneShot marks commands that are expected to do bounded work and exit.
	LifecycleOneShot Lifecycle = "one_shot"
)

// Config holds the parameters for starting a new session.
type Config struct {
	// WorkDir is the working directory for the session process.
	WorkDir string

	// Command is the shell command to run in the session.
	// If empty, a default shell is started.
	Command string

	// Lifecycle describes whether the command is long-lived or expected to
	// exit after one turn. Empty means the default long-lived session lifecycle.
	Lifecycle Lifecycle

	// Upstream is the model-serving selection identity ("anthropic", "bedrock",
	// "proxy:<name>") — WHO serves+resolves the model. It is hashed into the
	// LAUNCH half of the fingerprint (Phase C), so switching upstream relaunches
	// the agent in the warm box (B2.3) rather than reprovisioning; the resolved
	// serving env (ANTHROPIC_BASE_URL/_API_KEY, injected into Env) is deliberately
	// NOT hashed, so a credential rotation never moves a fingerprint. Empty = no
	// upstream selected (behavior-identical; contributes nothing to the hash).
	Upstream string

	// Env is additional environment variables set in the session.
	Env map[string]string

	// MCPServers is the effective ACP session/new MCP server list for this
	// session. Non-ACP providers ignore it.
	MCPServers []MCPServerConfig

	// StartupEnvelope carries provider-specific startup metadata used by
	// the T3 bridge path. It is excluded from the core fingerprint.
	StartupEnvelope json.RawMessage

	// Startup reliability hints (all optional — zero values skip).

	// ReadyPromptPrefix is the prompt prefix for readiness detection (e.g. "> ").
	ReadyPromptPrefix string

	// ReadyDelayMs is a fallback fixed delay when no prompt prefix is available.
	ReadyDelayMs int

	// ProcessNames lists expected process names for liveness checks.
	ProcessNames []string

	// EmitsPermissionWarning is true if the agent shows a bypass-permissions dialog.
	EmitsPermissionWarning bool

	// AcceptStartupDialogs overrides automatic startup dialog handling.
	// Nil keeps the runtime default derived from other startup hints.
	AcceptStartupDialogs *bool

	// MouseOn reports whether tmux mouse mode should be preserved for this session.
	// When false, tmux startup disables mouse mode and monitor-activity to keep
	// terminal mouse escape sequences out of headless agent stdin.
	MouseOn bool

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
//
// The concrete providerOverlayName is the primary slot, falling back to the
// launch family providerName only when the concrete name is empty. Callers that
// stage onto a real overlay source should instead use
// EffectiveOverlayProviderNames, which downgrades a concrete name with no
// per-provider/<concrete>/ directory to the family so a custom provider (e.g.
// base="builtin:pi" "pi-vllm", which has no per-provider/pi-vllm/ overlay)
// still stages the family overlay where its hooks live (gc-6bw8o), while a
// provider that ships its own overlay (e.g. Kiro) keeps it.
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
