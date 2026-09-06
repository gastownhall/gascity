package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/promptsafe"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/telemetry"
	workertranscript "github.com/gastownhall/gascity/internal/worker/transcript"
)

// staleKeyDetectDelay is the immutable production window between a keyed
// session start and the liveness probe that detects a stale resume key.
const staleKeyDetectDelay = 2 * time.Second

// StaleKeyDetectionWaiter waits until a started keyed session is ready for its
// stale-resume-key liveness probe. Implementations must return the context
// error when the wait is canceled.
type StaleKeyDetectionWaiter func(context.Context, string) error

func waitForStaleKeyDetection(ctx context.Context, _ string) error {
	return sleepWithContext(ctx, staleKeyDetectDelay)
}

const waitIdleNudgeTimeout = 30 * time.Second

// ErrStateSync reports that the runtime reached the requested lifecycle
// boundary but persisting the corresponding bead metadata failed.
var ErrStateSync = errors.New("session state sync failed")

// stripResumeFlag removes the resume flag and session key from a command
// string, returning a command suitable for a fresh start. When the strip
// is a no-op (the flag/key isn't in cmd, or either argument is empty),
// the original cmd is returned exactly — TrimSpace only runs when a
// replacement actually happened. Callers rely on exact equality with
// the input to detect the no-op case; trimming on a non-replacement
// path would corrupt that signal when cmd has leading/trailing
// whitespace.
func stripResumeFlag(cmd, resumeFlag, sessionKey string) string {
	if resumeFlag == "" || sessionKey == "" {
		return cmd
	}
	// Remove "--resume <key>" or similar from the command.
	target := resumeFlag + " " + sessionKey
	result := strings.Replace(cmd, " "+target, "", 1)
	if result == cmd {
		// Try without the leading space (flag at start of args).
		result = strings.Replace(cmd, target+" ", "", 1)
	}
	if result == cmd {
		return cmd
	}
	return strings.TrimSpace(result)
}

// stripSessionIDFlag removes the fresh-start session-id flag and its key from a
// command string. It is the first-start counterpart to stripResumeFlag: a
// session launched with "claude ... --session-id <key>" never carries the resume
// flag, so the resume strip is a no-op on it and the retry would otherwise
// respawn the command byte-identically. That replay is worse than useless —
// claude 2.1.233 rejects a reused id outright ("Error: Session ID <uuid> is
// already in use.", exit 1), so every retry dies the same way.
//
// Both the space form ("--session-id <key>") and the equals form
// ("--session-id=<key>") are handled. Like stripResumeFlag, a no-op returns cmd
// exactly so callers can detect it by equality.
func stripSessionIDFlag(cmd, sessionIDFlag, sessionKey string) string {
	if sessionIDFlag == "" || sessionKey == "" {
		return cmd
	}
	for _, target := range []string{
		sessionIDFlag + " " + sessionKey,
		sessionIDFlag + "=" + sessionKey,
	} {
		result := strings.Replace(cmd, " "+target, "", 1)
		if result == cmd {
			result = strings.Replace(cmd, target+" ", "", 1)
		}
		if result != cmd {
			return strings.TrimSpace(result)
		}
	}
	return cmd
}

// stripResumeFlagArg removes the generated resume flag/key pair from cmd,
// regardless of the key's value. It is the value-agnostic fallback for
// stripResumeFlag: when the session_key embedded in the resume command at build
// time has diverged from the bead's current session_key — a concurrent fresh
// start minted a new key, or a stale store read — the keyed strip is a no-op,
// and we must still produce a clean fresh-start command rather than wedge the
// session. Returns cmd unchanged when the generated resume shape is not present
// (in which case the command already carries no generated resume key and is
// itself a valid fresh-start command).
func stripResumeFlagArg(cmd, resumeFlag, resumeStyle string) string {
	if resumeFlag == "" {
		return cmd
	}
	if resumeStyle == "subcommand" {
		return stripInsertedResumeSubcommandArg(cmd, resumeFlag)
	}
	return stripTrailingResumeFlagArg(cmd, resumeFlag)
}

func stripTrailingResumeFlagArg(cmd, resumeFlag string) string {
	trimmed := strings.TrimRight(cmd, " ")
	keyStart := strings.LastIndexByte(trimmed, ' ')
	if keyStart < 0 {
		return cmd
	}
	beforeKey := strings.TrimRight(trimmed[:keyStart], " ")
	flagStart := strings.LastIndexByte(beforeKey, ' ')
	if flagStart < 0 {
		return cmd
	}
	if beforeKey[flagStart+1:] != resumeFlag {
		return cmd
	}
	return strings.TrimSpace(beforeKey[:flagStart])
}

func stripInsertedResumeSubcommandArg(cmd, resumeFlag string) string {
	trimmed := strings.TrimSpace(cmd)
	binaryEnd := strings.IndexByte(trimmed, ' ')
	if binaryEnd < 0 {
		return cmd
	}
	binary := trimmed[:binaryEnd]
	rest := strings.TrimLeft(trimmed[binaryEnd+1:], " ")
	needle := resumeFlag + " "
	if !strings.HasPrefix(rest, needle) {
		return cmd
	}
	afterFlag := strings.TrimLeft(rest[len(needle):], " ")
	keyEnd := strings.IndexByte(afterFlag, ' ')
	if keyEnd < 0 {
		return binary
	}
	afterKey := strings.TrimLeft(afterFlag[keyEnd+1:], " ")
	if afterKey == "" {
		return binary
	}
	return strings.TrimSpace(binary + " " + afterKey)
}

// stripSessionIDFlagArg removes the fresh-start session-id flag and its value
// from cmd regardless of the value — the value-agnostic fallback for
// stripSessionIDFlag, mirroring stripResumeFlagArg for the resume flag. When the
// session_key embedded in a first-start "<flag> <key>" command at build time has
// diverged from the bead's current session_key (a concurrent fresh start minted
// a new key, or a stale store read), the keyed strip is a no-op, and — because a
// first start carries no resume flag — the resume fallback cannot reach it
// either. Without this strip the retry replays the dead "--session-id <oldkey>"
// into the same "id already in use" provider rejection the retry exists to
// escape. Both the space form ("--session-id <key>") and the equals form
// ("--session-id=<key>") are handled. Returns cmd unchanged when the flag is
// empty or absent — the command then carries no generated session id and is
// itself a valid fresh-start command.
//
// The removal is done in place: only the "<flag> <value>" / "<flag>=<value>"
// span (plus one adjacent separator) is excised, leaving every other argument
// and its whitespace verbatim — the keyed stripSessionIDFlag and the resume
// fallback stripResumeFlagArg preserve the rest of the command the same way.
// (Re-tokenizing with strings.Fields and rejoining with strings.Join would
// collapse unrelated whitespace and rewrite the structure of quoted or
// multiline commands even when only the flag was meant to change.) Like those
// siblings it is deliberately shell-token-simple: it matches the flag only at a
// start-of-string or single-space boundary, which is sufficient for the
// framework-generated commands this fallback ever sees and never for
// caller-supplied shell text.
func stripSessionIDFlagArg(cmd, sessionIDFlag string) string {
	if sessionIDFlag == "" {
		return cmd
	}
	for searchFrom := 0; ; {
		idx := strings.Index(cmd[searchFrom:], sessionIDFlag)
		if idx < 0 {
			return cmd
		}
		flagStart := searchFrom + idx
		afterFlag := flagStart + len(sessionIDFlag)
		// Require a token boundary before the flag so it is not matched inside a
		// longer token (e.g. "--not-session-id" or quote-prefixed literal text).
		if flagStart > 0 && cmd[flagStart-1] != ' ' {
			searchFrom = afterFlag
			continue
		}
		spanEnd := afterFlag
		switch {
		case afterFlag < len(cmd) && cmd[afterFlag] == '=':
			// Equals form: flag and value are one token ending at the next space.
			spanEnd = afterFlag + 1
			for spanEnd < len(cmd) && cmd[spanEnd] != ' ' {
				spanEnd++
			}
		case afterFlag == len(cmd) || cmd[afterFlag] == ' ':
			// Space form: skip the separator to the value token, then to its end.
			for spanEnd < len(cmd) && cmd[spanEnd] == ' ' {
				spanEnd++
			}
			for spanEnd < len(cmd) && cmd[spanEnd] != ' ' {
				spanEnd++
			}
		default:
			// Right side is not a boundary (e.g. "--session-id-file"): keep looking.
			searchFrom = afterFlag
			continue
		}
		// Consume one adjacent separator so the removal leaves no doubled space,
		// preferring the space before the flag (matches stripSessionIDFlag's
		// " "+target / target+" " removal followed by TrimSpace).
		spanStart := flagStart
		if spanStart > 0 && cmd[spanStart-1] == ' ' {
			spanStart--
		} else if spanEnd < len(cmd) && cmd[spanEnd] == ' ' {
			spanEnd++
		}
		return strings.TrimSpace(cmd[:spanStart] + cmd[spanEnd:])
	}
}

func freshStartCommandFromMetadata(metadata map[string]string, fallback string) string {
	if metadata == nil {
		return fallback
	}
	if cmd := metadata["command"]; cmd != "" {
		return cmd
	}
	if provider := metadata["provider"]; provider != "" {
		return provider
	}
	return fallback
}

func (m *Manager) clearStaleResumeMetadata(id string, b *beads.Bead) error {
	if err := m.store.SetMetadata(id, "session_key", ""); err != nil {
		return fmt.Errorf("clearing stale resume metadata session_key: %w", err)
	}
	if err := m.store.SetMetadata(id, "started_config_hash", ""); err != nil {
		return fmt.Errorf("clearing stale resume metadata started_config_hash: %w", err)
	}
	if err := m.store.SetMetadata(id, "continuation_reset_pending", "true"); err != nil {
		return fmt.Errorf("clearing stale resume metadata continuation_reset_pending: %w", err)
	}
	// Priming markers share started_config_hash's lifetime (S19 Stage 2): this
	// stale-resume clear forces a fresh start, so the markers reset with it.
	for _, k := range primingResetKeys {
		if err := m.store.SetMetadata(id, k, ""); err != nil {
			return fmt.Errorf("clearing stale resume metadata %s: %w", k, err)
		}
	}
	if b.Metadata == nil {
		b.Metadata = make(map[string]string)
	}
	b.Metadata["session_key"] = ""
	b.Metadata["started_config_hash"] = ""
	b.Metadata["continuation_reset_pending"] = "true"
	for _, k := range primingResetKeys {
		b.Metadata[k] = ""
	}
	return nil
}

func (m *Manager) retryFreshStartAfterStaleKey(
	ctx context.Context,
	id string,
	b *beads.Bead,
	sessName,
	resumeCommand string,
	cfg runtime.Config,
	unroute func(),
) (bool, error) {
	// An empty session_key does not mean there is nothing to recover. The
	// command can still carry a generated resume shape, because it was built
	// while the key was present and the key was cleared before this start ran.
	// Refusing the recovery on an empty key is what made that launch
	// unrecoverable: the caller returned the start error, the supervisor
	// retried the identical doomed command, and the loop never converged.
	// stripResumeFlag is an exact no-op on an empty key, so this case falls
	// through to the value-agnostic strip below.
	resumeFlag := b.Metadata["resume_flag"]
	sessionKey := b.Metadata["session_key"]
	freshCmd := stripResumeFlag(resumeCommand, resumeFlag, b.Metadata["session_key"])
	// A first start carries "<session_id_flag> <key>", not the resume flag, so
	// the strip above cannot touch it. Remove it here or the retry replays the
	// dead command verbatim against an id the provider now considers taken.
	sessionIDFlag := b.Metadata["session_id_flag"]
	beforeSessionIDStrip := freshCmd
	freshCmd = stripSessionIDFlag(freshCmd, sessionIDFlag, b.Metadata["session_key"])
	// A non-empty session_id_flag whose keyed strip was a no-op means the
	// session_key embedded in the first-start command diverged from the bead's
	// current session_key (a concurrent fresh start minted a new key, or a stale
	// store read). The resume fallback below cannot reach it — a first start
	// carries no resume flag — so strip the "<session_id_flag> <key>" pair
	// value-agnostically here, mirroring stripResumeFlagArg. Otherwise the retry
	// replays the dead id into the same provider rejection it exists to escape.
	if sessionIDFlag != "" && freshCmd == beforeSessionIDStrip {
		freshCmd = stripSessionIDFlagArg(freshCmd, sessionIDFlag)
	}
	// An empty resume_flag means the command was never resume-capable
	// (e.g. a named-always session whose start command carries no
	// --resume-style flag). stripResumeFlag is intentionally a no-op in
	// that case, and the command is already a valid fresh start.
	//
	// A non-empty resume_flag whose keyed strip was a no-op means the
	// session_key embedded in resumeCommand diverged from the bead's
	// current session_key (a concurrent fresh start minted a new key, or a
	// stale store read). Fall back to a value-agnostic strip so we still
	// produce a clean fresh-start command. Refusing to retry here is what
	// wedged the session into a respawn/SIGTERM loop: the embedded resume
	// key was always stale, the keyed strip could never match it, and the
	// dead remain-on-exit pane lingered because we returned before
	// killExistingOrphans. If even the generic strip finds nothing, the
	// command carries no resume flag and is itself a fresh-start command.
	if resumeFlag != "" && freshCmd == resumeCommand {
		// With no key on the bead there is nothing a command could have diverged
		// from, and the decline below returns without starting anything, so the
		// supervisor re-enters this path once per reconcile tick. Log the
		// divergence only when a key actually exists to diverge.
		divergedFromKey := sessionKey != ""
		if b.Metadata["resume_command"] != "" {
			if divergedFromKey {
				log.Printf("session: resume key for %q diverged from explicit resume_command; falling back to stored start command", id)
			}
			freshCmd = freshStartCommandFromMetadata(b.Metadata, resumeCommand)
		} else {
			if divergedFromKey {
				log.Printf("session: resume key for %q diverged from bead metadata; falling back to generated resume strip", id)
			}
			freshCmd = stripResumeFlagArg(resumeCommand, resumeFlag, b.Metadata["resume_style"])
		}
	}
	// On the empty-key path there is no stale key to clear and no orphan of our
	// own to sweep, so a command that the strips left untouched carries no
	// resume shape at all. Relaunching it verbatim repeats the same failure at the
	// same cost, which is the loop this recovery exists to end. Report "not
	// retried" and let the caller propagate the original start error. Paths that
	// still hold a key keep their prior behavior: there the metadata clear and
	// the orphan sweep below are themselves the recovery, so an unchanged
	// command is still worth relaunching.
	if sessionKey == "" && freshCmd == resumeCommand {
		return false, nil
	}
	if err := m.clearStaleResumeMetadata(id, b); err != nil {
		if unroute != nil {
			unroute()
		}
		return false, err
	}
	cfg.Command = freshCmd
	// Refuse the fresh start if a prior escaped process for this session could
	// not be confirmed dead: a survivor would race this replacement for the
	// same work bead. This path reuses the existing bead ID, so there is no
	// fresh-create to roll back — unroute and propagate the error before Start.
	if orphanErr := m.killExistingOrphans(ctx, id); orphanErr != nil {
		if unroute != nil {
			unroute()
		}
		return false, fmt.Errorf("pre-start orphan cleanup: %w", orphanErr)
	}
	if err := m.sp.Start(ctx, sessName, cfg); err != nil {
		if unroute != nil {
			unroute()
		}
		return false, fmt.Errorf("fresh start after stale key: %w", err)
	}
	return true, nil
}

var (
	// ErrNotSession reports that the requested bead is not a session bead.
	ErrNotSession = errors.New("bead is not a session")
	// ErrSessionClosed reports that the requested session has been closed.
	ErrSessionClosed = errors.New("session is closed")
	// ErrSessionInactive reports that the requested session has no live runtime.
	ErrSessionInactive = errors.New("session is not active")
	// ErrSessionActive reports that the requested session currently has or is starting a live runtime.
	ErrSessionActive = errors.New("session is active")
	// ErrResumeRequired reports that the session cannot be resumed without an
	// explicit resume command.
	ErrResumeRequired = errors.New("session requires resume command")
	// ErrNoPendingInteraction reports that a session has nothing awaiting
	// user input or approval resolution.
	ErrNoPendingInteraction = errors.New("session has no pending interaction")
	// ErrInteractionUnsupported reports that the backing runtime cannot
	// surface or resolve structured pending interactions.
	ErrInteractionUnsupported = errors.New("session provider does not support interactive responses")
	// ErrInteractionMismatch reports that the response does not match the
	// currently pending interaction request.
	ErrInteractionMismatch = errors.New("pending interaction does not match request")
	// ErrPendingInteraction reports that the session is blocked on a pending
	// approval or question and cannot accept a new user turn.
	ErrPendingInteraction = errors.New("session has a pending interaction")
)

type sessionMutationLockEntry struct {
	mu   sync.Mutex
	refs int
}

var (
	sessionMutationLocksMu sync.Mutex
	sessionMutationLocks   = map[string]*sessionMutationLockEntry{}
)

// WithSessionMutationLock serializes metadata mutations for one session bead.
func WithSessionMutationLock(id string, fn func() error) error {
	return withSessionMutationLock(id, fn)
}

func withSessionMutationLock(id string, fn func() error) error {
	lock := acquireSessionMutationLock(id)
	defer releaseSessionMutationLock(id, lock)
	return fn()
}

func acquireSessionMutationLock(id string) *sessionMutationLockEntry {
	sessionMutationLocksMu.Lock()
	lock := sessionMutationLocks[id]
	if lock == nil {
		lock = &sessionMutationLockEntry{}
		sessionMutationLocks[id] = lock
	}
	lock.refs++
	sessionMutationLocksMu.Unlock()

	lock.mu.Lock()
	return lock
}

func releaseSessionMutationLock(id string, lock *sessionMutationLockEntry) {
	lock.mu.Unlock()

	sessionMutationLocksMu.Lock()
	lock.refs--
	if lock.refs == 0 {
		delete(sessionMutationLocks, id)
	}
	sessionMutationLocksMu.Unlock()
}

func sessionName(id string, b beads.Bead) string {
	sessName := b.Metadata["session_name"]
	if sessName == "" {
		sessName = sessionNameFor(id)
	}
	return sessName
}

func (m *Manager) loadSessionBead(id string, allowClosed bool) (beads.Bead, string, error) {
	b, err := m.store.Get(id)
	if err != nil {
		return beads.Bead{}, "", fmt.Errorf("getting session: %w", err)
	}
	if !IsSessionBeadOrRepairable(b) {
		return beads.Bead{}, "", fmt.Errorf("%w: bead %s (type=%q)", ErrNotSession, id, b.Type)
	}
	RepairEmptyType(m.store, &b)
	if !allowClosed && b.Status == "closed" {
		return beads.Bead{}, "", fmt.Errorf("%w: %s", ErrSessionClosed, id)
	}
	sessName := sessionName(id, b)
	if b.Status != "closed" {
		transport, _ := m.transportForBead(b, sessName)
		_ = m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)
	}
	return b, sessName, nil
}

func (m *Manager) sessionBead(id string) (beads.Bead, string, error) {
	return m.loadSessionBead(id, false)
}

// commitPendingContinuationReset resolves the continuation epoch a runtime
// start should publish, consuming a pending conversation reset on the way.
//
// Starts that do not route through the controller's pre-wake commit — Submit,
// Send, Attach, Start — rebuilt GC_CONTINUATION_EPOCH verbatim from metadata
// and never consumed the marker, so a message arriving inside the reconciler's
// kill-to-wake window restarted the pane on the pre-reset epoch and silently
// defeated the reset. Providers that carry conversation identity themselves
// (zcode keys its persisted provider session on this epoch) then resumed the
// conversation the operator had just reset.
//
// Rotation belongs to the consumer, not to the request: the reconciler records
// the same marker directly without routing through Manager.RequestFreshRestart,
// so rotating at request time would rotate on one path and not the other. Every
// start path bumps-and-clears in one batch instead, which keeps the total at
// exactly one rotation per reset however the reset arrived.
func (m *Manager) commitPendingContinuationReset(id string, b beads.Bead) (int, error) {
	epoch, err := strconv.Atoi(b.Metadata["continuation_epoch"])
	if err != nil || epoch <= 0 {
		epoch = DefaultContinuationEpoch
	}
	if strings.TrimSpace(b.Metadata["continuation_reset_pending"]) == "" {
		return epoch, nil
	}
	// Consume the marker and rotate together: whichever start path gets here
	// first clears it, so the epoch advances exactly once per reset even though
	// several paths can service one. This mirrors preWakeCommit, which does the
	// same for the controller wake.
	epoch++
	if err := m.store.SetMetadataBatch(id, map[string]string{
		"continuation_epoch":         strconv.Itoa(epoch),
		"continuation_reset_pending": "",
	}); err != nil {
		return 0, fmt.Errorf("committing pending continuation reset: %w", err)
	}
	if b.Metadata != nil {
		b.Metadata["continuation_epoch"] = strconv.Itoa(epoch)
		b.Metadata["continuation_reset_pending"] = ""
	}
	return epoch, nil
}

func (m *Manager) ensureRunning(ctx context.Context, id string, b beads.Bead, sessName, resumeCommand string, hints runtime.Config) error {
	transport, transportVerified := m.transportForBead(b, sessName)
	unroute := m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)
	if State(b.Metadata["state"]) != StateSuspended && m.sp.IsRunning(sessName) {
		if b.Metadata["transport"] == "" && transportVerified {
			m.persistTransport(id, b.Metadata["provider"], transport)
		}
		if err := m.confirmLiveSessionState(id, &b); err != nil {
			return err
		}
		return nil
	}
	// The runtime is not running. If a controller-driven reset is already
	// in flight for this session (durable continuation_reset_pending
	// marker, cleared by PreWakePatch only immediately before the
	// reconciler's own fresh Start), wait for it instead of independently
	// starting a second, uncoordinated runtime — the reconciler runs in a
	// separate long-lived controller process, so withSessionMutationLock
	// (in-process only) cannot serialize against it (#4079).
	if strings.TrimSpace(b.Metadata["continuation_reset_pending"]) == "true" {
		landed, fresh, err := m.waitForInFlightReset(ctx, id, sessName)
		if err != nil {
			return err
		}
		if landed {
			b = fresh
			if b.Metadata["transport"] == "" && transportVerified {
				m.persistTransport(id, b.Metadata["provider"], transport)
			}
			if err := m.confirmLiveSessionState(id, &b); err != nil {
				return err
			}
			return nil
		}
		// Timed out — the controller-driven reset itself appears stalled.
		// Fall through to the existing independent-start behavior below
		// rather than hang gc session attach indefinitely.
	}
	if resumeCommand == "" {
		return fmt.Errorf("%w: %s", ErrResumeRequired, id)
	}

	cfg := hints
	cfg.Command = resumeCommand
	if cfg.WorkDir == "" {
		cfg.WorkDir = b.Metadata["work_dir"]
	}
	generation, err := strconv.Atoi(b.Metadata["generation"])
	if err != nil || generation <= 0 {
		generation = DefaultGeneration
	}
	continuationEpoch, err := m.commitPendingContinuationReset(id, b)
	if err != nil {
		return err
	}
	instanceToken := b.Metadata["instance_token"]
	if instanceToken == "" {
		instanceToken = NewInstanceToken()
		if err := m.store.SetMetadata(id, "instance_token", instanceToken); err != nil {
			return fmt.Errorf("storing instance token: %w", err)
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata["instance_token"] = instanceToken
	}
	cfg.Env = mergeEnv(cfg.Env, RuntimeEnvWithSessionContext(
		infoFromPersistedBead(b),
		generation,
		continuationEpoch,
		instanceToken,
	))
	if gcProvider := providerKind(b); gcProvider != "" {
		cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": gcProvider})
	}
	cfg = runtime.SyncWorkDirEnv(cfg)
	started := false
	// Refuse to resume if a prior escaped process for this session could not be
	// confirmed dead: a survivor would race this replacement for the same work
	// bead (duplicate bd close). This is the stable/reused-bead-ID path — the
	// exact "old process survives alongside its replacement" scenario. No
	// fresh-create to roll back, so unroute and propagate before Start.
	if orphanErr := m.killExistingOrphans(ctx, id); orphanErr != nil {
		if unroute != nil {
			unroute()
		}
		return fmt.Errorf("pre-start orphan cleanup: %w", orphanErr)
	}
	if err := m.sp.Start(ctx, sessName, cfg); err != nil {
		if errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
			retried, retryErr := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, unroute)
			if retryErr != nil {
				return retryErr
			}
			if !retried {
				// The recovery declined: the command carries no resume shape to
				// strip, so a relaunch would repeat this failure verbatim.
				// Propagate the original start error rather than reporting a
				// start that never happened.
				if unroute != nil {
					unroute()
				}
				return fmt.Errorf("resuming session: %w", err)
			}
			started = retried
		} else if !errors.Is(err, runtime.ErrSessionExists) || !m.sp.IsRunning(sessName) {
			// Another caller may have resumed the same session after we loaded the
			// bead but before we reached Start. If the runtime is already up, treat
			// the resume as converged and only persist active state below.
			if unroute != nil {
				unroute()
			}
			return fmt.Errorf("resuming session: %w", err)
		}
	} else {
		started = true
	}

	// Stale session key detection: if we just started a session with a
	// resume flag but it died immediately, the session key is likely
	// invalid (e.g., "No conversation found"). Clear the key and retry
	// with a fresh start so the user isn't stuck with a dead pane.
	if started && b.Metadata["session_key"] != "" {
		if err := m.staleKeyDetectionWaiter(ctx, sessName); err != nil {
			// Context canceled during stale-key sleep: the runtime session
			// may already be running but we skip setting state="active".
			// This is self-healing via NDI — the next ensureRunning call
			// sees the suspended-state bead, attempts sp.Start, gets
			// ErrSessionExists (IsRunning=true), and persists "active".
			if unroute != nil {
				unroute()
			}
			return err
		}
		if !m.sp.IsRunning(sessName) {
			retried, err := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, unroute)
			if err != nil {
				return err
			}
			started = retried
		}
	}
	if b.Metadata["transport"] == "" && (started || transportVerified) {
		m.persistTransport(id, b.Metadata["provider"], transport)
	}
	if err := m.syncStoredMCPServers(id, &b, cfg.MCPServers); err != nil {
		return fmt.Errorf("%w: %w", ErrStateSync, err)
	}
	if err := m.confirmLiveSessionState(id, &b); err != nil {
		if started && !errors.Is(err, ErrStateSync) {
			_ = m.sp.Stop(sessName)
		}
		return err
	}
	return nil
}

// inFlightResetPollInterval and inFlightResetWaitTimeout bound how long
// waitForInFlightReset polls for a controller-driven reset to land before
// giving up and letting the caller start the runtime itself. Declared as
// vars (not consts) so tests can shrink them.
var (
	inFlightResetPollInterval = 200 * time.Millisecond
	inFlightResetWaitTimeout  = 5 * time.Second
)

// waitForInFlightReset polls for a controller-driven reset already in
// flight for id (continuation_reset_pending=="true") to complete, instead
// of racing it with an independent Start (#4079). Returns landed=true and
// the refreshed bead once continuation_reset_pending clears and the
// runtime is live; landed=false if inFlightResetWaitTimeout elapses first
// (e.g. the reconciler itself is stalled), so the caller can fall back to
// starting the runtime itself rather than hang indefinitely.
func (m *Manager) waitForInFlightReset(ctx context.Context, id, sessName string) (landed bool, fresh beads.Bead, err error) {
	deadline := time.Now().Add(inFlightResetWaitTimeout)
	for time.Now().Before(deadline) {
		if err := sleepWithContext(ctx, inFlightResetPollInterval); err != nil {
			return false, beads.Bead{}, err
		}
		fresh, err := m.store.Get(id)
		if err != nil {
			return false, beads.Bead{}, fmt.Errorf("polling in-flight reset for %s: %w", id, err)
		}
		if strings.TrimSpace(fresh.Metadata["continuation_reset_pending"]) != "true" && m.sp.IsRunning(sessName) {
			return true, fresh, nil
		}
	}
	return false, beads.Bead{}, nil
}

func (m *Manager) ensureRunningRuntimeOnly(ctx context.Context, id string, b beads.Bead, sessName, resumeCommand string, hints runtime.Config) error {
	transport, _ := m.transportForBead(b, sessName)
	unroute := m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)
	if m.sp.IsRunning(sessName) {
		return nil
	}
	if resumeCommand == "" {
		return fmt.Errorf("%w: %s", ErrResumeRequired, id)
	}

	cfg := hints
	cfg.Command = resumeCommand
	if cfg.WorkDir == "" {
		cfg.WorkDir = b.Metadata["work_dir"]
	}
	generation, err := strconv.Atoi(b.Metadata["generation"])
	if err != nil || generation <= 0 {
		generation = DefaultGeneration
	}
	continuationEpoch, err := m.commitPendingContinuationReset(id, b)
	if err != nil {
		return err
	}
	instanceToken := b.Metadata["instance_token"]
	if instanceToken == "" {
		instanceToken = NewInstanceToken()
		if err := m.store.SetMetadata(id, "instance_token", instanceToken); err != nil {
			return fmt.Errorf("storing instance token: %w", err)
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata["instance_token"] = instanceToken
	}
	cfg.Env = mergeEnv(cfg.Env, RuntimeEnvWithSessionContext(
		infoFromPersistedBead(b),
		generation,
		continuationEpoch,
		instanceToken,
	))
	if gcProvider := strings.TrimSpace(b.Metadata["provider_kind"]); gcProvider != "" {
		cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": gcProvider})
	} else if provider := strings.TrimSpace(b.Metadata["provider"]); provider != "" {
		cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": provider})
	}
	cfg = runtime.SyncWorkDirEnv(cfg)
	started := false
	// Refuse to respawn if a prior escaped process for this session could not
	// be confirmed dead: a survivor would race this replacement for the same
	// work bead. This is the reconciler respawn bridge on a stable/reused bead
	// ID. No fresh-create to roll back, so unroute and propagate before Start.
	if orphanErr := m.killExistingOrphans(ctx, id); orphanErr != nil {
		if unroute != nil {
			unroute()
		}
		return fmt.Errorf("pre-start orphan cleanup: %w", orphanErr)
	}
	if err := m.sp.Start(ctx, sessName, cfg); err != nil {
		switch {
		case errors.Is(err, runtime.ErrSessionDiedDuringStartup):
			retried, retryErr := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, unroute)
			if retryErr != nil {
				return retryErr
			}
			if !retried {
				// The recovery declined: nothing to strip, so a relaunch would
				// repeat this failure verbatim. Propagate the original error.
				if unroute != nil {
					unroute()
				}
				return fmt.Errorf("resuming session: %w", err)
			}
			started = retried
		case errors.Is(err, runtime.ErrSessionExists) && m.sp.IsRunning(sessName):
			return err
		default:
			if unroute != nil {
				unroute()
			}
			return fmt.Errorf("resuming session: %w", err)
		}
	} else {
		started = true
	}
	if started && b.Metadata["session_key"] != "" {
		if err := m.staleKeyDetectionWaiter(ctx, sessName); err != nil {
			if unroute != nil {
				unroute()
			}
			return err
		}
		if !m.sp.IsRunning(sessName) {
			if _, err := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, unroute); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) confirmLiveSessionState(id string, b *beads.Bead) error {
	if b == nil {
		return nil
	}
	batch := make(map[string]string)
	switch State(b.Metadata["state"]) {
	case "", StateStartPending, StateCreating, StateAsleep, StateSuspended:
		batch["state"] = string(StateActive)
		batch["state_reason"] = "creation_complete"
	}
	if strings.TrimSpace(b.Metadata["pending_create_claim"]) != "" {
		batch["pending_create_claim"] = ""
		batch["pending_create_started_at"] = ""
	}
	if len(batch) == 0 {
		return nil
	}
	if err := m.store.SetMetadataBatch(id, batch); err != nil {
		return fmt.Errorf("%w: updating session state: %w", ErrStateSync, err)
	}
	if b.Metadata == nil {
		b.Metadata = make(map[string]string)
	}
	for k, v := range batch {
		b.Metadata[k] = v
	}
	return nil
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if ctx == nil {
		time.Sleep(d)
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func formatWaitIdleReminder(source, message string) string {
	// Sanitize attacker-controllable fields before interpolating into the
	// <system-reminder> block. The deferred-nudge body is sender-supplied, so
	// without this a sender can embed </system-reminder> sequences to break out
	// of the reminder and inject a forged operator/system directive.
	// See gastownhall/gascity#2195 and the ga-vs7 notification-injection incident.
	source = promptsafe.SanitizeForSystemReminder(source)
	message = promptsafe.SanitizeForSystemReminder(message)
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	sb.WriteString("You have a deferred reminder that was queued until a safe boundary:\n\n")
	fmt.Fprintf(&sb, "- [%s] %s\n", source, message)
	sb.WriteString("\nHandle them after this turn.\n")
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}

func (m *Manager) nudgeSession(ctx context.Context, sessName, message string, immediate bool) error {
	content := runtime.TextContent(message)
	err := m.nudgeContent(sessName, content, immediate)
	recordCtx := ctx
	if recordCtx == nil || recordCtx.Err() != nil {
		recordCtx = context.Background()
	}
	telemetry.RecordNudge(recordCtx, sessName, err)
	if err != nil {
		return fmt.Errorf("sending message to session: %w", err)
	}
	return nil
}

func (m *Manager) nudgeContent(sessName string, content []runtime.ContentBlock, immediate bool) error {
	if immediate {
		if np, ok := m.sp.(runtime.ImmediateNudgeProvider); ok {
			return np.NudgeNow(sessName, content)
		}
	}
	return m.sp.Nudge(sessName, content)
}

func normalizeWaitIdleNudgeSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "session"
	}
	return source
}

func (m *Manager) tryWaitIdleNudgeLocked(ctx context.Context, id string, b beads.Bead, source, sessName, message, resumeCommand string, hints runtime.Config) (bool, error) {
	if transportFromMetadata(b) == "acp" {
		if err := m.ensureRunning(ctx, id, b, sessName, resumeCommand, hints); err != nil {
			return false, err
		}
		if err := m.nudgeSession(ctx, sessName, message, false); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := m.ensureRunning(ctx, id, b, sessName, resumeCommand, hints); err != nil {
		return false, err
	}
	if providerKind(b) != "claude" {
		return false, nil
	}
	waiter, ok := m.sp.(runtime.IdleWaitProvider)
	if !ok {
		return false, nil
	}
	if err := waiter.WaitForIdle(ctx, sessName, waitIdleNudgeTimeout); err != nil {
		return false, nil
	}
	if err := m.nudgeSession(ctx, sessName, formatWaitIdleReminder(normalizeWaitIdleNudgeSource(source), message), true); err != nil {
		return false, nil
	}
	return true, nil
}

func (m *Manager) tryWaitIdleNudgeLiveOnlyLocked(ctx context.Context, b beads.Bead, source, sessName, message string) (bool, error) {
	if !m.sp.IsRunning(sessName) {
		return false, nil
	}
	if transportFromMetadata(b) == "acp" {
		if err := m.nudgeSession(ctx, sessName, message, false); err != nil {
			return false, err
		}
		return true, nil
	}
	if providerKind(b) != "claude" {
		return false, nil
	}
	waiter, ok := m.sp.(runtime.IdleWaitProvider)
	if !ok {
		return false, nil
	}
	if err := waiter.WaitForIdle(ctx, sessName, waitIdleNudgeTimeout); err != nil {
		return false, nil
	}
	if err := m.nudgeSession(ctx, sessName, formatWaitIdleReminder(normalizeWaitIdleNudgeSource(source), message), true); err != nil {
		return false, nil
	}
	return true, nil
}

func (m *Manager) pendingInteractionLocked(sessName string) error {
	if ip, ok := m.sp.(runtime.InteractionProvider); ok {
		pending, err := ip.Pending(sessName)
		if err != nil && !errors.Is(err, runtime.ErrInteractionUnsupported) {
			return fmt.Errorf("getting pending interaction: %w", err)
		}
		if pending != nil {
			return ErrPendingInteraction
		}
	}
	return nil
}

func (m *Manager) dismissKnownDialogsLocked(ctx context.Context, sessName string, timeout time.Duration) bool {
	dp, ok := m.sp.(runtime.DialogProvider)
	if !ok {
		return false
	}
	_ = dp.DismissKnownDialogs(ctx, sessName, timeout)
	return true
}

func (m *Manager) markStartupDialogsVerifiedLocked(id string, b *beads.Bead) {
	if err := m.store.SetMetadata(id, startupDialogVerifiedKey, "true"); err != nil {
		return
	}
	if b.Metadata == nil {
		b.Metadata = make(map[string]string)
	}
	b.Metadata[startupDialogVerifiedKey] = "true"
}

func (m *Manager) sendLocked(ctx context.Context, id string, b beads.Bead, sessName, message, resumeCommand string, hints runtime.Config, immediate bool) error {
	if err := m.ensureRunning(ctx, id, b, sessName, resumeCommand, hints); err != nil {
		return err
	}
	verifyDeferredDialogs := needsDeferredStartupDialogVerification(b)
	if verifyDeferredDialogs {
		m.dismissKnownDialogsLocked(ctx, sessName, codexDeferredDialogDelay)
	}
	if err := m.pendingInteractionLocked(sessName); err != nil {
		return err
	}
	if err := m.nudgeSession(ctx, sessName, message, immediate); err != nil {
		return err
	}
	if verifyDeferredDialogs && m.dismissKnownDialogsLocked(ctx, sessName, codexDeferredDialogDelay) {
		m.markStartupDialogsVerifiedLocked(id, &b)
	}
	return nil
}

func (m *Manager) send(ctx context.Context, id, message, resumeCommand string, hints runtime.Config, immediate bool) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		return m.sendLocked(ctx, id, b, sessName, message, resumeCommand, hints, immediate)
	})
}

func (m *Manager) sendLiveOnly(ctx context.Context, id, message string, immediate bool) (bool, error) {
	var delivered bool
	err := withSessionMutationLock(id, func() error {
		_, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		if !m.sp.IsRunning(sessName) {
			delivered = false
			return nil
		}
		if err := m.nudgeSession(ctx, sessName, message, immediate); err != nil {
			return err
		}
		delivered = true
		return nil
	})
	return delivered, err
}

// Start ensures the session runtime is live without sending a message.
// It is the canonical manager-level bring-up path for worker handles and
// other callers that need bounded startup without attaching a terminal.
func (m *Manager) Start(ctx context.Context, id, resumeCommand string, hints runtime.Config) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		return m.ensureRunning(ctx, id, b, sessName, resumeCommand, hints)
	})
}

// StartRuntimeOnly brings the runtime live for a bead-backed session without
// mutating persisted lifecycle metadata. Legacy reconciler callers use this
// bridge while they still own commit/rollback bookkeeping above the worker
// boundary.
func (m *Manager) StartRuntimeOnly(ctx context.Context, id, resumeCommand string, hints runtime.Config) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		return m.ensureRunningRuntimeOnly(ctx, id, b, sessName, resumeCommand, hints)
	})
}

// Send resumes a suspended session if needed, then nudges the runtime with a
// new user message.
func (m *Manager) Send(ctx context.Context, id, message, resumeCommand string, hints runtime.Config) error {
	return m.send(ctx, id, message, resumeCommand, hints, false)
}

// SendImmediate resumes a suspended session if needed, then injects the new
// user message without waiting for an idle boundary when the runtime supports
// immediate nudges. Falls back to Send semantics on runtimes without the
// optional immediate nudge capability.
func (m *Manager) SendImmediate(ctx context.Context, id, message, resumeCommand string, hints runtime.Config) error {
	return m.send(ctx, id, message, resumeCommand, hints, true)
}

// SendLiveOnly nudges the runtime only when the current session is already
// running. It never resumes or restarts the session.
func (m *Manager) SendLiveOnly(ctx context.Context, id, message string) (bool, error) {
	return m.sendLiveOnly(ctx, id, message, false)
}

// SendImmediateLiveOnly is like SendLiveOnly but uses the immediate nudge path
// when the runtime supports it. It never resumes or restarts the session.
func (m *Manager) SendImmediateLiveOnly(ctx context.Context, id, message string) (bool, error) {
	return m.sendLiveOnly(ctx, id, message, true)
}

// TryWaitIdleNudge delivers a best-effort session nudge at a provider-defined
// safe boundary. It resumes supported runtimes if needed, then reports whether
// live delivery actually happened. Unsupported providers return (false, nil)
// so higher layers can fall back to queue semantics without treating that as
// an operational error.
func (m *Manager) TryWaitIdleNudge(ctx context.Context, id, source, message, resumeCommand string, hints runtime.Config) (bool, error) {
	var delivered bool
	err := withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		delivered, err = m.tryWaitIdleNudgeLocked(ctx, id, b, source, sessName, message, resumeCommand, hints)
		return err
	})
	return delivered, err
}

// TryWaitIdleNudgeLiveOnly delivers a best-effort nudge at a safe boundary
// only when the runtime is already live. It never resumes or restarts the
// session.
func (m *Manager) TryWaitIdleNudgeLiveOnly(ctx context.Context, id, source, message string) (bool, error) {
	var delivered bool
	err := withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		delivered, err = m.tryWaitIdleNudgeLiveOnlyLocked(ctx, b, source, sessName, message)
		return err
	})
	return delivered, err
}

// StopTurn issues a provider-appropriate interrupt for the currently running
// turn. For providers that need post-interrupt idle settlement (e.g. Claude),
// it waits for the session to return to an idle prompt before returning.
func (m *Manager) StopTurn(id string) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		interruptStartedAt := time.Now()
		if err := m.stopTurnLocked(b, sessName); err != nil {
			return err
		}
		if err := m.waitForInterruptIdleLocked(context.Background(), b, sessName); err != nil {
			return fmt.Errorf("waiting for stopped session to become idle: %w", err)
		}
		if err := m.waitForInterruptBoundaryLocked(context.Background(), b, sessName, interruptStartedAt); err != nil {
			return fmt.Errorf("waiting for stopped session interrupt boundary: %w", err)
		}
		return nil
	})
}

// Pending returns the provider's current structured pending interaction, if
// the provider supports that capability.
func (m *Manager) Pending(id string) (*runtime.PendingInteraction, bool, error) {
	_, sessName, err := m.sessionBead(id)
	if err != nil {
		return nil, false, err
	}
	return m.PendingByName(sessName)
}

// PendingByName probes the provider for a pending interaction using an
// already-resolved runtime session name, skipping the bead-store lookup that
// Pending performs to map an id to a name. Callers that aggregate across many
// sessions (e.g. the city-wide pending snapshot) already hold each session's
// name and would otherwise pay a redundant store.Get per session. Error
// mapping mirrors Pending: unsupported -> (nil, false, nil); a gone runtime
// session -> (nil, true, nil); any other error -> (nil, true, error).
func (m *Manager) PendingByName(sessName string) (*runtime.PendingInteraction, bool, error) {
	ip, ok := m.sp.(runtime.InteractionProvider)
	if !ok {
		return nil, false, nil
	}
	pending, err := ip.Pending(sessName)
	if err != nil {
		if errors.Is(err, runtime.ErrInteractionUnsupported) {
			return nil, false, nil
		}
		if errors.Is(err, runtime.ErrSessionNotFound) {
			log.Printf("session: pending interaction runtime session gone for %q: %v", sessName, err)
			return nil, true, nil
		}
		return nil, true, fmt.Errorf("getting pending interaction: %w", err)
	}
	return pending, true, nil
}

// Respond resolves the current pending interaction for a session.
func (m *Manager) Respond(id string, response runtime.InteractionResponse) error {
	return withSessionMutationLock(id, func() error {
		_, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		ip, ok := m.sp.(runtime.InteractionProvider)
		if !ok {
			return ErrInteractionUnsupported
		}
		pending, err := ip.Pending(sessName)
		if err != nil {
			if errors.Is(err, runtime.ErrInteractionUnsupported) {
				return ErrInteractionUnsupported
			}
			if errors.Is(err, runtime.ErrSessionNotFound) {
				log.Printf("session: respond pending probe runtime session gone for %q: %v", sessName, err)
				return ErrNoPendingInteraction
			}
			return fmt.Errorf("getting pending interaction: %w", err)
		}
		if pending == nil {
			return ErrNoPendingInteraction
		}
		if response.RequestID == "" {
			response.RequestID = pending.RequestID
		}
		if response.Action == "" {
			return fmt.Errorf("interaction action is required")
		}
		if pending.RequestID != "" && response.RequestID != pending.RequestID {
			return fmt.Errorf("%w: pending interaction %q does not match request %q", ErrInteractionMismatch, pending.RequestID, response.RequestID)
		}
		if err := ip.Respond(sessName, response); err != nil {
			if errors.Is(err, runtime.ErrInteractionUnsupported) {
				return ErrInteractionUnsupported
			}
			if errors.Is(err, runtime.ErrSessionNotFound) {
				log.Printf("session: respond runtime session gone for %q: %v", sessName, err)
				return ErrNoPendingInteraction
			}
			return fmt.Errorf("responding to pending interaction: %w", err)
		}
		return nil
	})
}

// TranscriptLookup classifies why a transcript resolution came back empty, so
// callers can tell a permanent refusal from a not-yet-written transcript.
type TranscriptLookup int

const (
	// TranscriptFound reports that a transcript path was resolved.
	TranscriptFound TranscriptLookup = iota
	// TranscriptNoWorkDir reports that the session bead carries no work_dir, so
	// there is nothing to search — now or ever.
	TranscriptNoWorkDir
	// TranscriptAmbiguous reports that more than one session shares the workdir
	// with no stable session key to tell them apart, so resolution was refused
	// on purpose rather than coming up empty.
	TranscriptAmbiguous
	// TranscriptAbsent reports that the session was unambiguous but no
	// transcript file exists yet; a later attempt may find one.
	TranscriptAbsent
)

// TranscriptPath resolves the best available session transcript file.
// It prefers session-key-specific lookup and falls back to workdir-based
// discovery for providers that do not expose a stable session key.
func (m *Manager) TranscriptPath(id string, searchPaths []string) (string, error) {
	path, _, err := m.TranscriptPathClassified(id, searchPaths)
	return path, err
}

// TranscriptPathClassified resolves the best available session transcript file
// and reports why the resolution came back empty. Callers that must distinguish
// a permanent refusal (no workdir, or same-workdir ambiguity with no stable
// session key) from a transcript that simply has not been written yet use this
// instead of TranscriptPath, whose empty path is overloaded across all three.
func (m *Manager) TranscriptPathClassified(id string, searchPaths []string) (string, TranscriptLookup, error) {
	b, _, err := m.loadSessionBead(id, true)
	if err != nil {
		return "", TranscriptAbsent, err
	}
	workDir := b.Metadata["work_dir"]
	if workDir == "" {
		return "", TranscriptNoWorkDir, nil
	}
	provider := strings.TrimSpace(b.Metadata["provider_kind"])
	if provider == "" {
		provider = strings.TrimSpace(b.Metadata["provider"])
	}
	if len(searchPaths) == 0 {
		searchPaths = sessionlog.DefaultSearchPaths()
	}
	if path := workertranscript.DiscoverKeyedPath(searchPaths, provider, workDir, b.Metadata["session_key"]); path != "" {
		return path, TranscriptFound, nil
	}
	// zcode carries no session_key — no session-id flag, no hook plugin — so
	// the keyed lookup above can never hit for it and the ambiguity guard below
	// would leave every pooled worker transcript-dark. Its mirror is keyed by
	// the identity the bead does hold.
	if path := workertranscript.DiscoverScopedPath(
		searchPaths,
		provider,
		workDir,
		b.Metadata["session_name"],
		b.Metadata["continuation_epoch"],
	); path != "" {
		return path, TranscriptFound, nil
	}

	sameWorkDirSessions, err := m.sameWorkDirSessionBeads(b, provider, workDir)
	if err != nil {
		return "", TranscriptAbsent, err
	}
	if len(sameWorkDirSessions) > 1 {
		sameWorkDirInfos := make([]Info, 0, len(sameWorkDirSessions))
		for _, s := range sameWorkDirSessions {
			sameWorkDirInfos = append(sameWorkDirInfos, infoFromPersistedBead(s))
		}
		if path := ResolveCodexTranscriptBySessionOrder(searchPaths, provider, workDir, b.ID, sameWorkDirInfos); path != "" {
			return path, TranscriptFound, nil
		}
		// Without a stable session key, multiple sessions sharing the same
		// workdir cannot be mapped safely to a single transcript.
		return "", TranscriptAmbiguous, nil
	}
	if path := workertranscript.DiscoverPath(searchPaths, provider, workDir, ""); path != "" {
		return path, TranscriptFound, nil
	}
	return "", TranscriptAbsent, nil
}

// sameWorkDirSessionBeads returns the session beads that share workDir with the
// target b, restricted to the same provider family when the target's provider is
// known. For a live target, closed historical sessions are excluded; for a
// closed target they are kept so historical same-workdir ambiguity is preserved.
func (m *Manager) sameWorkDirSessionBeads(b beads.Bead, provider, workDir string) ([]beads.Bead, error) {
	all, err := m.store.List(beads.ListQuery{
		Label:         LabelSession,
		IncludeClosed: b.Status == "closed",
	})
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	var same []beads.Bead
	for _, other := range all {
		if !IsSessionBeadOrRepairable(other) {
			continue
		}
		if b.Status != "closed" && other.Status == "closed" {
			continue
		}
		otherProvider := strings.TrimSpace(other.Metadata["provider_kind"])
		if otherProvider == "" {
			otherProvider = strings.TrimSpace(other.Metadata["provider"])
		}
		if provider != "" && otherProvider != provider {
			continue
		}
		if other.Metadata["work_dir"] == workDir {
			same = append(same, other)
		}
	}
	return same, nil
}

// KeyedTranscriptPath returns the transcript path only when it resolves to a
// single session's file by a stable per-session key — never by the ambiguous
// workdir/newest-mtime fallback. Callers that must attribute a file to exactly
// one session (e.g. writing a session-id sidecar next to it) use this instead
// of TranscriptPath, which additionally serves that workdir fallback for
// history rendering.
//
// Coverage is whatever has both a captured per-session id and a 1:1 lookup:
// claude/kimi/pi/antigravity (keyed-path construction) and codex (its rollout
// filename carries the session-id suffix; the id is captured by the SessionStart
// hook). It returns "" for gemini/opencode/mimocode, which have a session id but
// no 1:1 by-id lookup, so only the unsafe workdir fallback would be available.
func (m *Manager) KeyedTranscriptPath(id string, searchPaths []string) (string, error) {
	b, _, err := m.loadSessionBead(id, true)
	if err != nil {
		return "", err
	}
	return ResolveKeyedTranscriptPath(infoFromPersistedBead(b), searchPaths), nil
}
