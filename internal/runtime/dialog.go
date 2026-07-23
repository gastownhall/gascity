package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	dialogPollInterval       = 500 * time.Millisecond
	dialogPollTimeout        = 8 * time.Second
	startupDialogAcceptDelay = 500 * time.Millisecond
	bypassDialogConfirmDelay = 200 * time.Millisecond
	startupDialogPeekLines   = 120
	// When a startup stream emits only irrelevant snapshots and then goes quiet,
	// fall back instead of waiting the full dialog timeout.
	startupDialogStreamIdleGrace = 100 * time.Millisecond
	// Give streamed startup snapshots a short chance to surface a follow-on
	// dialog after an initial shell prompt appears.
	startupDialogStreamReadyGrace = 100 * time.Millisecond
)

// StartupDialogTimeout returns the current timeout budget used by the shared
// startup dialog helpers. Tests override the backing variable directly.
func StartupDialogTimeout() time.Duration {
	return dialogPollTimeout
}

// StartupDialogOption configures optional policy for the startup-dialog helpers.
// Options are variadic so existing callers stay source-compatible.
type StartupDialogOption func(*startupDialogConfig)

// startupDialogConfig holds resolved optional startup-dialog policy.
type startupDialogConfig struct {
	// trustedImportRoot gates auto-acceptance of the "Allow external CLAUDE.md
	// file imports?" modal. When set, only imports within this first-party
	// workspace tree are accepted automatically; when empty the modal is left
	// for a human. See externalImportsTrusted.
	trustedImportRoot string
}

// WithTrustedImportRoot restricts external-CLAUDE.md-import auto-acceptance to
// imports that resolve within dir, the root of the repository the session runs
// in (resolve it with WorkspaceImportTrustRoot). Without it, the external-imports
// modal is left unaccepted so a human can decide, because auto-accepting imports
// from outside the repository would trust files the worker was never meant to
// read.
func WithTrustedImportRoot(dir string) StartupDialogOption {
	return func(c *startupDialogConfig) { c.trustedImportRoot = dir }
}

func newStartupDialogConfig(opts []StartupDialogOption) startupDialogConfig {
	var cfg startupDialogConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// AcceptStartupDialogs dismisses startup dialogs that can block automated
// sessions. Handles (in order):
//  1. Claude resume selector — requires Down+Enter to resume the full session
//  2. Codex update dialog ("Update available") — requires Down+Enter to skip
//  3. Workspace trust dialog (Claude "Quick safety check", Codex "Do you trust the contents of this directory?", pi "Trust project folder?")
//  4. External CLAUDE.md imports dialog (Claude "Allow external CLAUDE.md file imports?") — requires Enter to allow (option 1 pre-selected)
//  5. MCP trust dialog (Claude "New MCP server found in this project") — requires Down+Enter to trust all project MCP servers
//  6. Codex hook review dialog — requires Down+Enter to trust hooks
//  7. Bypass permissions warning ("Bypass Permissions mode") — requires Down+Enter
//  8. Claude custom API key confirmation — requires Up+Enter to select "Yes"
//
// The peek function should return the last N lines of the session's terminal output.
// The sendKeys function should send bare tmux-style keystrokes (e.g., "Enter", "Down").
//
// Idempotent: safe to call on sessions without dialogs.
func AcceptStartupDialogs(
	ctx context.Context,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
	opts ...StartupDialogOption,
) error {
	return AcceptStartupDialogsWithTimeout(ctx, dialogPollTimeout, peek, sendKeys, opts...)
}

// AcceptStartupDialogsFromStream dismisses known startup dialogs using an
// event stream of full-screen snapshots instead of repeated peeks.
func AcceptStartupDialogsFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots <-chan string,
	sendKeys func(keys ...string) error,
	opts ...StartupDialogOption,
) error {
	_, err := AcceptStartupDialogsFromStreamWithStatus(ctx, timeout, snapshots, sendKeys, opts...)
	return err
}

// AcceptStartupDialogsFromStreamWithStatus dismisses known startup dialogs
// using an event stream of full-screen snapshots instead of repeated peeks
// and reports whether the stream observed readiness or a known dialog state.
func AcceptStartupDialogsFromStreamWithStatus(
	ctx context.Context,
	timeout time.Duration,
	snapshots <-chan string,
	sendKeys func(keys ...string) error,
	opts ...StartupDialogOption,
) (bool, error) {
	cfg := newStartupDialogConfig(opts)
	stream := newReplayableSnapshotCursor(snapshots)
	observed := false
	handledDialog := false
	trackingSendKeys := func(keys ...string) error {
		handledDialog = true
		return sendKeys(keys...)
	}

	phaseObserved, err := acceptClaudeResumeDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("claude resume dialog: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = acceptCodexUpdateDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("codex update dialog: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = acceptWorkspaceTrustDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("workspace trust dialog: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = acceptExternalImportsDialogFromStream(ctx, timeout, stream, trackingSendKeys, cfg.trustedImportRoot)
	if err != nil {
		return observed, fmt.Errorf("external imports dialog: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = acceptMCPTrustDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("mcp trust dialog: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = acceptCodexHookReviewDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("codex hook review dialog: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = acceptBypassPermissionsWarningFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("bypass permissions warning: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = acceptCustomAPIKeyDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("custom API key dialog: %w", err)
	}
	observed = observed || phaseObserved
	if !phaseObserved && !observed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return observed, err
	}
	phaseObserved, err = dismissRateLimitDialogFromStream(ctx, timeout, stream, trackingSendKeys)
	if err != nil {
		return observed, fmt.Errorf("rate limit dialog: %w", err)
	}
	observed = observed || phaseObserved
	if handledDialog {
		promptObserved, err := acceptDialogFromStream(ctx, startupDialogStreamReadyGrace, stream, nil, streamDialogSpec{
			ready: containsPromptIndicator,
		})
		if err != nil {
			return observed, fmt.Errorf("startup readiness: %w", err)
		}
		if !promptObserved {
			return false, nil
		}
		observed = true
	}
	return observed, nil
}

// AcceptStartupDialogsWithTimeout dismisses known startup dialogs using the
// provided timeout budget for each dialog class.
func AcceptStartupDialogsWithTimeout(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
	opts ...StartupDialogOption,
) error {
	cfg := newStartupDialogConfig(opts)
	if err := acceptClaudeResumeDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("claude resume dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptCodexUpdateDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("codex update dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptWorkspaceTrustDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("workspace trust dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptExternalImportsDialog(ctx, timeout, peek, sendKeys, cfg.trustedImportRoot); err != nil {
		return fmt.Errorf("external imports dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptMCPTrustDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("mcp trust dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptCodexHookReviewDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("codex hook review dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptBypassPermissionsWarning(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("bypass permissions warning: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := acceptCustomAPIKeyDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("custom API key dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dismissRateLimitDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("rate limit dialog: %w", err)
	}
	return nil
}

// acceptClaudeResumeDialog dismisses Claude's high-token/old-session resume
// selector. The menu cursor uses the same ❯ prefix as the normal input prompt,
// so this must run before generic prompt detection. Choose "Resume full session
// as-is" to preserve the in-flight workflow context instead of summarizing it.
func acceptClaudeResumeDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if containsClaudeResumeDialog(content) {
			if err := sendKeys("Down"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) ||
			containsCodexUpdateDialog(content) ||
			containsWorkspaceTrustDialog(content) ||
			containsExternalImportsDialog(content) ||
			containsMCPTrustDialog(content) ||
			containsCodexHookReviewDialog(content) ||
			strings.Contains(content, "Bypass Permissions mode") ||
			containsCustomAPIKeyDialog(content) ||
			ContainsRateLimitDialog(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func containsClaudeResumeDialog(content string) bool {
	return strings.Contains(content, "Resume from summary") &&
		strings.Contains(content, "Resume full session as-is") &&
		strings.Contains(content, "Enter to confirm")
}

func acceptClaudeResumeDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       containsClaudeResumeDialog,
		matchKeys:   []string{"Down", "Enter"},
		matchDelay:  bypassDialogConfirmDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsPostClaudeResumeStartupDialog,
	})
}

func containsPostClaudeResumeStartupDialog(content string) bool {
	return containsCodexUpdateDialog(content) ||
		containsWorkspaceTrustDialog(content) ||
		containsExternalImportsDialog(content) ||
		containsMCPTrustDialog(content) ||
		containsCodexHookReviewDialog(content) ||
		strings.Contains(content, "Bypass Permissions mode") ||
		containsCustomAPIKeyDialog(content) ||
		ContainsRateLimitDialog(content)
}

// acceptCodexUpdateDialog skips Codex's interactive update prompt. The default
// selection is "Update now", so automated sessions must move down to "Skip".
func acceptCodexUpdateDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if containsCodexUpdateDialog(content) {
			if err := sendKeys("Down"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) ||
			containsWorkspaceTrustDialog(content) ||
			containsExternalImportsDialog(content) ||
			containsMCPTrustDialog(content) ||
			containsCodexHookReviewDialog(content) ||
			strings.Contains(content, "Bypass Permissions mode") ||
			containsCustomAPIKeyDialog(content) ||
			ContainsRateLimitDialog(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func containsCodexUpdateDialog(content string) bool {
	return strings.Contains(content, "Update available!") &&
		strings.Contains(content, "Skip until next version") &&
		strings.Contains(content, "Press enter to continue")
}

func acceptCodexUpdateDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       containsCodexUpdateDialog,
		matchKeys:   []string{"Down", "Enter"},
		matchDelay:  bypassDialogConfirmDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsPostUpdateStartupDialog,
	})
}

func containsPostUpdateStartupDialog(content string) bool {
	return containsWorkspaceTrustDialog(content) ||
		containsExternalImportsDialog(content) ||
		containsMCPTrustDialog(content) ||
		containsCodexHookReviewDialog(content) ||
		strings.Contains(content, "Bypass Permissions mode") ||
		containsCustomAPIKeyDialog(content) ||
		ContainsRateLimitDialog(content)
}

// acceptWorkspaceTrustDialog dismisses workspace trust dialogs for supported
// agents. Claude shows "Quick safety check"; Codex shows
// "Do you trust the contents of this directory?"; pi (>= 0.79) shows
// "Trust project folder?". In all cases the safe continue option is
// pre-selected, so Enter accepts.
func acceptWorkspaceTrustDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if containsWorkspaceTrustDialog(content) {
			if err := sendKeys("Enter"); err != nil {
				return err
			}
			sleep(ctx, startupDialogAcceptDelay)
			return nil
		}

		if containsPromptIndicator(content) {
			return nil
		}

		if containsExternalImportsDialog(content) ||
			containsMCPTrustDialog(content) ||
			containsCodexHookReviewDialog(content) ||
			strings.Contains(content, "Bypass Permissions mode") ||
			containsCustomAPIKeyDialog(content) ||
			ContainsRateLimitDialog(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func acceptWorkspaceTrustDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       containsWorkspaceTrustDialog,
		matchKeys:   []string{"Enter"},
		matchDelay:  startupDialogAcceptDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsPostTrustStartupDialog,
	})
}

func containsWorkspaceTrustDialog(content string) bool {
	return strings.Contains(content, "trust this folder") ||
		strings.Contains(content, "Quick safety check") ||
		strings.Contains(content, "Do you trust the contents of this directory?") ||
		strings.Contains(content, "Do you trust the files in this folder?") ||
		strings.Contains(content, "Trust project folder?")
}

func containsPostTrustStartupDialog(content string) bool {
	return containsExternalImportsDialog(content) ||
		containsMCPTrustDialog(content) ||
		containsCodexHookReviewDialog(content) ||
		strings.Contains(content, "Bypass Permissions mode") ||
		containsCustomAPIKeyDialog(content) ||
		ContainsRateLimitDialog(content)
}

// acceptExternalImportsDialog dismisses Claude Code's "Allow external
// CLAUDE.md file imports?" modal. It appears at startup when the project's
// CLAUDE.md @-imports a file outside the current working directory (this fork's
// CLAUDE.md imports ../AGENTS.md). A headless managed agent cannot answer it, so
// gc accepts the pre-selected option 1, "Yes, allow external imports", with
// Enter. The modal appears after workspace trust and before MCP server
// discovery, so this runs after acceptWorkspaceTrustDialog. See Claude Code
// v2.1.207.
//
// Auto-acceptance is gated on trustedRoot (the worker's repository root): only
// imports that resolve inside it are accepted (see externalImportsTrusted). The
// modal warns not to allow external imports for third-party repositories, so an
// import that escapes the repository, or one that cannot be verified, is left
// unaccepted for a human rather than pressing Enter on files outside the
// repository. An empty trustedRoot trusts nothing.
func acceptExternalImportsDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
	trustedRoot string,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if containsExternalImportsDialog(content) && externalImportsTrusted(content, trustedRoot) {
			if err := sendKeys("Enter"); err != nil {
				return err
			}
			sleep(ctx, startupDialogAcceptDelay)
			return nil
		}

		if containsPromptIndicator(content) {
			return nil
		}

		if containsMCPTrustDialog(content) ||
			containsCodexHookReviewDialog(content) ||
			strings.Contains(content, "Bypass Permissions mode") ||
			containsCustomAPIKeyDialog(content) ||
			ContainsRateLimitDialog(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func acceptExternalImportsDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
	trustedRoot string,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match: func(content string) bool {
			return containsExternalImportsDialog(content) && externalImportsTrusted(content, trustedRoot)
		},
		matchKeys:   []string{"Enter"},
		matchDelay:  startupDialogAcceptDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsPostExternalImportsStartupDialog,
	})
}

func containsExternalImportsDialog(content string) bool {
	return strings.Contains(content, "Allow external CLAUDE.md") &&
		strings.Contains(content, "allow external imports")
}

func containsPostExternalImportsStartupDialog(content string) bool {
	return containsMCPTrustDialog(content) ||
		containsCodexHookReviewDialog(content) ||
		strings.Contains(content, "Bypass Permissions mode") ||
		containsCustomAPIKeyDialog(content) ||
		ContainsRateLimitDialog(content)
}

// externalImportsTrusted reports whether every path listed in the "Allow
// external CLAUDE.md file imports?" modal is a first-party file inside
// trustRoot, the root of the repository the worker runs in (see
// WorkspaceImportTrustRoot). The modal fires because a project CLAUDE.md
// @-imports a file outside the working directory — for this fork, the
// repository's own AGENTS.md, which a worktree subdirectory sees as external.
// That file still lives inside the repository root, so it is first-party; an
// import that escapes the repository root (a sibling repo, a parent directory, a
// home or system path) is not, and neither is an in-root path that descends
// through a repository metadata or runtime directory such as .git or .gc (see
// importPathFirstParty). An empty trustRoot, or a modal with no parseable import
// path, trusts nothing so a human decides.
func externalImportsTrusted(content, trustRoot string) bool {
	if strings.TrimSpace(trustRoot) == "" {
		return false
	}
	imports := parseExternalImportPaths(content)
	if len(imports) == 0 {
		return false
	}
	for _, importPath := range imports {
		if !importPathFirstParty(importPath, trustRoot) {
			return false
		}
	}
	return true
}

// parseExternalImportPaths returns the absolute filesystem paths the external
// imports modal lists under its "External imports:" header. Each import renders
// on its own line; collection stops at the first blank line after a path or at
// the first non-absolute line (the trailing guidance text or numbered options).
func parseExternalImportPaths(content string) []string {
	const header = "External imports:"
	idx := strings.Index(content, header)
	if idx < 0 {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(content[idx+len(header):], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(paths) > 0 {
				break
			}
			continue
		}
		if !strings.HasPrefix(trimmed, "/") {
			break
		}
		paths = append(paths, trimmed)
	}
	return paths
}

// pathWithinTrustRoot reports whether importPath resolves inside (or equal to)
// trustRoot. Both are cleaned before comparison and the prefix test is
// path-segment aware, so "/a/b" never matches "/a/bc" and a "../" escape is
// rejected after cleaning. Only absolute paths are trusted; anything else (a
// "~"-relative or truncated path) fails closed.
func pathWithinTrustRoot(importPath, trustRoot string) bool {
	if !strings.HasPrefix(importPath, "/") {
		return false
	}
	root := filepath.Clean(trustRoot)
	imp := filepath.Clean(importPath)
	if !filepath.IsAbs(root) || !filepath.IsAbs(imp) {
		return false
	}
	return isPathPrefix(root, imp)
}

// importPathFirstParty reports whether importPath is a first-party instruction
// file the worker may auto-import. The path must resolve inside trustRoot (see
// pathWithinTrustRoot) AND must not descend through a repository metadata or
// runtime directory. Any component that is a hidden ("dot") directory relative
// to the root — VCS metadata such as .git, Gas City runtime state such as .gc,
// or per-tool caches such as .claude — is refused, because a PR-controlled
// CLAUDE.md could otherwise auto-import repository-internal state (for example
// .git/config, which can hold remote credentials) instead of a genuine
// instruction file. Those imports fail closed and are left for a human.
func importPathFirstParty(importPath, trustRoot string) bool {
	if !pathWithinTrustRoot(importPath, trustRoot) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(trustRoot), filepath.Clean(importPath))
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(segment, ".") {
			return false
		}
	}
	return true
}

// isPathPrefix reports whether ancestor equals descendant or is a
// path-segment-boundary prefix of it.
func isPathPrefix(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(ancestor, sep) {
		ancestor += sep
	}
	return strings.HasPrefix(descendant, ancestor)
}

// acceptMCPTrustDialog dismisses Claude Code's project-MCP trust modal
// ("New MCP server found in this project"). A headless managed agent cannot
// answer it, so gc selects option 2, "Use this and all future MCP servers in
// this project" (Down, Enter). Option 2 persists trust to ~/.claude.json so
// the modal does not recur. The modal appears after workspace trust, so this
// runs after acceptWorkspaceTrustDialog. See gascity#3466.
func acceptMCPTrustDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if containsMCPTrustDialog(content) {
			if err := sendKeys("Down"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) ||
			containsCodexHookReviewDialog(content) ||
			strings.Contains(content, "Bypass Permissions mode") ||
			containsCustomAPIKeyDialog(content) ||
			ContainsRateLimitDialog(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func containsMCPTrustDialog(content string) bool {
	return strings.Contains(content, "New MCP server found") &&
		strings.Contains(content, "Use this and all future MCP servers")
}

func containsPostMCPTrustStartupDialog(content string) bool {
	return containsCodexHookReviewDialog(content) ||
		strings.Contains(content, "Bypass Permissions mode") ||
		containsCustomAPIKeyDialog(content) ||
		ContainsRateLimitDialog(content)
}

func acceptMCPTrustDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       containsMCPTrustDialog,
		matchKeys:   []string{"Down", "Enter"},
		matchDelay:  bypassDialogConfirmDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsPostMCPTrustStartupDialog,
	})
}

// acceptCodexHookReviewDialog dismisses Codex's startup hook trust review.
// The first option reviews hook details; automated managed sessions want the
// second option, "Trust all and continue", so press Down then Enter.
func acceptCodexHookReviewDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if containsCodexHookReviewDialog(content) {
			if err := sendKeys("Down"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) ||
			strings.Contains(content, "Bypass Permissions mode") ||
			containsCustomAPIKeyDialog(content) ||
			ContainsRateLimitDialog(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func acceptCodexHookReviewDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       containsCodexHookReviewDialog,
		matchKeys:   []string{"Down", "Enter"},
		matchDelay:  bypassDialogConfirmDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsPostCodexHookReviewStartupDialog,
	})
}

func containsCodexHookReviewDialog(content string) bool {
	return (strings.Contains(content, "Hooks need review") ||
		strings.Contains(content, "hooks need review")) &&
		(strings.Contains(content, "Trust all and continue") ||
			strings.Contains(content, "trust all")) &&
		(strings.Contains(content, "Continue without trusting") ||
			strings.Contains(content, "enter to review hooks"))
}

func containsPostCodexHookReviewStartupDialog(content string) bool {
	return strings.Contains(content, "Bypass Permissions mode") ||
		containsCustomAPIKeyDialog(content) ||
		ContainsRateLimitDialog(content)
}

// acceptBypassPermissionsWarning dismisses the Claude Code bypass permissions
// warning. When Claude starts with --dangerously-skip-permissions, it shows a
// warning requiring Down arrow to select "Yes, I accept" and then Enter.
func acceptBypassPermissionsWarning(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if strings.Contains(content, "Bypass Permissions mode") {
			if err := sendKeys("Down"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func acceptBypassPermissionsWarningFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       func(content string) bool { return strings.Contains(content, "Bypass Permissions mode") },
		matchKeys:   []string{"Down", "Enter"},
		matchDelay:  bypassDialogConfirmDelay,
		ready:       containsPromptIndicator,
		readyOrNext: containsPostBypassStartupDialog,
	})
}

func containsPostBypassStartupDialog(content string) bool {
	return containsCustomAPIKeyDialog(content) || ContainsRateLimitDialog(content)
}

// acceptCustomAPIKeyDialog dismisses Claude's API-key confirmation prompt.
// In headless CI, Claude detects the injected ANTHROPIC_API_KEY and asks if it
// should use it. The menu defaults to "No (recommended)", so press Up then
// Enter to choose "Yes" and proceed with the configured provider.
func acceptCustomAPIKeyDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if containsCustomAPIKeyDialog(content) {
			if err := sendKeys("Up"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) || ContainsRateLimitDialog(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func acceptCustomAPIKeyDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:       containsCustomAPIKeyDialog,
		matchKeys:   []string{"Up", "Enter"},
		matchDelay:  bypassDialogConfirmDelay,
		ready:       containsPromptIndicator,
		readyOrNext: ContainsRateLimitDialog,
	})
}

func containsCustomAPIKeyDialog(content string) bool {
	return strings.Contains(content, "Detected a custom API key in your environment") ||
		strings.Contains(content, "Do you want to use this API key?")
}

// dismissRateLimitDialog detects rate limit / usage limit dialogs (e.g.,
// Gemini's "Usage limit reached") and selects "Stop" to let the session
// exit cleanly. The reconciler then peeks the pane and quarantines provider
// rate-limit exits with sleep_reason=rate_limit instead of counting them as
// wake failures.
func dismissRateLimitDialog(
	ctx context.Context,
	timeout time.Duration,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		content, err := peek(startupDialogPeekLines)
		if err != nil {
			return err
		}

		if ContainsRateLimitDialog(content) {
			// Select "Stop" (option 2). The menu has "Keep trying" selected
			// by default, so press Down then Enter.
			if err := sendKeys("Down"); err != nil {
				return err
			}
			sleep(ctx, bypassDialogConfirmDelay)
			return sendKeys("Enter")
		}

		if containsPromptIndicator(content) {
			return nil
		}

		sleep(ctx, dialogPollInterval)
	}
	return nil
}

func dismissRateLimitDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
) (bool, error) {
	return acceptDialogFromStream(ctx, timeout, snapshots, sendKeys, streamDialogSpec{
		match:      ContainsRateLimitDialog,
		matchKeys:  []string{"Down", "Enter"},
		matchDelay: bypassDialogConfirmDelay,
		ready:      containsPromptIndicator,
	})
}

type streamDialogSpec struct {
	match       func(string) bool
	ready       func(string) bool
	readyOrNext func(string) bool
	matchKeys   []string
	matchDelay  time.Duration
}

type replayableSnapshotStream struct {
	mu      sync.Mutex
	history []string
	closed  bool
	update  chan struct{}
}

type replayableSnapshotCursor struct {
	stream *replayableSnapshotStream
	next   int
	carry  []string
}

func newReplayableSnapshotCursor(src <-chan string) *replayableSnapshotCursor {
	return newReplayableSnapshotCursorFromStream(newReplayableSnapshotStream(src))
}

func newReplayableSnapshotCursorFromStream(stream *replayableSnapshotStream) *replayableSnapshotCursor {
	return &replayableSnapshotCursor{stream: stream}
}

func newReplayableSnapshotStream(src <-chan string) *replayableSnapshotStream {
	stream := &replayableSnapshotStream{update: make(chan struct{})}
	go func() {
		for content := range src {
			stream.publish(content)
		}
		stream.finish()
	}()
	return stream
}

func (s *replayableSnapshotStream) publish(content string) {
	s.mu.Lock()
	s.history = append(s.history, content)
	update := s.update
	s.update = make(chan struct{})
	s.mu.Unlock()
	close(update)
}

func (s *replayableSnapshotStream) finish() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	update := s.update
	s.mu.Unlock()
	close(update)
}

func (s *replayableSnapshotStream) historyFrom(start int) ([]string, bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if start < 0 {
		start = 0
	}
	if start > len(s.history) {
		start = len(s.history)
	}
	snapshots := append([]string(nil), s.history[start:]...)
	return snapshots, s.closed, s.update
}

func (c *replayableSnapshotCursor) nextBatch() ([]string, bool, <-chan struct{}) {
	batch := append([]string(nil), c.carry...)
	c.carry = nil
	history, closed, updated := c.stream.historyFrom(c.next)
	c.next += len(history)
	if len(history) > 0 {
		batch = append(batch, history...)
	}
	return batch, closed, updated
}

func (c *replayableSnapshotCursor) replay(history []string) {
	if len(history) == 0 {
		return
	}
	c.carry = append(append([]string(nil), history...), c.carry...)
}

func acceptDialogFromStream(
	ctx context.Context,
	timeout time.Duration,
	snapshots *replayableSnapshotCursor,
	sendKeys func(keys ...string) error,
	spec streamDialogSpec,
) (bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var (
		readySeen     bool
		latestReady   string
		readyTimer    *time.Timer
		readyDeadline <-chan time.Time
		idleTimer     *time.Timer
		idleDeadline  <-chan time.Time
	)
	stopTimer := func(timer *time.Timer) {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	resetIdleTimer := func() {
		if startupDialogStreamIdleGrace <= 0 {
			return
		}
		if idleTimer == nil {
			idleTimer = time.NewTimer(startupDialogStreamIdleGrace)
			idleDeadline = idleTimer.C
			return
		}
		stopTimer(idleTimer)
		idleTimer.Reset(startupDialogStreamIdleGrace)
		idleDeadline = idleTimer.C
	}
	defer stopTimer(readyTimer)
	defer stopTimer(idleTimer)

	for {
		history, closed, updated := snapshots.nextBatch()
		if len(history) > 0 {
			for idx, content := range history {
				if spec.match != nil && spec.match(content) {
					snapshots.replay(history[idx+1:])
					return true, sendDialogKeys(ctx, sendKeys, spec.matchKeys, spec.matchDelay)
				}
				if spec.readyOrNext != nil && spec.readyOrNext(content) {
					snapshots.replay(history[idx:])
					return true, nil
				}
				if spec.ready != nil && spec.ready(content) {
					latestReady = content
					if !readySeen {
						readySeen = true
						stopTimer(idleTimer)
						idleDeadline = nil
						if startupDialogStreamReadyGrace <= 0 {
							snapshots.replay([]string{latestReady})
							return true, nil
						}
						readyTimer = time.NewTimer(startupDialogStreamReadyGrace)
						readyDeadline = readyTimer.C
					}
				}
			}
			if !readySeen {
				resetIdleTimer()
			}
		}
		if closed {
			if readySeen {
				snapshots.replay([]string{latestReady})
			}
			return readySeen, nil
		}
		if readySeen {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-timer.C:
				snapshots.replay([]string{latestReady})
				return true, nil
			case <-readyDeadline:
				snapshots.replay([]string{latestReady})
				return true, nil
			case <-updated:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, nil
		case <-idleDeadline:
			return false, nil
		case <-updated:
		}
	}
}

func sendDialogKeys(
	ctx context.Context,
	sendKeys func(keys ...string) error,
	keys []string,
	delay time.Duration,
) error {
	if len(keys) == 0 {
		return nil
	}
	if len(keys) == 1 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sendKeys(keys[0]); err != nil {
			return err
		}
		sleep(ctx, delay)
		return ctx.Err()
	}
	for i, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sendKeys(key); err != nil {
			return err
		}
		if i < len(keys)-1 {
			sleep(ctx, delay)
		}
	}
	return nil
}

// ContainsRateLimitDialog reports whether pane content shows a provider
// rate-limit or usage-limit startup dialog. It is intentionally permissive for
// startup compatibility; use ContainsProviderRateLimitScreen when classifying
// arbitrary post-crash scrollback.
func ContainsRateLimitDialog(content string) bool {
	return strings.Contains(content, "Usage limit reached") ||
		strings.Contains(content, "You've hit your limit") ||
		strings.Contains(content, "/rate-limit-options") ||
		strings.Contains(content, "rate limit") ||
		strings.Contains(content, "Rate limit")
}

// ContainsModelSwitchModal reports whether pane content shows the mid-session
// "approaching rate limits — switch to a cheaper model?" modal that Codex/GPT
// raises (offering to downgrade the model). Unlike ContainsRateLimitDialog — a
// permissive matcher used only at startup — this requires BOTH the switch offer
// and the keep-current-model option, so ordinary agent output that merely
// mentions "rate limit" cannot false-match and receive spurious keystrokes when
// checked mid-session against an arbitrary working pane.
func ContainsModelSwitchModal(content string) bool {
	return strings.Contains(content, "Keep current model") &&
		strings.Contains(content, "Switch to ")
}

// ContainsProviderRateLimitScreen reports whether pane content has
// high-confidence provider rate-limit screen evidence.
func ContainsProviderRateLimitScreen(content string) bool {
	if strings.Contains(content, "Usage limit reached") ||
		strings.Contains(content, "You've hit your limit") ||
		strings.Contains(content, "/rate-limit-options") {
		return true
	}
	if containsClaudeSpendLimitModal(content) {
		return true
	}
	return strings.Contains(strings.ToLower(content), "rate limit") &&
		strings.Contains(content, "Keep trying") &&
		strings.Contains(content, "Stop")
}

// spendLimitModalWindowLines bounds how many consecutive lines the Claude
// spend-limit modal's anchor tokens may span. The modal renders "Usage credit
// balance", "Adjust monthly spend limit", and "Wait for limit to reset" on
// adjacent lines inside one bordered box; a small window tolerates a border or
// blank line between them while still rejecting the same tokens scattered across
// unrelated scrollback.
const spendLimitModalWindowLines = 6

// containsClaudeSpendLimitModal reports whether pane content shows Claude's
// spend-limit modal (which is a rate-limit, not a crash).
//
// It requires the modal's three anchor tokens to co-occur within one on-screen
// block rather than matching each token anywhere in the buffer. Whole-buffer
// strings.Contains for each token independently lets the tokens land on
// unrelated scrollback lines — e.g. a pane displaying billing notes or these
// very test fixtures — and misclassify a genuinely crashed session as
// rate-limited. That suppresses the session's SessionCrashed event and, because
// the rate-limit quarantine re-detects the same scrollback every reconcile
// cycle, masks the real crash indefinitely with no self-heal. "Wait for limit
// to reset" is always present in the real modal and is the reliable anchor, so
// the loose "Resets " arm is dropped as too weak.
func containsClaudeSpendLimitModal(content string) bool {
	return linesContainAllWithin(content, spendLimitModalWindowLines,
		"Usage credit balance",
		"Adjust monthly spend limit",
		"Wait for limit to reset")
}

// ProviderTerminalErrorReason classifies high-confidence provider errors that
// require operator/config intervention rather than immediate retry.
func ProviderTerminalErrorReason(content string) string {
	lower := strings.ToLower(content)
	switch {
	case strings.Contains(lower, "model_not_found"):
		return "model_not_found"
	case lineContainsAll(lower, "model", "not found"):
		// Require both tokens on the same line so the loose phrasing matches a
		// real "model … not found" provider error, not "model" and "not found"
		// landing on unrelated scrollback lines (which would permanently and
		// wrongly mark the session terminal with no self-heal).
		return "model_not_found"
	case strings.Contains(lower, "insufficient_quota"):
		return "quota_exceeded"
	case strings.Contains(lower, "quota_exceeded"):
		return "quota_exceeded"
	case strings.Contains(lower, "quota exceeded") && !strings.Contains(lower, "disk quota"):
		return "quota_exceeded"
	default:
		return ""
	}
}

// lineContainsAll reports whether any single line of content contains every
// substring in subs. It bounds loose multi-token matches to one line so the
// tokens must co-occur in the same message rather than anywhere in scrollback.
func lineContainsAll(content string, subs ...string) bool {
	for _, line := range strings.Split(content, "\n") {
		all := true
		for _, sub := range subs {
			if !strings.Contains(line, sub) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// linesContainAllWithin reports whether some window of at most maxSpan
// consecutive lines in content jointly contains every substring in subs. Like
// lineContainsAll it bounds a loose multi-token match to co-occurring text, but
// across a small block of adjacent lines (e.g. a modal box) rather than a single
// line, so the tokens cannot smear across unrelated scrollback lines and wrongly
// classify the pane.
func linesContainAllWithin(content string, maxSpan int, subs ...string) bool {
	if maxSpan < 1 || len(subs) == 0 {
		return false
	}
	lines := strings.Split(content, "\n")
	for start := range lines {
		window := strings.Join(lines[start:min(start+maxSpan, len(lines))], "\n")
		all := true
		for _, sub := range subs {
			if !strings.Contains(window, sub) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// containsPromptIndicator checks whether any line in the content looks like a
// common shell or agent prompt, indicating the session is ready and no dialog is
// present. Full-screen agent UIs often render placeholder input after the prompt
// glyph, so Claude/Codex prompts are accepted as prefixes too.
func containsPromptIndicator(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.ReplaceAll(line, "\u00a0", " ")
		trimmed = strings.TrimRight(trimmed, " \t")
		// Strip a leading box-drawing border + whitespace so a prompt glyph a TUI
		// renders inside a bordered input box (grok: "\u2502 \u276f \u2026") is recognized. Without
		// this the startup-dialog handlers never early-return for grok and burn
		// their full timeout, blowing doStartSession's start-context deadline so the
		// initial nudge (Step 6) is never sent and the worker idles forever.
		trimmed = stripLeadingBoxBorder(trimmed)
		if trimmed == "" {
			continue
		}
		for _, prefix := range []string{"\u276f", "\u203a", ">"} {
			rest, ok := strings.CutPrefix(trimmed, prefix+" ")
			if trimmed == prefix || (ok && !isNumberedMenuRow(rest)) {
				return true
			}
		}
		for _, suffix := range []string{">", "$", "%", "#", "\u276f", "\u203a"} {
			if strings.HasSuffix(trimmed, suffix) {
				return true
			}
		}
	}
	return false
}

// stripLeadingBoxBorder removes a leading vertical box-drawing character (│/┃)
// plus surrounding spaces, so a boxed prompt glyph (grok) is detected. No-op for
// borderless lines.
func stripLeadingBoxBorder(s string) string {
	s = strings.TrimLeft(s, " \t")
	r := []rune(s)
	if len(r) > 0 && (r[0] == '│' || r[0] == '┃') {
		return strings.TrimLeft(string(r[1:]), " \t")
	}
	return s
}

func isNumberedMenuRow(content string) bool {
	digits := 0
	for digits < len(content) && content[digits] >= '0' && content[digits] <= '9' {
		digits++
	}
	return digits > 0 && digits < len(content) && content[digits] == '.'
}

// sleep waits for the given duration or until ctx is canceled.
func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
