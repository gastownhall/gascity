---
title: "Startup Roadmap"
---

Created 2026-02-23 after implementing the 5-step startup sequence
(ensureFreshSession, waitForCommand, acceptBypass, waitForReady,
verifySurvived) in `internal/session/tmux/adapter.go`.

Gastown's `StartSession()` in `internal/session/lifecycle.go` has 15
steps. We implemented 7 (5 original + SetRemainOnExit + zombie crash
capture). This file tracks what's missing and when to add each piece.

## Source reference

Gastown file: `/data/projects/gastown/internal/session/lifecycle.go`
Gas City file: `/data/projects/gascity/internal/session/tmux/adapter.go`

---

## Tutorial 02-03: Settings & Slash Commands

### EnsureSettingsForRole (gastown step 2)

- **What:** Installs provider-specific settings files (Claude's
  `settings.json`, OpenCode orders) and provisions slash commands
  into the working directory.
- **Gastown code:** `runtime.EnsureSettingsForRole(settingsDir, workDir, role, runtimeConfig)`
  in `internal/runtime/runtime.go` lines 46-77.
- **Why we need it:** When agents need slash commands and hooks.
  Without it, agent starts with default settings instead of Gas
  City-specific ones.
- **When:** Tutorial 02 (named crew with hooks) or 03 (agent loop).

---

## Tutorial 04: Theme & Multi-Agent UX

### ConfigureGasTownSession (gastown step 9)

- **What:** Applies color theme, status bar format (rig/worker/role),
  dynamic status, mouse mode, and key bindings (mail, feed, agents,
  cycle-sessions). The entire tmux UX layer.
- **Gastown code:** `t.ConfigureGasTownSession(sessionID, theme, rig, agent, role)`
  in `internal/tmux/tmux.go` lines 1920-1948.
- **Suboperations:**
  - `ApplyTheme` — color scheme
  - `SetStatusFormat` — status bar with rig/worker/role info
  - `SetDynamicStatus` — live status updates
  - `SetMailClickBinding` — click to read mail
  - `SetFeedBinding` — feed key binding
  - `SetAgentsBinding` — agents panel binding
  - `SetCycleBindings` — cycle between agent panes
  - `EnableMouseMode` — mouse support
- **Why we need it:** Navigation with multiple agents. Without it,
  sessions are plain tmux — functional but hard to tell apart and
  no quick-nav between agents.
- **When:** Tutorial 04 (agent team, multiple agents).
- **Note:** We already extracted `internal/session/tmux/theme.go` with
  `AssignTheme`, `GetThemeByName`, `ThemeStyle`. The tmux operations
  themselves are in `tmux.go`. Wiring is the remaining work.

---

## Tutorial 05b: Health Monitoring (package deal)

These four steps are interdependent. They arrive together.

### SetRemainOnExit (gastown step 7) — DONE

- **Implemented:** Called unconditionally in `doStartSession` after
  `ensureFreshSession`, best-effort. Added to `startOps` interface.
- **Zombie crash capture:** reconcile peeks last 50 lines from zombie
  panes and emits `agent.crashed` event before restart.
- **Commit:** 4df81f5

### SetEnvironment post-create (gastown step 8) — NOT NEEDED

- **What:** Gastown calls `tmux set-environment` after session creation
  to set GT_ROLE, GT_RIG, etc. at the session level, separate from
  `-e` flags on `new-session`.
- **Gastown's reasoning:** Believed `-e` flags don't survive respawn.
- **Empirically false (tmux 3.5a):** Tested 2026-02-26. `-e` flags
  set on `new-session` ARE inherited by `respawn-pane`, `new-window`,
  and `split-window`. Both `-e` and `set-environment` vars appear in
  respawned processes. The only difference: `set-environment` vars are
  NOT visible to the initial process (already forked), while `-e` vars
  ARE visible to the initial process.
- **Conclusion:** Gas City's current approach (passing env via `-e`
  flags in `NewSessionWithCommandAndEnv`) is sufficient. No post-create
  `set-environment` step needed. If we ever need to set env vars AFTER
  session creation (e.g., runtime-discovered values), `SetEnvironment`
  is available via the Provider's `SetMeta`/`GetMeta` for metadata, but
  it's not needed for identity vars like GC_AGENT.

### SetAutoRespawnHook (gastown step 11)

- **What:** Sets tmux `pane-died` hook: sleep 3s -> `respawn-pane -k`
  -> re-enable remain-on-exit (because respawn-pane resets it to off).
  This is the "let it crash" / Erlang supervisor mechanism.
- **Gastown code:** `t.SetAutoRespawnHook(sessionID)` — tmux.go lines
  2368-2403.
- **Hook command:** `run-shell "sleep 3 && tmux respawn-pane -k -t '<session>' && tmux set-option -t '<session>' remain-on-exit on"`
- **Dependencies:** Requires SetRemainOnExit (done).
- **PATCH-010 reference:** Fixes Deacon crash loop.
- **Why we need it:** Dead agents stay dead without it. For daemon mode
  with unattended agents, this is critical.

### TrackSessionPID (gastown step 15)

- **What:** Captures pane PID + process start time, writes to
  `.runtime/pids/<session>.pid`. Defense-in-depth for orphan cleanup.
- **Gastown code:** `TrackSessionPID(townRoot, sessionID, t)` —
  `internal/session/pidtrack.go` lines 36-56.
- **Why we need it:** If tmux itself dies or KillSession fails, the
  controller can find and kill orphaned processes by PID.
- **When:** Arrives with health monitoring / daemon mode.

---

## Not needed (permanent exclusions or handled differently)

| Gastown step | Why not needed |
|---|---|
| Step 1: ResolveRoleAgentConfig | Done in CLI via `config.ResolveProvider()` |
| Steps 3-5: Build command + env | Done in `agent.managed.Start()` + `cmd_start.go` |
| Step 13: SleepForReadyDelay | Handled inside `WaitForRuntimeReady` fallback |
| Step 4: ConfigDirEnv prepend | Gas City uses `-e` flags; `-e` survives respawn |
| Step 8: SetEnvironment post-create | `-e` flags survive respawn (verified tmux 3.5a) |

---

## Implementation notes

When implementing the 05b health monitoring cluster:

1. **SetRemainOnExit is done.** Already called unconditionally in
   `doStartSession`. SetAutoRespawnHook is the remaining piece.

2. **`-e` flags survive respawn.** Verified empirically on tmux 3.5a.
   No need for post-create `set-environment` for identity vars.
   GC_AGENT and other `-e` vars will be available to respawned panes.

3. **KillSessionWithProcesses** (gastown uses this instead of plain
   KillSession for cleanup) — kills descendant processes before
   killing the session. Important when agents spawn child processes.
   We already have this in `Provider.Stop()`.

4. **Add SetAutoRespawnHook to startOps interface** so it remains
   unit-testable via fakeStartOps.
