# Hosted auth: the CLI credential model and the identity edge

*Status: design (hardened by a 4-lens Fable red-team, 2026-07-10 — all lenses
SOUND-WITH-FIXES). Companion to `DESIGN.md` (onboarding flows) and
`docs/reference/specs/service-protocol-v0.md` (wire protocol). Grounded in code
audits of the live `gasworks-platform` STS + `crucible` control plane.*

## 0. Decision (TL;DR)

Model the CLI credential path on **`docker login`**: the OSS `gc` CLI holds a
**dumb, long-lived, per-service bearer** (a *session handle*) and does **no
crypto** — no proof-of-possession, no JWT parsing, no token minting. The real
per-action credentials are **short-lived, per-audience, minted server-side** and
verified offline by each product. The one piece that does not exist yet is a
**public, human/CLI identity edge** that turns the CLI bearer into those
credentials. Build that edge; keep the OSS CLI a dumb bearer client.

1. **Dumb bearer in the CLI** — `gc login` stores an opaque session handle per
   service (done). The CLI never parses it and holds no signing/DPoP key.
2. **No proof-of-possession on the human path.** The shipped STS binds sessions
   to a DPoP key; adopting that forces a keypair + proof signing into the OSS
   client. We do not. The human session is therefore a **materially weaker plain
   bearer** than the DPoP-bound STS session (§6) — the `docker`/`gh` tradeoff,
   bounded by short TTL + server-side revocation, *not* a peer of the STS spine.
3. **Authorization stays server-side.** The CLI hardcodes no audience or scope.
   When a scoped credential is needed the server *declares* it (a standard RFC
   6750 `Bearer` challenge). Keeps authz judgment out of Go — our standing rule.
4. **The identity edge is the net-new piece** (private). It is a **stateful
   protocol server**, not a config tweak — see §3/§5. All authz/commercial policy
   lives there; the OSS CLI stays generic.

## 1. The problem

`gc login` (PR #4135) stores an opaque bearer and sends `Authorization: Bearer`
to `/gc/v0/*`. The audit shows "the CLI holds a dumb bearer; the edge does
everything" is only half-true against the real infrastructure:

- **STS** (`gasworks-platform/internal/sts`) is **client-driven and DPoP-bound**:
  the client holds a DPoP session and calls `POST /sts/v0/token` (RFC 8693) itself
  per credential. No gasworks code calls `/sts/v0/token` server-side. Adopting it
  verbatim pushes crypto into the CLI.
- **identity-edge** (`gasworks-platform/internal/identityedge`) *does* resolve →
  mint → inject in one hop, but only for an **API-key** bearer (machine) or a
  **BFF-verified human header** — not a human opaque session.

So a human-CLI dumb-bearer path needs a **net-new edge** that (a) *issues* CLI
sessions and (b) mints per-action credentials from them server-side. This is the
`identityedge` mint pattern extended to a new principal class (a CLI session),
plus the session-issuance/store/revocation surface `identityedge` does not have.
**No STS code changes; the human path never calls `/sts/v0/token`** — "STS minus
DPoP" is only a conceptual analogy (§2), not a code dependency.

## 2. What Docker does (the reference)

Docker's registry auth is the same shape, minus proof-of-possession:

- `docker login <registry>` stores a **long-lived static credential** (password /
  PAT) per host, or delegates to an OS-keychain helper. No client crypto.
- Per `pull`/`push`: the registry replies `401` with a standard **RFC 6750**
  `WWW-Authenticate: Bearer realm="<token-server>", service="…", scope="repository:library/ubuntu:pull"`.
  The client fetches a **short-lived, scope-limited token** from the named realm
  (presenting the stored credential), then retries. The registry **verifies the
  token offline** and checks the scope — it never sees the password.

| Docker | Gas City |
| --- | --- |
| stored PAT/password (per registry, keychain) | opaque session handle `gc login` stores |
| token server (`realm`, often a *different* host) | the identity edge's token endpoint |
| short-lived scoped token (~min) | short-lived per-audience credential (offline-verified) |
| `repository:ubuntu:pull` scope | opaque server-defined scope strings |
| **client presents a static secret (no DPoP)** | **CLI presents its session (no DPoP)** |
| standard `Bearer` challenge declares realm+scope | server declares realm+scope; CLI echoes them |

We reuse the **standard `Bearer` scheme + RFC 8693 token-exchange** so any
off-the-shelf client/server library interops — no bespoke `GC-Bearer` parsing in
OSS. Note Docker's realm is legitimately **cross-origin** (registry-1.docker.io →
auth.docker.io); our trust anchor (§4) must allow that without trusting the
challenge header blindly.

## 3. The identity edge (the net-new piece)

A **public, human/CLI-facing identity edge** (private repo, at
`works.gascity.com`) issues CLI sessions and turns them into per-audience
credentials. It is **stateful** — the red-team's key correction. Its surface:

- **Session issuance riding the existing browser trust.** `GET /gc/v0/auth/cli`
  (and the device-code pair) render behind the existing apex-cookie/BFF browser
  session — the input class `identityedge` already trusts — and mint an opaque
  **CLI session** the browser callback hands back. Plus a **session store with
  server-side revocation** and a **device-code store**.
- **Identity.** `GET /gc/v0/me` validates the **session** (not a minted
  credential) — this is the login-validity oracle the CLI polls.
- **Per-action mint (Variant A, default).** For a product path the edge
  **resolves** the session → org/subject/entitlement ceiling (no DPoP), **mints**
  the per-audience credential (scope intersected fail-closed against the ceiling),
  strips inbound `Authorization`, **injects** the identity header, and proxies to
  the product. The **CLI is byte-identical to today** — it just sends its session
  bearer.
- **Cities translation (stateful).** `POST /gc/v0/cities` → crucible
  `POST /v0/cities`; the edge holds a **`request_id` → `city_id` idempotency
  table** (crucible dedupes on `(org,name)`, so map or derive the crucible name
  deterministically from `(org, request_id)`), and **synthesizes** the
  `configuring`/`wizard_url` state + `status_url`/`links.dashboard`/`api.base_url`
  that crucible never emits (crucible: `pending|provisioning|ready|error`, 201
  not 202).

**Variant B (client-fetched scoped token)** — the literal Docker model, for when
the client must *hold* a scoped credential (e.g. `gc auth token-helper` feeding
the remote-gc client). Deferred; reserved in the spec (§4), implemented only when
a client-held token is actually needed (two-implementations rule).

## 4. The challenge shape (spec: reserved, not normative for v0)

When a client must fetch its own token (Variant B), the server declares it with a
**standard RFC 6750 `Bearer` challenge** — no custom scheme:

```
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer realm="https://…/gc/v0/auth/token", scope="example:resource.action", audience="example"
```

Trust anchor (closes the confused-deputy hole): **the client sends its session
bearer to a `realm` only if that realm's origin is (a) the same origin it logged
into, or (b) named by the login origin's own discovery document
(`/.well-known/gascity-service`, which gains a `token_endpoint` field).** Trust
flows from the origin the user logged into — **never from the challenge header
alone, and there is no server- or challenge-supplied allowlist.** Scopes are
opaque strings the client echoes verbatim; the CLI hardcodes none.

For v0 this is a **single-line reservation** in the spec: a 401 MAY carry a
`Bearer` challenge; **v0 clients ignore it and report not-logged-in per §7**. The
normative token-endpoint contract (response `{access_token, token_type,
expires_in}`, one-fetch-one-retry, EIA-never-persisted, error codes) lands only
when the B client is built.

## 5. Mapping to real infrastructure (built vs the gap)

**Built + deployed:** crucible `POST /v0/cities` (201, EIA-gated), the provisioner
daemon (mint orchestrator cred → beads ledger → controller sandbox, ≤6 min),
`GET /v0/cities/{id}/status` (`pending|provisioning|ready|error`), and a live
cityproxy **read** plane (writes gated behind an unstanding minter). STS per-product
signers in OpenBao; sessions + `/token` exchange live (DPoP-bound, machine + human
via Keycloak).

**The net-new work (honestly sized):**

1. **The edge is a stateful auth server**, not an `identityedge` flag: CLI-session
   issuance (riding apex/BFF) + session store + revocation + device-code store +
   `/me` + the cities idempotency/wizard state (§3). This is the bulk.
2. **Crucible must trust edge-issued human credentials.** `identityedge` stamps
   `iss=edge.gascity.internal` (≠ STS `iss`), and the only deployed public leg
   (`eia-machine-proxy`) **403s human EIAs** (`subject_type==service` +
   `org_internal`). **Verify/adjust crucible's verifier** to accept the edge's
   `iss` + `subject_type=user` on `city.create`, and give the edge its **own
   tailnet leg** to crucible (it bypasses `eia-machine-proxy`).
3. **`crucible:city.create` role + grant** — lives on unmerged crucible PR #257;
   main has only machine-only `city.provision`/`city.work`, and no real user holds
   the role. Cheap but a **hard gate** in a different repo/owner.
4. **Same-origin city API.** Every hosted city API must be fronted by the edge on
   the **login origin** (path-routed via cityproxy, e.g.
   `works.gascity.com/…/cities/<id>/api/…`) so `api.base_url` is same-origin and
   Variant A alone suffices — otherwise Variant B is back on the onboarding
   critical path. (Constrained in spec §9.2.)

**Build order (critical path):** (1) land crucible PR #257 + grant the role — days,
different owner, do first; (2) **cliauth hardening + spec edits — PR #4135, now**;
(3) crucible verifier trust audit; (4) the edge auth/session surface — the bulk;
(5) cities translation. Variant B client stays deferred.

## 6. Security model & transport (the load-bearing part)

The session bearer is the crown jewel — the only long-lived credential, and
without DPoP it is **replayable until expiry** (a stolen 0600 file is enough).
That mandates:

- **HTTPS only.** The CLI MUST refuse a non-`https` base URL (except explicit
  loopback for dev). Today an explicitly-typed `http://` is accepted → the bearer
  goes out in cleartext. **(Client fix, PR #4135.)**
- **Redirect hardening.** The protocol HTTP client MUST refuse any redirect that
  changes **scheme+host+port** from the login origin (in particular an
  `https→http` same-host downgrade, which Go's stdlib does *not* strip) — the
  bearer must never egress off-origin. **(Client fix, PR #4135.)**
- **Mandatory callback service-match.** The browser-login callback MUST reject a
  payload whose `service` is **absent or unequal** to the login target — today the
  check is skipped when `service` is omitted. **(Client fix, PR #4135.)**
- **Origin, defined once** as scheme+host+port, shared by the callback check and
  the (future) realm-trust check — no two subtly different comparisons.
- **Bounded replay window.** Short session TTL, **fast server-side revocation
  checked on every resolve**, and mint-per-session rate/anomaly limits (edge-side).
  The high-value minted credentials stay short-lived and **never touch disk**.
- **DPoP still applies to machine principals**; a future high-assurance human tier
  could opt in. v0 human onboarding does not.

The design does **not** claim parity with the DPoP spine — it claims a bounded,
revocable, TLS-only bearer, which is the accepted get-started-CLI tradeoff.

## 7. OSS vs private split

**OSS `gc` / spec (this repo), now:** the dumb bearer client (done) + the three
transport fixes (HTTPS-only, redirect hardening, mandatory service-match) + the
401/403 error split (§8) + generic spec wording (session handle; server-side
exchange is invisible; **no EIA/X-Gc-Identity/STS/crucible-scope vocabulary**) +
a one-line reserved `Bearer` challenge. Zero minting, DPoP, JWT parsing, or scope
constants. A credential-helper hook is **deferred (not #4135)**; when built it
stays a pure get/store/erase exec contract.

**Private (gasworks/crucible):** the stateful identity edge (issuance + store +
revocation + resolve/mint/inject + cities translation), per-product signers, the
`city.create` role + grant, all authorization policy, and any DPoP.

## 8. Changes for the `gc login` PR (#4135) — the must-fix list

**Client (`internal/cliauth`, `cmd/gc/cmd_login.go`):**
1. **Enforce HTTPS** in `normalizeServiceBaseURL` — reject `http://` except
   loopback/localhost.
2. **Redirect hardening** — a `CheckRedirect` on the protocol client that refuses
   any scheme/host/port change from the base URL (covers `Whoami` and, on the
   stacked branch, `doAuthedJSON`). Test cross-host + `https→http` downgrade.
3. **Mandatory callback `service`-match** — reject on absent or mismatched
   `service`.
4. **401/403 split** — the error paths classify: `401`/`invalid_token` → "not
   logged in; run `gc login`"; `403`/`forbidden`/`insufficient_scope` →
   authenticated-but-unauthorized, print the server `message` verbatim, do **not**
   advise re-login; `5xx` → server failure, retryable (no re-login advice).

**Spec (`service-protocol-v0.md`):**
5. **Credential-model precision** (§5/§7), generic: the stored token is an opaque,
   **server-revocable session handle**; a server MAY internally exchange it for
   short-lived downstream credentials, invisible to the client. No vendor
   vocabulary; servers SHOULD make token classes syntactically distinguishable
   (server-defined prefix).
6. **Enumerate error codes** (§6): `invalid_token`, `forbidden`/`insufficient_scope`,
   a server-failure code; and **split 401 vs 403** semantics in §7 (kill the
   current "any 401/403 → re-login" conflation that would loop a user lacking a
   scope).
7. **Reserve the `Bearer` challenge** (one line, §9/§10 reserved): a 401 MAY carry
   a standard RFC 6750 `Bearer` challenge naming a token endpoint; v0 clients
   ignore it and report not-logged-in. Neutral placeholder scope only.
8. **Same-origin city API** (§9.2, on the stacked cities branch): one sentence —
   the hosted city `api.base_url` is served under the login service origin.

## 9. Open decisions

1. **Variant A vs B for v0** — recommend A (edge-transparent, CLI unchanged);
   reserve the standard-`Bearer` challenge now.
2. **No DPoP on the human path** — accept the bounded, revocable, TLS-only bearer
   tradeoff (Docker/`gh`)? The load-bearing call.
3. **Session TTL + revocation** — concrete TTL and "revocation = stop resolving,
   checked every request" (no client CRL). Resolve before calling the model
   shippable.
4. **Which edge** — a distinct `cli-edge` service reusing `identityedge`'s
   mint/inject library (recommended), vs. extending the deployed `identityedge`'s
   accepted principal set.
5. **Crucible trust** — accept edge-`iss` human EIAs on `city.create` (verifier
   audit), or have the edge mint with STS-`iss` semantics via the shared signer.
