---
title: "Transport Security (mTLS / Certificate Lifecycle)"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: security
current_state: not-implemented
priority: medium
author: trillium
date: 2026-03-21
upstream_ref: "steveyegge/gastown#2852, steveyegge/gastown#2794"
labels: [security, mtls, certificates, transport]
---

# Transport Security (mTLS / Certificate Lifecycle)

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Upstream Reference

Draws from [steveyegge/gastown#2852](https://github.com/steveyegge/gastown/issues/2852)
(bootstrap & mTLS transport) and the security architecture in
[steveyegge/gastown#2794](https://github.com/steveyegge/gastown/issues/2794).

## Summary

When agents communicate across machines, the transport needs authentication and
encryption. The Gastown satellite work used mTLS with per-agent certificates.
Gas City needs to decide what security model to use for cross-machine communication.

## Current State: Not Implemented

Gas City has no cross-machine transport security. The existing providers are:

| Provider | Transport | Security |
|----------|-----------|----------|
| tmux | Local Unix socket | OS-level (same machine) |
| exec | Local subprocess pipes | OS-level (same machine) |
| ACP | Local stdio pipes | OS-level (same machine) |
| K8s | Kubernetes API | K8s RBAC + service accounts |

No provider communicates over the network without Kubernetes.

## Gastown's mTLS Design (Reference)

The Gastown satellite work (#2794, #2852) implemented:

### Certificate Lifecycle

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| TTL | 720h (30 days) | Default in admin API |
| Renewal | None (v1) | Re-sling gets a new cert |
| Revocation | `POST /v1/admin/deny-cert` | In-memory deny list |
| Key type | ECDSA P-256 | Sub-millisecond issuance |
| CN format | `gt-<rig>-<name>` | Identity derived from cert |

### Bootstrap Sequence

```
Hub:   Issue cert via admin API → deliver cert to satellite via SSH
Sat:   Agent starts with cert → all gt/bd calls go through mTLS proxy
Hub:   Proxy validates cert → forwards to local gt/bd/dolt
```

### Security Properties

- Every cross-machine call is authenticated (agent identity in cert CN)
- Every cross-machine call is encrypted (TLS)
- Revocation is immediate (deny-cert endpoint)
- Compromise of one cert doesn't affect others (per-agent certs)

### Known Limitations (accepted for v1)

- Admin API is unauthenticated (any process on hub localhost can issue certs)
- Deny list is in-memory (proxy restart clears revocations)
- No connection pooling (fresh TLS per call, ~50-100ms overhead)

## Design Options for Gas City

### Option A: Tailscale-Only (simplest)

If all machines are on a Tailscale network, Tailscale provides:
- Encrypted transport (WireGuard)
- Machine identity (Tailscale node keys)
- ACLs (Tailscale access controls)
- SSH (Tailscale SSH with identity)

**Pros**: Zero additional infrastructure, already deployed on our machines
**Cons**: Requires Tailscale, no per-agent identity (machine-level only)

### Option B: SSH Keys (moderate)

SSH with key-based authentication for all cross-machine operations:
- Machine identity via SSH host keys
- Agent identity via SSH user/key
- Encryption via SSH transport

**Pros**: Simple, well-understood, works everywhere
**Cons**: No per-agent identity without per-agent keys, connection overhead

### Option C: mTLS (Gastown's approach)

Full mTLS with per-agent certificates:
- Per-agent identity (cert CN = agent name)
- Mutual authentication (both sides verify)
- Certificate rotation and revocation

**Pros**: Strongest security model, per-agent identity, production-grade
**Cons**: Most complex, requires CA management, cert lifecycle

### Option D: Tailscale + SSH (recommended for v1)

Combine Tailscale for network-level security with SSH for operations:
- Tailscale encrypts all traffic and provides machine identity
- SSH provider (003) uses Tailscale SSH or standard SSH keys
- No additional certificate infrastructure needed
- mTLS can be added later if per-agent identity is needed

**Pros**: Minimal infrastructure, strong security, works with our setup
**Cons**: No per-agent identity (acceptable for personal machines)

## Recommendation

Start with **Option D (Tailscale + SSH)** for our mini2/mini3 setup. This gives
us encrypted, authenticated transport with zero additional infrastructure.

If Gas City later needs per-agent identity (multi-tenant, shared infrastructure),
Option C (mTLS) can be added as a transport layer without changing the provider
interface.

### The "City Doesn't Care" Principle

The security model is an infrastructure concern. Agents don't know or care whether
their calls go through mTLS, SSH, or plain localhost. The provider abstraction
handles this transparently.

## Audit Findings (2026-03-21)

Traced against Gas City codebase. **Issue is accurate — no mismatches found.**

### K8s Credential Pattern (Reusable)

K8s provider demonstrates a working credential delivery pattern:

1. K8s Secret `claude-credentials` mounted at `/tmp/claude-secret/` in pod
2. Init script copies to `$HOME/.claude/` (pod.go:80, 119-129)
3. RBAC separates agent (minimal: `pods.get` only) from controller (full pod lifecycle)

This pattern can inform SSH credential delivery: SCP credentials to remote before
agent start, or use SSH agent forwarding.

### Gap: Credential Delivery for SSH Satellites

Issue focuses on transport security but doesn't address how SSH-based agents receive
API keys. Options:
- SSH agent forwarding (inherit from controller)
- SCP credentials before agent start (K8s Secret model for SSH)
- Environment variable injection at session start

### Zero Security Code

Confirmed: no `crypto/tls`, `x509`, certificate parsing, or mTLS anywhere in codebase.
Only `crypto/rand` for session UUIDs and `crypto/sha256` for state isolation hashing.

## Dependencies

- [003 — Remote Transport](003-remote-transport.md) (transport layer that security wraps)
- [001 — Machine Registry](001-machine-registry.md) (machine identity and credentials)

## Dependents

- All cross-machine features rely on secure transport
