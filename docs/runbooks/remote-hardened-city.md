---
title: Operate a Direct Hardened City from the gc CLI
description: Stand up a self-hosted city that accepts remote writes over a signed X-GC-City-Write grant, wire a gc context to it, and run rig add --git-url and sling against it — with the threat posture and accepted residual risks stated up front.
---

This runbook is for operators standing up a **direct, self-hosted, hardened
city** that a `gc` client mutates over the HTTP+SSE control plane — no hosted
edge, no external broker. The capstone it enables is the one-liner:

```bash
gc --context prod rig add --git-url https://github.com/org/repo.git --name web \
  && gc --context prod sling <agent> <bead-id>
```

`rig add --git-url` drives **server-side** provisioning (the server clones the
repo, inits beads, composes packs) and `sling` routes an existing bead into the
new rig — both authenticated by a fresh, request-bound grant your machine mints.

> **Read [Prerequisites and threat posture](#1-prerequisites-and-threat-posture)
> first.** A hardened city exposes a **fully unauthenticated read plane**; the
> network front is not optional.

## 1. Prerequisites and threat posture

- **One controller replica only.** The grant replay guard and the rig-create
  in-flight index are process-local. A second controller against the same city
  reopens the grant replay window and races double-clones. Nothing in code
  detects a second replica — this is an operator rule.
- **The read plane is FULLY UNAUTHENTICATED.** Write-auth gates *mutations*
  only. Anyone who can reach the port can read every bead payload, all mail,
  session peeks and transcripts, and the entire event stream — **including the
  202 rig-provisioning progress**. A network/TLS front (reverse proxy, private
  network, or firewall) is **REQUIRED**, not optional. In-band read auth is later work.
- **TLS.** `gc` refuses a plain-`http` non-loopback URL at context validation.
  Terminate TLS in front of the city and, if the cert is private, pass its CA to
  the context with `--ca-file`.

## 2. Mint the city keypair

The server holds only the **public** key; the private key stays on the operator
machine (`0600`). `gc-write-mint` accepts a PEM PKCS#8, or a raw / hex / base64
32-byte ed25519 seed.

```bash
mkdir -p ~/.gc/keys && chmod 700 ~/.gc/keys

# Generate a PKCS#8 private key.
openssl genpkey -algorithm ed25519 -out ~/.gc/keys/city.ed25519
chmod 600 ~/.gc/keys/city.ed25519

# Extract the raw 32-byte public key and base64 it for the server config.
openssl pkey -in ~/.gc/keys/city.ed25519 -pubout -outform DER \
  | tail -c 32 | base64
# -> <base64 ed25519 pubkey>
```

The verify key is configured as `kid:base64pub` (comma-separable for rotation),
e.g. `k1:<base64 ed25519 pubkey>`.

## 3. Configure and boot the hardened city

`city.toml`:

```toml
[api]
port = 9443            # behind the TLS front
bind = "0.0.0.0"       # non-loopback => read-only unless allow_mutations
allow_mutations = true
write_auth_verify_key = "k1:<base64 ed25519 pubkey>"
```

**Exactly one key source.** The `GC_CITY_WRITE_PUBKEY` env **overrides** the
config key — set one, never both. `GC_CITY_WRITE_EPOCH_FLOOR` is env-only.

**Boot behavior matrix:**

| Bind + config | Result |
|---|---|
| verify key present | boots hardened; prints the loud read-plane warning (below) |
| no key, no ack | **refuses to boot** (G10 fail-closed) |
| no key + `write_auth_allow_unverified = true` (or `GC_CITY_WRITE_ALLOW_UNVERIFIED=1`) | boots with an **unauthenticated write plane** — only ever behind a trusted network front |

On any non-loopback bind that allows mutations, boot now emits a **loud
warning** naming the unauthenticated read surface — this is expected:

```text
WARNING: 0.0.0.0 is a non-loopback bind with mutations enabled — the READ plane is UNAUTHENTICATED.
  Anyone who can reach this port can read, with no credential:
    - beads (work items and their payloads) and mail
    - session peeks and full transcripts
    - the event stream, including 202 rig-provisioning progress
  Write-auth gates MUTATIONS only (posture: grant-gated — every mutation requires a signed X-GC-City-Write grant).
  A network/TLS front (reverse proxy, private network, or firewall) is REQUIRED, not optional.
```

With the ack knob instead of a key, the posture line reads `UNVERIFIED — no
write-auth verify key is set; mutations are gated ONLY by the network front`.

**Supervisor-managed variant.** A `gc supervisor` deployment must list the
public hostname in `[supervisor] allowed_hosts` or every request dies **421**.
The standalone `gc controller` allows any host (the network front is the
boundary), so it needs no `allowed_hosts`.

**Supervisor that is ALSO a local control plane: `[supervisor.hardened]`.**
`[supervisor] write_auth_verify_key` gates *every* per-city mutation on the
supervisor's single listener — including the ones first-party callers make with
only the CSRF header (the dashboard SPA, workspace-service adapters such as a
chat bridge posting `POST /v0/city/{c}/session/{id}/messages`, the local gc
client). On a machine where the supervisor is the trusted loopback control
plane *and* must accept remote writes, put the grant gate on a **second
listener** instead and leave the primary loopback listener as it is:

```toml
[supervisor]
port = 8372                       # primary, loopback, first-party (unchanged)
allowed_hosts = ["city.example.ts.net"]

[supervisor.hardened]
bind = "127.0.0.1"                # default; reachable only through the TLS front on this host
port = 8447
write_auth_verify_key = "peer:<base64 ed25519 pubkey>"   # REQUIRED; no unverified ack knob
```

The hardened listener serves the same mux, host/CORS/audit chain, and
read-auth posture as the primary, with its own write gate **plus a front door
the primary does not have**: it refuses (403) every supervisor-scope mutation
(above all `POST /v0/city` — city creation with a caller-chosen
`start_command`, which write-auth exempts because a not-yet-created city has
no name to bind a grant to), the whole `/svc/*` workspace-service pass-through
(any method — a GET can be a WebSocket upgrade the proxy then tunnels), and any
protocol upgrade on any path. Only safe reads and grant-signed
`/v0/city/{city}/…` mutations are served. Front the hardened port with your
TLS edge (e.g. `tailscale serve --https=8443 http://127.0.0.1:8447`,
tailnet-only) and register that URL as the peer's context. Boot is fail-closed
and all-or-nothing: an enabled hardened port without a key, a hardened port
equal to the primary address, a hardened kid that also appears in the primary
key list (each verifier keeps its own single-use replay guard), a read-only
primary bind, or a hardened bind failure refuses to start with **no** listener
up. `GC_CITY_WRITE_PUBKEY` is *not* consulted for the hardened key — that env
override belongs to the primary gate. The read plane on the hardened port is
exactly the primary's (see §1), so it still needs the network front.

## 4. Configure the client context

```bash
gc context add prod \
  --url  https://city.example.com:9443 \
  --city example-city \
  --grant-command "gc-write-mint --kid k1 --key ~/.gc/keys/city.ed25519 --city example-city" \
  --ca-file ~/.gc/keys/city-front-ca.pem   # only if the TLS front uses a private CA

gc context current    # dry-run: prints the winning tier and what it shadowed
```

The context is stored `0600` in `$GC_HOME/contexts.toml`. **Pin `--city` on the
minter** — `gc-write-mint` refuses to sign a request for any other city. The
grant command re-validates the audience and city and recomputes the request
digest before signing; the private key never enters `gc`.

## 5. The one-liner

```bash
gc --context prod rig add --git-url https://github.com/org/repo.git --name web \
  && gc --context prod sling <agent> <bead-id>
```

Expected transcript:

```text
# stderr — the resolved target echo
target: example-city @ https://city.example.com:9443 (context: prod, cred: grant:gc-write-mint …, source: flag --context)

# stdout — streamed provisioning progress, then the terminal line
Cloning rig working tree from git
Adding rig 'web'...
Prefix: web
Default branch: main
Initialized beads database
Generated routes.jsonl for cross-rig routing
Rig added.
provisioned → web (prefix web, branch main)

# stdout — the sling result
slung → <agent> (<bead-id>)
```

Watch the provision live from a second terminal:

```bash
gc --context prod events --follow --type rig.provision.progress \
  --payload-match request_id=<id>
```

**Remote sling contract.** A remote sling is the **2-arg explicit-target,
existing-bead** shape only: `gc --context prod sling <agent> <bead-id>`. Inline
text, `--stdin`, and the 1-arg target-inference form are refused (a remote city
cannot see your local rig config or create a local bead).

### 5a. Cross-city mail (city ↔ city, real sender identity)

Two hardened cities can mail each other with `gc` alone — no ssh, no
`--from human`. Each city holds its own private key and configures the *peer's*
public key (kid = peer city name) on its hardened listener:

```bash
# on city A (context "b" points at city B's hardened port; grant signed with A's key, kid "a")
gc --context b mail send mayor -s "hello from A" -m "…"
# -> stored in B with From "a/<A's identity>" (e.g. a/mayor); B's `gc mail inbox mayor` shows it.
gc --context b mail inbox           # mail addressed to "a/<identity>" inside B — where B's plain `gc mail reply` lands
# on city B, answering
gc --context a mail send mayor -s "Re: hello from A" -m "…"
```

The sender defaults to `<local city>/<identity>` (`--from` overrides it); `--all`
and `--notify` are refused for a remote city. gc has no cross-city addressing
yet, so answer with `gc --context <city> mail send`, not a bare `gc mail reply`.
`gc --context <peer> mail inbox` pages through the peer's whole unread mailbox
and refuses to render a partial (degraded) read.

**Naming rule.** The peer stores an unknown sender literally, but if it has a
*session* whose alias equals the qualified sender (a rig named after the sending
city, so `citadel/mayor` is one of its own agents) its beadmail binds the
message to that local session and a reply routes there. Do not name a rig after
a city that mails you.

## 6. Failure and resume recipes

The CLI prints these recipes itself; here is what each means.

- **Lost stream / deadline** (`rig_stream_lost`, `rig_stream_deadline`): the
  provision **continues server-side**. Resume idempotently by re-running the
  *exact* printed command — same `--request-id`, same digest-affecting flags
  (`--name` / `--prefix` / `--default-branch` / `--git-url`). An omitted flag
  must stay omitted, or the digest mismatches and the server returns 409. Or
  watch passively:

  ```bash
  gc --context prod events --watch --type request.result.rig.create \
    --payload-match request_id=<id>
  ```

- **`rig_provision_failed`** (e.g. `clone_failed`, `blocked_host`): the server
  **rolled back to no-rig**. Retry the **same `--request-id`** to re-clone
  cleanly — the rolled-back record is re-executable.

- **`rig_name_conflict` with an in-flight id**: another request is already
  provisioning that name. Watch *its* stream (the printed recipe); never re-POST
  your body under its id (that 409s).

- **SPA 401 by design.** The dashboard loads fine but **401s on every
  mutation** — it mints no grant. Operate writes through `gc` with the grant.

- **401 on `gc` mutations.** The context lacks a `grant_command`, the kid/key
  mismatches the server, or the epoch floor moved. Check the server audit log;
  the client-facing body is deliberately generic (no verification oracle).

## 7. Validate (the manual real-clone proof)

The automated tests stub the git-fetch boundary, so this is the honest home for
the real-git / real-DNS / real-TLS proof. Run it once per release against a
hardened city fronted by TLS, with a **real** public repo URL:

1. **Boot warning present.** Restart the city; confirm the unauthenticated
   read-plane warning from [§3](#3-configure-and-boot-the-hardened-city) prints.
2. **Grant-less mutation 401s.** A raw `curl` POST with only the CSRF header is
   refused:

   ```bash
   curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
     -H 'X-GC-Request: true' \
     https://city.example.com:9443/v0/city/example-city/rigs
   # -> 401
   ```
3. **`/svc` mutation 403s.** A workspace-service mutation is refused on a
   hardened bind (G11):

   ```bash
   curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
     -H 'X-GC-Request: true' \
     https://city.example.com:9443/v0/city/example-city/svc/whatever
   # -> 403
   ```
4. **The one-liner succeeds** against a real repo (the transcript in
   [§5](#5-the-one-liner)).
5. **Replay is idempotent.** Re-run the rig-add with the same `--request-id`:

   ```text
   exists → web (idempotent replay)
   ```

## 8. Accepted residual risks

These are known and accepted for the direct-hardened deployment. Treat them as
operating constraints, not bugs.

- **Single-replica only.** The replay guard (grant `jti`) and the rig-create
  in-flight index are process-local. A second controller against the same city
  reopens grant replay (bounded by the ≤2 m TTL + 30 s skew) and races
  double-clones. Run exactly one controller per hardened city.
- **Unauthenticated read plane.** Everything readable is readable by anyone who
  reaches the port. The mitigation is the network/TLS front — a mandatory part
  of the deployment, not an add-on.
- **Same-user grant trust.** Anyone who can exec as the operator user can run
  `grant_command` and mint valid grants. `0600` on `contexts.toml` and the key
  file is the boundary; treat helper-exec access as write access. (A credential
  embedded in a `--git-url` is also argv-visible to a same-user `ps` during the
  server-side clone — an accepted residual.)
- **Repo-content trust (single-tenant).** The clone hardening blocks transport
  abuse (`ext::` / `file://` / SSRF / hooks / submodules), but the **cloned
  content** then runs inside pipeline agents. Only add repos you would run
  locally.
- **DNS-rebinding TOCTOU.** The strict SSRF fence resolves once, fail-closed;
  git re-resolves at fetch. Redirects are refused and rebind-to-SERVFAIL is
  blocked, but a fast A-record flip between fence and fetch remains
  theoretically open — the host's egress firewall is the backstop.
