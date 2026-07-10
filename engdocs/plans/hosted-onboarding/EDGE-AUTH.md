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

## 6. Security: the no-DPoP posture, resolved

*Resolved by a 4-lens Fable security review (2026-07-10) that weighed decisions
(2) and (3). Decision (2) — accept the Docker/`gh` no-DPoP bearer — is **SAFE
only WITH the compensating controls below** (all four lenses). Decision (3) —
the TTL/revocation model — is resolved to concrete numbers here.*

### 6.1 Transport (shipped in #4135)

- **HTTPS only** — the CLI refuses a non-`https` base URL (loopback excepted), so
  the bearer never travels in cleartext.
- **Redirect hardening** — the client refuses any redirect changing
  scheme+host+port from the login origin (incl. the `https→http` same-host
  downgrade the stdlib does not strip); the bearer never egresses off-origin.
- **Mandatory callback service-match** — reject a callback whose `service` is
  absent or unequal to the login target.
- **Origin** is defined once (scheme+host+port), shared by the callback and the
  future realm-trust check.

### 6.2 The tradeoff, honestly (decision 2)

"Good enough for Docker" holds for the **auth shape**, not the **authorization
consequences**. A stolen `~/.gc/credentials.json` mints, for the session's life:
`city.create` (hosted compute + trial credits = free-tier fraud economics that
hit GitHub Actions / Heroku / GitLab CI), `beads.write` (inject work autonomous
agents execute), and `config.write` (**≈ RCE** on the hosted controller, per the
city-write red-team). Docker's worst case is pushing a bad image. The model is
safe **only** with the §6.4 controls; the design does not claim DPoP parity.

### 6.3 Session lifetime & revocation (decision 3 — RESOLVED)

| Knob | Policy |
| --- | --- |
| **Session lifetime** | **7-day idle (sliding) / 30-day absolute, non-extendable.** Trial/unverified orgs: **72h idle / 7d absolute** until verified, then upgrade in place. **No non-expiring sessions.** |
| **Renewal** | **Server-side sliding only** — the edge bumps `last_used` per resolve (throttled ~1 write / 5 min). **No refresh-token rotation in v0** (it forces write-after-use into the dumb client and races/corrupts `credentials.json` across parallel `gc`/CI copies). At the 30-day cap → full re-login (seconds, ~monthly). |
| **Revocation** | Tombstone the session row, **checked synchronously on every resolve/mint, fail-closed.** Kill is effectively instant (next mint denied); total residual = the already-minted **≤90s** credential → a hard **≤90s (≤2.5 min worst-case) containment SLA**. Triggers: `gc logout`, web per-session + revoke-all, password/SSO change, secret-scan hit, anomaly, org offboarding. |

**⚠ Protected invariant — "validity checked on every resolve."** The entire
no-DPoP posture rests on this. The first engineer who adds a session cache or
read-replica for latency silently converts near-instant revocation into
TTL-bounded revocation. State it as an invariant in the edge spec with a **≤60s
max-staleness cap** if a cache is ever introduced.

**Optional fast-follow (reserve now, build later): opaque-handle rotation.** A
session >24h old → the edge returns a replacement handle in a response header;
the CLI atomically swaps it (pure string swap, zero crypto), old handle valid for
a 60s grace. Buys theft *detection* (reuse of a superseded handle = compromise →
revoke the family), the OAuth refresh-rotation payoff without DPoP.

### 6.4 Compensating controls (edge-side; required for §6.2 to hold)

Ship **with** the edge, not after:

1. **Scannable handles + secret-scanning + auto-revoke** — `gcs_<32B>` prefix
   registered with GitHub secret scanning; auto-revoke + notify on any hit. This
   is the load-bearing half of "good enough for `gh`." Store only the SHA-256.
2. **Trial gating before first `city.create`** — verified email + (payment /
   aged-OAuth / phone), hard spend cap (~$10–20), ≤2 concurrent trial cities,
   `city.create` ≤2/h/session and ≤5/day/org, signup-velocity checks.
3. **24h interactive-auth freshness (sudo-mode)** on `city.create` +
   `config.write` — shrinks the highest-value replay window from the session TTL
   to a day, with **zero CLI change** (edge returns 401 → `gc login`).
4. **Per-session mint-rate limits + anomaly detection** (new-ASN / impossible-
   travel → auto-suspend + notify).
5. **Session inventory** — a web session/device list (created / last-used / geo)
   with per-session and revoke-all; you cannot revoke what the user cannot see.

### 6.5 CLI must-ships (this repo)

Revocation is the containment, so the kill switch and visibility are not optional:

- **`gc logout [service] [--all]`** — server-revoke (DELETE the session) then
  delete the local entry; revoke-first, always-delete-locally.
- **`gc whoami`** surfaces session `created` / `expires` / `last_used` + a
  fingerprint (display-only — never parses the token) + a `<72h` warning.
- **401/403 split** so a revoked/expired session says "run `gc login`" not a loop
  — ✅ shipped in #4135.
- **CI / non-TTY warning at `gc login`** — steer automation to a machine principal,
  never a pasted human session; plus an "exclude `~/.gc` from dotfile/backup sync"
  notice.
- **Rotation-header acceptance** — reserve the response header in the v0 spec now;
  the CLI atomically re-stores a replacement handle (fast-follow).

DPoP still applies to machine principals; a future high-assurance human tier could
opt in. v0 human onboarding does not.

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
5. **`gc logout` + session visibility** (the containment kill switch, §6.5):
   `gc logout [service] [--all]` server-revokes then deletes locally, and
   `gc whoami` surfaces session `created`/`expires`/`last_used` (display-only).
   Best-effort server-revoke until the edge lands; the local delete always works.

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
2. **No DPoP on the human path** — ✅ **RESOLVED: accepted (SAFE-WITH-CONTROLS).**
   The Docker/`gh` bearer shape is fine; the §6.4 compensating controls are
   mandatory because the blast radius (config.write ≈ RCE, city.create = compute +
   trial-fraud) exceeds Docker's.
3. **Session TTL + revocation** — ✅ **RESOLVED (§6.3):** 7d idle / 30d absolute
   (trial 72h/7d), server-side sliding, no client rotation in v0, revocation
   checked per-resolve fail-closed (≤90s / ≤2.5min containment). "Checked every
   resolve" is a protected invariant.
4. **Which edge** — a distinct `cli-edge` service reusing `identityedge`'s
   mint/inject library (recommended), vs. extending the deployed `identityedge`'s
   accepted principal set.
5. **Crucible trust** — accept edge-`iss` human EIAs on `city.create` (verifier
   audit), or have the edge mint with STS-`iss` semantics via the shared signer.
