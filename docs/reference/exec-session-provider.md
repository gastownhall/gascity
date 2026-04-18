---
title: "Exec Session Provider"
---

Gas City's exec session provider delegates each `runtime.Provider` operation
to a user-supplied script. This allows any terminal multiplexer or process
manager to be used as a session backend without writing Go code.

## Usage

Set the `GC_SESSION` environment variable to `exec:<script>`:

```bash
# Absolute path
export GC_SESSION=exec:/path/to/gc-session-screen

# PATH lookup
export GC_SESSION=exec:gc-session-screen
```

## Calling Convention

The script receives the operation name as its first argument:

```
<script> <operation> <session-name> [args...]
```

No shell invocation — the script is exec'd directly.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Failure (stderr contains error message) |
| 2 | Unknown operation (treated as success — forward compatible) |

Exit code 2 is the forward-compatibility mechanism. When Gas City adds new
operations in the future, old scripts return exit 2 and the provider treats
it as a no-op success. Scripts only need to implement the operations they
care about.

## Operations

| Operation | Invocation | Stdin | Stdout |
|-----------|-----------|-------|--------|
| `start` | `script start <name>` | JSON config | — |
| `stop` | `script stop <name>` | — | — |
| `interrupt` | `script interrupt <name>` | — | — |
| `is-running` | `script is-running <name>` | — | `true` or `false` |
| `attach` | `script attach <name>` | tty passthrough | tty passthrough |
| `process-alive` | `script process-alive <name>` | process names (1/line) | `true` or `false` |
| `nudge` | `script nudge <name>` | message text | — |
| `set-meta` | `script set-meta <name> <key>` | value on stdin | — |
| `get-meta` | `script get-meta <name> <key>` | — | value (empty = not set) |
| `remove-meta` | `script remove-meta <name> <key>` | — | — |
| `peek` | `script peek <name> <lines>` | — | captured text |
| `list-running` | `script list-running <prefix>` | — | one name per line |
| `get-last-activity` | `script get-last-activity <name>` | — | RFC3339 or empty |

### Start Config (JSON on stdin)

The `start` operation receives a JSON object on stdin:

```json
{
  "work_dir": "/path/to/working/directory",
  "command": "claude --dangerously-skip-permissions",
  "env": {"GC_AGENT": "mayor", "GC_CITY": "/home/user/bright-lights"},
  "process_names": ["claude", "node"],
  "nudge": "initial prompt text",
  "pre_start": ["mkdir -p /workspace", "git clone repo /workspace"],
  "session_setup": ["./scripts/install-hooks.sh"],
  "session_setup_script": "/path/to/setup-script.sh",
  "session_live": ["./scripts/session-style.sh"],
  "pack_overlay_dirs": ["/path/to/pack-overlay"],
  "overlay_dir": "/path/to/agent-overlay",
  "copy_files": [
    {"src": "/tmp/settings.json", "rel_dst": ".gc/settings.json"}
  ]
}
```

All fields are optional (omitted when empty).

### Startup Hints

The JSON config contains startup hints shared with other session providers
that do multi-step orchestration. The exec provider itself is fire-and-forget — it
calls `script start` and returns immediately. Scripts may handle these
hints or ignore them:

- **`process_names`** — providers that verify readiness can poll for these
  process names to appear in the session's process tree (30s timeout)
  before considering the agent "started." A script can implement this by
  polling its backend's process tree after session creation, or ignore it
  for fire-and-forget behavior (like the subprocess provider does).

- **`nudge`** — text that an interactive provider can send into the session
  after the agent is ready. Scripts that support interactive input can
  handle this in `start` (send the text after session creation) or leave
  it to the separate `nudge` operation which gc calls after `start` returns.

- **`pre_start`** — array of shell commands to run on the target
  filesystem **before** the session is created. Used for directory
  preparation, worktree creation, or other setup that must exist before
  the agent starts. Scripts should execute each command in the target
  environment before creating the session. Fatal: abort startup if a
  command fails so the agent never launches into an unprepared workDir.

- **`session_setup`** — array of shell commands to run on the target
  filesystem after the session is created and ready, before returning.
  These are provider hints, not a hardcoded `gc` execution model: the
  built-in tmux backend runs its equivalent commands from the controller
  process, while exec scripts typically execute each command inside the
  session environment
  (e.g. `kubectl exec -- sh -c '<cmd>'` for K8s, `docker exec -- sh -c
  '<cmd>'` for Docker, or plain `sh -c '<cmd>'` for local providers).
  Non-fatal: warn on stderr if a command fails, but don't abort start.

- **`session_setup_script`** — path to a script on the controller
  filesystem, run after `session_setup` commands. For remote providers
  (K8s, Docker), read the file locally and pipe its contents into the
  session (e.g. `kubectl exec -i -- sh < script`). For local providers,
  run directly via `sh -c`. Non-fatal like `session_setup`.

Fields that are **not** included in the JSON (gc-internal, not part of
the exec protocol):

- `ready_prompt_prefix` — prompt prefix for readiness detection (gc polls
  via `peek` after `start` returns)
- `ready_delay_ms` — fixed delay fallback (gc sleeps after `start` returns)
- `emits_permission_warning` — bypass-permissions dialog handling
- `fingerprint_extra` — config change detection metadata

The distinction: readiness polling and delay are the *caller's*
responsibility. Session setup commands are the *script's* responsibility
— they run on the target filesystem, not the controller.

### Conventions

- **stdin for values**: `set-meta`, `nudge`, and `start` pass data on stdin
  to avoid shell quoting and argument length limits.
- **stdout for results**: `is-running`, `process-alive` return `true`/`false`.
  `get-meta` returns the value or empty for unset. `list-running` returns one
  name per line.
- **Idempotent stop**: `stop` must succeed (exit 0) even if the session
  doesn't exist.
- **Best-effort interrupt/nudge**: Return 0 even if the session doesn't exist.
- **Empty = unsupported**: `get-last-activity` returning empty stdout means
  the backend doesn't support activity tracking (zero time in Go).

## Writing Your Own Script

1. Start with `contrib/session-scripts/gc-session-screen` as a template.
2. Implement the operations your backend supports.
3. Return exit 2 for operations you don't support.
4. Test with `GC_SESSION=exec:./your-script gc start <city>`.

### Minimal script (start/stop/is-running only)

```bash
#!/bin/sh
op="$1"
name="$2"
case "$op" in
  start)     cat > /dev/null; my-mux new "$name" ;;
  stop)      my-mux kill "$name" 2>/dev/null; exit 0 ;;
  is-running) my-mux list | grep -q "^${name}$" && echo true || echo false ;;
  *)         exit 2 ;;
esac
```

## Environment Variables

Scripts can use `GC_EXEC_STATE_DIR` (if set) as a directory for sidecar
state files (metadata, wrappers). If not set, scripts should use a
reasonable default under `$TMPDIR` or `/tmp`.

## Shipped Scripts

See `contrib/session-scripts/` for maintained implementations:

- **gc-session-screen** — GNU screen backend. Dependencies: `screen`,
  `jq`, `bash`.
