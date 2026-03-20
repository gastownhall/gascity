---
title: "K8s Session Provider Backlog"
---

Robustness issues identified during initial implementation review.
Items marked FIXED have been addressed.

## P0 — Will break in normal use

### 1. YAML injection in `start` handler — FIXED

Pod manifest is now generated as JSON via `jq` — all values are properly
escaped. No string interpolation into YAML.

### 2. `nudge` writes to dead-letter file — FIXED

Switched to tmux-inside-pod architecture. The pod runs tmux as its
session manager; nudge sends keystrokes via `kubectl exec -- tmux
send-keys`. Same semantics as the local tmux provider.

### 3. Pod name / label value sanitization — FIXED

Added `sanitize_label()` helper. All label queries go through it.
Pod names and label values are sanitized consistently.

## P1 — Fragile under real conditions

### 4. `peek` returns logs, not terminal output — FIXED

Switched to tmux-inside-pod. `peek` now uses `kubectl exec -- tmux
capture-pane -p`, which returns real terminal scrollback content.
Same semantics as the local tmux provider.

### 5. `start` returns before pod is schedulable — FIXED

`start` now calls `kubectl wait --for=condition=Ready` with a 120s
timeout after `kubectl apply`. Reports failure with phase info if
the pod doesn't reach Running.

### 6. `process-alive` race with pod termination

If the main process exits, pod enters Completed state. `kubectl exec`
fails on non-Running pods. `get_pod_name_by_label` filters to Running,
so `process-alive` returns "false" (correct). Low priority.

## P2 — Phase 1 acceptable, fix later

### 7. No `get-last-activity` support — FIXED

Now queries tmux `#{session_activity}` via `kubectl exec` and converts
the unix timestamp to RFC3339. Health patrol can detect idle agents.

### 8. `clear-scrollback` — FIXED

Now delegates to `kubectl exec -- tmux clear-history`. Works with
tmux-inside-pod architecture.

### 9. No `session_setup` support — FIXED (gc-session-k8s)

The `start` handler now parses `session_setup` commands and
`session_setup_script` from the start JSON and executes them inside
the pod via `kubectl exec -- sh -c`. For `session_setup_script`
(a file path on the controller), the script contents are piped into
the pod via `kubectl exec -i -- sh < script`. Non-fatal: warnings
on stderr if a command fails.

## gc-beads-k8s — Beads Runner

### 10. No `purge` support

The beads runner does not support the `purge` operation (exit 2). Closed
ephemeral beads accumulate in Dolt until manually cleaned up. For Phase 1
this is acceptable — purge is a dolt-specific optimization.

### 11. Single-prefix init

`ensure-ready` always initializes with prefix `gc`. If the city uses a
different prefix, run `gc-beads-k8s init <dir> <prefix>` explicitly
after ensure-ready, or configure `issue_prefix` via `config-set`.

### 12. Fixed pod name

The beads runner uses a fixed pod name (`gc-beads-runner`). Only one
instance per namespace. Multiple cities sharing a namespace would conflict.
Use separate namespaces per city if needed.
