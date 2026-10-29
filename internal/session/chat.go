package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/git"
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

// startFailure labels a failed provider Start: "resuming session" when cmd
// carries a resume shape (the bead's resume flag and key, a generated resume
// flag with any key, or the bead's expanded resume_command), otherwise
// "starting session". The shape tests are the exact no-op strips that
// retryFreshStartAfterStaleKey relies on.
func startFailure(cmd string, meta map[string]string, err error) error {
	resumeFlag := meta["resume_flag"]
	sessionKey := meta["session_key"]
	resumed := stripResumeFlag(cmd, resumeFlag, sessionKey) != cmd ||
		stripResumeFlagArg(cmd, resumeFlag, meta["resume_style"]) != cmd
	if rc := meta["resume_command"]; !resumed && rc != "" && sessionKey != "" {
		resumed = cmd == strings.ReplaceAll(rc, "{{.SessionKey}}", sessionKey)
	}
	if resumed {
		return fmt.Errorf("resuming session: %w", err)
	}
	return fmt.Errorf("starting session: %w", err)
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

// retainsCommittedConversation reports whether a failed resume must keep the
// conversation rather than start fresh: PreserveConversationOnRuntimeLoss
// retains the row, and its conversation is committed (a provider key and the
// configuration it started under). A first start whose key was only
// preallocated, an always-named one included, has nothing to keep and takes
// the stale-key recovery.
func retainsCommittedConversation(info Info) bool {
	return PreserveConversationOnRuntimeLoss(info) &&
		strings.TrimSpace(info.SessionKey) != "" && strings.TrimSpace(info.StartedConfigHash) != ""
}

// exitedSessionPeekLines bounds the last pane output reported for a session
// that started and then exited.
const exitedSessionPeekLines = 80

// exitedAfterStart is the startup death of a session that started and then
// exited, with its last pane output redacted against env, the launch's
// environment, as the runtime's own startup-failure capture is. A failed read
// is reported, not wrapped: the session still exited, whatever the read ran
// into.
func (m *Manager) exitedAfterStart(sessName string, env map[string]string) error {
	pane, err := m.sp.Peek(sessName, exitedSessionPeekLines)
	pane = strings.TrimSpace(runtime.RedactSecrets(pane, runtime.SetupCommandSecrets(env)))
	switch {
	case err != nil:
		return fmt.Errorf("%w: session %q exited after it started; reading its last pane output: %s", runtime.ErrSessionDiedDuringStartup, sessName, err.Error())
	case pane == "":
		return fmt.Errorf("%w: session %q exited after it started", runtime.ErrSessionDiedDuringStartup, sessName)
	}
	return fmt.Errorf("%w: session %q exited after it started; last pane output:\n%s", runtime.ErrSessionDiedDuringStartup, sessName, pane)
}

// retainFailedResume keeps a committed conversation whose resume launch died,
// because a dead runtime is not consent to reset it; the same policy fences
// advisory healing. The error wraps the failed start's own error, its last pane
// output included, so the caller can still tell a refused resume from any other
// death, and names the explicit reset that starts the session fresh. startErr
// is nil when the launch started and then exited; the pane is read before the
// route is released, and env is the launch's environment it is redacted
// against.
func (m *Manager) retainFailedResume(id, sessName string, env map[string]string, startErr error, unroute func()) error {
	if startErr == nil {
		startErr = m.exitedAfterStart(sessName, env)
	}
	if unroute != nil {
		unroute()
	}
	return fmt.Errorf("session %s: %w after failed resume; run `gc session reset %s` to start fresh: %w", id, ErrConversationRetained, id, startErr)
}

// retryFreshStartAfterStaleKey recovers a resume that died during startup by
// relaunching the session fresh. startErr is the failed Start's error, nil
// when the launch started and then exited. A Manager start's relaunch first
// reads the runtime fresh under lease (leasedLiveness), before it clears the
// key: a read that cannot answer refuses it, the key kept and nothing
// minted, and a runtime up with its agent alive is the start's own launch,
// which stands under inc with the key kept and nothing relaunched; a pane
// whose agent died is relaunched over. The relaunch is a new process, so it
// mints again under lease over inc, the failed start's incarnation, after
// the orphan kill, and runs under the new one, which it leaves in inc once
// the Start succeeds; a failed Start withdraws it. A nil inc relaunches under
// the failed start's identity with no read: the legacy runtime-only start,
// whose caller commits against the incarnation its PreWake minted, so a mint
// under it would make its own commit stale (deleted with legacy at P6).
func (m *Manager) retryFreshStartAfterStaleKey(
	ctx context.Context,
	id string,
	b *beads.Bead,
	sessName,
	resumeCommand string,
	cfg runtime.Config,
	startErr error,
	unroute func(),
	lease *RuntimeLease,
	inc *Incarnation,
) (bool, error) {
	if retainsCommittedConversation(infoFromPersistedBead(*b)) {
		return false, m.retainFailedResume(id, sessName, cfg.Env, startErr, unroute)
	}
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
	// (e.g. a never-committed always-named session whose start command
	// carries no --resume-style flag; a committed one is retained above).
	// stripResumeFlag is intentionally a no-op in that case, and the command
	// is already a valid fresh start.
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
	// Under the lease, a runtime up with its agent alive is the start's own
	// launch, whose death its Start reported or a cached read took for gone:
	// the start converges on it, as on one it finds up before its mint
	// (ensureRunningOn), and like that converge reads no token (ga-9yerfq).
	if inc != nil {
		upAndAlive, err := m.leasedLiveness(id, sessName, cfg.ProcessNames)
		if err != nil {
			if unroute != nil {
				unroute()
			}
			return false, fmt.Errorf("fresh start after stale key: %w", err)
		}
		if upAndAlive {
			return true, nil
		}
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
	// The relaunch keeps the failed start's continuation epoch and leaves the
	// reset the clear just recorded to the next start, as it always has: only
	// its generation and token are new. A relaunch that fails withdraws its
	// mint, leaving the row on inc, unless its process may have survived
	// (launchedUnder): then the row stays on the relaunch's, left in inc.
	var next Incarnation
	if inc != nil {
		var err error
		if next, err = m.mintStart(ctx, lease, inc.By, b, false); err != nil {
			if unroute != nil {
				unroute()
			}
			return false, fmt.Errorf("fresh start after stale key: %w", err)
		}
		cfg.Env = mergeEnv(cfg.Env, next.Env())
	}
	if err := m.sp.Start(ctx, sessName, cfg); err != nil {
		switch {
		case inc == nil:
		case m.launchedUnder(sessName, cfg.ProcessNames, next):
			*inc = next // the failed relaunch's process may run under it
		default:
			m.unmintStart(next, b)
		}
		if unroute != nil {
			unroute()
		}
		return false, fmt.Errorf("fresh start after stale key: %w", err)
	}
	if inc != nil {
		*inc = next
	}
	return true, nil
}

// mintStart mints the incarnation a Manager start of row b runs under, under
// lease, with nothing beyond the identity keys; consumeReset also consumes
// the continuation reset b records in the same CAS (the epoch bumped, the
// marker cleared), as commitPendingContinuationReset does, when the mint's
// read still records it. The start was decided on b, so a reset recorded
// after b was read stays pending for the next start, and one consumed since
// bumps nothing. What it wrote is folded onto b.
func (m *Manager) mintStart(ctx context.Context, lease *RuntimeLease, by ActorKind, b *beads.Bead, consumeReset bool) (Incarnation, error) {
	expect := infoFromPersistedBead(*b)
	inc, err := m.PersistedStore().MintIncarnation(ctx, lease, MintRequest{
		Expect: expect, By: by, Now: m.now(),
		FreshContext: func(fresh Info) bool {
			return consumeReset && strings.TrimSpace(fresh.ContinuationResetPending) != ""
		},
	})
	if err != nil {
		return Incarnation{}, err
	}
	if b.Metadata == nil {
		b.Metadata = make(map[string]string)
	}
	maps.Copy(b.Metadata, inc.IdentityPatch())
	return inc, nil
}

// unmintStart withdraws inc, a start's mint no process ran under
// (unmintIncarnation), folds what it wrote back onto b when b is not nil, and
// reports whether the withdrawal landed. One the row refused (it moved on)
// or that failed is logged.
func (m *Manager) unmintStart(inc Incarnation, b *beads.Bead) bool {
	landed, err := m.PersistedStore().unmintIncarnation(inc)
	switch {
	case err != nil:
		log.Printf("session: %s: withdrawing the unlaunched %s: %v", inc.ID, inc, err)
	case !landed:
		log.Printf("session: %s: not withdrawing the unlaunched %s: the row moved on", inc.ID, inc)
	case b != nil:
		maps.Copy(b.Metadata, inc.prior)
	}
	return err == nil && landed
}

// launchedUnder reports whether a failed start may have left a process
// running under inc: the runtime under sessName carries inc's token, or the
// read cannot say. A Start can fail after its launch (a readiness wait that
// timed out) with a cleanup that could not kill what it launched (tmux's
// is best-effort), so a mint is withdrawn only on positive evidence that no
// process carries it: the runtime is gone, or carries another token. No
// token is no evidence: t3bridge records none, tmux and subprocess have
// none before they seed it, and k8s reads none when its exec fails. Then a
// fresh read decides, as it decides a converge: a runtime up with its agent
// (processNames) alive may carry the mint, and one that is down, or a pane
// whose agent died, carries nothing. A mint no process carries is benign; a
// live process its row no longer records is not.
func (m *Manager) launchedUnder(sessName string, processNames []string, inc Incarnation) bool {
	tok, err := runtime.MetaValue(m.sp.GetMeta(sessName, runtime.InstanceTokenEnv))
	switch {
	case errors.Is(err, runtime.ErrSessionNotFound):
		return false
	case err != nil:
		return true
	}
	if tok = strings.TrimSpace(tok); tok != "" {
		return tok == inc.token
	}
	live, err := m.freshLiveness(sessName, processNames)
	return err != nil || (live.Running && live.Alive)
}

// startEnv is the runtime environment of a start of info's session under
// inc: the session's context, with every identity key inc's. The context is
// built without identity values: inc.Env() sets each of them at full width,
// so its int64 generation and epoch never narrow to RuntimeEnv's int.
func startEnv(info Info, inc Incarnation) map[string]string {
	return mergeEnv(RuntimeEnvWithSessionContext(info, 0, 0, ""), inc.Env())
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
	// ErrConversationRetained reports that a resume of a committed
	// conversation failed and the conversation was kept rather than replaced
	// by a fresh start (SESSION-LIFE-MANUAL-LOSS). The error also carries the
	// failed start's own error and names the explicit reset that starts the
	// session fresh.
	ErrConversationRetained = errors.New("retaining committed conversation")
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
	// approval or question and cannot accept a new user turn. It is the
	// runtime sentinel, so a refusal raised by the provider itself (for
	// example the tmux guard against typing into a permission prompt) matches
	// it too.
	ErrPendingInteraction = runtime.ErrPendingInteraction
	// ErrSessionKillPending reports that a `gc session kill` is tearing the
	// session's runtime down (see KillPendingReason). The caller should retry
	// once the kill completes and the lifecycle rules have taken over again.
	ErrSessionKillPending = errors.New("session is being killed")
	// ErrSessionStopping reports that the controller is stopping the
	// session's runtime (a drain-ack stop pending). The caller should retry
	// once the stop completes.
	ErrSessionStopping = errors.New("session is stopping, retry")
	// ErrTurnInterruptUnsupported reports that the session's runtime can
	// neither deliver interrupt keys nor interrupt the turn by protocol
	// (runtime.TurnInterruptUnsupported), so nothing was sent to stop it.
	ErrTurnInterruptUnsupported = errors.New("session runtime cannot interrupt the running turn")
)

// ErrSessionMutationBusy reports that a context-aware acquire of a session's
// mutation lock gave up while another holder still had it. fn never ran, so
// the caller may retry the whole operation. The returned error also wraps the
// context's own error.
var ErrSessionMutationBusy = errors.New("session mutation lock busy")

// sessionMutationLockEntry is one session's lock. sem is a channel of
// capacity one, full while the lock is held, so a waiter can select against a
// context; a sync.Mutex cannot be abandoned. refs counts holders and waiters
// and is guarded by sessionMutationLocksMu.
type sessionMutationLockEntry struct {
	sem  chan struct{}
	refs int
}

var (
	// sessionMutationLocksMu guards the sessionMutationLocks map and every
	// entry's refs. It is bookkeeping only and is never held across a
	// blocking operation.
	sessionMutationLocksMu sync.Mutex
	sessionMutationLocks   = map[string]*sessionMutationLockEntry{}
)

// WithSessionMutationLock serializes metadata mutations for one session bead.
// It waits for the lock without bound; use WithSessionMutationLockContext to
// bound the wait.
func WithSessionMutationLock(id string, fn func() error) error {
	return withSessionMutationLock(id, fn)
}

// WithSessionMutationLockContext is WithSessionMutationLock with a bounded
// wait. It tries the lock first, so a free lock is acquired even when ctx is
// already done; ctx bounds only the wait for a held lock, never fn. When ctx
// ends while the lock is still held it returns an error wrapping both
// ErrSessionMutationBusy and ctx.Err(), and fn has not run and never will.
func WithSessionMutationLockContext(ctx context.Context, id string, fn func() error) error {
	lock, err := acquireSessionMutationLockContext(ctx, id)
	if err != nil {
		return err
	}
	defer releaseSessionMutationLock(id, lock)
	return fn()
}

func withSessionMutationLock(id string, fn func() error) error {
	return WithSessionMutationLockContext(context.Background(), id, fn)
}

// tryWithSessionMutationLock runs fn under session id's mutation lock only
// when the lock is free, and reports whether fn ran. It never waits: it takes
// WithSessionMutationLockContext's try-first path with an already-done
// context, so while another holder has the lock it returns (false, nil) and fn
// does not run. An error fn returns itself is returned with ran true.
func tryWithSessionMutationLock(id string, fn func() error) (bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := WithSessionMutationLockContext(ctx, id, func() error {
		ran = true
		return fn()
	})
	if !ran && errors.Is(err, ErrSessionMutationBusy) {
		return false, nil
	}
	return ran, err
}

func acquireSessionMutationLockContext(ctx context.Context, id string) (*sessionMutationLockEntry, error) {
	sessionMutationLocksMu.Lock()
	lock := sessionMutationLocks[id]
	if lock == nil {
		lock = &sessionMutationLockEntry{sem: make(chan struct{}, 1)}
		sessionMutationLocks[id] = lock
	}
	lock.refs++
	sessionMutationLocksMu.Unlock()

	select {
	case lock.sem <- struct{}{}:
		return lock, nil
	default:
	}
	select {
	case lock.sem <- struct{}{}:
		return lock, nil
	case <-ctx.Done():
		dropSessionMutationLockRef(id, lock)
		return nil, fmt.Errorf("session %s: %w: %w", id, ErrSessionMutationBusy, ctx.Err())
	}
}

func releaseSessionMutationLock(id string, lock *sessionMutationLockEntry) {
	<-lock.sem
	dropSessionMutationLockRef(id, lock)
}

// dropSessionMutationLockRef gives up one reference without touching sem and
// deletes the entry when none remain.
func dropSessionMutationLockRef(id string, lock *sessionMutationLockEntry) {
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

// drainAckStopPendingMetadata reports whether a row's drain-ack stop is
// pending: the controller's stop owns its runtime (ErrSessionStopping).
func drainAckStopPendingMetadata(meta map[string]string) bool {
	return State(strings.TrimSpace(meta["state"])) == StateDraining && strings.TrimSpace(meta["state_reason"]) == DrainAckStopPendingReason
}

// ensureRunning starts or resumes session id's runtime for by, as by.Kind may
// (queueFor). replacing is an interrupt's restart of the runtime it just
// stopped: the hold was decided before that stop, so it is not re-read.
// startRefusal is ensureRunning's refusal of a row no actor may start or
// deliver to now, read from b alone.
func (m *Manager) startRefusal(by Actor, id string, b beads.Bead) error {
	if err := by.CheckStarts(); err != nil {
		return err
	}
	// A kill-fenced row reads asleep while its runtime is still being torn
	// down. Treating that runtime as live would flip the row back to active
	// (confirmLiveSessionState) and erase the fence, so once the Stop landed the
	// row would claim a live runtime that is gone; delivering into it would lose
	// the input with the process. Starting a replacement would race the kill.
	if KillPendingMetadata(b.Metadata["state"], b.Metadata["state_reason"], b.Metadata["sleep_reason"], b.Metadata["slept_at"], m.now()) {
		return fmt.Errorf("%w: %s", ErrSessionKillPending, id)
	}
	// Likewise a row whose drain-ack stop is pending: the controller's stop
	// owns its runtime, and a resume that flipped it back to active would
	// race that stop (CONTRACT v5.9a, as the kill fence). Every actor.
	if drainAckStopPendingMetadata(b.Metadata) {
		return fmt.Errorf("%w: %s", ErrSessionStopping, id)
	}
	return nil
}

func (m *Manager) ensureRunning(ctx context.Context, by Actor, id string, b beads.Bead, sessName, resumeCommand string, hints runtime.Config, replacing bool) error {
	return m.ensureRunningOn(ctx, by, id, b, sessName, resumeCommand, hints, replacing, m.sp.IsRunning(sessName))
}

// ensureRunningOn is ensureRunning with whether sessName's runtime runs
// already read: the provider's IsRunning for ensureRunning, a fresh read for
// a delivery (ensureDeliverable), so a corpse a stale cache still shows
// running is not taken for a live runtime to type into (J26 C2, mc-ir8qp).
func (m *Manager) ensureRunningOn(ctx context.Context, by Actor, id string, b beads.Bead, sessName, resumeCommand string, hints runtime.Config, replacing, running bool) error {
	// A start never runs under a caller's input fence: the fence names the
	// incarnation this start may replace (FI9).
	ctx = runtime.WithInputFence(ctx, runtime.InputFence{})
	if err := m.startRefusal(by, id, b); err != nil {
		return err
	}
	// Only an operator's own resume consumes an operator's hold (CONTRACT
	// v5.9 D8 rule 1); any other caller queues instead.
	if !replacing && m.queueForRunning(b.Metadata, running, by) {
		return fmt.Errorf("%w: %s", ErrResumeHeld, id)
	}
	transport, transportVerified := m.transportForBead(b, sessName)
	unroute := m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)
	if State(b.Metadata["state"]) != StateSuspended && running {
		return m.ensureLive(ctx, by, id, b, sessName, transport, transportVerified)
	}
	if resumeCommand == "" {
		return fmt.Errorf("%w: %s", ErrResumeRequired, id)
	}
	// The start runs under the runtime lease, taken before its first write
	// (withSessionStartLock reruns this on busy), or under its caller's, and
	// its calls stop once the lease lapses or moves.
	lease, release, err := m.leaseRuntime(ctx, by, id, sessName, 0)
	if err != nil {
		if unroute != nil {
			unroute()
		}
		return err
	}
	defer release()
	if lease != nil {
		var cancel context.CancelFunc
		if ctx, cancel, err = lease.Watch(ctx, runtimeLeaseWatchEvery); err != nil {
			return err
		}
		defer cancel()
	}
	// The start decides on the row as of the lease, not the read before it
	// (#7638 review): every writer of the kill fence or of drain-ack
	// stop-pending holds the runtime name's flock, which the lease holds
	// now, so a stop the controller signaled in between is seen here, before
	// anything starts, and the restart clear and the hold consume below read
	// this row.
	fresh, err := m.PersistedStore().freshBead(id)
	switch {
	case err != nil:
		err = fmt.Errorf("reading session %s under its lease: %w", id, err) // nothing started: no state sync to report
	case KillPendingMetadata(fresh.Metadata["state"], fresh.Metadata["state_reason"], fresh.Metadata["sleep_reason"], fresh.Metadata["slept_at"], m.now()):
		err = fmt.Errorf("%w: %s", ErrSessionKillPending, id)
	case drainAckStopPendingMetadata(fresh.Metadata):
		err = fmt.Errorf("%w: %s", ErrSessionStopping, id)
	}
	if err != nil {
		if unroute != nil {
			unroute()
		}
		return err
	}
	b = fresh
	// Under the lease, a runtime that is up with its agent alive is the
	// incarnation the row already records (or another start's that ran
	// before this one took the lease): a start never mints over it. Whether
	// it is up is a fresh read, never the provider's cache, which can read a
	// live runtime gone (the tmux state cache's TTL and degraded mode), and
	// a pane whose agent is dead is not alive: Start recycles it. The row,
	// suspended (a managed suspend whose drain has not stopped it yet) or
	// not, converges below without a mint or a Start, as a Start that found
	// the runtime up always has: its binding recorded, an operator's hold
	// consumed, the row confirmed unfenced. The converge takes that runtime
	// for the row's incarnation without reading its token, so one running
	// under an incarnation the row no longer records is confirmed too
	// (ga-9yerfq).
	// A read that cannot answer refuses the start (fail closed): nothing is
	// minted over a runtime that may be live.
	upAndAlive, err := m.leasedLiveness(id, sessName, hints.ProcessNames)
	if err != nil {
		if unroute != nil {
			unroute()
		}
		return err
	}

	cfg := hints
	cfg.Command = resumeCommand
	if cfg.WorkDir == "" {
		cfg.WorkDir = b.Metadata["work_dir"]
	}
	started := false
	converged := upAndAlive
	// inc is the start's incarnation, minted under the lease right before
	// the Start and provisional until a process runs under it: a start that
	// launches nothing withdraws it (unmintStart), so the row never records
	// an incarnation no process carries. A failed start withdraws it only on
	// evidence that no process survived under it (launchedUnder).
	var inc *Incarnation
	provisional := false
	defer func() {
		if provisional && !m.launchedUnder(sessName, hints.ProcessNames, *inc) {
			m.unmintStart(*inc, nil)
		}
	}()
	if !converged {
		// The start launches a new process, so it gets the generated launch
		// env its creator built, not only the hints the caller holds. One
		// that cannot be built refuses the start before anything is killed
		// or minted.
		if m.launchEnvResolver != nil {
			launchEnv, err := m.launchEnvResolver(infoFromPersistedBead(b), maps.Clone(b.Metadata))
			if err != nil {
				if unroute != nil {
					unroute()
				}
				return fmt.Errorf("session %s: building launch env: %w", id, err)
			}
			cfg.Env = mergeEnv(cfg.Env, launchEnv)
		}
		// Refuse to resume if a prior escaped process for this session could
		// not be confirmed dead: a survivor would race this replacement for
		// the same work bead (duplicate bd close). This is the
		// stable/reused-bead-ID path — the exact "old process survives
		// alongside its replacement" scenario. No fresh-create to roll back,
		// so unroute and propagate before Start.
		if orphanErr := m.killExistingOrphans(ctx, id); orphanErr != nil {
			if unroute != nil {
				unroute()
			}
			return fmt.Errorf("pre-start orphan cleanup: %w", orphanErr)
		}
		minted, err := m.mintStart(ctx, lease, by.Kind, &b, strings.TrimSpace(b.Metadata["continuation_reset_pending"]) != "")
		if err != nil {
			if unroute != nil {
				unroute()
			}
			return err
		}
		inc, provisional = &minted, true
		cfg = startConfig(cfg, b, minted)
		if err := m.sp.Start(ctx, sessName, cfg); err != nil {
			switch {
			// A capacity refusal is also a startup death, but the endpoint
			// refused the launch: that says nothing about the resume key, so
			// it falls through to the plain failure below instead of the
			// stale-key recovery.
			case errors.Is(err, runtime.ErrSessionDiedDuringStartup) && !runtime.IsProviderCapacity(err):
				retried, retryErr := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, err, unroute, lease, inc)
				if retryErr != nil {
					return retryErr
				}
				if !retried {
					// The recovery declined: the command carries no resume shape
					// to strip, so a relaunch would repeat this failure
					// verbatim. Propagate the original start error rather than
					// reporting a start that never happened.
					if unroute != nil {
						unroute()
					}
					return startFailure(resumeCommand, b.Metadata, err)
				}
				started = retried
			case errors.Is(err, runtime.ErrSessionExists) && m.sp.IsRunning(sessName):
				// The runtime came up past the fresh read (a start that took
				// no lease, or a resume of ours that returned before
				// confirming it: the canceled stale-key wait below). The
				// resume converged on it: the mint is withdrawn, and active
				// state is persisted below with the binding of that runtime.
				if !m.unmintStart(*inc, &b) {
					provisional = false
					// The route stays: the runtime it routes to is up.
					return fmt.Errorf("%w: session %s converged on a runtime already up, but the row still records %s", ErrStateSync, id, *inc)
				}
				inc, provisional, converged = nil, false, true
			default:
				if unroute != nil {
					unroute()
				}
				return startFailure(resumeCommand, b.Metadata, err)
			}
		} else {
			started = true
		}
		if started {
			provisional = false // a process runs under inc
		}
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
			// sees the suspended-state bead, reads the runtime up, and
			// persists "active" with the runtime's binding. The mint stands:
			// the process launched carries it.
			if unroute != nil {
				unroute()
			}
			return err
		}
		if !m.sp.IsRunning(sessName) {
			retried, err := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, nil, unroute, lease, inc)
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
	var binding MetadataPatch
	if started || converged {
		binding = m.startedRuntimeBindingPatch(sessName)
	}
	// An operator's resume of a row the operator holds consumes the hold, but
	// only once the runtime is up (resume_user_hold.go). It decides on the row
	// as this start read it under the lease, with the incarnation it minted
	// last (a stale-key relaunch mints again) or, converged, the one the row
	// records, so only another's start since refuses the consume.
	var consume Decided
	consumes := false
	if by.consumesHold() {
		consume, consumes = userHoldDecision(b)
	}
	if consumes {
		clears := MetadataPatch{}
		if started {
			clears = restartStopRequestClears(b.Metadata)
		}
		maps.Copy(clears, binding)
		return m.consumeUserHold(consume, m.now(), clears)
	}
	// A start confirms its own incarnation (fenced); a converged resume,
	// which minted nothing, confirms the row as it is.
	return m.confirmLiveSessionState(id, &b, started, binding, inc)
}

// ensureLive is ensureRunningOn's tail for a row not suspended whose runtime
// its caller read running before any lease: nothing starts and nothing is
// minted. It records a verified transport, consumes the operator's hold of a
// live row the operator holds (a suspend whose drain has not stopped it yet)
// under the runtime lease (tried, not waited for: a busy lease is the
// controller's stop deciding, and ErrRuntimeLeaseBusy is retryable), and
// confirms the row active.
func (m *Manager) ensureLive(ctx context.Context, by Actor, id string, b beads.Bead, sessName, transport string, transportVerified bool) error {
	if b.Metadata["transport"] == "" && transportVerified {
		m.persistTransport(id, b.Metadata["provider"], transport)
	}
	if consume, ok := liveUserHoldDecision(b, by); ok {
		_, release, err := m.leaseRuntime(ctx, by, id, sessName, 0)
		if err != nil {
			return err
		}
		defer release()
		return m.consumeUserHold(consume, m.now(), nil)
	}
	return m.confirmLiveSessionState(id, &b, false, nil, nil)
}

// startConfig is cfg for row b's start under inc: its identity env inc's,
// the provider family, SSH keepalives and the work dir env.
func startConfig(cfg runtime.Config, b beads.Bead, inc Incarnation) runtime.Config {
	cfg.Env = mergeEnv(cfg.Env, startEnv(infoFromPersistedBead(b), inc))
	if gcProvider := providerKind(b); gcProvider != "" {
		cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": gcProvider})
	}
	cfg.Env = git.ApplySSHKeepaliveEnv(cfg.Env)
	return runtime.SyncWorkDirEnv(cfg)
}

// ensureDeliverable is ensureRunning for a delivery. Past ensureRunning's
// refusals (a kill-fenced or stopping row still refuses), it decides on a
// fresh read of an unsuspended row's runtime: a live pane whose agent is
// dead, or a runtime the read cannot vouch for, is ErrAgentDead, so the
// caller queues the text rather than type it into a pane it would be lost
// with (mc-48az4, mc-wdjni). Whether the runtime runs is that read's too
// (ensureRunningOn), never a caching provider's IsRunning. Start and
// Attach keep ensureRunning's: an operator attaches to a crashed pane to read
// it.
func (m *Manager) ensureDeliverable(ctx context.Context, by Actor, id string, b beads.Bead, sessName, resumeCommand string, hints runtime.Config) error {
	if err := m.startRefusal(by, id, b); err != nil {
		return err
	}
	if State(b.Metadata["state"]) == StateSuspended {
		return m.ensureRunning(ctx, by, id, b, sessName, resumeCommand, hints, false)
	}
	live, err := m.freshLiveness(sessName, hints.ProcessNames)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %s: %w", ErrAgentDead, id, err)
	case live.Running && !live.Alive:
		return fmt.Errorf("%w: %s", ErrAgentDead, id)
	}
	// The fresh read, not the provider's cache, says whether the runtime
	// runs: a corpse reads not running whether or not the read refreshed a
	// caching provider (J26 C2, mc-ir8qp).
	return m.ensureRunningOn(ctx, by, id, b, sessName, resumeCommand, hints, false, live.Running)
}

// freshLiveness reads sessName's runtime as of now, not from a provider's
// cache: the tmux state cache can show a crashed agent alive for seconds,
// and a crash never invalidates it (mc-48az4). Alive is by processNames;
// with none (and none the runtime published) a running pane reads alive.
func (m *Manager) freshLiveness(sessName string, processNames []string) (runtime.Liveness, error) {
	return runtime.ObserveLivenessSince(m.sp, sessName, processNames, time.Now())
}

// leasedLiveness is the fresh read (freshLiveness) a start of row id decides
// on under its lease: whether sessName's runtime is up with its agent alive.
// A read that cannot answer is runtime.ErrRuntimeUnavailable, so the start
// refuses (fail closed) rather than launch over a runtime that may be live.
func (m *Manager) leasedLiveness(id, sessName string, processNames []string) (bool, error) {
	live, err := m.freshLiveness(sessName, processNames)
	if err != nil {
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			err = fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, err)
		}
		return false, fmt.Errorf("session %s: reading its runtime under the lease: %w", id, err)
	}
	return live.Running && live.Alive, nil
}

// liveAgent reports a fresh read of sessName that finds its pane up and its
// agent alive, which a live-only send needs.
func (m *Manager) liveAgent(sessName string, processNames []string) bool {
	live, err := m.freshLiveness(sessName, processNames)
	return err == nil && live.Running && live.Alive
}

func (m *Manager) ensureRunningRuntimeOnly(ctx context.Context, by Actor, id string, inc Incarnation, b beads.Bead, sessName, resumeCommand string, hints runtime.Config) error {
	// It runs under its caller's lease (by.Lease) and takes none: without
	// one it refuses rather than start outside the lease.
	if by.Lease == nil {
		return fmt.Errorf("%w: session %q", ErrNoCallerLease, id)
	}
	if err := checkBorrowed(by.Lease, id, sessName); err != nil {
		return err
	}
	// It starts under the incarnation its caller's PreWake minted, while the
	// row still records it. A runtime already up starts nothing and is left
	// to the caller's start commit, which refuses a row that no longer
	// records inc (CommitStartedIfCurrentUnder).
	if !inc.Valid() || inc.ID != id {
		return fmt.Errorf("%w: session %q (%s)", ErrIncarnationNotMinted, id, inc)
	}
	transport, _ := m.transportForBead(b, sessName)
	unroute := m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)
	if m.sp.IsRunning(sessName) {
		return nil
	}
	if resumeCommand == "" {
		return fmt.Errorf("%w: %s", ErrResumeRequired, id)
	}

	if !inc.recordedBy(b.Metadata) {
		return fmt.Errorf("%w: session %s no longer records %s", ErrMintSuperseded, id, inc)
	}

	cfg := hints
	cfg.Command = resumeCommand
	if cfg.WorkDir == "" {
		cfg.WorkDir = b.Metadata["work_dir"]
	}
	// A continuation reset recorded since the PreWake (the prepared start's
	// stale-resume clear) is consumed here, as legacy always has.
	continuationEpoch, err := m.commitPendingContinuationReset(id, b)
	if err != nil {
		return err
	}
	inc = inc.withContinuationEpoch(continuationEpoch)
	cfg.Env = mergeEnv(cfg.Env, startEnv(infoFromPersistedBead(b), inc))
	if gcProvider := strings.TrimSpace(b.Metadata["provider_kind"]); gcProvider != "" {
		cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": gcProvider})
	} else if provider := strings.TrimSpace(b.Metadata["provider"]); provider != "" {
		cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": provider})
	}
	cfg.Env = git.ApplySSHKeepaliveEnv(cfg.Env)
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
		// A capacity refusal says nothing about the resume key; it takes the
		// plain failure path, not the stale-key recovery (see ensureRunning).
		case errors.Is(err, runtime.ErrSessionDiedDuringStartup) && !runtime.IsProviderCapacity(err):
			retried, retryErr := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, err, unroute, nil, nil)
			if retryErr != nil {
				return retryErr
			}
			if !retried {
				// The recovery declined: nothing to strip, so a relaunch would
				// repeat this failure verbatim. Propagate the original error.
				if unroute != nil {
					unroute()
				}
				return startFailure(resumeCommand, b.Metadata, err)
			}
			started = retried
		case errors.Is(err, runtime.ErrSessionExists) && m.sp.IsRunning(sessName):
			return err
		default:
			if unroute != nil {
				unroute()
			}
			return startFailure(resumeCommand, b.Metadata, err)
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
			if _, err := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, nil, unroute, nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// confirmLiveSessionState marks the row of a session found or brought live as
// active. extra rides the same write (the runtime binding of a start, or of a
// resume that converged on a runtime already up). A start's confirm (inc, the
// incarnation it minted) lands only while the row still records inc
// (ConfirmIncarnation); a row that moved on is ErrStateSync wrapping
// ErrMintSuperseded, and the runtime is left to its newer owner. A live row
// found running, which minted nothing, is confirmed without a fence.
func (m *Manager) confirmLiveSessionState(id string, b *beads.Bead, started bool, extra MetadataPatch, inc *Incarnation) error {
	if b == nil {
		return nil
	}
	batch := make(map[string]string)
	if started {
		maps.Copy(batch, restartStopRequestClears(b.Metadata))
	}
	maps.Copy(batch, extra)
	switch State(b.Metadata["state"]) {
	case "", StateStartPending, StateCreating, StateAsleep, StateSuspended:
		batch["state"] = string(StateActive)
		batch["state_reason"] = "creation_complete"
	}
	if strings.TrimSpace(b.Metadata["pending_create_claim"]) != "" {
		batch["pending_create_claim"] = ""
		batch["pending_create_started_at"] = ""
	}
	if len(batch) == 0 && inc == nil {
		return nil
	}
	if inc == nil {
		if err := m.store.SetMetadataBatch(id, batch); err != nil {
			return fmt.Errorf("%w: updating session state: %w", ErrStateSync, err)
		}
	} else if landed, err := m.PersistedStore().ConfirmIncarnation(*inc, batch); err != nil {
		return fmt.Errorf("%w: updating session state: %w", ErrStateSync, err)
	} else if !landed {
		return fmt.Errorf("%w: session %s no longer records %s: %w", ErrStateSync, id, *inc, ErrMintSuperseded)
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
	err := m.nudgeContent(ctx, sessName, content, immediate)
	if errors.Is(err, runtime.ErrNudgeDeliveredUnobserved) {
		// The provider proved delivery and only missed the turn start, so
		// this nudge is delivered: a caller that retried it would deliver
		// the message twice.
		log.Printf("session: %q: %v", sessName, err)
		err = nil
	}
	recordCtx := ctx
	if recordCtx == nil || recordCtx.Err() != nil {
		recordCtx = context.Background()
	}
	telemetry.RecordNudge(recordCtx, sessName, err)
	if errors.Is(err, runtime.ErrNudgeRefusedPendingInteraction) {
		return fmt.Errorf("%w: %w", ErrPendingInteraction, err)
	}
	if err != nil {
		return fmt.Errorf("sending message to session: %w", err)
	}
	return nil
}

// nudgeContent delivers content, passing the delivery key ctx carries (see
// runtime.WithNudgeDeliveryKey) to a provider that deduplicates keyed
// deliveries, immediate delivery included: such a provider's keyed send
// starts the turn without an idle wait (runtime.NudgeWithReceipt). Only an
// unkeyed immediate delivery, or one to a provider that keeps no keyed
// deliveries, goes out through NudgeNow, which takes no key.
//
// Under an input fence in ctx (runtime.WithInputFence) the runtime delivers
// only to the incarnation it names, or refuses.
func (m *Manager) nudgeContent(ctx context.Context, sessName string, content []runtime.ContentBlock, immediate bool) error {
	return runtime.NudgeWithContext(ctx, m.sp, sessName, content, immediate)
}

func normalizeWaitIdleNudgeSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "session"
	}
	return source
}

func (m *Manager) tryWaitIdleNudgeLocked(ctx context.Context, by Actor, id string, b beads.Bead, source, sessName, message, resumeCommand string, hints runtime.Config) (bool, error) {
	if err := m.ensureDeliverable(ctx, by, id, b, sessName, resumeCommand, hints); errors.Is(err, ErrAgentDead) {
		return false, nil // not delivered: the caller queues it for the healed agent
	} else if err != nil {
		return false, err
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

func (m *Manager) tryWaitIdleNudgeLiveOnlyLocked(ctx context.Context, b beads.Bead, source, sessName, message string, processNames []string) (bool, error) {
	if !m.liveAgent(sessName, processNames) { // a dead agent's pane takes nothing (mc-wdjni)
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
			return PendingInteractionError(pending)
		}
	}
	return nil
}

// PendingInteractionError returns an error wrapping ErrPendingInteraction that
// names the pending request, so callers can tell the operator what must be
// answered before text can be delivered: a permission prompt is answered with
// Respond, a question in the terminal.
func PendingInteractionError(pending *runtime.PendingInteraction) error {
	what := strings.TrimSpace(pending.Kind)
	if what == "" {
		what = "interaction"
	}
	if prompt := strings.TrimSpace(pending.Prompt); prompt != "" {
		what += " " + strconv.Quote(prompt)
	}
	if pending.RequestID != "" {
		what += " (request " + pending.RequestID + ")"
	}
	if strings.TrimSpace(pending.Kind) == "question" {
		// A question dialog offers no response verbs: only a person answers
		// it, in the terminal.
		return fmt.Errorf("%w: %s is waiting for an answer in the terminal (gc session attach <session>); text sent now would select one of its options", ErrPendingInteraction, what)
	}
	return fmt.Errorf("%w: %s is waiting for an answer; respond to it (approve or deny) before sending text", ErrPendingInteraction, what)
}

func (m *Manager) dismissKnownDialogsLocked(ctx context.Context, sessName string, timeout time.Duration) bool {
	dp, ok := m.sp.(runtime.DialogProvider)
	if !ok {
		return false
	}
	switch err := dp.DismissKnownDialogs(ctx, sessName, timeout); {
	case errors.Is(err, runtime.ErrInteractionUnsupported):
		// A composite whose backend for this session cannot dismiss dialogs.
		return false
	case errors.Is(err, runtime.ErrWorkspaceTrustUnconfirmed):
		log.Printf("session: %q: %v", sessName, err)
	}
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

// sendLocked delivers message, starting or resuming the runtime as by
// allows; a row it may not resume, or whose agent is dead in a pane still
// up, gets the message queued (reports true). So does a live session the
// runtime would not type into because it is running a turn the submit keys
// would interrupt (runtime.ErrSessionBusy): the queue delivers it later.
func (m *Manager) sendLocked(ctx context.Context, by Actor, id string, b beads.Bead, sessName, message, resumeCommand string, hints runtime.Config, immediate bool, queueSource string) (bool, error) {
	if err := m.ensureDeliverable(ctx, by, id, b, sessName, resumeCommand, hints); err != nil {
		if !errors.Is(err, ErrResumeHeld) && !errors.Is(err, ErrAgentDead) {
			return false, err
		}
		return true, m.enqueueDeferredSubmitLocked(b, sessName, message, queueSource)
	}
	verifyDeferredDialogs := needsDeferredStartupDialogVerification(b)
	if verifyDeferredDialogs {
		m.dismissKnownDialogsLocked(ctx, sessName, codexDeferredDialogDelay)
	}
	if err := m.pendingInteractionLocked(sessName); err != nil {
		return false, err
	}
	if err := m.nudgeSession(ctx, sessName, message, immediate); err != nil {
		if errors.Is(err, runtime.ErrSessionBusy) {
			return true, m.enqueueDeferredSubmitLocked(b, sessName, message, queueSource)
		}
		return false, err
	}
	if verifyDeferredDialogs && m.dismissKnownDialogsLocked(ctx, sessName, codexDeferredDialogDelay) {
		m.markStartupDialogsVerifiedLocked(id, &b)
	}
	return false, nil
}

func (m *Manager) send(ctx context.Context, by Actor, id, message, resumeCommand string, hints runtime.Config, immediate bool) (SubmitOutcome, error) {
	if err := m.checkStarter(by); err != nil {
		return SubmitOutcome{}, err
	}
	var outcome SubmitOutcome
	err := withSessionStartLock(ctx, id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		outcome.Queued, err = m.sendLocked(ctx, by, id, b, sessName, message, resumeCommand, hints, immediate, deferredNudgeSource)
		outcome.Deferred = outcome.Queued
		return err
	})
	return outcome, err
}

func (m *Manager) sendLiveOnly(ctx context.Context, id, message string, immediate bool, processNames []string) (bool, error) {
	var delivered bool
	err := withSessionMutationLock(id, func() error {
		_, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		live, err := m.freshLiveness(sessName, processNames)
		if err != nil || !live.Running || !live.Alive { // a dead agent's pane takes nothing (mc-wdjni)
			delivered = false
			return runtime.LivenessFenceMiss(ctx, sessName, live, err)
		}
		if err := m.pendingInteractionLocked(sessName); err != nil {
			return err
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
// other callers that need bounded startup without attaching a terminal. A
// dormant row it may not resume as its actor returns ErrResumeHeld.
func (m *Manager) Start(ctx context.Context, by Actor, id, resumeCommand string, hints runtime.Config) error {
	if err := m.checkStarter(by); err != nil {
		return err
	}
	return withSessionStartLock(ctx, id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		return m.ensureRunning(ctx, by, id, b, sessName, resumeCommand, hints, false)
	})
}

// StartRuntimeOnly brings the runtime live for a bead-backed session without
// mutating persisted lifecycle metadata. Legacy reconciler callers use this
// bridge while they still own commit/rollback bookkeeping above the worker
// boundary. It runs under by.Lease, the caller's, and under inc, the
// incarnation the caller's PreWake minted under that lease
// (MintIncarnation).
func (m *Manager) StartRuntimeOnly(ctx context.Context, by Actor, id string, inc Incarnation, resumeCommand string, hints runtime.Config) error {
	if err := m.checkStarter(by); err != nil {
		return err
	}
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		return m.ensureRunningRuntimeOnly(ctx, by, id, inc, b, sessName, resumeCommand, hints)
	})
}

// Send starts or resumes the session as its actor may, then nudges the
// runtime with a new user message, or queues it (SubmitOutcome.Queued).
func (m *Manager) Send(ctx context.Context, by Actor, id, message, resumeCommand string, hints runtime.Config) (SubmitOutcome, error) {
	return m.send(ctx, by, id, message, resumeCommand, hints, false)
}

// SendImmediate is Send, injecting the new user message without waiting for
// an idle boundary when the runtime supports immediate nudges. Falls back to
// Send semantics on runtimes without the optional immediate nudge capability.
func (m *Manager) SendImmediate(ctx context.Context, by Actor, id, message, resumeCommand string, hints runtime.Config) (SubmitOutcome, error) {
	return m.send(ctx, by, id, message, resumeCommand, hints, true)
}

// SendLiveOnly nudges the runtime only when the current session is already
// running with its agent alive (by processNames; none reads alive). It never
// resumes or restarts the session. Under an input fence in ctx, a session it
// finds not running, or with its agent exited, is a miss rather than
// undelivered, and a liveness read that fails is runtime.ErrRuntimeUnavailable
// (runtime.LivenessFenceMiss).
func (m *Manager) SendLiveOnly(ctx context.Context, id, message string, processNames []string) (bool, error) {
	return m.sendLiveOnly(ctx, id, message, false, processNames)
}

// SendImmediateLiveOnly is like SendLiveOnly but uses the immediate nudge path
// when the runtime supports it. It never resumes or restarts the session.
func (m *Manager) SendImmediateLiveOnly(ctx context.Context, id, message string, processNames []string) (bool, error) {
	return m.sendLiveOnly(ctx, id, message, true, processNames)
}

// TryWaitIdleNudge delivers a best-effort session nudge at a provider-defined
// safe boundary. It resumes supported runtimes if needed, then reports whether
// live delivery actually happened. Unsupported providers return (false, nil)
// so higher layers can fall back to queue semantics without treating that as
// an operational error. A held row the actor may not resume returns
// ErrResumeHeld, queueing nothing, so the caller queues it and says why.
func (m *Manager) TryWaitIdleNudge(ctx context.Context, by Actor, id, source, message, resumeCommand string, hints runtime.Config) (bool, error) {
	if err := m.checkStarter(by); err != nil {
		return false, err
	}
	var delivered bool
	err := withSessionStartLock(ctx, id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		delivered, err = m.tryWaitIdleNudgeLocked(ctx, by, id, b, source, sessName, message, resumeCommand, hints)
		return err
	})
	return delivered, err
}

// TryWaitIdleNudgeLiveOnly delivers a best-effort nudge at a safe boundary
// only when the runtime is already live with its agent alive (by
// processNames; none reads alive). It never resumes or restarts the session.
func (m *Manager) TryWaitIdleNudgeLiveOnly(ctx context.Context, id, source, message string, processNames []string) (bool, error) {
	var delivered bool
	err := withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		delivered, err = m.tryWaitIdleNudgeLiveOnlyLocked(ctx, b, source, sessName, message, processNames)
		return err
	})
	return delivered, err
}

// StopTurn issues a provider-appropriate interrupt for the currently running
// turn. For providers that need post-interrupt idle settlement (e.g. Claude),
// it waits for the session to return to an idle prompt before returning.
// Claude Code puts a prompt interrupted before its first response chunk back
// into its input box, so for Claude StopTurn also clears the input box when
// the runtime can do a verified clear (see clearRestoredInputLocked). Codex
// keeps running an interrupted turn's commands in background terminals, and
// Claude a command the model ran in the background, so for both StopTurn also
// has the runtime stop them once the interrupt boundary is reached and any
// restored prompt is cleared (see stopBackgroundWorkLocked). That stops ALL
// of the session's background work, including what earlier turns started: a
// stop stops the session's work. An interrupt_now submit does not do this.
// A runtime that interrupts turns by protocol (runtime.TurnInterruptProtocol)
// gets its Interrupt for every provider and no keys. A runtime that can
// interrupt neither way gets nothing sent and StopTurn returns
// ErrTurnInterruptUnsupported. A wait after the interrupt that fails is an
// error wrapping runtime.ErrTurnNotEnded: the interrupt was sent, and the
// turn was not seen to settle. A stop that finds the runtime gone when it
// comes to send sends nothing and waits for nothing.
func (m *Manager) StopTurn(id string) error {
	return m.StopTurnIf(context.Background(), id, nil)
}

// StopTurnIf is StopTurn with a check (X11 §5.2, SESSION-RUNTIME-023). Once
// the stop has found a turn to interrupt, and before it sends anything,
// still runs under the session mutation lock; an error from it is returned,
// wrapped, and nothing is sent. Every stop, submit and respond of the session
// in this process takes that lock, so none of them acts between the check
// and the keys; a turn the agent starts on its own, or input from another
// process, can still land in that window (the checked tier, X11 I3). still
// runs under that lock, which is not reentrant: it must not take the
// session mutation lock itself, or it waits forever. A stop that finds no
// turn (a suspended row, a runtime that is not running) returns as StopTurn
// does without calling still, as does one whose row, loaded under the lock,
// is closed or will not load: the load's error, wrapped with ErrStopNotSent,
// nothing sent. A nil still is StopTurn.
//
// ctx bounds the waits after the interrupt, and an input fence it carries
// (runtime.WithInputFence) fences every key, as StopTurnFenced does. With a
// check or a ctx deadline, ctx also bounds the wait for the session mutation
// lock: a lock still held when ctx ends is an error wrapping
// runtime.ErrRuntimeUnavailable and ErrSessionMutationBusy, nothing sent.
// With a check, whether the runtime runs, before still and
// again before the first key, is a fresh liveness read's, as under a fence,
// never the provider's IsRunning, which the tmux state cache can answer
// stale: a read that does not find the agent alive is no turn, and one that
// cannot answer an error wrapping runtime.ErrRuntimeUnavailable, nothing
// sent. A checked stop that finds the turn gone when it comes to send, after
// still passed, sends nothing and returns an error wrapping
// runtime.ErrSessionNotFound (or, under an input fence, the miss); a send
// that fails after still passed wraps ErrInterruptOutcomeUnknown and
// runtime.ErrRuntimeUnavailable: it is unknown whether the interrupt reached
// the agent. A follow-up interrupt of the keystroke sequence (a soft-escape
// family's Ctrl-C fallback) goes only once still passes again: it interrupts
// whatever turn runs when it lands. A refusal then sends nothing more. One
// saying the turn is no longer current (runtime.ErrTurnNotCurrent) has seen
// the turn end, and the stop returns as when its waits pass; any other makes
// it return an error wrapping runtime.ErrTurnNotEnded and none of the
// refusal's sentinels, since the first key was sent.
func (m *Manager) StopTurnIf(ctx context.Context, id string, still func() error) error {
	_, err := m.StopTurnWith(ctx, id, StopTurnOptions{Still: still})
	return err
}

// TurnInterruptSettle bounds the wait for an interrupted turn to end, from
// the moment the interrupt was decided (DES-T3I D6).
const TurnInterruptSettle = turnSettleTimeout

// InterruptTurn interrupts session id's turn turnID by protocol and waits for
// it to end (X11 §5.2, SESSION-RUNTIME-023): InterruptTurnOn the session's
// runtime, under the session mutation lock, so no other stop, submit or
// respond of the session in this process acts between the runtime's fence
// read and the wait. ctx bounds the wait for that lock: a lock still held
// when ctx ends is an error wrapping runtime.ErrRuntimeUnavailable, nothing
// sent. Nothing is sent when it refuses: a runtime that cannot interrupt the
// session's turn (runtime.TurnInterruptUnsupported) is
// ErrTurnInterruptUnsupported, one with no turn fence
// (runtime.TurnFenceProvider) runtime.ErrTurnIdentityUnsupported, and a
// suspended row, which has no turn, a *runtime.TurnNotCurrentError. The row
// is loaded under the lock, so a row that is closed by then, or will not
// load, returns an error wrapping ErrStopNotSent and the load's error
// (ErrSessionClosed for a closed one), nothing sent, as StopTurnWith's does.
// Otherwise it returns what InterruptTurnOn returns.
//
// restart, when set, is a restart on failure, run as StopTurnWith runs
// StopTurnOptions.Restart: an actor that may not start the runtime is
// refused before anything else (ErrStopNotSent and the refusal), and a row
// whose drain-ack stop is pending with ErrSessionStopping, nothing sent. On
// a keystroke runtime (ACP, whose stop replaces the agent) it takes the
// runtime lease under the lock before the dispatch (a lease it cannot take
// is runtime.ErrRuntimeUnavailable, nothing sent), and when the interrupted
// turn is not seen to end it stops the runtime and starts it again in its
// place under that lease, reporting StopTurnReport.Restarted with a nil
// error; a restart whose stop or start fails is the error StopTurnWith's
// is, and a caller whose ctx ended by then, or a turn whose wait read
// nothing (TurnEndUnreadError), gets no restart (restartWithheld). A
// protocol runtime is never restarted, as its stop may leave it running: its
// not-ended error is returned.
func (m *Manager) InterruptTurn(ctx context.Context, id, turnID string, restart *StopTurnRestart) (runtime.TurnInterrupt, StopTurnReport, error) {
	var report StopTurnReport
	if restart != nil {
		if err := m.checkStarter(restart.By); err != nil {
			return runtime.TurnInterrupt{}, report, fmt.Errorf("interrupting turn %q of %s, restarting it on failure: %w: %w", turnID, id, ErrStopNotSent, err)
		}
	}
	var ack runtime.TurnInterrupt
	err := WithSessionMutationLockContext(ctx, id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return fmt.Errorf("interrupting turn %q of %s: %w: %w", turnID, id, ErrStopNotSent, err)
		}
		if restart != nil && drainAckStopPendingMetadata(b.Metadata) {
			// The row belongs to the controller's stop, as for StopTurnWith.
			return fmt.Errorf("interrupting turn %q of %s, restarting it on failure: %w", turnID, id, ErrSessionStopping)
		}
		mode := runtime.TurnInterruptModeFor(m.sp, sessName)
		if mode == runtime.TurnInterruptUnsupported {
			return fmt.Errorf("interrupting turn %q of %s: %w", turnID, id, ErrTurnInterruptUnsupported)
		}
		tf, ok := m.sp.(runtime.TurnFenceProvider)
		if !ok {
			return fmt.Errorf("interrupting turn %q of %s: %w", turnID, id, runtime.ErrTurnIdentityUnsupported)
		}
		if State(b.Metadata["state"]) == StateSuspended {
			return &runtime.TurnNotCurrentError{Session: sessName, TurnID: turnID}
		}
		restarts := restart != nil && mode == runtime.TurnInterruptKeystroke
		var by Actor
		if restarts {
			// The lease goes before the dispatch and to the restart, as
			// StopTurnWith's does (D8D-5).
			leased, release, err := m.leaseForRestartLocked(ctx, restart.By, id, sessName)
			if err != nil {
				return fmt.Errorf("interrupting turn %q of %s: taking the runtime lease for its restart on failure, nothing sent: %w: %w", turnID, id, runtime.ErrRuntimeUnavailable, err)
			}
			defer release()
			by = leased
		}
		ack, err = InterruptTurnOn(ctx, tf, sessName, turnID)
		if !restarts || !errors.Is(err, runtime.ErrTurnNotEnded) {
			return err
		}
		if err := restartWithheld(ctx, fmt.Sprintf("interrupting turn %q of %s", turnID, id), nil, err); err != nil {
			return err
		}
		if err := m.restartAfterFailedInterruptLocked(ctx, by, id, b, sessName, restart); err != nil {
			return err
		}
		report.Restarted = true
		return nil
	})
	if errors.Is(err, ErrSessionMutationBusy) {
		return ack, report, fmt.Errorf("interrupting turn %q of %s: %w: %w", turnID, id, runtime.ErrRuntimeUnavailable, err)
	}
	return ack, report, err
}

// InterruptTurnOn interrupts sessName's turn turnID through tf and waits for
// it to end: tf.InterruptTurn, its read and dispatch bounded at 10 s, then
// tf.WaitTurnEnded, bounded at 15 s. It returns the runtime's
// acknowledgement: Fenced false means the runtime interrupted whatever turn
// ran when it handled the request (the checked tier, X11 I3). The error is:
//   - nil: the runtime read turnID no longer in progress within the bound,
//     a session it no longer hosts included (WaitTurnEnded's rule);
//   - a *runtime.TurnNotCurrentError (runtime.ErrTurnNotCurrent): turnID was
//     not in progress, or the runtime no longer hosts sessName (a bare
//     runtime.ErrSessionNotFound from the read); nothing was sent;
//   - runtime.ErrTurnIdentityUnsupported: a composite routes sessName to a
//     backend without the fence; nothing was sent;
//   - runtime.ErrTurnUnnamed: turnID is empty; nothing was sent;
//   - an error wrapping runtime.ErrTurnNotEnded: sent, but not seen to end.
//     It wraps no other sentinel: the wait's own error is in its text only,
//     so ErrTurnNotEnded, "sent", is never misread as a refusal. A wait
//     whose error wraps runtime.ErrRuntimeUnavailable read nothing (no read
//     answered, or the runtime cannot answer the wait at all), so its error
//     is a TurnEndUnreadError, which a restart on failure does not restart
//     (Manager.InterruptTurn);
//   - an error wrapping runtime.ErrRuntimeUnavailable and not
//     ErrInterruptOutcomeUnknown: ctx ended before the dispatch, and nothing
//     was sent;
//   - an error wrapping ErrInterruptOutcomeUnknown and
//     runtime.ErrRuntimeUnavailable: the read or the dispatch failed, and it
//     is unknown whether the request reached the runtime, as
//     TurnFenceProvider.InterruptTurn has every error outside the classes
//     above. The runtime's own runtime.ErrRuntimeUnavailable is wrapped so,
//     as is a runtime error in none of the classes above, a bare ctx error
//     included. A runtime gone after a failed attempt (an error wrapping
//     both runtime.ErrRuntimeUnavailable and runtime.ErrSessionNotFound) is
//     unknown and only that: its not-found is in the text only, so a caller
//     that tests not-found first never reads "maybe sent" as "nothing
//     sent".
func InterruptTurnOn(ctx context.Context, tf runtime.TurnFenceProvider, sessName, turnID string) (runtime.TurnInterrupt, error) {
	if err := ctx.Err(); err != nil {
		return runtime.TurnInterrupt{}, fmt.Errorf("interrupting turn %q of %s: nothing sent: %w: %w", turnID, sessName, runtime.ErrRuntimeUnavailable, err)
	}
	ack, err := interruptTurn(ctx, tf, sessName, turnID)
	switch {
	case errors.Is(err, runtime.ErrInteractionUnsupported):
		return runtime.TurnInterrupt{}, fmt.Errorf("interrupting turn %q of %s: %w: %w", turnID, sessName, runtime.ErrTurnIdentityUnsupported, err)
	case errors.Is(err, runtime.ErrSessionNotFound) && !errors.Is(err, runtime.ErrRuntimeUnavailable):
		// Gone before any dispatch: no turn runs (SESSION-RUNTIME-020's
		// rule). Gone after a failed attempt (both sentinels) may have
		// reached the runtime: unknown, below.
		return runtime.TurnInterrupt{}, &runtime.TurnNotCurrentError{Session: sessName, TurnID: turnID}
	case errors.Is(err, runtime.ErrSessionNotFound):
		return runtime.TurnInterrupt{}, fmt.Errorf("interrupting turn %q of %s: %w: %w (%s)", turnID, sessName, ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, err.Error())
	case errors.Is(err, runtime.ErrTurnNotCurrent), errors.Is(err, runtime.ErrTurnUnnamed):
		return runtime.TurnInterrupt{}, fmt.Errorf("interrupting turn %q of %s: %w", turnID, sessName, err)
	case errors.Is(err, runtime.ErrRuntimeUnavailable):
		// The runtime could not tell (an RPC whose answer never came, a
		// re-read after a failed dispatch): unknown whether the request
		// reached it.
		return runtime.TurnInterrupt{}, fmt.Errorf("interrupting turn %q of %s: %w: %w", turnID, sessName, ErrInterruptOutcomeUnknown, err)
	case err != nil:
		// The runtime's error names no class (an RPC that failed, a bare
		// ctx error): unknown whether the request reached it.
		return runtime.TurnInterrupt{}, fmt.Errorf("interrupting turn %q of %s: %w: %w: %w", turnID, sessName, ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, err)
	}
	if _, err := tf.WaitTurnEnded(ctx, sessName, turnID, turnSettleTimeout); err != nil {
		what := fmt.Sprintf("interrupted turn %q of %s", turnID, sessName)
		if errors.Is(err, runtime.ErrRuntimeUnavailable) {
			// No read of the wait answered (WaitTurnEnded's rule): nothing
			// showed the turn still running.
			return ack, TurnEndUnreadError(what, err)
		}
		return ack, TurnNotEndedError(what, err)
	}
	return ack, nil
}

// TurnNotEndedError is the error of an interrupt that was sent but whose
// turn was not seen to end: it wraps runtime.ErrTurnNotEnded and nothing
// else, and carries cause, the last failure, in its text only, so a caller
// matching sentinels never reads "sent" as a refusal or an unknown.
func TurnNotEndedError(what string, cause error) error {
	if cause == nil || cause.Error() == runtime.ErrTurnNotEnded.Error() {
		return fmt.Errorf("%s: %w", what, runtime.ErrTurnNotEnded)
	}
	return fmt.Errorf("%s: %w (last: %s)", what, runtime.ErrTurnNotEnded, cause.Error())
}

// TurnEndUnreadError is TurnNotEndedError for an interrupted turn that
// nothing showed still running either: no read of it answered after the
// interrupt, and the stop's own waits passed or failed only for want of an
// answer. Like TurnNotEndedError it wraps runtime.ErrTurnNotEnded and
// nothing else, cause in its text only, so a caller matching sentinels still
// reads it as sent and not seen to end. A restart on failure takes it as no
// ground to restart (restartWithheld): whether the interrupt ended the turn
// is unknown, and whatever runs now may be another turn, which a restart
// would kill.
func TurnEndUnreadError(what string, cause error) error {
	return &turnEndUnreadError{err: TurnNotEndedError(what+", nothing showing it still running", cause)}
}

// turnEndUnreadError is TurnEndUnreadError's error.
type turnEndUnreadError struct{ err error }

func (e *turnEndUnreadError) Error() string { return e.err.Error() }

// Unwrap is the TurnNotEndedError it carries.
func (e *turnEndUnreadError) Unwrap() error { return e.err }

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
	return m.respond(id, response, func(ip runtime.InteractionProvider, sessName string, response runtime.InteractionResponse) error {
		return ip.Respond(sessName, response)
	})
}

// RespondFenced answers as Respond does, only in the incarnation fence names
// (SESSION-RUNTIME-024): the provider must fence answers to the session
// (runtime.FencedInteractionProvider), else nothing is sent and the error is
// runtime.ErrInputFenceUnsupported; a respawn between the check and the key
// is a *runtime.FenceMismatchError. ctx bounds the provider's fenced answer,
// not the session mutation lock.
func (m *Manager) RespondFenced(ctx context.Context, id string, fence runtime.InputFence, response runtime.InteractionResponse) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	return m.respond(id, response, func(_ runtime.InteractionProvider, sessName string, response runtime.InteractionResponse) error {
		return runtime.RespondFencedVia(ctx, m.sp, sessName, fence, response)
	})
}

// respond is Respond with the provider's answer sent by send.
func (m *Manager) respond(id string, response runtime.InteractionResponse, send func(runtime.InteractionProvider, string, runtime.InteractionResponse) error) error {
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
		// The provider may not return until it sees the prompt clear (tmux
		// waits up to 10 s for Claude Code to redraw), and this session's
		// mutation lock is held all that time, so sends, stops and other
		// mutations of the session wait behind it.
		if err := send(ip, sessName, response); err != nil {
			if errors.Is(err, runtime.ErrInteractionUnsupported) {
				return ErrInteractionUnsupported
			}
			if errors.Is(err, runtime.ErrInteractionGone) {
				return fmt.Errorf("%w: %w", ErrInteractionMismatch, err)
			}
			if errors.Is(err, runtime.ErrInteractionActionUnavailable) {
				return fmt.Errorf("%w: %w", ErrInteractionMismatch, err)
			}
			if errors.Is(err, runtime.ErrSessionNotFound) {
				log.Printf("session: respond runtime session gone for %q: %v", sessName, err)
				return ErrNoPendingInteraction
			}
			if errors.Is(err, runtime.ErrInteractionResponseInvalid) {
				return fmt.Errorf("%w: %w", ErrInteractionMismatch, err)
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
		if path := m.acpCaptureTranscriptPath(b); path != "" {
			return path, TranscriptFound, nil
		}
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
	// An ACP session's transcript is the capture gc wrote itself. A keyed
	// native transcript (above) still wins; the workdir fallbacks below could
	// return an unrelated provider's file.
	if path := m.acpCaptureTranscriptPath(b); path != "" {
		return path, TranscriptFound, nil
	}
	// zcode carries no session_key — no session-id flag, no hook plugin — so
	// the keyed lookup above can never hit for it and the ambiguity guard below
	// would leave every pooled worker transcript-dark. Its mirror is keyed by
	// the identity the bead does hold: its name, its own id (the seat — two
	// seats can share a name and epoch), and its continuation epoch.
	if path := workertranscript.DiscoverScopedPath(
		searchPaths,
		provider,
		workDir,
		b.Metadata["session_name"],
		b.ID,
		b.Metadata["continuation_epoch"],
	); path != "" {
		return path, TranscriptFound, nil
	}
	// A provider with a session-id flag runs each new conversation under a new
	// key. A creating row of such a provider without a key is a start whose
	// conversation has no key yet, and the work-directory fallback cannot tell
	// its transcript from the one that start replaces. Other sessions in the
	// work directory leave nothing to disambiguate before that key exists, so
	// the window is absent, not ambiguous.
	if strings.TrimSpace(b.Metadata["session_key"]) == "" &&
		strings.TrimSpace(b.Metadata["session_id_flag"]) != "" &&
		State(strings.TrimSpace(b.Metadata["state"])) == StateCreating {
		return "", TranscriptAbsent, nil
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
	if path := workertranscript.DiscoverPath(searchPaths, provider, workDir, b.Metadata["session_key"]); path != "" {
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
