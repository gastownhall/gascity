# Design: fix the MCP-trust-modal crash-loop in managed Claude sessions

- **Date:** 2026-06-14
- **Bead:** ga-nan (P1 bug) — blocks ga-psn.2.6
- **Upstream:** [gastownhall/gascity#3466](https://github.com/gastownhall/gascity/issues/3466) (sibling of #3109; relates to #534)
- **Fork branch:** `fix/ga-nan-mcp-trust-prompt` on `remuscazacu/gascity` (synced to upstream `main`)
- **Status:** approved 2026-06-14

## Problem

On `gc resume`, the always-on `call-control` agent crash-loops (~1 attempt / 2 min,
cycling `reserved-unmaterialized → start-pending → creating → failed-create`).
Session beads close with:

```
state=failed-create
close_reason='session create failed: aborted before creation_complete'
startup_dialog_verified=''
```

The tmux pane **does** spawn and Claude Code launches, but the pane renders Claude's
project-MCP **trust modal** and waits for a keystroke that a headless managed agent
cannot supply:

```
New MCP server found in this project: <server-name>

❯ 1. Use this MCP server
  2. Use this and all future MCP servers in this project
  3. Continue without using this MCP server

Enter to confirm · Esc to cancel
```

gc's startup-dialog auto-dismissal (`internal/runtime/dialog.go`) handles
workspace-trust, bypass-perms, resume, custom-API-key, Codex update/hook, and
rate-limit dialogs — but **not** the project-MCP trust modal. So nothing answers it,
gc times out at the `creation_complete` handshake, the session fails, and because the
agent is `mode=always` the reconciler re-creates it → crash loop.

## Evidence (verified 2026-06-14, claude 2.1.177)

A bounded tmux experiment in a throwaway `/tmp/mcptest` dir with one stdio MCP server:

- **Control** — launch `claude --settings <no-key>`; after accepting workspace trust,
  the **"New MCP server found in this project"** modal blocks startup. Reproduces the
  bug exactly. (`-p`/print mode does **not** render the gate — that is why the original
  isolation in #3466 was misleading.)
- **Test** — launch `claude --settings <with enableAllProjectMcpServers: true>` in the
  now-trusted dir; Claude goes **straight to the prompt, no modal**. The `--settings`
  key is honored by claude 2.1.177 and is a pure runtime setting (it did not write back
  into `~/.claude.json`), so it suppresses the modal on every launch.

This resolves the open question in #3466 ("never confirmed; gc reverted it"): the key
works; it simply had no repo-side source, so gc regenerated the file without it.

## Goal

Managed Claude agents that load a project-scoped MCP server start cleanly and stay up.
Fix delivered on the fork, upstreamed as a PR (always-on default), then built and
installed locally to unblock `ga-psn.2.6`.

## Approach: defense in depth (two independent layers)

### Layer B — preventive (primary)

Add `"enableAllProjectMcpServers": true` to the embedded settings template
`internal/hooks/config/claude.json`, alongside the existing
`skipDangerousModePermissionPrompt`. It flows through `desiredClaudeSettings()` → merge
→ the projected `<city>/.gc/settings.json`, so Claude pre-trusts project MCP servers and
the modal never renders for projected agents.

- **Always-on**, not gated behind a knob: managed agents are headless, so the modal is
  never something a managed agent wants to see. This mirrors the existing always-on
  `skipDangerousModePermissionPrompt`.
- **Verification (in implementation plan):**
  1. Build gc, run a real projection, and `grep` the generated `.gc/settings.json` for
     the key — confirms #3406's "one projection with a reflective conformance guard"
     does not strip an unrecognized key.
  2. Add/extend a hooks unit test asserting the key is present in projected settings,
     mirroring the existing assertion at `internal/hooks/hooks_test.go:155`.

**Coverage limit:** only agents whose settings gc projects. That is the call-control
case, but not every tmux agent — hence Layer A.

### Layer A — reactive safety net (defense in depth)

Add a new dialog class to `internal/runtime/dialog.go`, following the existing
six-dialog pattern exactly (each dialog has a `contains…` matcher, a peek handler, a
stream handler, and a `containsPost…StartupDialog` passthrough entry):

- `containsMCPTrustDialog(content)` — matches `"New MCP server found in this project"`
  together with the three option lines.
- `acceptMCPTrustDialog(...)` (peek) and `acceptMCPTrustDialogFromStream(...)` (stream)
  — send **Down, Enter** to select **option 2, "Use this and all future MCP servers in
  this project."** Option 2 is **self-persisting**: Claude writes the trust to
  `~/.claude.json`, so the modal never recurs for that project even on later launches.
- Wire the new handler into both dispatch chains
  (`AcceptStartupDialogsWithTimeout` and `AcceptStartupDialogsFromStreamWithStatus`),
  and add it to the `containsPost…StartupDialog` early-return chains. Order it **after**
  workspace-trust, since the MCP modal appears only after the folder is trusted.
- Tests in `internal/runtime/dialog_test.go` following the 33 existing cases: matcher
  positive/negative, correct accept keystrokes, and passthrough when a later dialog or a
  ready prompt is already present.

**Coverage:** any tmux-transport agent, including ones gc does not project settings for,
and the case where a project is already trusted but a *new* MCP server is added later
(Layer B's blanket pre-approval also covers the latter, but A does not depend on B).

### Why both, as separate commits

Layer B is the cleanest, most mergeable fix (it is ask #2 in #3466 and extends an
existing key). Layer A is the durable general mechanism (the narrow, still-open piece of
#534 / ask #1 in #3466) and stands on its own. Landing them as two independent commits
lets the maintainer accept either or both.

## Out of scope

- **ACP transport** (issue ask #4) — a possible maintainer-preferred path; recorded as
  alternative-considered only, not implemented here.
- No change to workspace-trust handling or any other existing dialog handler.
- No change to `.mcp.json` materialization (`internal/materialize/mcp_project.go`).

## Alternatives considered

- **A only (reactive).** Broadest runtime coverage, but depends on terminal
  string-matching and the modal must render every first launch; #534's broad version was
  closed NOT_PLANNED, so acceptance was less certain. Rejected as the *sole* fix; kept as
  Layer A.
- **B behind an opt-in config knob.** More conservative, but managed agents never want
  the modal, so a default-off knob would just be a footgun. Rejected in favor of
  always-on.
- **Pre-seeding `~/.claude.json` trust store from gc.** Invasive (gc writing the user's
  global Claude config) and redundant with B's `--settings` route. Rejected.

## Validation / definition of done

1. `make build && make check` green on the fork.
2. Layer B: projection `grep` shows the key in generated `.gc/settings.json`; new hooks
   test passes.
3. Layer A: new `dialog_test.go` cases pass.
4. Install the fork's `gc`; `gc resume` briefly → confirm `call-control` reaches
   `running` and stays up, a session bead reaches awake with `startup_dialog_verified`
   set; then `gc suspend` to restore the intentional-suspended state (per the
   suspended-city working agreement).
5. Open a PR to `gastownhall/gascity` from `fix/ga-nan-mcp-trust-prompt`, cross-linking
   #3466 / #3109 / #534, with the two layers as separate commits.
6. On acceptance/local install, ga-psn.2.6 acceptance (supervisor + managed Dolt +
   call-control alive) can be confirmed.
