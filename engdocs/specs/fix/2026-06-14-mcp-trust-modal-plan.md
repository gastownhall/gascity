# MCP-trust-modal crash-loop fix — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop managed Claude agents from crash-looping at the session-create handshake on Claude Code's "New MCP server found in this project" trust modal (`aborted before creation_complete`).

**Architecture:** Defense in depth. **Layer B (preventive):** add `enableAllProjectMcpServers: true` to the embedded Claude settings template so the projected `.gc/settings.json` pre-trusts project MCP servers and the modal never renders. **Layer A (reactive):** add a new dialog class to gc's startup-dialog auto-dismisser that detects the MCP-trust modal and selects option 2 ("Use this and all future MCP servers in this project"). The layers are independent and land as separate commits.

**Tech Stack:** Go 1.x, standard `testing`; gc build via `make build`/`make install`; provider Claude Code 2.1.177; tmux transport.

**Spec:** `engdocs/specs/fix/2026-06-14-mcp-trust-modal-design.md`

**Branch:** `fix/ga-nan-mcp-trust-prompt` (already created on `remuscazacu/gascity`, synced to upstream `main`).

---

## File Structure

- `internal/hooks/config/claude.json` — embedded Claude settings template (Layer B: add one key).
- `internal/hooks/hooks_test.go` — `TestInstallClaude` asserts the projected `/city/.gc/settings.json` contents (Layer B: add one assertion; this also proves #3406's projection conformance guard does not strip the key).
- `internal/runtime/dialog.go` — startup-dialog auto-dismisser (Layer A: new matcher + peek handler + stream handler + chain wiring).
- `internal/runtime/dialog_test.go` — dialog unit tests + fixtures (Layer A: new fixture + tests).

---

## Task 1: Layer B — pre-approve project MCP servers in projected settings

**Files:**
- Modify: `internal/hooks/config/claude.json`
- Test: `internal/hooks/hooks_test.go` (extend `TestInstallClaude`)

- [ ] **Step 1: Add the failing assertion to `TestInstallClaude`**

In `internal/hooks/hooks_test.go`, find the existing `skipDangerousModePermissionPrompt` assertion (search for the string `"skipDangerousModePermissionPrompt": true`). Immediately after that `if` block, add:

```go
	if !strings.Contains(s, `"enableAllProjectMcpServers": true`) {
		t.Error("claude settings should pre-approve project MCP servers (enableAllProjectMcpServers) so managed agents don't block on the 'New MCP server found' trust modal (#3466)")
	}
```

(`s` is `string(runtimeData)` where `runtimeData = fs.Files["/city/.gc/settings.json"]`, so this asserts the key reaches the *projected* runtime settings, not just the template.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/hooks/ -run TestInstallClaude -v`
Expected: FAIL — `claude settings should pre-approve project MCP servers (enableAllProjectMcpServers)...`

- [ ] **Step 3: Add the key to the embedded template**

In `internal/hooks/config/claude.json`, add the key after `skipDangerousModePermissionPrompt`. The top of the file becomes:

```json
{
  "skipDangerousModePermissionPrompt": true,
  "enableAllProjectMcpServers": true,
  "editorMode": "normal",
  "awaySummaryEnabled": false,
  "hooks": {
```

(Leave the rest of the file — `editorMode`, `awaySummaryEnabled`, `hooks` — unchanged.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/hooks/ -run TestInstallClaude -v`
Expected: PASS

- [ ] **Step 5: Run the full hooks package to catch upgrade/merge regressions**

Run: `go test ./internal/hooks/`
Expected: PASS (in particular `TestInstallClaudeUpgradesStaleGeneratedFile` must still pass — the new key flows through the merge path unchanged).

- [ ] **Step 6: Commit**

```bash
git add internal/hooks/config/claude.json internal/hooks/hooks_test.go
git commit -m "fix(hooks): pre-approve project MCP servers in projected Claude settings (#3466)

Managed Claude agents launch headless via tmux. When their work_dir loads a
project-scoped MCP server (.mcp.json), Claude Code 2.1.177 renders a 'New MCP
server found in this project' trust modal that nothing answers, so gc aborts
the session-create handshake (aborted before creation_complete) and, for
mode=always agents, crash-loops.

Add enableAllProjectMcpServers:true to the embedded settings template, next to
the existing skipDangerousModePermissionPrompt, so the projected
.gc/settings.json pre-trusts project MCP servers and the modal never renders.
Verified live (claude 2.1.177): the key is honored from a --settings file and
suppresses the modal.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Layer A — detect and answer the MCP-trust modal (peek + stream)

**Files:**
- Modify: `internal/runtime/dialog.go`
- Test: `internal/runtime/dialog_test.go`

The MCP-trust modal renders *after* the workspace-trust dialog, so the new handler is inserted between workspace-trust and Codex hook-review in both dispatch chains. Selecting **option 2** sends `Down, Enter` (cursor defaults to option 1).

- [ ] **Step 1: Add a fixture and failing tests**

In `internal/runtime/dialog_test.go`, add this fixture near the other fixtures (`codexUpdateDialogFixture`, `codexHookReviewDialogFixture` around line 752):

```go
func mcpTrustDialogFixture() string {
	return "New MCP server found in this project: mcptest-probe\n" +
		"MCP servers may execute code or access system resources. All tool calls require approval. Learn more in the MCP documentation.\n" +
		"❯ 1. Use this MCP server\n" +
		"  2. Use this and all future MCP servers in this project\n" +
		"  3. Continue without using this MCP server\n" +
		"Enter to confirm · Esc to cancel"
}
```

Then add these tests (place them next to the other `TestAcceptStartupDialogs*` tests):

```go
func TestContainsMCPTrustDialog(t *testing.T) {
	if !containsMCPTrustDialog(mcpTrustDialogFixture()) {
		t.Error("containsMCPTrustDialog should match the MCP trust modal")
	}
	if containsMCPTrustDialog("Do you trust the contents of this directory?") {
		t.Error("containsMCPTrustDialog should not match the workspace trust dialog")
	}
	if containsMCPTrustDialog("› Implement {feature}") {
		t.Error("containsMCPTrustDialog should not match a ready prompt")
	}
}

func TestAcceptStartupDialogsAcceptsMCPTrustDialog(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			if len(sent) == 0 {
				return mcpTrustDialogFixture(), nil
			}
			return "› Implement {feature}", nil
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsHandlesTrustThenMCPTrust(t *testing.T) {
	withZeroDialogTimings(t)
	dialogPollTimeout = time.Second

	var sent []string
	err := AcceptStartupDialogs(
		context.Background(),
		func(_ int) (string, error) {
			switch len(sent) {
			case 0:
				return "Do you trust the contents of this directory?", nil
			case 1:
				return mcpTrustDialogFixture(), nil
			default:
				return "› Implement {feature}", nil
			}
		},
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogs returned error: %v", err)
	}
	if got, want := strings.Join(sent, ","), "Enter,Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestAcceptStartupDialogsFromStreamAcceptsMCPTrustDialog(t *testing.T) {
	var sent []string
	snapshots := make(chan string, 2)
	snapshots <- mcpTrustDialogFixture()
	snapshots <- "› Implement {feature}"
	close(snapshots)

	err := AcceptStartupDialogsFromStream(
		context.Background(),
		time.Second,
		snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStream() error = %v", err)
	}
	if got, want := strings.Join(sent, ","), "Down,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail to compile**

Run: `go test ./internal/runtime/ -run 'MCPTrust' -v`
Expected: FAIL — `undefined: containsMCPTrustDialog` (the handler does not exist yet).

- [ ] **Step 3: Add the matcher and peek handler to `dialog.go`**

In `internal/runtime/dialog.go`, add (place near `acceptWorkspaceTrustDialog`):

```go
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
```

- [ ] **Step 4: Teach the workspace-trust handlers to defer to the MCP modal**

So the workspace-trust handler yields once the MCP modal is the visible dialog:

In `containsPostTrustStartupDialog` (used by the stream trust handler's `readyOrNext`), add `containsMCPTrustDialog`. It becomes:

```go
func containsPostTrustStartupDialog(content string) bool {
	return containsMCPTrustDialog(content) ||
		containsCodexHookReviewDialog(content) ||
		strings.Contains(content, "Bypass Permissions mode") ||
		containsCustomAPIKeyDialog(content) ||
		ContainsRateLimitDialog(content)
}
```

In the peek handler `acceptWorkspaceTrustDialog`, the final pre-`sleep` early-return block currently checks `containsCodexHookReviewDialog(content) || ...`. Add `containsMCPTrustDialog(content) ||` as the first term of that block so a workspace-trust handler that has already accepted trust returns promptly when the MCP modal renders. The block becomes:

```go
		if containsMCPTrustDialog(content) ||
			containsCodexHookReviewDialog(content) ||
			strings.Contains(content, "Bypass Permissions mode") ||
			containsCustomAPIKeyDialog(content) ||
			ContainsRateLimitDialog(content) {
			return nil
		}
```

- [ ] **Step 5: Wire the new handler into both dispatch chains**

In `AcceptStartupDialogsWithTimeout` (peek chain), insert the MCP handler immediately after the `acceptWorkspaceTrustDialog` block and before the `acceptCodexHookReviewDialog` block:

```go
	if err := acceptMCPTrustDialog(ctx, timeout, peek, sendKeys); err != nil {
		return fmt.Errorf("mcp trust dialog: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
```

In `AcceptStartupDialogsFromStreamWithStatus` (stream chain), insert immediately after the `acceptWorkspaceTrustDialogFromStream` block and before the `acceptCodexHookReviewDialogFromStream` block, mirroring the surrounding pattern exactly:

```go
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
```

- [ ] **Step 6: Run the new tests to verify they pass**

Run: `go test ./internal/runtime/ -run 'MCPTrust' -v`
Expected: PASS (all four: `TestContainsMCPTrustDialog`, `TestAcceptStartupDialogsAcceptsMCPTrustDialog`, `TestAcceptStartupDialogsHandlesTrustThenMCPTrust`, `TestAcceptStartupDialogsFromStreamAcceptsMCPTrustDialog`).

- [ ] **Step 7: Run the full runtime dialog suite for regressions**

Run: `go test ./internal/runtime/ -run 'Dialog|StartupDialogs'`
Expected: PASS (all 33+ existing dialog tests, plus the 4 new ones — confirms the inserted chain step did not break ordering/passthrough for the other dialogs).

- [ ] **Step 8: Commit**

```bash
git add internal/runtime/dialog.go internal/runtime/dialog_test.go
git commit -m "fix(runtime): answer Claude's project-MCP trust modal in startup-dialog auto-dismissal (#3466)

gc's startup-dialog auto-dismisser handled workspace-trust, bypass-perms,
resume, custom-API-key, Codex update/hook, and rate-limit dialogs, but not
Claude Code's 'New MCP server found in this project' trust modal. A headless
tmux agent could not answer it, so gc aborted the session-create handshake.

Add an MCP-trust dialog class (matcher + peek and stream handlers) following
the existing pattern, inserted after workspace-trust in both dispatch chains.
It selects option 2, 'Use this and all future MCP servers in this project'
(Down, Enter), which persists trust so the modal does not recur. This is the
narrow, still-open piece of #534 and a reactive safety net complementing the
settings-projection fix for agents gc does not project settings for.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Build and run the fast quality gates

**Files:** none (verification only)

- [ ] **Step 1: Build the binary**

Run: `make build`
Expected: compiles, no errors; binary at `build/gc` (or the configured `BUILD_DIR`).

- [ ] **Step 2: Run the fast CI-equivalent gates**

Run: `make check`
Expected: PASS — `fmt-check`, `lint`, `vet`, `check-routed-test-rows`, `test` all green. If `fmt-check` flags formatting, run `make fmt` and re-run; re-commit any formatting deltas with `git commit --amend --no-edit` to the owning task's commit.

---

## Task 4: Local install and live acceptance against the suspended city

**Files:** none (live validation). **Caution:** this resumes the intentionally-suspended `gascity` city briefly. Do not touch the DEV city (`~/dev/city`, prefix `ci`) or its `mayor` tmux process. Confirm `call-control` (not `mayor`) is the configured always-on session before resuming (per ga-psn.2.2/2.3). Re-suspend when done.

- [ ] **Step 1: Install the fork's gc**

Run: `make install`
Then verify the installed binary is the fork build:
Run: `which gc && gc cities 2>/dev/null | head`
Expected: `gc` resolves to the freshly installed binary (GOPATH/bin, with `~/.local/bin/gc` migrated to a symlink). Ensure `~/go/bin` is on PATH.

- [ ] **Step 2: Confirm the projected settings now carry the key (belt-and-suspenders)**

After the next projection (init/resume writes `<city>/.gc/settings.json`), run:
Run: `grep -n enableAllProjectMcpServers <gascity-city-dir>/.gc/settings.json`
Expected: one match, `"enableAllProjectMcpServers": true`.

- [ ] **Step 3: Resume briefly and watch call-control**

Resume the city, then watch the call-control session reach and hold `running`:
Run: `gc resume` then `gc agents` (or `gc rigs status`) a few times over ~3–4 minutes.
Expected: `call-control` reaches `running` and stays up — no `creating → reserved-unmaterialized → creating` cycle; a session bead reaches a started/awake state with `startup_dialog_verified` set (no `failed-create` / `aborted before creation_complete`).

- [ ] **Step 4: Re-suspend to restore the intended state**

Run: `gc suspend`
Expected: city returns to the intentional-suspended state.

---

## Task 5: Push the branch and open the upstream PR

**Files:** none (the two fix commits from Tasks 1 and 2, plus the spec and plan docs).

- [ ] **Step 1: Push the branch**

Run: `git push -u origin fix/ga-nan-mcp-trust-prompt`
Expected: branch pushed to `remuscazacu/gascity`.

- [ ] **Step 2: Open the PR to upstream**

Run:
```bash
gh pr create --repo gastownhall/gascity \
  --base main --head remuscazacu:fix/ga-nan-mcp-trust-prompt \
  --title "fix: managed Claude agents crash-loop on project-MCP trust modal (aborted before creation_complete) (#3466)" \
  --body "$(cat <<'BODY'
Fixes the crash-loop reported in #3466 (sibling of #3109; relates to #534): a
tmux-transport agent whose work_dir loads a project-scoped MCP server blocks on
Claude Code's "New MCP server found in this project" trust modal, which a
headless managed agent cannot answer, so the session-create handshake aborts
("aborted before creation_complete") and mode=always agents crash-loop.

Defense in depth, two independent commits:

1. **Preventive** — `enableAllProjectMcpServers: true` in the projected Claude
   settings template (`internal/hooks/config/claude.json`), next to the existing
   `skipDangerousModePermissionPrompt`. The modal never renders for projected
   agents. Verified live on Claude Code 2.1.177 that the key is honored from a
   `--settings` file. (Issue ask #2.)
2. **Reactive** — a new MCP-trust dialog class in
   `internal/runtime/dialog.go` that selects option 2 ("Use this and all future
   MCP servers in this project"), covering agents gc does not project settings
   for. (The narrow, still-open piece of #534 / issue ask #1.)

Tests: extended `TestInstallClaude`; added matcher + peek + stream tests in
`internal/runtime/dialog_test.go`.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
BODY
)"
```
Expected: PR opened against `gastownhall/gascity`.

- [ ] **Step 3: Record the PR and resolve the bead**

Note the PR URL on the `ga-nan` bead (`bd update ga-nan --notes=...`) and, once the local acceptance in Task 4 passed, the bead can be closed (per the conservative session-close protocol — report status; close on user authority).

---

## Self-Review

- **Spec coverage:** Layer B → Task 1; Layer A → Task 2; build/check → Task 3; local install + live acceptance + re-suspend → Task 4; upstream PR with two separate commits + cross-links → Task 5. Out-of-scope items (ACP, workspace-trust, mcp_project.go) are untouched. The #3406 conformance-guard concern is covered by Task 1 Step 1 asserting the key in the *projected* `.gc/settings.json`. All spec requirements map to a task.
- **Placeholder scan:** none — every code/JSON/command step shows the actual content; `<gascity-city-dir>` and `<name>` are genuine runtime values the operator substitutes, not unspecified plan content.
- **Type consistency:** `containsMCPTrustDialog`, `acceptMCPTrustDialog`, `acceptMCPTrustDialogFromStream`, `containsPostMCPTrustStartupDialog`, and `mcpTrustDialogFixture` are named identically across the test (Task 2 Step 1), the implementation (Steps 3–5), and the wiring; keystrokes `Down, Enter` are consistent between handler, tests, and PR text.
