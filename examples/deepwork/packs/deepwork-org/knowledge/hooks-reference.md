# Hooks Reference — Claude Code Lifecycle Hooks

Gas Town uses Claude Code hooks to inject behavior at key moments in every agent's lifecycle.

## Shared Base (~/.gt/hooks-base.json)

Applied to ALL agents via `gt hooks sync`.

| Event | Matcher | Command | Purpose |
|-------|---------|---------|---------|
| SessionStart | (all) | `gt prime --hook; cold-start-recovery.sh` | Load role context, recover from crashes |
| Stop | (all) | `graceful-handoff.sh --reason session-stop; gt costs record` | Save state to mail, log changelog, record costs |
| PreCompact | (all) | `gt handoff --auto --collect; graceful-handoff.sh --reason compaction` | Preserve context before compression |
| UserPromptSubmit | (all) | `timeout 5 gt mail check --inject` | Check for incoming mail on every user message |
| PreToolUse | `Bash(gh pr create*)` | `gt tap guard pr-workflow` | Block GitHub PRs, enforce Gitea |
| PreToolUse | `Bash(git checkout -b*)` | `gt tap guard pr-workflow` | Control branching |
| PreToolUse | `Bash(git switch -c*)` | `gt tap guard pr-workflow` | Control branching |
| PreToolUse | `Task` | `gt tap guard task-dispatch` | Intercept task creation |

## Hook Scripts

### graceful-handoff.sh
Runs on every session end (Stop) and compaction (PreCompact).
- Collects: hooked work, git state, recent events, inbox count
- Sends handoff mail to self (pinned, permanent)
- Logs session activity to changelog (if dirty work or hooked bead)
- Falls back to /tmp file if Dolt is down

### cold-start-recovery.sh
Runs on SessionStart when gt prime detects no handoff context.
- Queries event log to reconstruct predecessor's state
- Prints context summary injected into agent prompt

## Known Issues

### Mayor override is weaker than base
Mayor's settings.json replaces (not extends) the base hooks. Missing:
- graceful-handoff.sh on Stop
- handoff collection on PreCompact
- Task guard on PreToolUse

Fix: align mayor override with base, or remove override and rely on base.

## Per-Role Overlays

The reference implementation (gascity) supports per-role overlays:
- Default: just PreCompact handoff
- Witness: PreToolUse blockers that prevent patrol formula issues
- These are defined in pack overlay directories

Gas Town currently has no per-role overlays (empty ~/.gt/hooks-overrides/).
