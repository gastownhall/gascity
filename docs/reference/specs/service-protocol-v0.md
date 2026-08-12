---
title: Gas City Service Protocol — v0
description: Authoritative specification for the generic hosted-service wire protocol used by gc login.
---

| Field | Value |
|---|---|
| Status | Authoritative specification |
| Last verified | 2026-07-15 |
| Contract | `gascity.dev/service/v0` |
| Primary implementation | `internal/cliauth`, `cmd/gc` (`gc login`, `gc whoami`) |
| Concept model | [How Gas City Works](/getting-started/how-gas-city-works) — the Agent (WHO) primitive |
| Default endpoint | `https://gascity.com` (a flag default, not part of the contract) |

# Gas City Service Protocol v0

Version string: `gascity.dev/service/v0`

## What this is

A small, generic HTTP protocol that lets the `gc` CLI authenticate to a
**Gas City service** and act against it — the same way `docker login
<registry>` authenticates to any conforming container registry and
`registry-1.docker.io` merely happens to be the default. `gc login`
speaks this protocol; **`https://gascity.com` is only the default value of
a flag.** Any server that implements the endpoints below works with an
unmodified `gc` binary:

```
gc login                                  # → the default, https://gascity.com
gc login --at https://gc.mycorp.example   # → a self-hosted conforming server
```

The protocol is deliberately dumb. The CLI holds an **opaque bearer token**
it never parses; it opens URLs the server returns and prints strings the
server authored. Everything a product does behind auth — account creation,
trial credits, org selection, plan enforcement — lives entirely server-side
and is invisible to the CLI. This is not only a design preference: it is the
property that keeps the OSS client free of any vendor-specific or commercial
logic. **Generic and vendor-neutral are the same constraint.**

This document specifies the **auth + identity** surface that `gc login` and
`gc whoami` require. Two further resource families — **cities** (provision a
hosted city) and **runs** (submit a formula) — extend the same protocol under
the same versioned prefix and are specified separately; see
[§9 Reserved surface](#9-reserved-surface).

## 1. Base URL and endpoint resolution

Every request targets a **base URL**, which MAY itself carry a path prefix
(e.g. `https://gascity.com` or `https://example.com/gascity`). All protocol
paths are relative to it.

The `gc` client resolves the base URL through this ladder (identical in shape
to the existing pack-registry resolution):

1. the explicit `--at <url>` flag value;
2. the `GC_SERVICE_URL` environment variable;
3. the stored default in the client credential file (the last URL logged into);
4. the compiled-in default constant `https://gascity.com`.

A URL string is **configuration data, not vendor logic** — the compiled-in
default is exactly the same category as the pack registry's
`registry.gascity.com` default and carries no policy.

A client MUST use `https` for the base URL; plain `http` is permitted only against
loopback (`localhost` / `127.0.0.1` / `::1`) for local development. The session
bearer is transmitted on every authenticated request, so cleartext transport is
refused (see [§7](#7-the-session-token-and-401-vs-403)).

## 2. Versioning

- The version string for this revision is `gascity.dev/service/v0`.
- Every request from a conforming client SHOULD send
  `X-GC-Service-Version: gascity.dev/service/v0`. A server MAY use it to route
  or to reject an unsupported client with `426 Upgrade Required`.
- Paths are namespaced under the versioned prefix `/gc/v0/`. A future
  incompatible revision uses a new prefix (`/gc/v1/`) and a new version string;
  the two can be served side by side.

## 3. Endpoints (auth + identity)

Protocol API request and response bodies are `application/json`. Field names
are `snake_case`. Unknown fields MUST be ignored by both sides (forward
compatibility). Errors use the shape in [§6](#6-errors).

The server-rendered browser routes are the exception. `GET /gc/v0/auth/cli`
and `GET /gc/v0/device` return HTML. `POST /gc/v0/device` accepts
`application/x-www-form-urlencoded` form data and returns HTML. These pages
remain server-owned; the CLI neither renders nor parses them.

| Method | Path | Auth | Purpose |
| ------ | ---- | ---- | ------- |
| `GET`  | `/gc/v0/auth/cli`          | none    | Browser sign-in page (server-rendered) |
| `POST` | `/gc/v0/auth/device/code`  | none    | Begin device-code login (headless) |
| `POST` | `/gc/v0/auth/device/token` | none    | Poll for the device-code token |
| `GET`  | `/gc/v0/device`            | browser session | Show the device-code approval page (server-rendered) |
| `POST` | `/gc/v0/device`            | browser session | Approve or deny a device code (form submission) |
| `GET`    | `/gc/v0/me`                | bearer  | Identify the authenticated account |
| `DELETE` | `/gc/v0/session`           | bearer  | Revoke the presented session (`gc logout`) |

### 3.1 Browser-callback login — `GET /gc/v0/auth/cli`

The primary interactive flow. The CLI starts a **loopback HTTP server** on
`127.0.0.1:<random-port>`, generates a random CSRF `state`, and opens the
user's browser to:

```
GET {base}/gc/v0/auth/cli?redirect_uri={loopback}/callback&state={state}&label={label}
```

- `redirect_uri` — the CLI's loopback callback (`http://127.0.0.1:<port>/callback`).
- `state` — an opaque, unguessable CSRF value the CLI generates and later verifies.
- `label` — a human label for the minted token (e.g. `user@host`), for display
  on the account's token list.

Everything behind this URL is **server-rendered and server-owned**: sign-in,
sign-up, org/workspace selection, consent. On success the page redirects the
browser to `redirect_uri` with the credential in the **URL fragment** (so it
never reaches the server logs of any intermediary), and a small script on the
CLI-served callback page forwards it to the loopback server:

```
POST http://127.0.0.1:<port>/token
Content-Type: application/json

{ "token": "<opaque-bearer>", "service": "{base}", "state": "{state}" }
```

The CLI:
- rejects the delivery unless `state` matches the value it generated;
- rejects it unless `service` is **present and equals** the login target — a
  callback that omits or mismatches `service` is refused, so a stray or hostile
  callback can never redirect the token to a different service. (`service` is
  therefore REQUIRED in the fragment.)
- stores `token` and returns.

`token` is an **opaque bearer string** with a server-defined lifetime. The CLI
never inspects it — no JWT parsing, no DPoP, no refresh protocol lives in the
client. A server that wants short-lived credentials performs any exchange
internally and re-issues on `401` (see [§7](#7-token-lifetime-and-401)).

### 3.2 Device-code login — `POST /gc/v0/auth/device/{code,token}`

The headless / SSH flow, shaped after RFC 8628. Selected by `gc login --device`
(and offered automatically when no browser can be opened).

**Begin** — `POST /gc/v0/auth/device/code`

```json
{ "label": "user@host" }
```

Response `200`:

```json
{
  "device_code": "<opaque>",
  "user_code": "BDWK-JQPX",
  "verification_uri": "https://gascity.com/gc/v0/device",
  "verification_uri_complete": "https://gascity.com/gc/v0/device?code=BDWK-JQPX",
  "expires_in": 900,
  "interval": 5
}
```

The CLI prints `verification_uri` and `user_code` (and the `_complete` link if
present), then polls.

The verification URI opens the server-rendered browser surface. An
authenticated browser uses `GET /gc/v0/device` to enter or inspect the user
code, then submits an approve or deny decision to `POST /gc/v0/device` as
`application/x-www-form-urlencoded` data. The server owns browser-session and
CSRF enforcement; this browser exchange never exposes its session to the CLI.

**Poll** — `POST /gc/v0/auth/device/token`

```json
{ "device_code": "<opaque>" }
```

- On success `200`: `{ "access_token": "<opaque-bearer>", "token_type": "bearer" }`.
- While pending, respond with a non-2xx status and a body carrying an `error`:
  - `authorization_pending` — keep polling at the current interval;
  - `slow_down` — increase the interval (to the returned `interval` if present,
    else by a fixed step);
  - `access_denied` — the user rejected; stop with an error;
  - `expired_token` — the code expired; stop with an error.

The CLI honors the server's `interval` and stops at `expires_in` (plus a small
grace).

### 3.3 Identity — `GET /gc/v0/me`

```
GET {base}/gc/v0/me
Authorization: Bearer <token>
```

Response `200`:

```json
{
  "user": { "id": "<opaque>", "handle": "julian", "display_name": "Julian K." },
  "session": { "created_at": "…Z", "expires_at": "…Z", "last_used": "…Z", "fingerprint": "gcs_ab" },
  "message": "You have $5 of trial credit.",
  "links": { "account": "https://gascity.com/account" }
}
```

- `user.id` — a stable opaque account identifier. **There is no org or tenant
  field.** An account is addressed only by opaque `id`/`handle`; the wire
  carries no tenancy identity.
- `session` — optional, **display-only** metadata (`created_at`, `expires_at`,
  `last_used`, `fingerprint`) the CLI shows via `gc whoami` so a user can see when
  the session expires and correlate it with the account's session list. The client
  never parses the token; `fingerprint` is a short non-secret label, never the
  handle.
- `message` / `links` — optional, server-authored, printed verbatim by the CLI
  (see [§5](#5-the-opacity-rule)).

A non-2xx response means the token is not valid; the CLI treats the caller as
not-logged-in. `gc login` calls `/gc/v0/me` immediately after obtaining a token
to verify it before storing.

### 3.4 Logout — `DELETE /gc/v0/session`

```
DELETE {base}/gc/v0/session
Authorization: Bearer <token>
```

Revokes the presented session server-side. `gc logout` calls this, then removes
the local credential. It is best-effort: a server that has not implemented
revocation returns `404`/`405`/`501` and the client still removes the local
token (and warns). Because the session is the only long-lived credential and is
not proof-of-possession bound, **revocation is the containment for a leaked
credential**; a conforming hosted server SHOULD implement it, revoking such that
the session stops resolving on the next request (see [§7](#7-the-session-token-and-401-vs-403)).

## 4. Client credential storage

Informative (client behavior, not wire): `gc` stores tokens in
`~/.gc/credentials.json` (under the Gas City home, overridable via the standard
Gas City home env), keyed by base URL, written atomically with `0600`
permissions:

```json
{
  "default_service_url": "https://gascity.com",
  "services": {
    "https://gascity.com": { "token": "<opaque>", "updated_at": "2026-07-10T…Z" }
  }
}
```

Multiple services coexist (the docker model: many registries, one `docker
login`). `gc whoami` reads the stored token for the resolved base URL.

## 5. The opacity rule

The CLI renders **only** what the server sends:

- Human-facing policy copy travels as opaque `message` strings.
- Actionable destinations travel as opaque `links` URLs the CLI may open.
- The token is an opaque bearer.

The CLI MUST NOT contain — and this protocol MUST NOT define wire fields for —
trial/credit/billing/plan/quota/subscription semantics, provisioning steps,
expiry math, or org/tenant identity. If a product wants to show "$5 of trial
credit," that sentence arrives as a `message`. This keeps the client generic
across arbitrary servers and free of vendor-specific logic. A conforming server
MAY reject a client that attempts to negotiate such semantics.

## 6. Errors

Non-2xx responses SHOULD carry a JSON body with an `error` object:

```json
{ "error": { "code": "invalid_token", "message": "Session expired." } }
```

`code` is a short machine token; `message` is human-facing and printed verbatim.
Well-known codes a client keys on (all other non-2xx are handled by HTTP status):

- `invalid_token` — the session is missing, expired, or invalid (**re-login**).
- `forbidden` / `insufficient_scope` — authenticated but not permitted for this
  action (**do not re-login**; surface `message`).

The device-token endpoint additionally uses the bare RFC-8628 `error` string
values enumerated in [§3.2](#32-device-code-login--post-gcv0authdevicecodetoken).
Servers MAY additionally emit `application/problem+json`; clients treat any
non-2xx as failure and surface `message` when present.

## 7. The session token, and 401 vs 403

The stored token is an **opaque, server-revocable session handle** with a
server-defined lifetime. The client never inspects it, never refreshes it, and
holds no key. A server MAY internally exchange the session for short-lived
downstream credentials to reach individual products — **that exchange is entirely
invisible to the client**, which only ever sends the session bearer. To let a
server distinguish token classes on the wire, servers SHOULD make the session
handle **syntactically distinguishable** (e.g. a stable, server-defined prefix);
the prefix itself is not part of this contract.

A rejected request is classified so the client gives the right remedy — and never
loops:

- **`401`** (or `error.code` = `invalid_token`) → the session is invalid/expired →
  "not logged in; run `gc login`".
- **`403`** (`forbidden` / `insufficient_scope`) → authenticated but not permitted
  → print the server `message` verbatim; **do not** advise re-login.
- **`5xx`** → a server-side failure → retryable; do not advise re-login.

The client MUST NOT treat a `403` as a login failure — re-login mints the same
session and would loop. Human credentials are never silently re-minted.

**Transport.** The session bearer is the only long-lived credential and is *not*
proof-of-possession bound, so a client MUST use `https` for the base URL (plain
`http` only against loopback), and MUST NOT follow a redirect that changes
scheme/host/port from the login origin — the bearer must never leave that origin.

## 8. Discovery (optional, forward-compatible)

A server MAY publish:

```
GET {base}/.well-known/gascity-service
→ 200 { "version": "gascity.dev/service/v0",
        "endpoints": { "auth_cli": "/gc/v0/auth/cli", "device_code": "…",
                       "device_token": "…", "device_verify": "/gc/v0/device",
                       "me": "/gc/v0/me" },
        "message": "…" }
```

A client MAY probe this to allow a server to relocate paths; absent or `404`, the
client uses the fixed well-known paths in [§3](#3-endpoints-auth--identity). The
v0 `gc` client uses the fixed paths and does not require discovery.

## 9. Reserved surface

The following extend this protocol under `/gc/v0/` and are specified in
companion documents as they land. They are listed here so the namespace and
version are coordinated:

- **Cities** — `POST /gc/v0/cities`, `GET /gc/v0/cities/{id}`: provision and
  watch a hosted city (the `gc init --at` flow).
- **Runs** — `POST /gc/v0/runs`, `GET /gc/v0/runs/{id}`,
  `GET /gc/v0/runs/{id}/events`, `POST /gc/v0/runs/{id}/claim`: submit and watch
  a formula run, optionally anonymously (the `gc run --at` flow).

- **Scoped-token challenge** — a `401` MAY carry a standard RFC 6750
  `WWW-Authenticate: Bearer realm="…", scope="example:resource.action"` challenge
  naming a token endpoint from which a client fetches a short-lived, scoped
  credential (the `docker login` model, for a future client that holds its own
  scoped tokens). A **v0 client ignores the challenge and reports not-logged-in per
  §7.** When implemented: the `realm` is trusted only if same-origin with the login
  base URL or named by the login origin's discovery document (§8, via a
  `token_endpoint` field); the session bearer is sent only to the login origin;
  scopes are opaque strings the client echoes verbatim.
- **Session rotation** — a server MAY return a replacement session handle in a
  response header on an authenticated response; a client that supports rotation
  atomically re-stores it (a pure string swap — no parsing) and treats reuse of a
  superseded handle as the server's cue to re-login. **v0 clients ignore the
  header.** Reserved so the header name can be fixed without a breaking change;
  rotation buys server-side theft detection without proof-of-possession.

Both reuse §1–§7 verbatim (base-URL resolution, versioning, session handle, the
opacity rule, error shape). `POST /gc/v0/runs` additionally permits an **absent**
`Authorization` header (anonymous submission).

## 10. Server conformance checklist (for `gc login`)

A server is conformant for `gc login` / `gc whoami` if it:

1. serves an interactive sign-in page at `GET /gc/v0/auth/cli` that redirects to
   `redirect_uri` with `token`, `service`, and `state` in the URL fragment;
2. implements the device-code pair at `POST /gc/v0/auth/device/{code,token}`
   with the RFC-8628-shaped fields and error values above;
3. serves the device verification page at `GET /gc/v0/device` and accepts its
   approve/deny form at `POST /gc/v0/device` for an authenticated browser;
4. implements `GET /gc/v0/me` returning `{ user: { id, handle, display_name } }`
   for a valid bearer and a non-2xx for an invalid one;
5. treats the token as an opaque bearer it can validate;
6. confines all account/commercial policy behind these endpoints, exposing it to
   the client only through opaque `message`/`links`.

`https://gascity.com` is one such server. So is any other.
