---
title: "Remote Transport Layer"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: runtime
current_state: not-implemented
priority: high
author: trillium
date: 2026-03-21
labels: [runtime, transport, ssh, security]
---

# Remote Transport Layer

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

Gas City has no mechanism to spawn or manage agents on remote machines. All current
providers (tmux, exec, ACP) operate locally. The K8s provider reaches remote pods via
the Kubernetes API, but that requires a full K8s cluster. This issue covers building
a general-purpose remote transport for arbitrary machines.

## Current State: Not Implemented

### What Exists

| Provider | Scope | Transport |
|----------|-------|-----------|
| tmux | Local | Unix socket (tmux -L) |
| exec | Local | Subprocess stdin/stdout pipes |
| ACP | Local | JSON-RPC 2.0 over stdio |
| K8s | Remote (cluster) | Kubernetes API (client-go) |
| hybrid | Routing | Delegates to local or remote |

### What's Missing

- No SSH-based agent spawning
- No mechanism to run tmux commands on remote machines
- No persistent daemon on satellite machines
- No mTLS or other transport security (the Gastown satellite work had this)
- No proxy infrastructure for tunneling agent traffic

### Gastown Satellite Work (Reference)

The now-closed PRs (#2858–#2863) on steveyegge/gastown included:

- mTLS certificate issuance and bootstrap
- SSH-based remote polecat spawning (`gt polecat spawn --machine`)
- Proxy server on hub for tunneling
- Remote worktree creation
- Bootstrap sequence: cert → spawn → tmux → env → verify → cleanup

This was Go code in the gastown binary. The equivalent in Gas City would be a new
runtime provider.

## Design Options

### Option A: SSH Provider

A new `internal/runtime/ssh/` provider that implements `runtime.Provider` by
executing tmux commands over SSH:

```go
type SSHProvider struct {
    host       string
    user       string
    keyPath    string
    tmuxSocket string
}

func (p *SSHProvider) Start(ctx, name, cmd, workDir string) error {
    // ssh user@host "tmux -L gc new-session -d -s name 'cmd'"
}
```

**Pros**: Simple, uses existing tmux patterns, no daemon needed on remote
**Cons**: SSH connection per operation, latency, connection management

### Option B: Satellite Daemon

A lightweight `gc-satellite` daemon running on remote machines that accepts
commands over HTTP/gRPC:

```
Hub Controller → HTTP/gRPC → gc-satellite → local tmux
```

**Pros**: Persistent connection, lower latency, richer protocol
**Cons**: More infrastructure to deploy and manage

### Option C: SSH + Multiplexed Control Socket

SSH with `ControlMaster` for connection reuse:

```
~/.ssh/config:
Host mini3
    ControlMaster auto
    ControlPath ~/.ssh/sockets/%r@%h-%p
    ControlPersist 10m
```

**Pros**: Simple like Option A but with connection reuse
**Cons**: SSH-specific, platform differences

### Recommendation

Start with **Option A (SSH Provider)** with connection pooling. It's the simplest
path that works with our Tailscale setup. The satellite daemon (Option B) can come
later if SSH latency becomes a problem.

## Security Considerations

- SSH key authentication (no passwords)
- Tailscale provides encrypted transport and identity
- Agent credentials (Claude API keys) need secure propagation to satellites
- Consider: should satellites have their own API keys, or tunnel through hub?

## city.toml Configuration

```toml
[[machines]]
name = "mini3"
host = "mini3.hippo-tilapia.ts.net"
role = "satellite"

[machines.ssh]
user = "2020mini_2"
key = "~/.ssh/id_ed25519"
# Or rely on ssh-agent / Tailscale SSH
```

## Audit Findings (2026-03-21)

Traced against Gas City codebase. **Issue needs significant rewrite — the Gastown mTLS
approach is architecturally wrong for Gas City.**

### Critical Mismatch: Provider Model

Gas City treats providers as **session management**, not transport layers. The Provider
interface has **16 methods** that any new provider must implement:

```
Start, Stop, Interrupt, IsRunning, IsAttached, ProcessAlive,
Attach, Nudge, SendKeys, CopyTo, Peek, SetMeta, GetMeta, RemoveMeta,
ListRunning, GetLastActivity, ClearScrollback, RunLive, Capabilities
```

Plus optional extension interfaces: `InteractionProvider`, `IdleWaitProvider`,
`ImmediateNudgeProvider`.

### The `exec:<script>` Provider May Suffice

Gas City already has an `exec` provider (`internal/runtime/exec/exec.go`) that delegates
**every operation** to a user-supplied script. The script receives the operation as argv[1]:
`start`, `stop`, `nudge`, `is-running`, etc.

A user could write an SSH-based script that implements all operations without any Go code:

```toml
[session]
provider = "exec:scripts/ssh-provider.sh"
```

This makes a native SSH provider **optional** — the exec provider + script is the
Gas City-native pattern for custom transport.

### If Building a Native SSH Provider

Would live at `internal/runtime/ssh/` and follow the tmux provider pattern:
- **Per-session nudge locks** (channel-based semaphores) for concurrency safety
- **Connection pooling** via SSH ControlMaster
- Register in `newSessionProviderByName()` at `providers.go:85-116`

### Key Code Locations

| File | Lines | What |
|------|-------|------|
| `internal/runtime/runtime.go` | 65-152 | Full Provider interface (16 methods) |
| `internal/runtime/tmux/adapter.go` | full | Reference implementation |
| `internal/runtime/exec/exec.go` | full | Script-delegating provider |
| `internal/runtime/k8s/provider.go` | full | Remote execution patterns |
| `cmd/gc/providers.go` | 85-116 | Provider factory switch |

### Gastown Patterns NOT Applicable

- mTLS proxy bootstrap → Gas City uses provider abstraction instead
- `gt-proxy-client` shim → not needed; provider handles routing
- Cert lifecycle → Tailscale provides transport security (issue 013)

## Dependencies

- [001 — Machine Registry](001-machine-registry.md) (machine definitions)

## Dependents

- [002 — Hybrid Provider Config](002-hybrid-provider-config.md) (remote backend)
- [004 — Cross-Machine Dispatch](004-cross-machine-dispatch.md) (dispatch needs transport)
- [008 — Remote Health](008-remote-health.md) (health checks need transport)
- [009 — Session Tracking](009-session-tracking.md) (remote session discovery)
