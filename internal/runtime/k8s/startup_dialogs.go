package k8s

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// startupDialogsUnresolvedKey is the session metadata key ([Provider.SetMeta]
// writes it to the pod's tmux session environment) recording a
// startup-dialog pass that did not leave the pane clear of startup dialogs:
// the initial nudge withheld because a dialog is still on screen or the
// screen could not be read, or a pass that failed. stderr alone is not a
// durable record: a daemonized controller discards it. A clean pass removes
// the key, so on a warm pod it describes the current agent, not an earlier
// one.
const startupDialogsUnresolvedKey = "GC_K8S_STARTUP_DIALOGS_UNRESOLVED"

// k8sAcceptsStartupDialogs reports whether Start and Relaunch run the shared
// startup-dialog pass for cfg: the explicit AcceptStartupDialogs setting, or
// acceptance inferred from the managed process hints. A one-shot command is
// never polled, keeping its Start a fire-and-forget launch.
func k8sAcceptsStartupDialogs(cfg runtime.Config) bool {
	return k8sRequiresPostStartLiveness(cfg) && runtime.ShouldAcceptStartupDialogs(cfg)
}

// acceptStartupDialogs runs the shared startup-dialog pass
// ([runtime.AcceptStartupDialogs]) over the session's pane under ctx, so the
// dialog rules stay those every tmux-in-a-box runtime shares. Startup keys go
// straight to the carrier, as tmux's own startup pass sends them. External
// CLAUDE.md imports are allowed only inside trustRoot; "" allows none.
func (p *Provider) acceptStartupDialogs(ctx context.Context, name, trustRoot string) error {
	c := p.carrier()
	return runtime.AcceptStartupDialogs(ctx,
		func(lines int) (string, error) { return p.peekPane(ctx, name, lines) },
		func(keys ...string) error { return c.SendKeys(ctx, name, keys...) },
		runtime.WithTrustedImportRoot(trustRoot),
	)
}

// peekPane is the startup-dialog pass's peek: the last lines of the pane, as
// the carrier's Peek reads them (all scrollback when lines <= 0).
func (p *Provider) peekPane(ctx context.Context, name string, lines int) (string, error) {
	start := "-"
	if lines > 0 {
		start = "-" + strconv.Itoa(lines)
	}
	return p.capturePane(ctx, name, "-S", start)
}

// capturePane runs tmux capture-pane on the session's pane with args; none
// reads the visible screen alone. Unlike the carrier, which drops the exit
// code, it reports a capture that ran and exited non-zero as
// [runtime.ErrSessionNotFound]: tmux has no such session, because the agent
// exited and its session went with it. The startup-dialog pass then stops at
// its first read, and the post-start liveness probe reports the death, rather
// than the pass reading the failed capture as an empty pane for its whole
// budget.
func (p *Provider) capturePane(ctx context.Context, name string, args ...string) (string, error) {
	argv := append([]string{"tmux", "capture-pane", "-t", tmuxSession, "-p"}, args...)
	out, code, err := p.Exec(ctx, name, argv)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("%w: session %q: tmux capture-pane exited %d", runtime.ErrSessionNotFound, name, code)
	}
	return string(out), nil
}

// clearRelaunchHistory clears the warm pane's scrollback after a respawn so
// the previous agent's prompt cannot read as the new agent being ready. The
// visible screen and the pipe-pane log are untouched. A failure is reported
// on stderr and the relaunch continues; only an ended ctx is returned.
func (p *Provider) clearRelaunchHistory(ctx context.Context, name string) error {
	if err := p.carrier().ClearScrollback(ctx, name); err != nil {
		fmt.Fprintf(p.stderr, "gc: warning: session %q: clearing history before the startup-dialog pass: %v\n", name, err) //nolint:errcheck
	}
	return ctx.Err()
}

// runStartupDialogPass answers the agent's startup dialogs before the
// post-start liveness probe and the initial nudge, so the nudge reaches the
// agent rather than a workspace-trust or permission dialog. A pass that
// fails is best-effort: it is reported on stderr and startup continues. Only
// a ctx that ended during the pass, or during the read of the screen after
// it, is returned, so the caller stops before probing or nudging.
//
// A pass that ends on a transport failure, which the apiserver exec path
// produces independently of the pod (see livenessProbeAttempts), runs once
// more first, so one failed read cannot end it on its first frame. A session
// that is gone ends it at once: the caller's liveness probe reports the death.
//
// nudgeAllowed is false when the nudge's text could land in a startup dialog
// the pass left on screen, whose answer its submit Enter would give (see
// startupNudgeWithheldReason). The session stays up and the nudge is
// withheld, as herdr does. A withheld nudge and a failed pass are recorded on
// the session (startupDialogsUnresolvedKey); only a clean pass whose screen
// shows no such dialog clears the record.
func (p *Provider) runStartupDialogPass(ctx context.Context, name string, cfg runtime.Config) (nudgeAllowed bool, err error) {
	// The agent lists its imports as pod paths, so the repository they must
	// lie in is resolved in the pod, for the work dir the agent runs in.
	trustRoot := runtime.WorkspaceImportTrustRootInBox(ctx, p, name, projectedPodWorkDir(cfg))
	dialogErr := p.acceptStartupDialogs(ctx, name, trustRoot)
	if startupDialogsRetryable(ctx, dialogErr) {
		dialogErr = p.acceptStartupDialogs(ctx, name, trustRoot)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if errors.Is(dialogErr, runtime.ErrSessionNotFound) {
		fmt.Fprintf(p.stderr, "gc: warning: session %q: startup dialogs: %v\n", name, dialogErr) //nolint:errcheck
		return true, nil
	}
	withheld, err := p.startupNudgeWithheldReason(ctx, name, dialogErr)
	if err != nil {
		return false, err
	}
	detail := startupDialogsUnresolved(withheld, dialogErr)
	p.recordStartupDialogs(name, detail)
	if detail != "" {
		fmt.Fprintf(p.stderr, "gc: warning: session %q: %s\n", name, detail) //nolint:errcheck
	}
	return withheld == "", nil
}

// startupDialogsRetryable reports whether a startup-dialog step that failed
// with err, the pass or the read of the screen after it, may succeed when run
// once more: a transport failure reading or keying the pane. An ended ctx, a
// trust dialog left unconfirmed, and a session that is gone are final.
func startupDialogsRetryable(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil &&
		!errors.Is(err, runtime.ErrWorkspaceTrustUnconfirmed) &&
		!errors.Is(err, runtime.ErrSessionNotFound)
}

// startupNudgeWithheldReason says why the initial nudge must not be typed
// after a pass that ended on dialogErr, or "" when it may be: a
// workspace-trust dialog the pass left unconfirmed
// ([runtime.ErrWorkspaceTrustUnconfirmed]), its cursor off the trust row and
// possibly on "No, exit"; a dialog the pass left on screen
// ([runtime.StartupDialogAwaitingAnswer]), such as an external CLAUDE.md
// imports modal whose imports lie outside the repository (its pre-selected
// option allows them); or a screen that cannot be read, so neither can be
// ruled out. It reads the visible screen, where a pending dialog is drawn,
// and reads it once more after a transport failure, as the pass runs once
// more after one. The error is the ctx's, when it ended during the read.
func (p *Provider) startupNudgeWithheldReason(ctx context.Context, name string, dialogErr error) (string, error) {
	if errors.Is(dialogErr, runtime.ErrWorkspaceTrustUnconfirmed) {
		return "the workspace-trust dialog is still on screen, unconfirmed", nil
	}
	screen, err := p.capturePane(ctx, name)
	if startupDialogsRetryable(ctx, err) {
		screen, err = p.capturePane(ctx, name)
	}
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		return fmt.Sprintf("the screen is unreadable after the startup-dialog pass: %v", err), nil
	case runtime.StartupDialogAwaitingAnswer(screen):
		return "a startup dialog is still on screen after the startup-dialog pass", nil
	}
	return "", nil
}

// startupDialogsUnresolved describes what the pass left unresolved, for the
// session record and stderr: why the initial nudge is withheld, when it is,
// and the error the pass ended on, when it failed. It is "" for a clean pass
// whose nudge may go.
func startupDialogsUnresolved(withheld string, dialogErr error) string {
	var parts []string
	if withheld != "" {
		parts = append(parts, "initial nudge withheld so its Enter cannot answer a startup dialog: "+withheld)
	}
	if dialogErr != nil {
		parts = append(parts, fmt.Sprintf("startup dialogs: %v", dialogErr))
	}
	return strings.Join(parts, "; ")
}

// recordStartupDialogs writes detail, stamped with the time, to the session's
// startupDialogsUnresolvedKey record, or removes the record when detail is
// empty. Best-effort, as [Provider.SetMeta] is.
func (p *Provider) recordStartupDialogs(name, detail string) {
	if detail == "" {
		_ = p.RemoveMeta(name, startupDialogsUnresolvedKey)
		return
	}
	_ = p.SetMeta(name, startupDialogsUnresolvedKey, time.Now().UTC().Format(time.RFC3339)+" "+detail)
}
